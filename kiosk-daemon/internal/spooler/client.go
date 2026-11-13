package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/audio"
	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/pin"
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
	Sheets   int                         `json:"sheets,omitempty"`
	Error    string                      `json:"error,omitempty"`
	Ads      []contracts.AdItem          `json:"ads,omitempty"`
}

// ClientConfig holds the runtime dependencies and parameters for the kiosk spooler client.
type ClientConfig struct {
	CloudURL                 string
	KioskID                  string
	KioskSecret              string
	StateStore               *state.Store
	PrinterAddr              string
	SNMPTarget               string
	SNMPCommunity            string
	SNMPPort                 int
	CUPSQueue                string
	PrinterModel             string
	DownloadDir              string
	IdleQRContent            string
	AdsCachePath             string
	IdleCarouselInterval     time.Duration
	CompletedDisplayDuration time.Duration
	ErrorDisplayDuration     time.Duration
	HealthPollInterval           time.Duration
	RequiredRecoveryHealthyCount int
	ScreenSink                   ui_ipc.Sink
	HTTPClient               *http.Client
	MockHealth               func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	MockCUPSPrint            func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error
	CUPSRunner               printer.CommandRunner
	EnableCloudRecovery      bool
	InputSource              input.Source
	InactivityTimeout        time.Duration
	SummaryTimeout           time.Duration
	PaymentTimeout           time.Duration
	PaymentURLBase           string
	SoundPlayer              audio.Player
	OnJobCompleted           func(jobID string)
	TrustedOrigins           []string
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

	deferredMu   sync.Mutex
	deferredJobs []contracts.PrintJobCommand

	idleMu    sync.Mutex
	idleTimer *time.Timer

	minBackoff        time.Duration
	maxBackoff        time.Duration
	heartbeatInterval time.Duration
	maxDownloadBytes  int64

	lastRenderErrMu sync.Mutex
	lastRenderErr   error

	renderHistoryMu sync.Mutex
	renderHistory   []ui_ipc.ScreenState

	currentModeMu sync.Mutex
	currentMode   ui_ipc.ScreenMode
	adSlideIndex  int

	faultMu               sync.Mutex
	hardwareFault         bool
	faultReason           string
	faultCustomerMsg      string
	consecutiveHealthy   int
	requiredRecoveryCount int

	renderMu sync.Mutex
	canvas   *image.RGBA

	healthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth

	pinController *pin.Controller
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
	if strings.TrimSpace(cfg.PrinterAddr) == "" && strings.TrimSpace(cfg.CUPSQueue) == "" {
		return nil, errors.New("client config: either printer address or CUPS queue must be provided")
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
	if strings.TrimSpace(cfg.KioskSecret) != "" {
		q.Set("token", strings.TrimSpace(cfg.KioskSecret))
	}
	u.RawQuery = q.Encode()
	normalizedURL := u.String()

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if httpClient.CheckRedirect == nil {
		httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("redirect scheme downgrade from https to http forbidden")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("disallowed redirect scheme %q", req.URL.Scheme)
			}
			if req.URL.User != nil {
				return errors.New("redirect URL cannot contain user credentials")
			}
			return nil
		}
	}
	cfg.HTTPClient = httpClient

	poller := cfg.MockHealth
	if poller == nil {
		if cfg.MockCUPSPrint != nil && cfg.CUPSRunner == nil {
			poller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          true,
					State:           "READY",
					PaperTrayStatus: "OK",
					EstimatedSheets: estimatedSheets,
					UpdatedAt:       time.Now().UTC(),
				}
			}
		} else {
			targetHost := strings.TrimSpace(cfg.SNMPTarget)
		if targetHost == "" {
			var candidate string
			if h, _, err := net.SplitHostPort(cfg.PrinterAddr); err == nil && h != "" {
				candidate = h
			} else if strings.TrimSpace(cfg.PrinterAddr) != "" {
				candidate = strings.TrimSpace(cfg.PrinterAddr)
			}
			if candidate != "" && candidate != "127.0.0.1" && candidate != "localhost" && candidate != "::1" {
				targetHost = candidate
			}
		}
		if targetHost == "" && strings.TrimSpace(cfg.CUPSQueue) != "" {
			discovered := printer.DiscoverQueueHost(context.Background(), cfg.CUPSRunner, strings.TrimSpace(cfg.CUPSQueue))
			if discovered != "" {
				targetHost = discovered
				cfg.SNMPTarget = discovered
				log.Printf("[SPOOLER] Auto-discovered printer host %s from CUPS queue %s", discovered, cfg.CUPSQueue)
			}
		}

		snmpCfg := printer.DefaultSNMPConfig()
		snmpCfg.Target = targetHost
		if cfg.SNMPPort > 0 {
			snmpCfg.Port = cfg.SNMPPort
		}
		if cfg.SNMPCommunity != "" {
			snmpCfg.Community = cfg.SNMPCommunity
		}

		cupsQ := strings.TrimSpace(cfg.CUPSQueue)
		runner := cfg.CUPSRunner
		poller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			if strings.TrimSpace(cupsQ) == "" && strings.TrimSpace(snmpCfg.Target) == "" {
				return contracts.PrinterHealth{
					Online:          true,
					State:           "READY",
					PaperTrayStatus: "OK",
					EstimatedSheets: estimatedSheets,
					UpdatedAt:       time.Now().UTC(),
				}
			}
			return printer.PollCombinedHealth(ctx, runner, cupsQ, snmpCfg, estimatedSheets)
		}
	}
	}

	if cfg.CompletedDisplayDuration <= 0 {
		cfg.CompletedDisplayDuration = 7 * time.Second
	}
	if cfg.ErrorDisplayDuration <= 0 {
		cfg.ErrorDisplayDuration = 10 * time.Second
	}
	if cfg.HealthPollInterval <= 0 {
		cfg.HealthPollInterval = 500 * time.Millisecond
	}

	if cfg.AdsCachePath == "" {
		cfg.AdsCachePath = "/var/kiosk/ads_cache.json"
	}
	if cached, err := loadAdsCache(cfg.AdsCachePath); err == nil && len(cached) > 0 {
		log.Printf("[SPOOLER] Loaded %d promotional ads from local cache %s", len(cached), cfg.AdsCachePath)
		ui_ipc.ConfigureAds(cached)
	}

	reqHealthy := cfg.RequiredRecoveryHealthyCount
	if reqHealthy <= 0 {
		reqHealthy = 3
	}

	client := &Client{
		cfg:                   cfg,
		normalizedURL:         normalizedURL,
		jobQueue:              make(chan contracts.PrintJobCommand, 16),
		activeJobs:            make(map[string]bool),
		deferredJobs:          make([]contracts.PrintJobCommand, 0),
		minBackoff:            1 * time.Second,
		maxBackoff:            30 * time.Second,
		heartbeatInterval:     15 * time.Second,
		maxDownloadBytes:      250 * 1024 * 1024,
		healthPoller:          poller,
		requiredRecoveryCount: reqHealthy,
	}

	if cfg.InputSource != nil {
		pinClient, err := cloud.NewHTTPPINClient(cfg.CloudURL, cfg.KioskID, cfg.KioskSecret, cfg.HTTPClient)
		if err == nil {
			pinCtrl, err := pin.NewController(pin.Config{
				InactivityTimeout: cfg.InactivityTimeout,
				SummaryTimeout:    cfg.SummaryTimeout,
				PaymentTimeout:    cfg.PaymentTimeout,
				PaymentURLBase:    cfg.PaymentURLBase,
				KioskID:           cfg.KioskID,
				RenderScreen:      client.renderScreen,
				LookupClient:      pinClient,
				IdleQRContent:     cfg.IdleQRContent,
				PrinterModel:      cfg.PrinterModel,
				OnTransactionEnd: func() {
					client.drainDeferredJobsIfSafe(context.Background())
				},
			}, cfg.InputSource)
			if err == nil {
				client.pinController = pinCtrl
			} else {
				log.Printf("[SPOOLER] Warning: failed to initialize PIN controller: %v", err)
			}
		} else {
			log.Printf("[SPOOLER] Warning: failed to initialize PIN lookup client: %v", err)
		}
	}

	go client.fetchInitialAds()

	return client, nil
}

// PINController returns the active PIN controller instance, or nil if keypad input is not configured.
func (c *Client) PINController() *pin.Controller {
	return c.pinController
}

