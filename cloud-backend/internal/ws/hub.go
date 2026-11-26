package ws

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

const (
	// DefaultTelemetryFreshnessWindow is the canonical maximum age (6s) for both
	// printer observation UpdatedAt and cloud server receipt time.
	DefaultTelemetryFreshnessWindow = 6 * time.Second
	// FutureHealthTimestampTolerance bounds acceptable clock skew on incoming timestamps.
	FutureHealthTimestampTolerance = 5 * time.Second
)

type wireMessage struct {
	Type         string                      `json:"type"`
	Job          *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress     *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health       *contracts.PrinterHealth    `json:"health,omitempty"`
	Ads          []contracts.AdItem          `json:"ads,omitempty"`
	Sheets       *int                        `json:"sheets,omitempty"`
	RequestID    string                      `json:"request_id,omitempty"`
	OK           bool                        `json:"ok,omitempty"`
	Calibrated   bool                        `json:"calibrated,omitempty"`
	InventorySeq uint64                      `json:"inventory_seq,omitempty"`
	Error        string                      `json:"error,omitempty"`
}

type pendingRefillOp struct {
	reqID           string
	requestedSheets int
	initialSeq      uint64
	ch              chan error
}

type sessionHealthEntry struct {
	health     contracts.PrinterHealth
	receivedAt time.Time
}

type kioskSession struct {
	kioskID string
	conn    *websocket.Conn
	sendMu  sync.Mutex
	closed  chan struct{}

	healthMu       sync.Mutex
	latestHealth   *sessionHealthEntry
	pendingRefill  *pendingRefillOp
	inventorySeq   uint64
	expiryTimer    *time.Timer
	expiryTimerGen uint64

	dispatchMu           sync.Mutex
	initialReconnectDone atomic.Bool
	reconcileRetryNeeded atomic.Bool
	inactive             atomic.Bool
}

// Hub manages active WebSocket connections from kiosks and relays jobs,
// progress events, and connection-bound canonical printer health telemetry.
type Hub struct {
	db                  *store.Store
	mu                  sync.RWMutex
	resetMu             sync.Mutex
	sessions            map[string]*kioskSession
	kioskSecret         []byte
	policyMu            sync.RWMutex
	policy              contracts.ProductCapabilityPolicy
	reserveMapMu        sync.Mutex
	reserveLocks        map[string]*sync.Mutex
	invalidationMu      sync.Mutex
	pendingInvalidation map[string]uint64
	refillSeq           atomic.Uint64
}

// NewHub creates and initializes a new cloud WebSocket hub.
func NewHub(db *store.Store) *Hub {
	return &Hub{
		db:                  db,
		sessions:            make(map[string]*kioskSession),
		policy:              contracts.DefaultDemoProductPolicy(),
		reserveLocks:        make(map[string]*sync.Mutex),
		pendingInvalidation: make(map[string]uint64),
	}
}

