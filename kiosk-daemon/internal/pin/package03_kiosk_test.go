package pin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

// Scenario 6: Timeout at 120s returns kiosk to idle and invalidates checkout.
func TestPackage03_Scenario06_CheckoutTimeout120s_ReturnsToIdleAndCancelsSession(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	// Verify that NewController defaults PaymentTimeout to 120s
	defaultCfg := Config{
		RenderScreen: rnd.Render,
		LookupClient: &mockLookupClient{},
	}
	ctrlDefault, err := NewController(defaultCfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}
	if ctrlDefault.cfg.PaymentTimeout != 120*time.Second {
		t.Fatalf("expected default PaymentTimeout 120s, got %v", ctrlDefault.cfg.PaymentTimeout)
	}

	// Now test active checkout timeout transition using 100ms timeout
	cancelCh := make(chan string, 1)
	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_sc6_to",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_sc6_to",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			cancelCh <- sessionID
			return nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    100 * time.Millisecond,
		CountdownInterval: 10 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Enter PIN "1234"
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary, got %s", ctrl.GetState())
	}

	// Confirm order summary -> transitions to StatePaymentPending
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// Wait for 100ms payment timeout to fire
	select {
	case canceledSessID := <-cancelCh:
		if canceledSessID != "sess_sc6_to" {
			t.Errorf("expected CancelSession for sess_sc6_to, got %s", canceledSessID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CancelSession on checkout timeout")
	}

	// Controller must return to StateIdle
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle after timeout, got %s", ctrl.GetState())
	}

	last := rnd.LastState()
	if last.Mode != ui_ipc.ModeIdle {
		t.Fatalf("expected ModeIdle screen after timeout, got %s", last.Mode)
	}
}

// Scenario 9: Rapid Esc/re-entry cannot return a session canceled by a delayed request.
func TestPackage03_Scenario09_RapidEscReentry_DoesNotCancelFreshSession(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	rnd := &mockRenderer{}

	var claimCount int32
	cancelDone := make(chan struct{})

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_sc9_rapid",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			count := atomic.AddInt32(&claimCount, 1)
			if count == 1 {
				// First claim: return sess_old_1
				return &cloud.SessionDetails{
					SessionID:        "sess_old_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			if count == 2 {
				// Second claim (simulating race before cancel reaches server): returns sess_old_1 again
				return &cloud.SessionDetails{
					SessionID:        "sess_old_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			// Third claim (after cancel finishes): returns fresh sess_fresh_2
			return &cloud.SessionDetails{
				SessionID:        "sess_fresh_2",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			// Simulate delayed network cancellation
			time.Sleep(100 * time.Millisecond)
			close(cancelDone)
			return nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("9999")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}
	if ctrl.GetCurrentSession().SessionID != "sess_old_1" {
		t.Fatalf("expected sess_old_1, got %s", ctrl.GetCurrentSession().SessionID)
	}

	// 2. User presses Esc to cancel -> returns to idle immediately while cancel is in-flight
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// 3. User rapidly re-enters PIN and confirms immediately (before cancel finishes)
	sim.EmitDigits("9999")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for controller to enter StatePaymentPending with fresh session
	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending on re-claim, got %s", ctrl.GetState())
	}

	// The active session must be the fresh sess_fresh_2, NOT the canceled sess_old_1!
	activeSess := ctrl.GetCurrentSession()
	if activeSess == nil || activeSess.SessionID != "sess_fresh_2" {
		t.Fatalf("expected active session sess_fresh_2 after rapid re-entry, got: %v", activeSess)
	}
}

// Regression Test 1: Slow cancellation (>2s) waits for completion and obtains fresh session.
func TestPackage03_SlowCancellation_WaitsAndObtainsFreshSession(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	rnd := &mockRenderer{}

	var claimCount int32
	cancelDone := make(chan struct{})

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_slow_cancel",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			count := atomic.AddInt32(&claimCount, 1)
			if count == 1 || count == 2 {
				return &cloud.SessionDetails{
					SessionID:        "sess_slow_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_slow_2",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			// Takes 2.2 seconds (> 2s unsafe wait from previous code)
			time.Sleep(2200 * time.Millisecond)
			close(cancelDone)
			return nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 5 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}
	if ctrl.GetCurrentSession().SessionID != "sess_slow_1" {
		t.Fatalf("expected sess_slow_1, got %s", ctrl.GetCurrentSession().SessionID)
	}

	// 2. Press Esc to cancel -> immediate idle while 2.2s cancel runs
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// 3. User rapidly re-enters PIN and submits
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for controller to wait out the 2.2s cancel and obtain fresh session sess_slow_2
	deadline = time.Now().Add(4 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending after slow cancel resolution, got %s", ctrl.GetState())
	}

	activeSess := ctrl.GetCurrentSession()
	if activeSess == nil || activeSess.SessionID != "sess_slow_2" {
		t.Fatalf("expected fresh session sess_slow_2 after slow cancel, got: %v", activeSess)
	}
}

// Regression Test 2: Cancellation failure fails safely without displaying QR.
func TestPackage03_CancellationFailure_FailsSafelyWithoutDisplayingQR(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	rnd := &mockRenderer{}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_fail_cancel",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_fail_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			time.Sleep(20 * time.Millisecond)
			return errors.New("simulated network cancellation failure")
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 2 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("5555")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// 2. Press Esc to cancel
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// 3. User rapidly re-enters PIN and submits
	sim.EmitDigits("5555")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for claim resolution
	time.Sleep(200 * time.Millisecond)

	// Must fail safely and remain on StateOrderSummary with error; must NOT enter StatePaymentPending
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary on cancellation failure, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError on cancellation failure")
	}

	last := rnd.LastState()
	if last.Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("expected not to render ModePaymentPending on cancellation failure")
	}
	if last.QRContent != "" {
		t.Fatalf("expected empty QRContent on cancellation failure, got %s", last.QRContent)
	}
}

// Regression Test 3: Cancellation timeout fails safely without displaying QR.
func TestPackage03_CancellationTimeout_FailsSafelyWithoutDisplayingQR(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	rnd := &mockRenderer{}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_to_cancel",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_hang_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			time.Sleep(2 * time.Second) // simulates hung/slow cancellation
			return nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 100 * time.Millisecond, // short wait timeout
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("7777")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// 2. Press Esc to cancel
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// 3. User rapidly re-enters PIN and submits
	sim.EmitDigits("7777")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for 100ms CancelWaitTimeout to fire and resolve
	time.Sleep(250 * time.Millisecond)

	// Must fail safely and remain on StateOrderSummary with error; must NOT enter StatePaymentPending
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary on cancellation timeout, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError on cancellation timeout")
	}

	last := rnd.LastState()
	if last.Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("expected not to render ModePaymentPending on cancellation timeout")
	}
	if last.QRContent != "" {
		t.Fatalf("expected empty QRContent on cancellation timeout, got %s", last.QRContent)
	}
}

