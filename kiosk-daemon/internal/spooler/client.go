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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// StateStore defines the persistence interface required by the spooler client.
// In production, *state.Store implements this interface against local SQLite.
type StateStore interface {
	SaveJob(ctx context.Context, cmd contracts.PrintJobCommand, status contracts.JobStatus) error
	UpdateProgress(ctx context.Context, event contracts.JobProgressEvent) error
	RecordSheetProgress(ctx context.Context, event contracts.JobProgressEvent) error
	MarkJobSubmitting(ctx context.Context, jobID string) error
	RecordJobSubmission(ctx context.Context, jobID string, cupsJobID string) error
	GetIncompleteJobs(ctx context.Context) ([]state.StoredJob, error)
	GetJob(ctx context.Context, jobID string) (*state.StoredJob, error)
	GetJobsByOrderID(ctx context.Context, orderID string) ([]state.StoredJob, error)
	IsOrderCompleted(ctx context.Context, orderID string) (bool, error)
	GetEstimatedSheets(ctx context.Context) (int, error)
	GetPaperSupply(ctx context.Context) (int, bool, error)
	ResetPaperTray(ctx context.Context, sheets int) error
}

// ClientConfig holds the runtime dependencies and parameters for the kiosk spooler client.
type ClientConfig struct {
	CloudURL                     string
	KioskID                      string
	KioskSecret                  string
	StateStore                   StateStore
	PrinterAddr                  string
	SNMPTarget                   string
	SNMPCommunity                string
	SNMPPort                     int
	CUPSQueue                    string
	PrinterModel                 string
	DownloadDir                  string
	IdleQRContent                string
	AdsCachePath                 string
	IdleCarouselInterval         time.Duration
	CompletedDisplayDuration     time.Duration
	ErrorDisplayDuration         time.Duration
	HealthPollInterval           time.Duration
	RequiredRecoveryHealthyCount int
	ScreenSink                   ui_ipc.Sink
	HTTPClient                   *http.Client
	MockHealth                   func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	MockCUPSPrint                func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error
	SNMPPoller                   func(ctx context.Context) printer.HardwareTelemetry
	CUPSRunner                   printer.CommandRunner
	EnableCloudRecovery          bool
	InputSource                  input.Source
	InactivityTimeout            time.Duration
	SummaryTimeout               time.Duration
	PaymentTimeout               time.Duration
	PaymentURLBase               string
	BypassPayment                bool
	SoundPlayer                  audio.Player
	OnJobCompleted               func(jobID string)
	TrustedOrigins               []string
	EnableShadowHealth           bool
	ShadowHealthInterval         time.Duration
	MockCUPSHealth               func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	MockCUPSHealthDiag           func(ctx context.Context, estimatedSheets int) (contracts.PrinterHealth, error)
	MockSNMPHealth               func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	MockIPPState                 func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error)
	JobQueueCapacity             int
	PingInterval                 time.Duration
	PingTimeout                  time.Duration
	HeartbeatInterval            time.Duration
	MinBackoff                   time.Duration
	MaxBackoff                   time.Duration
}

