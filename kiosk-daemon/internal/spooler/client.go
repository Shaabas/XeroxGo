package spooler

import (
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
	Type           string                      `json:"type"`
	Job            *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress       *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health         *contracts.PrinterHealth    `json:"health,omitempty"`
	Sheets         *int                        `json:"sheets,omitempty"`
	RequestID      string                      `json:"request_id,omitempty"`
	OK             bool                        `json:"ok,omitempty"`
	Calibrated     bool                        `json:"calibrated,omitempty"`
	InventorySeq   uint64                      `json:"inventory_seq,omitempty"`
	OutcomeVersion int64                       `json:"outcome_version,omitempty"`
	Error          string                      `json:"error,omitempty"`
	Ads            []contracts.AdItem          `json:"ads,omitempty"`
}

// StateStore defines the persistence interface required by the spooler client.
// In production, *state.Store implements this interface against local SQLite.
type StateStore interface {
	SaveJob(ctx context.Context, cmd contracts.PrintJobCommand, status contracts.JobStatus) error
	BeginJobSpooling(ctx context.Context, jobID string) error
	UpdateProgress(ctx context.Context, event contracts.JobProgressEvent) error
	RecordSheetProgress(ctx context.Context, event contracts.JobProgressEvent) error
	FinalizeOrderFailure(ctx context.Context, orderID string, jobID string, event contracts.JobProgressEvent, invalidateInventory bool) ([]state.StoredJob, error)
	GetPendingOutcomeSyncJobs(ctx context.Context) ([]state.StoredJob, error)
	MarkOutcomeSynced(ctx context.Context, jobID string, version int64) error
	MarkJobSubmitting(ctx context.Context, jobID string) error
	MarkJobSubmittingWithBaseline(ctx context.Context, jobID string, baselineSet bool, baselineValue int64, baselineIndex string, baselineUnit int64, baselineObservedAt time.Time) error
	RecordJobSubmission(ctx context.Context, jobID string, cupsJobID string) error
	GetIncompleteJobs(ctx context.Context) ([]state.StoredJob, error)
	GetJob(ctx context.Context, jobID string) (*state.StoredJob, error)
	GetJobsByOrderID(ctx context.Context, orderID string) ([]state.StoredJob, error)
	IsOrderCompleted(ctx context.Context, orderID string) (bool, error)
	GetEstimatedSheets(ctx context.Context) (int, error)
	GetPaperSupply(ctx context.Context) (int, bool, error)
	ResetPaperTray(ctx context.Context, sheets int) error
	CalibratePaperTray(ctx context.Context, sheets int) error
	TrayCapacity() int
}

// ClientConfig holds the runtime dependencies and parameters for the kiosk spooler client.
type ClientConfig struct {
	CloudURL                     string
	KioskID                      string
	KioskSecret                  string
	StateStore                   StateStore
	ConfiguredTrayCapacity       int
	CapabilityPolicy             *printer.PrinterCapabilityPolicy
	MockCapabilities             *printer.PrinterCapabilities
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
	InputSource                  input.Source
	InactivityTimeout            time.Duration
	PaymentTimeout               time.Duration
	PaymentURLBase               string
	SoundPlayer                  audio.Player
	OnJobCompleted               func(jobID string)
	TrustedOrigins               []string
	NativeIPPURI                 string
	SNMPMonitorFactory           printer.DynamicSessionFactory
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

	connMu    sync.Mutex
	conn      *websocket.Conn
	runActive bool
	sendMu    sync.Mutex

	jobQueue   chan contracts.PrintJobCommand
	activeMu      sync.Mutex
	activeJobs    map[string]bool
	monitoredJobs map[string]bool
	calibrationMu sync.Mutex

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

	currentModeMu   sync.Mutex
	currentMode     ui_ipc.ScreenMode
	lastErrorMsg    string
	lastErrorDetail string
	adSlideIndex    int

	faultMu               sync.Mutex
	hardwareFault         bool
	faultReason           string
	faultCustomerMsg      string
	faultSources          map[string]bool
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

	snmpMonitor     *printer.SNMPHealthMonitor
	nativeIPPMu     sync.Mutex
	resolvedIPPURI  string
	acqMu           sync.Mutex
	acqSeq          uint64
	calibrationSeq  uint64
	lastRecoverySeq uint64
	snapshotMu      sync.RWMutex
	latestSnap      telemetrySnapshot
	reconcileMu     sync.Mutex
	reconciling     atomic.Bool
	persistenceMu         sync.Mutex
	pendingFinalizations  map[string]pendingFinalizeOp
	persistenceReadErr    string
	startupRecoveryFailed bool
	outcomeSyncCh         chan struct{}
}

type pendingFinalizeOp struct {
	orderID             string
	jobID               string
	evt                 contracts.JobProgressEvent
	invalidateInventory bool
	reason              string
}

const (
	// DefaultHealthPollInterval is the canonical background telemetry cycle (2s).
	DefaultHealthPollInterval = 2 * time.Second
	// DefaultTelemetryMaxAge is the canonical maximum age (6s) before required telemetry expires.
	DefaultTelemetryMaxAge = printer.DefaultMaxSourceAge
)