// LockKioskReservation acquires the per-kiosk ordering boundary mutex used to serialize
// reservation inventory-read+commit against the start of paper calibration.
func (h *Hub) LockKioskReservation(kioskID string) func() {
	if h == nil {
		return func() {}
	}
	h.reserveMapMu.Lock()
	if h.reserveLocks == nil {
		h.reserveLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := h.reserveLocks[kioskID]
	if !ok {
		mu = &sync.Mutex{}
		h.reserveLocks[kioskID] = mu
	}
	h.reserveMapMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// SetProductPolicy sets the canonical product capability policy enforced during dispatch and reconciliation.
func (h *Hub) SetProductPolicy(policy contracts.ProductCapabilityPolicy) {
	if h == nil {
		return
	}
	h.policyMu.Lock()
	h.policy = policy
	h.policyMu.Unlock()
}

func (h *Hub) productPolicy() contracts.ProductCapabilityPolicy {
	if h == nil {
		return contracts.DefaultDemoProductPolicy()
	}
	h.policyMu.RLock()
	p := h.policy
	h.policyMu.RUnlock()
	return p
}

// SetKioskSecret sets the pre-shared secret used to authenticate kiosk connections.
func (h *Hub) SetKioskSecret(secret []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kioskSecret = secret
}

func (h *Hub) isActiveSession(kioskID string, sess *kioskSession) bool {
	if h == nil || sess == nil || sess.inactive.Load() {
		return false
	}
	h.mu.RLock()
	cur := h.sessions[kioskID]
	h.mu.RUnlock()
	return cur == sess
}

// ServeHTTP handles incoming WebSocket connection upgrades at /ws?kiosk_id=<id>.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kioskID := strings.TrimSpace(r.URL.Query().Get("kiosk_id"))
	if kioskID == "" {
		http.Error(w, "kiosk_id required", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	expectedSecret := h.kioskSecret
	h.mu.RUnlock()

	if len(expectedSecret) > 0 {
		var token string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("secret"))
		}
		if !hmac.Equal([]byte(token), expectedSecret) {
			http.Error(w, "unauthorized kiosk", http.StatusUnauthorized)
			return
		}
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	conn.SetReadLimit(1024 * 1024)

	// New connections begin strictly unverified (latestHealth == nil).
	sess := &kioskSession{
		kioskID: kioskID,
		conn:    conn,
		closed:  make(chan struct{}),
	}

	var replacedOld *kioskSession
	h.resetMu.Lock()
	h.mu.Lock()
	if old, exists := h.sessions[kioskID]; exists {
		old.inactive.Store(true)
		replacedOld = old
	}
	h.sessions[kioskID] = sess
	h.mu.Unlock()
	h.resetMu.Unlock()
	if replacedOld != nil {
		replacedOld.healthMu.Lock()
		replacedOld.expiryTimerGen++
		if replacedOld.expiryTimer != nil {
			replacedOld.expiryTimer.Stop()
			replacedOld.expiryTimer = nil
		}
		replacedOld.healthMu.Unlock()
		if replacedOld.conn != nil {
			go func(c *websocket.Conn) {
				_ = c.Close(websocket.StatusPolicyViolation, "replaced by new connection")
			}(replacedOld.conn)
		}
	}
	// On every new kiosk connection (including after backend restart), invalidate any obsolete
	// persisted unpaid checkout sessions before restoring customer admission.
	h.triggerUnpaidInvalidation(kioskID)
	log.Printf("[HUB] Kiosk connected (unverified readiness): %q (client IP: %s)", kioskID, r.RemoteAddr)

	defer func() {
		sess.healthMu.Lock()
		sess.expiryTimerGen++
		if sess.expiryTimer != nil {
			sess.expiryTimer.Stop()
			sess.expiryTimer = nil
		}
		sess.healthMu.Unlock()

		var wasActive bool
		h.mu.Lock()
		sess.inactive.Store(true)
		if h.sessions[kioskID] == sess {
			delete(h.sessions, kioskID)
			wasActive = true
		}
		h.mu.Unlock()
		close(sess.closed)
		if wasActive {
			h.triggerUnpaidInvalidation(kioskID)
		}
		log.Printf("[HUB] Kiosk disconnected: %q (active=%v)", kioskID, wasActive)
		_ = conn.Close(websocket.StatusNormalClosure, "session ended")
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sync active ads on connection; pending dispatch intents intentionally wait until
	// verified healthy and execution-safe telemetry arrives on this exact session.
	if h.db != nil {
		if activeAds, err := h.db.GetActiveAds(ctx); err == nil && len(activeAds) > 0 {
			sess.sendMu.Lock()
			_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: activeAds})
			sess.sendMu.Unlock()
		}
	}

	for {
		var msg wireMessage
		err := wsjson.Read(ctx, conn, &msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "heartbeat":
			// Maintain connection liveness only; heartbeats never extend health readiness.
		case "ack", "job_ack":
			jobID := ""
			if msg.Progress != nil {
				jobID = msg.Progress.JobID
			}
			if jobID == "" && msg.Job != nil {
				jobID = msg.Job.JobID
			}
			if jobID != "" && h.db != nil {
				_ = h.db.MarkDispatchIntentAcknowledged(ctx, jobID)
				if msg.Progress != nil && msg.Progress.KioskID == kioskID && isValidJobStatus(msg.Progress.Status) && !msg.Progress.Timestamp.IsZero() {
					_ = h.db.UpdateJobProgress(ctx, *msg.Progress)
				}
			}
		case "progress":
			if msg.Progress == nil || msg.Progress.KioskID != kioskID {
				continue
			}
			if strings.TrimSpace(msg.Progress.JobID) == "" || !isValidJobStatus(msg.Progress.Status) || msg.Progress.Timestamp.IsZero() {
				continue
			}
			if h.db != nil {
				_ = h.db.UpdateJobProgress(ctx, *msg.Progress)
			}
		case "health":
			h.handleHealthMessage(ctx, kioskID, sess, msg.Health)
		case "refill_ack":
			h.handleRefillAck(sess, msg)
		case "error":
			// Operational error
		default:
			// Ignore unexpected message type
		}
	}
}

func (h *Hub) handleHealthMessage(ctx context.Context, kioskID string, sess *kioskSession, incoming *contracts.PrinterHealth) {
	if !h.isActiveSession(kioskID, sess) || incoming == nil {
		return
	}

	now := time.Now().UTC()
	if err := validateHealthPayload(incoming, now); err != nil {
		log.Printf("[HUB] Ignoring malformed health telemetry from kiosk %q: %v", kioskID, err)
		return
	}

	snapshot := cloneHealth(*incoming)

	sess.healthMu.Lock()
	if sess.inactive.Load() {
		sess.healthMu.Unlock()
		return
	}
	wasReady := false
	recvAt := now
	if sess.latestHealth != nil {
		prev := sess.latestHealth
		wasReady, _ = isHealthEntryReady(prev, now)
		if snapshot.UpdatedAt.Before(prev.health.UpdatedAt) {
			sess.healthMu.Unlock()
			return
		}
		// Equal-timestamp frames may revoke readiness immediately, but must NEVER
		// extend freshness or restore previously revoked readiness.
		if snapshot.UpdatedAt.Equal(prev.health.UpdatedAt) {
			snapReady, _ := isHealthEntryReady(&sessionHealthEntry{health: snapshot, receivedAt: prev.receivedAt}, now)
			if snapReady {
				sess.healthMu.Unlock()
				return
			}
			recvAt = prev.receivedAt
		}
	}
	// Pi alone owns and advances InventorySeq. Sequence 0 is treated consistently as the initial sequence.
	// Only an authoritative Pi sequence advancement (> sess.inventorySeq) proves post-refill state;
	// a frame with an older sequence (< sess.inventorySeq) cannot overwrite newer inventory.
	if snapshot.InventorySeq < sess.inventorySeq {
		if sess.latestHealth != nil {
			snapshot.EstimatedSheets = sess.latestHealth.health.EstimatedSheets
			snapshot.InventoryCalibrated = sess.latestHealth.health.InventoryCalibrated
			snapshot.InventorySeq = sess.latestHealth.health.InventorySeq
		}
	} else if snapshot.InventorySeq > sess.inventorySeq {
		sess.inventorySeq = snapshot.InventorySeq
		if op := sess.pendingRefill; op != nil && snapshot.InventorySeq > op.initialSeq {
			sess.pendingRefill = nil
			var opErr error
			if snapshot.EstimatedSheets != op.requestedSheets {
				opErr = fmt.Errorf("telemetry sequence %d reported %d sheets (expected %d)", snapshot.InventorySeq, snapshot.EstimatedSheets, op.requestedSheets)
			}
			if op.ch != nil {
				select {
				case op.ch <- opErr:
				default:
				}
			}
		}
	}
	entry := &sessionHealthEntry{
		health:     snapshot,
		receivedAt: recvAt,
	}
	sess.latestHealth = entry
	ready, _ := isHealthEntryReady(entry, now)
	availOK, _ := contracts.IsHealthAvailable(snapshot, now, DefaultTelemetryFreshnessWindow)
	sess.expiryTimerGen++
	if sess.expiryTimer != nil {
		sess.expiryTimer.Stop()
		sess.expiryTimer = nil
	}
	if availOK && sess.pendingRefill == nil {
		// Schedule invalidation at the earliest authoritative expiry across hardware observations,
		// UpdatedAt, and server receipt freshness without restarting the full window.
		earliestExpiry := recvAt.Add(DefaultTelemetryFreshnessWindow)
		if _, oldestObs, _ := contracts.ValidateHealthEvidenceFreshness(snapshot, now, DefaultTelemetryFreshnessWindow); !oldestObs.IsZero() {
			if obsExp := oldestObs.Add(DefaultTelemetryFreshnessWindow); obsExp.Before(earliestExpiry) {
				earliestExpiry = obsExp
			}
		}
		if !snapshot.UpdatedAt.IsZero() {
			if updExp := snapshot.UpdatedAt.Add(DefaultTelemetryFreshnessWindow); updExp.Before(earliestExpiry) {
				earliestExpiry = updExp
			}
		}
		delay := earliestExpiry.Sub(now) + 10*time.Millisecond
		if delay < time.Millisecond {
			delay = time.Millisecond
		}
		timerGen := sess.expiryTimerGen
		sess.expiryTimer = time.AfterFunc(delay, func() {
			h.onSessionTelemetryExpired(kioskID, sess, timerGen)
		})
	}
	sess.healthMu.Unlock()

	if !availOK {
		h.triggerUnpaidInvalidation(kioskID)
	}

	if !ready || !h.isActiveSession(kioskID, sess) {
		return
	}

	if !sess.inactive.Load() && (!wasReady || !sess.initialReconnectDone.Load() || sess.reconcileRetryNeeded.Load()) {
		h.reconcilePendingDispatchOrders(ctx, kioskID, sess)
	}
}

func (h *Hub) reconcilePendingDispatchOrders(ctx context.Context, kioskID string, sess *kioskSession) {
	if h.db == nil || !h.isActiveSession(kioskID, sess) {
		return
	}

	sess.dispatchMu.Lock()
	defer sess.dispatchMu.Unlock()

	if ready, _ := h.isSessionReadyForDispatch(sess, time.Now().UTC()); !ready || !h.isActiveSession(kioskID, sess) {
		return
	}

	if !sess.initialReconnectDone.Load() {
		h.resetMu.Lock()
		active := h.isActiveSession(kioskID, sess)
		var resetErr error
		if active {
			resetCtx, resetCancel := context.WithTimeout(ctx, 2*time.Second)
			resetErr = h.db.ResetUnacknowledgedDispatchIntents(resetCtx, kioskID)
			resetCancel()
		}
		h.resetMu.Unlock()
		if !active {
			return
		}
		if resetErr != nil {
			sess.reconcileRetryNeeded.Store(true)
			log.Printf("[HUB] Failed resetting unacknowledged dispatch intents for kiosk %s: %v", kioskID, resetErr)
			return
		}
		sess.initialReconnectDone.Store(true)
	}

	// Clear retry flag before loading/sending so concurrent SendJob failures during this
	// loop are preserved and never overwritten.
	sess.reconcileRetryNeeded.Store(false)

	pendingJobs, err := h.db.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		sess.reconcileRetryNeeded.Store(true)
		log.Printf("[HUB] Failed loading pending dispatch orders for kiosk %s: %v", kioskID, err)
		return
	}
	if len(pendingJobs) == 0 {
		return
	}

	log.Printf("[HUB] Reconciling %d pending dispatch orders for verified ready kiosk %s", len(pendingJobs), kioskID)
	for _, jobCmd := range pendingJobs {
		if ready, reason := h.isSessionReadyForDispatch(sess, time.Now().UTC()); !ready || !h.isActiveSession(kioskID, sess) {
			sess.reconcileRetryNeeded.Store(true)
			log.Printf("[HUB] Halting pending dispatch reconciliation for kiosk %s (%s)", kioskID, reason)
			break
		}
		if err := h.sendJobToSession(ctx, sess, jobCmd); err != nil {
			sess.reconcileRetryNeeded.Store(true)
			log.Printf("[HUB] Failed to dispatch pending job %s to kiosk %s: %v", jobCmd.JobID, kioskID, err)
			break
		}
		if err := h.db.MarkDispatchIntentSent(ctx, jobCmd.JobID); err != nil {
			// Bounded retry with background context in case caller ctx was canceled right after wire write.
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 2*time.Second)
			retryErr := h.db.MarkDispatchIntentSent(bgCtx, jobCmd.JobID)
			bgCancel()
			if retryErr != nil {
				sess.reconcileRetryNeeded.Store(true)
				log.Printf("[HUB] Warning: failed to mark reconciled job %s SENT: %v", jobCmd.JobID, retryErr)
				break
			}
		}
		log.Printf("[HUB] Reconciled and dispatched pending job %s to kiosk %s", jobCmd.JobID, kioskID)
	}
}