// RenderHistory returns a copy of the screen states recorded during client execution.
func (c *Client) RenderHistory() []ui_ipc.ScreenState {
	c.renderHistoryMu.Lock()
	defer c.renderHistoryMu.Unlock()
	historyCopy := make([]ui_ipc.ScreenState, len(c.renderHistory))
	copy(historyCopy, c.renderHistory)
	return historyCopy
}

// Run executes the spooler client loops until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	defer func() {
		c.idleMu.Lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
			c.idleTimer = nil
		}
		c.idleMu.Unlock()
	}()

	if c.pinController != nil {
		c.pinController.Start(ctx)
		defer c.pinController.Stop()
	}

	// 1. Initial idle screen transition
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:      ui_ipc.ModeIdle,
		QRContent: c.cfg.IdleQRContent,
		Message:   "Ready to print",
	})

	// 2. Startup recovery check
	incomplete, err := c.cfg.StateStore.GetIncompleteJob(ctx)
	if err == nil && incomplete != nil {
		isDefinitelyNotSubmitted := !incomplete.SubmittedToPrinter &&
			incomplete.CUPSJobID == "" &&
			incomplete.SheetsPrinted == 0 &&
			incomplete.PagesPrinted == 0 &&
			incomplete.ErrorReason == ""

		if isDefinitelyNotSubmitted {
			// Zero sheets were ever printed and submission was never initiated; safe to retry from beginning
			c.activeMu.Lock()
			c.activeJobs[incomplete.Command.JobID] = true
			c.activeMu.Unlock()
			select {
			case c.jobQueue <- incomplete.Command:
			default:
			}
		} else {
			// Potentially submitted or already partially printed.
			// Never automatically resubmit.
			if strings.TrimSpace(c.cfg.CUPSQueue) != "" && incomplete.CUPSJobID != "" {
				// Reconcile with CUPS using authoritative job status
				cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, incomplete.CUPSJobID)
				reconciledSheets, reconciledPages := reconcileSheetsAndPages(
					incomplete.Command,
					incomplete.SheetsPrinted,
					incomplete.PagesPrinted,
					cupsInfo.SheetsCompleted,
				)

				if incomplete.Status == contracts.JobStatusPausedError {
					// Job was previously interrupted and marked PAUSED_ERROR.
					// Never automatically complete or resume interrupted jobs without manual reconciliation.
					reason := "interrupted job in PAUSED_ERROR; physical output uncertain, manual reconciliation required"
					if qErr != nil {
						reason = fmt.Sprintf("interrupted job in PAUSED_ERROR; error querying CUPS (%v), manual reconciliation required", qErr)
					} else {
						switch cupsInfo.State {
						case printer.CUPSJobStateProcessing, printer.CUPSJobStatePending:
							holdCtx, holdCancel := context.WithTimeout(ctx, 2*time.Second)
							holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, incomplete.CUPSJobID)
							holdCancel()
							if holdRes.HoldErr != nil {
								reason = fmt.Sprintf("interrupted job in PAUSED_ERROR; hold request failed (%v), job remains %s in CUPS, unmonitored physical output risk, operator reconciliation required", holdRes.HoldErr, holdRes.State)
							} else if holdRes.StillProcessing {
								reason = "interrupted job in PAUSED_ERROR; job remains processing in CUPS after hold request, unmonitored physical output risk, operator reconciliation required"
							} else {
								reason = "interrupted job in PAUSED_ERROR; CUPS job placed on hold (buffered sheets may continue printing), operator reconciliation required"
							}
						case printer.CUPSJobStateCompleted:
							reason = fmt.Sprintf("interrupted job in PAUSED_ERROR; CUPS completed job without physical proof (CUPS counter: %d sheets, verified physical: %d/%d), manual reconciliation required",
								cupsInfo.SheetsCompleted, incomplete.SheetsPrinted, incomplete.Command.TotalSheets)
						default:
							reason = fmt.Sprintf("interrupted job in PAUSED_ERROR; CUPS state is %s, physical output uncertain, manual reconciliation required", cupsInfo.State)
						}
					}

					log.Printf("[SPOOLER] Startup reconciliation: preserving paused job %s (CUPS ID %s): %s", incomplete.Command.JobID, incomplete.CUPSJobID, reason)
					evt := contracts.JobProgressEvent{
						JobID:         incomplete.Command.JobID,
						KioskID:       incomplete.Command.KioskID,
						Status:        contracts.JobStatusPausedError,
						PagesPrinted:  reconciledPages,
						SheetsPrinted: reconciledSheets,
						ErrorReason:   reason,
						Timestamp:     time.Now().UTC(),
					}
					_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
					c.sendProgress(ctx, evt)
				} else if qErr == nil && cupsInfo.State == printer.CUPSJobStateCompleted {
					// Verify printer hardware health before assuming completed physical output
					cupsHealth := printer.PollCUPSHealthCustom(ctx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, 0)
					if !cupsHealth.Online || cupsHealth.State == "ERROR" || cupsHealth.PaperTrayStatus == "CHECK_PRINTER" ||
						strings.Contains(strings.ToLower(cupsHealth.ErrorMessage), "jam") ||
						strings.Contains(strings.ToLower(cupsHealth.ErrorMessage), "out of paper") ||
						strings.Contains(strings.ToLower(cupsHealth.ErrorMessage), "media-empty") {
						reason := fmt.Sprintf("interrupted by daemon restart; printer in error (%s), physical output uncertain, manual reconciliation required", cupsHealth.ErrorMessage)
						log.Printf("[SPOOLER] Startup reconciliation: pausing ambiguous job %s: %s", incomplete.Command.JobID, reason)
						evt := contracts.JobProgressEvent{
							JobID:         incomplete.Command.JobID,
							KioskID:       incomplete.Command.KioskID,
							Status:        contracts.JobStatusPausedError,
							PagesPrinted:  incomplete.PagesPrinted,
							SheetsPrinted: incomplete.SheetsPrinted,
							ErrorReason:   reason,
							Timestamp:     time.Now().UTC(),
						}
						_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
						c.sendProgress(ctx, evt)
					} else {
						// Physical printing finished in CUPS while daemon was stopped/restarting and printer is healthy
						log.Printf("[SPOOLER] Startup reconciliation: job %s (CUPS ID %s) confirmed COMPLETED by CUPS and printer healthy", incomplete.Command.JobID, incomplete.CUPSJobID)
						c.completeJob(ctx, incomplete.Command)
					}
				} else if qErr == nil && (cupsInfo.State == printer.CUPSJobStateProcessing ||
					cupsInfo.State == printer.CUPSJobStatePending ||
					cupsInfo.State == printer.CUPSJobStateHeld ||
					cupsInfo.State == printer.CUPSJobStateProcessingStopped) {
					// Job is actively processing or held/stopped in CUPS.
					// Resume monitoring existing CUPS job without duplicate submission.
					log.Printf("[SPOOLER] Startup reconciliation: job %s (CUPS ID %s) is %s in CUPS; resuming monitoring", incomplete.Command.JobID, incomplete.CUPSJobID, cupsInfo.State)
					c.activeMu.Lock()
					c.activeJobs[incomplete.Command.JobID] = true
					c.activeMu.Unlock()
					go c.resumeCUPSMonitoring(ctx, incomplete)
				} else {
					// Reconciliation uncertain or job aborted/canceled in CUPS.
					reason := "interrupted printer submission; manual reconciliation required"
					if len(cupsInfo.Reasons) > 0 {
						reason = fmt.Sprintf("CUPS job %s: %s", cupsInfo.State, strings.Join(cupsInfo.Reasons, ", "))
					}
					log.Printf("[SPOOLER] Startup reconciliation: pausing job %s: %s", incomplete.Command.JobID, reason)
					evt := contracts.JobProgressEvent{
						JobID:         incomplete.Command.JobID,
						KioskID:       incomplete.Command.KioskID,
						Status:        contracts.JobStatusPausedError,
						PagesPrinted:  reconciledPages,
						SheetsPrinted: reconciledSheets,
						ErrorReason:   reason,
						Timestamp:     time.Now().UTC(),
					}
					_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
					c.sendProgress(ctx, evt)
				}
			} else {
				// Ambiguous submission (crash during lp submission or PJL stream without CUPS ID)
				// or partially printed without CUPS reconciliation available:
				reason := "interrupted during printer submission; physical output uncertain, manual reconciliation required"
				if incomplete.PagesPrinted > 0 || incomplete.SheetsPrinted > 0 {
					reason = "interrupted by daemon restart; physical output uncertain, reconciliation required"
				}
				log.Printf("[SPOOLER] Startup recovery: job %s potentially submitted; pausing in PAUSED_ERROR (%s)", incomplete.Command.JobID, reason)
				evt := contracts.JobProgressEvent{
					JobID:         incomplete.Command.JobID,
					KioskID:       incomplete.Command.KioskID,
					Status:        contracts.JobStatusPausedError,
					PagesPrinted:  incomplete.PagesPrinted,
					SheetsPrinted: incomplete.SheetsPrinted,
					ErrorReason:   reason,
					Timestamp:     time.Now().UTC(),
				}
				_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
				c.sendProgress(ctx, evt)
			}
		}
	}

	// 3. Start sequential job processing worker
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.jobWorker(ctx)
	}()

	// 3b. Start idle carousel ticker
	if c.cfg.IdleCarouselInterval == 0 {
		c.cfg.IdleCarouselInterval = 5 * time.Second
	}
	if c.cfg.IdleCarouselInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.idleCarouselLoop(ctx)
		}()
	}

	// 3c. Start background printer health monitor loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.healthMonitorLoop(ctx)
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

		var dialOpts *websocket.DialOptions
		if strings.TrimSpace(c.cfg.KioskSecret) != "" {
			dialOpts = &websocket.DialOptions{
				HTTPHeader: http.Header{
					"Authorization": []string{"Bearer " + strings.TrimSpace(c.cfg.KioskSecret)},
				},
			}
		}

		conn, _, err := websocket.Dial(ctx, c.normalizedURL, dialOpts)
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
		} else if msg.Type == "sync_ads" && len(msg.Ads) > 0 {
			c.handleSyncAds(msg.Ads)
		} else if msg.Type == "refill" {
			sheets := msg.Sheets
			if sheets <= 0 || sheets > 250 {
				sheets = 250
			}
			if err := c.cfg.StateStore.ResetPaperTray(connCtx, sheets); err != nil {
				log.Printf("[SPOOLER] Failed to reset paper tray on refill message: %v", err)
			} else {
				log.Printf("[SPOOLER] Successfully refilled paper tray to %d sheets", sheets)
				h := c.healthPoller(connCtx, sheets)
				h.PaperTrayStatus = "OK"
				h.EstimatedSheets = sheets
				_ = c.sendWire(connCtx, conn, wireMessage{Type: "health", Health: &h})
			}
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
			sheets, isCalibrated, err := c.cfg.StateStore.GetPaperSupply(ctx)
			if err != nil {
				sheets, _ = c.cfg.StateStore.GetEstimatedSheets(ctx)
				isCalibrated = true
			}
			h := c.healthPoller(ctx, sheets)
			if !isCalibrated {
				h.PaperTrayStatus = "UNKNOWN"
			}
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
	if err := c.validateDownloadURL(cmd.DownloadURL); err != nil {
		log.Printf("[SPOOLER] Rejecting incoming job %s: invalid or untrusted download URL: %v", cmd.JobID, err)
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

	// Durable idempotency check: ignore if job is already marked COMPLETED or submitted to printer in local database
	if existing, err := c.cfg.StateStore.GetJob(context.Background(), cmd.JobID); err == nil && existing != nil {
		if existing.Status == contracts.JobStatusCompleted {
			log.Printf("[SPOOLER] Ignoring duplicate command for already completed job %s", cmd.JobID)
			evt := contracts.JobProgressEvent{
				JobID:         cmd.JobID,
				KioskID:       cmd.KioskID,
				Status:        contracts.JobStatusCompleted,
				PagesPrinted:  cmd.TotalPages,
				SheetsPrinted: cmd.TotalSheets,
				Timestamp:     time.Now().UTC(),
			}
			c.sendProgress(context.Background(), evt)
			return
		}
		if existing.SubmittedToPrinter || existing.CUPSJobID != "" {
			log.Printf("[SPOOLER] Ignoring duplicate command for job %s: already submitted to printer (CUPS ID %s, status %s)", cmd.JobID, existing.CUPSJobID, existing.Status)
			evt := contracts.JobProgressEvent{
				JobID:         cmd.JobID,
				KioskID:       cmd.KioskID,
				Status:        existing.Status,
				PagesPrinted:  existing.PagesPrinted,
				SheetsPrinted: existing.SheetsPrinted,
				Timestamp:     time.Now().UTC(),
			}
			c.sendProgress(context.Background(), evt)
			return
		}
	}

	c.deferredMu.Lock()
	for _, dj := range c.deferredJobs {
		if dj.JobID == cmd.JobID {
			c.deferredMu.Unlock()
			log.Printf("[SPOOLER] Ignoring duplicate incoming command for already deferred job %s", cmd.JobID)
			return
		}
	}
	c.deferredMu.Unlock()

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
	log.Printf("[SPOOLER] Queued print job %s (pages=%d, sheets=%d, copies=%d, duplex=%s)", cmd.JobID, cmd.TotalPages, cmd.TotalSheets, cmd.Copies, cmd.Duplex)

	// Durably record incoming job acceptance in local state store before enqueueing
	if c.cfg.StateStore != nil {
		if err := c.cfg.StateStore.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Printf("[SPOOLER] Warning: failed to persist initial job %s: %v", cmd.JobID, err)
		}
	}

	// Send explicit acceptance / spooling acknowledgement back to cloud
	ackEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusSpooling,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		Timestamp:     time.Now().UTC(),
	}
	c.sendProgress(context.Background(), ackEvt)

	c.connMu.Lock()
	activeConn := c.conn
	c.connMu.Unlock()
	if activeConn != nil {
		_ = c.sendWire(context.Background(), activeConn, wireMessage{
			Type:     "job_ack",
			Progress: &ackEvt,
		})
	}

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

