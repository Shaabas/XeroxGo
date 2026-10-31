package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

type wireMessage struct {
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

// ClientConfig holds the runtime dependencies and parameters for the kiosk spooler client.
type ClientConfig struct {
	CloudURL      string
	KioskID       string
	StateStore    *state.Store
	PrinterAddr   string
	DownloadDir   string
	IdleQRContent string
	ScreenSink    ui_ipc.Sink
	HTTPClient    *http.Client
	MockHealth    func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
}

// Client connects to the cloud WebSocket hub, handles heartbeats, downloads and verifies PDFs,
// streams print jobs via PJL, renders status screens, and recovers incomplete jobs.
type Client struct {
	cfg           ClientConfig
	normalizedURL string

	connMu sync.Mutex
	conn   *websocket.Conn
	sendMu sync.Mutex

	jobQueue   chan contracts.PrintJobCommand
	activeMu   sync.Mutex
	activeJobs map[string]bool

	minBackoff        time.Duration
	maxBackoff        time.Duration
	heartbeatInterval time.Duration
	maxDownloadBytes  int64

	lastRenderErrMu sync.Mutex
	lastRenderErr   error

	renderHistoryMu sync.Mutex
	renderHistory   []ui_ipc.ScreenState

	healthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
}

// NewClient validates the configuration and initializes a new spooler client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.CloudURL) == "" {
		return nil, errors.New("client config: cloud URL cannot be empty")
	}
	u, err := url.Parse(cfg.CloudURL)
	if err != nil {
		return nil, fmt.Errorf("client config: invalid cloud URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("client config: cloud URL scheme must be ws or wss, got %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("client config: cloud URL missing host")
	}

	if strings.TrimSpace(cfg.KioskID) == "" {
		return nil, errors.New("client config: kiosk ID cannot be empty")
	}
	if cfg.StateStore == nil {
		return nil, errors.New("client config: state store cannot be nil")
	}
	if strings.TrimSpace(cfg.PrinterAddr) == "" {
		return nil, errors.New("client config: printer address cannot be empty")
	}
	if strings.TrimSpace(cfg.DownloadDir) == "" {
		return nil, errors.New("client config: download directory cannot be empty")
	}
	if strings.TrimSpace(cfg.IdleQRContent) == "" {
		return nil, errors.New("client config: idle QR content cannot be empty")
	}
	if cfg.ScreenSink == nil {
		return nil, errors.New("client config: screen sink cannot be nil")
	}

	q := u.Query()
	q.Set("kiosk_id", cfg.KioskID)
	u.RawQuery = q.Encode()
	normalizedURL := u.String()

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	cfg.HTTPClient = httpClient

	poller := cfg.MockHealth
	if poller == nil {
		targetHost := "192.168.99.2"
		if h, _, err := net.SplitHostPort(cfg.PrinterAddr); err == nil && h != "" {
			targetHost = h
		} else if strings.TrimSpace(cfg.PrinterAddr) != "" {
			targetHost = strings.TrimSpace(cfg.PrinterAddr)
		}
		poller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return printer.PollHealthTarget(ctx, targetHost, estimatedSheets)
		}
	}

	return &Client{
		cfg:               cfg,
		normalizedURL:     normalizedURL,
		jobQueue:          make(chan contracts.PrintJobCommand, 16),
		activeJobs:        make(map[string]bool),
		minBackoff:        1 * time.Second,
		maxBackoff:        30 * time.Second,
		heartbeatInterval: 15 * time.Second,
		maxDownloadBytes:  250 * 1024 * 1024,
		healthPoller:      poller,
	}, nil
}

// Run executes the spooler client loops until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	// 1. Initial idle screen transition
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:      ui_ipc.ModeIdle,
		QRContent: c.cfg.IdleQRContent,
		Message:   "Ready to print",
	})

	// 2. Startup recovery check
	incomplete, err := c.cfg.StateStore.GetIncompleteJob(ctx)
	if err == nil && incomplete != nil {
		c.activeMu.Lock()
		c.activeJobs[incomplete.Command.JobID] = true
		c.activeMu.Unlock()
		select {
		case c.jobQueue <- incomplete.Command:
		default:
		}
	}

	// 3. Start sequential job processing worker
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.jobWorker(ctx)
	}()

	// 4. Connection & reconnection loop
	delay := c.minBackoff
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		default:
		}

		conn, _, err := websocket.Dial(ctx, c.normalizedURL, nil)
		if err != nil {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			case <-time.After(delay):
				delay *= 2
				if delay > c.maxBackoff {
					delay = c.maxBackoff
				}
				continue
			}
		}

		// Connected successfully: reset backoff delay
		delay = c.minBackoff
		c.handleConnection(ctx, conn)
	}
}