func validateHealthPayload(h *contracts.PrinterHealth, now time.Time) error {
	if h == nil {
		return errors.New("nil health payload")
	}
	if !isValidHealthState(h.State) {
		return fmt.Errorf("invalid operational state %q", h.State)
	}
	if !isValidEvaluatedHealth(h.EvaluatedHealth) {
		return fmt.Errorf("invalid evaluated_health %q", h.EvaluatedHealth)
	}
	if !isValidTrayState(h.PaperTrayStatus) {
		return fmt.Errorf("invalid paper_tray_status %q", h.PaperTrayStatus)
	}
	if h.UpdatedAt.IsZero() {
		return errors.New("updated_at must be non-zero")
	}
	if h.UpdatedAt.After(now.Add(FutureHealthTimestampTolerance)) {
		return fmt.Errorf("updated_at %s is in the future", h.UpdatedAt.Format(time.RFC3339))
	}
	if h.TonerLevelKnown {
		if h.TonerLevelPercent < 0 || h.TonerLevelPercent > 100 {
			return fmt.Errorf("toner_level_percent %d out of range [0,100]", h.TonerLevelPercent)
		}
	} else if h.TonerLevelPercent < -1 || h.TonerLevelPercent > 100 {
		return fmt.Errorf("unknown toner_level_percent %d invalid", h.TonerLevelPercent)
	}
	if h.DrumLifeKnown {
		if h.DrumLifePercent < 0 || h.DrumLifePercent > 100 {
			return fmt.Errorf("drum_life_percent %d out of range [0,100]", h.DrumLifePercent)
		}
	} else if h.DrumLifePercent < -1 || h.DrumLifePercent > 100 {
		return fmt.Errorf("unknown drum_life_percent %d invalid", h.DrumLifePercent)
	}
	if h.EvaluatedHealth == contracts.EvaluatedHealthHealthy {
		if ok, _, reason := contracts.ValidateHealthEvidenceFreshness(*h, now, DefaultTelemetryFreshnessWindow); !ok {
			return fmt.Errorf("HEALTHY payload lacks fresh required source evidence: %s", reason)
		}
		if !h.Online || h.State == "ERROR" || h.State == "OFFLINE" || h.State == "UNKNOWN" ||
			h.CoverOpen || h.PaperJam || h.OutputBinFull || h.PaperTrayStatus == "EMPTY" || len(h.BlockingReasons) > 0 {
			return errors.New("contradictory HEALTHY claim with active fault or non-ready state")
		}
	}
	return nil
}

