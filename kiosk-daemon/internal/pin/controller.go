package pin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

// State represents the current logical state of the PIN controller.
type State string

const (
	StateIdle           State = "IDLE"
	StatePINEntry       State = "PIN_ENTRY"
	StatePINLoading     State = "PIN_LOADING"
	StatePaymentPending State = "PAYMENT_PENDING"
)

// Config specifies runtime dependencies and timeout policies for the Controller.
type Config struct {
	InactivityTimeout time.Duration // Duration of inactivity before returning to IDLE from PIN_ENTRY or claim failure (default 30s)
	ErrorClearTimeout time.Duration // Duration before clearing PIN error back to clean state (default 5s)
	PaymentTimeout    time.Duration // Duration before returning to IDLE from PAYMENT_PENDING (default 120s)
	ClaimTimeout      time.Duration // Per-attempt timeout for acquiring claim slot or claiming an order (default 15s)
	CancelWaitTimeout time.Duration // Duration to wait for in-flight cancellation to resolve before failing safely (default 5s)
	CountdownInterval time.Duration // Tick interval for countdown timer (default 1s)
	RenderScreen      func(ui_ipc.ScreenState) error
	LookupClient      cloud.PINLookupClient
	IdleQRContent     string
	PaymentURLBase    string
	KioskID           string
	PrinterModel      string
	OnTransactionEnd  func()
	Now               func() time.Time // Optional clock injection; defaults to time.Now
}

type cancelTracker struct {
	sessionID string
	done      chan struct{}
	err       error
	attempts  int
	createdAt time.Time
	expiresAt time.Time
}

type orderClaimLock struct {
	mu   chan struct{}
	refs int
}

// Controller coordinates user keypad inputs, cloud lookup, timeouts, and UI rendering.
type Controller struct {
	cfg Config

	mu               sync.Mutex
	state            State
	pinBuffer        string
	activePIN        string
	pinError         string
	currentOrder     *cloud.OrderDetails
	currentSession   *cloud.SessionDetails
	currentAttempt   *cloud.PaymentAttemptDetails
	currentQRPayload string
	checkoutDeadline time.Time
	pendingCancels   map[string]*cancelTracker
	canceledSessions map[string]time.Time
	orderClaims      map[string]*orderClaimLock
	lookupSeq        uint64 // Monotonically increasing sequence to discard stale responses
	timerGen         uint64 // Guards against stale inactivity timer callbacks
	errorTimerGen    uint64 // Guards against stale error-clear timer callbacks
	isClaiming       bool
	isInitiating     bool

	inactivityTimer *time.Timer
	errorTimer      *time.Timer
	stopCountdown   chan struct{}

	inputSource input.Source
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewController creates and validates a new PIN Controller.
func NewController(cfg Config, inputSource input.Source) (*Controller, error) {
	if cfg.RenderScreen == nil {
		return nil, errors.New("pin controller: RenderScreen callback is required")
	}
	if cfg.LookupClient == nil {
		return nil, errors.New("pin controller: LookupClient is required")
	}
	if cfg.InactivityTimeout <= 0 {
		cfg.InactivityTimeout = 30 * time.Second
	}
	if cfg.ErrorClearTimeout <= 0 {
		cfg.ErrorClearTimeout = 5 * time.Second
	}
	if cfg.CountdownInterval <= 0 {
		cfg.CountdownInterval = 1 * time.Second
	}
	if cfg.PaymentTimeout <= 0 {
		cfg.PaymentTimeout = 120 * time.Second
	}
	if cfg.ClaimTimeout <= 0 {
		cfg.ClaimTimeout = 15 * time.Second
	}
	if cfg.CancelWaitTimeout <= 0 {
		cfg.CancelWaitTimeout = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Controller{
		cfg:              cfg,
		inputSource:      inputSource,
		state:            StateIdle,
		done:             make(chan struct{}),
		pendingCancels:   make(map[string]*cancelTracker),
		canceledSessions: make(map[string]time.Time),
		orderClaims:      make(map[string]*orderClaimLock),
	}, nil
}

// Start begins processing input events in the background.
func (c *Controller) Start(ctx context.Context) {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	go c.eventLoop()
}

// Stop stops the controller and releases timers.
func (c *Controller) Stop() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.stopInactivityTimerLocked()
	c.stopCountdownLocked()
	c.stopErrorTimerLocked()
	c.mu.Unlock()

	<-c.done
}

// GetState returns the current state of the controller (thread-safe).
func (c *Controller) GetState() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// GetPINBuffer returns the current PIN buffer (thread-safe).
func (c *Controller) GetPINBuffer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinBuffer
}

// GetPINError returns the current PIN error string (thread-safe).
func (c *Controller) GetPINError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinError
}

// HasActiveTransaction returns true if a user is actively in a PIN or payment flow.
func (c *Controller) HasActiveTransaction() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state != StateIdle
}

// ActiveOrderID returns the order ID associated with the active transaction, if any.
func (c *Controller) ActiveOrderID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeOrderIDLocked()
}

func (c *Controller) activeOrderIDLocked() string {
	if c.currentOrder != nil {
		return c.currentOrder.OrderID
	}
	if c.currentSession != nil {
		return c.currentSession.OrderID
	}
	return ""
}

// ActiveJobID returns the job ID associated with the active transaction, if any.
func (c *Controller) ActiveJobID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentOrder != nil {
		return "job_" + c.currentOrder.OrderID
	}
	if c.currentSession != nil {
		return "job_" + c.currentSession.OrderID
	}
	return ""
}