func (c *Client) handleConnection(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(1024 * 1024)

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "disconnecting")
	}()

	// Start heartbeat and telemetry loop
	var hbWg sync.WaitGroup
	hbWg.Add(1)
	go func() {
		defer hbWg.Done()
		c.heartbeatLoop(connCtx, conn)
	}()

	// Reader loop
	for {
		var msg wireMessage
		err := wsjson.Read(connCtx, conn, &msg)
		if err != nil {
			break
		}
		if msg.Type == "job" && msg.Job != nil {
			c.handleIncomingJob(*msg.Job)
		}
	}

	cancel()
	hbWg.Wait()
}

func (c *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Send heartbeat
			_ = c.sendWire(ctx, conn, wireMessage{Type: "heartbeat"})

			// Relay printer health
			sheets, _ := c.cfg.StateStore.GetEstimatedSheets(ctx)
			h := c.healthPoller(ctx, sheets)
			_ = c.sendWire(ctx, conn, wireMessage{Type: "health", Health: &h})
		}
	}
}

func (c *Client) sendWire(ctx context.Context, conn *websocket.Conn, msg wireMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.connMu.Lock()
	activeConn := c.conn
	c.connMu.Unlock()

	if activeConn == nil || activeConn != conn {
		return errors.New("websocket connection not active")
	}

	return wsjson.Write(ctx, activeConn, msg)
}

func (c *Client) sendProgress(ctx context.Context, event contracts.JobProgressEvent) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	_ = wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &event})
}

func (c *Client) handleIncomingJob(cmd contracts.PrintJobCommand) {
	if cmd.KioskID != c.cfg.KioskID {
		return
	}
	if strings.TrimSpace(cmd.JobID) == "" {
		return
	}
	u, err := url.Parse(cmd.DownloadURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return
	}
	if len(cmd.SHA256Checksum) != 64 {
		return
	}
	if _, err := hex.DecodeString(cmd.SHA256Checksum); err != nil {
		return
	}
	if cmd.TotalPages < 1 || cmd.TotalSheets < 1 || cmd.Copies < 1 || cmd.StartFromPage < 1 || cmd.StartFromPage > cmd.TotalPages {
		return
	}

	c.activeMu.Lock()
	if c.activeJobs[cmd.JobID] {
		c.activeMu.Unlock()
		return
	}
	if len(c.activeJobs) > 0 {
		c.activeMu.Unlock()
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn != nil {
			_ = c.sendWire(context.Background(), conn, wireMessage{
				Type:  "error",
				Error: "kiosk is busy processing an existing job",
			})
		}
		return
	}
	c.activeJobs[cmd.JobID] = true
	c.activeMu.Unlock()

	select {
	case c.jobQueue <- cmd:
	default:
		c.activeMu.Lock()
		delete(c.activeJobs, cmd.JobID)
		c.activeMu.Unlock()

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn != nil {
			_ = c.sendWire(context.Background(), conn, wireMessage{
				Type:  "error",
				Error: "kiosk job queue is full",
			})
		}
	}
}

func (c *Client) jobWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-c.jobQueue:
			c.processJob(ctx, cmd)
			c.activeMu.Lock()
			delete(c.activeJobs, cmd.JobID)
			c.activeMu.Unlock()
		}
	}
}