type telemetrySnapshot struct {
	HasSample     bool
	Seq           uint64
	AcquiredAt    time.Time
	Health        contracts.PrinterHealth
	Reconciled    printer.ReconciledHealthResult
	rawTrayStatus string
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
	if poller == nil && cfg.MockCUPSPrint != nil && cfg.CUPSRunner == nil &&
		cfg.MockCUPSHealth == nil && cfg.MockCUPSHealthDiag == nil && cfg.MockSNMPHealth == nil && cfg.MockIPPState == nil {
		// Explicit mock CUPS test mode
		poller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			now := time.Now().UTC()
			return contracts.PrinterHealth{
				Online:              true,
				State:               "IDLE",
				EvaluatedHealth:     contracts.EvaluatedHealthHealthy,
				NativeIPPAdmission:  contracts.AcceptingJobsAccepting,
				PaperTrayStatus:     "OK",
				EstimatedSheets:     estimatedSheets,
				InventoryCalibrated: true,
				ExecutionSafe:       true,
				TonerLevelPercent:   95,
				TonerLevelKnown:     true,
				DrumLifePercent:     98,
				DrumLifeKnown:       true,
				InputTrayLevel:      -1,
				InputTrayCapacity:   -1,
				UpdatedAt:           now,
				EvidenceFreshness:   contracts.BuildUniformEvidenceFreshness(now, DefaultTelemetryMaxAge),
			}
		}
	}

	wasExplicit := strings.TrimSpace(cfg.SNMPTarget) != ""
	var targetHost string
	if wasExplicit || (cfg.MockHealth == nil && cfg.MockSNMPHealth == nil) {
		targetHost = printer.ResolveSNMPTarget(context.Background(), cfg.CUPSRunner, cfg.SNMPTarget, cfg.CUPSQueue, cfg.PrinterAddr)
	}
	if !wasExplicit && targetHost != "" && strings.TrimSpace(cfg.CUPSQueue) != "" {
		log.Printf("[SPOOLER] Auto-discovered printer host %s from CUPS queue %s", targetHost, cfg.CUPSQueue)
	}
	cfg.SNMPTarget = targetHost

	var snmpMon *printer.SNMPHealthMonitor
	if strings.TrimSpace(targetHost) != "" {
		snmpCfg := printer.DefaultSNMPConfig()
		snmpCfg.Target = targetHost
		if cfg.SNMPPort > 0 {
			snmpCfg.Port = cfg.SNMPPort
		}
		if cfg.SNMPCommunity != "" {
			snmpCfg.Community = cfg.SNMPCommunity
		}
		snmpMon = printer.NewSNMPMonitor(snmpCfg, cfg.SNMPMonitorFactory)
	}

	if cfg.CompletedDisplayDuration <= 0 {
		cfg.CompletedDisplayDuration = 7 * time.Second
	}
	if cfg.ErrorDisplayDuration <= 0 {
		cfg.ErrorDisplayDuration = 10 * time.Second
	}
	if cfg.HealthPollInterval <= 0 {
		cfg.HealthPollInterval = DefaultHealthPollInterval
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
		outcomeSyncCh:         make(chan struct{}, 1),
		monitoredJobs:         make(map[string]bool),
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
		faultSources:          make(map[string]bool),
		cupsJobIDs:            make(map[string]string),
		capabilities:          initialCaps,
		ippState:              initialIPPState,
		snmpMonitor:           snmpMon,
		resolvedIPPURI:        strings.TrimSpace(cfg.NativeIPPURI),
	}

	if cfg.InputSource != nil {
		pinClient, err := cloud.NewHTTPPINClient(cfg.CloudURL, cfg.KioskID, cfg.KioskSecret, cfg.HTTPClient)
		if err == nil {
			pinCtrl, err := pin.NewController(pin.Config{
				InactivityTimeout: cfg.InactivityTimeout,
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
				pinCtrl.SetAvailability(false, "initializing kiosk availability")
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
	if c.cfg.MockHealth != nil || (c.cfg.CUPSRunner == nil && c.cfg.MockCUPSPrint != nil) {
		return c.Capabilities()
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
// against the configured CUPS queue and attempts initial native IPP endpoint resolution.
// Discovery errors are logged as warnings and produce unverified readiness rather than failing startup.
func (c *Client) DiscoverStartupPrinterState(ctx context.Context) printer.IPPPrinterStateResult {
	cupsQ := strings.TrimSpace(c.cfg.CUPSQueue)
	if cupsQ == "" {
		return printer.IPPPrinterStateResult{}
	}
	if c.cfg.MockHealth != nil || c.cfg.MockIPPState != nil || (c.cfg.CUPSRunner == nil && c.cfg.MockCUPSPrint != nil) {
		return c.IPPPrinterState()
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
		log.Printf("[SPOOLER] Warning: startup CUPS IPP printer-state discovery failed for queue %q: %v", cupsQ, err)
	} else {
		log.Printf("[SPOOLER] Discovered CUPS queue %q IPP state: state=%s (raw=%s), accepting=%s, reasons=%v (source: %s)",
			cupsQ, st.State, st.RawState, st.AcceptingJobs, st.RawReasons, st.Source)
	}

	if nativeURI, nErr := printer.ResolveNativeIPPEndpoint(diagCtx, c.cfg.CUPSRunner, cupsQ, c.cfg.NativeIPPURI); nErr == nil && nativeURI != "" {
		c.nativeIPPMu.Lock()
		c.resolvedIPPURI = nativeURI
		c.nativeIPPMu.Unlock()
		log.Printf("[SPOOLER] Resolved native physical printer IPP endpoint for queue %q", cupsQ)
	} else if nErr != nil {
		log.Printf("[SPOOLER] Warning: native physical printer IPP endpoint unresolved for queue %q: %v (readiness will remain UNVERIFIED)", cupsQ, nErr)
	}
	return c.IPPPrinterState()
}

// Run executes the spooler client loops until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	c.connMu.Lock()
	c.runActive = true
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		c.runActive = false
		c.connMu.Unlock()
	}()

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

	// 1. Read-only startup diagnostics and durable startup recovery BEFORE advertising readiness.
	c.DiscoverStartupCapabilities(ctx)
	c.DiscoverStartupPrinterState(ctx)

	// 2. Startup recovery check
	incompleteJobs, err := c.cfg.StateStore.GetIncompleteJobs(ctx)
	if err != nil {
		log.Printf("[SPOOLER] Startup recovery failed to read incomplete jobs: %v; failing closed", err)
		c.persistenceMu.Lock()
		c.startupRecoveryFailed = true
		c.persistenceMu.Unlock()
		c.setPersistenceReadBarrier(fmt.Errorf("startup recovery read failed: %w", err))
	} else if len(incompleteJobs) > 0 {
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
				c.persistenceMu.Lock()
				c.startupRecoveryFailed = true
				c.persistenceMu.Unlock()
				c.setPersistenceReadBarrier(errors.New(reason))
				c.pauseStartupOrderInconsistency(ctx, oid, incList, reason)
				continue
			}

			totalDocs := allOrderJobs[0].Command.TotalDocuments
			if totalDocs < 1 {
				totalDocs = 1
			}

			if len(allOrderJobs) != totalDocs {
				reason := fmt.Sprintf("startup recovery: order %s has inconsistent document count (expected %d, found %d); database inconsistency, manual reconciliation required", oid, totalDocs, len(allOrderJobs))
				log.Printf("[SPOOLER] %s", reason)
				c.persistenceMu.Lock()
				c.startupRecoveryFailed = true
				c.persistenceMu.Unlock()
				c.setPersistenceReadBarrier(errors.New(reason))
				c.pauseStartupOrderInconsistency(ctx, oid, incList, reason)
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
				c.persistenceMu.Lock()
				c.startupRecoveryFailed = true
				c.persistenceMu.Unlock()
				c.setPersistenceReadBarrier(errors.New(reason))
				c.pauseStartupOrderInconsistency(ctx, oid, incList, reason)
				continue
			}

			// Evaluate the entire order before enqueueing any documents.
			orderHasPausedOrError := false
			hasUncertainSubmitted := false
			for _, job := range allOrderJobs {
				if job.Status == contracts.JobStatusCompleted {
					continue
				}
				if job.SubmittedToPrinter || job.CUPSJobID != "" || job.SheetsPrinted > 0 || job.PagesPrinted > 0 {
					hasUncertainSubmitted = true
				}
				if job.Status == contracts.JobStatusFailedRefundable ||
					job.Status == contracts.JobStatusPausedError ||
					job.Status == contracts.JobStatusSpooling ||
					job.Status == contracts.JobStatusPrinting ||
					job.SubmittedToPrinter ||
					job.CUPSJobID != "" ||
					job.SheetsPrinted > 0 ||
					job.PagesPrinted > 0 ||
					strings.TrimSpace(job.ErrorReason) != "" {
					orderHasPausedOrError = true
				}
			}

			var jobsToResume []contracts.PrintJobCommand

			for _, incomplete := range incList {
				isUntouchedDispatched := incomplete.Status == contracts.JobStatusDispatched &&
					!incomplete.SubmittedToPrinter &&
					incomplete.CUPSJobID == "" &&
					incomplete.SheetsPrinted == 0 &&
					incomplete.PagesPrinted == 0 &&
					strings.TrimSpace(incomplete.ErrorReason) == ""
				isUnsubmittedStarted := (incomplete.Status == contracts.JobStatusSpooling || incomplete.Status == contracts.JobStatusDispatched) &&
					!incomplete.SubmittedToPrinter &&
					incomplete.CUPSJobID == "" &&
					incomplete.SheetsPrinted == 0 &&
					incomplete.PagesPrinted == 0

				if isUntouchedDispatched && !orderHasPausedOrError {
					jobsToResume = append(jobsToResume, incomplete.Command)
				} else if isUnsubmittedStarted {
					targetStatus := contracts.JobStatusPausedError
					reason := fmt.Sprintf("startup recovery: order %s has paused/failed sibling; keeping document %d unprinted", oid, incomplete.Command.DocumentIndex+1)
					if !hasUncertainSubmitted {
						targetStatus = contracts.JobStatusFailedRefundable
						if incomplete.Status == contracts.JobStatusSpooling {
							reason = firstNonEmptyString(incomplete.ErrorReason, "startup recovery: interrupted during document preparation before printer submission")
						}
					}
					log.Printf("[SPOOLER] %s", reason)
					c.finalizeAndEmitOrderFailure(ctx, oid, incomplete.Command.JobID, contracts.JobProgressEvent{
						JobID:         incomplete.Command.JobID,
						KioskID:       incomplete.Command.KioskID,
						Status:        targetStatus,
						PagesPrinted:  0,
						SheetsPrinted: 0,
						ErrorReason:   reason,
						Timestamp:     time.Now().UTC(),
					}, false)
				} else {
					// Potentially submitted or already partially printed.
					// Never automatically resubmit.
					orderHasPausedOrError = true
					if strings.TrimSpace(c.cfg.CUPSQueue) != "" && incomplete.CUPSJobID != "" && incomplete.Status != contracts.JobStatusPausedError {
						cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, incomplete.CUPSJobID)
						if qErr == nil && (cupsInfo.State == printer.CUPSJobStateCompleted ||
							cupsInfo.State == printer.CUPSJobStateProcessing ||
							cupsInfo.State == printer.CUPSJobStatePending ||
							cupsInfo.State == printer.CUPSJobStateHeld ||
							cupsInfo.State == printer.CUPSJobStateProcessingStopped) {
							// Route both active and CUPS-completed jobs through the single combined-evidence
							// monitoring/completion verifier using the persisted original baseline.
							log.Printf("[SPOOLER] Startup reconciliation: job %s (CUPS ID %s) is %s in CUPS; verifying via resumed monitoring", incomplete.Command.JobID, incomplete.CUPSJobID, cupsInfo.State)
							c.activeMu.Lock()
							c.activeJobs[incomplete.Command.JobID] = true
							c.activeMu.Unlock()
							incCopy := incomplete
							go c.resumeCUPSMonitoring(ctx, &incCopy)
							continue
						}
					}

					reason := incomplete.ErrorReason
					if strings.TrimSpace(reason) == "" {
						if incomplete.PagesPrinted > 0 || incomplete.SheetsPrinted > 0 {
							reason = "interrupted by daemon restart; physical output uncertain, reconciliation required"
						} else {
							reason = "interrupted during printer submission; physical output uncertain, manual reconciliation required"
						}
					}
					incCopy := incomplete
					c.reconcileSubmittedOrInterruptedJob(ctx, &incCopy, reason, true)
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
					if deferAdm, adm := c.shouldDeferForNativeIPPAdmission(ctx); deferAdm {
						c.deferJobsInOrder(jobsToResume)
						log.Printf("[SPOOLER] Startup recovery: deferred %d untouched job(s) for order %s (native IPP admission=%s)",
							len(jobsToResume), oid, adm)
					} else {
						c.activeMu.Lock()
						for _, j := range jobsToResume {
							c.activeJobs[j.JobID] = true
						}
						c.activeMu.Unlock()

						c.enqueueJobs(jobsToResume)
					}
				} else if len(jobsToResume) > 0 {
					reason := fmt.Sprintf("startup recovery: order %s has non-contiguous completion gap; manual reconciliation required", oid)
					log.Printf("[SPOOLER] %s", reason)
					c.persistenceMu.Lock()
					c.startupRecoveryFailed = true
					c.persistenceMu.Unlock()
					c.setPersistenceReadBarrier(errors.New(reason))
					for _, j := range jobsToResume {
						c.finalizeAndEmitOrderFailure(ctx, oid, j.JobID, contracts.JobProgressEvent{
							JobID:         j.JobID,
							KioskID:       j.KioskID,
							Status:        contracts.JobStatusPausedError,
							PagesPrinted:  0,
							SheetsPrinted: 0,
							ErrorReason:   reason,
							Timestamp:     time.Now().UTC(),
						}, false)
					}
				}
			}
		}
	}

	// Acquire canonical telemetry and render truthful startup screen once durable recovery completes.
	_ = c.CheckPrinterHealth(ctx)
	c.activeMu.Lock()
	hasActiveAtBoot := len(c.activeJobs) > 0
	c.activeMu.Unlock()
	if !hasActiveAtBoot && (c.pinController == nil || !c.pinController.HasActiveTransaction()) {
		c.syncCustomerServiceState(ctx)
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

	// 3d. Start background HTTP keep-alive probe to keep cloud backend (Render) warm 24/7
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
	c.syncCustomerServiceState(ctx)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		c.syncCustomerServiceState(ctx)
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

	// Synchronize any pending durable job outcomes with the cloud upon connection
	// BEFORE advertising readiness or accepting new dispatch commands.
	c.syncPendingOutcomes(connCtx, conn)

	// Start bounded connected retry loop for any unacknowledged safety-critical outcomes.
	connWg.Add(1)
	go func() {
		defer connWg.Done()
		c.retryPendingOutcomesLoop(connCtx, conn)
	}()

	// Publish latest shared telemetry snapshot immediately on connection
	initHealth := c.CurrentHealthSnapshot(connCtx)
	if err := c.sendWire(connCtx, conn, wireMessage{Type: "health", Health: &initHealth}); err != nil {
		if connCtx.Err() == nil {
			log.Printf("[SPOOLER] Initial health publish failed (%s)", c.sanitizeError(err))
		}
	}

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
			c.handleRefillMessage(connCtx, conn, msg)
		} else if msg.Type == "progress_ack" {
			c.handleProgressAck(connCtx, msg)
			c.signalOutcomeSync()
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
			// Heartbeats maintain connection liveness only; hardware acquisition runs strictly in the canonical telemetry loop.
			if err := c.sendWire(ctx, conn, wireMessage{Type: "heartbeat"}); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[SPOOLER] Heartbeat write failed (%s); terminating connection", c.sanitizeError(err))
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
	if err := ctx.Err(); err != nil {
		return err
	}

	if !c.sendMu.TryLock() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()

		acquired := false
		for !acquired {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if err := ctx.Err(); err != nil {
					return err
				}
				if c.sendMu.TryLock() {
					acquired = true
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		c.sendMu.Unlock()
		return err
	}
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
	c.sendProgressWithVersion(ctx, event, 0)
}

func (c *Client) sendProgressWithVersion(ctx context.Context, event contracts.JobProgressEvent, outcomeVersion int64) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	_ = wsjson.Write(ctx, conn, wireMessage{
		Type:           "progress",
		Progress:       &event,
		OutcomeVersion: outcomeVersion,
	})
}

func (c *Client) signalOutcomeSync() {
	if c.outcomeSyncCh == nil {
		return
	}
	select {
	case c.outcomeSyncCh <- struct{}{}:
	default:
	}
}

func (c *Client) syncPendingOutcomes(ctx context.Context, conn *websocket.Conn) bool {
	if c.cfg.StateStore == nil {
		return true
	}
	pending, err := c.cfg.StateStore.GetPendingOutcomeSyncJobs(ctx)
	if err != nil {
		log.Printf("[SPOOLER] Failed to query pending outcome sync jobs: %v", err)
		return false
	}
	if len(pending) == 0 {
		return true
	}
	for _, inc := range pending {
		if inc.OutcomeTimestamp.IsZero() {
			log.Printf("[SPOOLER] Refusing to sync outcome for %s v%d: missing durable outcome_timestamp", inc.Command.JobID, inc.OutcomeVersion)
			return false
		}
		evt := contracts.JobProgressEvent{
			JobID:         inc.Command.JobID,
			KioskID:       c.cfg.KioskID,
			Status:        inc.Status,
			PagesPrinted:  inc.PagesPrinted,
			SheetsPrinted: inc.SheetsPrinted,
			ErrorReason:   inc.ErrorReason,
			Timestamp:     inc.OutcomeTimestamp,
		}
		if err := c.sendWire(ctx, conn, wireMessage{
			Type:           "progress",
			Progress:       &evt,
			OutcomeVersion: inc.OutcomeVersion,
		}); err != nil {
			log.Printf("[SPOOLER] Pending outcome sync write failed for %s: %s", inc.Command.JobID, c.sanitizeError(err))
			return false
		}
	}
	return true
}

func (c *Client) retryPendingOutcomesLoop(ctx context.Context, conn *websocket.Conn) {
	if c.cfg.StateStore == nil {
		return
	}
	backoffs := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
	}
	attempt := 0
	for {
		pending, err := c.cfg.StateStore.GetPendingOutcomeSyncJobs(ctx)
		var wait time.Duration
		if err == nil && len(pending) == 0 {
			attempt = 0
			wait = 5 * time.Second
		} else {
			idx := attempt
			if idx >= len(backoffs) {
				idx = len(backoffs) - 1
			}
			wait = backoffs[idx]
			if err == nil && attempt > 0 {
				_ = c.syncPendingOutcomes(ctx, conn)
			} else if err != nil {
				log.Printf("[SPOOLER] Pending outcome sync query failed: %v", err)
			}
			attempt++
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.outcomeSyncCh:
			timer.Stop()
			attempt = 0
			_ = c.syncPendingOutcomes(ctx, conn)
		case <-timer.C:
		}
	}
}

