package pin

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

const canonicalTestToken = "dmo_0123456789abcdef0123456789abcdef0123456789abcdef"

type mockLookupClient struct {
	mu           sync.Mutex
	lookupFn     func(ctx context.Context, pin string) (*cloud.OrderDetails, error)
	claimFn      func(ctx context.Context, orderID string) (*cloud.SessionDetails, error)
	releaseFn    func(ctx context.Context, orderID string) error
	cancelFn     func(ctx context.Context, sessionID string) error
	getFn        func(ctx context.Context, sessionID string) (*cloud.SessionDetails, error)
	initiateFn   func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error)
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

func (m *mockLookupClient) InitiatePayment(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
	m.mu.Lock()
	fn := m.initiateFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, sessionID)
	}
	return &cloud.PaymentAttemptDetails{
		AttemptID:     "att_mock_001",
		SessionID:     sessionID,
		Status:        "PAYMENT_PENDING",
		AttemptStatus: "CREATED",
		DemoToken:     canonicalTestToken,
		QRPayload:     "https://test.local/pay/" + canonicalTestToken,
		AmountPaise:   1800,
	}, nil
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

func TestController_SuccessfulLookupAndDirectCheckout(t *testing.T) {
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
	deadline := time.Now().Add(2 * time.Second)
	for (ctrl.GetState() != StatePaymentPending || ctrl.GetCurrentSession() == nil) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}

	last := rnd.LastState()
	if last.Mode != ui_ipc.ModePaymentPending {
		t.Errorf("expected mode %s, got %s", ui_ipc.ModePaymentPending, last.Mode)
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

	// Cancel on Checkout returns to Idle
	sim.EmitCancel()
	time.Sleep(20 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("state after cancel on checkout = %s, want %s", ctrl.GetState(), StateIdle)
	}
}

func TestController_DirectCheckoutClaimAndPaymentPending(t *testing.T) {
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

	// Enter PIN & single Submit -> automatically claims order and transitions to PAYMENT_PENDING
	sim.EmitDigits("1234")
	time.Sleep(20 * time.Millisecond)
	sim.EmitSubmit()
	deadline := time.Now().Add(2 * time.Second)
	for (ctrl.GetState() != StatePaymentPending || ctrl.GetCurrentSession() == nil) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

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

	// 1. Enter PIN and submit -> automatically attempts ClaimOrder and shows paper shortage error in StatePaymentPending
	sim.EmitDigits("4567")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(ctrl.GetPINError(), "refill paper") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s on shortage, got %s", StatePaymentPending, ctrl.GetState())
	}
	if ctrl.GetCurrentSession() != nil {
		t.Fatalf("expected nil session on claim failure, got %+v", ctrl.GetCurrentSession())
	}
	if !strings.Contains(ctrl.GetPINError(), "refill paper") {
		t.Errorf("expected refill paper message in pinError, got %q", ctrl.GetPINError())
	}
	if last := rnd.LastState(); last.TimeoutSeconds != 0 {
		t.Errorf("expected no session countdown (TimeoutSeconds=0) before valid session exists, got %d", last.TimeoutSeconds)
	}

	// 2. Attendant refills paper; customer presses Confirm to retry claim without retyping PIN
	sim.EmitSubmit()
	deadline = time.Now().Add(2 * time.Second)
	for (ctrl.GetCurrentSession() == nil || ctrl.GetPINError() != "") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("state after refill retry = %s, want %s", ctrl.GetState(), StatePaymentPending)
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_refilled_123" {
		t.Fatalf("expected session sess_refilled_123 after refill retry, got %+v", sess)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("pinError after success = %q, want empty", ctrl.GetPINError())
	}
}

