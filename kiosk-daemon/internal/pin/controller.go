package pin

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	StateOrderSummary   State = "ORDER_SUMMARY"
	StatePaymentPending State = "PAYMENT_PENDING"
)

// Config specifies runtime dependencies and timeout policies for the Controller.
type Config struct {
	InactivityTimeout time.Duration // Duration of inactivity before returning to IDLE from PIN_ENTRY (default 30s)
	SummaryTimeout    time.Duration // Duration before returning to IDLE from ORDER_SUMMARY (default 60s)
	PaymentTimeout    time.Duration // Duration before returning to IDLE from PAYMENT_PENDING (default 300s)
	CountdownInterval time.Duration // Tick interval for countdown timer (default 1s)
	RenderScreen      func(ui_ipc.ScreenState) error
	LookupClient      cloud.PINLookupClient
	IdleQRContent     string
	PrinterModel      string
	OnTransactionEnd  func()
}

// Controller coordinates user keypad inputs, cloud lookup, timeouts, and UI rendering.
type Controller struct {
	cfg Config

	mu             sync.Mutex
	state          State
	pinBuffer      string
	pinError       string
	currentOrder     *cloud.OrderDetails
	currentSession   *cloud.SessionDetails
	currentQRPayload string
	lookupSeq        uint64 // Monotonically increasing sequence to discard stale responses

	inactivityTimer *time.Timer
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
	if cfg.SummaryTimeout <= 0 {
		cfg.SummaryTimeout = 60 * time.Second
	}
	if cfg.CountdownInterval <= 0 {
		cfg.CountdownInterval = 1 * time.Second
	}
	if cfg.PaymentTimeout <= 0 {
		cfg.PaymentTimeout = 300 * time.Second
	}

	return &Controller{
		cfg:         cfg,
		inputSource: inputSource,
		state:       StateIdle,
		done:        make(chan struct{}),
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

// ResetToIdle resets the controller to StateIdle and re-renders the idle screen.
func (c *Controller) ResetToIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetToIdleLocked()
}

func (c *Controller) resetToIdleLocked() {
	c.stopInactivityTimerLocked()
	c.stopCountdownLocked()
	atomic.AddUint64(&c.lookupSeq, 1) // Invalidate any inflight lookups
	c.state = StateIdle
	c.pinBuffer = ""
	c.pinError = ""
	c.currentOrder = nil
	c.currentSession = nil
	c.currentQRPayload = ""

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
	case StateOrderSummary:
		c.handleOrderSummaryEvent(ev)
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
		c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
		c.renderPINEntryLocked()
	}
}

func (c *Controller) handlePINEntryEvent(ev input.Event) {
	c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)

	switch ev.Type {
	case input.ActionDigit:
		if len(c.pinBuffer) < 4 {
			c.pinBuffer += string(ev.Digit)
			c.pinError = ""
			c.renderPINEntryLocked()
		}
	case input.ActionBackspace:
		if len(c.pinBuffer) > 0 {
			c.pinBuffer = c.pinBuffer[:len(c.pinBuffer)-1]
			c.pinError = ""
			c.renderPINEntryLocked()
		}
	case input.ActionClear:
		c.pinBuffer = ""
		c.pinError = ""
		c.renderPINEntryLocked()
	case input.ActionCancel:
		c.resetToIdleLocked()
	case input.ActionSubmit:
		if len(c.pinBuffer) != 4 {
			c.pinError = "PIN must be exactly 4 digits"
			c.renderPINEntryLocked()
			return
		}
		c.startLookupLocked()
	}
}

func (c *Controller) handlePINLoadingEvent(ev input.Event) {
	// While loading, only Cancel is accepted to abort and return to idle
	if ev.Type == input.ActionCancel {
		c.resetToIdleLocked()
	}
}

func (c *Controller) handleOrderSummaryEvent(ev input.Event) {
	c.resetInactivityTimerLocked(c.cfg.SummaryTimeout)

	switch ev.Type {
	case input.ActionCancel:
		c.resetToIdleLocked()
	case input.ActionSubmit:
		c.startClaimLocked()
	}
}