func (c *Client) handleProgressAck(ctx context.Context, msg wireMessage) {
	if c.cfg.StateStore == nil || !msg.OK || msg.Progress == nil || msg.Progress.JobID == "" || msg.OutcomeVersion <= 0 {
		return
	}
	if err := c.cfg.StateStore.MarkOutcomeSynced(ctx, msg.Progress.JobID, msg.OutcomeVersion); err != nil {
		log.Printf("[SPOOLER] Failed to mark outcome synced for %s v%d: %v", msg.Progress.JobID, msg.OutcomeVersion, err)
	}
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
	if existing.Status == contracts.JobStatusPausedError || existing.Status == contracts.JobStatusFailedRefundable || strings.TrimSpace(existing.ErrorReason) != "" {
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
			status = contracts.JobStatusDispatched
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
	// If ANY sibling is paused or failed, the entire order is stopped and cannot be admitted for fresh printing.
	for idx := 0; idx < totalDocs; idx++ {
		sib := docMap[idx]
		if sib.Status == contracts.JobStatusPausedError || sib.Status == contracts.JobStatusFailedRefundable {
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

	// 3. Evaluate uncompleted documents for prior submission, started processing, or physical uncertainty:
	// Never requeue started, already-submitted, CUPS-queued, or physically started jobs for fresh printing.
	for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
		sib := docMap[idx]
		if sib.Status != contracts.JobStatusDispatched || sib.SubmittedToPrinter || sib.CUPSJobID != "" || sib.SheetsPrinted > 0 || strings.TrimSpace(sib.ErrorReason) != "" {
			log.Printf("[SPOOLER] Order %s doc %d (%s) not eligible for fresh admission (status=%s, submitted=%v, cupsID=%s, sheets=%d)",
				orderID, idx, sib.Command.JobID, sib.Status, sib.SubmittedToPrinter, sib.CUPSJobID, sib.SheetsPrinted)
			return
		}
	}

	// 4. Defer untouched DISPATCHED jobs when printer is healthy/safe but native IPP admission is not yet ACCEPTING.
	// (Hardware faults or uninitialized test queues still proceed to processJob/jobQueue so existing error handling runs.)
	deferForAdmission, admState := c.shouldDeferForNativeIPPAdmission(context.Background())

	// 5. Check in-memory state:
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
	if isBusy || deferForAdmission {
		// Defer uncompleted documents in strict index order
		for idx := firstUncompletedIdx; idx < totalDocs; idx++ {
			c.deferredJobs = append(c.deferredJobs, docMap[idx].Command)
		}
		c.deferredMu.Unlock()
		c.activeMu.Unlock()
		if isBusy {
			log.Printf("[SPOOLER] Order %s uncompleted documents (%d..%d of %d) deferred; kiosk is busy",
				orderID, firstUncompletedIdx+1, totalDocs, totalDocs)
		} else {
			log.Printf("[SPOOLER] Order %s uncompleted documents (%d..%d of %d) deferred; native IPP job admission is %s",
				orderID, firstUncompletedIdx+1, totalDocs, totalDocs, admState)
		}
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
	// Block new job admission while safety-critical outcomes for the same job or paused/failed state
	// have not been durably synchronized with the cloud, or if pending outcome query fails.
	pendingOutcomes, pErr := c.cfg.StateStore.GetPendingOutcomeSyncJobs(context.Background())
	if pErr != nil {
		log.Printf("[SPOOLER] Rejecting incoming job %s: failed to check pending outcome sync jobs (%v)", cmd.JobID, pErr)
		return
	}
	for _, po := range pendingOutcomes {
		if po.Command.JobID == cmd.JobID || po.Status == contracts.JobStatusPausedError || po.Status == contracts.JobStatusFailedRefundable {
			log.Printf("[SPOOLER] Deferring incoming job %s: pending outcome sync exists for %s (%s)", cmd.JobID, po.Command.JobID, po.Status)
			return
		}
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
		if sib.Status == contracts.JobStatusPausedError || sib.Status == contracts.JobStatusFailedRefundable || strings.TrimSpace(sib.ErrorReason) != "" {
			log.Printf("[SPOOLER] Rejecting job %s: order %s has stopped/failed sibling %s (status=%s, reason=%q)",
				cmd.JobID, orderID, sib.Command.JobID, sib.Status, sib.ErrorReason)
			return
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

	// 3. Durably persist job in SQLite as DISPATCHED (queued, processing not started)
	if err := c.cfg.StateStore.SaveJob(context.Background(), cmd, contracts.JobStatusDispatched); err != nil {
		log.Printf("[SPOOLER] Failed to persist initial job %s: %v", cmd.JobID, err)
		return
	}

	// 4. Send explicit job_ack back to cloud immediately upon durable persistence
	c.sendJobAck(context.Background(), contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusDispatched,
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
	c.setMonitoredJob(job.Command.JobID, true)
	defer func() {
		c.setMonitoredJob(job.Command.JobID, false)
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

	handleJobInterruption := func(reason string, isHardwareFault bool) {
		if !faultHandled.CompareAndSwap(false, true) {
			return
		}
		preserveCUPSJob.Store(true)
		if isHardwareFault {
			c.setHardwareFault(true, reason, "")
		}

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

		log.Printf("[SPOOLER] Interruption during resumed monitoring for %s (CUPS ID %s): %s (at page %d/%d, sheet %d/%d)",
			cmd.JobID, job.CUPSJobID, reason, curPages, cmd.TotalPages, curSheets, cmd.TotalSheets)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}

	monitorDone := make(chan struct{})
	// Ensure an initial canonical snapshot exists when resumed monitoring is invoked directly without Run.
	_ = c.ensureOrCurrentHealthSnapshot(jobCtx)

	// Active shared-snapshot consumer during resumed CUPS monitoring (no independent hardware acquisition).
	go func() {
		defer close(monitorDone)
		interval := 250 * time.Millisecond
		if c.cfg.HealthPollInterval > 0 && c.cfg.HealthPollInterval < interval {
			interval = c.cfg.HealthPollInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				h := c.CurrentHealthSnapshot(jobCtx)
				if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
					handleJobInterruption(reason, true)
					return
				}
				if h.EvaluatedHealth == contracts.EvaluatedHealthUnverified {
					handleJobInterruption("required printer telemetry unverified or expired during resumed printing; physical output uncertain, manual reconciliation required", false)
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

		if !c.recordPhysicalSheetProgress(jobCtx, cmd, "Job ID: "+cmd.JobID, cmd.StartFromPage, 0, true, streamSheetsCompleted, &pagesPrinted, &sheetsPrinted) {
			cancelJob()
			return
		}
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

	// Package 6B requirement: uncertain consumption must be reconciled before clearing the
	// corresponding execution-safety barrier and restoring available inventory.
	baseline := printer.CounterBaseline{
		HasBaseline:     job.BaselineCounterSet,
		Value:           job.BaselineCounterValue,
		Index:           job.BaselineCounterIndex,
		Unit:            job.BaselineCounterUnit,
		ObservedAt:      job.BaselineObservedAt,
		PersistedSheets: job.SheetsPrinted,
	}
	poller := c.snmpPoller
	if poller == nil {
		if snmpCfg := c.getSNMPConfig(); snmpCfg != nil {
			cfgCopy := *snmpCfg
			counterIdx := baseline.Index
			poller = func(pCtx context.Context) printer.HardwareTelemetry {
				return printer.PollSelectedCounterTelemetry(pCtx, cfgCopy, counterIdx)
			}
		}
	}
	err := printer.MonitorCUPSJobWithTelemetryBaselineCustom(jobCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, cmd, job.CUPSJobID, onPage, 500*time.Millisecond, poller, baseline)

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
		if isHw, reason, _ := faultFromPrintStreamError(err); isHw {
			handleJobInterruption(reason, true)
			return
		}
		c.handleJobError(ctx, cmd, curPages, curSheets, fmt.Sprintf("CUPS monitoring error: %v", err))
		return
	}

	compHealth := c.CurrentHealthSnapshot(ctx)
	if isFault, reason, _ := isBlockingHardwareFault(compHealth); isFault {
		handleJobInterruption(fmt.Sprintf("printer reported hardware error on completion (%s); physical output uncertain, manual reconciliation required", reason), true)
		return
	}
	if compHealth.EvaluatedHealth != contracts.EvaluatedHealthHealthy {
		handleJobInterruption(fmt.Sprintf("required printer readiness unverified on completion (%s); physical output uncertain, manual reconciliation required", compHealth.ErrorMessage), false)
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
			c.syncCustomerServiceState(ctx)
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

func (c *Client) shouldDeferForNativeIPPAdmission(ctx context.Context) (bool, contracts.AcceptingJobsStatus) {
	if ctx == nil {
		ctx = context.Background()
	}
	snap := c.ensureOrCurrentHealthSnapshot(ctx)
	if c.isHardwareFaultActive() || snap.EvaluatedHealth != contracts.EvaluatedHealthHealthy || !snap.ExecutionSafe {
		return false, snap.EffectiveNativeIPPAdmission()
	}
	adm := snap.EffectiveNativeIPPAdmission()
	return adm != contracts.AcceptingJobsAccepting, adm
}

func (c *Client) hasDeferredOrder(orderID string) bool {
	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()
	for _, dj := range c.deferredJobs {
		djOrderID := dj.OrderID
		if djOrderID == "" {
			djOrderID = dj.JobID
		}
		if djOrderID == orderID {
			return true
		}
	}
	return false
}

func (c *Client) deferJobsInOrder(cmds []contracts.PrintJobCommand) {
	if len(cmds) == 0 {
		return
	}
	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()
	existing := make(map[string]bool, len(c.deferredJobs))
	for _, dj := range c.deferredJobs {
		existing[dj.JobID] = true
	}
	for _, cmd := range cmds {
		if !existing[cmd.JobID] {
			c.deferredJobs = append(c.deferredJobs, cmd)
			existing[cmd.JobID] = true
		}
	}
}

func (c *Client) canAdmitFreshPhysicalSubmission(ctx context.Context) (bool, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	snap := c.ensureOrCurrentHealthSnapshot(ctx)
	if c.isHardwareFaultActive() {
		return false, firstNonEmptyString(c.getFaultReason(), "active hardware fault")
	}
	if ok, reason := contracts.ValidateReadyHealth(snap, time.Now().UTC(), DefaultTelemetryMaxAge); !ok {
		return false, reason
	}
	if adm := snap.EffectiveNativeIPPAdmission(); adm != contracts.AcceptingJobsAccepting {
		return false, fmt.Sprintf("native IPP job admission is %s", adm)
	}
	if safe, safeReason := c.computeExecutionSafety(ctx, ""); !safe {
		return false, safeReason
	}
	return true, ""
}

func (c *Client) drainDeferredJobsIfSafe(ctx context.Context) {
	c.deferredMu.Lock()
	hasDeferred := len(c.deferredJobs) > 0
	c.deferredMu.Unlock()
	if !hasDeferred {
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

	// Gate deferred admission on canonical printer readiness, native IPP admission, absence of active fault,
	// and durable SQLite execution safety BEFORE locking deferredMu so CheckPrinterHealth -> syncCustomerServiceState cannot self-deadlock.
	if ok, _ := c.canAdmitFreshPhysicalSubmission(ctx); !ok {
		return
	}

	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()

	if len(c.deferredJobs) == 0 {
		return
	}
	if c.pinController != nil && c.pinController.HasActiveTransaction() {
		return
	}
	c.activeMu.Lock()
	isBusyNow := len(c.activeJobs) > 0
	c.activeMu.Unlock()
	if isBusyNow {
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
	c.syncCustomerServiceState(ctx)

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

	// Durable processing boundary: atomically transition DISPATCHED -> SPOOLING before
	// capability validation, downloading, preparation, or any processing-related failure handling.
	if c.cfg.StateStore == nil {
		log.Printf("[SPOOLER] Rejecting job %s: StateStore not configured", cmd.JobID)
		c.setPersistenceReadBarrier(errors.New("state store not configured"))
		c.syncCustomerServiceState(ctx)
		return
	}
	if preStored, preErr := c.cfg.StateStore.GetJob(ctx, cmd.JobID); preErr == nil && preStored != nil {
		isUntouchedDispatched := preStored.Status == contracts.JobStatusDispatched &&
			!preStored.SubmittedToPrinter &&
			preStored.CUPSJobID == "" &&
			preStored.SheetsPrinted == 0 &&
			preStored.PagesPrinted == 0 &&
			strings.TrimSpace(preStored.ErrorReason) == ""
		if isUntouchedDispatched {
			if deferAdm, adm := c.shouldDeferForNativeIPPAdmission(ctx); deferAdm || c.hasDeferredOrder(orderID) {
				c.deferJobsInOrder([]contracts.PrintJobCommand{cmd})
				log.Printf("[SPOOLER] Deferring untouched job %s (order %s, doc %d/%d) before BeginJobSpooling (native IPP admission=%s)",
					cmd.JobID, orderID, cmd.DocumentIndex+1, cmd.TotalDocuments, adm)
				return
			}
		}
	}
	if err := c.cfg.StateStore.BeginJobSpooling(ctx, cmd.JobID); err != nil {
		log.Printf("[SPOOLER] Failed to transition job %s DISPATCHED -> SPOOLING: %v; failing closed", cmd.JobID, err)
		c.setPersistenceReadBarrier(fmt.Errorf("processing boundary persistence failed for %s: %w", cmd.JobID, err))
		c.syncCustomerServiceState(ctx)
		return
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

	// 2. Read durable job record (guaranteed present by BeginJobSpooling)
	savedJob, err := c.cfg.StateStore.GetJob(ctx, cmd.JobID)
	if err != nil || savedJob == nil {
		log.Printf("[SPOOLER] Failed to read durable job %s after BeginJobSpooling (%v); failing closed", cmd.JobID, err)
		c.setPersistenceReadBarrier(fmt.Errorf("durable job read failed for %s: %w", cmd.JobID, err))
		c.syncCustomerServiceState(ctx)
		return
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

	// 4. Report SPOOLING progress using durable progress counters
	pagesPrinted := savedJob.PagesPrinted
	sheetsPrinted := savedJob.SheetsPrinted

	spoolEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusSpooling,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	c.sendProgress(ctx, spoolEvt)

	// 5. Calculate start page and check paper supply
	startPage := pagesPrinted + 1
	if startPage < cmd.StartFromPage {
		startPage = cmd.StartFromPage
	}

	if startPage > cmd.TotalPages {
		if savedJob != nil && savedJob.CUPSJobID != "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
			c.resumeCUPSMonitoring(ctx, savedJob)
		} else {
			reason := fmt.Sprintf("pages already recorded (%d/%d) without verified terminal completion; manual reconciliation required", pagesPrinted, cmd.TotalPages)
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

	// 5b. Acquire or refresh canonical health snapshot before acquiring calibrationMu
	// so CheckPrinterHealth never runs while calibrationMu is held.
	_ = c.ensureOrCurrentHealthSnapshot(ctx)

	c.calibrationMu.Lock()
	estimatedSheets, isCalibrated, err := c.cfg.StateStore.GetPaperSupply(ctx)
	admHealth := c.CurrentHealthSnapshot(ctx)
	effectiveSheets := contracts.EffectiveInventorySheets(
		estimatedSheets,
		c.configuredTrayCapacity(),
		admHealth.InputTrayCapacity,
		admHealth.InputTrayCapacityKnown,
	)
	if err != nil || !isCalibrated || effectiveSheets < orderRemainingSheets || effectiveSheets <= 0 {
		c.calibrationMu.Unlock()
		reason := "paper tray supply insufficient for remaining print job"
		if err == nil && !isCalibrated {
			reason = "printer paper supply is uncalibrated; tray calibration required before submission"
		} else if err == nil && isCalibrated && effectiveSheets < orderRemainingSheets && estimatedSheets >= orderRemainingSheets {
			reason = fmt.Sprintf("effective verified tray capacity (%d sheets) insufficient for remaining print job (%d sheets)", effectiveSheets, orderRemainingSheets)
		}
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, reason)
		return
	}
	if c.isHardwareFaultActive() {
		c.calibrationMu.Unlock()
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, firstNonEmptyString(c.getFaultReason(), "active printer hardware fault blocks physical submission"))
		return
	}
	if isFault, reason, _ := isBlockingHardwareFault(admHealth); isFault {
		c.calibrationMu.Unlock()
		c.setHardwareFault(true, reason, "")
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, reason)
		return
	}
	if admHealth.EvaluatedHealth != contracts.EvaluatedHealthHealthy {
		c.calibrationMu.Unlock()
		reason := firstNonEmptyString(admHealth.ErrorMessage, fmt.Sprintf("printer readiness is %s", admHealth.EvaluatedHealth))
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("printer readiness gate rejected submission: %s", reason))
		return
	}
	if adm := admHealth.EffectiveNativeIPPAdmission(); adm != contracts.AcceptingJobsAccepting {
		c.calibrationMu.Unlock()
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("printer admission gate rejected submission: native IPP job admission is %s", adm))
		return
	}
	if safe, safeReason := c.computeExecutionSafety(ctx, cmd.JobID); !safe {
		c.calibrationMu.Unlock()
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("execution-safety barrier blocks submission: %s", safeReason))
		return
	}
	if capErr := c.validateJobCapabilities(ctx, cmd); capErr != nil {
		c.calibrationMu.Unlock()
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("printer capability validation rejected submission: %v", capErr))
		return
	}

	// 6. Imposed print artifact (zero Pi-side slicing or transformation)
	printPath := finalPath
	streamCmd := cmd

	// 7. Register genuine live physical monitoring (PRINTING state is recorded only once pre-submission persistence succeeds)
	c.setMonitoredJob(cmd.JobID, true)
	c.calibrationMu.Unlock()
	defer c.setMonitoredJob(cmd.JobID, false)

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

	handleJobInterruption := func(reason string, isHardwareFault bool) {
		if !faultHandled.CompareAndSwap(false, true) {
			return
		}
		preserveCUPSJob.Store(true)
		if isHardwareFault {
			c.setHardwareFault(true, reason, "")
		}

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

		log.Printf("[SPOOLER] Interruption for job %s (CUPS ID %s): %s (at page %d/%d, sheet %d/%d)",
			cmd.JobID, cID, reason, curPages, cmd.TotalPages, curSheets, cmd.TotalSheets)
		c.handleJobError(ctx, cmd, curPages, curSheets, reason)
	}

	monitorDone := make(chan struct{})

	// Active monitor consumes the shared telemetry snapshot (no independent hardware polling).
	go func() {
		defer close(monitorDone)
		interval := 250 * time.Millisecond
		if c.cfg.HealthPollInterval > 0 && c.cfg.HealthPollInterval < interval {
			interval = c.cfg.HealthPollInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				h := c.CurrentHealthSnapshot(jobCtx)
				if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
					handleJobInterruption(reason, true)
					return
				}
				if h.EvaluatedHealth == contracts.EvaluatedHealthUnverified {
					handleJobInterruption("required printer telemetry unverified or expired during printing; physical output uncertain, manual reconciliation required", false)
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
			isCUPS := strings.TrimSpace(c.cfg.CUPSQueue) != ""
			if !c.recordPhysicalSheetProgress(jobCtx, cmd, docDetail, startPage, initialSheetsOffset, isCUPS, streamSheetsCompleted, &pagesPrinted, &sheetsPrinted) {
				cancelJob()
				return
			}

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

	emitPrinting := func() {
		progressMu.Lock()
		curP, curS := pagesPrinted, sheetsPrinted
		progressMu.Unlock()
		printEvt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       cmd.KioskID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  curP,
			SheetsPrinted: curS,
			Timestamp:     time.Now().UTC(),
		}
		_ = c.cfg.StateStore.RecordSheetProgress(jobCtx, printEvt)
		c.sendProgress(jobCtx, printEvt)
	}

	var printErr error
	if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		log.Printf("[SPOOLER] Sending job %s to CUPS queue %s", cmd.JobID, c.cfg.CUPSQueue)
		if c.cfg.MockCUPSPrint != nil {
			if ok, admitReason := c.canAdmitFreshPhysicalSubmission(jobCtx); !ok {
				printErr = fmt.Errorf("printer admission gate rejected mock CUPS submission: %s", admitReason)
			} else if err := c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID); err != nil {
				printErr = fmt.Errorf("pre-submission persistence failed for mock CUPS print: %w", err)
			} else {
				emitPrinting()
				printErr = c.cfg.MockCUPSPrint(jobCtx, c.cfg.CUPSQueue, streamCmd, printPath, onPage)
			}
		} else {
			hooks := &printer.CUPSPrintHooks{
				OnSubmittingWithBaseline: func(baseline printer.CounterBaseline) error {
					if ok, admitReason := c.canAdmitFreshPhysicalSubmission(jobCtx); !ok {
						return fmt.Errorf("printer admission gate rejected CUPS submission: %s", admitReason)
					}
					if err := c.cfg.StateStore.MarkJobSubmittingWithBaseline(
						jobCtx,
						cmd.JobID,
						baseline.HasBaseline,
						baseline.Value,
						baseline.Index,
						baseline.Unit,
						baseline.ObservedAt,
					); err != nil {
						return err
					}
					emitPrinting()
					return nil
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
		if ok, admitReason := c.canAdmitFreshPhysicalSubmission(jobCtx); !ok {
			printErr = fmt.Errorf("printer admission gate rejected PJL submission: %s", admitReason)
		} else if err := c.cfg.StateStore.MarkJobSubmitting(jobCtx, cmd.JobID); err != nil {
			printErr = fmt.Errorf("pre-submission persistence failed for PJL print: %w", err)
		} else {
			emitPrinting()
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
		if isHw, reason, _ := faultFromPrintStreamError(printErr); isHw {
			handleJobInterruption(reason, true)
			return
		}
		// Consume shared telemetry snapshot (refresh in-memory MockHealth simulation on simulated stream abort).
		var h contracts.PrinterHealth
		if c.cfg.MockHealth != nil {
			h = c.CheckPrinterHealth(ctx)
		} else {
			h = c.CurrentHealthSnapshot(ctx)
		}
		if isFault, reason, _ := isBlockingHardwareFault(h); isFault {
			handleJobInterruption(reason, true)
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

	compHealth := c.CurrentHealthSnapshot(ctx)
	if isFault, reason, _ := isBlockingHardwareFault(compHealth); isFault {
		handleJobInterruption(fmt.Sprintf("printer reported hardware error on completion (%s); physical output uncertain, manual reconciliation required", reason), true)
		return
	}
	if compHealth.EvaluatedHealth != contracts.EvaluatedHealthHealthy {
		handleJobInterruption(fmt.Sprintf("required printer readiness unverified on completion (%s); physical output uncertain, manual reconciliation required", compHealth.ErrorMessage), false)
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

// recordPhysicalSheetProgress is the single shared accounting implementation used by both
// initial execution and resumed monitoring to commit atomic SQLite progress and inventory updates.
func (c *Client) recordPhysicalSheetProgress(
	jobCtx context.Context,
	cmd contracts.PrintJobCommand,
	docDetail string,
	startPage int,
	initialSheetsOffset int,
	isCUPS bool,
	streamSheetsCompleted int,
	pagesPrinted *int,
	sheetsPrinted *int,
) bool {
	targetSheets := initialSheetsOffset + streamSheetsCompleted
	if isCUPS {
		targetSheets = streamSheetsCompleted
	}
	if targetSheets > cmd.TotalSheets {
		targetSheets = cmd.TotalSheets
	}
	if targetSheets < *sheetsPrinted {
		targetSheets = *sheetsPrinted
	}

	nextPages := *pagesPrinted
	if cmd.Duplex == contracts.DuplexSimplex {
		nextPages = (startPage - 1) + streamSheetsCompleted/cmd.Copies
	} else {
		nextPages = (startPage - 1) + (streamSheetsCompleted*2)/cmd.Copies
	}
	if nextPages > cmd.TotalPages {
		nextPages = cmd.TotalPages
	}
	if nextPages < *pagesPrinted {
		nextPages = *pagesPrinted
	}

	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  nextPages,
		SheetsPrinted: targetSheets,
		Timestamp:     time.Now().UTC(),
	}
	if err := c.cfg.StateStore.RecordSheetProgress(jobCtx, evt); err != nil {
		log.Printf("[SPOOLER] RecordSheetProgress error for job %s: %v", cmd.JobID, err)
		return false
	}
	*pagesPrinted = nextPages
	*sheetsPrinted = targetSheets
	c.sendProgress(jobCtx, evt)
	return true
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

	var outcomeVersion int64
	if stored, err := c.cfg.StateStore.GetJob(ctx, cmd.JobID); err == nil && stored != nil {
		outcomeVersion = stored.OutcomeVersion
		if !stored.OutcomeTimestamp.IsZero() {
			evt.Timestamp = stored.OutcomeTimestamp
		}
	}
	c.sendProgressWithVersion(ctx, evt, outcomeVersion)
	c.signalOutcomeSync()

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

	// Orderly daemon shutdown (ctx canceled) on an unsubmitted job without physical progress
	// must not mark a customer print failure.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer persistCancel()

	stored, err := c.cfg.StateStore.GetJob(persistCtx, cmd.JobID)
	if ctx != nil && ctx.Err() != nil && err == nil && stored != nil &&
		!stored.SubmittedToPrinter &&
		strings.TrimSpace(stored.CUPSJobID) == "" &&
		stored.SheetsPrinted == 0 &&
		pagesPrinted == 0 &&
		sheetsPrinted == 0 &&
		strings.Contains(strings.ToLower(reason), "context canceled") {
		log.Printf("[SPOOLER] Ignoring shutdown context cancellation on unsubmitted job %s", cmd.JobID)
		return
	}

	customerMsg, customerDetail := formatCustomerError(cmd, pagesPrinted, reason)
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: customerMsg,
		Detail:  customerDetail,
	})

	if err != nil || stored == nil {
		// Fail closed: never transmit uncommitted outcomes or clear barriers when durable state cannot be read.
		log.Printf("[SPOOLER] Fail-closed: cannot read durable state for job %s during error handling (%v)", cmd.JobID, err)
		readErr := err
		if readErr == nil {
			readErr = fmt.Errorf("missing durable job record for %s", cmd.JobID)
		}
		c.setPersistenceReadBarrier(readErr)
		c.syncCustomerServiceState(persistCtx)
		return
	}
	if stored.Status == contracts.JobStatusCompleted {
		return
	}

	definitelyUnsubmitted := !stored.SubmittedToPrinter &&
		strings.TrimSpace(stored.CUPSJobID) == "" &&
		stored.SheetsPrinted == 0 &&
		stored.Status != contracts.JobStatusPrinting &&
		pagesPrinted == 0 &&
		sheetsPrinted == 0
	if definitelyUnsubmitted {
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       cmd.KioskID,
			Status:        contracts.JobStatusFailedRefundable,
			PagesPrinted:  pagesPrinted,
			SheetsPrinted: sheetsPrinted,
			ErrorReason:   reason,
			Timestamp:     time.Now().UTC(),
		}
		c.finalizeAndEmitOrderFailure(persistCtx, cmd.OrderID, cmd.JobID, evt, false)
	} else {
		storedCopy := *stored
		if pagesPrinted > storedCopy.PagesPrinted {
			storedCopy.PagesPrinted = pagesPrinted
		}
		if sheetsPrinted > storedCopy.SheetsPrinted {
			storedCopy.SheetsPrinted = sheetsPrinted
		}
		c.reconcileSubmittedOrInterruptedJob(persistCtx, &storedCopy, reason, false)
	}

	c.scheduleResetToIdle(c.cfg.ErrorDisplayDuration)
}

func (c *Client) finalizeAndEmitOrderFailure(
	ctx context.Context,
	orderID string,
	jobID string,
	evt contracts.JobProgressEvent,
	invalidateInventory bool,
) {
	persistCtx := ctx
	var persistCancel context.CancelFunc
	if persistCtx == nil || persistCtx.Err() != nil {
		persistCtx, persistCancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer persistCancel()
	}

	updatedJobs, err := c.cfg.StateStore.FinalizeOrderFailure(persistCtx, orderID, jobID, evt, invalidateInventory)
	if err != nil {
		// Fail closed: associate this unresolved failure with the exact job and retain the persistence barrier.
		log.Printf("[SPOOLER] FinalizeOrderFailure failed for %s: %v; retaining fail-closed persistence barrier", jobID, err)
		c.recordPendingFinalization(pendingFinalizeOp{
			orderID:             orderID,
			jobID:               jobID,
			evt:                 evt,
			invalidateInventory: invalidateInventory,
			reason:              firstNonEmptyString(evt.ErrorReason, "failed to persist durable failure state"),
		})
		c.syncCustomerServiceState(persistCtx)
		return
	}
	c.clearPendingFinalization(jobID)
	for _, u := range updatedJobs {
		if u.Status == contracts.JobStatusPausedError || u.Status == contracts.JobStatusFailedRefundable {
			c.clearPendingFinalization(u.Command.JobID)
		}
	}

	for _, job := range updatedJobs {
		if job.Status != contracts.JobStatusPausedError && job.Status != contracts.JobStatusFailedRefundable {
			continue
		}
		if job.OutcomeTimestamp.IsZero() {
			log.Printf("[SPOOLER] Skipping progress emit for %s: missing durable outcome_timestamp", job.Command.JobID)
			continue
		}
		c.sendProgressWithVersion(persistCtx, contracts.JobProgressEvent{
			JobID:         job.Command.JobID,
			KioskID:       firstNonEmptyString(job.Command.KioskID, c.cfg.KioskID),
			Status:        job.Status,
			PagesPrinted:  job.PagesPrinted,
			SheetsPrinted: job.SheetsPrinted,
			ErrorReason:   job.ErrorReason,
			Timestamp:     job.OutcomeTimestamp,
		}, job.OutcomeVersion)
	}
	c.signalOutcomeSync()

	if invalidateInventory {
		c.syncCustomerServiceState(persistCtx)
	}
}

func (c *Client) recordPendingFinalization(op pendingFinalizeOp) {
	c.persistenceMu.Lock()
	if c.pendingFinalizations == nil {
		c.pendingFinalizations = make(map[string]pendingFinalizeOp)
	}
	op.reason = strings.TrimSpace(op.reason)
	if op.reason == "" {
		op.reason = "durable persistence failure barrier active"
	}
	c.pendingFinalizations[op.jobID] = op
	c.persistenceMu.Unlock()
}

func (c *Client) clearPendingFinalization(jobID string) {
	c.persistenceMu.Lock()
	delete(c.pendingFinalizations, jobID)
	c.persistenceMu.Unlock()
}

func (c *Client) setPersistenceReadBarrier(err error) {
	c.persistenceMu.Lock()
	if err != nil {
		c.persistenceReadErr = strings.TrimSpace(err.Error())
		if c.persistenceReadErr == "" {
			c.persistenceReadErr = "durable state read failure"
		}
	} else {
		c.persistenceReadErr = ""
	}
	c.persistenceMu.Unlock()
}

func (c *Client) retryPendingFinalizations(ctx context.Context) {
	c.persistenceMu.Lock()
	if len(c.pendingFinalizations) == 0 {
		c.persistenceMu.Unlock()
		return
	}
	ops := make([]pendingFinalizeOp, 0, len(c.pendingFinalizations))
	for _, op := range c.pendingFinalizations {
		ops = append(ops, op)
	}
	c.persistenceMu.Unlock()

	for _, op := range ops {
		c.finalizeAndEmitOrderFailure(ctx, op.orderID, op.jobID, op.evt, op.invalidateInventory)
	}
}

func (c *Client) isPersistenceBlocked() bool {
	c.persistenceMu.Lock()
	defer c.persistenceMu.Unlock()
	return len(c.pendingFinalizations) > 0 || c.persistenceReadErr != "" || c.startupRecoveryFailed
}

func (c *Client) persistenceBlockReason() string {
	c.persistenceMu.Lock()
	defer c.persistenceMu.Unlock()
	if c.startupRecoveryFailed {
		return "startup durable state recovery failed"
	}
	if c.persistenceReadErr != "" {
		return c.persistenceReadErr
	}
	for _, op := range c.pendingFinalizations {
		return op.reason
	}
	return ""
}

func appendReasonOnce(base, addition string) string {
	base = strings.TrimSpace(base)
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return base
	}
	if base == "" {
		return addition
	}
	if strings.Contains(base, addition) {
		return base
	}
	return base + "; " + addition
}

func isQuiescentPostTermination(snap contracts.PrinterHealth, termTime time.Time, cancelIssued bool) bool {
	if snap.EvaluatedHealth != contracts.EvaluatedHealthHealthy || !snap.Online {
		return false
	}
	stateUpper := strings.ToUpper(strings.TrimSpace(snap.State))
	if stateUpper != "IDLE" && stateUpper != "READY" {
		return false
	}
	if snap.PaperJam || snap.CoverOpen || snap.OutputBinFull || snap.PaperTrayStatus == "EMPTY" || len(snap.BlockingReasons) > 0 {
		return false
	}
	if cancelIssued && !termTime.IsZero() && snap.UpdatedAt.Before(termTime) {
		return false
	}
	ok, _, _ := contracts.ValidateHealthEvidenceFreshness(snap, time.Now().UTC(), DefaultTelemetryMaxAge)
	return ok
}

func (c *Client) verifyPostTerminationQuiescence(ctx context.Context, termTime time.Time) (bool, contracts.PrinterHealth) {
	if c.cfg.MockHealth != nil || c.getSNMPConfig() != nil || c.snmpPoller != nil {
		_, _, _ = c.refreshPrinterHealthTelemetry(ctx)
	}
	snap := c.CurrentHealthSnapshot(ctx)
	return !c.isHardwareFaultActive() && isQuiescentPostTermination(snap, termTime, true), snap
}

func (c *Client) finalizeStoppedUnsubmittedSiblings(ctx context.Context, orderID, completedJobID, reason string) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		orderID = completedJobID
	}
	siblings, err := c.cfg.StateStore.GetJobsByOrderID(ctx, orderID)
	if err != nil {
		return
	}
	for _, sib := range siblings {
		if sib.Command.JobID == completedJobID || sib.Status != contracts.JobStatusPausedError {
			continue
		}
		if !sib.SubmittedToPrinter && strings.TrimSpace(sib.CUPSJobID) == "" && sib.SheetsPrinted == 0 && sib.PagesPrinted == 0 {
			evt := contracts.JobProgressEvent{
				JobID:         sib.Command.JobID,
				KioskID:       firstNonEmptyString(sib.Command.KioskID, c.cfg.KioskID),
				Status:        contracts.JobStatusFailedRefundable,
				PagesPrinted:  0,
				SheetsPrinted: 0,
				ErrorReason:   firstNonEmptyString(sib.ErrorReason, reason),
				Timestamp:     time.Now().UTC(),
			}
			c.finalizeAndEmitOrderFailure(ctx, orderID, sib.Command.JobID, evt, false)
		}
	}
}

// reconcileSubmittedOrInterruptedJob is the single canonical reconciliation path for
// submitted or ambiguous jobs across runtime failure, startup recovery, and printer recovery.
// It serializes against active monitoring/submission, re-reads durable state, stops active CUPS work,
// requires distinct fresh post-termination quiescent hardware telemetry, reuses Package 6A's canonical
// verifier and accounting for completed CUPS jobs when allowed, finalizes to FAILED_REFUNDABLE only
// when further physical output is ruled out, preserves PAUSED_ERROR otherwise, stops unsubmitted order
// siblings, and atomically invalidates inventory calibration when physical consumption is uncertain.
func (c *Client) reconcileSubmittedOrInterruptedJob(
	ctx context.Context,
	stored *state.StoredJob,
	baseReason string,
	allowCompletionVerify bool,
) contracts.JobStatus {
	if stored == nil {
		return ""
	}
	c.reconcileMu.Lock()
	reconcileLocked := true
	defer func() {
		if reconcileLocked {
			c.reconcileMu.Unlock()
		}
	}()

	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 5*time.Second)
	defer reconcileCancel()

	// Re-read durable state under the reconciliation boundary before acting on a previously loaded snapshot.
	freshStored, getErr := c.cfg.StateStore.GetJob(reconcileCtx, stored.Command.JobID)
	if getErr != nil {
		c.setPersistenceReadBarrier(fmt.Errorf("reconciliation state read error for %s: %w", stored.Command.JobID, getErr))
		return stored.Status
	}
	if freshStored != nil {
		// Preserve higher monotonic progress passed by active monitoring while keeping durable status/flags.
		if stored.PagesPrinted > freshStored.PagesPrinted {
			freshStored.PagesPrinted = stored.PagesPrinted
		}
		if stored.SheetsPrinted > freshStored.SheetsPrinted {
			freshStored.SheetsPrinted = stored.SheetsPrinted
		}
		stored = freshStored
	}

	if stored.Status == contracts.JobStatusCompleted || stored.Status == contracts.JobStatusFailedRefundable {
		return stored.Status
	}

	reconciledSheets, reconciledPages := reconcileSheetsAndPages(
		stored.Command,
		stored.SheetsPrinted,
		stored.PagesPrinted,
		0,
	)

	invalidateInventory := stored.SubmittedToPrinter ||
		strings.TrimSpace(stored.CUPSJobID) != "" ||
		stored.SheetsPrinted > 0

	targetStatus := contracts.JobStatusPausedError
	reason := strings.TrimSpace(baseReason)
	if reason == "" {
		reason = "interrupted printer submission; physical output uncertain, manual reconciliation required"
	}

	if strings.TrimSpace(c.cfg.CUPSQueue) != "" && strings.TrimSpace(stored.CUPSJobID) != "" {
		cupsInfo, qErr := printer.QueryCUPSJobInfo(reconcileCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, stored.CUPSJobID)
		if qErr != nil {
			reason = appendReasonOnce(reason, fmt.Sprintf("error querying CUPS job %s (%v), manual reconciliation required", stored.CUPSJobID, qErr))
		} else {
			switch cupsInfo.State {
			case printer.CUPSJobStatePending, printer.CUPSJobStateProcessing, printer.CUPSJobStateHeld, printer.CUPSJobStateProcessingStopped:
				// Preserve already-held jobs during an active hardware fault until fault clears;
				// always attempt termination for active resumable (pending/processing) CUPS jobs.
				if c.isHardwareFaultActive() && (cupsInfo.State == printer.CUPSJobStateHeld || cupsInfo.State == printer.CUPSJobStateProcessingStopped) {
					targetStatus = contracts.JobStatusPausedError
					break
				}
				cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 2*time.Second)
				cancelRes := printer.CancelAndVerifyCUPSJob(cancelCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, stored.CUPSJobID)
				cancelCancel()
				termTime := time.Now().UTC()

				if cancelRes.ConfirmedStopped {
					if ok, _ := c.verifyPostTerminationQuiescence(reconcileCtx, termTime); ok {
						targetStatus = contracts.JobStatusFailedRefundable
						reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s confirmed %s and hardware quiescent (%d/%d sheets verified)",
							stored.CUPSJobID, cancelRes.State, reconciledSheets, stored.Command.TotalSheets))
					} else {
						reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s confirmed %s, awaiting fresh quiescent hardware evidence before finalization",
							stored.CUPSJobID, cancelRes.State))
					}
				} else if cancelRes.AlreadyCompleted {
					reason = appendReasonOnce(reason, fmt.Sprintf("CUPS completed job without physical proof (CUPS counter: %d sheets, verified physical: %d/%d), physical output uncertain, manual reconciliation required",
						cancelRes.JobInfo.SheetsCompleted, reconciledSheets, stored.Command.TotalSheets))
				} else {
					reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s state is %s after cancel (unmonitored physical output risk), operator reconciliation required",
						stored.CUPSJobID, cancelRes.State))
				}

			case printer.CUPSJobStateCanceled, printer.CUPSJobStateAborted:
				qCheckErr := printer.VerifyNoUnrelatedActiveCUPSJob(reconcileCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, "")
				termTime := time.Now().UTC()
				if qCheckErr != nil {
					reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s is %s but active queue verification failed (%v), manual reconciliation required",
						stored.CUPSJobID, cupsInfo.State, qCheckErr))
				} else if ok, _ := c.verifyPostTerminationQuiescence(reconcileCtx, termTime); ok {
					targetStatus = contracts.JobStatusFailedRefundable
					reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s confirmed %s and hardware quiescent (%d/%d sheets verified)",
						stored.CUPSJobID, cupsInfo.State, reconciledSheets, stored.Command.TotalSheets))
				} else {
					reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s is %s, awaiting fresh quiescent hardware evidence before finalization",
						stored.CUPSJobID, cupsInfo.State))
				}

			case printer.CUPSJobStateCompleted:
				// Every COMPLETED outcome must pass the complete canonical Package 6A evidence verifier
				// and canonical recordPhysicalSheetProgress accounting (no full-sheets shortcut).
				if allowCompletionVerify && !c.isHardwareFaultActive() && stored.BaselineCounterSet && (c.snmpPoller != nil || c.getSNMPConfig() != nil) {
					baseline := printer.CounterBaseline{
						HasBaseline:     stored.BaselineCounterSet,
						Value:           stored.BaselineCounterValue,
						Index:           stored.BaselineCounterIndex,
						Unit:            stored.BaselineCounterUnit,
						ObservedAt:      stored.BaselineObservedAt,
						PersistedSheets: stored.SheetsPrinted,
					}
					poller := c.snmpPoller
					if poller == nil {
						if snmpCfg := c.getSNMPConfig(); snmpCfg != nil {
							cfgCopy := *snmpCfg
							counterIdx := baseline.Index
							poller = func(pCtx context.Context) printer.HardwareTelemetry {
								return printer.PollSelectedCounterTelemetry(pCtx, cfgCopy, counterIdx)
							}
						}
					}
					if poller != nil {
						curPages := reconciledPages
						curSheets := reconciledSheets
						var progressMu sync.Mutex
						verifyCtx, verifyCancel := context.WithCancel(reconcileCtx)
						verifyErr := printer.MonitorCUPSJobWithTelemetryBaselineCustom(
							verifyCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, stored.Command, stored.CUPSJobID,
							func(streamSheets int) {
								progressMu.Lock()
								defer progressMu.Unlock()
								if !c.recordPhysicalSheetProgress(verifyCtx, stored.Command, "Job ID: "+stored.Command.JobID, stored.Command.StartFromPage, 0, true, streamSheets, &curPages, &curSheets) {
									verifyCancel()
								}
							},
							50*time.Millisecond, poller, baseline,
						)
						verifyCancel()

						progressMu.Lock()
						finalSheets := curSheets
						finalPages := curPages
						progressMu.Unlock()

						if verifyErr == nil && finalSheets >= stored.Command.TotalSheets {
							snap := c.CurrentHealthSnapshot(reconcileCtx)
							if snap.EvaluatedHealth == contracts.EvaluatedHealthHealthy {
								reconcileLocked = false
								c.reconcileMu.Unlock()
								c.completeJob(reconcileCtx, stored.Command)
								if post, pErr := c.cfg.StateStore.GetJob(reconcileCtx, stored.Command.JobID); pErr == nil && post != nil && post.Status == contracts.JobStatusCompleted {
									c.finalizeStoppedUnsubmittedSiblings(reconcileCtx, stored.Command.OrderID, stored.Command.JobID, "Order stopped after earlier interruption")
									return contracts.JobStatusCompleted
								}
								return stored.Status
							}
						}
						reconciledSheets = finalSheets
						reconciledPages = finalPages
					}
				}
				// When a completed CUPS job lacks successful physical-output proof, never mark it COMPLETED.
				// If exact terminal state, empty active queue, and fresh quiescent hardware evidence establish
				// that further output cannot occur, finalize unsuccessfully using only verified physical progress.
				qCheckErr := printer.VerifyNoUnrelatedActiveCUPSJob(reconcileCtx, c.cfg.CUPSRunner, c.cfg.CUPSQueue, "")
				termTime := time.Now().UTC()
				if qCheckErr == nil {
					if ok, _ := c.verifyPostTerminationQuiescence(reconcileCtx, termTime); ok {
						targetStatus = contracts.JobStatusFailedRefundable
						reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s completed without full physical proof and hardware is quiescent (%d/%d sheets verified)",
							stored.CUPSJobID, reconciledSheets, stored.Command.TotalSheets))
						break
					}
				}
				reason = appendReasonOnce(reason, fmt.Sprintf("CUPS completed job without physical proof (CUPS counter: %d sheets, verified physical: %d/%d), physical output uncertain, manual reconciliation required",
					cupsInfo.SheetsCompleted, reconciledSheets, stored.Command.TotalSheets))

			default:
				reason = appendReasonOnce(reason, fmt.Sprintf("CUPS job %s state is %s, physical output uncertain, manual reconciliation required",
					stored.CUPSJobID, cupsInfo.State))
			}
		}
	}

	log.Printf("[SPOOLER] Reconciliation for job %s (CUPS ID %s, status %s): %s",
		stored.Command.JobID, stored.CUPSJobID, targetStatus, reason)

	evt := contracts.JobProgressEvent{
		JobID:         stored.Command.JobID,
		KioskID:       firstNonEmptyString(stored.Command.KioskID, c.cfg.KioskID),
		Status:        targetStatus,
		PagesPrinted:  reconciledPages,
		SheetsPrinted: reconciledSheets,
		ErrorReason:   reason,
		Timestamp:     time.Now().UTC(),
	}
	c.finalizeAndEmitOrderFailure(reconcileCtx, stored.Command.OrderID, stored.Command.JobID, evt, invalidateInventory)
	return targetStatus
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
	if !active {
		c.consecutiveHealthy = 0
		c.faultSources = make(map[string]bool)
	} else if len(c.faultSources) == 0 {
		if c.faultSources == nil {
			c.faultSources = make(map[string]bool)
		}
		c.faultSources["hardware"] = true
	}
}