func TestController_InitiatePayment_DynamicQR(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	rnd := &mockRenderer{}

	token := "dmo_1111222233334444aaaabbbbccccddddeeeeffff00001111"
	expectedQR := "https://demo.local/pay/" + token
	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:          "ord_qr_test",
				KioskID:          "kiosk-1",
				Status:           "PENDING_PAYMENT",
				TotalPricePaise:  1250,
				TotalPages:       4,
				TotalSheets:      2,
				Copies:           1,
				Duplex:           "simplex",
				ColorMode:        "mono",
				PINExpiresAt:     time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				SecondsRemaining: 600,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_qr_123",
				OrderID:          orderID,
				KioskID:          "kiosk-1",
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  1250,
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID:     "att_qr_123",
				SessionID:     sessionID,
				OrderID:       "ord_qr_test",
				KioskID:       "kiosk-1",
				Status:        "PAYMENT_PENDING",
				AttemptStatus: "CREATED",
				DemoToken:     token,
				QRPayload:     expectedQR,
				AmountPaise:   1250,
				Currency:      "INR",
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
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

	// Enter PIN and submit -> directly claims order and initiates payment
	sim.EmitDigits("8888")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}

	// Wait for async InitiatePayment to deliver QRContent
	var foundQR bool
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rnd.mu.Lock()
		for _, s := range rnd.states {
			if s.Mode == ui_ipc.ModePaymentPending && s.QRContent == expectedQR {
				foundQR = true
				break
			}
		}
		rnd.mu.Unlock()
		if foundQR {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !foundQR {
		t.Errorf("renderer did not receive expected dynamic QRContent %q in ModePaymentPending", expectedQR)
	}
}

func TestController_SSH_CanonicalStdinSequence(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	readerSrc := input.NewReaderSource(pr)
	defer readerSrc.Close()

	rnd := &mockRenderer{}
	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			if pin == "4281" {
				return &cloud.OrderDetails{
					OrderID:         "ord_ssh_4281",
					TotalPages:      4,
					TotalSheets:     2,
					Copies:          1,
					Duplex:          "LONG_EDGE",
					ColorMode:       "MONO",
					TotalPricePaise: 200,
				}, nil
			}
			return nil, cloud.ErrInvalidOrExpiredPIN
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_ssh_001",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  200,
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			token := "dmo_2222333344445555bbbbccccddddeeeeffff000011112222"
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_ssh_001",
				DemoToken: token,
				QRPayload: "https://xeroxgo.demo/pay/" + token,
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		CountdownInterval: 100 * time.Millisecond,
		PaymentTimeout:    300 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
		IdleQRContent:     "https://xeroxgo.test/idle",
		PrinterModel:      "Brother HL-L5210DN",
	}

	ctrl, err := NewController(cfg, readerSrc)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Simulate user typing "4281\n" over SSH in line-buffered canonical mode -> direct checkout
	go func() {
		_, _ = pw.Write([]byte("4281\n"))
	}()

	// Wait for transition to StatePaymentPending
	deadline := time.Now().Add(2 * time.Second)
	for (ctrl.GetState() != StatePaymentPending || ctrl.GetCurrentSession() == nil) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s after typing 4281\\n, got %s", StatePaymentPending, ctrl.GetState())
	}
	if ctrl.ActiveOrderID() != "ord_ssh_4281" {
		t.Errorf("ActiveOrderID = %q, want ord_ssh_4281", ctrl.ActiveOrderID())
	}
}

func TestController_PaymentURLBase_Wrapping(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	expectedDemoToken := "dmo_3333444455556666ccccddddeeeeffff0000111122223333"
	expectedWrappedURL := "https://xeroxgo.vercel.app/pay/" + expectedDemoToken

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_wrap_001",
				KioskID:         "kiosk-1",
				Status:          "DRAFT",
				TotalPages:      3,
				TotalSheets:     2,
				Copies:          1,
				Duplex:          "long_edge",
				ColorMode:       "BW",
				TotalPricePaise: 400,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_wrap_101",
				OrderID:          orderID,
				Status:           "CLAIMED",
				TotalPricePaise:  400,
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID:     "att_wrap_201",
				SessionID:     sessionID,
				DemoToken:     expectedDemoToken,
				QRPayload:     "https://test.local/pay/" + expectedDemoToken,
				AmountPaise:   400,
				Status:        "PAYMENT_PENDING",
				AttemptStatus: "CREATED",
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		IdleQRContent:     "https://xeroxgo.vercel.app",
		PrinterModel:      "Brother HL-L5210DN",
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter PIN "1234" and submit -> direct checkout
	sim.EmitDigit('1')
	sim.EmitDigit('2')
	sim.EmitDigit('3')
	sim.EmitDigit('4')
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}

	// 3. Wait for InitiatePayment to deliver wrapped QR content
	var foundWrappedQR bool
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rnd.mu.Lock()
		for _, s := range rnd.states {
			if s.Mode == ui_ipc.ModePaymentPending && s.QRContent == expectedWrappedURL {
				foundWrappedQR = true
				break
			}
		}
		rnd.mu.Unlock()
		if foundWrappedQR {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !foundWrappedQR {
		t.Fatalf("expected QRContent %q to be rendered on payment screen", expectedWrappedURL)
	}
}