func isHealthEntryReady(entry *sessionHealthEntry, now time.Time) (bool, string) {
	if entry == nil {
		return false, "no telemetry received on active connection"
	}
	if now.Sub(entry.receivedAt) > DefaultTelemetryFreshnessWindow {
		return false, "server receipt freshness expired (>6s)"
	}
	return contracts.ValidateReadyHealth(entry.health, now, DefaultTelemetryFreshnessWindow)
}

func (h *Hub) isSessionReadyForDispatch(sess *kioskSession, now time.Time) (bool, string) {
	if sess == nil {
		return false, "kiosk not connected"
	}
	sess.healthMu.Lock()
	entry := sess.latestHealth
	var copied *sessionHealthEntry
	if entry != nil {
		c := cloneHealth(entry.health)
		copied = &sessionHealthEntry{health: c, receivedAt: entry.receivedAt}
	}
	sess.healthMu.Unlock()
	return isHealthEntryReady(copied, now)
}

// EvaluateKioskReadiness returns the canonical connection-bound readiness snapshot and decision
// for a kiosk so WS dispatch admission and HTTP reservation/release share one source of truth.
func (h *Hub) EvaluateKioskReadiness(kioskID string) (connected bool, health contracts.PrinterHealth, hasHealth bool, ready bool, reason string) {
	if h == nil {
		return false, contracts.PrinterHealth{}, false, false, "hub is nil"
	}

	for attempt := 0; attempt < 2; attempt++ {
		h.mu.RLock()
		sess, ok := h.sessions[kioskID]
		h.mu.RUnlock()
		if !ok || sess == nil || sess.inactive.Load() {
			return false, contracts.PrinterHealth{}, false, false, "kiosk not connected"
		}

		sess.healthMu.Lock()
		inactive := sess.inactive.Load()
		entry := sess.latestHealth
		calUncertain := sess.pendingRefill != nil
		var copied contracts.PrinterHealth
		var receivedAt time.Time
		hasEntry := entry != nil
		if !inactive && hasEntry {
			copied = cloneHealth(entry.health)
			if calUncertain {
				copied.InventoryCalibrated = false
			}
			receivedAt = entry.receivedAt
		}
		sess.healthMu.Unlock()

		if inactive || !h.isActiveSession(kioskID, sess) {
			continue
		}

		if !hasEntry {
			return true, contracts.PrinterHealth{}, false, false, "no telemetry received on active connection"
		}
		if h.hasPendingInvalidation(kioskID) {
			return true, copied, true, false, "pending unpaid checkout invalidation"
		}

		now := time.Now().UTC()
		entryCopy := &sessionHealthEntry{health: copied, receivedAt: receivedAt}
		ready, reason = isHealthEntryReady(entryCopy, now)

		if okFresh, _, freshErr := contracts.ValidateHealthEvidenceFreshness(copied, now, DefaultTelemetryFreshnessWindow); now.Sub(receivedAt) > DefaultTelemetryFreshnessWindow || !okFresh {
			copied.Online = false
			if copied.EvaluatedHealth != contracts.EvaluatedHealthFault {
				copied.EvaluatedHealth = contracts.EvaluatedHealthUnverified
			}
			if copied.EvidenceFreshness != nil {
				ef := *copied.EvidenceFreshness
				ef.CUPS.Fresh = false
				ef.NativeIPP.Fresh = false
				ef.SNMP.Fresh = false
				copied.EvidenceFreshness = &ef
			}
			if copied.ErrorMessage == "" {
				copied.ErrorMessage = firstNonEmpty(freshErr, "printer health telemetry expired")
			}
		}
		return true, copied, true, ready, reason
	}
	return false, contracts.PrinterHealth{}, false, false, "kiosk not connected"
}