func isBlockingHardwareFault(h contracts.PrinterHealth) (bool, string, string) {
	if h.PaperJam {
		return true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer."
	}
	if h.CoverOpen {
		return true, "printer cover is open", "The printer cover was opened."
	}
	if h.OutputBinFull {
		return true, "printer output bin full", "The printer output bin is full."
	}
	if h.PaperTrayStatus == "EMPTY" {
		return true, "printer paper tray is empty", "The printer has run out of paper."
	}
	if len(h.BlockingReasons) > 0 {
		r := h.BlockingReasons[0]
		msg := firstNonEmptyString(r.Message, h.ErrorMessage, r.Code, "printer reported blocking fault")
		switch r.Code {
		case "paper_jam":
			return true, msg, "A paper feed issue occurred with the printer."
		case "cover_open":
			return true, msg, "The printer cover was opened."
		case "output_bin_full":
			return true, msg, "The printer output bin is full."
		case "paper_empty":
			return true, msg, "The printer has run out of paper."
		case "snmp_offline", "cups_offline", "offline":
			return true, msg, "The printer is offline or unreachable."
		default:
			return true, msg, "A hardware issue interrupted printing your document."
		}
	}
	if h.State == "OFFLINE" || (!h.Online && h.EvaluatedHealth != contracts.EvaluatedHealthUnverified && h.State != "UNKNOWN" && h.State != "" && h.State != "IDLE" && h.State != "PRINTING" && h.State != "READY" && h.State != "WARMING_UP") {
		return true, firstNonEmptyString(h.ErrorMessage, "printer is offline or unreachable"), "The printer is offline or unreachable."
	}
	if h.State == "ERROR" || h.EvaluatedHealth == contracts.EvaluatedHealthFault {
		return true, firstNonEmptyString(h.ErrorMessage, "printer reported hardware error"), "A hardware issue interrupted printing your document."
	}
	return false, "", ""
}