func (c *Client) processJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	// 1. Initial screen transition to DOWNLOADING
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeDownloading,
		ProgressPercent: 0,
		Message:         "Downloading document...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 2. Persist initial job record if not already stored
	savedJob, _ := c.cfg.StateStore.GetIncompleteJob(ctx)
	if savedJob == nil || savedJob.Command.JobID != cmd.JobID {
		_ = c.cfg.StateStore.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	}

	// 3. Download & verify PDF
	h := sha256.Sum256([]byte(cmd.JobID))
	finalName := hex.EncodeToString(h[:]) + ".pdf"
	finalPath := filepath.Join(c.cfg.DownloadDir, finalName)

	needDownload := true
	if existingData, err := os.ReadFile(finalPath); err == nil {
		existingH := sha256.Sum256(existingData)
		existingSum := hex.EncodeToString(existingH[:])
		if existingSum == strings.ToLower(cmd.SHA256Checksum) {
			needDownload = false
		}
	}

	if needDownload {
		if err := c.downloadAndVerify(ctx, cmd, finalPath); err != nil {
			c.handleJobError(ctx, cmd, 0, 0, fmt.Sprintf("download failed: %v", err))
			return
		}
	}

	// Screen: Download 100%
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeDownloading,
		ProgressPercent: 100,
		Message:         "Download verified",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 4. Update progress: SPOOLING
	pagesPrinted := 0
	sheetsPrinted := 0
	if savedJob != nil && savedJob.Command.JobID == cmd.JobID {
		pagesPrinted = savedJob.PagesPrinted
		sheetsPrinted = savedJob.SheetsPrinted
	}

	spoolEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusSpooling,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, spoolEvt)
	c.sendProgress(ctx, spoolEvt)

	// 5. Calculate start page and check paper supply
	startPage := pagesPrinted + 1
	if startPage < cmd.StartFromPage {
		startPage = cmd.StartFromPage
	}

	if startPage > cmd.TotalPages {
		c.completeJob(ctx, cmd)
		return
	}

	remainingPages := cmd.TotalPages - startPage + 1
	remainingSheets := 0
	if cmd.Duplex == contracts.DuplexSimplex {
		remainingSheets = remainingPages * cmd.Copies
	} else {
		remainingSheets = ((remainingPages + 1) / 2) * cmd.Copies
	}

	estimatedSheets, err := c.cfg.StateStore.GetEstimatedSheets(ctx)
	if err != nil || estimatedSheets < remainingSheets {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, "paper tray supply insufficient for remaining print job")
		return
	}

	// 6. Page Slicing if resuming partway
	printPath := finalPath
	streamCmd := cmd
	if startPage > 1 {
		slicedPath := filepath.Join(c.cfg.DownloadDir, fmt.Sprintf("slice_%s_%d.pdf", finalName, startPage))
		if err := printer.SlicePDFForResume(ctx, finalPath, slicedPath, startPage); err != nil {
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("slicing failed: %v", err))
			return
		}
		defer os.Remove(slicedPath)
		printPath = slicedPath

		streamCmd.StartFromPage = 1
		streamCmd.TotalPages = remainingPages
		streamCmd.TotalSheets = remainingSheets
	}

	// 7. Update progress: PRINTING
	printEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, printEvt)
	c.sendProgress(ctx, printEvt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModePrinting,
		CurrentPage:     pagesPrinted,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
		Message:         "Printing document...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 8. Stream PDF via PJL
	f, err := os.Open(printPath)
	if err != nil {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("failed to open PDF for print: %v", err))
		return
	}
	defer f.Close()

	lastReportedStreamSheets := 0
	initialSheetsOffset := sheetsPrinted

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	onPage := func(streamSheetsCompleted int) {
		if streamSheetsCompleted > lastReportedStreamSheets {
			delta := streamSheetsCompleted - lastReportedStreamSheets
			lastReportedStreamSheets = streamSheetsCompleted

			if err := c.cfg.StateStore.DecrementSheets(jobCtx, delta); err != nil {
				cancelJob()
				return
			}
			sheetsPrinted = initialSheetsOffset + streamSheetsCompleted

			if cmd.Duplex == contracts.DuplexSimplex {
				pagesPrinted = (startPage - 1) + streamSheetsCompleted/cmd.Copies
			} else {
				pagesPrinted = (startPage - 1) + (streamSheetsCompleted*2)/cmd.Copies
			}

			if pagesPrinted > cmd.TotalPages {
				pagesPrinted = cmd.TotalPages
			}
			if sheetsPrinted > cmd.TotalSheets {
				sheetsPrinted = cmd.TotalSheets
			}

			evt := contracts.JobProgressEvent{
				JobID:         cmd.JobID,
				KioskID:       cmd.KioskID,
				Status:        contracts.JobStatusPrinting,
				PagesPrinted:  pagesPrinted,
				SheetsPrinted: sheetsPrinted,
				Timestamp:     time.Now().UTC(),
			}
			if err := c.cfg.StateStore.UpdateProgress(jobCtx, evt); err != nil {
				cancelJob()
				return
			}
			c.sendProgress(jobCtx, evt)

			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:            ui_ipc.ModePrinting,
				CurrentPage:     pagesPrinted,
				TotalPages:      cmd.TotalPages,
				ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
				Message:         "Printing document...",
				Detail:          "Job ID: " + cmd.JobID,
			})
		}
	}

	if err := printer.StreamPDFWithPJL(jobCtx, c.cfg.PrinterAddr, streamCmd, f, onPage); err != nil {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("printing failed: %v", err))
		return
	}

	// Reconcile remaining sheets if not already deducted
	if sheetsPrinted < cmd.TotalSheets {
		remainingToDeduct := cmd.TotalSheets - sheetsPrinted
		_ = c.cfg.StateStore.DecrementSheets(ctx, remainingToDeduct)
	}

	// 9. Complete Job
	c.completeJob(ctx, cmd)
}