// IsKioskReadyForDispatch returns true iff the kiosk has an active session whose latest valid
// telemetry is fresh within 6s (both observation and server receipt), HEALTHY, and ExecutionSafe.
func (h *Hub) IsKioskReadyForDispatch(kioskID string) (bool, string) {
	_, _, _, ready, reason := h.EvaluateKioskReadiness(kioskID)
	return ready, reason
}

// SendJob dispatches a PrintJobCommand envelope ONLY when the kiosk is connected and currently
// passes connection-bound canonical readiness and execution-safety gating.
func (h *Hub) SendJob(ctx context.Context, cmd contracts.PrintJobCommand) error {
	if h == nil {
		return fmt.Errorf("send job: hub is nil")
	}
	if ctx == nil {
		return fmt.Errorf("send job: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send job: %w", err)
	}

	h.mu.RLock()
	sess, ok := h.sessions[cmd.KioskID]
	h.mu.RUnlock()
	if !ok || sess == nil {
		return fmt.Errorf("send job: kiosk %q not connected", cmd.KioskID)
	}
	return h.sendJobToSession(ctx, sess, cmd)
}

func (h *Hub) sendJobToSession(ctx context.Context, sess *kioskSession, cmd contracts.PrintJobCommand) error {
	if sess == nil {
		return fmt.Errorf("send job: kiosk %q not connected", cmd.KioskID)
	}
	if strings.TrimSpace(cmd.JobID) == "" {
		return fmt.Errorf("send job: empty job ID")
	}
	if strings.TrimSpace(cmd.KioskID) == "" {
		return fmt.Errorf("send job: empty kiosk ID")
	}
	if strings.TrimSpace(cmd.DownloadURL) == "" {
		return fmt.Errorf("send job: empty download URL")
	}
	if cmd.TotalPages < 1 {
		return fmt.Errorf("send job: total pages must be >= 1")
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("send job: total sheets must be >= 1")
	}
	if cmd.Copies < 1 {
		return fmt.Errorf("send job: copies must be >= 1")
	}
	if !isValidDuplex(cmd.Duplex) {
		return fmt.Errorf("send job: invalid duplex mode %q", cmd.Duplex)
	}
	if err := contracts.ValidatePrintModeAgainstPolicy(cmd.Duplex, cmd.ColorMode, h.productPolicy()); err != nil {
		return fmt.Errorf("send job: %w", err)
	}
	if cmd.StartFromPage < 1 || cmd.StartFromPage > cmd.TotalPages {
		return fmt.Errorf("send job: invalid start from page %d", cmd.StartFromPage)
	}

	msg := wireMessage{
		Type: "job",
		Job:  &cmd,
	}

	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()

	if !h.isActiveSession(cmd.KioskID, sess) {
		return fmt.Errorf("send job: kiosk %q connection replaced", cmd.KioskID)
	}
	if ready, reason := h.isSessionReadyForDispatch(sess, time.Now().UTC()); !ready {
		sess.reconcileRetryNeeded.Store(true)
		return fmt.Errorf("send job: kiosk %q not ready (%s)", cmd.KioskID, reason)
	}

	if err := wsjson.Write(ctx, sess.conn, msg); err != nil {
		sess.reconcileRetryNeeded.Store(true)
		return fmt.Errorf("send job: write to kiosk %q: %w", cmd.KioskID, err)
	}

	return nil
}