// Client connects to the cloud WebSocket hub, handles heartbeats, downloads and verifies PDFs,
// streams print jobs via PJL, renders status screens, and recovers incomplete jobs.
type Client struct {
	cfg           ClientConfig
	normalizedURL string
	safeURL       string

	connMu sync.Mutex
	conn   *websocket.Conn
	sendMu sync.Mutex

	jobQueue   chan contracts.PrintJobCommand
	activeMu   sync.Mutex
	activeJobs map[string]bool

	deferredMu   sync.Mutex
	deferredJobs []contracts.PrintJobCommand

	queueMu         sync.Mutex
	pendingQueue    []contracts.PrintJobCommand
	producerRunning bool
	runCtx          context.Context
	runCancel       context.CancelFunc

	idleMu    sync.Mutex
	idleTimer *time.Timer

	minBackoff        time.Duration
	maxBackoff        time.Duration
	heartbeatInterval time.Duration
	pingInterval      time.Duration
	pingTimeout       time.Duration
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
	consecutiveHealthy    int
	requiredRecoveryCount int

	renderMu sync.Mutex
	canvas   *image.RGBA

	healthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	snmpPoller   func(ctx context.Context) printer.HardwareTelemetry

	pinController *pin.Controller

	cupsJobsMu sync.Mutex
	cupsJobIDs map[string]string

	capsMu       sync.RWMutex
	capabilities printer.PrinterCapabilities

	ippStateMu sync.RWMutex
	ippState   printer.IPPPrinterStateResult

	shadowMu     sync.RWMutex
	latestShadow *ShadowHealthReport

	prodHealthMu     sync.RWMutex
	latestProdHealth contracts.PrinterHealth
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

	// Remove credentials, query strings and fragments from every logged URL.
	safeU := *u
	safeU.User = nil
	safeU.RawQuery = ""
	safeU.Fragment = ""
	safeURL := safeU.String()

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
			wasExplicit := strings.TrimSpace(cfg.SNMPTarget) != ""
			targetHost := printer.ResolveSNMPTarget(context.Background(), cfg.CUPSRunner, cfg.SNMPTarget, cfg.CUPSQueue, cfg.PrinterAddr)
			if !wasExplicit && targetHost != "" && strings.TrimSpace(cfg.CUPSQueue) != "" {
				log.Printf("[SPOOLER] Auto-discovered printer host %s from CUPS queue %s", targetHost, cfg.CUPSQueue)
			}
			cfg.SNMPTarget = targetHost

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
	if cfg.ShadowHealthInterval <= 0 {
		cfg.ShadowHealthInterval = 30 * time.Second
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

	cupsQ := strings.TrimSpace(cfg.CUPSQueue)
	var initialCaps printer.PrinterCapabilities
	var initialIPPState printer.IPPPrinterStateResult
	if cupsQ != "" {
		initialCaps = printer.PrinterCapabilities{
			Source:                 printer.CapabilitySourceCUPSQueue,
			Queue:                  cupsQ,
			PDFSupport:             printer.CapabilityUnknown,
			A4Support:              printer.CapabilityUnknown,
			SimplexSupport:         printer.CapabilityUnknown,
			DuplexLongEdgeSupport:  printer.CapabilityUnknown,
			DuplexShortEdgeSupport: printer.CapabilityUnknown,
			ColorSupport:           printer.CapabilityUnknown,
			MonochromeSupport:      printer.CapabilityUnknown,
		}
		initialIPPState = printer.IPPPrinterStateResult{
			Source:        printer.CapabilitySourceCUPSQueue,
			Queue:         cupsQ,
			State:         printer.IPPPrinterStateUnknown,
			AcceptingJobs: printer.AcceptingJobsUnknown,
		}
	}

	queueCap := cfg.JobQueueCapacity
	if queueCap <= 0 {
		queueCap = 16
	}

	minBackoff := cfg.MinBackoff
	if minBackoff <= 0 {
		minBackoff = 1 * time.Second
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	hbInterval := cfg.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 15 * time.Second
	}
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = 30 * time.Second
	}
	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 10 * time.Second
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	client := &Client{
		cfg:                   cfg,
		normalizedURL:         normalizedURL,
		safeURL:               safeURL,
		jobQueue:              make(chan contracts.PrintJobCommand, queueCap),
		activeJobs:            make(map[string]bool),
		deferredJobs:          make([]contracts.PrintJobCommand, 0),
		pendingQueue:          make([]contracts.PrintJobCommand, 0),
		runCtx:                runCtx,
		runCancel:             runCancel,
		minBackoff:            minBackoff,
		maxBackoff:            maxBackoff,
		heartbeatInterval:     hbInterval,
		pingInterval:          pingInterval,
		pingTimeout:           pingTimeout,
		maxDownloadBytes:      250 * 1024 * 1024,
		healthPoller:          poller,
		snmpPoller:            cfg.SNMPPoller,
		requiredRecoveryCount: reqHealthy,
		cupsJobIDs:            make(map[string]string),
		capabilities:          initialCaps,
		ippState:              initialIPPState,
	}

	if cfg.InputSource != nil {
		pinClient, err := cloud.NewHTTPPINClient(cfg.CloudURL, cfg.KioskID, cfg.KioskSecret, cfg.HTTPClient)
		if err == nil {
			pinCtrl, err := pin.NewController(pin.Config{
				InactivityTimeout: cfg.InactivityTimeout,
				SummaryTimeout:    cfg.SummaryTimeout,
				PaymentTimeout:    cfg.PaymentTimeout,
				PaymentURLBase:    cfg.PaymentURLBase,
				BypassPayment:     cfg.BypassPayment,
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

var (
	bearerAuthRegex  = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)
	urlUserInfoRegex = regexp.MustCompile(`(?i)(wss?://|https?://)[^/\s@]+@`)
	urlQueryRegex    = regexp.MustCompile(`\?[^\s"',;` + "`" + `]+`)
)

// SafeURL returns the cloud WebSocket URL without any query parameters or authentication credentials.
func (c *Client) SafeURL() string {
	return c.safeURL
}

func sanitizeErrorMessage(msg string, secret string, rawURL string, safeURL string) string {
	if msg == "" {
		return ""
	}
	if rawURL != "" && safeURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, safeURL)
	}
	trimmedSecret := strings.TrimSpace(secret)
	if trimmedSecret != "" {
		msg = strings.ReplaceAll(msg, trimmedSecret, "[REDACTED]")
	}
	msg = bearerAuthRegex.ReplaceAllString(msg, "${1}[REDACTED]")
	msg = urlUserInfoRegex.ReplaceAllString(msg, "${1}")
	msg = urlQueryRegex.ReplaceAllString(msg, "")
	return msg
}

func (c *Client) sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrorMessage(err.Error(), c.cfg.KioskSecret, c.normalizedURL, c.safeURL)
}

// RenderHistory returns a copy of the screen states recorded during client execution.
func (c *Client) RenderHistory() []ui_ipc.ScreenState {
	c.renderHistoryMu.Lock()
	defer c.renderHistoryMu.Unlock()
	historyCopy := make([]ui_ipc.ScreenState, len(c.renderHistory))
	copy(historyCopy, c.renderHistory)
	return historyCopy
}

// GetCUPSJobID returns the in-memory or persisted CUPS request ID for the specified print job.
func (c *Client) GetCUPSJobID(jobID string) string {
	c.cupsJobsMu.Lock()
	id := c.cupsJobIDs[jobID]
	c.cupsJobsMu.Unlock()
	if id != "" {
		return id
	}
	if c.cfg.StateStore != nil {
		if job, err := c.cfg.StateStore.GetJob(context.Background(), jobID); err == nil && job != nil {
			return job.CUPSJobID
		}
	}
	return ""
}

// Capabilities returns the latest discovered printer capabilities.
func (c *Client) Capabilities() printer.PrinterCapabilities {
	c.capsMu.RLock()
	defer c.capsMu.RUnlock()
	return c.capabilities
}

// DiscoverStartupCapabilities performs a bounded read-only IPP capability discovery
// against the configured CUPS queue. Discovery errors are logged as warnings and do NOT prevent startup.
// If raw PJL mode is configured (empty CUPS queue), IPP is not queried.
func (c *Client) DiscoverStartupCapabilities(ctx context.Context) printer.PrinterCapabilities {
	cupsQ := strings.TrimSpace(c.cfg.CUPSQueue)
	if cupsQ == "" {
		return printer.PrinterCapabilities{}
	}

	diagCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	caps, err := printer.DiscoverCUPSPrinterCapabilities(diagCtx, c.cfg.CUPSRunner, cupsQ)
	c.capsMu.Lock()
	if err == nil {
		c.capabilities = caps
	}
	c.capsMu.Unlock()

	if err != nil {
		log.Printf("[SPOOLER] Warning: startup printer capability discovery failed for CUPS queue %q: %v (continuing startup)", cupsQ, err)
	} else {
		log.Printf("[SPOOLER] Discovered CUPS queue %q capabilities: PDF=%s, A4=%s, Simplex=%s, DuplexLong=%s, DuplexShort=%s, Color=%s, Mono=%s (source: %s)",
			cupsQ, caps.PDFSupport, caps.A4Support, caps.SimplexSupport, caps.DuplexLongEdgeSupport, caps.DuplexShortEdgeSupport, caps.ColorSupport, caps.MonochromeSupport, caps.Source)
	}
	return c.Capabilities()
}

// IPPPrinterState returns the latest discovered IPP printer state.
func (c *Client) IPPPrinterState() printer.IPPPrinterStateResult {
	c.ippStateMu.RLock()
	defer c.ippStateMu.RUnlock()
	return c.ippState
}

// DiscoverStartupPrinterState performs a bounded read-only IPP printer-state query
// against the configured CUPS queue. Discovery errors are logged as warnings and do NOT
// prevent startup, alter PollCombinedHealth(), or mark any active jobs failed.
// If raw PJL mode is configured (empty CUPS queue), IPP is not queried.
func (c *Client) DiscoverStartupPrinterState(ctx context.Context) printer.IPPPrinterStateResult {
	cupsQ := strings.TrimSpace(c.cfg.CUPSQueue)
	if cupsQ == "" {
		return printer.IPPPrinterStateResult{}
	}

	diagCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	st, err := printer.QueryIPPPrinterState(diagCtx, c.cfg.CUPSRunner, cupsQ)
	c.ippStateMu.Lock()
	if err == nil {
		c.ippState = st
	}
	c.ippStateMu.Unlock()

	if err != nil {
		log.Printf("[SPOOLER] Warning: startup IPP printer-state discovery failed for CUPS queue %q: %v (continuing startup)", cupsQ, err)
	} else {
		log.Printf("[SPOOLER] Discovered CUPS queue %q IPP state: state=%s (raw=%s), accepting=%s, reasons=%v, jam=%v, empty=%v, cover=%v (source: %s)",
			cupsQ, st.State, st.RawState, st.AcceptingJobs, st.RawReasons, st.PaperJam, st.MediaEmpty, st.CoverOpen, st.Source)
	}
	return c.IPPPrinterState()
}

// Run executes the spooler client loops until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	runCtx, runCancel := context.WithCancel(ctx)
	c.queueMu.Lock()
	c.runCtx = runCtx
	c.runCancel = runCancel
	c.queueMu.Unlock()
	defer func() {
		c.queueMu.Lock()
		if c.runCancel != nil {
			c.runCancel()
		}
		c.queueMu.Unlock()
	}()

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

	// 2. Read-only startup diagnostics: discover CUPS printer capabilities and state if configured
	c.DiscoverStartupCapabilities(ctx)
	c.DiscoverStartupPrinterState(ctx)

	// 3. Startup recovery check
	incompleteJobs, err := c.cfg.StateStore.GetIncompleteJobs(ctx)
	if err == nil && len(incompleteJobs) > 0 {
		// Group incomplete jobs by order_id
		orderIncompleteMap := make(map[string][]state.StoredJob)
		var orderIDs []string
		for _, inc := range incompleteJobs {
			oid := inc.Command.OrderID
			if oid == "" {
				oid = inc.Command.JobID
			}
			if _, exists := orderIncompleteMap[oid]; !exists {
				orderIDs = append(orderIDs, oid)
			}
			orderIncompleteMap[oid] = append(orderIncompleteMap[oid], inc)
		}

		for _, oid := range orderIDs {
			incList := orderIncompleteMap[oid]
			allOrderJobs, err := c.cfg.StateStore.GetJobsByOrderID(ctx, oid)
			if err != nil || len(allOrderJobs) == 0 {
				reason := fmt.Sprintf("startup recovery: failed to read order %s jobs (err: %v, count: %d); database inconsistency, manual reconciliation required", oid, err, len(allOrderJobs))
				log.Printf("[SPOOLER] %s", reason)
				for _, incomplete := range incList {
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
				continue
			}

			totalDocs := allOrderJobs[0].Command.TotalDocuments
			if totalDocs < 1 {
				totalDocs = 1
			}

			if len(allOrderJobs) != totalDocs {
				reason := fmt.Sprintf("startup recovery: order %s has inconsistent document count (expected %d, found %d); database inconsistency, manual reconciliation required", oid, totalDocs, len(allOrderJobs))
				log.Printf("[SPOOLER] %s", reason)
				for _, incomplete := range incList {
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
				continue
			}

			docIdxMap := make(map[int]bool)
			validIndices := true
			for _, oj := range allOrderJobs {
				idx := oj.Command.DocumentIndex
				if idx < 0 || idx >= totalDocs || docIdxMap[idx] {
					validIndices = false
					break
				}
				docIdxMap[idx] = true
			}
			if !validIndices || len(docIdxMap) != totalDocs {
				reason := fmt.Sprintf("startup recovery: order %s has invalid or duplicate document indices; database inconsistency, manual reconciliation required", oid)
				log.Printf("[SPOOLER] %s", reason)
				for _, incomplete := range incList {
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
				continue
			}

			// Check if any job in this order had an actual error or was partially submitted/printed
			orderHasPausedOrError := false
			for _, job := range allOrderJobs {
				jobDefinitelyNotSubmitted := !job.SubmittedToPrinter &&
					job.CUPSJobID == "" &&
					job.SheetsPrinted == 0 &&
					job.PagesPrinted == 0 &&
					job.ErrorReason == ""
				if !jobDefinitelyNotSubmitted && (job.Status == contracts.JobStatusPausedError || job.Status == contracts.JobStatusFailedRefundable) {
					orderHasPausedOrError = true
					break
				}
			}

			var jobsToResume []contracts.PrintJobCommand

			for _, incomplete := range incList {
				isDefinitelyNotSubmitted := !incomplete.SubmittedToPrinter &&
					incomplete.CUPSJobID == "" &&
					incomplete.SheetsPrinted == 0 &&
					incomplete.PagesPrinted == 0 &&
					incomplete.ErrorReason == ""

				if isDefinitelyNotSubmitted {
					if !orderHasPausedOrError {
						jobsToResume = append(jobsToResume, incomplete.Command)
					} else {
						reason := fmt.Sprintf("startup recovery: order %s has paused/failed sibling; keeping document %d unprinted", oid, incomplete.Command.DocumentIndex+1)
						log.Printf("[SPOOLER] %s", reason)
						evt := contracts.JobProgressEvent{
							JobID:         incomplete.Command.JobID,
							KioskID:       incomplete.Command.KioskID,
							Status:        contracts.JobStatusPausedError,
							PagesPrinted:  0,
							SheetsPrinted: 0,
							ErrorReason:   reason,
							Timestamp:     time.Now().UTC(),
						}
						_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
						c.sendProgress(ctx, evt)
					}
				} else {
					// Potentially submitted or already partially printed.
					// Never automatically resubmit.
					orderHasPausedOrError = true
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
								// Physical printing finished in CUPS while daemon was stopped/restarting and printer is healthy.
								// Only complete if physical output was physically confirmed before restart.
								if incomplete.SheetsPrinted >= incomplete.Command.TotalSheets {
									log.Printf("[SPOOLER] Startup reconciliation: job %s (CUPS ID %s) confirmed COMPLETED with physical proof (%d/%d sheets) and printer healthy",
										incomplete.Command.JobID, incomplete.CUPSJobID, incomplete.SheetsPrinted, incomplete.Command.TotalSheets)
									c.completeJob(ctx, incomplete.Command)
								} else {
									reason := fmt.Sprintf("interrupted by daemon restart; CUPS completed queue without physical proof (CUPS counter: %d sheets, verified physical: %d/%d), physical evidence insufficient, manual reconciliation required",
										cupsInfo.SheetsCompleted, incomplete.SheetsPrinted, incomplete.Command.TotalSheets)
									log.Printf("[SPOOLER] Startup reconciliation: preserving job %s (CUPS ID %s) in PAUSED_ERROR: %s", incomplete.Command.JobID, incomplete.CUPSJobID, reason)
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
							incCopy := incomplete
							go c.resumeCUPSMonitoring(ctx, &incCopy)
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

			// If all N documents for this order exist and none had errors / were paused, resume unsubmitted in document index order
			if !orderHasPausedOrError && len(jobsToResume) > 0 && len(allOrderJobs) == totalDocs {
				sort.Slice(jobsToResume, func(i, j int) bool {
					return jobsToResume[i].DocumentIndex < jobsToResume[j].DocumentIndex
				})

				// Verify contiguous completed prefix: all documents before jobsToResume[0] must be COMPLETED
				firstResumeIdx := jobsToResume[0].DocumentIndex
				canResume := true
				for i := 0; i < firstResumeIdx; i++ {
					found := false
					for _, oj := range allOrderJobs {
						if oj.Command.DocumentIndex == i {
							found = true
							if oj.Status != contracts.JobStatusCompleted {
								canResume = false
							}
							break
						}
					}
					if !found || !canResume {
						canResume = false
						log.Printf("[SPOOLER] Startup recovery: order %s cannot resume doc %d because earlier doc %d is not completed",
							oid, firstResumeIdx, i)
						break
					}
				}

				// Verify contiguous sequence of resume indices:
				// documents from firstResumeIdx to totalDocs-1 must all be present in jobsToResume
				if canResume {
					expectedSuffixCount := totalDocs - firstResumeIdx
					if len(jobsToResume) != expectedSuffixCount {
						canResume = false
					}
					for k, j := range jobsToResume {
						if j.DocumentIndex != firstResumeIdx+k {
							canResume = false
							break
						}
					}
				}

				if canResume {
					c.activeMu.Lock()
					for _, j := range jobsToResume {
						c.activeJobs[j.JobID] = true
					}
					c.activeMu.Unlock()

					c.enqueueJobs(jobsToResume)
				} else if len(jobsToResume) > 0 {
					reason := fmt.Sprintf("startup recovery: order %s has non-contiguous completion gap; manual reconciliation required", oid)
					log.Printf("[SPOOLER] %s", reason)
					for _, j := range jobsToResume {
						evt := contracts.JobProgressEvent{
							JobID:         j.JobID,
							KioskID:       j.KioskID,
							Status:        contracts.JobStatusPausedError,
							PagesPrinted:  0,
							SheetsPrinted: 0,
							ErrorReason:   reason,
							Timestamp:     time.Now().UTC(),
						}
						_ = c.cfg.StateStore.RecordSheetProgress(ctx, evt)
						c.sendProgress(ctx, evt)
					}
				}
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

	// 3d. Start background printer health shadow reconciliation loop if enabled and CUPS queue is configured
	if c.cfg.EnableShadowHealth && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.shadowHealthLoop(ctx)
		}()
	}

	// 3e. Start background HTTP keep-alive probe to keep cloud backend (Render) warm 24/7
	healthURL := c.resolveHealthURL()
	if healthURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.cloudKeepAliveLoop(ctx, healthURL)
		}()
	}

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
			log.Printf("[SPOOLER] WebSocket connection to cloud failed (%s); retrying in %v...", c.sanitizeError(err), delay)
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

		log.Printf("[SPOOLER] Connected to cloud WebSocket hub at %s", c.safeURL)
		// Connected successfully: reset backoff delay
		delay = c.minBackoff
		c.handleConnection(ctx, conn)
		log.Printf("[SPOOLER] Cloud WebSocket disconnected; reconnecting...")
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
	}()

	var connWg sync.WaitGroup

	// Start heartbeat and telemetry loop
	connWg.Add(1)
	go func() {
		defer connWg.Done()
		c.heartbeatLoop(connCtx, cancel, conn)
	}()

	// Start independent ping watchdog loop
	connWg.Add(1)
	go func() {
		defer connWg.Done()
		c.pingWatchdog(connCtx, cancel, conn)
	}()

	// Synchronize any paused/error job states with the cloud upon connection
	connWg.Add(1)
	go func() {
		defer connWg.Done()
		incList, err := c.cfg.StateStore.GetIncompleteJobs(connCtx)
		if err == nil {
			for _, inc := range incList {
				if inc.Status == contracts.JobStatusPausedError || inc.Status == contracts.JobStatusFailedRefundable {
					evt := contracts.JobProgressEvent{
						JobID:         inc.Command.JobID,
						KioskID:       c.cfg.KioskID,
						Status:        inc.Status,
						PagesPrinted:  inc.PagesPrinted,
						SheetsPrinted: inc.SheetsPrinted,
						ErrorReason:   inc.ErrorReason,
						Timestamp:     time.Now().UTC(),
					}
					c.sendProgress(connCtx, evt)
				}
			}
		}
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
	_ = conn.CloseNow()
	connWg.Wait()
}

func (c *Client) heartbeatLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Send heartbeat
			if err := c.sendWire(ctx, conn, wireMessage{Type: "heartbeat"}); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[SPOOLER] Heartbeat write failed (%s); terminating connection", c.sanitizeError(err))
				cancel()
				_ = conn.CloseNow()
				return
			}

			// Relay printer health
			sheets, isCalibrated, err := c.cfg.StateStore.GetPaperSupply(ctx)
			if err != nil {
				sheets, _ = c.cfg.StateStore.GetEstimatedSheets(ctx)
				isCalibrated = true
			}
			h := c.healthPoller(ctx, sheets)
			h.EstimatedSheets = sheets
			if !isCalibrated {
				h.PaperTrayStatus = "UNKNOWN"
			}
			if err := c.sendWire(ctx, conn, wireMessage{Type: "health", Health: &h}); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[SPOOLER] Health telemetry write failed (%s); terminating connection", c.sanitizeError(err))
				cancel()
				_ = conn.CloseNow()
				return
			}
		}
	}
}