func faultFromPrintStreamError(err error) (bool, string, string) {
	if err == nil {
		return false, "", ""
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "jam"):
		return true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer."
	case strings.Contains(lower, "cover") || strings.Contains(lower, "door"):
		return true, "printer cover is open", "The printer cover was opened."
	case strings.Contains(lower, "out of paper") || strings.Contains(lower, "media-empty") || strings.Contains(lower, "media-needed"):
		return true, "printer paper tray is empty", "The printer has run out of paper."
	case strings.Contains(lower, "bin full") || strings.Contains(lower, "output-area-full") || strings.Contains(lower, "output-bin-full"):
		return true, "printer output bin full", "The printer output bin is full."
	case strings.Contains(lower, "offline") || strings.Contains(lower, "unreachable"):
		return true, "printer is offline or unreachable", "The printer is offline or unreachable."
	default:
		return false, "", ""
	}
}

func collectFaultSources(h contracts.PrinterHealth) map[string]bool {
	out := make(map[string]bool)
	for _, r := range h.BlockingReasons {
		src := strings.ToLower(strings.TrimSpace(r.Source))
		if src == "" {
			continue
		}
		if src == "ipp" {
			src = "native_ipp"
		}
		out[src] = true
	}
	if len(out) == 0 {
		out["hardware"] = true
	}
	return out
}