func TestPackage03_CancellationFailure_RetryRecoversFreshSession(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var cancelAttempts int32
	var mu sync.Mutex
	activeSessionID := "sess_fail_1"

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_fail_retry",
				TotalPricePaise: 400,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			mu.Lock()
			sID := activeSessionID
			mu.Unlock()
			return &cloud.SessionDetails{
				SessionID:        sID,
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			if atomic.AddInt32(&cancelAttempts, 1) == 1 {
				return errors.New("network failure during cancellation")
			}
			mu.Lock()
			activeSessionID = "sess_fail_2"
			mu.Unlock()
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_fail_retry",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: time.Now().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 100 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("8888")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary, got %s", ctrl.GetState())
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_fail_1" {
		t.Fatalf("expected session sess_fail_1, got %+v", sess)
	}

	// 2. Press Esc to cancel -> immediate return to idle
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// Wait for background cancellation to fail and register in tracker
	deadline = time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt32(&cancelAttempts) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// 3. User re-enters PIN and attempts to start checkout
	sim.EmitDigits("8888")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary, got %s", ctrl.GetState())
	}

	// First attempt to confirm checkout after failed cancellation must fail closed
	sim.EmitSubmit()
	time.Sleep(150 * time.Millisecond)

	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected controller to stay in StateOrderSummary on failed cancellation, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError when previous cancellation failed")
	}
	if rnd.LastState().Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("must not render payment pending when previous cancellation failed")
	}

	// 4. Customer retries by pressing Confirm again; this reattempts cancellation and recovers fresh session
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending after retry, got %s", ctrl.GetState())
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_fail_2" {
		t.Fatalf("expected fresh session sess_fail_2 after recovery, got %+v", sess)
	}
	if ctrl.GetPINError() != "" {
		t.Fatalf("expected clean pinError after successful retry, got %s", ctrl.GetPINError())
	}
	last := rnd.LastState()
	if last.Mode != ui_ipc.ModePaymentPending {
		t.Fatalf("expected ModePaymentPending, got %s", last.Mode)
	}
	if last.QRContent == "" {
		t.Fatalf("expected non-empty QR content on recovered fresh session")
	}
}