func TestController_KeypadConfirm_DemoPayment(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_demo_keypad_1",
				TotalPricePaise: 400,
				TotalPages:      2,
				TotalSheets:     2,
				Copies:          1,
				Duplex:          "simplex",
				ColorMode:       "bw",
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_demo_keypad_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_demo_keypad_1",
				SessionID: sessionID,
				DemoToken: "dmo_4444555566667777ddddeeeeffff00001111222233334444",
				QRPayload: "https://xeroxgo.vercel.app/pay/dmo_4444555566667777ddddeeeeffff00001111222233334444",
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter PIN -> directly claims order and enters StatePaymentPending
	sim.EmitDigit('1')
	sim.EmitDigit('2')
	sim.EmitDigit('3')
	sim.EmitDigit('4')
	sim.EmitSubmit()

	// Wait for PaymentPending and QRContent arrival in rendered state
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		last := rnd.LastState()
		if ctrl.GetState() == StatePaymentPending && last.Mode == ui_ipc.ModePaymentPending && strings.Contains(last.QRContent, "dmo_4444555566667777ddddeeeeffff00001111222233334444") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}

	// Press Submit (Confirm/Enter) on keypad - must NOT authorize payment!
	sim.EmitSubmit()

	time.Sleep(300 * time.Millisecond)

	if ctrl.GetState() != StatePaymentPending {
		t.Errorf("expected controller to remain in StatePaymentPending, got %s", ctrl.GetState())
	}
}

func TestController_ErrorClearTimeout(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	rnd := &mockRenderer{}
	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return nil, cloud.ErrInvalidOrExpiredPIN
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		ErrorClearTimeout: 80 * time.Millisecond,
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

	// 1. Submit < 4 digits (e.g. 2 digits)
	sim.EmitDigit('1')
	sim.EmitDigit('2')
	sim.EmitSubmit()

	deadline := time.Now().Add(200 * time.Millisecond)
	for ctrl.GetPINError() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ctrl.GetPINError() != "PIN must be exactly 4 digits" {
		t.Fatalf("expected PIN error for 2-digit submit, got %q", ctrl.GetPINError())
	}

	// Wait for ErrorClearTimeout (80ms)
	deadline = time.Now().Add(500 * time.Millisecond)
	for ctrl.GetPINError() != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("expected PIN error to auto-clear after ErrorClearTimeout, got %q", ctrl.GetPINError())
	}
	if ctrl.GetPINBuffer() != "" {
		t.Errorf("expected PIN buffer to clear after ErrorClearTimeout, got %q", ctrl.GetPINBuffer())
	}
}

func TestController_PaymentQR_ValidDemoToken(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	expectedDemoToken := "dmo_5555666677778888eeeeffff000011112222333344445555"
	expectedURL := "https://xeroxgo.vercel.app/pay/" + expectedDemoToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:          "ord_valid_qr_1",
				KioskID:          "kiosk-1",
				Status:           "PENDING_PAYMENT",
				TotalPricePaise:  500,
				TotalPages:       2,
				TotalSheets:      1,
				Copies:           1,
				Duplex:           "simplex",
				ColorMode:        "BW",
				SecondsRemaining: 300,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_valid_qr_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  500,
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID:     "att_valid_qr_1",
				SessionID:     sessionID,
				OrderID:       "ord_valid_qr_1",
				Status:        "PAYMENT_PENDING",
				AttemptStatus: "CREATED",
				DemoToken:     expectedDemoToken,
				QRPayload:     "https://ignore.local/pay/" + expectedDemoToken,
				AmountPaise:   500,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter PIN and submit -> direct checkout
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
	}

	// Wait for async InitiatePayment to deliver valid QR
	var foundValidQR bool
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rnd.mu.Lock()
		for _, s := range rnd.states {
			if s.Mode == ui_ipc.ModePaymentPending && s.QRContent == expectedURL && s.PINError == "" {
				foundValidQR = true
				break
			}
		}
		rnd.mu.Unlock()
		if foundValidQR {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !foundValidQR {
		t.Fatalf("expected QRContent %q with empty PINError to be rendered", expectedURL)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("expected empty pinError, got %q", ctrl.GetPINError())
	}
}