func faultSourcesCleared(faultSources map[string]bool, h contracts.PrinterHealth, now time.Time, maxAge time.Duration) bool {
	if h.EvaluatedHealth != contracts.EvaluatedHealthHealthy {
		return false
	}
	freshOK, _, _ := contracts.ValidateHealthEvidenceFreshness(h, now, maxAge)
	if !freshOK {
		return false
	}
	if h.EvidenceFreshness == nil {
		return false
	}
	checkSrc := func(sf contracts.SourceFreshness) bool {
		if !sf.Available || !sf.Fresh || sf.ObservedAt == nil || sf.ObservedAt.IsZero() {
			return false
		}
		if now.Sub(*sf.ObservedAt) > maxAge {
			return false
		}
		return true
	}
	for src := range faultSources {
		switch src {
		case "cups":
			if !checkSrc(h.EvidenceFreshness.CUPS) {
				return false
			}
		case "native_ipp", "ipp":
			if !checkSrc(h.EvidenceFreshness.NativeIPP) {
				return false
			}
		case "snmp":
			if !checkSrc(h.EvidenceFreshness.SNMP) {
				return false
			}
		}
	}
	return true
}

func (c *Client) pauseStartupOrderInconsistency(ctx context.Context, orderID string, incList []state.StoredJob, reason string) {
	for _, incomplete := range incList {
		c.finalizeAndEmitOrderFailure(ctx, orderID, incomplete.Command.JobID, contracts.JobProgressEvent{
			JobID:         incomplete.Command.JobID,
			KioskID:       firstNonEmptyString(incomplete.Command.KioskID, c.cfg.KioskID),
			Status:        contracts.JobStatusPausedError,
			PagesPrinted:  incomplete.PagesPrinted,
			SheetsPrinted: incomplete.SheetsPrinted,
			ErrorReason:   reason,
			Timestamp:     time.Now().UTC(),
		}, false)
	}
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// CheckPrinterHealth runs a serialized canonical telemetry acquisition cycle, applies the 3-cycle
// recovery barrier before committing the authoritative shared snapshot, publishes over WebSocket
// in acquisition sequence order, reconciles interrupted jobs on healthy/recovery transitions,
// and synchronizes the local screen display when idle.
func (c *Client) CheckPrinterHealth(ctx context.Context) contracts.PrinterHealth {
	if ctx == nil {
		ctx = context.Background()
	}

	effectiveH, wasFault, isNowFault := c.refreshPrinterHealthTelemetry(ctx)

	if wasFault && !isNowFault {
		log.Printf("[SPOOLER] Printer hardware reliably recovered from fault (consecutive healthy cycles: %d). Refreshing screen cleanly.", c.requiredRecoveryCount)
		c.idleMu.Lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
			c.idleTimer = nil
		}
		c.idleMu.Unlock()

		// Reconcile any interrupted CUPS job that was paused during the fault without auto-reprinting.
		c.reconcileInterruptedCUPSJob(ctx)
	} else if !c.isHardwareFaultActive() && effectiveH.EvaluatedHealth == contracts.EvaluatedHealthHealthy && !c.reconciling.Load() {
		// Bounded repeatable reconciliation independent of requiring another hardware-fault transition.
		c.reconcileInterruptedCUPSJob(ctx)
	}

	c.syncCustomerServiceState(ctx)
	if !c.isHardwareFaultActive() && effectiveH.EvaluatedHealth == contracts.EvaluatedHealthHealthy {
		c.drainDeferredJobsIfSafe(ctx)
	}

	return effectiveH
}

// refreshPrinterHealthTelemetry acquires fresh canonical telemetry and commits the authoritative
// snapshot without triggering reconciliation side effects (safe to call from reconciliation).
func (c *Client) refreshPrinterHealthTelemetry(ctx context.Context) (contracts.PrinterHealth, bool, bool) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.acqMu.Lock()
	rawH, rec, rawTrayStatus, seq, acqTime := c.acquireTelemetryCycleLocked(ctx)

	isRawFault, reason, custMsg := isBlockingHardwareFault(rawH)
	evalNow := time.Now().UTC()
	freshOK, _, _ := contracts.ValidateHealthEvidenceFreshness(rawH, evalNow, DefaultTelemetryMaxAge)
	evaluatedHealthy := !isRawFault && rawH.EvaluatedHealth == contracts.EvaluatedHealthHealthy && freshOK

	c.faultMu.Lock()
	wasFault := c.hardwareFault
	alreadyProcessedSeq := (seq != 0 && seq == c.lastRecoverySeq)
	if !alreadyProcessedSeq {
		c.lastRecoverySeq = seq
	}

	if isRawFault {
		c.consecutiveHealthy = 0
		c.hardwareFault = true
		c.faultReason = reason
		c.faultCustomerMsg = custMsg
		if c.faultSources == nil {
			c.faultSources = make(map[string]bool)
		}
		for src := range collectFaultSources(rawH) {
			c.faultSources[src] = true
		}
	} else if !alreadyProcessedSeq && c.hardwareFault {
		// Recovery confirmation requires N distinct consecutive healthy acquisition cycles with fresh
		// clearing evidence from every source that reported the fault.
		if !evaluatedHealthy || !faultSourcesCleared(c.faultSources, rawH, evalNow, DefaultTelemetryMaxAge) {
			c.consecutiveHealthy = 0
		} else {
			c.consecutiveHealthy++
			if c.consecutiveHealthy >= c.requiredRecoveryCount {
				c.hardwareFault = false
				c.faultReason = ""
				c.faultCustomerMsg = ""
				c.faultSources = make(map[string]bool)
			}
		}
	}
	isNowFault := c.hardwareFault
	effReason := c.faultReason
	streak := c.consecutiveHealthy
	reqCount := c.requiredRecoveryCount
	c.faultMu.Unlock()

	effectiveH := clonePrinterHealth(rawH)
	if isNowFault {
		effectiveH.Online = false
		effectiveH.EvaluatedHealth = contracts.EvaluatedHealthFault
		if effectiveH.State != "OFFLINE" {
			effectiveH.State = "ERROR"
		}
		if !isRawFault {
			// Healthy observation during unconfirmed recovery streak must NOT publish HEALTHY.
			recoveryMsg := fmt.Sprintf("recovering from printer fault (%d/%d verified healthy cycles): %s", streak, reqCount, firstNonEmptyString(effReason, "awaiting confirmation"))
			effectiveH.ErrorMessage = recoveryMsg
			effectiveH.BlockingReasons = []contracts.HealthReason{{
				Code:    "recovery_barrier_active",
				Source:  "spooler",
				Message: recoveryMsg,
			}}
		}
	}

	c.calibrationMu.Lock()
	// Re-read durable paper supply after hardware acquisition so a calibration completed
	// while telemetry was in-flight is never overwritten by an obsolete count.
	latestSheets, latestCalibrated, supplyErr := c.cfg.StateStore.GetPaperSupply(ctx)
	if supplyErr != nil {
		latestSheets, _ = c.cfg.StateStore.GetEstimatedSheets(ctx)
		latestCalibrated = false
	}
	trayCap := c.configuredTrayCapacity()
	effectiveH.ConfiguredTrayCapacity = trayCap
	effectiveH.InventoryCalibrated = latestCalibrated
	effectiveH.InventorySeq = c.calibrationSeq
	effectiveH.EstimatedSheets = contracts.EffectiveInventorySheets(
		latestSheets,
		trayCap,
		effectiveH.InputTrayCapacity,
		effectiveH.InputTrayCapacityKnown,
	)
	if !latestCalibrated && effectiveH.PaperTrayStatus != "EMPTY" {
		effectiveH.PaperTrayStatus = "UNKNOWN"
	} else if latestCalibrated &&
		!effectiveH.PaperJam &&
		!effectiveH.CoverOpen &&
		!effectiveH.OutputBinFull &&
		effectiveH.PaperTrayStatus == "UNKNOWN" &&
		rawTrayStatus == "OK" {
		effectiveH.PaperTrayStatus = "OK"
	}

	c.snapshotMu.Lock()
	c.latestSnap = telemetrySnapshot{
		HasSample:     true,
		Seq:           seq,
		AcquiredAt:    acqTime,
		Health:        clonePrinterHealth(effectiveH),
		Reconciled:    rec,
		rawTrayStatus: rawTrayStatus,
	}
	c.snapshotMu.Unlock()

	c.publishHealthSnapshot(ctx, effectiveH)
	c.calibrationMu.Unlock()
	c.acqMu.Unlock()

	return effectiveH, wasFault, isNowFault
}