// GetCurrentSession returns the active session details (thread-safe).
func (c *Controller) GetCurrentSession() *cloud.SessionDetails {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentSession
}

func (c *Controller) now() time.Time {
	if c.cfg.Now != nil {
		return c.cfg.Now()
	}
	return time.Now()
}

// PendingCancelCount returns the current number of tracked in-flight or unresolved cancellations.
func (c *Controller) PendingCancelCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneCompletedTrackersLocked()
	return len(c.pendingCancels)
}

// CanceledSessionCount returns the current number of tracked canceled sessions.
func (c *Controller) CanceledSessionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneCompletedTrackersLocked()
	return len(c.canceledSessions)
}

func (c *Controller) isSessionCanceledLocked(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	exp, ok := c.canceledSessions[sessionID]
	if !ok {
		return false
	}
	if !c.now().Before(exp) {
		delete(c.canceledSessions, sessionID)
		return false
	}
	return true
}

func (c *Controller) isSessionPendingCancelLocked(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	tr, ok := c.pendingCancels[sessionID]
	if !ok {
		return false
	}
	if !c.now().Before(tr.expiresAt) {
		delete(c.pendingCancels, sessionID)
		return false
	}
	return true
}

// ResetToIdle resets the controller to StateIdle and re-renders the idle screen.
func (c *Controller) ResetToIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetToIdleLocked()
}

func (c *Controller) resetToIdleLocked() {
	c.pruneCompletedTrackersLocked()
	c.stopInactivityTimerLocked()
	c.stopCountdownLocked()
	c.stopErrorTimerLocked()
	atomic.AddUint64(&c.lookupSeq, 1) // Invalidate any inflight lookups
	c.state = StateIdle
	c.pinBuffer = ""
	c.pinError = ""
	c.isClaiming = false
	c.isInitiating = false
	c.currentOrder = nil
	c.currentSession = nil
	c.currentAttempt = nil
	c.currentQRPayload = ""
	c.checkoutDeadline = time.Time{}

	_ = c.cfg.RenderScreen(ui_ipc.ScreenState{
		Mode:         ui_ipc.ModeIdle,
		QRContent:    c.cfg.IdleQRContent,
		Message:      "Ready to print",
		PrinterModel: c.cfg.PrinterModel,
	})

	if c.cfg.OnTransactionEnd != nil {
		onEnd := c.cfg.OnTransactionEnd
		go onEnd()
	}
}

func (c *Controller) eventLoop() {
	defer close(c.done)

	if c.inputSource == nil {
		return
	}

	events := c.inputSource.Events()
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			c.handleEvent(ev)
		}
	}
}

func (c *Controller) handleEvent(ev input.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case StateIdle:
		c.handleIdleEvent(ev)
	case StatePINEntry:
		c.handlePINEntryEvent(ev)
	case StatePINLoading:
		c.handlePINLoadingEvent(ev)
	case StatePaymentPending:
		c.handlePaymentPendingEvent(ev)
	}
}

func (c *Controller) handleIdleEvent(ev input.Event) {
	if ev.Type == input.ActionDigit {
		// First digit pressed: switch to PIN entry screen
		c.state = StatePINEntry
		c.pinBuffer = string(ev.Digit)
		c.pinError = ""
		log.Printf("[KEYPAD] Digit pressed: %c | Buffer: %s", ev.Digit, c.pinBuffer)
		c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
		c.renderPINEntryLocked()
	}
}

func (c *Controller) handlePINEntryEvent(ev input.Event) {
	c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)

	switch ev.Type {
	case input.ActionDigit:
		if len(c.pinBuffer) < 4 {
			c.stopErrorTimerLocked()
			c.pinBuffer += string(ev.Digit)
			c.pinError = ""
			log.Printf("[KEYPAD] Digit pressed: %c | Buffer: %s", ev.Digit, c.pinBuffer)
			c.renderPINEntryLocked()
		}
	case input.ActionBackspace:
		if len(c.pinBuffer) > 0 {
			c.stopErrorTimerLocked()
			c.pinBuffer = c.pinBuffer[:len(c.pinBuffer)-1]
			c.pinError = ""
			log.Printf("[KEYPAD] Backspace | Buffer: %s", c.pinBuffer)
			c.renderPINEntryLocked()
		}
	case input.ActionClear:
		c.stopErrorTimerLocked()
		c.pinBuffer = ""
		c.pinError = ""
		log.Printf("[KEYPAD] Cleared | Buffer reset")
		c.renderPINEntryLocked()
	case input.ActionCancel:
		c.stopErrorTimerLocked()
		log.Printf("[KEYPAD] Canceled | Returning to IDLE")
		c.resetToIdleLocked()
	case input.ActionSubmit:
		if len(c.pinBuffer) != 4 {
			log.Printf("[KEYPAD] Submit rejected: PIN must be 4 digits (got '%s')", c.pinBuffer)
			c.setPINErrorLocked("PIN must be exactly 4 digits")
			c.renderPINEntryLocked()
			return
		}
		log.Printf("[KEYPAD] Submitting PIN: %s", c.pinBuffer)
		c.startLookupLocked()
	}
}

func (c *Controller) handlePINLoadingEvent(ev input.Event) {
	// While loading, only Cancel is accepted to abort and return to idle
	if ev.Type == input.ActionCancel {
		c.resetToIdleLocked()
	}
}