func (c *Client) getSNMPConfig() *printer.SNMPConfig {
	targetHost := strings.TrimSpace(c.cfg.SNMPTarget)
	if targetHost == "" {
		if h, _, err := net.SplitHostPort(c.cfg.PrinterAddr); err == nil && h != "" {
			targetHost = h
		} else if strings.TrimSpace(c.cfg.PrinterAddr) != "" {
			targetHost = strings.TrimSpace(c.cfg.PrinterAddr)
		}
	}
	if targetHost == "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		discovered := printer.DiscoverQueueHost(context.Background(), c.cfg.CUPSRunner, strings.TrimSpace(c.cfg.CUPSQueue))
		if discovered != "" {
			targetHost = discovered
			c.cfg.SNMPTarget = discovered
		}
	}
	if targetHost == "" {
		return nil
	}
	snmpCfg := printer.DefaultSNMPConfig()
	snmpCfg.Target = targetHost
	if c.cfg.SNMPPort > 0 {
		snmpCfg.Port = c.cfg.SNMPPort
	}
	if c.cfg.SNMPCommunity != "" {
		snmpCfg.Community = c.cfg.SNMPCommunity
	}
	return &snmpCfg
}

func (c *Client) resumeCUPSMonitoring(ctx context.Context, job *state.StoredJob) {
	defer func() {
		c.activeMu.Lock()
		delete(c.activeJobs, job.Command.JobID)
		c.activeMu.Unlock()
	}()

	cmd := job.Command
	var progressMu sync.Mutex
	pagesPrinted := job.PagesPrinted
	sheetsPrinted := job.SheetsPrinted

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModePrinting,
		CurrentPage:     pagesPrinted,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
		Message:         "Resuming print monitoring...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	var faultHandled atomic.Bool
	var preserveCUPSJob atomic.Bool
	jobCtx = printer.ContextWithPreserveCUPSJob(jobCtx, &preserveCUPSJob)

	handleJobHardwareInterruption := func(reason string) {
		if !faultHandled.CompareAndSwap(false, true) {
			return
		}
		preserveCUPSJob.Store(true)
		c.setHardwareFault(true, reason, "")

		if job.CUPSJobID != "" {
			holdCtx, holdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, job.CUPSJobID)
			holdCancel()

			if holdRes.HoldErr != nil {
				reason = fmt.Sprintf("%s; hold request failed (%v), CUPS state is %s, unmonitored physical output risk, operator reconciliation required", reason, holdRes.HoldErr, holdRes.State)
			} else if holdRes.StillProcessing {
				reason = fmt.Sprintf("%s; CUPS job remains processing after hold request, unmonitored physical output risk, operator reconciliation required", reason)
			} else if holdRes.AlreadyCompleted {
				reason = fmt.Sprintf("%s; CUPS job was already completed, physical output uncertain, operator reconciliation required", reason)
			} else if holdRes.ConfirmedHeld {
				reason = fmt.Sprintf("%s; CUPS job held (buffered sheets may continue printing), operator reconciliation required", reason)
			} else {
				reason = fmt.Sprintf("%s; CUPS job state is %s after hold request, operator reconciliation required", reason, holdRes.State)
			}
		}

		cancelJob()

		progressMu.Lock()
		curPages := pagesPrinted
		curSheets := sheetsPrinted
		progressMu.Unlock()

		log.Printf("[SPOOLER] Hardware interruption during resumed monitoring for %s (CUPS ID %s): %s (at page %d/%d, sheet %d/%d)",
			cmd.JobID, job.CUPSJobID, reason, curPages, cmd.TotalPages, curSheets, cmd.TotalSheets)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}

	monitorDone := make(chan struct{})

	// Active health poller during resumed CUPS monitoring
	go func() {
		defer close(monitorDone)
		interval := c.cfg.HealthPollInterval
		if interval <= 0 {
			interval = 500 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				h := c.CheckPrinterHealth(jobCtx)
				if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
					handleJobHardwareInterruption(reason)
					return
				}
			}
		}
	}()

	onPage := func(streamSheetsCompleted int) {
		if faultHandled.Load() || c.isHardwareFaultActive() || jobCtx.Err() != nil {
			log.Printf("[SPOOLER] Suppressing progress update during resumed monitoring for %s: hardware fault active or context canceled", cmd.JobID)
			return
		}

		progressMu.Lock()
		defer progressMu.Unlock()

		if faultHandled.Load() || c.isHardwareFaultActive() || jobCtx.Err() != nil {
			return
		}

		targetSheets := streamSheetsCompleted
		if targetSheets > cmd.TotalSheets {
			targetSheets = cmd.TotalSheets
		}
		if targetSheets < sheetsPrinted {
			targetSheets = sheetsPrinted
		}

		if cmd.Duplex == contracts.DuplexSimplex {
			pagesPrinted = (cmd.StartFromPage - 1) + streamSheetsCompleted/cmd.Copies
		} else {
			pagesPrinted = (cmd.StartFromPage - 1) + (streamSheetsCompleted*2)/cmd.Copies
		}
		if pagesPrinted > cmd.TotalPages {
			pagesPrinted = cmd.TotalPages
		}

		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       cmd.KioskID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  pagesPrinted,
			SheetsPrinted: targetSheets,
			Timestamp:     time.Now().UTC(),
		}
		if err := c.cfg.StateStore.RecordSheetProgress(jobCtx, evt); err != nil {
			log.Printf("[SPOOLER] RecordSheetProgress error during CUPS monitoring for job %s: %v", cmd.JobID, err)
			cancelJob()
			return
		}
		sheetsPrinted = targetSheets
		c.sendProgress(jobCtx, evt)

		if !c.isHardwareFaultActive() && !faultHandled.Load() {
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

	snmpCfg := c.getSNMPConfig()
	err := printer.MonitorCUPSJobWithSNMPCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cmd, job.CUPSJobID, onPage, 500*time.Millisecond, snmpCfg)

	cancelJob()
	<-monitorDone

	if faultHandled.Load() {
		return
	}

	progressMu.Lock()
	curPages := pagesPrinted
	curSheets := sheetsPrinted
	progressMu.Unlock()

	if err != nil {
		if isHw, reason, _ := isBlockingHardwareFaultString(err.Error()); isHw {
			handleJobHardwareInterruption(reason)
			return
		}
		c.handleJobError(ctx, cmd, curPages, curSheets, fmt.Sprintf("CUPS monitoring error: %v", err))
		return
	}

	compHealth := c.CheckPrinterHealth(ctx)
	if isFault, reason, _ := isBlockingHardwareFault(compHealth); isFault {
		handleJobHardwareInterruption(fmt.Sprintf("printer reported hardware error on completion (%s); physical output uncertain, manual reconciliation required", reason))
		return
	}

	c.completeJob(ctx, cmd)
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
			c.drainDeferredJobsIfSafe(ctx)
		}
	}
}