func TestPackage03_CancellationTimeout_EventualCompletionAllowsFreshSession(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	cancelStarted := make(chan struct{})
	cancelRelease := make(chan struct{})
	var mu sync.Mutex
	activeSessionID := "sess_slow_1"

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_slow_eventual",
				TotalPricePaise: 400,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			mu.Lock()
			sID := activeSessionID
			mu.Unlock()
			return &cloud.SessionDetails{
				SessionID:        sID,
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			select {
			case <-cancelStarted:
			default:
				close(cancelStarted)
			}
			<-cancelRelease
			mu.Lock()
			activeSessionID = "sess_slow_2"
			mu.Unlock()
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_slow_eventual",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: time.Now().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 50 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("9999")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// 2. Cancel via Esc -> immediate return to idle
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// Wait until background cancellation has actually started
	select {
	case <-cancelStarted:
	case <-time.After(1 * time.Second):
		t.Fatalf("background cancellation did not start")
	}

	// 3. User rapidly re-enters PIN and submits checkout while cancellation is still blocked
	sim.EmitDigits("9999")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for 50ms CancelWaitTimeout to fire and resolve
	time.Sleep(150 * time.Millisecond)

	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary on cancellation timeout, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError on cancellation timeout")
	}

	// 4. Now allow the background cancellation to complete
	close(cancelRelease)

	// Wait briefly for the background cancel goroutine to complete and clean up the tracker
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.PendingCancelCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := ctrl.PendingCancelCount(); count != 0 {
		t.Fatalf("expected pending cancel count to reach 0 after completion, got %d", count)
	}

	// Assert that late cancellation completion did NOT invalidate or mutate the order summary screen
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("late cancellation completion mutated state to %s", ctrl.GetState())
	}

	// 5. Customer now presses Enter to retry; since cancellation finished cleanly, fresh session is granted
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending after retry, got %s", ctrl.GetState())
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_slow_2" {
		t.Fatalf("expected fresh session sess_slow_2, got %+v", sess)
	}
}

func TestPackage03_FailedCancellation_ProtectedAfter30sWhileSessionValid(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var cancelCalls int32
	baseTime := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentTime := baseTime

	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}

	advanceTime := func(d time.Duration) {
		clockMu.Lock()
		currentTime = currentTime.Add(d)
		clockMu.Unlock()
	}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_fail_30s",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    nowFn().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_fail_30s",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			atomic.AddInt32(&cancelCalls, 1)
			return errors.New("network error on cancel")
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_fail_30s",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: nowFn().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 100 * time.Millisecond,
		Now:               nowFn,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// 2. Press Esc to cancel -> immediate return to idle
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// Wait for background cancellation to execute and fail
	deadline = time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt32(&cancelCalls) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Tracker exists and failed
	if ctrl.PendingCancelCount() != 1 {
		t.Fatalf("expected 1 pending cancel tracker, got %d", ctrl.PendingCancelCount())
	}

	// 3. Advance clock by 45 seconds (> 30 seconds, but < 120 seconds PaymentTimeout)
	advanceTime(45 * time.Second)

	// In safe lifecycle tracking, it must remain protected because 120s session is still valid
	if ctrl.PendingCancelCount() != 1 {
		t.Fatalf("failed cancellation must remain protected after 45 seconds, got %d", ctrl.PendingCancelCount())
	}

	// 4. User re-enters PIN and attempts to start checkout
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()
	time.Sleep(150 * time.Millisecond)

	// Must fail closed on StateOrderSummary; must NOT reopen the failed session
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary on protected failed cancellation, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError when failed cancellation is protected")
	}
	if rnd.LastState().Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("must not render payment pending for protected failed session")
	}

	// 5. Advance clock past authoritative 120s session expiry (e.g. advance 80s more -> 125s total)
	advanceTime(80 * time.Second)

	// Now that authoritative session expiry has passed, cleanup can occur
	if count := ctrl.PendingCancelCount(); count != 0 {
		t.Fatalf("expected 0 pending cancel trackers after authoritative expiry, got %d", count)
	}
}

