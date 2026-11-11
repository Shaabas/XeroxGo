package pin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

type mockLookupClient struct {
	mu           sync.Mutex
	lookupFn     func(ctx context.Context, pin string) (*cloud.OrderDetails, error)
	claimFn      func(ctx context.Context, orderID string) (*cloud.SessionDetails, error)
	releaseFn    func(ctx context.Context, orderID string) error
	cancelFn     func(ctx context.Context, sessionID string) error
	getFn        func(ctx context.Context, sessionID string) (*cloud.SessionDetails, error)
	calls        int
	cancelCalls  int
	releaseCalls int
}

func (m *mockLookupClient) LookupOrderByPIN(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
	m.mu.Lock()
	m.calls++
	fn := m.lookupFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, pin)
	}
	return nil, cloud.ErrInvalidOrExpiredPIN
}

func (m *mockLookupClient) ClaimOrder(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
	m.mu.Lock()
	fn := m.claimFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, orderID)
	}
	return &cloud.SessionDetails{
		SessionID:        "sess_mock_001",
		OrderID:          orderID,
		Status:           "PAYMENT_PENDING",
		TotalPricePaise:  1800,
		SecondsRemaining: 300,
	}, nil
}

func (m *mockLookupClient) ReleaseOrder(ctx context.Context, orderID string) error {
	m.mu.Lock()
	m.releaseCalls++
	fn := m.releaseFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, orderID)
	}
	return nil
}

func (m *mockLookupClient) ReleaseCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.releaseCalls
}

func (m *mockLookupClient) CancelSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	m.cancelCalls++
	fn := m.cancelFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, sessionID)
	}
	return nil
}

func (m *mockLookupClient) CancelCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelCalls
}

func (m *mockLookupClient) GetSession(ctx context.Context, sessionID string) (*cloud.SessionDetails, error) {
	m.mu.Lock()
	fn := m.getFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, sessionID)
	}
	return nil, nil
}

type mockRenderer struct {
	mu     sync.Mutex
	states []ui_ipc.ScreenState
}

func (r *mockRenderer) Render(state ui_ipc.ScreenState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
	return nil
}

func (r *mockRenderer) LastState() ui_ipc.ScreenState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return ui_ipc.ScreenState{}
	}
	return r.states[len(r.states)-1]
}

func (r *mockRenderer) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.states)
}

func TestController_DigitInputAndBufferOperations(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
		IdleQRContent:     "https://test.kiosk",
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Initial state is Idle
	if ctrl.GetState() != StateIdle {
		t.Errorf("initial state = %s, want %s", ctrl.GetState(), StateIdle)
	}

	// 2. First digit -> StatePINEntry
	sim.EmitDigit('4')
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetState() != StatePINEntry {
		t.Errorf("state after first digit = %s, want %s", ctrl.GetState(), StatePINEntry)
	}
	if ctrl.GetPINBuffer() != "4" {
		t.Errorf("buffer = %q, want %q", ctrl.GetPINBuffer(), "4")
	}

	// 3. Emit 3 more digits -> total 4
	sim.EmitDigits("829")
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetPINBuffer() != "4829" {
		t.Errorf("buffer = %q, want %q", ctrl.GetPINBuffer(), "4829")
	}

	// 4. 5th digit must be ignored
	sim.EmitDigit('9')
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetPINBuffer() != "4829" {
		t.Errorf("buffer after 5th digit = %q, want %q", ctrl.GetPINBuffer(), "4829")
	}

	// 5. Backspace removes last digit
	sim.EmitBackspace()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetPINBuffer() != "482" {
		t.Errorf("buffer after backspace = %q, want %q", ctrl.GetPINBuffer(), "482")
	}

	// 6. Clear removes all digits
	sim.EmitClear()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetPINBuffer() != "" {
		t.Errorf("buffer after clear = %q, want empty", ctrl.GetPINBuffer())
	}

	// 7. Cancel returns to Idle
	sim.EmitCancel()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("state after cancel = %s, want %s", ctrl.GetState(), StateIdle)
	}
}

func TestController_SubmitLessThan4Digits(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("12")
	time.Sleep(20 * time.Millisecond)

	sim.EmitSubmit()
	time.Sleep(20 * time.Millisecond)

	last := rnd.LastState()
	if last.PINError != "PIN must be exactly 4 digits" {
		t.Errorf("expected PINError 'PIN must be exactly 4 digits', got %q", last.PINError)
	}
	if client.calls != 0 {
		t.Errorf("client was called %d times for incomplete PIN, expected 0", client.calls)
	}
}