func (c *Controller) acquireOrderClaimEntryLocked(orderID string) *orderClaimLock {
	if c.orderClaims == nil {
		c.orderClaims = make(map[string]*orderClaimLock)
	}
	entry, ok := c.orderClaims[orderID]
	if !ok {
		entry = &orderClaimLock{
			mu: make(chan struct{}, 1),
		}
		entry.mu <- struct{}{}
		c.orderClaims[orderID] = entry
	}
	entry.refs++
	return entry
}

func (c *Controller) releaseOrderClaimEntry(orderID string, entry *orderClaimLock) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry == nil {
		return
	}
	entry.refs--
	if entry.refs <= 0 && c.orderClaims[orderID] == entry {
		delete(c.orderClaims, orderID)
	}
}

func (c *Controller) pruneCompletedTrackersLocked() {
	now := c.now()
	for id, tr := range c.pendingCancels {
		select {
		case <-tr.done:
			if tr.err == nil {
				delete(c.pendingCancels, id)
			} else if !now.Before(tr.expiresAt) {
				delete(c.pendingCancels, id)
			}
		default:
			if !now.Before(tr.expiresAt) {
				delete(c.pendingCancels, id)
			}
		}
	}
	for id, exp := range c.canceledSessions {
		if !now.Before(exp) {
			delete(c.canceledSessions, id)
		}
	}
}