func (c *Controller) handlePaymentPendingEvent(ev input.Event) {
	switch ev.Type {
	case input.ActionCancel:
		if c.currentSession != nil {
			sessID := c.currentSession.SessionID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = c.cfg.LookupClient.CancelSession(ctx, sessID)
			}()
		}
		c.resetToIdleLocked()
	case input.ActionSubmit:
		if c.pinError != "" && c.currentSession != nil {
			log.Printf("[PIN-CONTROLLER] Retrying payment initiation for session %s", c.currentSession.SessionID)
			c.pinError = ""
			c.renderPaymentPendingLocked(int(c.cfg.PaymentTimeout.Seconds()))
			seq := atomic.AddUint64(&c.lookupSeq, 1)
			sID := c.currentSession.SessionID
			go func(targetSeq uint64, sessionID string) {
				initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer initCancel()
				attempt, pErr := c.cfg.LookupClient.InitiatePayment(initCtx, sessionID)
				c.mu.Lock()
				defer c.mu.Unlock()
				if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending {
					return
				}
				if pErr != nil {
					log.Printf("[PIN-CONTROLLER] Retry initiate payment failed: %v", pErr)
					c.pinError = "Failed to load payment QR. Press Confirm to retry."
					c.renderPaymentPendingLocked(int(c.cfg.PaymentTimeout.Seconds()))
					return
				}
				c.currentQRPayload = attempt.QRPayload
				c.pinError = ""
				c.renderPaymentPendingLocked(int(c.cfg.PaymentTimeout.Seconds()))
			}(seq, sID)
		} else {
			log.Printf("[PIN-CONTROLLER] User acknowledged payment screen for order %s", c.ActiveOrderID())
		}
	}
}

func (c *Controller) startClaimLocked() {
	if c.currentOrder == nil {
		c.resetToIdleLocked()
		return
	}

	orderID := c.currentOrder.OrderID
	c.stopInactivityTimerLocked()
	c.stopCountdownLocked()
	seq := atomic.AddUint64(&c.lookupSeq, 1)

	go func(targetSeq uint64, ordID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		session, err := c.cfg.LookupClient.ClaimOrder(ctx, ordID)

		c.mu.Lock()
		defer c.mu.Unlock()

		if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StateOrderSummary {
			return
		}

		if err != nil {
			log.Printf("[PIN-CONTROLLER] Failed to claim order %s: %v", ordID, err)
			var userError string
			switch {
			case errors.Is(err, cloud.ErrInsufficientPaper):
				userError = "Paper tray supply is low or empty. Please ask attendant to refill paper and press Confirm."
			case errors.Is(err, cloud.ErrPrinterOffline):
				userError = "Printer is offline. Please ask attendant for assistance."
			case errors.Is(err, cloud.ErrPrinterError):
				userError = "Printer hardware error. Please ask attendant for assistance."
			case errors.Is(err, cloud.ErrOrderAlreadyClaimed):
				userError = "Order is already claimed by an active transaction."
			case errors.Is(err, cloud.ErrOrderExpired):
				userError = "Order has expired. Please create a new order."
			case errors.Is(err, cloud.ErrUnauthorized):
				userError = "Kiosk authentication error. Please see attendant."
			default:
				var netErr *cloud.NetworkError
				if errors.As(err, &netErr) {
					userError = "Network error connecting to cloud. Please try again."
				} else {
					userError = "Unable to start payment session. Please try again."
				}
			}

			c.pinError = userError
			c.renderOrderSummaryLocked(int(c.cfg.SummaryTimeout.Seconds()))
			c.resetInactivityTimerLocked(c.cfg.SummaryTimeout)
			c.startCountdownLocked(c.cfg.SummaryTimeout)
			return
		}

		c.currentSession = session

		// If the order is already paid, release physical print dispatch immediately!
		if c.currentOrder != nil && c.currentOrder.Status == string(contracts.JobStatusPaidWaiting) {
			log.Printf("[PIN-CONTROLLER] Order %s is already PAID_WAITING; requesting physical print release", ordID)
			go func(releaseOrdID string) {
				relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer relCancel()
				if rErr := c.cfg.LookupClient.ReleaseOrder(relCtx, releaseOrdID); rErr != nil {
					log.Printf("[PIN-CONTROLLER] Failed to release paid order %s: %v", releaseOrdID, rErr)
				}
			}(ordID)
			c.resetToIdleLocked()
			return
		}

		c.state = StatePaymentPending
		c.pinError = ""
		c.currentQRPayload = ""

		timeoutSec := int(c.cfg.PaymentTimeout.Seconds())
		if session.SecondsRemaining > 0 && int(session.SecondsRemaining) < timeoutSec {
			timeoutSec = int(session.SecondsRemaining)
		}
		if timeoutSec < 10 {
			timeoutSec = 10
		}

		c.renderPaymentPendingLocked(timeoutSec)
		c.startCountdownLocked(time.Duration(timeoutSec) * time.Second)

		// Asynchronously initiate payment to obtain dynamic payment QR payload
		sessID := session.SessionID
		go func(targetSeq uint64, sID string, tSec int) {
			initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer initCancel()

			attempt, pErr := c.cfg.LookupClient.InitiatePayment(initCtx, sID)
			c.mu.Lock()
			defer c.mu.Unlock()

			if atomic.LoadUint64(&c.lookupSeq) != targetSeq || c.state != StatePaymentPending {
				return
			}

			if pErr != nil {
				log.Printf("[PIN-CONTROLLER] Failed to initiate payment for session %s: %v", sID, pErr)
				c.pinError = "Failed to load payment QR. Press Confirm to retry."
				c.renderPaymentPendingLocked(tSec)
				return
			}

			c.currentQRPayload = attempt.QRPayload
			c.pinError = ""
			c.renderPaymentPendingLocked(tSec)
		}(targetSeq, sessID, timeoutSec)
	}(seq, orderID)
}

