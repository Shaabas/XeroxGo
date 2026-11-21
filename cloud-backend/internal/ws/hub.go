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

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

type wireMessage struct {
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Ads      []contracts.AdItem          `json:"ads,omitempty"`
	Sheets   int                         `json:"sheets,omitempty"`
	Error    string                      `json:"error,omitempty"`
	OrderID  string                      `json:"order_id,omitempty"`
}

type kioskSession struct {
	kioskID string
	conn    *websocket.Conn
	sendMu  sync.Mutex
	closed  chan struct{}
}

// Hub manages active WebSocket connections from kiosks and relays jobs,
// progress events, and printer health observations.
type Hub struct {
	db          *store.Store
	mu          sync.RWMutex
	sessions    map[string]*kioskSession
	kioskSecret []byte

	healthMu sync.RWMutex
	health   map[string]contracts.PrinterHealth
}

// NewHub creates and initializes a new cloud WebSocket hub.
func NewHub(db *store.Store) *Hub {
	return &Hub{
		db:       db,
		sessions: make(map[string]*kioskSession),
		health:   make(map[string]contracts.PrinterHealth),
	}
}

// SetKioskSecret sets the pre-shared secret used to authenticate kiosk connections.
func (h *Hub) SetKioskSecret(secret []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kioskSecret = secret
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

	sess := &kioskSession{
		kioskID: kioskID,
		conn:    conn,
		closed:  make(chan struct{}),
	}

	h.mu.Lock()
	if old, exists := h.sessions[kioskID]; exists {
		_ = old.conn.Close(websocket.StatusPolicyViolation, "replaced by new connection")
	}
	h.sessions[kioskID] = sess
	h.mu.Unlock()
	log.Printf("[HUB] Kiosk connected: %q (client IP: %s)", kioskID, r.RemoteAddr)

	defer func() {
		h.mu.Lock()
		if h.sessions[kioskID] == sess {
			delete(h.sessions, kioskID)
		}
		h.mu.Unlock()
		close(sess.closed)
		log.Printf("[HUB] Kiosk disconnected: %q", kioskID)
		// Leave SENT intact until the next connection performs reconciliation.
		// Resetting from the old connection's defer can race a replacement
		// connection and downgrade intents it has just delivered.
		_ = conn.Close(websocket.StatusNormalClosure, "session ended")
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Proactively sync active ads to newly connected kiosk
	if h.db != nil {
		if activeAds, err := h.db.GetActiveAds(ctx); err == nil && len(activeAds) > 0 {
			sess.sendMu.Lock()
			_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: activeAds})
			sess.sendMu.Unlock()
		}
		// Only a new authenticated Pi connection may initiate replay.
		// ACKNOWLEDGED and COMPLETED jobs stay terminal; the Pi uses its
		// persisted JobID state to deduplicate a previously delivered command.
		if err := h.db.ResetUnacknowledgedDispatchIntents(ctx, kioskID); err != nil {
			log.Printf("[HUB] Failed to reset unacknowledged intents for kiosk %s: %v", kioskID, err)
		} else if pendingJobs, err := h.db.GetPendingDispatchOrders(ctx, kioskID); err != nil {
			log.Printf("[HUB] Failed to reconcile pending intents for kiosk %s: %v", kioskID, err)
		} else if len(pendingJobs) > 0 {
			log.Printf("[HUB] Reconciling %d pending dispatch orders for kiosk %s", len(pendingJobs), kioskID)
			for _, jobCmd := range pendingJobs {
				if err := h.SendJob(ctx, jobCmd); err != nil {
					log.Printf("[HUB] Failed to dispatch pending job %s to kiosk %s: %v", jobCmd.JobID, kioskID, err)
					break
				}
				_ = h.db.MarkDispatchIntentSent(ctx, jobCmd.JobID)
				log.Printf("[HUB] Reconciled and dispatched pending job %s to kiosk %s", jobCmd.JobID, kioskID)
			}
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
			// Maintain liveness
		case "ack", "job_ack":
			jobID := ""
			if msg.Progress != nil {
				jobID = msg.Progress.JobID
			}
			if jobID == "" && msg.Job != nil {
				jobID = msg.Job.JobID
			}
			if msg.Progress != nil && msg.Progress.KioskID != kioskID {
				continue // fail closed: acknowledgement cannot claim a different kiosk
			}
			if jobID != "" && h.db != nil {
				if err := h.db.MarkDispatchAcknowledgedForKiosk(ctx, kioskID, jobID); err != nil {
					log.Printf("[HUB] Could not persist job ack %s for kiosk %s: %v", jobID, kioskID, err)
					continue
				}
				if msg.Progress != nil && isValidJobStatus(msg.Progress.Status) && !msg.Progress.Timestamp.IsZero() {
					if err := h.db.UpdateJobProgress(ctx, *msg.Progress); err == nil {
						h.onProgressFault(ctx, kioskID, *msg.Progress)
					}
				}
			}
		case "progress":
			if msg.Progress == nil {
				continue
			}
			if msg.Progress.KioskID != kioskID {
				// Reject mismatched kiosk progress
				continue
			}
			if strings.TrimSpace(msg.Progress.JobID) == "" || !isValidJobStatus(msg.Progress.Status) || msg.Progress.Timestamp.IsZero() {
				continue
			}
			if h.db != nil {
				if err := h.db.UpdateJobProgress(ctx, *msg.Progress); err == nil {
					h.onProgressFault(ctx, kioskID, *msg.Progress)
				}
			}
		case "health":
			if msg.Health == nil {
				continue
			}
			if !isValidHealthState(msg.Health.State) || !isValidTrayState(msg.Health.PaperTrayStatus) {
				continue
			}
			if msg.Health.TonerLevelPercent < 0 || msg.Health.TonerLevelPercent > 100 ||
				msg.Health.DrumLifePercent < 0 || msg.Health.DrumLifePercent > 100 {
				continue
			}
			h.healthMu.Lock()
			h.health[kioskID] = *msg.Health
			h.healthMu.Unlock()
			if h.db != nil && (msg.Health.PaperJam || msg.Health.CoverOpen || msg.Health.State == "ERROR" || msg.Health.OutputBinFull) {
				if err := h.db.BlockKiosk(ctx, kioskID, "printer hardware fault requires attendant clearance"); err != nil {
					log.Printf("[KIOSK] Failed to latch outage for %s: %v", kioskID, err)
				}
			}
		case "safe_stop":
			// Authenticated Pi attests that every CUPS job in this order has
			// reached a cancel/abort state or was never submitted.
			if h.db != nil && strings.TrimSpace(msg.OrderID) != "" {
				order, err := h.db.GetOrder(ctx, msg.OrderID)
				if err == nil && order != nil && order.Command.KioskID == kioskID {
					if err := h.db.ConfirmRefundSafeStop(ctx, msg.OrderID); err != nil {
						log.Printf("[REFUND] safe-stop persistence failed for %s: %v", msg.OrderID, err)
					}
				}
			}
		case "error":
			// Operational error
		default:
			// Ignore unexpected type
		}
	}
}