func (c *Controller) cancelSessionAsyncLocked(sessID string) {
	if sessID == "" {
		return
	}
	c.pruneCompletedTrackersLocked()

	timeout := c.cfg.PaymentTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	expiresAt := c.now().Add(timeout)

	c.canceledSessions[sessID] = expiresAt

	if existing, ok := c.pendingCancels[sessID]; ok {
		select {
		case <-existing.done:
			// Existing tracker completed previously; allow re-attempt
		default:
			// In-flight cancellation is already running for this session
			return
		}
	}

	tracker := &cancelTracker{
		sessionID: sessID,
		done:      make(chan struct{}),
		createdAt: c.now(),
		expiresAt: expiresAt,
	}
	c.pendingCancels[sessID] = tracker
	go func(sID string, tr *cancelTracker, exp time.Time) {
		defer close(tr.done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.cfg.LookupClient.CancelSession(ctx, sID)
		c.mu.Lock()
		tr.err = err
		if err == nil {
			delete(c.pendingCancels, sID)
			c.canceledSessions[sID] = exp
		}
		c.mu.Unlock()
		if err != nil {
			log.Printf("[PIN-CONTROLLER] Background cancel failed for session %s: %v", sID, err)
		} else {
			log.Printf("[PIN-CONTROLLER] Background cancel completed for session %s", sID)
		}
	}(sessID, tracker, expiresAt)
}

func (c *Controller) handlePaymentPendingEvent(ev input.Event) {
	switch ev.Type {
	case input.ActionCancel:
		if c.currentSession != nil {
			c.cancelSessionAsyncLocked(c.currentSession.SessionID)
		}
		c.resetToIdleLocked()
	case input.ActionSubmit:
		// Ignore repeated Enter presses while claim, release, or payment initiation is already in flight
		if c.isClaiming || c.isInitiating {
			log.Printf("[PIN-CONTROLLER] Keypad Confirm ignored while checkout request is in flight for order %s", c.activeOrderIDLocked())
			return
		}
		// Ignore Enter when a valid QR is already displayed
		if c.currentQRPayload != "" {
			log.Printf("[PIN-CONTROLLER] Keypad Confirm ignored on payment QR screen for order %s (scan QR code to pay)", c.activeOrderIDLocked())
			return
		}
		// Retry claiming when claim failed before a session was obtained (allowed for both unpaid and PAID_WAITING orders)
		if c.currentSession == nil && c.pinError != "" {
			log.Printf("[PIN-CONTROLLER] Retrying claim for order %s", c.activeOrderIDLocked())
			c.startClaimLocked()
			return
		}
		// Never initiate payment or retry print release on an already-paid order after a session was obtained
		if c.isPaidOrderLocked() {
			log.Printf("[PIN-CONTROLLER] Keypad Confirm ignored for already-paid order %s", c.activeOrderIDLocked())
			return
		}
		// Retry payment initiation against the existing session without extending checkout deadline
		if c.pinError != "" && c.currentSession != nil {
			if c.isCheckoutExpiredLocked() {
				log.Printf("[PIN-CONTROLLER] Checkout deadline expired before retry for session %s; returning to idle", c.currentSession.SessionID)
				c.expireCheckoutLocked()
				return
			}
			log.Printf("[PIN-CONTROLLER] Retrying payment initiation for session %s", c.currentSession.SessionID)
			c.isInitiating = true
			c.pinError = ""
			remSec := c.checkoutRemainingSecondsLocked()
			c.renderPaymentPendingLocked(remSec)
			seq := atomic.AddUint64(&c.lookupSeq, 1)
			sID := c.currentSession.SessionID
			baseCtx := c.ctx
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			go c.initiatePaymentAsync(baseCtx, seq, sID, 1)
			return
		}
		log.Printf("[PIN-CONTROLLER] Keypad Confirm ignored on payment QR screen for order %s (scan QR code to pay)", c.activeOrderIDLocked())
	}
}

func (c *Controller) canRetryConfirmLocked() bool {
	if c.state != StatePaymentPending || c.isClaiming || c.isInitiating || c.currentQRPayload != "" || c.pinError == "" {
		return false
	}
	if c.currentSession == nil {
		return true
	}
	if c.isPaidOrderLocked() {
		return false
	}
	return !c.isCheckoutExpiredLocked()
}

func (c *Controller) isPaidOrderLocked() bool {
	if c.currentOrder != nil && c.currentOrder.Status == string(contracts.JobStatusPaidWaiting) {
		return true
	}
	if c.currentSession != nil && c.currentSession.Status == string(contracts.JobStatusPaidWaiting) {
		return true
	}
	return false
}

func (c *Controller) resolveSessionDeadline(session *cloud.SessionDetails) (time.Time, bool) {
	if session == nil || session.SecondsRemaining <= 0 {
		return time.Time{}, false
	}
	expiresRaw := strings.TrimSpace(session.ExpiresAt)
	if expiresRaw == "" {
		return time.Time{}, false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		return time.Time{}, false
	}

	now := c.now()
	if !expiresAt.After(now) {
		return time.Time{}, false
	}

	deadline := expiresAt
	maxLocal := c.cfg.PaymentTimeout
	if maxLocal <= 0 {
		maxLocal = 120 * time.Second
	}
	if localCap := now.Add(maxLocal); localCap.Before(deadline) {
		deadline = localCap
	}
	if reportedCap := now.Add(time.Duration(session.SecondsRemaining) * time.Second); reportedCap.Before(deadline) {
		deadline = reportedCap
	}
	if !deadline.After(now) {
		return time.Time{}, false
	}
	return deadline, true
}

func (c *Controller) checkoutRemainingLocked() time.Duration {
	if c.checkoutDeadline.IsZero() {
		return 0
	}
	rem := c.checkoutDeadline.Sub(c.now())
	if rem <= 0 {
		return 0
	}
	return rem
}

func (c *Controller) checkoutRemainingSecondsLocked() int {
	rem := c.checkoutRemainingLocked()
	if rem <= 0 {
		return 0
	}
	return int(math.Ceil(rem.Seconds()))
}

func (c *Controller) isCheckoutExpiredLocked() bool {
	return c.checkoutDeadline.IsZero() || !c.checkoutDeadline.After(c.now())
}

func (c *Controller) expireCheckoutLocked() {
	if c.currentSession != nil {
		c.cancelSessionAsyncLocked(c.currentSession.SessionID)
	}
	c.resetToIdleLocked()
}

func (c *Controller) initiatePaymentAsync(baseCtx context.Context, targetSeq uint64, sID string, maxAttempts int) {
	var attempt *cloud.PaymentAttemptDetails
	var pErr error

	for try := 0; try < maxAttempts; try++ {
		if try > 0 {
			c.mu.Lock()
			remForRetry := c.checkoutRemainingLocked()
			c.mu.Unlock()
			if remForRetry <= 2*time.Second {
				break
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-baseCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		c.mu.Lock()
		curSeq := atomic.LoadUint64(&c.lookupSeq)
		curState := c.state
		curSess := c.currentSession
		remDur := c.checkoutRemainingLocked()
		c.mu.Unlock()
		if curSeq != targetSeq || curState != StatePaymentPending || curSess == nil || curSess.SessionID != sID {
			return
		}
		if remDur <= 0 {
			break
		}

		attemptTimeout := 15 * time.Second
		if remDur < attemptTimeout {
			attemptTimeout = remDur
		}
		initCtx, initCancel := context.WithTimeout(baseCtx, attemptTimeout)
		attempt, pErr = c.cfg.LookupClient.InitiatePayment(initCtx, sID)
		initCancel()

		if pErr == nil && attempt != nil {
			break
		}
		log.Printf("[PIN-CONTROLLER] InitiatePayment attempt %d failed: %v", try+1, pErr)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending || c.currentSession == nil || c.currentSession.SessionID != sID {
		return
	}
	c.isInitiating = false

	if c.isCheckoutExpiredLocked() {
		log.Printf("[PIN-CONTROLLER] Checkout deadline expired during InitiatePayment for session %s; discarding response", sID)
		c.expireCheckoutLocked()
		return
	}

	rem := c.checkoutRemainingSecondsLocked()
	if pErr != nil || attempt == nil {
		log.Printf("[PIN-CONTROLLER] InitiatePayment failed for session %s: %v", sID, pErr)
		c.pinError = "Failed to load payment QR. Press Confirm to retry."
		c.currentQRPayload = ""
		c.renderPaymentPendingLocked(rem)
		return
	}

	qrPayload := c.resolvePaymentQRPayload(attempt)
	if qrPayload == "" {
		log.Printf("[PIN-CONTROLLER] Missing or invalid demo payment token for session %s (order %s)", sID, c.activeOrderIDLocked())
		c.currentAttempt = attempt
		c.currentQRPayload = ""
		c.pinError = "Failed to load payment QR. Press Confirm to retry."
		c.renderPaymentPendingLocked(rem)
		return
	}

	c.currentAttempt = attempt
	c.currentQRPayload = qrPayload
	c.pinError = ""
	log.Printf("[PAYMENT] Demo checkout QR displayed for order %s", c.activeOrderIDLocked())
	c.renderPaymentPendingLocked(rem)
}

func mapReleaseError(err error) string {
	switch {
	case errors.Is(err, cloud.ErrInsufficientPaper):
		return "Paper tray supply is low or empty. Please ask attendant to refill paper."
	case errors.Is(err, cloud.ErrPrinterOffline):
		return "Printer is offline or disconnected. Please ask attendant for assistance."
	case errors.Is(err, cloud.ErrPrinterError):
		return "Printer hardware error. Please ask attendant for assistance."
	case errors.Is(err, cloud.ErrOrderExpired), errors.Is(err, cloud.ErrInvalidOrExpiredPIN), errors.Is(err, cloud.ErrSessionNotFound):
		return "Print session expired. Please press Esc and try again."
	case errors.Is(err, cloud.ErrUnauthorized):
		return "Kiosk authentication error. Please see attendant."
	default:
		var netErr *cloud.NetworkError
		if errors.As(err, &netErr) {
			return "Network error connecting to cloud. Please ask attendant or press Esc to retry."
		}
		return "Unable to release print job. Please ask attendant for assistance."
	}
}

func mapClaimError(err error) string {
	switch {
	case errors.Is(err, cloud.ErrInsufficientPaper):
		return "Paper tray supply is low or empty. Please ask attendant to refill paper and press Confirm."
	case errors.Is(err, cloud.ErrPrinterOffline):
		return "Printer is offline or disconnected. Please ask attendant for assistance."
	case errors.Is(err, cloud.ErrPrinterError):
		return "Printer hardware error. Please ask attendant for assistance."
	case errors.Is(err, cloud.ErrOrderAlreadyClaimed):
		return "Order is already claimed by an active transaction."
	case errors.Is(err, cloud.ErrOrderExpired):
		return "Order has expired. Please create a new order."
	case errors.Is(err, cloud.ErrUnauthorized):
		return "Kiosk authentication error. Please see attendant."
	default:
		var netErr *cloud.NetworkError
		if errors.As(err, &netErr) {
			return "Network error connecting to cloud. Please try again."
		}
		return "Unable to start payment session. Please try again."
	}
}

func (c *Controller) isSessionExpired(session *cloud.SessionDetails) bool {
	if session == nil {
		return false
	}
	_, ok := c.resolveSessionDeadline(session)
	return !ok
}

func (c *Controller) reclaimAndVerifySession(ctx context.Context, ordID string, canceledID string) (*cloud.SessionDetails, error) {
	reclaimCtx, reclaimCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reclaimCancel()
	session, err := c.cfg.LookupClient.ClaimOrder(reclaimCtx, ordID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, errors.New("received nil session from claim")
	}
	if c.isSessionExpired(session) {
		log.Printf("[PIN-CONTROLLER] Reclaimed session %s returned non-positive SecondsRemaining (%d); rejecting", session.SessionID, session.SecondsRemaining)
		return nil, cloud.ErrOrderExpired
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	stillPending := c.isSessionPendingCancelLocked(session.SessionID)
	stillCanceled := c.isSessionCanceledLocked(session.SessionID)
	if stillPending || stillCanceled || session.SessionID == canceledID {
		log.Printf("[PIN-CONTROLLER] Reclaimed session %s matches canceled session %s; rejecting fail-closed", session.SessionID, canceledID)
		return nil, errors.New("received unresolved session; please try again")
	}
	return session, nil
}

func (c *Controller) startClaimLocked() {
	if c.currentOrder == nil {
		c.resetToIdleLocked()
		return
	}

	c.state = StatePaymentPending
	c.isClaiming = true
	c.isInitiating = false
	c.pinError = ""
	c.currentSession = nil
	c.currentAttempt = nil
	c.currentQRPayload = ""
	c.checkoutDeadline = time.Time{}

	c.stopInactivityTimerLocked()
	c.stopCountdownLocked()
	c.renderPaymentPendingLocked(0)

	orderID := c.currentOrder.OrderID
	seq := atomic.AddUint64(&c.lookupSeq, 1)
	claimEntry := c.acquireOrderClaimEntryLocked(orderID)
	baseCtx := c.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	go func(targetSeq uint64, ordID string, bCtx context.Context, entry *orderClaimLock) {
		defer c.releaseOrderClaimEntry(ordID, entry)

		claimTimeout := c.cfg.ClaimTimeout
		if claimTimeout <= 0 {
			claimTimeout = 15 * time.Second
		}
		slotCtx, slotCancel := context.WithTimeout(bCtx, claimTimeout)
		defer slotCancel()

		// Acquire per-order binary semaphore; timed-out waiters never release another claim's barrier.
		select {
		case <-entry.mu:
			defer func() {
				entry.mu <- struct{}{}
			}()
		case <-slotCtx.Done():
			c.mu.Lock()
			if atomic.LoadUint64(&c.lookupSeq) == targetSeq && c.state == StatePaymentPending {
				c.isClaiming = false
				c.pinError = mapClaimError(slotCtx.Err())
				c.renderPaymentPendingLocked(0)
				c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
			}
			c.mu.Unlock()
			return
		}

		c.mu.Lock()
		alreadyStale := atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending
		c.mu.Unlock()
		if alreadyStale {
			return
		}

		ctx, cancel := context.WithTimeout(bCtx, claimTimeout)
		defer cancel()

		session, err := c.cfg.LookupClient.ClaimOrder(ctx, ordID)

		if err == nil && session != nil && c.isSessionExpired(session) {
			log.Printf("[PIN-CONTROLLER] ClaimOrder returned session %s with invalid or expired deadline (expires_at=%q, remaining=%d); rejecting", session.SessionID, session.ExpiresAt, session.SecondsRemaining)
			err = cloud.ErrOrderExpired
			session = nil
		}

		if err == nil && session != nil {
			c.mu.Lock()
			c.pruneCompletedTrackersLocked()
			tracker, hasPending := c.pendingCancels[session.SessionID]
			isCanceled := c.isSessionCanceledLocked(session.SessionID)
			c.mu.Unlock()
			if hasPending {
				waitDuration := c.cfg.CancelWaitTimeout
				if waitDuration <= 0 {
					waitDuration = 5 * time.Second
				}
				waitTimer := time.NewTimer(waitDuration)
				defer waitTimer.Stop()

				var cancelCompleted bool
				select {
				case <-tracker.done:
					cancelCompleted = true
				case <-waitTimer.C:
					log.Printf("[PIN-CONTROLLER] Cancellation timed out for session %s", session.SessionID)
					tracker.attempts++
					err = errors.New("previous session cancellation timed out; please try again")
					session = nil
				case <-ctx.Done():
					log.Printf("[PIN-CONTROLLER] Context expired while waiting for session %s cancellation", session.SessionID)
					tracker.attempts++
					err = errors.New("previous session cancellation timed out; please try again")
					session = nil
				}

				if cancelCompleted {
					if tracker.err != nil {
						// Previous cancellation failed. If customer is retrying (tracker.attempts > 0),
						// safely reattempt cancellation after connectivity recovers.
						if tracker.attempts > 0 {
							log.Printf("[PIN-CONTROLLER] Customer retry: reattempting cancellation for session %s", session.SessionID)
							retryCtx, retryCancel := context.WithTimeout(ctx, waitDuration)
							retryErr := c.cfg.LookupClient.CancelSession(retryCtx, session.SessionID)
							retryCancel()
							if retryErr != nil {
								log.Printf("[PIN-CONTROLLER] Cancellation reattempt failed for session %s: %v", session.SessionID, retryErr)
								tracker.err = retryErr
								err = errors.New("previous session cancellation failed; please try again")
								session = nil
							} else {
								log.Printf("[PIN-CONTROLLER] Cancellation reattempt succeeded for session %s", session.SessionID)
								tracker.err = nil
								canceledID := session.SessionID
								c.mu.Lock()
								delete(c.pendingCancels, canceledID)
								c.canceledSessions[canceledID] = c.now().Add(c.cfg.PaymentTimeout)
								c.mu.Unlock()

								session, err = c.reclaimAndVerifySession(ctx, ordID, canceledID)
							}
						} else {
							tracker.attempts++
							log.Printf("[PIN-CONTROLLER] Cancellation failed for session %s: %v", session.SessionID, tracker.err)
							err = errors.New("previous session cancellation failed; please try again")
							session = nil
						}
					} else {
						// Cancellation completed successfully
						canceledID := session.SessionID
						c.mu.Lock()
						delete(c.pendingCancels, canceledID)
						c.canceledSessions[canceledID] = c.now().Add(c.cfg.PaymentTimeout)
						c.mu.Unlock()

						session, err = c.reclaimAndVerifySession(ctx, ordID, canceledID)
					}
				}
			} else if isCanceled {
				// Recognized as a previously canceled session even if tracker was already cleaned up
				canceledID := session.SessionID
				log.Printf("[PIN-CONTROLLER] Stale ClaimOrder response returned canceled session %s; requesting fresh checkout", canceledID)
				session, err = c.reclaimAndVerifySession(ctx, ordID, canceledID)
			}
		}

		var deadline time.Time
		if err == nil && session != nil {
			var deadlineOK bool
			deadline, deadlineOK = c.resolveSessionDeadline(session)
			if !deadlineOK {
				log.Printf("[PIN-CONTROLLER] Session %s has invalid or expired deadline (expires_at=%q, remaining=%d); rejecting without starting checkout", session.SessionID, session.ExpiresAt, session.SecondsRemaining)
				err = cloud.ErrOrderExpired
				session = nil
			}
		}

		c.mu.Lock()
		if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending {
			if err == nil && session != nil {
				if c.currentSession == nil || c.currentSession.SessionID != session.SessionID {
					log.Printf("[PIN-CONTROLLER] Stale ClaimOrder returned session %s after cancel/reset; canceling", session.SessionID)
					c.cancelSessionAsyncLocked(session.SessionID)
				}
			}
			c.mu.Unlock()
			return
		}

		if err != nil || session == nil {
			c.isClaiming = false
			log.Printf("[PIN-CONTROLLER] Failed to claim order %s: %v", ordID, err)
			c.pinError = mapClaimError(err)
			c.currentSession = nil
			c.checkoutDeadline = time.Time{}
			c.renderPaymentPendingLocked(0)
			c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
			c.mu.Unlock()
			return
		}

		isPaidOrder := (c.currentOrder != nil && c.currentOrder.Status == string(contracts.JobStatusPaidWaiting)) ||
			session.Status == string(contracts.JobStatusPaidWaiting)
		if isPaidOrder {
			c.currentSession = session
			c.checkoutDeadline = deadline
			relTimeout := deadline.Sub(c.now())
			if relTimeout <= 0 {
				log.Printf("[PIN-CONTROLLER] Session %s deadline already passed before paid-order release for order %s; expiring immediately", session.SessionID, ordID)
				c.expireCheckoutLocked()
				c.mu.Unlock()
				return
			}
			if relTimeout > 10*time.Second {
				relTimeout = 10 * time.Second
			}
			c.mu.Unlock()

			log.Printf("[PIN-CONTROLLER] Order %s is already PAID_WAITING; releasing physical print with session %s", ordID, session.SessionID)
			relCtx, relCancel := context.WithTimeout(bCtx, relTimeout)
			relErr := c.cfg.LookupClient.ReleaseOrder(relCtx, ordID, session.SessionID)
			relCancel()

			c.mu.Lock()
			if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending {
				if relErr != nil && (c.currentSession == nil || c.currentSession.SessionID != session.SessionID) {
					c.cancelSessionAsyncLocked(session.SessionID)
				}
				c.mu.Unlock()
				return
			}
			c.isClaiming = false

			if relErr != nil {
				log.Printf("[PIN-CONTROLLER] Failed to release paid order %s (session %s): %v", ordID, session.SessionID, relErr)
				c.currentSession = session
				c.checkoutDeadline = deadline
				c.currentQRPayload = ""
				rem := c.checkoutRemainingLocked()
				if rem <= 0 {
					log.Printf("[PIN-CONTROLLER] Session %s deadline passed after paid-order release failure for order %s; expiring immediately", session.SessionID, ordID)
					c.expireCheckoutLocked()
					c.mu.Unlock()
					return
				}
				c.pinError = mapReleaseError(relErr)
				inactivityDur := c.cfg.InactivityTimeout
				if inactivityDur <= 0 || rem < inactivityDur {
					inactivityDur = rem
				}
				c.renderPaymentPendingLocked(c.checkoutRemainingSecondsLocked())
				c.startCountdownWithTimeoutLocked(inactivityDur)
				c.mu.Unlock()
				return
			}

			log.Printf("[PIN-CONTROLLER] Released paid order %s using session %s", ordID, session.SessionID)
			c.resetToIdleLocked()
			c.mu.Unlock()
			return
		}

		c.isClaiming = false
		c.currentSession = session
		c.checkoutDeadline = deadline
		c.state = StatePaymentPending
		c.isInitiating = true
		c.pinError = ""
		c.currentQRPayload = c.resolvePaymentQRPayload(nil)

		timeoutSec := c.checkoutRemainingSecondsLocked()
		c.renderPaymentPendingLocked(timeoutSec)
		c.startCountdownLocked()

		// Asynchronously initiate payment to obtain dynamic payment QR payload with retry
		sessID := session.SessionID
		go c.initiatePaymentAsync(bCtx, targetSeq, sessID, 3)
		c.mu.Unlock()
	}(seq, orderID, baseCtx, claimEntry)
}

func (c *Controller) startLookupLocked() {
	c.state = StatePINLoading
	c.pinError = ""
	c.stopInactivityTimerLocked()

	seq := atomic.AddUint64(&c.lookupSeq, 1)
	pinToLookup := c.pinBuffer
	c.activePIN = pinToLookup

	c.renderPINEntryLocked()

	// Launch async cloud lookup
	go func(targetSeq uint64, pin string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		order, err := c.cfg.LookupClient.LookupOrderByPIN(ctx, pin)

		c.mu.Lock()
		defer c.mu.Unlock()

		// Discard stale or canceled lookups
		if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePINLoading {
			return
		}

		if err == nil && order == nil {
			err = errors.New("received nil order")
		}

		if err != nil {
			c.state = StatePINEntry
			c.pinBuffer = "" // Clear buffer so user enters fresh PIN
			var rle *cloud.RateLimitError
			var ne *cloud.NetworkError
			var errMsg string

			switch {
			case errors.Is(err, cloud.ErrInvalidOrExpiredPIN):
				errMsg = "Invalid or expired PIN. Please try again."
			case errors.Is(err, cloud.ErrUnauthorized):
				errMsg = "Kiosk authentication error. Please see attendant."
			case errors.As(err, &rle):
				if rle.RetryAfterSeconds > 0 {
					errMsg = fmt.Sprintf("Too many attempts. Please wait %d seconds.", rle.RetryAfterSeconds)
				} else {
					errMsg = "Too many failed attempts. Please wait a moment."
				}
			case errors.As(err, &ne):
				errMsg = "Network error connecting to cloud. Please try again."
			default:
				errMsg = "Lookup failed. Please check PIN and try again."
			}

			c.setPINErrorLocked(errMsg)
			c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
			c.renderPINEntryLocked()
			return
		}

		// Direct PIN-to-Checkout: automatically claim an authoritative session for both unpaid and PAID_WAITING orders
		c.currentOrder = order
		c.startClaimLocked()
	}(seq, pinToLookup)
}

func (c *Controller) renderPINEntryLocked() {
	remSeconds := 0
	if c.inactivityTimer != nil {
		remSeconds = int(c.cfg.InactivityTimeout.Seconds())
	}

	_ = c.cfg.RenderScreen(ui_ipc.ScreenState{
		Mode:           ui_ipc.ModePINEntry,
		PINDigits:      c.pinBuffer,
		PINLoading:     c.state == StatePINLoading,
		PINError:       c.pinError,
		TimeoutSeconds: remSeconds,
		PrinterModel:   c.cfg.PrinterModel,
	})
}

func isValidDemoToken(token string) bool {
	if len(token) != 52 || !strings.HasPrefix(token, "dmo_") {
		return false
	}
	hexPart := token[4:]
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (c *Controller) resolvePaymentQRPayload(attempt *cloud.PaymentAttemptDetails) string {
	if attempt == nil {
		return ""
	}
	token := strings.TrimSpace(attempt.DemoToken)
	if !isValidDemoToken(token) {
		return ""
	}

	rawBase := strings.TrimSpace(c.cfg.PaymentURLBase)
	if rawBase != "" {
		parsedBase, err := url.Parse(rawBase)
		if err != nil || (parsedBase.Scheme != "http" && parsedBase.Scheme != "https") || parsedBase.Host == "" {
			return ""
		}
		if strings.TrimRight(parsedBase.Path, "/") != "/pay" {
			return ""
		}
		if parsedBase.RawQuery != "" || parsedBase.Fragment != "" {
			return ""
		}
		return fmt.Sprintf("%s/%s", strings.TrimRight(rawBase, "/"), token)
	}

	rawCheckout := strings.TrimSpace(attempt.QRPayload)
	if rawCheckout == "" {
		return ""
	}
	parsedCheckout, err := url.Parse(rawCheckout)
	if err != nil || (parsedCheckout.Scheme != "http" && parsedCheckout.Scheme != "https") || parsedCheckout.Host == "" {
		return ""
	}
	if parsedCheckout.Path != "/pay/"+token {
		return ""
	}
	if parsedCheckout.RawQuery != "" || parsedCheckout.Fragment != "" {
		return ""
	}
	return rawCheckout
}

func (c *Controller) renderPaymentPendingLocked(remainingSeconds int) {
	if c.currentOrder == nil {
		return
	}

	summary := &ui_ipc.OrderSummary{
		OrderID:         c.currentOrder.OrderID,
		TotalPricePaise: c.currentOrder.TotalPricePaise,
		TotalPages:      c.currentOrder.TotalPages,
		TotalSheets:     c.currentOrder.TotalSheets,
		Copies:          c.currentOrder.Copies,
		Duplex:          c.currentOrder.Duplex,
		ColorMode:       c.currentOrder.ColorMode,
	}
	sessID := ""
	if c.currentSession != nil {
		sessID = c.currentSession.SessionID
		if c.currentSession.TotalPricePaise > 0 {
			summary.TotalPricePaise = c.currentSession.TotalPricePaise
		}
	}

	printReleaseFailed := c.pinError != "" && c.currentSession != nil && c.isPaidOrderLocked()

	_ = c.cfg.RenderScreen(ui_ipc.ScreenState{
		Mode:               ui_ipc.ModePaymentPending,
		OrderSummary:       summary,
		SessionID:          sessID,
		QRContent:          c.currentQRPayload,
		PINLoading:         c.isClaiming,
		PINError:           c.pinError,
		PINErrorRetry:      c.canRetryConfirmLocked(),
		PrintReleaseFailed: printReleaseFailed,
		TimeoutSeconds:     remainingSeconds,
		PrinterModel:       c.cfg.PrinterModel,
	})
}

func (c *Controller) resetInactivityTimerLocked(d time.Duration) {
	c.stopInactivityTimerLocked()
	gen := atomic.AddUint64(&c.timerGen, 1)
	c.inactivityTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if atomic.LoadUint64(&c.timerGen) != gen {
			return
		}
		if c.state == StatePINEntry || c.state == StatePaymentPending {
			log.Printf("[PIN-CONTROLLER] Inactivity timeout reached, returning to idle screen")
			if c.currentSession != nil {
				c.cancelSessionAsyncLocked(c.currentSession.SessionID)
			}
			c.resetToIdleLocked()
		}
	})
}

func (c *Controller) stopInactivityTimerLocked() {
	atomic.AddUint64(&c.timerGen, 1)
	if c.inactivityTimer != nil {
		c.inactivityTimer.Stop()
		c.inactivityTimer = nil
	}
}

func (c *Controller) setPINErrorLocked(errStr string) {
	c.pinError = errStr
	c.stopErrorTimerLocked()
	timeout := c.cfg.ErrorClearTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	gen := atomic.AddUint64(&c.errorTimerGen, 1)
	c.errorTimer = time.AfterFunc(timeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if atomic.LoadUint64(&c.errorTimerGen) != gen {
			return
		}
		if c.state == StatePINEntry && c.pinError != "" {
			log.Printf("[PIN-CONTROLLER] Error clear timeout reached, reverting to clean PIN view")
			c.pinError = ""
			c.pinBuffer = ""
			c.renderPINEntryLocked()
		}
	})
}

func (c *Controller) stopErrorTimerLocked() {
	atomic.AddUint64(&c.errorTimerGen, 1)
	if c.errorTimer != nil {
		c.errorTimer.Stop()
		c.errorTimer = nil
	}
}

func (c *Controller) startCountdownLocked() {
	c.startCountdownWithTimeoutLocked(c.checkoutRemainingLocked())
}

func (c *Controller) startCountdownWithTimeoutLocked(inactivityDur time.Duration) {
	c.stopCountdownLocked()
	if c.checkoutRemainingLocked() <= 0 || inactivityDur <= 0 {
		c.expireCheckoutLocked()
		return
	}
	c.stopCountdown = make(chan struct{})
	c.resetInactivityTimerLocked(inactivityDur)
	baseCtx := c.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	go func(stopCh chan struct{}, bCtx context.Context) {
		ticker := time.NewTicker(c.cfg.CountdownInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-bCtx.Done():
				return
			case <-ticker.C:
				c.mu.Lock()
				select {
				case <-stopCh:
					c.mu.Unlock()
					return
				default:
				}
				if c.state != StatePaymentPending || c.currentSession == nil {
					c.mu.Unlock()
					return
				}
				rem := c.checkoutRemainingSecondsLocked()
				if rem <= 0 {
					c.expireCheckoutLocked()
					c.mu.Unlock()
					return
				}
				c.renderPaymentPendingLocked(rem)
				c.mu.Unlock()
			}
		}
	}(c.stopCountdown, baseCtx)
}

func (c *Controller) stopCountdownLocked() {
	if c.stopCountdown != nil {
		close(c.stopCountdown)
		c.stopCountdown = nil
	}
}