// MarkDispatchRetryNeeded flags the currently active kiosk session so its next verified healthy
// telemetry frame retries any still-PENDING authorized dispatch intents without requiring reconnect.
func (h *Hub) MarkDispatchRetryNeeded(kioskID string) {
	if h == nil {
		return
	}
	h.mu.RLock()
	sess, ok := h.sessions[kioskID]
	h.mu.RUnlock()
	if !ok || sess == nil || sess.inactive.Load() {
		return
	}
	sess.reconcileRetryNeeded.Store(true)
}

// IsConnected returns whether the specified kiosk currently has an active WebSocket session.
func (h *Hub) IsConnected(kioskID string) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	sess, ok := h.sessions[kioskID]
	return ok && sess != nil
}

// LatestHealth returns the connection-bound printer health observation for an active kiosk session.
// If the kiosk is disconnected or no telemetry has arrived on this session, ok is false.
// If stored telemetry is older than 6s (observation or server receipt), EvaluatedHealth is expired to UNVERIFIED.
func (h *Hub) LatestHealth(kioskID string) (contracts.PrinterHealth, bool) {
	connected, copied, hasHealth, _, _ := h.EvaluateKioskReadiness(kioskID)
	if !connected || !hasHealth {
		return contracts.PrinterHealth{}, false
	}
	return copied, true
}

