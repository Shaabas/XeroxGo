package pin

import (
	"context"
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