func (c *Controller) startLookupLocked() {
	c.state = StatePINLoading
	c.pinError = ""
	c.stopInactivityTimerLocked()

	seq := atomic.AddUint64(&c.lookupSeq, 1)
	pinToLookup := c.pinBuffer

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

		if err != nil {
			c.state = StatePINEntry
			c.pinBuffer = "" // Clear buffer so user enters fresh PIN
			var rle *cloud.RateLimitError
			var ne *cloud.NetworkError

			switch {
			case errors.Is(err, cloud.ErrInvalidOrExpiredPIN):
				c.pinError = "Invalid or expired PIN. Please try again."
			case errors.Is(err, cloud.ErrUnauthorized):
				c.pinError = "Kiosk authentication error. Please see attendant."
			case errors.As(err, &rle):
				if rle.RetryAfterSeconds > 0 {
					c.pinError = fmt.Sprintf("Too many attempts. Please wait %d seconds.", rle.RetryAfterSeconds)
				} else {
					c.pinError = "Too many failed attempts. Please wait a moment."
				}
			case errors.As(err, &ne):
				c.pinError = "Network error connecting to cloud. Please try again."
			default:
				c.pinError = "Lookup failed. Please check PIN and try again."
			}

			c.resetInactivityTimerLocked(c.cfg.InactivityTimeout)
			c.renderPINEntryLocked()
			return
		}

		// Success: transition to Order Summary screen
		c.state = StateOrderSummary
		c.currentOrder = order
		c.startCountdownLocked(c.cfg.SummaryTimeout)
		c.renderOrderSummaryLocked(int(c.cfg.SummaryTimeout.Seconds()))
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

func (c *Controller) renderOrderSummaryLocked(remainingSeconds int) {
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

	_ = c.cfg.RenderScreen(ui_ipc.ScreenState{
		Mode:           ui_ipc.ModeOrderSummary,
		OrderSummary:   summary,
		TimeoutSeconds: remainingSeconds,
		PrinterModel:   c.cfg.PrinterModel,
	})
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

	_ = c.cfg.RenderScreen(ui_ipc.ScreenState{
		Mode:           ui_ipc.ModePaymentPending,
		OrderSummary:   summary,
		SessionID:      sessID,
		QRContent:      c.currentQRPayload,
		PINError:       c.pinError,
		TimeoutSeconds: remainingSeconds,
		PrinterModel:   c.cfg.PrinterModel,
	})
}

func (c *Controller) resetInactivityTimerLocked(d time.Duration) {
	c.stopInactivityTimerLocked()
	c.inactivityTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.state == StatePINEntry || c.state == StateOrderSummary || c.state == StatePaymentPending {
			log.Printf("[PIN-CONTROLLER] Inactivity timeout reached, returning to idle screen")
			if c.currentSession != nil {
				sessID := c.currentSession.SessionID
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = c.cfg.LookupClient.CancelSession(ctx, sessID)
				}()
			}
			c.resetToIdleLocked()
		}
	})
}

func (c *Controller) stopInactivityTimerLocked() {
	if c.inactivityTimer != nil {
		c.inactivityTimer.Stop()
		c.inactivityTimer = nil
	}
}

func (c *Controller) startCountdownLocked(totalDuration time.Duration) {
	c.stopCountdownLocked()
	c.stopCountdown = make(chan struct{})
	c.resetInactivityTimerLocked(totalDuration)

	go func(stopCh chan struct{}, totalSec int) {
		ticker := time.NewTicker(c.cfg.CountdownInterval)
		defer ticker.Stop()

		rem := totalSec
		for {
			select {
			case <-stopCh:
				return
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				rem--
				if rem <= 0 {
					return
				}
				c.mu.Lock()
				if c.state == StateOrderSummary {
					c.renderOrderSummaryLocked(rem)
				} else if c.state == StatePaymentPending {
					c.renderPaymentPendingLocked(rem)
				}
				c.mu.Unlock()
			}
		}
	}(c.stopCountdown, int(totalDuration.Seconds()))
}

func (c *Controller) stopCountdownLocked() {
	if c.stopCountdown != nil {
		close(c.stopCountdown)
		c.stopCountdown = nil
	}
}