func TestController_SuccessfulLookupAndOrderSummary(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			if pin == "6543" {
				return &cloud.OrderDetails{
					OrderID:          "ord_success_999",
					KioskID:          "kiosk-001",
					Status:           "PENDING_PAYMENT",
					TotalPricePaise:  1800,
					TotalPages:       6,
					TotalSheets:      3,
					Copies:           1,
					Duplex:           "long_edge",
					ColorMode:        "mono",
					PINExpiresAt:     time.Now().Add(10 * time.Minute).Format(time.RFC3339),
					SecondsRemaining: 600,
				}, nil
			}
			return nil, cloud.ErrInvalidOrExpiredPIN
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		CountdownInterval: 100 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("6543")
	time.Sleep(20 * time.Millisecond)

	sim.EmitSubmit()
	time.Sleep(50 * time.Millisecond)

	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected state %s, got %s", StateOrderSummary, ctrl.GetState())
	}

	last := rnd.LastState()
	if last.Mode != ui_ipc.ModeOrderSummary {
		t.Errorf("expected mode %s, got %s", ui_ipc.ModeOrderSummary, last.Mode)
	}
	if last.OrderSummary == nil {
		t.Fatalf("expected non-nil OrderSummary")
	}
	if last.OrderSummary.OrderID != "ord_success_999" {
		t.Errorf("expected orderID ord_success_999, got %s", last.OrderSummary.OrderID)
	}
	if last.OrderSummary.TotalPricePaise != 1800 {
		t.Errorf("expected price 1800 paise, got %d", last.OrderSummary.TotalPricePaise)
	}

	// Cancel on Order Summary returns to Idle
	sim.EmitCancel()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("state after cancel on summary = %s, want %s", ctrl.GetState(), StateIdle)
	}
}

func TestController_OrderConfirmationAndPaymentPending(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:          "ord_confirm_123",
				KioskID:          "kiosk-001",
				Status:           "PENDING_PAYMENT",
				TotalPricePaise:  2100,
				TotalPages:       4,
				TotalSheets:      2,
				Copies:           1,
				Duplex:           "long_edge",
				ColorMode:        "mono",
				PINExpiresAt:     time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				SecondsRemaining: 600,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_conf_456",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  2100,
				SecondsRemaining: 300,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		CountdownInterval: 50 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN & Submit -> Order Summary
	sim.EmitDigits("1234")
	time.Sleep(20 * time.Millisecond)
	sim.EmitSubmit()
	time.Sleep(50 * time.Millisecond)

	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected state %s, got %s", StateOrderSummary, ctrl.GetState())
	}

	// 2. Submit on Order Summary -> Claim Order & Transition to PAYMENT_PENDING
	sim.EmitSubmit()
	time.Sleep(60 * time.Millisecond)

	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}
	if !ctrl.HasActiveTransaction() {
		t.Errorf("expected HasActiveTransaction() == true")
	}
	if ctrl.ActiveOrderID() != "ord_confirm_123" {
		t.Errorf("expected active order ID ord_confirm_123, got %s", ctrl.ActiveOrderID())
	}
	if ctrl.ActiveJobID() != "job_ord_confirm_123" {
		t.Errorf("expected active job ID job_ord_confirm_123, got %s", ctrl.ActiveJobID())
	}

	last := rnd.LastState()
	if last.Mode != ui_ipc.ModePaymentPending {
		t.Errorf("expected mode %s, got %s", ui_ipc.ModePaymentPending, last.Mode)
	}
	if last.SessionID != "sess_conf_456" {
		t.Errorf("expected session ID sess_conf_456, got %s", last.SessionID)
	}
	if last.OrderSummary == nil || last.OrderSummary.TotalPricePaise != 2100 {
		t.Errorf("expected price 2100 paise, got %v", last.OrderSummary)
	}

	// 3. Cancel on Payment Pending returns to IDLE and calls CancelSession
	sim.EmitCancel()
	time.Sleep(50 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("state after cancel = %s, want %s", ctrl.GetState(), StateIdle)
	}
	if ctrl.HasActiveTransaction() {
		t.Errorf("expected HasActiveTransaction() == false after cancel")
	}
	if client.CancelCalls() == 0 {
		t.Errorf("expected CancelSession to be called on cancel")
	}
}