func (c *Client) acquireTelemetryCycleLocked(ctx context.Context) (contracts.PrinterHealth, printer.ReconciledHealthResult, string, uint64, time.Time) {
	c.acqSeq++
	seq := c.acqSeq
	now := time.Now().UTC()

	sheets, calibrated, supplyErr := c.cfg.StateStore.GetPaperSupply(ctx)
	if supplyErr != nil {
		sheets, _ = c.cfg.StateStore.GetEstimatedSheets(ctx)
		calibrated = false
	}
	execSafe, execReason := c.computeExecutionSafety(ctx, "")

	var h contracts.PrinterHealth
	var rec printer.ReconciledHealthResult
	var rawTrayStatus string

	switch {
	case c.healthPoller != nil:
		// Explicit mock simulation or white-box test poller
		h = c.healthPoller(ctx, sheets)
		rawTrayStatus = h.PaperTrayStatus
		h.NativeIPPAdmission = h.EffectiveNativeIPPAdmission()
		h.EstimatedSheets = sheets
		h.InventoryCalibrated = calibrated
		h.ExecutionSafe = execSafe
		h.ExecutionSafetyReason = execReason
		if h.UpdatedAt.IsZero() {
			h.UpdatedAt = now
		}
		if !h.TonerLevelKnown && h.TonerLevelPercent > 0 {
			h.TonerLevelKnown = true
		} else if !h.TonerLevelKnown && h.TonerLevelPercent == 0 {
			h.TonerLevelPercent = -1
		}
		if !h.DrumLifeKnown && h.DrumLifePercent > 0 {
			h.DrumLifeKnown = true
		} else if !h.DrumLifeKnown && h.DrumLifePercent == 0 {
			h.DrumLifePercent = -1
		}
		if !calibrated && h.PaperTrayStatus != "EMPTY" {
			h.PaperTrayStatus = "UNKNOWN"
		}
		if h.EvaluatedHealth == "" {
			if isFault, _, _ := isBlockingHardwareFault(h); isFault {
				h.EvaluatedHealth = contracts.EvaluatedHealthFault
			} else if !h.Online || h.State == "" || h.State == "UNKNOWN" {
				h.EvaluatedHealth = contracts.EvaluatedHealthUnverified
			} else {
				h.EvaluatedHealth = contracts.EvaluatedHealthHealthy
			}
		}
		if h.EvaluatedHealth == contracts.EvaluatedHealthHealthy && h.EvidenceFreshness == nil {
			h.EvidenceFreshness = contracts.BuildUniformEvidenceFreshness(h.UpdatedAt, DefaultTelemetryMaxAge)
		}
		if h.EvaluatedHealth == contracts.EvaluatedHealthHealthy {
			if ok, _, freshReason := contracts.ValidateHealthEvidenceFreshness(h, time.Now().UTC(), DefaultTelemetryMaxAge); !ok {
				h.EvaluatedHealth = contracts.EvaluatedHealthUnverified
				h.Online = false
				h.ErrorMessage = firstNonEmptyString(h.ErrorMessage, freshReason)
			}
		}

	case strings.TrimSpace(c.cfg.CUPSQueue) == "" && strings.TrimSpace(c.cfg.SNMPTarget) == "" &&
		c.cfg.MockCUPSHealth == nil && c.cfg.MockCUPSHealthDiag == nil && c.cfg.MockSNMPHealth == nil && c.cfg.MockIPPState == nil:
		// Missing production printer configuration must NEVER synthesize healthy/ready state
		h = contracts.PrinterHealth{
			Online:                false,
			State:                 "UNKNOWN",
			EvaluatedHealth:       contracts.EvaluatedHealthUnverified,
			PaperTrayStatus:       "UNKNOWN",
			EstimatedSheets:        sheets,
			ConfiguredTrayCapacity: c.configuredTrayCapacity(),
			InventoryCalibrated:    calibrated,
			ExecutionSafe:         execSafe,
			ExecutionSafetyReason: execReason,
			TonerLevelPercent:     -1,
			DrumLifePercent:       -1,
			InputTrayLevel:        -1,
			InputTrayCapacity:     -1,
			ErrorMessage:          "printer telemetry configuration missing: no CUPS queue or SNMP target configured",
			UpdatedAt:             now,
		}

	default:
		acqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()

		cupsQ := strings.TrimSpace(c.cfg.CUPSQueue)
		var (
			cupsHealth  contracts.PrinterHealth
			cupsErr     error
			cupsQueried bool
			nativeIPP   printer.IPPPrinterStateResult
			snmpObs     printer.SNMPObservation
			legacySNMP  *contracts.PrinterHealth
		)

		var wg sync.WaitGroup
		if cupsQ != "" || c.cfg.MockCUPSHealthDiag != nil || c.cfg.MockCUPSHealth != nil {
			cupsQueried = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c.cfg.MockCUPSHealthDiag != nil {
					cupsHealth, cupsErr = c.cfg.MockCUPSHealthDiag(acqCtx, sheets)
				} else if c.cfg.MockCUPSHealth != nil {
					cupsHealth = c.cfg.MockCUPSHealth(acqCtx, sheets)
				} else {
					cupsHealth, cupsErr = printer.QueryCUPSHealthDiagnostic(acqCtx, c.cfg.CUPSRunner, cupsQ, sheets)
				}
			}()
		}

		if cupsQ != "" || c.cfg.MockIPPState != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c.cfg.MockIPPState != nil {
					res, err := c.cfg.MockIPPState(acqCtx, cupsQ)
					if err != nil && res.Error == "" {
						res.Error = err.Error()
					}
					nativeIPP = res
					return
				}

				c.nativeIPPMu.Lock()
				cachedURI := c.resolvedIPPURI
				c.nativeIPPMu.Unlock()

				targetURI := cachedURI
				usedCached := targetURI != ""
				if targetURI == "" {
					var rErr error
					targetURI, rErr = printer.ResolveNativeIPPEndpoint(acqCtx, c.cfg.CUPSRunner, cupsQ, c.cfg.NativeIPPURI)
					if rErr != nil {
						nativeIPP = printer.IPPPrinterStateResult{
							Source:        printer.CapabilitySourceNativeIPP,
							State:         printer.IPPPrinterStateUnknown,
							AcceptingJobs: printer.AcceptingJobsUnknown,
							ObservedAt:    time.Now().UTC(),
							Error:         rErr.Error(),
						}
						return
					}
					c.nativeIPPMu.Lock()
					c.resolvedIPPURI = targetURI
					c.nativeIPPMu.Unlock()
				}

				res, _ := printer.QueryNativeIPPPrinterState(acqCtx, c.cfg.CUPSRunner, targetURI)
				if !res.EndpointReachable && usedCached && strings.TrimSpace(c.cfg.NativeIPPURI) == "" {
					// Cached URI failed; rediscover once from CUPS before reporting failure.
					newURI, rErr := printer.ResolveNativeIPPEndpoint(acqCtx, c.cfg.CUPSRunner, cupsQ, "")
					c.nativeIPPMu.Lock()
					if rErr == nil && newURI != "" {
						c.resolvedIPPURI = newURI
					} else {
						c.resolvedIPPURI = ""
					}
					c.nativeIPPMu.Unlock()
					if rErr == nil && newURI != "" && newURI != targetURI {
						res, _ = printer.QueryNativeIPPPrinterState(acqCtx, c.cfg.CUPSRunner, newURI)
					}
				}
				nativeIPP = res
			}()
		}

		if c.cfg.MockSNMPHealth != nil || c.snmpMonitor != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c.cfg.MockSNMPHealth != nil {
					s := c.cfg.MockSNMPHealth(acqCtx, sheets)
					legacySNMP = &s
					return
				}
				snmpObs = c.snmpMonitor.Poll(acqCtx, sheets)
			}()
		}

		wg.Wait()

		cupsValid := false
		var cupsErrStr string
		if cupsQueried {
			cupsValid = cupsErr == nil && cupsHealth.State != "" && cupsHealth.State != "UNKNOWN"
			if cupsErr != nil {
				cupsErrStr = cupsErr.Error()
			}
		}

		var snmpValidPtr *bool
		if c.cfg.MockSNMPHealth != nil && legacySNMP != nil {
			v := legacySNMP.Online && legacySNMP.State != "" && legacySNMP.State != "UNKNOWN"
			snmpValidPtr = &v
		} else if c.snmpMonitor != nil {
			v := snmpObs.Available
			snmpValidPtr = &v
		}

		rec = printer.EvaluateCanonicalPrinterHealth(printer.CanonicalHealthInput{
			CUPS:             cupsHealth,
			CUPSValid:        &cupsValid,
			CUPSQueried:      cupsQueried,
			CUPSError:        cupsErrStr,
			IPP:              nativeIPP,
			RequireNativeIPP: (c.cfg.MockIPPState == nil),
			RequireCUPS:      true,
			RequireIPP:       true,
			SNMP:             snmpObs,
			LegacySNMP:       legacySNMP,
			SNMPValid:        snmpValidPtr,
			SNMPQueried:      (c.snmpMonitor != nil || c.cfg.MockSNMPHealth != nil),
			RequireSNMP:      true,
			EvalTime:               time.Now().UTC(),
			MaxSourceAge:           DefaultTelemetryMaxAge,
			ConfiguredTrayCapacity: c.configuredTrayCapacity(),
		})
		h = rec.ToContract(sheets, calibrated, execSafe, execReason)
		h.ConfiguredTrayCapacity = c.configuredTrayCapacity()
		rawTrayStatus = rec.PaperTrayStatus
	}

	if h.ConfiguredTrayCapacity <= 0 {
		h.ConfiguredTrayCapacity = c.configuredTrayCapacity()
	}
	return h, rec, rawTrayStatus, seq, time.Now().UTC()
}

func (c *Client) capabilityPolicy() printer.PrinterCapabilityPolicy {
	if c != nil && c.cfg.CapabilityPolicy != nil {
		return *c.cfg.CapabilityPolicy
	}
	return printer.DefaultDemoCapabilityPolicy()
}

func (c *Client) isSimulationMode() bool {
	if c == nil {
		return false
	}
	return c.cfg.MockCapabilities != nil ||
		c.cfg.MockHealth != nil ||
		c.cfg.MockCUPSPrint != nil ||
		c.cfg.MockCUPSHealth != nil ||
		c.cfg.MockIPPState != nil ||
		c.cfg.MockSNMPHealth != nil ||
		c.healthPoller != nil
}

func (c *Client) validateJobCapabilities(ctx context.Context, cmd contracts.PrintJobCommand) error {
	policy := c.capabilityPolicy()
	var hwPtr *printer.PrinterCapabilities
	if c != nil && c.cfg.MockCapabilities != nil {
		hwCopy := *c.cfg.MockCapabilities
		hwPtr = &hwCopy
	} else if c != nil && c.isSimulationMode() {
		synth := printer.SyntheticDemoCapabilities()
		if policy.AllowColor {
			synth.ColorSupport = printer.CapabilitySupported
		}
		if policy.AllowDuplex {
			synth.DuplexLongEdgeSupport = printer.CapabilitySupported
			synth.DuplexShortEdgeSupport = printer.CapabilitySupported
			synth.SidesSupported = append(synth.SidesSupported, "two-sided-long-edge", "two-sided-short-edge")
		}
		hwPtr = &synth
	} else if c != nil {
		hwCaps := c.Capabilities()
		if hwCaps.Source == "" && strings.TrimSpace(c.cfg.CUPSQueue) != "" {
			hwCaps = c.DiscoverStartupCapabilities(ctx)
		}
		if hwCaps.HasCapabilityAttributes() {
			hwPtr = &hwCaps
		}
	}
	return printer.ValidateJobAgainstPolicyAndHardware(cmd, policy, hwPtr)
}

func (c *Client) configuredTrayCapacity() int {
	if c != nil && c.cfg.ConfiguredTrayCapacity > 0 {
		return c.cfg.ConfiguredTrayCapacity
	}
	if c != nil && c.cfg.StateStore != nil {
		if cap := c.cfg.StateStore.TrayCapacity(); cap > 0 {
			return cap
		}
	}
	return state.DefaultTrayCapacity
}

func (c *Client) handleRefillMessage(ctx context.Context, conn *websocket.Conn, msg wireMessage) {
	sendAck := func(ok bool, sheetsVal int, invSeq uint64, errMsg string) {
		if conn == nil || strings.TrimSpace(msg.RequestID) == "" {
			return
		}
		var sheetsPtr *int
		if ok {
			s := sheetsVal
			sheetsPtr = &s
		}
		_ = c.sendWire(ctx, conn, wireMessage{
			Type:         "refill_ack",
			RequestID:    msg.RequestID,
			OK:           ok,
			Calibrated:   ok,
			Sheets:       sheetsPtr,
			InventorySeq: invSeq,
			Error:        errMsg,
		})
	}

	if msg.Sheets == nil {
		errMsg := "missing sheets value in refill command"
		log.Printf("[SPOOLER] Rejecting refill message: %s", errMsg)
		sendAck(false, 0, 0, errMsg)
		return
	}
	sheets := *msg.Sheets
	cap := c.configuredTrayCapacity()
	c.snapshotMu.RLock()
	if c.latestSnap.HasSample {
		cap = contracts.EffectiveTrayCapacity(
			cap,
			c.latestSnap.Health.InputTrayCapacity,
			c.latestSnap.Health.InputTrayCapacityKnown,
		)
	}
	c.snapshotMu.RUnlock()
	if sheets < 0 || sheets > cap {
		errMsg := fmt.Sprintf("refill sheets %d outside configured tray capacity [0, %d]", sheets, cap)
		log.Printf("[SPOOLER] Rejecting refill message: %s", errMsg)
		sendAck(false, 0, 0, errMsg)
		return
	}

	c.calibrationMu.Lock()
	defer c.calibrationMu.Unlock()

	c.activeMu.Lock()
	activeCount := len(c.activeJobs)
	monitoredCount := len(c.monitoredJobs)
	c.activeMu.Unlock()
	if activeCount > 0 || monitoredCount > 0 {
		errMsg := "cannot calibrate paper tray while active print job is in progress"
		log.Printf("[SPOOLER] Rejecting refill message: %s", errMsg)
		sendAck(false, 0, 0, errMsg)
		return
	}

	if err := c.cfg.StateStore.CalibratePaperTray(ctx, sheets); err != nil {
		log.Printf("[SPOOLER] Failed to calibrate paper tray on refill message: %v", err)
		sendAck(false, 0, 0, err.Error())
		return
	}

	log.Printf("[SPOOLER] Successfully refilled paper tray inventory to %d sheets (capacity %d)", sheets, cap)
	h := c.updateSnapshotInventory(ctx, sheets, true)
	sendAck(true, sheets, c.calibrationSeq, "")
	if conn != nil {
		_ = c.sendWire(ctx, conn, wireMessage{Type: "health", Health: &h})
	}
	go c.syncCustomerServiceState(context.Background())
}

// CurrentHealthSnapshot reads the shared canonical health snapshot without triggering
// hardware acquisition or refreshing observation timestamps. If no sample exists or the
// underlying evidence expires beyond 6s, it invalidates readiness cleanly to UNVERIFIED.
func (c *Client) CurrentHealthSnapshot(ctx context.Context) contracts.PrinterHealth {
	c.snapshotMu.RLock()
	snap := c.latestSnap
	c.snapshotMu.RUnlock()

	if ctx == nil {
		ctx = context.Background()
	}

	execSafe, execReason := c.computeExecutionSafety(ctx, "")
	sheets, calibrated, err := c.cfg.StateStore.GetPaperSupply(ctx)
	if err != nil {
		sheets, _ = c.cfg.StateStore.GetEstimatedSheets(ctx)
		calibrated = false
	}

	now := time.Now().UTC()
	if !snap.HasSample || snap.Health.UpdatedAt.IsZero() {
		return contracts.PrinterHealth{
			Online:                false,
			State:                 "UNKNOWN",
			EvaluatedHealth:       contracts.EvaluatedHealthUnverified,
			PaperTrayStatus:       "UNKNOWN",
			EstimatedSheets:        sheets,
			ConfiguredTrayCapacity: c.configuredTrayCapacity(),
			InventoryCalibrated:    calibrated,
			ExecutionSafe:         execSafe,
			ExecutionSafetyReason: execReason,
			TonerLevelPercent:     -1,
			DrumLifePercent:       -1,
			InputTrayLevel:        -1,
			InputTrayCapacity:     -1,
			ErrorMessage:          "printer telemetry not yet acquired",
		}
	}

	h := clonePrinterHealth(snap.Health)
	trayCap := c.configuredTrayCapacity()
	h.ConfiguredTrayCapacity = trayCap
	h.EstimatedSheets = contracts.EffectiveInventorySheets(
		sheets,
		trayCap,
		h.InputTrayCapacity,
		h.InputTrayCapacityKnown,
	)
	h.InventoryCalibrated = calibrated
	h.ExecutionSafe = execSafe
	h.ExecutionSafetyReason = execReason

	if ok, _, freshReason := contracts.ValidateHealthEvidenceFreshness(h, now, DefaultTelemetryMaxAge); !ok {
		// Preserve original timestamps so reading expired telemetry never extends age.
		h.Online = false
		h.NativeIPPAdmission = contracts.AcceptingJobsUnknown
		if h.EvaluatedHealth != contracts.EvaluatedHealthFault {
			h.EvaluatedHealth = contracts.EvaluatedHealthUnverified
		}
		if h.EvidenceFreshness != nil {
			ef := *h.EvidenceFreshness
			ef.CUPS.Fresh = false
			ef.NativeIPP.Fresh = false
			ef.SNMP.Fresh = false
			h.EvidenceFreshness = &ef
		}
		if h.ErrorMessage == "" {
			h.ErrorMessage = freshReason
		}
	}
	return h
}

func (c *Client) ensureOrCurrentHealthSnapshot(ctx context.Context) contracts.PrinterHealth {
	c.snapshotMu.RLock()
	has := c.latestSnap.HasSample
	c.snapshotMu.RUnlock()
	if !has {
		return c.CheckPrinterHealth(ctx)
	}
	return c.CurrentHealthSnapshot(ctx)
}

