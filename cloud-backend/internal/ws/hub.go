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
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Ads      []contracts.AdItem          `json:"ads,omitempty"`
	Sheets   int                         `json:"sheets,omitempty"`
	Error    string                      `json:"error,omitempty"`
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

	healthMu     sync.Mutex
	latestHealth *sessionHealthEntry

	dispatchMu           sync.Mutex
	initialReconnectDone bool
	reconcileRetryNeeded bool
}

// Hub manages active WebSocket connections from kiosks and relays jobs,
// progress events, and connection-bound canonical printer health telemetry.
type Hub struct {
	db          *store.Store
	mu          sync.RWMutex
	sessions    map[string]*kioskSession
	kioskSecret []byte
}

// NewHub creates and initializes a new cloud WebSocket hub.
func NewHub(db *store.Store) *Hub {
	return &Hub{
		db:       db,
		sessions: make(map[string]*kioskSession),
	}
}

// SetKioskSecret sets the pre-shared secret used to authenticate kiosk connections.
func (h *Hub) SetKioskSecret(secret []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kioskSecret = secret
}

func (h *Hub) isActiveSession(kioskID string, sess *kioskSession) bool {
	if h == nil || sess == nil {
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
	h.mu.Lock()
	if old, exists := h.sessions[kioskID]; exists {
		replacedOld = old
	}
	h.sessions[kioskID] = sess
	h.mu.Unlock()
	if replacedOld != nil && replacedOld.conn != nil {
		go func(c *websocket.Conn) {
			_ = c.Close(websocket.StatusPolicyViolation, "replaced by new connection")
		}(replacedOld.conn)
	}
	log.Printf("[HUB] Kiosk connected (unverified readiness): %q (client IP: %s)", kioskID, r.RemoteAddr)

	defer func() {
		var wasActive bool
		h.mu.Lock()
		if h.sessions[kioskID] == sess {
			delete(h.sessions, kioskID)
			wasActive = true
		}
		h.mu.Unlock()
		close(sess.closed)
		log.Printf("[HUB] Kiosk disconnected: %q (active=%v)", kioskID, wasActive)
		// Only reset unacknowledged intents if this was still the active session;
		// an old replaced session must never invalidate a newer connection's state.
		if wasActive && h.db != nil {
			_ = h.db.ResetUnacknowledgedDispatchIntents(context.Background(), kioskID)
		}
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
	if !h.isActiveSession(kioskID, sess) {
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
		// Repeated/replayed identical UpdatedAt must NOT extend server-side freshness, though
		// explicit invalidation to FAULT/UNVERIFIED or ExecutionSafe=false takes effect promptly.
		if snapshot.UpdatedAt.Equal(prev.health.UpdatedAt) {
			if snapshot.EvaluatedHealth == contracts.EvaluatedHealthHealthy && snapshot.ExecutionSafe == prev.health.ExecutionSafe {
				sess.healthMu.Unlock()
				return
			}
			recvAt = prev.receivedAt
		}
	}
	entry := &sessionHealthEntry{
		health:     snapshot,
		receivedAt: recvAt,
	}
	sess.latestHealth = entry
	ready, _ := isHealthEntryReady(entry, now)
	sess.healthMu.Unlock()

	if !ready {
		return
	}

	sess.dispatchMu.Lock()
	shouldReconcile := !wasReady || !sess.initialReconnectDone || sess.reconcileRetryNeeded
	sess.dispatchMu.Unlock()
	if shouldReconcile {
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

	if !sess.initialReconnectDone {
		if err := h.db.ResetUnacknowledgedDispatchIntents(ctx, kioskID); err != nil {
			sess.reconcileRetryNeeded = true
			log.Printf("[HUB] Failed resetting unacknowledged dispatch intents for kiosk %s: %v", kioskID, err)
			return
		}
		sess.initialReconnectDone = true
	}

	pendingJobs, err := h.db.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		sess.reconcileRetryNeeded = true
		log.Printf("[HUB] Failed loading pending dispatch orders for kiosk %s: %v", kioskID, err)
		return
	}
	if len(pendingJobs) == 0 {
		sess.reconcileRetryNeeded = false
		return
	}

	log.Printf("[HUB] Reconciling %d pending dispatch orders for verified ready kiosk %s", len(pendingJobs), kioskID)
	sess.reconcileRetryNeeded = false
	for _, jobCmd := range pendingJobs {
		if ready, reason := h.isSessionReadyForDispatch(sess, time.Now().UTC()); !ready || !h.isActiveSession(kioskID, sess) {
			sess.reconcileRetryNeeded = true
			log.Printf("[HUB] Halting pending dispatch reconciliation for kiosk %s (%s)", kioskID, reason)
			break
		}
		if err := h.sendJobToSession(ctx, sess, jobCmd); err != nil {
			sess.reconcileRetryNeeded = true
			log.Printf("[HUB] Failed to dispatch pending job %s to kiosk %s: %v", jobCmd.JobID, kioskID, err)
			break
		}
		_ = h.db.MarkDispatchIntentSent(ctx, jobCmd.JobID)
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
	h.mu.RLock()
	sess, ok := h.sessions[kioskID]
	h.mu.RUnlock()
	if !ok || sess == nil {
		return false, contracts.PrinterHealth{}, false, false, "kiosk not connected"
	}

	sess.healthMu.Lock()
	entry := sess.latestHealth
	var copied contracts.PrinterHealth
	var receivedAt time.Time
	hasEntry := entry != nil
	if hasEntry {
		copied = cloneHealth(entry.health)
		receivedAt = entry.receivedAt
	}
	sess.healthMu.Unlock()

	if !hasEntry {
		return true, contracts.PrinterHealth{}, false, false, "no telemetry received on active connection"
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
		return fmt.Errorf("send job: kiosk %q not ready (%s)", cmd.KioskID, reason)
	}

	if err := wsjson.Write(ctx, sess.conn, msg); err != nil {
		return fmt.Errorf("send job: write to kiosk %q: %w", cmd.KioskID, err)
	}

	return nil
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

// SendRefill sends an authenticated operator paper refill command to the connected kiosk.
func (h *Hub) SendRefill(ctx context.Context, kioskID string, sheets int) error {
	if h == nil {
		return errors.New("hub is nil")
	}
	h.mu.RLock()
	sess, ok := h.sessions[kioskID]
	h.mu.RUnlock()
	if !ok || sess == nil {
		return errors.New("kiosk not connected")
	}

	msg := wireMessage{
		Type:   "refill",
		Sheets: sheets,
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	return wsjson.Write(ctx, sess.conn, msg)
}

// ConnectedKiosksCount returns the number of currently connected kiosks.
func (h *Hub) ConnectedKiosksCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}