func TestPackage03_StaleClaimOrder_RejectedAfterSuccessfulCancelAndCleanup(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var claimCallCount int32
	baseTime := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentTime := baseTime

	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_stale_claim",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    nowFn().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			count := atomic.AddInt32(&claimCallCount, 1)
			// Call 1: initial checkout -> returns sess_stale_1
			// Call 2: user re-claims after cancellation -> backend returns stale sess_stale_1!
			// Call 3: controller reclaims -> backend returns fresh sess_stale_2!
			if count == 1 || count == 2 {
				return &cloud.SessionDetails{
					SessionID:        "sess_stale_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_stale_2",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			// Successful cancellation
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_stale_claim",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: nowFn().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 100 * time.Millisecond,
		Now:               nowFn,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending with sess_stale_1
	sim.EmitDigits("2345")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_stale_1" {
		t.Fatalf("expected session sess_stale_1, got %+v", sess)
	}

	// 2. Press Esc to cancel -> immediate return to idle
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// Wait for async cancel to complete and tracker cleanup to remove from pendingCancels
	deadline = time.Now().Add(500 * time.Millisecond)
	for ctrl.PendingCancelCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.PendingCancelCount() != 0 {
		t.Fatalf("expected tracker cleaned up from pendingCancels, got %d", ctrl.PendingCancelCount())
	}

	// 3. User re-enters PIN and submits checkout.
	// The backend ClaimOrder initially returns stale sess_stale_1.
	sim.EmitDigits("2345")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	// Wait for claim and reclaim to process
	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Must transition to StatePaymentPending WITH sess_stale_2, NEVER reopening sess_stale_1!
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending after reclaim, got %s", ctrl.GetState())
	}
	if sess := ctrl.GetCurrentSession(); sess == nil || sess.SessionID != "sess_stale_2" {
		t.Fatalf("stale session was reopened! expected fresh sess_stale_2, got %+v", sess)
	}
	if atomic.LoadInt32(&claimCallCount) < 3 {
		t.Fatalf("expected controller to reclaim fresh session, claim calls: %d", atomic.LoadInt32(&claimCallCount))
	}
}

func TestPackage03_RepeatedSessions_NoIndefiniteTrackerAccumulation(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var sessionSeq int64
	baseTime := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentTime := baseTime

	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}

	advanceTime := func(d time.Duration) {
		clockMu.Lock()
		currentTime = currentTime.Add(d)
		clockMu.Unlock()
	}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_repeated_clean",
				TotalPricePaise: 300,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    nowFn().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			seq := atomic.AddInt64(&sessionSeq, 1)
			return &cloud.SessionDetails{
				SessionID:        fmt.Sprintf("sess_rep_%d", seq),
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_rep",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: nowFn().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		Now:               nowFn,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Perform 5 successive checkout-and-cancel cycles
	for i := 0; i < 5; i++ {
		sim.EmitDigits("5555")
		sim.EmitSubmit()

		deadline := time.Now().Add(2 * time.Second)
		for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if ctrl.GetState() != StateOrderSummary {
			t.Fatalf("cycle %d: expected StateOrderSummary, got %s", i, ctrl.GetState())
		}

		sim.EmitSubmit()

		deadline = time.Now().Add(2 * time.Second)
		for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if ctrl.GetState() != StatePaymentPending {
			t.Fatalf("cycle %d: expected StatePaymentPending, got %s", i, ctrl.GetState())
		}

		sim.EmitCancel()

		deadline = time.Now().Add(1 * time.Second)
		for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if ctrl.GetState() != StateIdle {
			t.Fatalf("cycle %d: expected StateIdle after Esc, got %s", i, ctrl.GetState())
		}
	}

	// Advance clock past authoritative session expiry (120s)
	advanceTime(130 * time.Second)

	// Reset/prune triggers cleanup of expired sessions
	ctrl.ResetToIdle()

	if count := ctrl.PendingCancelCount(); count != 0 {
		t.Fatalf("expected 0 pending cancel trackers, got %d", count)
	}
	if count := ctrl.CanceledSessionCount(); count != 0 {
		t.Fatalf("expected 0 canceled session entries after authoritative expiry, got %d", count)
	}
}