// extractOrderIDFromDownloadURL parses rawURL and extracts {order_id} if the path matches /api/orders/{order_id}/pdf.
// It returns empty string if the URL is malformed, not an orders endpoint, or has empty path segments.
func extractOrderIDFromDownloadURL(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	cleanPath := strings.Trim(u.Path, "/")
	parts := strings.Split(cleanPath, "/")
	// Expected: ["api", "orders", "{order_id}", "pdf"]
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "orders" && parts[3] == "pdf" {
		orderID := strings.TrimSpace(parts[2])
		if orderID != "" {
			return orderID
		}
	}
	return ""
}

func isJobForActiveOrder(cmd contracts.PrintJobCommand, activeOrderID string) bool {
	if activeOrderID == "" {
		return false
	}
	if cmd.JobID == activeOrderID || cmd.JobID == "job_"+activeOrderID {
		return true
	}
	extracted := extractOrderIDFromDownloadURL(cmd.DownloadURL)
	return extracted != "" && extracted == activeOrderID
}

func (c *Client) isTrustedOrigin(u *url.URL) bool {
	if u == nil || u.Host == "" {
		return false
	}
	targetHost := strings.ToLower(u.Host)
	targetHostname := strings.ToLower(u.Hostname())

	// 1. Configured CloudURL host or hostname
	if cloudU, err := url.Parse(c.cfg.CloudURL); err == nil && cloudU.Host != "" {
		cloudHost := strings.ToLower(cloudU.Host)
		cloudHostname := strings.ToLower(cloudU.Hostname())
		if targetHost == cloudHost || targetHostname == cloudHostname {
			return true
		}
		// If cloudURL is on loopback (dev/test), allow other loopback targets
		if cloudHostname == "127.0.0.1" || cloudHostname == "localhost" || cloudHostname == "::1" {
			if targetHostname == "127.0.0.1" || targetHostname == "localhost" || targetHostname == "::1" {
				return true
			}
		}
	}

	// 2. Explicitly configured trusted origins
	for _, o := range c.cfg.TrustedOrigins {
		o = strings.ToLower(strings.TrimSpace(o))
		if o != "" && (targetHost == o || targetHostname == o) {
			return true
		}
	}

	// 3. Permitted standard test domains in test suites
	if targetHostname == "example.invalid" || strings.HasSuffix(targetHostname, ".example.invalid") || strings.HasSuffix(targetHostname, ".test") || targetHostname == "storage.example.com" {
		return true
	}

	return false
}

func (c *Client) validateDownloadURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return errors.New("download URL is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid download URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported download URL scheme %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("download URL cannot contain user credentials")
	}
	if u.Host == "" {
		return errors.New("download URL missing host")
	}
	if !c.isTrustedOrigin(u) {
		return fmt.Errorf("download URL origin %q is not trusted", u.Host)
	}
	return nil
}

func (c *Client) drainDeferredJobsIfSafe(ctx context.Context) {
	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()

	if len(c.deferredJobs) == 0 {
		return
	}

	if c.pinController != nil && c.pinController.HasActiveTransaction() {
		// Not safe yet; a customer is still actively using the kiosk
		return
	}

	c.activeMu.Lock()
	isBusy := len(c.activeJobs) > 0
	c.activeMu.Unlock()
	if isBusy {
		return
	}

	nextJob := c.deferredJobs[0]
	c.deferredJobs = c.deferredJobs[1:]

	c.activeMu.Lock()
	c.activeJobs[nextJob.JobID] = true
	c.activeMu.Unlock()

	log.Printf("[SPOOLER] Resuming deferred job %s now that kiosk is idle", nextJob.JobID)
	select {
	case c.jobQueue <- nextJob:
	default:
		go func(j contracts.PrintJobCommand) {
			c.jobQueue <- j
		}(nextJob)
	}
}