func (c *Client) pingWatchdog(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, c.pingTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[SPOOLER] WebSocket ping watchdog failed (%s); terminating connection", c.sanitizeError(err))
				cancel()
				_ = conn.CloseNow()
				return
			}
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

func (c *Client) sendJobAck(ctx context.Context, event contracts.JobProgressEvent) {
	c.connMu.Lock()
	activeConn := c.conn
	c.connMu.Unlock()
	if activeConn != nil {
		_ = c.sendWire(ctx, activeConn, wireMessage{
			Type:     "job_ack",
			Progress: &event,
		})
	}
}

func isConflictingDuplicate(stored, incoming contracts.PrintJobCommand) bool {
	if incoming.JobID != stored.JobID ||
		incoming.KioskID != stored.KioskID ||
		incoming.TotalPages != stored.TotalPages ||
		incoming.TotalSheets != stored.TotalSheets ||
		incoming.Copies != stored.Copies ||
		incoming.Duplex != stored.Duplex ||
		incoming.StartFromPage != stored.StartFromPage ||
		!strings.EqualFold(incoming.SHA256Checksum, stored.SHA256Checksum) ||
		incoming.DownloadURL != stored.DownloadURL ||
		incoming.DisplayCode != stored.DisplayCode ||
		incoming.DocumentIndex != stored.DocumentIndex ||
		(incoming.TotalDocuments > 0 && stored.TotalDocuments > 0 && incoming.TotalDocuments != stored.TotalDocuments) ||
		(incoming.OrderID != "" && stored.OrderID != "" && incoming.OrderID != stored.OrderID) ||
		(incoming.ColorMode != "" && stored.ColorMode != "" && !strings.EqualFold(incoming.ColorMode, stored.ColorMode)) {
		return true
	}
	return false
}

