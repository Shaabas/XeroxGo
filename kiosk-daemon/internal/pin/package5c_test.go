package pin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

func TestPackage5C_AvailabilityBarrierBlocksInputAndCancelsInFlightCheckout(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	var screensMu sync.Mutex
	var screens []ui_ipc.ScreenState
	renderFn := func(s ui_ipc.ScreenState) error {
		screensMu.Lock()
		screens = append(screens, s)
		screensMu.Unlock()
		return nil
	}

	claimGate := make(chan struct{})
	var claimStarted atomic.Bool
	var canceledSessions []string
	var cancelMu sync.Mutex

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord-5c-pin",
				TotalPricePaise: 300,
				TotalPages:      1,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			claimStarted.Store(true)
			<-claimGate
			return &cloud.SessionDetails{
				SessionID:        "sess-5c-pin",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(2 * time.Minute).Format(time.RFC3339Nano),
				SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att-5c-pin",
				SessionID: sessionID,
				DemoToken: "dmo_1234567890abcdef1234567890abcdef1234567890abcdef",
				QRPayload: "https://xeroxgo.vercel.app/pay/dmo_1234567890abcdef1234567890abcdef1234567890abcdef",
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			cancelMu.Lock()
			canceledSessions = append(canceledSessions, sessionID)
			cancelMu.Unlock()
			return nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      renderFn,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. When blocked, keypad digits are ignored
	ctrl.SetAvailability(false, "printer fault")
	before := ctrl.eventsProcessed.Load()
	sim.EmitDigits("1234")
	waitUntil(t, time.Second, "blocked digits drained", func() bool {
		return ctrl.eventsProcessed.Load() >= before+4
	})
	if ctrl.GetPINBuffer() != "" || ctrl.GetState() != StateIdle {
		t.Fatalf("expected keypad input blocked while unavailable, got state=%s buf=%q", ctrl.GetState(), ctrl.GetPINBuffer())
	}

	// 2. Unblock -> start PIN lookup & in-flight claim -> then fault occurs mid-claim
	ctrl.SetAvailability(true, "")
	sim.EmitDigits("4455")
	sim.EmitSubmit()
	waitUntil(t, time.Second, "in-flight claim started", func() bool {
		return claimStarted.Load()
	})

	// Unavailability occurs while claim is in-flight
	ctrl.SetAvailability(false, "printer cover open")
	if ctrl.GetState() != StateIdle || !ctrl.IsBlocked() {
		t.Fatalf("expected controller reset to blocked IDLE on unavailability, got state=%s blocked=%v", ctrl.GetState(), ctrl.IsBlocked())
	}

	// Release late claim response -> MUST NOT restore PAYMENT_PENDING or QR screen!
	close(claimGate)
	time.Sleep(40 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Fatalf("expected late claim response discarded after unavailability, got state=%s", ctrl.GetState())
	}
	screensMu.Lock()
	for _, s := range screens {
		if s.Mode == ui_ipc.ModePaymentPending && s.QRContent != "" {
			t.Fatalf("late async response must not render payment QR while unavailable")
		}
	}
	screensMu.Unlock()

	// 3. Automatic recovery re-enables keypad input
	ctrl.SetAvailability(true, "")
	sim.EmitDigits("77")
	waitUntil(t, time.Second, "keypad input restored after recovery", func() bool {
		return ctrl.GetPINBuffer() == "77"
	})
	_ = canceledSessions
}

func TestPackage5C_LateLookupAndInitiateDiscardedOnUnavailability(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()

	var screensMu sync.Mutex
	var screens []ui_ipc.ScreenState
	renderFn := func(s ui_ipc.ScreenState) error {
		screensMu.Lock()
		screens = append(screens, s)
		screensMu.Unlock()
		return nil
	}

	// Case A: Late Lookup response arrives after SetAvailability(false)
	lookupGate := make(chan struct{})
	var lookupStarted, claimCalled atomic.Bool
	clientA := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			lookupStarted.Store(true)
			<-lookupGate
			return &cloud.OrderDetails{OrderID: "ord-late-lookup", TotalPricePaise: 300, TotalPages: 1, TotalSheets: 1, Copies: 1}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			claimCalled.Store(true)
			return &cloud.SessionDetails{SessionID: "sess-late-lookup", OrderID: orderID, Status: "PAYMENT_PENDING"}, nil
		},
	}

	ctrlA, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      renderFn,
		LookupClient:      clientA,
	}, sim)
	if err != nil {
		t.Fatalf("NewController A failed: %v", err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctrlA.Start(ctxA)
	defer ctrlA.Stop()

	sim.EmitDigits("1122")
	sim.EmitSubmit()
	waitUntil(t, time.Second, "lookup started", func() bool { return lookupStarted.Load() })
	ctrlA.SetAvailability(false, "printer offline")
	close(lookupGate)
	time.Sleep(40 * time.Millisecond)

	if ctrlA.GetState() != StateIdle || claimCalled.Load() {
		t.Fatalf("expected late lookup discarded without claiming session, state=%s claimCalled=%v", ctrlA.GetState(), claimCalled.Load())
	}

	// Case B: Late InitiatePayment response arrives after SetAvailability(false)
	simB := input.NewSimulatedSource(32)
	defer simB.Close()
	initGate := make(chan struct{})
	var initStarted atomic.Bool
	screensMu.Lock()
	screens = nil
	screensMu.Unlock()

	clientB := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{OrderID: "ord-late-init", TotalPricePaise: 300, TotalPages: 1, TotalSheets: 1, Copies: 1}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID: "sess-late-init", OrderID: orderID, Status: "PAYMENT_PENDING",
				ExpiresAt: time.Now().Add(2 * time.Minute).Format(time.RFC3339Nano), SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			initStarted.Store(true)
			<-initGate
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att-late-init", SessionID: sessionID,
				QRPayload: "https://xeroxgo.vercel.app/pay/dmo_late_init",
			}, nil
		},
	}

	ctrlB, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      renderFn,
		LookupClient:      clientB,
	}, simB)
	if err != nil {
		t.Fatalf("NewController B failed: %v", err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	ctrlB.Start(ctxB)
	defer ctrlB.Stop()

	simB.EmitDigits("3344")
	simB.EmitSubmit()
	waitUntil(t, time.Second, "initiate started", func() bool { return initStarted.Load() })
	ctrlB.SetAvailability(false, "cover open")
	close(initGate)
	time.Sleep(40 * time.Millisecond)

	if ctrlB.GetState() != StateIdle {
		t.Fatalf("expected late initiate discarded and controller in IDLE, got %s", ctrlB.GetState())
	}
	screensMu.Lock()
	for _, s := range screens {
		if s.Mode == ui_ipc.ModePaymentPending && s.QRContent != "" {
			t.Fatalf("late initiate response must not render payment QR while unavailable")
		}
	}
	screensMu.Unlock()
}