// SendJob dispatches a PrintJobCommand envelope to the connected kiosk.
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

	if strings.TrimSpace(cmd.JobID) == "" {
		return fmt.Errorf("send job: empty job ID")
	}
	// This is the final dispatch safety gate, including delayed webhooks and
	// reconnect reconciliation. Faulted/refunding orders must never be printed.
	if h.db != nil {
		blocked, reason, err := h.db.GetKioskBlock(ctx, cmd.KioskID)
		if err != nil { return fmt.Errorf("send job: verify kiosk service state: %w", err) }
		if blocked { return fmt.Errorf("send job: kiosk out of service: %s", reason) }
		if cmd.OrderID != "" {
			refund, err := h.db.GetRefundRequest(ctx, cmd.OrderID)
			if err != nil { return fmt.Errorf("send job: verify refund obligation: %w", err) }
			if refund != nil { return fmt.Errorf("send job: order %s has a pending or processed refund", cmd.OrderID) }
		}
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

	h.mu.RLock()
	sess, ok := h.sessions[cmd.KioskID]
	h.mu.RUnlock()

	if !ok || sess == nil {
		return fmt.Errorf("send job: kiosk %q not connected", cmd.KioskID)
	}

	msg := wireMessage{
		Type: "job",
		Job:  &cmd,
	}

	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()

	if err := wsjson.Write(ctx, sess.conn, msg); err != nil {
		// Force the broken session to exit its receive loop and reconnect.
		// Do not retry this possibly delivered frame on the same connection.
		_ = sess.conn.CloseNow()
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

// LatestHealth returns the most recent cached printer health observation for a kiosk.
func (h *Hub) LatestHealth(kioskID string) (contracts.PrinterHealth, bool) {
	if h == nil {
		return contracts.PrinterHealth{}, false
	}
	h.healthMu.RLock()
	defer h.healthMu.RUnlock()

	health, ok := h.health[kioskID]
	return health, ok
}

func isValidHealthState(state string) bool {
	switch state {
	case "IDLE", "PRINTING", "WARMING_UP", "ERROR", "OFFLINE":
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

// onProgressFault is intentionally fail-closed: a fault records the refund
// obligation and blocks new orders, but DOES NOT submit a monetary refund.
// Only a later authenticated safe_stop attestation unlocks provider processing.
func (h *Hub) onProgressFault(ctx context.Context, kioskID string, event contracts.JobProgressEvent) {
	if event.Status != contracts.JobStatusPausedError && event.Status != contracts.JobStatusFailedRefundable {
		return
	}
	order, err := h.db.GetOrderByJobID(ctx, event.JobID)
	if err != nil || order == nil { return }
	if err := h.db.BlockKiosk(ctx, kioskID, event.ErrorReason); err != nil {
		log.Printf("[KIOSK] could not latch outage for %s: %v", kioskID, err)
	}
	if err := h.db.RecordRefundRequired(ctx, order.OrderID); err != nil {
		log.Printf("[REFUND] could not record obligation for %s: %v", order.OrderID, err)
	}
}