func (c *Client) handleDuplicateDelivery(existing *state.StoredJob, cmd contracts.PrintJobCommand) {
	// Stored, completed: Never resubmit; report existing completed state via job_ack and progress
	if existing.Status == contracts.JobStatusCompleted {
		log.Printf("[SPOOLER] Acknowledging duplicate command for already completed job %s", cmd.JobID)
		pagesPrinted := existing.PagesPrinted
		if pagesPrinted <= 0 {
			pagesPrinted = existing.Command.TotalPages
		}
		sheetsPrinted := existing.SheetsPrinted
		if sheetsPrinted <= 0 {
			sheetsPrinted = existing.Command.TotalSheets
		}
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       c.cfg.KioskID,
			Status:        contracts.JobStatusCompleted,
			PagesPrinted:  pagesPrinted,
			SheetsPrinted: sheetsPrinted,
			Timestamp:     time.Now().UTC(),
		}
		c.sendJobAck(context.Background(), evt)
		c.sendProgress(context.Background(), evt)
		return
	}

	// Stored, already submitted to printer: Never resubmit; acknowledge durable possession via job_ack and progress
	if existing.SubmittedToPrinter || existing.CUPSJobID != "" {
		log.Printf("[SPOOLER] Acknowledging duplicate command for already submitted job %s (CUPS ID %s, status %s)", cmd.JobID, existing.CUPSJobID, existing.Status)
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       c.cfg.KioskID,
			Status:        existing.Status,
			PagesPrinted:  existing.PagesPrinted,
			SheetsPrinted: existing.SheetsPrinted,
			ErrorReason:   existing.ErrorReason,
			Timestamp:     time.Now().UTC(),
		}
		c.sendJobAck(context.Background(), evt)
		c.sendProgress(context.Background(), evt)
		return
	}

	// Stored, paused or ambiguous: Never automatically resubmit; preserve recovery requirements via job_ack and progress
	if existing.Status == contracts.JobStatusPausedError || existing.Status == contracts.JobStatusFailedRefundable {
		log.Printf("[SPOOLER] Acknowledging duplicate command for paused/failed job %s (status %s)", cmd.JobID, existing.Status)
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       c.cfg.KioskID,
			Status:        existing.Status,
			PagesPrinted:  existing.PagesPrinted,
			SheetsPrinted: existing.SheetsPrinted,
			ErrorReason:   existing.ErrorReason,
			Timestamp:     time.Now().UTC(),
		}
		c.sendJobAck(context.Background(), evt)
		c.sendProgress(context.Background(), evt)
		return
	}

	// Stored, currently active or deferred in memory: acknowledge via job_ack only
	c.deferredMu.Lock()
	isDeferred := false
	for _, dj := range c.deferredJobs {
		if dj.JobID == cmd.JobID {
			isDeferred = true
			break
		}
	}
	c.deferredMu.Unlock()

	c.activeMu.Lock()
	isActive := c.activeJobs[cmd.JobID]
	c.activeMu.Unlock()

	if isDeferred || isActive {
		log.Printf("[SPOOLER] Acknowledging duplicate command for in-memory active/deferred job %s", cmd.JobID)
		status := existing.Status
		if status == "" {
			status = contracts.JobStatusSpooling
		}
		c.sendJobAck(context.Background(), contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       c.cfg.KioskID,
			Status:        status,
			PagesPrinted:  existing.PagesPrinted,
			SheetsPrinted: existing.SheetsPrinted,
			ErrorReason:   existing.ErrorReason,
			Timestamp:     time.Now().UTC(),
		})
		return
	}

	// Stored, unsubmitted, not currently active in memory: acknowledge via job_ack only
	c.sendJobAck(context.Background(), contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       c.cfg.KioskID,
		Status:        existing.Status,
		PagesPrinted:  existing.PagesPrinted,
		SheetsPrinted: existing.SheetsPrinted,
		ErrorReason:   existing.ErrorReason,
		Timestamp:     time.Now().UTC(),
	})

	totalDocs := existing.Command.TotalDocuments
	if totalDocs < 1 {
		totalDocs = 1
	}
	c.checkAndAdmitOrder(existing.Command.OrderID, totalDocs)
}