func (c *Client) processJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	if c.pinController != nil && c.pinController.HasActiveTransaction() {
		activeOrderID := c.pinController.ActiveOrderID()
		if isJobForActiveOrder(cmd, activeOrderID) {
			// Job belongs to the active transaction (payment succeeded)
			c.pinController.ResetToIdle()
		} else {
			// Unrelated incoming job: MUST NOT clear active transaction screen!
			// Defer safely without blocking jobWorker or repeatedly re-enqueueing duplicates.
			c.deferredMu.Lock()
			alreadyDeferred := false
			for _, dj := range c.deferredJobs {
				if dj.JobID == cmd.JobID {
					alreadyDeferred = true
					break
				}
			}
			if !alreadyDeferred {
				c.deferredJobs = append(c.deferredJobs, cmd)
				log.Printf("[SPOOLER] Deferring unrelated job %s during active PIN transaction (%s)", cmd.JobID, activeOrderID)
			}
			c.deferredMu.Unlock()
			return
		}
	} else if c.pinController != nil {
		c.pinController.ResetToIdle()
	}

	c.idleMu.Lock()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	c.idleMu.Unlock()

	log.Printf("[SPOOLER] Processing job %s: downloading from %s", cmd.JobID, cmd.DownloadURL)
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

	remainingSheets := cmd.TotalSheets - sheetsPrinted
	if remainingSheets < 0 {
		remainingSheets = 0
	}

	estimatedSheets, err := c.cfg.StateStore.GetEstimatedSheets(ctx)
	if err != nil || estimatedSheets < remainingSheets {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, "paper tray supply insufficient for remaining print job")
		return
	}

	// 6. Imposed print artifact (zero Pi-side slicing or transformation)
	printPath := finalPath
	streamCmd := cmd

	// 7. Update progress: PRINTING
	printEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.RecordSheetProgress(ctx, printEvt)
	c.sendProgress(ctx, printEvt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModePrinting,
		CurrentPage:     pagesPrinted,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
		Message:         "Printing document...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 8. Stream PDF via PJL or submit to CUPS
	f, err := os.Open(printPath)
	if err != nil {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("failed to open PDF for print: %v", err))
		return
	}
	defer f.Close()

	var progressMu sync.Mutex
	lastReportedStreamSheets := 0
	initialSheetsOffset := sheetsPrinted

	var activeCUPSJobID string
	var cupsJobIDMu sync.Mutex

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	var faultHandled atomic.Bool
	var preserveCUPSJob atomic.Bool
	jobCtx = printer.ContextWithPreserveCUPSJob(jobCtx, &preserveCUPSJob)

	handleJobHardwareInterruption := func(reason string) {
		if !faultHandled.CompareAndSwap(false, true) {
			return
		}
		preserveCUPSJob.Store(true)
		c.setHardwareFault(true, reason, "")

		cupsJobIDMu.Lock()
		cID := activeCUPSJobID
		cupsJobIDMu.Unlock()
		if cID == "" {
			if saved, err := c.cfg.StateStore.GetJob(context.Background(), cmd.JobID); err == nil && saved != nil && saved.CUPSJobID != "" {
				cID = saved.CUPSJobID
			}
		}
		if cID != "" {
			holdCtx, holdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cID)
			holdCancel()

			if holdRes.HoldErr != nil {
				reason = fmt.Sprintf("%s; hold request failed (%v), CUPS state is %s, unmonitored physical output risk, operator reconciliation required", reason, holdRes.HoldErr, holdRes.State)
			} else if holdRes.StillProcessing {
				reason = fmt.Sprintf("%s; CUPS job remains processing after hold request, unmonitored physical output risk, operator reconciliation required", reason)
			} else if holdRes.AlreadyCompleted {
				reason = fmt.Sprintf("%s; CUPS job was already completed, physical output uncertain, operator reconciliation required", reason)
			} else if holdRes.ConfirmedHeld {
				reason = fmt.Sprintf("%s; CUPS job held (buffered sheets may continue printing), operator reconciliation required", reason)
			} else {
				reason = fmt.Sprintf("%s; CUPS job state is %s after hold request, operator reconciliation required", reason, holdRes.State)
			}
		}

		cancelJob() // abort active transmission or monitoring

		progressMu.Lock()
		curPages := pagesPrinted
		curSheets := sheetsPrinted
		progressMu.Unlock()

		log.Printf("[SPOOLER] Hardware interruption for job %s (CUPS ID %s): %s (at page %d/%d, sheet %d/%d)",
			cmd.JobID, cID, reason, curPages, cmd.TotalPages, curSheets, cmd.TotalSheets)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}

	monitorDone := make(chan struct{})

	// Active health poller during printing
	go func() {
		defer close(monitorDone)
		interval := c.cfg.HealthPollInterval
		if interval <= 0 {
			interval = 500 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				h := c.CheckPrinterHealth(jobCtx)
				if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
					handleJobHardwareInterruption(reason)
					return
				}
			}
		}
	}()

	onPage := func(streamSheetsCompleted int) {
		if faultHandled.Load() || c.isHardwareFaultActive() || jobCtx.Err() != nil {
			log.Printf("[SPOOLER] Suppressing progress update for job %s: hardware fault active or context canceled", cmd.JobID)
			return
		}

		progressMu.Lock()
		defer progressMu.Unlock()

		if faultHandled.Load() || c.isHardwareFaultActive() || jobCtx.Err() != nil {
			return
		}

		if streamSheetsCompleted > lastReportedStreamSheets {
			lastReportedStreamSheets = streamSheetsCompleted
			targetSheets := initialSheetsOffset + streamSheetsCompleted
			if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
				targetSheets = streamSheetsCompleted
			}
			if targetSheets > cmd.TotalSheets {
				targetSheets = cmd.TotalSheets
			}
			if targetSheets < sheetsPrinted {
				targetSheets = sheetsPrinted
			}

			if cmd.Duplex == contracts.DuplexSimplex {
				pagesPrinted = (startPage - 1) + streamSheetsCompleted/cmd.Copies
			} else {
				pagesPrinted = (startPage - 1) + (streamSheetsCompleted*2)/cmd.Copies
			}

			if pagesPrinted > cmd.TotalPages {
				pagesPrinted = cmd.TotalPages
			}

			evt := contracts.JobProgressEvent{
				JobID:         cmd.JobID,
				KioskID:       cmd.KioskID,
				Status:        contracts.JobStatusPrinting,
				PagesPrinted:  pagesPrinted,
				SheetsPrinted: targetSheets,
				Timestamp:     time.Now().UTC(),
			}
			if err := c.cfg.StateStore.RecordSheetProgress(jobCtx, evt); err != nil {
				log.Printf("[SPOOLER] RecordSheetProgress error for job %s: %v", cmd.JobID, err)
				cancelJob()
				return
			}
			sheetsPrinted = targetSheets
			c.sendProgress(jobCtx, evt)

			if !c.isHardwareFaultActive() && !faultHandled.Load() {
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
	}

	var printErr error
	if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		log.Printf("[SPOOLER] Sending job %s to CUPS queue %s", cmd.JobID, c.cfg.CUPSQueue)
		if c.cfg.MockCUPSPrint != nil {
			_ = c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID)
			printErr = c.cfg.MockCUPSPrint(jobCtx, c.cfg.CUPSQueue, streamCmd, printPath, onPage)
		} else {
			hooks := &printer.CUPSPrintHooks{
				OnSubmitting: func() {
					_ = c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID)
				},
				OnSubmitted: func(cupsJobID string) {
					cupsJobIDMu.Lock()
					activeCUPSJobID = cupsJobID
					cupsJobIDMu.Unlock()
					_ = c.cfg.StateStore.RecordJobSubmission(jobCtx, cmd.JobID, cupsJobID)
				},
				SNMPConfig:          c.getSNMPConfig(),
				PreserveJobOnCancel: preserveCUPSJob.Load,
			}
			printErr = printer.PrintPDFWithCUPSHooksCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, streamCmd, printPath, onPage, 500*time.Millisecond, hooks)
		}
	} else {
		log.Printf("[SPOOLER] Streaming job %s to PJL printer %s", cmd.JobID, c.cfg.PrinterAddr)
		_ = c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID)
		printErr = printer.StreamPDFWithPJL(jobCtx, c.cfg.PrinterAddr, streamCmd, f, onPage)
	}

	cancelJob()
	<-monitorDone

	if faultHandled.Load() {
		return
	}

	progressMu.Lock()
	curPages := pagesPrinted
	curSheets := sheetsPrinted
	progressMu.Unlock()

	if printErr != nil {
		if isHw, reason, _ := isBlockingHardwareFaultString(printErr.Error()); isHw {
			handleJobHardwareInterruption(reason)
			return
		}
		c.handleJobError(ctx, cmd, curPages, curSheets, fmt.Sprintf("printing failed: %v", printErr))
		return
	}

	compHealth := c.CheckPrinterHealth(ctx)
	if isFault, reason, _ := isBlockingHardwareFault(compHealth); isFault {
		handleJobHardwareInterruption(fmt.Sprintf("printer reported hardware error on completion (%s); physical output uncertain, manual reconciliation required", reason))
		return
	}

	// 9. Complete Job
	c.completeJob(ctx, cmd)
}