func (c *Client) completeJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  cmd.TotalPages,
		SheetsPrinted: cmd.TotalSheets,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, evt)
	c.sendProgress(ctx, evt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeCompleted,
		CurrentPage:     cmd.TotalPages,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: 100,
		Message:         "Print complete",
		Detail:          "Job ID: " + cmd.JobID,
	})
}

func (c *Client) handleJobError(ctx context.Context, cmd contracts.PrintJobCommand, pagesPrinted, sheetsPrinted int, reason string) {
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		ErrorReason:   reason,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, evt)
	c.sendProgress(ctx, evt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: reason,
		Detail:  "Job ID: " + cmd.JobID,
	})
}

func (c *Client) downloadAndVerify(ctx context.Context, cmd contracts.PrintJobCommand, finalPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cmd.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned non-200 status: %d", resp.StatusCode)
	}

	limit := c.maxDownloadBytes
	if limit <= 0 {
		limit = 250 * 1024 * 1024
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("declared content length %d exceeds maximum limit of %d bytes", resp.ContentLength, limit)
	}

	tmpFile, err := os.CreateTemp(c.cfg.DownloadDir, "job_download_*.tmp")
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpName := tmpFile.Name()

	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFile.Close()
			_ = os.Remove(tmpName)
		}
	}()

	hasher := sha256.New()
	limitedReader := io.LimitReader(resp.Body, limit+1)

	buf := make([]byte, 32*1024)
	var written int64
	lastPct := 0

	for {
		nr, rerr := limitedReader.Read(buf)
		if nr > 0 {
			written += int64(nr)
			if written > limit {
				return fmt.Errorf("downloaded bytes %d exceeded limit of %d bytes", written, limit)
			}
			hasher.Write(buf[:nr])
			if _, werr := tmpFile.Write(buf[:nr]); werr != nil {
				return fmt.Errorf("write temp file: %w", werr)
			}

			if resp.ContentLength > 0 {
				pct := int((written * 100) / resp.ContentLength)
				if pct > 100 {
					pct = 100
				}
				if pct > lastPct {
					lastPct = pct
					_ = c.renderScreen(ui_ipc.ScreenState{
						Mode:            ui_ipc.ModeDownloading,
						ProgressPercent: pct,
						Message:         "Downloading document...",
						Detail:          "Job ID: " + cmd.JobID,
					})
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return fmt.Errorf("streaming download failed: %w", rerr)
		}
	}

	calcSum := hex.EncodeToString(hasher.Sum(nil))
	if calcSum != strings.ToLower(cmd.SHA256Checksum) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", calcSum, cmd.SHA256Checksum)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, finalPath); err != nil {
		return fmt.Errorf("rename to final path: %w", err)
	}

	cleanup = false
	return nil
}

func (c *Client) renderScreen(state ui_ipc.ScreenState) error {
	c.renderHistoryMu.Lock()
	c.renderHistory = append(c.renderHistory, state)
	c.renderHistoryMu.Unlock()

	img, err := ui_ipc.Render(state)
	if err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	if err := c.cfg.ScreenSink(img); err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	return nil
}
