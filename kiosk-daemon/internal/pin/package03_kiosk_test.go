package pin

import (
	"context"
	"errors"
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