func (c *Client) updateSnapshotInventory(ctx context.Context, sheets int, calibrated bool) contracts.PrinterHealth {
	c.calibrationSeq++
	calSeq := c.calibrationSeq
	execSafe, execReason := c.computeExecutionSafety(ctx, "")
	sheetsVal, calibVal, err := c.cfg.StateStore.GetPaperSupply(ctx)
	if err == nil {
		sheets = sheetsVal
		calibrated = calibVal
	}
	trayCap := c.configuredTrayCapacity()
	c.snapshotMu.Lock()
	if c.latestSnap.HasSample {
		c.latestSnap.Health.ConfiguredTrayCapacity = trayCap
		c.latestSnap.Health.EstimatedSheets = contracts.EffectiveInventorySheets(
			sheets,
			trayCap,
			c.latestSnap.Health.InputTrayCapacity,
			c.latestSnap.Health.InputTrayCapacityKnown,
		)
		c.latestSnap.Health.InventoryCalibrated = calibrated
		c.latestSnap.Health.InventorySeq = calSeq
		c.latestSnap.Health.ExecutionSafe = execSafe
		c.latestSnap.Health.ExecutionSafetyReason = execReason
		// Publish inventory calibration without re-polling hardware or overwriting active physical faults.
		if calibrated &&
			!c.latestSnap.Health.PaperJam &&
			!c.latestSnap.Health.CoverOpen &&
			!c.latestSnap.Health.OutputBinFull &&
			c.latestSnap.Health.PaperTrayStatus == "UNKNOWN" &&
			c.latestSnap.rawTrayStatus == "OK" {
			c.latestSnap.Health.PaperTrayStatus = "OK"
		}
	}
	c.snapshotMu.Unlock()
	return c.CurrentHealthSnapshot(ctx)
}

func (c *Client) setMonitoredJob(jobID string, active bool) {
	if strings.TrimSpace(jobID) == "" {
		return
	}
	c.activeMu.Lock()
	if c.monitoredJobs == nil {
		c.monitoredJobs = make(map[string]bool)
	}
	if active {
		c.monitoredJobs[jobID] = true
	} else {
		delete(c.monitoredJobs, jobID)
	}
	c.activeMu.Unlock()
}

func (c *Client) isJobActive(jobID string) bool {
	if strings.TrimSpace(jobID) == "" {
		return false
	}
	c.activeMu.Lock()
	active := c.activeJobs[jobID]
	c.activeMu.Unlock()
	return active
}

func (c *Client) isLiveMonitoredJob(jobID string) bool {
	if strings.TrimSpace(jobID) == "" {
		return false
	}
	c.activeMu.Lock()
	active := c.monitoredJobs[jobID]
	c.activeMu.Unlock()
	return active
}

func (c *Client) isIdleTimerActive() bool {
	c.idleMu.Lock()
	active := c.idleTimer != nil
	c.idleMu.Unlock()
	return active
}

func (c *Client) effectiveCustomerServiceReadiness(ctx context.Context) (bool, string, contracts.PrinterHealth) {
	if ctx == nil {
		ctx = context.Background()
	}
	snap := c.CurrentHealthSnapshot(ctx)

	c.connMu.Lock()
	mustBeConnected := c.runActive
	connected := c.conn != nil
	c.connMu.Unlock()
	if mustBeConnected && !connected {
		return false, "cloud connection unavailable", snap
	}

	if c.isHardwareFaultActive() {
		return false, firstNonEmptyString(c.getFaultReason(), "active hardware fault"), snap
	}
	if ok, reason := contracts.IsHealthAvailable(snap, time.Now().UTC(), DefaultTelemetryMaxAge); !ok {
		return false, reason, snap
	}
	return true, "", snap
}

func (c *Client) isServiceReadyForIdle(ctx context.Context) (bool, string, contracts.PrinterHealth) {
	return c.effectiveCustomerServiceReadiness(ctx)
}

func (c *Client) syncCustomerServiceState(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.activeMu.Lock()
	hasActiveJobs := len(c.activeJobs) > 0
	c.activeMu.Unlock()

	ready, reason, snap := c.effectiveCustomerServiceReadiness(ctx)
	if c.pinController != nil {
		pinReady := ready && !hasActiveJobs
		pinReason := reason
		if hasActiveJobs {
			pinReason = "kiosk is currently printing"
		}
		c.pinController.SetAvailability(pinReady, pinReason)
	}

	if ctx.Err() != nil {
		return
	}

	if hasActiveJobs {
		return
	}

	pinActive := c.pinController != nil && c.pinController.HasActiveTransaction()
	if pinActive {
		return
	}

	c.currentModeMu.Lock()
	currentMode := c.currentMode
	lastErrMsg := c.lastErrorMsg
	lastErrDetail := c.lastErrorDetail
	c.currentModeMu.Unlock()

	if ready {
		if currentMode != ui_ipc.ModeIdle && !c.isIdleTimerActive() {
			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:      ui_ipc.ModeIdle,
				QRContent: c.cfg.IdleQRContent,
				Message:   "Ready to print",
			})
		}
		c.drainDeferredJobsIfSafe(ctx)
		return
	}

	msg := "Temporarily unavailable"
	if c.isHardwareFaultActive() || snap.EvaluatedHealth == contracts.EvaluatedHealthFault {
		c.faultMu.Lock()
		cust := c.faultCustomerMsg
		c.faultMu.Unlock()
		msg = firstNonEmptyString(cust, "A printer hardware fault requires attention.")
	} else if !snap.ExecutionSafe {
		msg = "Manual check required before printing"
	}
	detail := firstNonEmptyString(snap.ExecutionSafetyReason, snap.ErrorMessage, reason, "Please try again shortly.")
	if currentMode == ui_ipc.ModeError && lastErrMsg == msg && lastErrDetail == detail {
		return
	}
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: msg,
		Detail:  detail,
	})
}

func (c *Client) syncIdleOrBlockedScreen(ctx context.Context) bool {
	ready, reason, snap := c.isServiceReadyForIdle(ctx)
	if ready {
		_ = c.renderScreen(ui_ipc.ScreenState{
			Mode:      ui_ipc.ModeIdle,
			QRContent: c.cfg.IdleQRContent,
			Message:   "Ready to print",
		})
		return true
	}
	msg := "Printer verification required"
	if c.isHardwareFaultActive() || snap.EvaluatedHealth == contracts.EvaluatedHealthFault {
		c.faultMu.Lock()
		cust := c.faultCustomerMsg
		c.faultMu.Unlock()
		msg = firstNonEmptyString(cust, "A printer hardware fault requires attention.")
	} else if !snap.ExecutionSafe {
		msg = "Manual check required before printing"
	}
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: msg,
		Detail:  firstNonEmptyString(snap.ExecutionSafetyReason, snap.ErrorMessage, reason),
	})
	return false
}

func (c *Client) computeExecutionSafety(ctx context.Context, excludeJobID string) (bool, string) {
	if c.isPersistenceBlocked() {
		return false, c.persistenceBlockReason()
	}
	if c.cfg.StateStore == nil {
		return false, "state store unavailable"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pendingOutcomes, err := c.cfg.StateStore.GetPendingOutcomeSyncJobs(ctx); err != nil {
		return false, fmt.Sprintf("failed reading pending outcome sync state: %v", err)
	} else {
		for _, po := range pendingOutcomes {
			if po.Status == contracts.JobStatusPausedError || po.Status == contracts.JobStatusFailedRefundable {
				return false, fmt.Sprintf("unsynchronized failure outcome for %s (%s)", po.Command.JobID, po.Status)
			}
		}
	}
	incomplete, err := c.cfg.StateStore.GetIncompleteJobs(ctx)
	if err != nil {
		return false, fmt.Sprintf("failed reading durable job state: %v", err)
	}
	for _, j := range incomplete {
		if excludeJobID != "" && j.Command.JobID == excludeJobID {
			continue
		}
		if j.Status == contracts.JobStatusPausedError {
			return false, fmt.Sprintf("unresolved paused job %s requires reconciliation", j.Command.JobID)
		}
		if j.SubmittedToPrinter || j.CUPSJobID != "" || j.SheetsPrinted > 0 {
			// Healthy live-monitored print owned by this daemon preserves order-prep ExecutionSafe=true
			// when evaluating kiosk readiness (excludeJobID == ""); physical submission serialization still
			// blocks other jobs via excludeJobID != "", and queued-only activeJobs entries never qualify.
			if excludeJobID == "" && !c.isHardwareFaultActive() && c.isLiveMonitoredJob(j.Command.JobID) {
				continue
			}
			return false, fmt.Sprintf("unresolved physical job %s (%s) has uncertain output", j.Command.JobID, j.Status)
		}
	}
	return true, ""
}

func (c *Client) publishHealthSnapshot(ctx context.Context, h contracts.PrinterHealth) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return
	}
	_ = c.sendWire(ctx, conn, wireMessage{Type: "health", Health: &h})
}

func clonePrinterHealth(h contracts.PrinterHealth) contracts.PrinterHealth {
	cp := h
	if len(h.BlockingReasons) > 0 {
		cp.BlockingReasons = append([]contracts.HealthReason(nil), h.BlockingReasons...)
	}
	if len(h.Warnings) > 0 {
		cp.Warnings = append([]contracts.HealthReason(nil), h.Warnings...)
	}
	if h.EvidenceFreshness != nil {
		ef := *h.EvidenceFreshness
		cp.EvidenceFreshness = &ef
	}
	return cp
}

func (c *Client) healthMonitorLoop(ctx context.Context) {
	interval := c.cfg.HealthPollInterval
	if interval <= 0 {
		interval = DefaultHealthPollInterval
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
		c.idleMu.Lock()
		c.idleTimer = nil
		c.idleMu.Unlock()

		c.activeMu.Lock()
		activeCount := len(c.activeJobs)
		c.activeMu.Unlock()

		if activeCount == 0 {
			if c.pinController != nil && c.pinController.HasActiveTransaction() {
				return
			}
			if !c.syncIdleOrBlockedScreen(context.Background()) {
				log.Printf("[SPOOLER] Suppressing idle reset: effective readiness or execution-safety barrier active")
				return
			}
			log.Printf("[SPOOLER] Resetting screen to idle state for next user")
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
	if !c.reconciling.CompareAndSwap(false, true) {
		return
	}
	defer c.reconciling.Store(false)

	c.retryPendingFinalizations(ctx)

	incList, err := c.cfg.StateStore.GetIncompleteJobs(ctx)
	if err != nil {
		c.setPersistenceReadBarrier(fmt.Errorf("reconciliation state read error: %w", err))
		return
	}
	c.setPersistenceReadBarrier(nil)
	if len(incList) == 0 {
		return
	}

	for _, inc := range incList {
		if inc.Status != contracts.JobStatusPausedError {
			continue
		}
		if c.isJobActive(inc.Command.JobID) || c.isLiveMonitoredJob(inc.Command.JobID) {
			continue
		}
		if inc.CUPSJobID == "" {
			if !inc.SubmittedToPrinter && inc.SheetsPrinted == 0 && inc.PagesPrinted == 0 {
				orderID := strings.TrimSpace(inc.Command.OrderID)
				if orderID == "" {
					orderID = inc.Command.JobID
				}
				sibs, sibErr := c.cfg.StateStore.GetJobsByOrderID(ctx, orderID)
				if sibErr == nil && len(sibs) > 0 {
					hasResolved := false
					hasActivePaused := false
					for _, s := range sibs {
						if s.Command.JobID == inc.Command.JobID {
							continue
						}
						if s.Status == contracts.JobStatusCompleted || s.Status == contracts.JobStatusFailedRefundable {
							hasResolved = true
						}
						if s.Status == contracts.JobStatusPausedError && (s.SubmittedToPrinter || strings.TrimSpace(s.CUPSJobID) != "" || s.SheetsPrinted > 0 || s.PagesPrinted > 0) {
							hasActivePaused = true
						}
					}
					if hasResolved && !hasActivePaused {
						c.finalizeStoppedUnsubmittedSiblings(ctx, orderID, "", "Order stopped after earlier interruption")
					}
				}
			}
			continue
		}
		incCopy := inc
		c.reconcileSubmittedOrInterruptedJob(ctx, &incCopy, inc.ErrorReason, true)
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
	lastErrMsg := c.lastErrorMsg
	lastErrDetail := c.lastErrorDetail
	c.currentModeMu.Unlock()

	isFault := c.isHardwareFaultActive()

	// Prevent printing/downloading progress updates from overwriting an active hardware fault screen or existing error
	if (state.Mode == ui_ipc.ModePrinting || state.Mode == ui_ipc.ModeDownloading) && (isFault || currentMode == ui_ipc.ModeError) {
		log.Printf("[SPOOLER] Suppressing %s render: hardware fault active or screen already in ModeError (%s)", state.Mode, c.getFaultReason())
		return nil
	}

	// Prevent redundant ModeError renders when an active hardware fault or identical unavailable screen is already shown
	if state.Mode == ui_ipc.ModeError && currentMode == ui_ipc.ModeError && (isFault || (state.Message == lastErrMsg && state.Detail == lastErrDetail)) {
		if isFault {
			log.Printf("[SPOOLER] Suppressing duplicate ModeError render: screen already in ModeError (%s)", c.getFaultReason())
		}
		return nil
	}

	// Prevent idle reset or idle frame from violating active fault, readiness, or execution safety barriers
	if state.Mode == ui_ipc.ModeIdle {
		if isFault {
			log.Printf("[SPOOLER] Suppressing ModeIdle render: hardware fault active (%s)", c.getFaultReason())
			return nil
		}
		if ready, blockReason, _ := c.isServiceReadyForIdle(context.Background()); !ready {
			log.Printf("[SPOOLER] Suppressing ModeIdle render: %s", blockReason)
			return nil
		}
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
	if state.Mode == ui_ipc.ModeError {
		c.lastErrorMsg = state.Message
		c.lastErrorDetail = state.Detail
	} else {
		c.lastErrorMsg = ""
		c.lastErrorDetail = ""
	}
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
			if ready, _, _ := c.isServiceReadyForIdle(ctx); !ready {
				c.syncIdleOrBlockedScreen(ctx)
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
	c.renderMu.Lock()
	ui_ipc.ConfigureAds(ads)
	c.renderMu.Unlock()
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