func TestController_PaymentQR_MissingOrInvalidDemoToken_ShowsRetryableError(t *testing.T) {
	testCases := []struct {
		name           string
		demoToken      string
		qrPayload      string
		paymentURLBase string
	}{
		{"empty demo token", "", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"whitespace demo token", "   ", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"incorrect prefix", "tok_0123456789abcdef0123456789abcdef0123456789abcdef", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"too short length", "dmo_0123456789abcdef0123456789abcdef0123456789abcde", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"too long length", "dmo_0123456789abcdef0123456789abcdef0123456789abcdef0", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"uppercase hex", "dmo_0123456789ABCDEF0123456789abcdef0123456789abcdef", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"non-hex character", "dmo_0123456789abcdef0123456789abcdef0123456789abcdeg", "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/pay"},
		{"mismatched URL token without base", canonicalTestToken, "https://demo.local/pay/dmo_999999999999999999999999999999999999999999999999", ""},
		{"malformed checkout URL without base", canonicalTestToken, "://bad-checkout-url", ""},
		{"non-http checkout URL without base", canonicalTestToken, "upi://pay?pa=mock", ""},
		{"malformed payment URL base (wrong path)", canonicalTestToken, "https://attacker.example.com/arbitrary-raw-qr", "https://xeroxgo.vercel.app/payment"},
		{"malformed payment URL base (non-http)", canonicalTestToken, "https://attacker.example.com/arbitrary-raw-qr", "ftp://xeroxgo.vercel.app/pay"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sim := input.NewSimulatedSource(16)
			defer sim.Close()
			rnd := &mockRenderer{}

			client := &mockLookupClient{
				lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
					return &cloud.OrderDetails{
						OrderID:          "ord_missing_qr_1",
						KioskID:          "kiosk-1",
						Status:           "PENDING_PAYMENT",
						TotalPricePaise:  300,
						TotalPages:       1,
						TotalSheets:      1,
						Copies:           1,
						Duplex:           "simplex",
						ColorMode:        "BW",
						SecondsRemaining: 300,
					}, nil
				},
				claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
					return &cloud.SessionDetails{
						SessionID:        "sess_missing_qr_1",
						OrderID:          orderID,
						Status:           "PAYMENT_PENDING",
						TotalPricePaise:  300,
						SecondsRemaining: 300,
					}, nil
				},
				initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
					return &cloud.PaymentAttemptDetails{
						AttemptID:     "att_missing_qr_1",
						SessionID:     sessionID,
						OrderID:       "ord_missing_qr_1",
						Status:        "PAYMENT_PENDING",
						AttemptStatus: "CREATED",
						DemoToken:     tc.demoToken,
						QRPayload:     tc.qrPayload,
						AmountPaise:   300,
					}, nil
				},
			}

			ctrl, err := NewController(Config{
				InactivityTimeout: 10 * time.Second,
				PaymentTimeout:    10 * time.Second,
				RenderScreen:      rnd.Render,
				LookupClient:      client,
				PaymentURLBase:    tc.paymentURLBase,
			}, sim)
			if err != nil {
				t.Fatalf("failed to create controller: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctrl.Start(ctx)
			defer ctrl.Stop()

			// Enter PIN -> directly enters checkout and initiates payment
			sim.EmitDigits("1234")
			sim.EmitSubmit()

			deadline := time.Now().Add(2 * time.Second)
			for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if ctrl.GetState() != StatePaymentPending {
				t.Fatalf("expected state %s, got %s", StatePaymentPending, ctrl.GetState())
			}

			// Wait for error state to appear
			expectedErr := "Failed to load payment QR. Press Confirm to retry."
			deadline = time.Now().Add(2 * time.Second)
			for ctrl.GetPINError() != expectedErr && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}

			if ctrl.GetPINError() != expectedErr {
				t.Fatalf("expected pinError %q, got %q", expectedErr, ctrl.GetPINError())
			}

			// Verify that arbitrary raw-QR or invalid QR was NEVER rendered, and QRContent remained empty
			rnd.mu.Lock()
			for _, s := range rnd.states {
				if s.Mode == ui_ipc.ModePaymentPending {
					if tc.qrPayload != "" && s.QRContent == tc.qrPayload {
						rnd.mu.Unlock()
						t.Fatalf("raw or mismatched QR payload %q was rendered unexpectedly!", tc.qrPayload)
					}
					if s.PINError == expectedErr && s.QRContent != "" {
						rnd.mu.Unlock()
						t.Fatalf("expected empty QRContent when error is displayed, got %q", s.QRContent)
					}
				}
			}
			last := rnd.states[len(rnd.states)-1]
			rnd.mu.Unlock()

			if last.QRContent != "" {
				t.Errorf("last rendered state has QRContent = %q, want empty", last.QRContent)
			}
			if last.PINError != expectedErr {
				t.Errorf("last rendered state has PINError = %q, want %q", last.PINError, expectedErr)
			}
		})
	}
}