func TestPackage03_OldSessionRemainsBlockedNearActualExpiry(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var cancelCalls int32
	baseTime := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentTime := baseTime

	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}

	advanceTime := func(d time.Duration) {
		clockMu.Lock()
		currentTime = currentTime.Add(d)
		clockMu.Unlock()
	}

	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_near_exp",
				TotalPricePaise: 400,
				TotalPages:      1,
				TotalSheets:     1,
				PINExpiresAt:    nowFn().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_near_exp",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			atomic.AddInt32(&cancelCalls, 1)
			return errors.New("network error on cancel")
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_near_exp",
				DemoToken: "dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				QRPayload: "https://kiosk.local/pay/dmo_0123456789abcdef0123456789abcdef0123456789abcdef",
				ExpiresAt: nowFn().Add(120 * time.Second).Format(time.RFC3339),
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 100 * time.Millisecond,
		Now:               nowFn,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN and confirm into StatePaymentPending
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StatePaymentPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StatePaymentPending {
		t.Fatalf("expected StatePaymentPending, got %s", ctrl.GetState())
	}

	// Simulate countdown having ticked down so locally rounded remaining-seconds counter is low (e.g. 5s)
	ctrl.mu.Lock()
	ctrl.currentRemainingSec = 5
	ctrl.mu.Unlock()

	// 2. Press Esc to cancel -> cancellation is initiated with full 120s protection
	sim.EmitCancel()
	deadline = time.Now().Add(1 * time.Second)
	for ctrl.GetState() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected StateIdle immediately after Esc, got %s", ctrl.GetState())
	}

	// Wait for background cancel to fail and be tracked
	deadline = time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt32(&cancelCalls) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Advance clock past the old 5s local counter, to 115s (near actual 120s expiry)
	advanceTime(115 * time.Second)

	// Session must REMAIN BLOCKED near its actual 120s expiry!
	if ctrl.PendingCancelCount() != 1 {
		t.Fatalf("session must remain blocked near 120s expiry (at 115s), got %d", ctrl.PendingCancelCount())
	}

	// Attempting to checkout must fail closed and never reopen the session
	sim.EmitDigits("1234")
	sim.EmitSubmit()

	deadline = time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sim.EmitSubmit()
	time.Sleep(150 * time.Millisecond)

	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary when blocked near expiry, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected retryable pinError when blocked near expiry")
	}
	if rnd.LastState().Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("must not render payment pending when blocked near expiry")
	}

	// 4. Advance clock past 120s (e.g. +10s -> 125s total)
	advanceTime(10 * time.Second)

	if count := ctrl.PendingCancelCount(); count != 0 {
		t.Fatalf("expected 0 pending cancel trackers after authoritative expiry, got %d", count)
	}
}

func TestPackage03_ZeroRemainingSeconds_NeverResetsCheckoutTime(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	rnd := &mockRenderer{}

	var claimCount int32
	mockClient := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_zero_sec",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     2,
				PINExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			atomic.AddInt32(&claimCount, 1)
			return &cloud.SessionDetails{
				SessionID:        "sess_zero_sec",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(-1 * time.Second).Format(time.RFC3339),
				SecondsRemaining: 0, // ClaimOrder returns 0 seconds remaining
			}, nil
		},
	}

	cfg := Config{
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      mockClient,
	}

	ctrl, err := NewController(cfg, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Enter PIN
	sim.EmitDigits("4321")
	sim.EmitSubmit()

	deadline := time.Now().Add(2 * time.Second)
	for ctrl.GetState() != StateOrderSummary && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected StateOrderSummary, got %s", ctrl.GetState())
	}

	// 2. Submit order summary
	sim.EmitSubmit()
	time.Sleep(150 * time.Millisecond)

	// Must reject safely and remain on StateOrderSummary with error; must NOT start or restart 120s checkout
	if ctrl.GetState() != StateOrderSummary {
		t.Fatalf("expected controller to reject SecondsRemaining <= 0 and remain on StateOrderSummary, got %s", ctrl.GetState())
	}
	if ctrl.GetPINError() == "" {
		t.Fatalf("expected error message when SecondsRemaining <= 0")
	}

	last := rnd.LastState()
	if last.Mode == ui_ipc.ModePaymentPending {
		t.Fatalf("must not render ModePaymentPending when SecondsRemaining <= 0")
	}
	if last.QRContent != "" {
		t.Fatalf("must not display payment QR when SecondsRemaining <= 0, got %s", last.QRContent)
	}
}