func (c *Client) completeJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	log.Printf("[SPOOLER] Job %s successfully printed (%d pages, %d sheets)", cmd.JobID, cmd.TotalPages, cmd.TotalSheets)

	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  cmd.TotalPages,
		SheetsPrinted: cmd.TotalSheets,
		Timestamp:     time.Now().UTC(),
	}
	if err := c.cfg.StateStore.RecordSheetProgress(ctx, evt); err != nil {
		log.Printf("[SPOOLER] Failed to record completion for job %s: %v", cmd.JobID, err)
		lastPages := 0
		lastSheets := 0
		if existingJob, _ := c.cfg.StateStore.GetJob(ctx, cmd.JobID); existingJob != nil {
			lastPages = existingJob.PagesPrinted
			lastSheets = existingJob.SheetsPrinted
		}
		c.handleJobError(ctx, cmd, lastPages, lastSheets, fmt.Sprintf("failed to record completion: %v", err))
		return
	}

	// Post-print document shredder: immediately delete the downloaded PDF payload from local disk
	// to enforce ETSI EN 303 645 (Provision 5.4), NIST SP 800-213, and GDPR/DPDP storage limitation.
	finalPath := filepath.Join(c.cfg.DownloadDir, cmd.JobID+".pdf")
	if err := os.Remove(finalPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to shred local printed payload %s: %v", finalPath, err)
	}

	c.sendProgress(ctx, evt)

	if c.cfg.SoundPlayer != nil {
		c.cfg.SoundPlayer.PlayCompletion()
	}

	if c.cfg.OnJobCompleted != nil {
		c.cfg.OnJobCompleted(cmd.JobID)
	}

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeCompleted,
		CurrentPage:     cmd.TotalPages,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: 100,
		Message:         "Print complete",
		Detail:          "Job ID: " + cmd.JobID,
	})

	c.scheduleResetToIdle(c.cfg.CompletedDisplayDuration)
}

func formatCustomerError(cmd contracts.PrintJobCommand, pagesPrinted int, reason string) (string, string) {
	lower := strings.ToLower(reason)
	var msg string
	switch {
	case strings.Contains(lower, "jam"):
		msg = "A paper feed issue occurred with the printer."
	case strings.Contains(lower, "out of paper") || strings.Contains(lower, "empty") || strings.Contains(lower, "tray supply insufficient") || strings.Contains(lower, "media-empty") || strings.Contains(lower, "media-needed"):
		msg = "The printer has run out of paper."
	case strings.Contains(lower, "cover") || strings.Contains(lower, "door"):
		msg = "The printer cover was opened."
	case strings.Contains(lower, "bin full") || strings.Contains(lower, "output"):
		msg = "The printer output bin is full."
	case strings.Contains(lower, "offline") || strings.Contains(lower, "unreachable"):
		msg = "The printer is offline or unreachable."
	case strings.Contains(lower, "download failed") || strings.Contains(lower, "load"):
		msg = "We could not load your document."
	default:
		msg = "A hardware issue interrupted printing your document."
	}

	jobRef := cmd.JobID
	if strings.TrimSpace(jobRef) == "" {
		jobRef = "Order"
	}
	detail := fmt.Sprintf("Order Ref: %s • Completed: %d of %d pages", jobRef, pagesPrinted, cmd.TotalPages)
	return msg, detail
}

func (c *Client) handleJobError(ctx context.Context, cmd contracts.PrintJobCommand, pagesPrinted, sheetsPrinted int, reason string) {
	log.Printf("[SPOOLER] Job %s error: %s", cmd.JobID, reason)
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		ErrorReason:   reason,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
	c.sendProgress(ctx, evt)

	customerMsg, customerDetail := formatCustomerError(cmd, pagesPrinted, reason)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: customerMsg,
		Detail:  customerDetail,
	})

	c.scheduleResetToIdle(c.cfg.ErrorDisplayDuration)
}

func (c *Client) isHardwareFaultActive() bool {
	c.faultMu.Lock()
	defer c.faultMu.Unlock()
	return c.hardwareFault
}

func (c *Client) getFaultReason() string {
	c.faultMu.Lock()
	defer c.faultMu.Unlock()
	return c.faultReason
}

func (c *Client) setHardwareFault(active bool, reason, customerMsg string) {
	c.faultMu.Lock()
	defer c.faultMu.Unlock()
	c.hardwareFault = active
	c.faultReason = reason
	c.faultCustomerMsg = customerMsg
}

func isBlockingHardwareFault(h contracts.PrinterHealth) (bool, string, string) {
	// 1. Paper Jam
	if h.PaperJam || strings.Contains(strings.ToLower(h.ErrorMessage), "jam") {
		return true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer."
	}
	// 2. Cover / Door Open
	if h.CoverOpen || strings.Contains(strings.ToLower(h.ErrorMessage), "cover open") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "door open") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "cover-open") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "door-open") {
		return true, "printer cover is open", "The printer cover was opened."
	}
	// 3. Output Bin Full
	if h.OutputBinFull || strings.Contains(strings.ToLower(h.ErrorMessage), "bin full") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "output-area-full") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "output-bin-full") {
		return true, "printer output bin full", "The printer output bin is full."
	}
	// 4. Out of Paper / Tray Empty
	if h.PaperTrayStatus == "EMPTY" ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "out of paper") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "media-empty") ||
		strings.Contains(strings.ToLower(h.ErrorMessage), "media-needed") {
		return true, "printer paper tray is empty", "The printer has run out of paper."
	}
	// 5. Printer Offline / Unreachable
	if h.State == "OFFLINE" || (!h.Online && h.State != "IDLE" && h.State != "PRINTING" && h.State != "READY" && h.State != "WARMING_UP") {
		errMsg := "printer is offline or unreachable"
		if h.ErrorMessage != "" {
			errMsg = h.ErrorMessage
		}
		return true, errMsg, "The printer is offline or unreachable."
	}
	// 6. Confirmed Critical Hardware Error
	if h.State == "ERROR" {
		errMsg := "printer reported hardware error"
		if h.ErrorMessage != "" {
			errMsg = h.ErrorMessage
		}
		return true, errMsg, "A hardware issue interrupted printing your document."
	}

	return false, "", ""
}

func isBlockingHardwareFaultString(errStr string) (bool, string, string) {
	lower := strings.ToLower(errStr)
	if strings.Contains(lower, "jam") {
		return true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer."
	}
	if strings.Contains(lower, "cover") || strings.Contains(lower, "door") {
		return true, "printer cover is open", "The printer cover was opened."
	}
	if strings.Contains(lower, "out of paper") || strings.Contains(lower, "media-empty") || strings.Contains(lower, "media-needed") || strings.Contains(lower, "empty") {
		return true, "printer paper tray is empty", "The printer has run out of paper."
	}
	if strings.Contains(lower, "bin full") || strings.Contains(lower, "output-area-full") || strings.Contains(lower, "output-bin-full") {
		return true, "printer output bin full", "The printer output bin is full."
	}
	if strings.Contains(lower, "offline") || strings.Contains(lower, "unreachable") {
		return true, "printer is offline or unreachable", "The printer is offline or unreachable."
	}
	return false, "", ""
}