func TestController_PaymentQR_RetryBehavior(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var attemptCount int
	var attemptMu sync.Mutex

	expectedRetryToken := "dmo_6666777788889999ffff0000111122223333444455556666"
	expectedRetryURL := "https://xeroxgo.vercel.app/pay/" + expectedRetryToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:          "ord_retry_test_1",
				KioskID:          "kiosk-1",
				Status:           "PENDING_PAYMENT",
				TotalPricePaise:  700,
				TotalPages:       2,
				TotalSheets:      1,
				Copies:           1,
				Duplex:           "simplex",
				ColorMode:        "BW",
				SecondsRemaining: 300,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_retry_test_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  700,
				SecondsRemaining: 300,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			attemptMu.Lock()
			attemptCount++
			count := attemptCount
			attemptMu.Unlock()

			if count == 1 {
				// First attempt returns missing demo token (fails)
				return &cloud.PaymentAttemptDetails{
					AttemptID:     "att_retry_1",
					SessionID:     sessionID,
					OrderID:       "ord_retry_test_1",
					Status:        "PAYMENT_PENDING",
					AttemptStatus: "CREATED",
					DemoToken:     "",
					AmountPaise:   700,
				}, nil
			}

			// Subsequent retry returns valid demo token
			return &cloud.PaymentAttemptDetails{
				AttemptID:     "att_retry_2",
				SessionID:     sessionID,
				OrderID:       "ord_retry_test_1",
				Status:        "PAYMENT_PENDING",
				AttemptStatus: "CREATED",
				DemoToken:     expectedRetryToken,
				AmountPaise:   700,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    10 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
	}, sim)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter PIN -> directly triggers ClaimOrder and first InitiatePayment
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	expectedErr := "Failed to load payment QR. Press Confirm to retry."
	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetPINError() != expectedErr && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if ctrl.GetPINError() != expectedErr {
		t.Fatalf("expected pinError %q after failed first attempt, got %q", expectedErr, ctrl.GetPINError())
	}

	// Press keypad Confirm (ActionSubmit) to explicitly retry
	sim.EmitSubmit()

	// Wait for retry to succeed with valid QR
	var foundRecoveredQR bool
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rnd.mu.Lock()
		for _, s := range rnd.states {
			if s.Mode == ui_ipc.ModePaymentPending && s.QRContent == expectedRetryURL && s.PINError == "" {
				foundRecoveredQR = true
				break
			}
		}
		rnd.mu.Unlock()
		if foundRecoveredQR {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !foundRecoveredQR {
		t.Fatalf("expected QRContent %q after retry, did not find it in rendered states", expectedRetryURL)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("expected empty pinError after retry recovery, got %q", ctrl.GetPINError())
	}

	// Pressing Confirm again when QR is active must be ignored
	attemptMu.Lock()
	countBefore := attemptCount
	attemptMu.Unlock()

	sim.EmitSubmit()
	time.Sleep(50 * time.Millisecond)

	attemptMu.Lock()
	countAfter := attemptCount
	attemptMu.Unlock()

	if countAfter != countBefore {
		t.Errorf("Confirm press on active QR screen triggered redundant InitiatePayment: before=%d after=%d", countBefore, countAfter)
	}
	if ctrl.GetPINError() != "" {
		t.Errorf("pinError became %q after ignoring Confirm, want empty", ctrl.GetPINError())
	}
}