func (c *Client) checkAndAdmitOrder(orderID string, totalDocs int) {
	if totalDocs < 1 {
		totalDocs = 1
	}

	siblings, err := c.cfg.StateStore.GetJobsByOrderID(context.Background(), orderID)
	if err != nil {
		log.Printf("[SPOOLER] checkAndAdmitOrder query failed for %s: %v; failing closed", orderID, err)
		return
	}

	if len(siblings) < totalDocs {
		log.Printf("[SPOOLER] Order %s: admitted %d of %d documents; waiting for remaining documents before queueing",
			orderID, len(siblings), totalDocs)
		return
	}

	if len(siblings) > totalDocs {
		log.Printf("[SPOOLER] Order %s: sibling count %d exceeds total_documents %d; inconsistent state, failing closed",
			orderID, len(siblings), totalDocs)
		return
	}

	docMap := make(map[int]state.StoredJob)
	for _, sib := range siblings {
		sibOrderID := sib.Command.OrderID
		if sibOrderID == "" {
			sibOrderID = sib.Command.JobID
		}
		if sibOrderID != orderID || sib.Command.TotalDocuments != totalDocs {
			log.Printf("[SPOOLER] Order %s: sibling %s has mismatched order_id %q or total_documents %d; failing closed",
				orderID, sib.Command.JobID, sibOrderID, sib.Command.TotalDocuments)
			return
		}
		if sib.Command.DocumentIndex < 0 || sib.Command.DocumentIndex >= totalDocs {
			log.Printf("[SPOOLER] Order %s: document index %d out of bounds [0, %d); failing closed",
				orderID, sib.Command.DocumentIndex, totalDocs)
			return
		}
		if _, exists := docMap[sib.Command.DocumentIndex]; exists {
			log.Printf("[SPOOLER] Order %s: duplicate document index %d; failing closed",
				orderID, sib.Command.DocumentIndex)
			return
		}
		docMap[sib.Command.DocumentIndex] = sib
	}
	if len(docMap) != totalDocs {
		log.Printf("[SPOOLER] Order %s: documents invalid or incomplete indices; waiting", orderID)
		return
	}

	// 1. Evaluate sibling error/paused states:
	// If ANY sibling has an actual error or was paused after submission, the order cannot be admitted for fresh printing.
	for idx := 0; idx < totalDocs; idx++ {
		sib := docMap[idx]
		sibDefinitelyNotSubmitted := !sib.SubmittedToPrinter &&
			sib.CUPSJobID == "" &&
			sib.SheetsPrinted == 0 &&
			sib.PagesPrinted == 0 &&
			sib.ErrorReason == ""
		if !sibDefinitelyNotSubmitted && (sib.Status == contracts.JobStatusPausedError || sib.Status == contracts.JobStatusFailedRefundable) {
			log.Printf("[SPOOLER] Order %s has paused/failed document %d (%s, status: %s); not admitting for fresh printing",
				orderID, idx, sib.Command.JobID, sib.Status)
			return
		}
	}

	// 2. Determine contiguous completion prefix:
	// Find the first uncompleted document index k.
	firstUncompletedIdx := totalDocs
	for idx := 0; idx < totalDocs; idx++ {
		if docMap[idx].Status != contracts.JobStatusCompleted {
			firstUncompletedIdx = idx
			break
		}
	}

	// If all documents are already completed, order is done; nothing to queue.
	if firstUncompletedIdx == totalDocs {
		log.Printf("[SPOOLER] Order %s: all %d documents already completed; nothing to queue", orderID, totalDocs)
		return
	}

	// Strict document ordering requirement:
	// All documents before firstUncompletedIdx are COMPLETED.
	// No document from firstUncompletedIdx onward may be COMPLETED.
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		if docMap[idx].Status == contracts.JobStatusCompleted {
			log.Printf("[SPOOLER] Order %s: inconsistent document completion state (doc %d completed while doc %d is not); failing closed",
				orderID, idx, firstUncompletedIdx)
			return
		}
	}

	// 3. Evaluate uncompleted documents for prior submission or physical uncertainty:
	// Never requeue already-submitted, CUPS-queued, or physically started jobs for fresh printing.
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		sib := docMap[idx]
		if sib.SubmittedToPrinter || sib.CUPSJobID != "" || sib.SheetsPrinted > 0 {
			log.Printf("[SPOOLER] Order %s doc %d (%s) already submitted or physically started (submitted=%v, cupsID=%s, sheets=%d); not admitting for fresh printing",
				orderID, idx, sib.Command.JobID, sib.SubmittedToPrinter, sib.CUPSJobID, sib.SheetsPrinted)
			return
		}
	}

	// 4. Check in-memory state:
	// Check if any of the uncompleted jobs are already in activeJobs
	c.activeMu.Lock()
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		if c.activeJobs[docMap[idx].Command.JobID] {
			c.activeMu.Unlock()
			return
		}
	}

	// Check if any job of this order is already in deferredJobs
	c.deferredMu.Lock()
	for _, dj := range c.deferredJobs {
		djOrderID := dj.OrderID
		if djOrderID == "" {
			djOrderID = dj.JobID
		}
		if djOrderID == orderID {
			c.deferredMu.Unlock()
			c.activeMu.Unlock()
			return
		}
	}

	isBusy := len(c.activeJobs) > 0
	if isBusy {
		// Defer uncompleted documents in strict index order
		for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
			c.deferredJobs = append(c.deferredJobs, docMap[idx].Command)
		}
		c.deferredMu.Unlock()
		c.activeMu.Unlock()
		log.Printf("[SPOOLER] Order %s uncompleted documents (%d..%d of %d) deferred; kiosk is busy",
			orderID, firstUncompletedIdx+1, totalDocs, totalDocs)
		return
	}
	c.deferredMu.Unlock()

	// Not busy: reserve uncompleted jobs in activeJobs
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		c.activeJobs[docMap[idx].Command.JobID] = true
	}
	c.activeMu.Unlock()

	// Queue uncompleted documents in strict DocumentIndex order via single ordered producer
	var toQueue []contracts.PrintJobCommand
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		dCmd := docMap[idx].Command
		log.Printf("[SPOOLER] Queued print job %s (order %s, doc %d/%d, pages=%d, sheets=%d, copies=%d, duplex=%s)",
			dCmd.JobID, orderID, idx+1, totalDocs, dCmd.TotalPages, dCmd.TotalSheets, dCmd.Copies, dCmd.Duplex)
		toQueue = append(toQueue, dCmd)
	}
	c.enqueueJobs(toQueue)
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
	switch cmd.Duplex {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
	default:
		return
	}

	totalDocs := cmd.TotalDocuments
	if totalDocs < 1 {
		totalDocs = 1
	}
	if cmd.DocumentIndex < 0 || cmd.DocumentIndex >= totalDocs {
		log.Printf("[SPOOLER] Rejecting incoming job %s: invalid document index %d for total %d", cmd.JobID, cmd.DocumentIndex, totalDocs)
		return
	}
	orderID := strings.TrimSpace(cmd.OrderID)
	if orderID == "" {
		orderID = cmd.JobID
	}
	cmd.OrderID = orderID
	cmd.TotalDocuments = totalDocs

	if c.cfg.StateStore == nil {
		log.Printf("[SPOOLER] Rejecting incoming job %s: StateStore not configured", cmd.JobID)
		return
	}

	// Sibling consistency check: query existing jobs for this order
	siblings, err := c.cfg.StateStore.GetJobsByOrderID(context.Background(), orderID)
	if err != nil {
		log.Printf("[SPOOLER] Failed to query local state store for order %s: %v; failing closed", orderID, err)
		return
	}

	for _, sib := range siblings {
		if sib.Command.JobID == cmd.JobID {
			continue
		}
		if sib.Command.TotalDocuments != cmd.TotalDocuments {
			log.Printf("[SPOOLER] Rejecting job %s: conflicting total_documents %d (expected %d from sibling %s)",
				cmd.JobID, cmd.TotalDocuments, sib.Command.TotalDocuments, sib.Command.JobID)
			return
		}
		if sib.Command.KioskID != cmd.KioskID {
			log.Printf("[SPOOLER] Rejecting job %s: conflicting kiosk_id %s (expected %s from sibling %s)",
				cmd.JobID, cmd.KioskID, sib.Command.KioskID, sib.Command.JobID)
			return
		}
		if sib.Command.DisplayCode != cmd.DisplayCode {
			log.Printf("[SPOOLER] Rejecting job %s: conflicting display_code %s (expected %s from sibling %s)",
				cmd.JobID, cmd.DisplayCode, sib.Command.DisplayCode, sib.Command.JobID)
			return
		}
		if sib.Command.DocumentIndex == cmd.DocumentIndex {
			log.Printf("[SPOOLER] Rejecting job %s: conflicting job_id for document index %d (already assigned to %s)",
				cmd.JobID, cmd.DocumentIndex, sib.Command.JobID)
			return
		}
	}

	// 1. Check existing durable state in local store
	existing, err := c.cfg.StateStore.GetJob(context.Background(), cmd.JobID)
	if err != nil {
		log.Printf("[SPOOLER] Failed to query local state store for job %s: %v; failing closed", cmd.JobID, err)
		return
	}

	if existing != nil {
		storedOrderID := existing.Command.OrderID
		if storedOrderID == "" {
			storedOrderID = existing.Command.JobID
		}
		incomingOrderID := orderID
		if incomingOrderID == "" {
			incomingOrderID = cmd.JobID
		}
		if storedOrderID != incomingOrderID {
			log.Printf("[SPOOLER] Rejecting job %s: conflicting order_id %s (stored under %s)", cmd.JobID, incomingOrderID, storedOrderID)
			return
		}
		if isConflictingDuplicate(existing.Command, cmd) {
			log.Printf("[SPOOLER] Rejecting conflicting duplicate command for job %s: payload does not match stored job", cmd.JobID)
			return
		}
		c.handleDuplicateDelivery(existing, cmd)
		return
	}

	// 2. New job acceptance (existing == nil)
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
	c.activeMu.Unlock()

	// 3. Durably persist job in SQLite
	if err := c.cfg.StateStore.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
		log.Printf("[SPOOLER] Failed to persist initial job %s: %v", cmd.JobID, err)
		return
	}

	// 4. Send explicit job_ack back to cloud immediately upon durable persistence
	c.sendJobAck(context.Background(), contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusSpooling,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		Timestamp:     time.Now().UTC(),
	})

	// 5. Try to admit the order if all expected documents are now durably stored
	c.checkAndAdmitOrder(orderID, totalDocs)
}

func (c *Client) getSNMPConfig() *printer.SNMPConfig {
	targetHost := printer.ResolveSNMPTarget(context.Background(), c.cfg.CUPSRunner, c.cfg.SNMPTarget, c.cfg.CUPSQueue, c.cfg.PrinterAddr)
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

		if job.CUPSJobID != "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
			holdCtx, holdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, job.CUPSJobID)
			holdCancel()

			reason = formatCUPSHoldOutcomeReason(reason, job.CUPSJobID, holdRes)
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

	var err error
	if c.snmpPoller != nil {
		err = printer.MonitorCUPSJobWithTelemetryCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cmd, job.CUPSJobID, onPage, 500*time.Millisecond, c.snmpPoller)
	} else {
		snmpCfg := c.getSNMPConfig()
		err = printer.MonitorCUPSJobWithSNMPCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cmd, job.CUPSJobID, onPage, 500*time.Millisecond, snmpCfg)
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

	if curSheets >= cmd.TotalSheets {
		c.completeJob(ctx, cmd)
	} else {
		reason := fmt.Sprintf("CUPS completed queue, but physical output incomplete (verified physical: %d/%d); physical evidence insufficient, manual reconciliation required", curSheets, cmd.TotalSheets)
		if curSheets == 0 {
			reason = fmt.Sprintf("CUPS completed queue without physical sheet verification (verified physical: 0/%d); physical evidence insufficient, manual reconciliation required", cmd.TotalSheets)
		}
		log.Printf("[SPOOLER] Resumed monitoring for job %s: %s", cmd.JobID, reason)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}
}

// enqueueJobs queues jobs in strict sequence using a single ordered producer mechanism.
// If jobQueue has available capacity, jobs are queued immediately and synchronously.
// Under queue saturation, remaining jobs are buffered in pendingQueue and fed to
// jobQueue strictly in FIFO document-index order by a single producer goroutine.
func (c *Client) enqueueJobs(jobs []contracts.PrintJobCommand) {
	if len(jobs) == 0 {
		return
	}
	c.queueMu.Lock()
	defer c.queueMu.Unlock()

	// If producer is already running, earlier jobs are pending;
	// append to pendingQueue to guarantee FIFO document order.
	if c.producerRunning {
		c.pendingQueue = append(c.pendingQueue, jobs...)
		return
	}

	// No producer currently active: try to push as many as possible directly into c.jobQueue
	c.pendingQueue = append(c.pendingQueue, jobs...)
	for len(c.pendingQueue) > 0 {
		select {
		case c.jobQueue <- c.pendingQueue[0]:
			c.pendingQueue = c.pendingQueue[1:]
		default:
			// c.jobQueue is saturated. Launch the single ordered producer.
			c.producerRunning = true
			go c.runProducer()
			return
		}
	}
}