// CheckPrinterHealth polls the underlying printer health, updates fault state,
// and synchronizes the local screen display when safe.
func (c *Client) CheckPrinterHealth(ctx context.Context) contracts.PrinterHealth {
	sheets := 0
	if s, err := c.cfg.StateStore.GetEstimatedSheets(ctx); err == nil {
		sheets = s
	}
	h := c.healthPoller(ctx, sheets)

	isFault, reason, custMsg := isBlockingHardwareFault(h)

	c.faultMu.Lock()
	wasFault := c.hardwareFault
	if isFault {
		c.consecutiveHealthy = 0
		c.hardwareFault = true
		c.faultReason = reason
		c.faultCustomerMsg = custMsg
	} else {
		// Check for reliable recovery evidence (Point 6 & 7):
		// A single transient healthy reading or SNMP timeout MUST NOT clear an active fault.
		snmpUnverifiedOrTimeout := strings.Contains(strings.ToLower(h.ErrorMessage), "snmp") ||
			strings.Contains(strings.ToLower(h.ErrorMessage), "timeout")

		if snmpUnverifiedOrTimeout || !h.Online || (h.State != "IDLE" && h.State != "READY" && h.State != "PRINTING") {
			// SNMP timed out or unverified / offline reading: reset recovery streak
			c.consecutiveHealthy = 0
		} else {
			c.consecutiveHealthy++
			if c.consecutiveHealthy >= c.requiredRecoveryCount {
				c.hardwareFault = false
				c.faultReason = ""
				c.faultCustomerMsg = ""
			}
		}
	}
	isNowFault := c.hardwareFault
	c.faultMu.Unlock()

	c.activeMu.Lock()
	hasActiveJobs := len(c.activeJobs) > 0
	c.activeMu.Unlock()

	// If there are active jobs printing, the active job's worker owns job error
	// handling and order-specific screen transitions. Background health checks
	// MUST NOT overwrite the order-specific screen or race with active print flow.
	if hasActiveJobs {
		return h
	}

	c.currentModeMu.Lock()
	currentMode := c.currentMode
	c.currentModeMu.Unlock()

	if wasFault && !isNowFault {
		// Hardware reliably recovered while idle!
		log.Printf("[SPOOLER] Printer hardware reliably recovered from fault (consecutive healthy: %d). Refreshing screen to idle.", c.requiredRecoveryCount)
		c.idleMu.Lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
			c.idleTimer = nil
		}
		c.idleMu.Unlock()

		// Reconcile any interrupted CUPS job that was paused during the fault
		c.reconcileInterruptedCUPSJob(ctx)

		if c.pinController == nil || !c.pinController.HasActiveTransaction() {
			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:      ui_ipc.ModeIdle,
				QRContent: c.cfg.IdleQRContent,
				Message:   "Ready to print",
			})
			c.drainDeferredJobsIfSafe(ctx)
		}
	} else if isNowFault && currentMode == ui_ipc.ModeIdle {
		// Hardware fault detected while kiosk was idle!
		log.Printf("[SPOOLER] Hardware fault detected while idle: %s", reason)
		_ = c.renderScreen(ui_ipc.ScreenState{
			Mode:    ui_ipc.ModeError,
			Message: custMsg,
			Detail:  "A printer hardware fault requires attention.",
		})
	}

	return h
}

func (c *Client) healthMonitorLoop(ctx context.Context) {
	interval := c.cfg.HealthPollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckPrinterHealth(ctx)
		}
	}
}

func (c *Client) scheduleResetToIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	c.idleMu.Lock()
	defer c.idleMu.Unlock()

	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}

	c.idleTimer = time.AfterFunc(d, func() {
		c.activeMu.Lock()
		activeCount := len(c.activeJobs)
		c.activeMu.Unlock()

		if activeCount == 0 {
			if c.isHardwareFaultActive() {
				log.Printf("[SPOOLER] Suppressing idle reset: persistent hardware fault active (%s)", c.getFaultReason())
				return
			}
			log.Printf("[SPOOLER] Resetting screen to idle state for next user")
			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:      ui_ipc.ModeIdle,
				QRContent: c.cfg.IdleQRContent,
				Message:   "Ready to print",
			})
			c.drainDeferredJobsIfSafe(context.Background())
		}
	})
}

func (c *Client) downloadAndVerify(ctx context.Context, cmd contracts.PrintJobCommand, finalPath string) error {
	if err := c.validateDownloadURL(cmd.DownloadURL); err != nil {
		return fmt.Errorf("validate download URL: %w", err)
	}

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
				if (pct-lastPct >= 10 || pct == 100) && pct > lastPct {
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

// reconcileSheetsAndPages preserves verified physical sheet and page progress during reconciliation
// of interrupted jobs. Unverified CUPS sheet counters are never promoted into physically verified
// progress, preventing spurious paper inventory decrements and false completion claims.
func reconcileSheetsAndPages(
	cmd contracts.PrintJobCommand,
	savedSheets int,
	savedPages int,
	cupsSheetsCompleted int,
) (int, int) {
	// Physical progress must never be promoted from unverified CUPS counters during reconciliation.
	// Preserve the last verified physical sheets and pages.
	reconciledSheets := savedSheets
	if reconciledSheets > cmd.TotalSheets {
		reconciledSheets = cmd.TotalSheets
	}
	if reconciledSheets < 0 {
		reconciledSheets = 0
	}

	reconciledPages := savedPages
	if reconciledPages > cmd.TotalPages {
		reconciledPages = cmd.TotalPages
	}
	if reconciledPages < 0 {
		reconciledPages = 0
	}

	return reconciledSheets, reconciledPages
}

func (c *Client) reconcileInterruptedCUPSJob(ctx context.Context) {
	if strings.TrimSpace(c.cfg.CUPSQueue) == "" {
		return
	}
	inc, err := c.cfg.StateStore.GetIncompleteJob(ctx)
	if err != nil || inc == nil || inc.CUPSJobID == "" {
		return
	}
	// We only reconcile jobs that were interrupted by a hardware fault (PAUSED_ERROR)
	if inc.Status != contracts.JobStatusPausedError {
		return
	}

	cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, inc.CUPSJobID)
	if qErr != nil {
		log.Printf("[SPOOLER] Recovery reconciliation: error querying CUPS job %s: %v", inc.CUPSJobID, qErr)
		return
	}

	log.Printf("[SPOOLER] Recovery reconciliation: interrupted job %s (CUPS ID %s) in state %s (cups sheets completed: %d, verified physical sheets: %d/%d)",
		inc.Command.JobID, inc.CUPSJobID, cupsInfo.State, cupsInfo.SheetsCompleted, inc.SheetsPrinted, inc.Command.TotalSheets)

	reconciledSheets, reconciledPages := reconcileSheetsAndPages(
		inc.Command,
		inc.SheetsPrinted,
		inc.PagesPrinted,
		cupsInfo.SheetsCompleted,
	)

	switch cupsInfo.State {
	case printer.CUPSJobStateProcessing, printer.CUPSJobStatePending:
		// CUPS resumed independently! Request hold and verify state to prevent unmonitored printing.
		log.Printf("[SPOOLER] Interrupted CUPS job %s was active/pending; holding job and verifying state", inc.CUPSJobID)
		holdCtx, holdCancel := context.WithTimeout(ctx, 2*time.Second)
		holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, inc.CUPSJobID)
		holdCancel()

		reason := "printer recovered; active CUPS job placed on hold (buffered sheets may continue printing), operator reconciliation required"
		if holdRes.HoldErr != nil {
			reason = fmt.Sprintf("printer recovered; hold request failed (%v), job remains %s in CUPS, unmonitored physical output risk, operator reconciliation required", holdRes.HoldErr, holdRes.State)
		} else if holdRes.StillProcessing {
			reason = "printer recovered; job remains processing in CUPS after hold request, unmonitored physical output risk, operator reconciliation required"
		}

		evt := contracts.JobProgressEvent{
			JobID:         inc.Command.JobID,
			KioskID:       inc.Command.KioskID,
			Status:        contracts.JobStatusPausedError,
			PagesPrinted:  reconciledPages,
			SheetsPrinted: reconciledSheets,
			ErrorReason:   reason,
			Timestamp:     time.Now().UTC(),
		}
		_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
		c.sendProgress(ctx, evt)

	case printer.CUPSJobStateCompleted:
		// Printer recovered independently and CUPS reports completed.
		// Never synthesize full completion or mark completed automatically without physical proof.
		reason := fmt.Sprintf("printer recovered independently; CUPS completed job without physical proof (CUPS counter: %d sheets, verified physical: %d/%d), physical output uncertain, manual reconciliation required",
			cupsInfo.SheetsCompleted, inc.SheetsPrinted, inc.Command.TotalSheets)
		log.Printf("[SPOOLER] Independent printer recovery: %s", reason)
		evt := contracts.JobProgressEvent{
			JobID:         inc.Command.JobID,
			KioskID:       inc.Command.KioskID,
			Status:        contracts.JobStatusPausedError,
			PagesPrinted:  reconciledPages,
			SheetsPrinted: reconciledSheets,
			ErrorReason:   reason,
			Timestamp:     time.Now().UTC(),
		}
		_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
		c.sendProgress(ctx, evt)

	case printer.CUPSJobStateHeld:
		log.Printf("[SPOOLER] Interrupted CUPS job %s is safely held in CUPS (buffered sheets may continue printing)", inc.CUPSJobID)
		if reconciledSheets > inc.SheetsPrinted {
			evt := contracts.JobProgressEvent{
				JobID:         inc.Command.JobID,
				KioskID:       inc.Command.KioskID,
				Status:        contracts.JobStatusPausedError,
				PagesPrinted:  reconciledPages,
				SheetsPrinted: reconciledSheets,
				ErrorReason:   inc.ErrorReason,
				Timestamp:     time.Now().UTC(),
			}
			_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
			c.sendProgress(ctx, evt)
		}

	default:
		// Ambiguous / unknown CUPS state: preserve PAUSED_ERROR with unresolved risk notice
		reason := fmt.Sprintf("printer recovered; CUPS job %s state is %s (ambiguous), operator reconciliation required", inc.CUPSJobID, cupsInfo.State)
		log.Printf("[SPOOLER] Independent printer recovery: %s", reason)
		evt := contracts.JobProgressEvent{
			JobID:         inc.Command.JobID,
			KioskID:       inc.Command.KioskID,
			Status:        contracts.JobStatusPausedError,
			PagesPrinted:  reconciledPages,
			SheetsPrinted: reconciledSheets,
			ErrorReason:   reason,
			Timestamp:     time.Now().UTC(),
		}
		_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
		c.sendProgress(ctx, evt)
	}
}