func TestController_ResolvePaymentQRPayload(t *testing.T) {
	ctrlWithBase, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "https://xeroxgo.vercel.app/pay",
	}, input.NewSimulatedSource(4))

	ctrlWithTrailingSlash, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "https://xeroxgo.vercel.app/pay/",
	}, input.NewSimulatedSource(4))

	ctrlWithoutBase, _ := NewController(Config{
		RenderScreen: func(ui_ipc.ScreenState) error { return nil },
		LookupClient: &mockLookupClient{},
	}, input.NewSimulatedSource(4))

	ctrlWithInvalidBasePath, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "https://xeroxgo.vercel.app",
	}, input.NewSimulatedSource(4))

	ctrlWithNonHTTPBase, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "ftp://xeroxgo.vercel.app/pay",
	}, input.NewSimulatedSource(4))

	ctrlWithMalformedBase, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "://bad-url",
	}, input.NewSimulatedSource(4))

	ctrlWithBaseQuery, _ := NewController(Config{
		RenderScreen:   func(ui_ipc.ScreenState) error { return nil },
		LookupClient:   &mockLookupClient{},
		PaymentURLBase: "https://xeroxgo.vercel.app/pay?foo=bar",
	}, input.NewSimulatedSource(4))

	tests := []struct {
		name     string
		ctrl     *Controller
		attempt  *cloud.PaymentAttemptDetails
		expected string
	}{
		{
			name:     "nil attempt returns empty string",
			ctrl:     ctrlWithBase,
			attempt:  nil,
			expected: "",
		},
		{
			name: "missing demo token returns empty string even with raw QRPayload",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_1",
				DemoToken: "",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "whitespace demo token returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_2",
				DemoToken: "   ",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "incorrect prefix (tok_ instead of dmo_) returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_3",
				DemoToken: "tok_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "too short length returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_4",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcde",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "too long length returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_5",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef0",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "non-hex uppercase characters return empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_6",
				DemoToken: "dmo_0123456789ABCDEF0123456789abcdef0123456789abcdef",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "non-hex character g returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_7",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdeg",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "demo token with space returns empty string",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_8",
				DemoToken: "dmo_0123456789abcdef 123456789abcdef0123456789abcdef",
				QRPayload: "https://arbitrary.raw.qr/payload",
			},
			expected: "",
		},
		{
			name: "valid canonical demo token with configured PaymentURLBase",
			ctrl: ctrlWithBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_9",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "https://xeroxgo.vercel.app/pay/" + canonicalTestToken,
		},
		{
			name: "valid canonical demo token with PaymentURLBase trailing slash normalizes",
			ctrl: ctrlWithTrailingSlash,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_10",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "https://xeroxgo.vercel.app/pay/" + canonicalTestToken,
		},
		{
			name: "configured PaymentURLBase without /pay path is rejected",
			ctrl: ctrlWithInvalidBasePath,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_11",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "",
		},
		{
			name: "configured PaymentURLBase with non-http scheme is rejected",
			ctrl: ctrlWithNonHTTPBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_12",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "",
		},
		{
			name: "configured PaymentURLBase with malformed URL is rejected",
			ctrl: ctrlWithMalformedBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_13",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "",
		},
		{
			name: "configured PaymentURLBase with query parameters is rejected",
			ctrl: ctrlWithBaseQuery,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_14",
				DemoToken: canonicalTestToken,
				QRPayload: "https://old.ignored.qr",
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase uses valid cloud checkout URL with exact /pay/{token} path",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_15",
				DemoToken: canonicalTestToken,
				QRPayload: "https://demo.local/pay/" + canonicalTestToken,
			},
			expected: "https://demo.local/pay/" + canonicalTestToken,
		},
		{
			name: "unconfigured PaymentURLBase with mismatched token in cloud checkout URL is rejected",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_16",
				DemoToken: canonicalTestToken,
				QRPayload: "https://demo.local/pay/dmo_999999999999999999999999999999999999999999999999",
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase with wrong path in cloud checkout URL is rejected",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_17",
				DemoToken: canonicalTestToken,
				QRPayload: "https://demo.local/payment/" + canonicalTestToken,
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase with non-http scheme in cloud checkout URL is rejected",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_18",
				DemoToken: canonicalTestToken,
				QRPayload: "upi://pay?pa=mock",
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase with malformed cloud checkout URL is rejected",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_19",
				DemoToken: canonicalTestToken,
				QRPayload: "://bad-url",
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase with query parameter in cloud checkout URL is rejected",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_20",
				DemoToken: canonicalTestToken,
				QRPayload: "https://demo.local/pay/" + canonicalTestToken + "?ref=1",
			},
			expected: "",
		},
		{
			name: "unconfigured PaymentURLBase with empty QRPayload does not invent fallback",
			ctrl: ctrlWithoutBase,
			attempt: &cloud.PaymentAttemptDetails{
				AttemptID: "att_21",
				DemoToken: canonicalTestToken,
				QRPayload: "",
			},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ctrl.resolvePaymentQRPayload(tc.attempt)
			if got != tc.expected {
				t.Errorf("resolvePaymentQRPayload() = %q, want %q", got, tc.expected)
			}
		})
	}
}