func (c *Client) runProducer() {
	for {
		c.queueMu.Lock()
		if len(c.pendingQueue) == 0 {
			c.producerRunning = false
			c.queueMu.Unlock()
			return
		}
		nextJob := c.pendingQueue[0]
		ctx := c.runCtx
		if ctx == nil {
			ctx = context.Background()
		}
		c.queueMu.Unlock()

		select {
		case <-ctx.Done():
			c.queueMu.Lock()
			c.producerRunning = false
			c.queueMu.Unlock()
			return
		case c.jobQueue <- nextJob:
			c.queueMu.Lock()
			if len(c.pendingQueue) > 0 && c.pendingQueue[0].JobID == nextJob.JobID {
				c.pendingQueue = c.pendingQueue[1:]
			}
			// Drain opportunistically while c.jobQueue has space
			for len(c.pendingQueue) > 0 {
				select {
				case c.jobQueue <- c.pendingQueue[0]:
					c.pendingQueue = c.pendingQueue[1:]
				default:
					goto nextIter
				}
			}
		nextIter:
			c.queueMu.Unlock()
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
			c.drainDeferredJobsIfSafe(ctx)
		}
	}
}

// extractOrderIDFromDownloadURL parses rawURL and extracts {order_id} if the path matches /api/orders/{order_id}/pdf
// or /api/orders/{order_id}/jobs/{job_id}/pdf.
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
	// Expected: ["api", "orders", "{order_id}", "jobs", "{job_id}", "pdf"]
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "orders" && parts[3] == "jobs" && parts[5] == "pdf" {
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
	if cmd.OrderID != "" && cmd.OrderID == activeOrderID {
		return true
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

	// Identify the first deferred order
	firstJob := c.deferredJobs[0]
	targetOrderID := firstJob.OrderID
	if targetOrderID == "" {
		targetOrderID = firstJob.JobID
	}

	// Collect all jobs belonging to targetOrderID from deferredJobs, and keep the rest
	var orderJobs []contracts.PrintJobCommand
	var remainingDeferred []contracts.PrintJobCommand

	for _, dj := range c.deferredJobs {
		djOrderID := dj.OrderID
		if djOrderID == "" {
			djOrderID = dj.JobID
		}
		if djOrderID == targetOrderID {
			orderJobs = append(orderJobs, dj)
		} else {
			remainingDeferred = append(remainingDeferred, dj)
		}
	}
	c.deferredJobs = remainingDeferred

	sort.Slice(orderJobs, func(i, j int) bool {
		return orderJobs[i].DocumentIndex < orderJobs[j].DocumentIndex
	})

	c.activeMu.Lock()
	for _, j := range orderJobs {
		c.activeJobs[j.JobID] = true
	}
	c.activeMu.Unlock()

	log.Printf("[SPOOLER] Resuming %d deferred jobs for order %s now that kiosk is idle", len(orderJobs), targetOrderID)
	c.enqueueJobs(orderJobs)
}

func formatCUPSHoldOutcomeReason(baseReason string, cupsJobID string, holdRes printer.HoldResult) string {
	if holdRes.HoldErr != nil {
		if holdRes.QueryErr != nil {
			return fmt.Sprintf("%s; hold request failed for CUPS job %s (%v, query error: %v): possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID, holdRes.HoldErr, holdRes.QueryErr)
		}
		return fmt.Sprintf("%s; hold request failed for CUPS job %s (%v), CUPS state is %s: possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID, holdRes.HoldErr, holdRes.State)
	}
	if holdRes.StillProcessing {
		return fmt.Sprintf("%s; CUPS job %s remains processing after hold request: possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID)
	}
	if holdRes.AlreadyCompleted {
		return fmt.Sprintf("%s; CUPS job %s was already completed: possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID)
	}
	if holdRes.ConfirmedHeld {
		return fmt.Sprintf("%s; CUPS job %s held (buffered sheets may continue printing), manual reconciliation required", baseReason, cupsJobID)
	}
	if holdRes.IsUnknown || holdRes.QueryErr != nil {
		if holdRes.QueryErr != nil {
			return fmt.Sprintf("%s; status is unknown for CUPS job %s (query error: %v, state: %s): possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID, holdRes.QueryErr, holdRes.State)
		}
		return fmt.Sprintf("%s; status is unknown for CUPS job %s (state: %s): possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID, holdRes.State)
	}
	return fmt.Sprintf("%s; CUPS job %s state is %s: possible unmonitored physical output risk, manual reconciliation required", baseReason, cupsJobID, holdRes.State)
}

func jobPDFPath(downloadDir, jobID string) string {
	h := sha256.Sum256([]byte(jobID))
	return filepath.Join(downloadDir, hex.EncodeToString(h[:])+".pdf")
}

func (c *Client) processJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	if cmd.TotalDocuments < 1 {
		cmd.TotalDocuments = 1
	}
	if strings.TrimSpace(cmd.OrderID) == "" {
		cmd.OrderID = cmd.JobID
	}

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

	orderID := cmd.OrderID
	if orderID == "" {
		orderID = cmd.JobID
	}

	// 0. Printer Capability Validation
	caps := c.Capabilities()
	colorMode := strings.ToUpper(strings.TrimSpace(cmd.ColorMode))
	if colorMode == "COLOR" && caps.ColorSupport == printer.CapabilityUnsupported {
		reason := "printer does not support requested COLOR printing mode"
		log.Printf("[SPOOLER] Job %s rejected: %s", cmd.JobID, reason)
		c.handleJobError(ctx, cmd, 0, 0, reason)
		return
	}
	if (colorMode == "BW" || colorMode == "MONOCHROME") && caps.MonochromeSupport == printer.CapabilityUnsupported {
		reason := "printer does not support requested BW/MONOCHROME printing mode"
		log.Printf("[SPOOLER] Job %s rejected: %s", cmd.JobID, reason)
		c.handleJobError(ctx, cmd, 0, 0, reason)
		return
	}

	// Early durable state & sibling checks
	if c.cfg.StateStore != nil {
		stored, err := c.cfg.StateStore.GetJob(ctx, cmd.JobID)
		if err != nil {
			log.Printf("[SPOOLER] Early check failed: SQLite read error for job %s: %v; failing closed", cmd.JobID, err)
			c.handleJobError(ctx, cmd, 0, 0, fmt.Sprintf("state store read error: %v", err))
			return
		}
		if stored != nil {
			if stored.Status == contracts.JobStatusCompleted {
				log.Printf("[SPOOLER] Job %s is already completed; skipping redundant processing", cmd.JobID)
				return
			}
			storedDefinitelyNotSubmitted := !stored.SubmittedToPrinter &&
				stored.CUPSJobID == "" &&
				stored.SheetsPrinted == 0 &&
				stored.PagesPrinted == 0 &&
				stored.ErrorReason == ""
			if !storedDefinitelyNotSubmitted && (stored.Status == contracts.JobStatusPausedError || stored.Status == contracts.JobStatusFailedRefundable) {
				log.Printf("[SPOOLER] Job %s is in %s state; skipping redundant processing", cmd.JobID, stored.Status)
				return
			}
			if stored.CUPSJobID != "" {
				if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
					log.Printf("[SPOOLER] Job %s already has CUPS job %s; resuming monitoring instead of re-submitting",
						cmd.JobID, stored.CUPSJobID)
					c.resumeCUPSMonitoring(ctx, stored)
					return
				}
				reason := fmt.Sprintf("job %s already has CUPS job %s without CUPS queue configured; manual reconciliation required", cmd.JobID, stored.CUPSJobID)
				c.handleJobError(ctx, cmd, stored.PagesPrinted, stored.SheetsPrinted, reason)
				return
			}
			if stored.SubmittedToPrinter {
				reason := fmt.Sprintf("job %s was already marked submitted to printer without recorded completion; physical status uncertain, manual reconciliation required", cmd.JobID)
				log.Printf("[SPOOLER] %s", reason)
				c.handleJobError(ctx, cmd, stored.PagesPrinted, stored.SheetsPrinted, reason)
				return
			}
		}

		siblings, err := c.cfg.StateStore.GetJobsByOrderID(ctx, orderID)
		if err != nil {
			log.Printf("[SPOOLER] Early check failed: SQLite read error for siblings of order %s: %v; failing closed", orderID, err)
			c.handleJobError(ctx, cmd, 0, 0, fmt.Sprintf("state store read error: %v", err))
			return
		}
		for _, sib := range siblings {
			if sib.Command.JobID == cmd.JobID {
				continue
			}
			sibDefinitelyNotSubmitted := !sib.SubmittedToPrinter &&
				sib.CUPSJobID == "" &&
				sib.SheetsPrinted == 0 &&
				sib.PagesPrinted == 0 &&
				sib.ErrorReason == ""
			if !sibDefinitelyNotSubmitted && (sib.Status == contracts.JobStatusPausedError || sib.Status == contracts.JobStatusFailedRefundable) {
				reason := fmt.Sprintf("order paused due to error in sibling document %d (%s): %s",
					sib.Command.DocumentIndex+1, sib.Command.JobID, sib.ErrorReason)
				log.Printf("[SPOOLER] Sibling job %s in order %s is %s; pausing job %s without submitting",
					sib.Command.JobID, orderID, sib.Status, cmd.JobID)
				c.handleJobError(ctx, cmd, 0, 0, reason)
				return
			}
			if sib.Command.DocumentIndex < cmd.DocumentIndex && sib.Status != contracts.JobStatusCompleted {
				reason := fmt.Sprintf("earlier document %d (%s) in order %s is not completed (status: %s)",
					sib.Command.DocumentIndex+1, sib.Command.JobID, orderID, sib.Status)
				log.Printf("[SPOOLER] Strict ordering violated: %s; pausing job %s", reason, cmd.JobID)
				c.handleJobError(ctx, cmd, 0, 0, reason)
				return
			}
			if sib.Command.DocumentIndex > cmd.DocumentIndex && sib.Status == contracts.JobStatusCompleted {
				reason := fmt.Sprintf("subsequent document %d (%s) in order %s is already completed while doc %d is not",
					sib.Command.DocumentIndex+1, sib.Command.JobID, orderID, cmd.DocumentIndex+1)
				log.Printf("[SPOOLER] Strict ordering violated: %s; pausing job %s", reason, cmd.JobID)
				c.handleJobError(ctx, cmd, 0, 0, reason)
				return
			}
		}
	}

	docDetail := "Job ID: " + cmd.JobID
	if cmd.TotalDocuments > 1 {
		docDetail = fmt.Sprintf("Doc %d of %d • Job: %s", cmd.DocumentIndex+1, cmd.TotalDocuments, cmd.JobID)
	}

	log.Printf("[SPOOLER] Processing job %s (doc %d/%d, order %s): downloading from %s",
		cmd.JobID, cmd.DocumentIndex+1, cmd.TotalDocuments, orderID, cmd.DownloadURL)
	// 1. Initial screen transition to DOWNLOADING
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeDownloading,
		ProgressPercent: 0,
		Message:         "Downloading document...",
		Detail:          docDetail,
	})

	// 2. Persist initial job record if not already stored
	savedJob, _ := c.cfg.StateStore.GetJob(ctx, cmd.JobID)
	if savedJob == nil {
		_ = c.cfg.StateStore.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	}

	// 3. Download & verify PDF
	finalPath := jobPDFPath(c.cfg.DownloadDir, cmd.JobID)

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
		Detail:          docDetail,
	})

	// 4. Update progress: SPOOLING
	pagesPrinted := 0
	sheetsPrinted := 0
	if savedJob != nil {
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
		if sheetsPrinted >= cmd.TotalSheets {
			c.completeJob(ctx, cmd)
		} else {
			reason := fmt.Sprintf("pages already printed, but physical sheets incomplete (verified physical: %d/%d); manual reconciliation required", sheetsPrinted, cmd.TotalSheets)
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, reason)
		}
		return
	}

	remainingSheets := cmd.TotalSheets - sheetsPrinted
	if remainingSheets < 0 {
		remainingSheets = 0
	}

	orderRemainingSheets := remainingSheets
	if c.cfg.StateStore != nil {
		if sibs, err := c.cfg.StateStore.GetJobsByOrderID(ctx, orderID); err == nil && len(sibs) > 0 {
			totalOrderRemaining := 0
			for _, s := range sibs {
				if s.Status == contracts.JobStatusCompleted {
					continue
				}
				rem := s.Command.TotalSheets - s.SheetsPrinted
				if rem > 0 {
					totalOrderRemaining += rem
				}
			}
			if totalOrderRemaining > orderRemainingSheets {
				orderRemainingSheets = totalOrderRemaining
			}
		}
	}

	estimatedSheets, err := c.cfg.StateStore.GetEstimatedSheets(ctx)
	if err != nil || estimatedSheets < orderRemainingSheets {
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
		Detail:          docDetail,
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
			cID = c.GetCUPSJobID(cmd.JobID)
		}
		if cID != "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
			holdCtx, holdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cID)
			holdCancel()

			reason = formatCUPSHoldOutcomeReason(reason, cID, holdRes)
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
					Detail:          docDetail,
				})
			}
		}
	}

	// Final durable-state safety check immediately before physical submission to CUPS/PJL
	if c.cfg.StateStore != nil {
		freshJob, sErr := c.cfg.StateStore.GetJob(jobCtx, cmd.JobID)
		if sErr != nil {
			cancelJob()
			<-monitorDone
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("pre-submission check failed: SQLite read error: %v", sErr))
			return
		}
		if freshJob == nil {
			cancelJob()
			<-monitorDone
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, "pre-submission check failed: job not found in state store")
			return
		}

		if freshJob.Status == contracts.JobStatusCompleted {
			log.Printf("[SPOOLER] Pre-submission safety check: job %s is already COMPLETED; rejecting duplicate submission", cmd.JobID)
			cancelJob()
			<-monitorDone
			return
		}

		freshJobDefinitelyNotSubmitted := !freshJob.SubmittedToPrinter &&
			freshJob.CUPSJobID == "" &&
			freshJob.SheetsPrinted == 0 &&
			freshJob.PagesPrinted == 0 &&
			freshJob.ErrorReason == ""
		if !freshJobDefinitelyNotSubmitted && (freshJob.Status == contracts.JobStatusPausedError || freshJob.Status == contracts.JobStatusFailedRefundable) {
			log.Printf("[SPOOLER] Pre-submission safety check: job %s is %s; rejecting submission", cmd.JobID, freshJob.Status)
			cancelJob()
			<-monitorDone
			return
		}

		// Existing CUPS job: resume monitoring or require reconciliation. Never issue a second lp submission.
		if freshJob.CUPSJobID != "" {
			cancelJob()
			<-monitorDone
			if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
				log.Printf("[SPOOLER] Pre-submission safety check: job %s has existing CUPS job %s; resuming monitoring", cmd.JobID, freshJob.CUPSJobID)
				c.resumeCUPSMonitoring(ctx, freshJob)
				return
			}
			reason := fmt.Sprintf("job %s already has CUPS job %s without CUPS queue configured; manual reconciliation required", cmd.JobID, freshJob.CUPSJobID)
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, reason)
			return
		}

		if freshJob.SubmittedToPrinter {
			cancelJob()
			<-monitorDone
			reason := fmt.Sprintf("job %s was already marked submitted to printer without recorded completion; physical output uncertain, manual reconciliation required", cmd.JobID)
			log.Printf("[SPOOLER] Pre-submission safety check: %s", reason)
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, reason)
			return
		}

		// Sibling evaluation immediately before submission
		sibs, sibErr := c.cfg.StateStore.GetJobsByOrderID(jobCtx, orderID)
		if sibErr != nil {
			cancelJob()
			<-monitorDone
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("pre-submission check failed: SQLite read error for order siblings: %v", sibErr))
			return
		}
		if len(sibs) != cmd.TotalDocuments {
			cancelJob()
			<-monitorDone
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("pre-submission check failed: expected %d documents for order %s, found %d", cmd.TotalDocuments, orderID, len(sibs)))
			return
		}
		for _, sib := range sibs {
			if sib.Command.JobID == cmd.JobID {
				continue
			}
			sibDefinitelyNotSubmitted := !sib.SubmittedToPrinter &&
				sib.CUPSJobID == "" &&
				sib.SheetsPrinted == 0 &&
				sib.PagesPrinted == 0 &&
				sib.ErrorReason == ""
			if !sibDefinitelyNotSubmitted && (sib.Status == contracts.JobStatusPausedError || sib.Status == contracts.JobStatusFailedRefundable) {
				cancelJob()
				<-monitorDone
				c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("order paused due to error in sibling document %d (%s): %s", sib.Command.DocumentIndex+1, sib.Command.JobID, sib.ErrorReason))
				return
			}
			if sib.Command.DocumentIndex < cmd.DocumentIndex && sib.Status != contracts.JobStatusCompleted {
				cancelJob()
				<-monitorDone
				c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("pre-submission order violated: earlier document %d (%s) is not completed (status: %s)", sib.Command.DocumentIndex+1, sib.Command.JobID, sib.Status))
				return
			}
			if sib.Command.DocumentIndex > cmd.DocumentIndex && sib.Status == contracts.JobStatusCompleted {
				cancelJob()
				<-monitorDone
				c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("pre-submission order violated: subsequent document %d (%s) is already completed while current document %d is not", sib.Command.DocumentIndex+1, sib.Command.JobID, cmd.DocumentIndex+1))
				return
			}
		}
	}

	var printErr error
	if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		log.Printf("[SPOOLER] Sending job %s to CUPS queue %s", cmd.JobID, c.cfg.CUPSQueue)
		if c.cfg.MockCUPSPrint != nil {
			if err := c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID); err != nil {
				printErr = fmt.Errorf("pre-submission persistence failed for mock CUPS print: %w", err)
			} else {
				printErr = c.cfg.MockCUPSPrint(jobCtx, c.cfg.CUPSQueue, streamCmd, printPath, onPage)
			}
		} else {
			hooks := &printer.CUPSPrintHooks{
				OnSubmitting: func() error {
					return c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID)
				},
				OnSubmitted: func(cupsJobID string) error {
					cupsJobIDMu.Lock()
					activeCUPSJobID = cupsJobID
					cupsJobIDMu.Unlock()

					c.cupsJobsMu.Lock()
					if c.cupsJobIDs == nil {
						c.cupsJobIDs = make(map[string]string)
					}
					c.cupsJobIDs[cmd.JobID] = cupsJobID
					c.cupsJobsMu.Unlock()

					return c.cfg.StateStore.RecordJobSubmission(jobCtx, cmd.JobID, cupsJobID)
				},
				SNMPConfig:          c.getSNMPConfig(),
				TelemetryPoller:     c.snmpPoller,
				PreserveJobOnCancel: preserveCUPSJob.Load,
			}
			printErr = printer.PrintPDFWithCUPSHooksCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, streamCmd, printPath, onPage, 500*time.Millisecond, hooks)
		}
	} else {
		log.Printf("[SPOOLER] Streaming job %s to PJL printer %s", cmd.JobID, c.cfg.PrinterAddr)
		if err := c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID); err != nil {
			printErr = fmt.Errorf("pre-submission persistence failed for PJL print: %w", err)
		} else {
			printErr = printer.StreamPDFWithPJL(jobCtx, c.cfg.PrinterAddr, streamCmd, f, onPage)
		}
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
		// Also check live printer health to see if a hardware fault caused the stream failure
		h := c.CheckPrinterHealth(ctx)
		if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
			handleJobHardwareInterruption(reason)
			return
		}

		cupsJobIDMu.Lock()
		cID := activeCUPSJobID
		cupsJobIDMu.Unlock()
		if cID == "" {
			cID = c.GetCUPSJobID(cmd.JobID)
		}
		errReason := fmt.Sprintf("printing failed: %v", printErr)
		if cID != "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
			holdCtx, holdTimeout := context.WithTimeout(context.Background(), 2*time.Second)
			holdRes := printer.HoldAndVerifyCUPSJob(holdCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cID)
			holdTimeout()
			errReason = formatCUPSHoldOutcomeReason(errReason, cID, holdRes)
		}

		c.handleJobError(ctx, cmd, curPages, curSheets, errReason)
		return
	}

	compHealth := c.CheckPrinterHealth(ctx)
	if isFault, reason, _ := isBlockingHardwareFault(compHealth); isFault {
		handleJobHardwareInterruption(fmt.Sprintf("printer reported hardware error on completion (%s); physical output uncertain, manual reconciliation required", reason))
		return
	}

	// 9. Complete Job
	if curSheets >= cmd.TotalSheets {
		c.completeJob(ctx, cmd)
	} else {
		reason := fmt.Sprintf("CUPS completed queue, but physical output incomplete (verified physical: %d/%d); physical evidence insufficient, manual reconciliation required", curSheets, cmd.TotalSheets)
		if curSheets == 0 {
			reason = fmt.Sprintf("CUPS completed queue without physical sheet verification (verified physical: 0/%d); physical evidence insufficient, manual reconciliation required", cmd.TotalSheets)
		}
		log.Printf("[SPOOLER] Job %s: %s", cmd.JobID, reason)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}
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

	// Post-print cleanup: remove downloaded PDF payload from local disk
	// to enforce ETSI EN 303 645 (Provision 5.4), NIST SP 800-213, and GDPR/DPDP storage limitation.
	finalPath := jobPDFPath(c.cfg.DownloadDir, cmd.JobID)
	if err := os.Remove(finalPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to remove local printed payload %s: %v", finalPath, err)
	}

	c.sendProgress(ctx, evt)

	if c.cfg.OnJobCompleted != nil {
		c.cfg.OnJobCompleted(cmd.JobID)
	}

	orderID := cmd.OrderID
	if orderID == "" {
		orderID = cmd.JobID
	}

	orderCompleted := false
	if c.cfg.StateStore != nil {
		var err error
		orderCompleted, err = c.cfg.StateStore.IsOrderCompleted(ctx, orderID)
		if err != nil {
			log.Printf("[SPOOLER] Error checking order completion for %s: %v", orderID, err)
			orderCompleted = false
		}
	} else {
		orderCompleted = true
	}

	if orderCompleted {
		log.Printf("[SPOOLER] Order %s fully completed (all %d documents completed)", orderID, cmd.TotalDocuments)
		if c.cfg.SoundPlayer != nil {
			c.cfg.SoundPlayer.PlayCompletion()
		}

		detail := "Job ID: " + cmd.JobID
		if cmd.TotalDocuments > 1 {
			detail = fmt.Sprintf("Order: %s • All %d documents printed", orderID, cmd.TotalDocuments)
		}

		_ = c.renderScreen(ui_ipc.ScreenState{
			Mode:            ui_ipc.ModeCompleted,
			CurrentPage:     cmd.TotalPages,
			TotalPages:      cmd.TotalPages,
			ProgressPercent: 100,
			Message:         "Print complete",
			Detail:          detail,
		})

		c.scheduleResetToIdle(c.cfg.CompletedDisplayDuration)
	} else {
		log.Printf("[SPOOLER] Completed document %d of %d for order %s; awaiting remaining documents",
			cmd.DocumentIndex+1, cmd.TotalDocuments, orderID)
		_ = c.renderScreen(ui_ipc.ScreenState{
			Mode:            ui_ipc.ModePrinting,
			CurrentPage:     cmd.TotalPages,
			TotalPages:      cmd.TotalPages,
			ProgressPercent: 100,
			Message:         fmt.Sprintf("Document %d of %d complete", cmd.DocumentIndex+1, cmd.TotalDocuments),
			Detail:          fmt.Sprintf("Order: %s • Job: %s", orderID, cmd.JobID),
		})
	}
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
	var detail string
	if cmd.TotalDocuments > 1 {
		orderRef := cmd.OrderID
		if orderRef == "" {
			orderRef = cmd.DisplayCode
		}
		if orderRef == "" {
			orderRef = jobRef
		}
		detail = fmt.Sprintf("Order Ref: %s • Doc %d of %d • Completed: %d of %d pages",
			orderRef, cmd.DocumentIndex+1, cmd.TotalDocuments, pagesPrinted, cmd.TotalPages)
	} else {
		detail = fmt.Sprintf("Order Ref: %s • Completed: %d of %d pages", jobRef, pagesPrinted, cmd.TotalPages)
	}
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

	c.prodHealthMu.Lock()
	c.latestProdHealth = h
	c.prodHealthMu.Unlock()

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
	incList, err := c.cfg.StateStore.GetIncompleteJobs(ctx)
	if err != nil || len(incList) == 0 {
		return
	}

	for _, inc := range incList {
		if inc.CUPSJobID == "" || inc.Status != contracts.JobStatusPausedError {
			continue
		}

		cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, inc.CUPSJobID)
		if qErr != nil {
			log.Printf("[SPOOLER] Recovery reconciliation: error querying CUPS job %s: %v", inc.CUPSJobID, qErr)
			continue
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

	c.enqueueJobs([]contracts.PrintJobCommand{cmd})
	return &cmd, nil
}

func (c *Client) resolveHealthURL() string {
	rawURL := strings.TrimSpace(c.cfg.CloudURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	scheme := "http"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "https"
	}
	host := u.Host
	if host == "" {
		host = u.Path
	}
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s/healthz", scheme, host)
}

func (c *Client) cloudKeepAliveLoop(ctx context.Context, healthURL string) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	httpClient := c.cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	probe := func() {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
		if err != nil {
			return
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("[KEEPALIVE] Cloud keep-alive probe to %s failed: %v", healthURL, err)
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			// Cloud backend is warm and responsive
		} else {
			log.Printf("[KEEPALIVE] Cloud keep-alive probe to %s returned status: %d", healthURL, resp.StatusCode)
		}
	}

	// Immediate probe on daemon startup to wake backend if cold
	probe()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe()
		}
	}
}