func cloneHealth(h contracts.PrinterHealth) contracts.PrinterHealth {
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

func firstNonEmpty(a, b string) string {
	if t := strings.TrimSpace(a); t != "" {
		return t
	}
	return strings.TrimSpace(b)
}

func isValidHealthState(state string) bool {
	switch state {
	case "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE", "UNKNOWN":
		return true
	default:
		return false
	}
}

func isValidEvaluatedHealth(status contracts.EvaluatedHealthStatus) bool {
	switch status {
	case contracts.EvaluatedHealthHealthy, contracts.EvaluatedHealthFault, contracts.EvaluatedHealthUnverified:
		return true
	default:
		return false
	}
}

func isValidTrayState(tray string) bool {
	switch tray {
	case "OK", "LOW", "EMPTY", "UNKNOWN", "CHECK_PRINTER":
		return true
	default:
		return false
	}
}

func isValidDuplex(d contracts.DuplexMode) bool {
	switch d {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
}

func isValidJobStatus(s contracts.JobStatus) bool {
	switch s {
	case contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusPausedError,
		contracts.JobStatusFailedRefundable:
		return true
	default:
		return false
	}
}

// BroadcastSyncAds broadcasts updated ads to all currently connected kiosks in real time.
func (h *Hub) BroadcastSyncAds(ctx context.Context, ads []contracts.AdItem) {
	h.mu.RLock()
	sessions := make([]*kioskSession, 0, len(h.sessions))
	for _, sess := range h.sessions {
		sessions = append(sessions, sess)
	}
	h.mu.RUnlock()

	msg := wireMessage{Type: "sync_ads", Ads: ads}
	for _, sess := range sessions {
		sess.sendMu.Lock()
		_ = wsjson.Write(ctx, sess.conn, msg)
		sess.sendMu.Unlock()
	}
}

func (h *Hub) handleRefillAck(sess *kioskSession, msg wireMessage) {
	if sess == nil || strings.TrimSpace(msg.RequestID) == "" {
		return
	}

	sess.healthMu.Lock()
	op := sess.pendingRefill
	if op == nil || op.reqID != msg.RequestID || sess.inactive.Load() {
		sess.healthMu.Unlock()
		return
	}

	notify := func(err error) {
		if op.ch != nil {
			select {
			case op.ch <- err:
			default:
			}
		}
	}

	if !msg.OK {
		sess.pendingRefill = nil
		sess.healthMu.Unlock()
		errMsg := firstNonEmpty(msg.Error, "kiosk rejected refill calibration")
		notify(errors.New(errMsg))
		return
	}
	if !msg.Calibrated {
		sess.healthMu.Unlock()
		notify(errors.New("kiosk did not confirm durable calibration flag"))
		return
	}
	if msg.Sheets == nil || *msg.Sheets != op.requestedSheets {
		got := 0
		if msg.Sheets != nil {
			got = *msg.Sheets
		}
		sess.healthMu.Unlock()
		notify(fmt.Errorf("returned sheet count %d does not match requested count %d", got, op.requestedSheets))
		return
	}
	if msg.InventorySeq == 0 || msg.InventorySeq <= op.initialSeq {
		sess.healthMu.Unlock()
		notify(fmt.Errorf("refill acknowledgement sequence %d did not advance past initial sequence %d", msg.InventorySeq, op.initialSeq))
		return
	}

	// Validated calibration outcome: resolve pending operation and update inventory only if
	// this sequence has not already been observed via telemetry (never overwrite same-sequence consumption).
	sess.pendingRefill = nil
	if msg.InventorySeq > sess.inventorySeq {
		sess.inventorySeq = msg.InventorySeq
		if sess.latestHealth != nil {
			sess.latestHealth.health.EstimatedSheets = *msg.Sheets
			sess.latestHealth.health.InventoryCalibrated = true
			sess.latestHealth.health.InventorySeq = msg.InventorySeq
		}
	}
	sess.healthMu.Unlock()
	notify(nil)
}

// SendRefill sends an authenticated operator paper refill command to the connected kiosk
// and waits for confirmed durable application from the Pi.
func (h *Hub) SendRefill(ctx context.Context, kioskID string, sheets int) error {
	if h == nil {
		return errors.New("hub is nil")
	}
	if ctx == nil {
		return errors.New("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sheets < 0 {
		return fmt.Errorf("sheets must be non-negative, got %d", sheets)
	}
	h.mu.RLock()
	sess, ok := h.sessions[kioskID]
	h.mu.RUnlock()
	if !ok || sess == nil || !h.isActiveSession(kioskID, sess) {
		return errors.New("kiosk not connected")
	}

	reqID := fmt.Sprintf("refill-%d-%d", time.Now().UnixNano(), h.refillSeq.Add(1))
	ackCh := make(chan error, 1)

	unlockReserve := h.LockKioskReservation(kioskID)
	sess.healthMu.Lock()
	if !h.isActiveSession(kioskID, sess) {
		sess.healthMu.Unlock()
		unlockReserve()
		return errors.New("kiosk not connected")
	}
	if sess.pendingRefill != nil {
		sess.healthMu.Unlock()
		unlockReserve()
		return errors.New("previous refill calibration is still unresolved; cannot start another calibration")
	}
	sess.pendingRefill = &pendingRefillOp{
		reqID:           reqID,
		requestedSheets: sheets,
		initialSeq:      sess.inventorySeq,
		ch:              ackCh,
	}
	sess.expiryTimerGen++
	if sess.expiryTimer != nil {
		sess.expiryTimer.Stop()
		sess.expiryTimer = nil
	}
	sess.healthMu.Unlock()
	unlockReserve()

	// Unresolved calibration makes kiosk unavailable; invalidate any active unpaid checkouts.
	h.triggerUnpaidInvalidation(kioskID)

	s := sheets
	msg := wireMessage{
		Type:      "refill",
		Sheets:    &s,
		RequestID: reqID,
	}
	sess.sendMu.Lock()
	writeErr := wsjson.Write(ctx, sess.conn, msg)
	sess.sendMu.Unlock()
	if writeErr != nil {
		return fmt.Errorf("failed to transmit refill command: %w", writeErr)
	}

	select {
	case opErr := <-ackCh:
		if !h.isActiveSession(kioskID, sess) {
			return errors.New("kiosk session replaced or disconnected before confirming refill calibration")
		}
		return opErr
	case <-sess.closed:
		return errors.New("kiosk session replaced or disconnected before confirming refill calibration")
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for kiosk refill confirmation: %w", ctx.Err())
	}
}

// ConnectedKiosksCount returns the number of currently connected kiosks.
func (h *Hub) ConnectedKiosksCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}

func (h *Hub) hasPendingInvalidation(kioskID string) bool {
	if h == nil {
		return false
	}
	h.invalidationMu.Lock()
	pending := h.pendingInvalidation[kioskID] > 0
	h.invalidationMu.Unlock()
	return pending
}

func (h *Hub) triggerUnpaidInvalidation(kioskID string) {
	if h == nil || strings.TrimSpace(kioskID) == "" || h.db == nil {
		return
	}
	h.invalidationMu.Lock()
	if h.pendingInvalidation == nil {
		h.pendingInvalidation = make(map[string]uint64)
	}
	h.pendingInvalidation[kioskID]++
	gen := h.pendingInvalidation[kioskID]
	h.invalidationMu.Unlock()

	h.runUnpaidInvalidation(kioskID, gen)
}

func (h *Hub) runUnpaidInvalidation(kioskID string, gen uint64) {
	if h == nil || h.db == nil {
		return
	}
	h.invalidationMu.Lock()
	if h.pendingInvalidation[kioskID] != gen {
		h.invalidationMu.Unlock()
		return
	}
	h.invalidationMu.Unlock()

	unlock := h.LockKioskReservation(kioskID)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := h.db.CancelActiveUnpaidSessionsForKiosk(ctx, kioskID, time.Now().UTC())
	cancel()
	unlock()

	h.invalidationMu.Lock()
	defer h.invalidationMu.Unlock()
	if err == nil || strings.Contains(err.Error(), "database is closed") {
		if h.pendingInvalidation[kioskID] == gen {
			delete(h.pendingInvalidation, kioskID)
		}
		return
	}
	log.Printf("[HUB] Failed canceling unpaid sessions for unavailable kiosk %q (retrying): %v", kioskID, err)
	if h.pendingInvalidation[kioskID] == gen {
		time.AfterFunc(250*time.Millisecond, func() {
			h.runUnpaidInvalidation(kioskID, gen)
		})
	}
}

func (h *Hub) onSessionTelemetryExpired(kioskID string, sess *kioskSession, timerGen uint64) {
	if !h.isActiveSession(kioskID, sess) {
		return
	}
	sess.healthMu.Lock()
	staleTimer := sess.inactive.Load() || sess.expiryTimerGen != timerGen
	sess.healthMu.Unlock()
	if staleTimer {
		return
	}
	if _, _, _, ready, _ := h.EvaluateKioskReadiness(kioskID); !ready {
		h.triggerUnpaidInvalidation(kioskID)
	}
}