func TestController_LookupErrors(t *testing.T) {
	testCases := []struct {
		name          string
		retErr        error
		expectedError string
	}{
		{
			name:          "invalid_or_expired",
			retErr:        cloud.ErrInvalidOrExpiredPIN,
			expectedError: "Invalid or expired PIN. Please try again.",
		},
		{
			name:          "rate_limited",
			retErr:        &cloud.RateLimitError{RetryAfterSeconds: 30},
			expectedError: "Too many attempts. Please wait 30 seconds.",
		},
		{
			name:          "network_error",
			retErr:        &cloud.NetworkError{Err: errors.New("connection refused")},
			expectedError: "Network error connecting to cloud. Please try again.",
		},
		{
			name:          "unauthorized",
			retErr:        cloud.ErrUnauthorized,
			expectedError: "Kiosk authentication error. Please see attendant.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sim := input.NewSimulatedSource(32)
			defer sim.Close()

			rnd := &mockRenderer{}
			client := &mockLookupClient{
				lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
					return nil, tc.retErr
				},
			}

			ctrl, err := NewController(Config{
				InactivityTimeout: 10 * time.Second,
				SummaryTimeout:    10 * time.Second,
				RenderScreen:      rnd.Render,
				LookupClient:      client,
			}, sim)
			if err != nil {
				t.Fatalf("failed to create controller: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctrl.Start(ctx)
			defer ctrl.Stop()

			sim.EmitDigits("1122")
			time.Sleep(20 * time.Millisecond)

			sim.EmitSubmit()
			time.Sleep(50 * time.Millisecond)

			if ctrl.GetState() != StatePINEntry {
				t.Errorf("expected state %s after error, got %s", StatePINEntry, ctrl.GetState())
			}

			last := rnd.LastState()
			if last.PINError != tc.expectedError {
				t.Errorf("expected error %q, got %q", tc.expectedError, last.PINError)
			}
			// Buffer should be cleared after lookup failure
			if ctrl.GetPINBuffer() != "" {
				t.Errorf("expected buffer cleared after failure, got %q", ctrl.GetPINBuffer())
			}
		})
	}
}

func TestController_InactivityTimeout(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{}

	ctrl, err := NewController(Config{
		InactivityTimeout: 50 * time.Millisecond,
		SummaryTimeout:    50 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter digit
	sim.EmitDigit('9')
	time.Sleep(10 * time.Millisecond)

	if ctrl.GetState() != StatePINEntry {
		t.Fatalf("expected state %s, got %s", StatePINEntry, ctrl.GetState())
	}

	// Wait for inactivity timer to fire
	time.Sleep(80 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("expected state to reset to %s after inactivity timeout, got %s", StateIdle, ctrl.GetState())
	}
	if ctrl.GetPINBuffer() != "" {
		t.Errorf("expected PIN buffer cleared, got %q", ctrl.GetPINBuffer())
	}
}

func TestController_DiscardStaleResponseOnCancel(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	lookupStarted := make(chan struct{})
	lookupRelease := make(chan struct{})

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			close(lookupStarted)
			<-lookupRelease
			return &cloud.OrderDetails{
				OrderID:         "ord_stale_123",
				TotalPricePaise: 1000,
				TotalPages:      2,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("9988")
	sim.EmitSubmit()

	<-lookupStarted

	// User cancels while lookup is inflight
	sim.EmitCancel()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected state %s after cancel, got %s", StateIdle, ctrl.GetState())
	}

	// Release slow lookup response
	close(lookupRelease)
	time.Sleep(30 * time.Millisecond)

	// State must STILL be Idle! Stale response was discarded!
	if ctrl.GetState() != StateIdle {
		t.Errorf("stale lookup corrupted state to %s, expected %s", ctrl.GetState(), StateIdle)
	}
}

func TestController_PaperShortageAndRetryAfterRefill(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	var claimAttempts int
	var mu sync.Mutex

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_shortage_456",
				TotalPricePaise: 2000,
				TotalPages:      10,
				TotalSheets:     5,
				Copies:          1,
				Duplex:          "simplex",
				ColorMode:       "BW",
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			mu.Lock()
			defer mu.Unlock()
			claimAttempts++
			if claimAttempts == 1 {
				// First claim attempt fails because of paper shortage
				return nil, cloud.ErrInsufficientPaper
			}
			// Second claim attempt (after refill) succeeds!
			return &cloud.SessionDetails{
				SessionID:        "sess_refilled_123",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  2000,
				SecondsRemaining: 300,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN
	sim.EmitDigits("4567")
	sim.EmitSubmit()

	// Wait for lookup to complete and reach StateOrderSummary
	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("state = %s, want %s", ctrl.GetState(), StateOrderSummary)
	}

	// 2. Customer confirms order
	sim.EmitSubmit()
	time.Sleep(50 * time.Millisecond)

	// Claim failed with ErrInsufficientPaper -> State should STILL be StateOrderSummary!
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected state to remain %s on shortage, got %s", StateOrderSummary, ctrl.GetState())
	}
	if !strings.Contains(ctrl.GetPINError(), "refill paper") {
		t.Errorf("expected refill paper message in pinError, got %q", ctrl.GetPINError())
	}

	// 3. Attendant refills paper; customer presses confirm AGAIN without retyping PIN!
	sim.EmitSubmit()
	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// State should now be StatePaymentPending!
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("state after refill retry = %s, want %s", ctrl.GetState(), StatePaymentPending)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("pinError after success = %q, want empty", ctrl.GetPINError())
	}
}