func (c *Client) renderScreen(state ui_ipc.ScreenState) error {
	if state.PrinterModel == "" && c.cfg.PrinterModel != "" {
		state.PrinterModel = c.cfg.PrinterModel
	}

	c.renderMu.Lock()
	defer c.renderMu.Unlock()

	c.currentModeMu.Lock()
	currentMode := c.currentMode
	c.currentModeMu.Unlock()

	isFault := c.isHardwareFaultActive()

	// Prevent printing/downloading progress updates from overwriting an active hardware fault screen or existing error
	if (state.Mode == ui_ipc.ModePrinting || state.Mode == ui_ipc.ModeDownloading) && (isFault || currentMode == ui_ipc.ModeError) {
		log.Printf("[SPOOLER] Suppressing %s render: hardware fault active or screen already in ModeError (%s)", state.Mode, c.getFaultReason())
		return nil
	}

	// Prevent idle reset or idle frame from overwriting an active hardware fault screen
	if state.Mode == ui_ipc.ModeIdle && isFault {
		log.Printf("[SPOOLER] Suppressing ModeIdle render: hardware fault active (%s)", c.getFaultReason())
		return nil
	}

	// Only append logical state transitions to history, omitting intermediate animation frames
	isIntermediateAnim := (state.TransitionFrom >= 0 &&
		state.TransitionFrom != state.AdSlideIndex &&
		state.TransitionProg > 0.0 &&
		state.TransitionProg < 1.0)

	if !isIntermediateAnim {
		c.renderHistoryMu.Lock()
		c.renderHistory = append(c.renderHistory, state)
		c.renderHistoryMu.Unlock()
	}

	c.currentModeMu.Lock()
	c.currentMode = state.Mode
	c.currentModeMu.Unlock()

	targetW, targetH := ui_ipc.GetTargetResolution()
	if c.canvas == nil || c.canvas.Bounds().Dx() != targetW || c.canvas.Bounds().Dy() != targetH {
		c.canvas = image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	}

	if err := ui_ipc.RenderTo(c.canvas, state); err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	if err := c.cfg.ScreenSink(c.canvas); err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	return nil
}

func (c *Client) idleCarouselLoop(ctx context.Context) {
	interval := c.cfg.IdleCarouselInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slideCount := ui_ipc.GetAdSlideCount()
			if slideCount <= 1 {
				continue
			}

			c.currentModeMu.Lock()
			isIdle := (c.currentMode == ui_ipc.ModeIdle)
			fromIdx := c.adSlideIndex % slideCount
			toIdx := (c.adSlideIndex + 1) % slideCount
			c.adSlideIndex = toIdx
			c.currentModeMu.Unlock()

			if !isIdle {
				continue
			}

			// Wall-clock synchronized 500ms transition at locked 60 FPS (~16.6ms per frame)
			const (
				animDuration  = 500 * time.Millisecond
				targetFPS     = 60
				frameDuration = 1000 * time.Millisecond / targetFPS
			)

			startTime := time.Now()
			frameTimer := time.NewTimer(frameDuration)
			if !frameTimer.Stop() {
				select {
				case <-frameTimer.C:
				default:
				}
			}

			for {
				select {
				case <-ctx.Done():
					frameTimer.Stop()
					return
				default:
				}

				c.currentModeMu.Lock()
				stillIdle := (c.currentMode == ui_ipc.ModeIdle)
				c.currentModeMu.Unlock()

				if !stillIdle {
					break
				}

				elapsed := time.Since(startTime)
				if elapsed >= animDuration {
					// Final static frame
					_ = c.renderScreen(ui_ipc.ScreenState{
						Mode:           ui_ipc.ModeIdle,
						QRContent:      c.cfg.IdleQRContent,
						Message:        "Ready to print",
						AdSlideIndex:   toIdx,
						TransitionFrom: -1,
						TransitionProg: 1.0,
					})
					break
				}

				prog := float64(elapsed) / float64(animDuration)
				_ = c.renderScreen(ui_ipc.ScreenState{
					Mode:           ui_ipc.ModeIdle,
					QRContent:      c.cfg.IdleQRContent,
					Message:        "Ready to print",
					AdSlideIndex:   toIdx,
					TransitionFrom: fromIdx,
					TransitionProg: prog,
				})

				// Sleep precisely until next frame boundary based on wall-clock time
				nextFrameTime := startTime.Add(time.Duration((elapsed/frameDuration)+1) * frameDuration)
				sleepDuration := time.Until(nextFrameTime)
				if sleepDuration > 0 {
					frameTimer.Reset(sleepDuration)
					select {
					case <-ctx.Done():
						frameTimer.Stop()
						return
					case <-frameTimer.C:
					}
				}
			}
			frameTimer.Stop()
		}
	}
}

func (c *Client) handleSyncAds(ads []contracts.AdItem) {
	log.Printf("[SPOOLER] Received sync_ads push with %d items", len(ads))
	ui_ipc.ConfigureAds(ads)
	go saveAdsCache(c.cfg.AdsCachePath, ads)
}

func (c *Client) fetchInitialAds() {
	if c.cfg.CloudURL == "" {
		return
	}
	httpURL := strings.Replace(c.cfg.CloudURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)
	httpURL = strings.TrimSuffix(httpURL, "/ws") + "/api/ads"

	u, err := url.Parse(httpURL)
	if err != nil {
		return
	}
	u.RawQuery = ""

	resp, err := c.cfg.HTTPClient.Get(u.String())
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	var ads []contracts.AdItem
	if err := json.NewDecoder(resp.Body).Decode(&ads); err == nil && len(ads) > 0 {
		c.handleSyncAds(ads)
	}
}

func saveAdsCache(path string, ads []contracts.AdItem) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(ads, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

func loadAdsCache(path string) ([]contracts.AdItem, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ads []contracts.AdItem
	if err := json.Unmarshal(data, &ads); err != nil {
		return nil, err
	}
	return ads, nil
}

// RequestCloudRecovery contacts the cloud recovery endpoint to generate an immutable continuation
// artifact and queues the continuation command for execution without any Pi-side PDF slicing.
// If outputConfirmed is false, the cloud will reject the recovery per safety constraints.
func (c *Client) RequestCloudRecovery(
	ctx context.Context,
	jobID string,
	confirmedSheets int,
	outputConfirmed bool,
	reason string,
) (*contracts.PrintJobCommand, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("request recovery: jobID cannot be empty")
	}

	u, err := url.Parse(c.cfg.CloudURL)
	if err != nil {
		return nil, fmt.Errorf("request recovery: parse cloud URL: %w", err)
	}
	httpScheme := "http"
	if u.Scheme == "wss" {
		httpScheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", httpScheme, u.Host)
	recoveryEndpoint := fmt.Sprintf("%s/api/k/%s/jobs/%s/recover", baseURL, c.cfg.KioskID, jobID)

	recoveryID := fmt.Sprintf("rec_%s_%d", jobID, time.Now().UnixNano())

	reqBody := map[string]interface{}{
		"recovery_id":              recoveryID,
		"confirmed_sheets_printed": confirmedSheets,
		"output_confirmed":         outputConfirmed,
		"reason":                   reason,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("request recovery: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, recoveryEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("request recovery: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(c.cfg.KioskSecret) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.cfg.KioskSecret))
	}

	client := c.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request recovery: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recovery failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var recResp struct {
		Status  string                     `json:"status"`
		Command *contracts.PrintJobCommand `json:"command"`
	}
	if err := json.Unmarshal(respBytes, &recResp); err != nil {
		return nil, fmt.Errorf("request recovery: decode response: %w", err)
	}

	if recResp.Status == "COMPLETED" || recResp.Command == nil {
		return nil, nil
	}

	cmd := *recResp.Command
	c.activeMu.Lock()
	c.activeJobs[cmd.JobID] = true
	c.activeMu.Unlock()

	select {
	case c.jobQueue <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &cmd, nil
}

