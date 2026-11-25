package pin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/cloud"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", desc)
	}
}

// 1, 2, 3, 8: Single PIN submit auto-claims without countdown during loading, initiates payment once, renders one dynamic QR, and ignores Enter on valid QR.
func TestPackage04_DirectCheckout_LifecycleAndSingleQR(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()
	rnd := &mockRenderer{}

	var claimCalls int32
	var initiateCalls int32
	var releaseCalls int32
	var cancelCalls int32

	claimStarted := make(chan struct{})
	claimRelease := make(chan struct{})
	initStarted := make(chan struct{})
	initRelease := make(chan struct{})

	token := "dmo_abcdef0123456789abcdef0123456789abcdef0123456789"
	expectedURL := "https://xeroxgo.vercel.app/pay/" + token

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:          "ord_pkg4_single_1",
				KioskID:          "kiosk-1",
				Status:           "PENDING_PAYMENT",
				TotalPricePaise:  3500,
				TotalPages:       7,
				TotalSheets:      4,
				Copies:           1,
				Duplex:           "long_edge",
				ColorMode:        "mono",
				PINExpiresAt:     time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				SecondsRemaining: 600,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			atomic.AddInt32(&claimCalls, 1)
			close(claimStarted)
			<-claimRelease
			return &cloud.SessionDetails{
				SessionID:        "sess_pkg4_single_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  3500,
				SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initiateCalls, 1)
			close(initStarted)
			<-initRelease
			return &cloud.PaymentAttemptDetails{
				AttemptID:   "att_pkg4_single_1",
				SessionID:   sessionID,
				OrderID:     "ord_pkg4_single_1",
				DemoToken:   token,
				QRPayload:   expectedURL,
				AmountPaise: 3500,
			}, nil
		},
		releaseFn: func(ctx context.Context, orderID string) error {
			atomic.AddInt32(&releaseCalls, 1)
			return nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			atomic.AddInt32(&cancelCalls, 1)
			return nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// Single PIN submission
	sim.EmitDigits("2468")
	sim.EmitSubmit()

	// Requirement 3: During claim loading, checkout screen renders PINLoading=true and TimeoutSeconds=0 (no fabricated countdown)
	<-claimStarted
	waitUntil(t, time.Second, "claiming checkout state", func() bool {
		last := rnd.LastState()
		return last.Mode == ui_ipc.ModePaymentPending && last.PINLoading
	})
	claimingState := rnd.LastState()
	if claimingState.TimeoutSeconds != 0 {
		t.Errorf("expected TimeoutSeconds=0 during claim loading, got %d", claimingState.TimeoutSeconds)
	}
	if claimingState.SessionID != "" || claimingState.QRContent != "" {
		t.Errorf("expected empty SessionID and QRContent during claim loading, got sess=%q qr=%q", claimingState.SessionID, claimingState.QRContent)
	}

	// Release claim -> transitions to initiation phase ("Generating QR...") with valid session countdown
	close(claimRelease)
	<-initStarted
	waitUntil(t, time.Second, "initiating checkout state", func() bool {
		last := rnd.LastState()
		return last.Mode == ui_ipc.ModePaymentPending && !last.PINLoading && last.SessionID == "sess_pkg4_single_1"
	})
	initState := rnd.LastState()
	if initState.TimeoutSeconds <= 0 {
		t.Errorf("expected positive TimeoutSeconds after session acquired, got %d", initState.TimeoutSeconds)
	}
	if initState.QRContent != "" {
		t.Errorf("expected empty QRContent before initiation completes, got %q", initState.QRContent)
	}

	// Release initiation -> renders exact dynamic checkout QR
	close(initRelease)
	waitUntil(t, time.Second, "ready QR state", func() bool {
		return rnd.LastState().QRContent == expectedURL
	})

	if atomic.LoadInt32(&claimCalls) != 1 {
		t.Errorf("expected 1 ClaimOrder call, got %d", atomic.LoadInt32(&claimCalls))
	}
	if atomic.LoadInt32(&initiateCalls) != 1 {
		t.Errorf("expected 1 InitiatePayment call, got %d", atomic.LoadInt32(&initiateCalls))
	}

	// Requirement 8: Pressing Enter when valid QR is displayed must NOT authorize payment or initiate again
	sim.EmitSubmit()
	sim.EmitSubmit()
	time.Sleep(40 * time.Millisecond)

	if ctrl.GetState() != StatePaymentPending {
		t.Errorf("expected StatePaymentPending to persist on Confirm with active QR, got %s", ctrl.GetState())
	}
	if atomic.LoadInt32(&releaseCalls) != 0 {
		t.Errorf("Confirm with valid QR must not call ReleaseOrder, got %d", atomic.LoadInt32(&releaseCalls))
	}
	if atomic.LoadInt32(&initiateCalls) != 1 {
		t.Errorf("Confirm with valid QR must not trigger another InitiatePayment, got %d", atomic.LoadInt32(&initiateCalls))
	}
	if atomic.LoadInt32(&cancelCalls) != 0 {
		t.Errorf("expected 0 cancels during active checkout, got %d", atomic.LoadInt32(&cancelCalls))
	}
}

// 4: Checkout expiry respects the shorter cloud session deadline (not a fixed 120s override).
func TestPackage04_CheckoutExpiryRespectsCloudDeadline(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	canceledCh := make(chan string, 1)
	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_short_deadline",
				TotalPricePaise: 500,
				TotalPages:      2,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_short_1s",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 1, // Much shorter than PaymentTimeout (120s)
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			select {
			case canceledCh <- sessionID:
			default:
			}
			return nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CountdownInterval: 200 * time.Millisecond,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("1111")
	sim.EmitSubmit()

	waitUntil(t, time.Second, "session countdown <= 1s rendered", func() bool {
		last := rnd.LastState()
		return last.Mode == ui_ipc.ModePaymentPending && last.SessionID == "sess_short_1s" && last.TimeoutSeconds == 1
	})

	// Must expire and cancel at ~1s rather than waiting 120s
	select {
	case sid := <-canceledCh:
		if sid != "sess_short_1s" {
			t.Fatalf("expected sess_short_1s to be canceled at session deadline, got %q", sid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for checkout expiry at cloud session deadline (1s)")
	}

	waitUntil(t, time.Second, "return to StateIdle after deadline expiry", func() bool {
		return ctrl.GetState() == StateIdle
	})
}

// 6 & 7: Payment initiation failure permits retry without extending expiry; repeated Enter presses do not create concurrent requests.
func TestPackage04_InitiateRetry_PreservesDeadlineAndDeduplicatesEnter(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()
	rnd := &mockRenderer{}

	var initCalls int32
	var claimCalls int32
	claimGate := make(chan struct{})
	retryStarted := make(chan struct{})
	retryRelease := make(chan struct{})

	token := "dmo_1234567890abcdef1234567890abcdef1234567890abcdef"
	expectedURL := "https://xeroxgo.vercel.app/pay/" + token

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_init_retry",
				TotalPricePaise: 900,
				TotalPages:      3,
				TotalSheets:     2,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			atomic.AddInt32(&claimCalls, 1)
			<-claimGate
			return &cloud.SessionDetails{
				SessionID:        "sess_init_retry",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 10,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			n := atomic.AddInt32(&initCalls, 1)
			if n == 1 {
				// Return invalid token to trigger immediate failure without 2s retry sleep loop if error != nil?
				// Wait: controller retries 3 times on pErr != nil || attempt == nil; returning attempt with invalid token stops immediately!
				return &cloud.PaymentAttemptDetails{
					AttemptID: "att_bad",
					SessionID: sessionID,
					DemoToken: "bad_token",
				}, nil
			}
			if n == 2 {
				close(retryStarted)
				<-retryRelease
			}
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_ok",
				SessionID: sessionID,
				DemoToken: token,
				QRPayload: expectedURL,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CountdownInterval: 40 * time.Millisecond,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("3333")
	sim.EmitSubmit()

	// Requirement 7a: Repeated Enter presses during in-flight claim must be ignored (only 1 ClaimOrder call)
	waitUntil(t, time.Second, "claiming started", func() bool {
		return atomic.LoadInt32(&claimCalls) == 1
	})
	sim.EmitSubmit()
	sim.EmitSubmit()
	sim.EmitSubmit()
	close(claimGate)

	expectedErr := "Failed to load payment QR. Press Confirm to retry."
	waitUntil(t, time.Second, "initiation error rendered", func() bool {
		return ctrl.GetPINError() == expectedErr
	})
	if atomic.LoadInt32(&claimCalls) != 1 {
		t.Fatalf("repeated Enter during claim caused %d claims, want 1", atomic.LoadInt32(&claimCalls))
	}

	// Let countdown tick down below initial 10s so we can prove retry never extends remaining seconds
	waitUntil(t, time.Second, "countdown ticked below 10", func() bool {
		ctrl.mu.Lock()
		rem := ctrl.currentRemainingSec
		ctrl.mu.Unlock()
		return rem > 0 && rem < 9
	})
	ctrl.mu.Lock()
	remBeforeRetry := ctrl.currentRemainingSec
	ctrl.mu.Unlock()

	// Requirement 7b: Rapid repeated Enter presses during retry initiation only create 1 concurrent request
	sim.EmitSubmit()
	<-retryStarted
	sim.EmitSubmit()
	sim.EmitSubmit()
	sim.EmitSubmit()
	time.Sleep(30 * time.Millisecond)

	if got := atomic.LoadInt32(&initCalls); got != 2 {
		t.Fatalf("repeated Enter during retry initiated %d requests, want 2 total", got)
	}

	close(retryRelease)
	waitUntil(t, time.Second, "QR recovered on retry", func() bool {
		return rnd.LastState().QRContent == expectedURL && ctrl.GetPINError() == ""
	})

	last := rnd.LastState()
	if last.TimeoutSeconds > remBeforeRetry {
		t.Errorf("retry extended checkout countdown from %ds to %ds", remBeforeRetry, last.TimeoutSeconds)
	}
}

// 9: Esc during an in-flight claim safely returns to Idle, skips payment initiation, and cancels late returned session.
func TestPackage04_EscDuringInFlightClaim_CancelsLateSession(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	claimStarted := make(chan struct{})
	claimRelease := make(chan struct{})
	canceledCh := make(chan string, 1)
	var initiateCalls int32

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_esc_claim",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			close(claimStarted)
			<-claimRelease
			return &cloud.SessionDetails{
				SessionID:        "sess_late_after_esc",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			canceledCh <- sessionID
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initiateCalls, 1)
			return nil, errors.New("must not initiate")
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("7777")
	sim.EmitSubmit()

	<-claimStarted

	// Customer presses Esc while ClaimOrder is still in flight
	sim.EmitCancel()
	waitUntil(t, time.Second, "StateIdle immediately on Esc", func() bool {
		return ctrl.GetState() == StateIdle
	})

	// Late ClaimOrder response arrives
	close(claimRelease)

	select {
	case sid := <-canceledCh:
		if sid != "sess_late_after_esc" {
			t.Fatalf("expected CancelSession for sess_late_after_esc, got %q", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for late claim session cancellation")
	}

	if ctrl.GetState() != StateIdle {
		t.Errorf("expected controller to remain in StateIdle, got %s", ctrl.GetState())
	}
	if atomic.LoadInt32(&initiateCalls) != 0 {
		t.Errorf("expected 0 InitiatePayment calls for canceled claim, got %d", atomic.LoadInt32(&initiateCalls))
	}
}

// 10: Esc during payment initiation cannot display a stale QR when late response returns.
func TestPackage04_EscDuringPaymentInitiation_DiscardsStaleQR(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	initStarted := make(chan struct{})
	initRelease := make(chan struct{})
	canceledCh := make(chan string, 1)
	staleToken := "dmo_aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666"
	staleURL := "https://xeroxgo.vercel.app/pay/" + staleToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_esc_init",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_esc_init",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			close(initStarted)
			<-initRelease
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_stale",
				SessionID: sessionID,
				DemoToken: staleToken,
				QRPayload: staleURL,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			canceledCh <- sessionID
			return nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("8888")
	sim.EmitSubmit()

	<-initStarted
	sim.EmitCancel()

	waitUntil(t, time.Second, "StateIdle on Esc", func() bool {
		return ctrl.GetState() == StateIdle
	})

	select {
	case sid := <-canceledCh:
		if sid != "sess_esc_init" {
			t.Fatalf("expected sess_esc_init canceled, got %q", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CancelSession")
	}

	// Late initiation returns valid QR -> must be discarded completely
	close(initRelease)
	time.Sleep(50 * time.Millisecond)

	if ctrl.GetState() != StateIdle {
		t.Errorf("stale initiation response changed state to %s", ctrl.GetState())
	}
	rnd.mu.Lock()
	for _, s := range rnd.states {
		if s.QRContent == staleURL {
			rnd.mu.Unlock()
			t.Fatalf("stale QR %q was displayed after Esc", staleURL)
		}
	}
	rnd.mu.Unlock()
}

// 11: Overlapping slow claim + rapid Esc/re-entry serializes claims so late cancellation cannot cancel newer checkout.
func TestPackage04_OverlappingClaimSerialization_RapidReentryProtected(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()
	rnd := &mockRenderer{}

	var mu sync.Mutex
	var claimCalls int
	claim1Started := make(chan struct{})
	claim1Release := make(chan struct{})
	canceledSessions := make(map[string]int)

	freshToken := "dmo_9999888877776666555544443333222211110000aaaabbbb"
	freshURL := "https://xeroxgo.vercel.app/pay/" + freshToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_overlap",
				TotalPricePaise: 600,
				TotalPages:      2,
				TotalSheets:     1,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			mu.Lock()
			claimCalls++
			n := claimCalls
			mu.Unlock()

			if n == 1 {
				close(claim1Started)
				<-claim1Release
				return &cloud.SessionDetails{
					SessionID:        "sess_overlap_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			// Second claim returns old sess_overlap_1 if not canceled yet; reclaim returns fresh sess_overlap_2
			mu.Lock()
			c1Done := canceledSessions["sess_overlap_1"] > 0
			mu.Unlock()
			if n == 2 || !c1Done {
				return &cloud.SessionDetails{
					SessionID:        "sess_overlap_1",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					SecondsRemaining: 120,
				}, nil
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_overlap_2",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				SecondsRemaining: 120,
			}, nil
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			mu.Lock()
			canceledSessions[sessionID]++
			mu.Unlock()
			return nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_overlap",
				SessionID: sessionID,
				DemoToken: freshToken,
				QRPayload: freshURL,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		CancelWaitTimeout: 2 * time.Second,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	// 1. Start Claim 1 (blocked)
	sim.EmitDigits("1234")
	sim.EmitSubmit()
	<-claim1Started

	// 2. Customer hits Esc while Claim 1 is in flight, then immediately re-enters same PIN!
	sim.EmitCancel()
	waitUntil(t, time.Second, "StateIdle on Esc", func() bool {
		return ctrl.GetState() == StateIdle
	})

	sim.EmitDigits("1234")
	sim.EmitSubmit()

	// Claim 2 should now be waiting for Claim 1's serialized slot while displaying loading
	waitUntil(t, time.Second, "second checkout entering claiming phase", func() bool {
		return ctrl.GetState() == StatePaymentPending
	})

	// 3. Release Claim 1 -> Claim 1 sees stale seq, cancels sess_overlap_1, then Claim 2 runs and reclaims fresh sess_overlap_2
	close(claim1Release)

	waitUntil(t, 2*time.Second, "fresh session sess_overlap_2 active with QR", func() bool {
		sess := ctrl.GetCurrentSession()
		return sess != nil && sess.SessionID == "sess_overlap_2" && rnd.LastState().QRContent == freshURL
	})

	mu.Lock()
	c1 := canceledSessions["sess_overlap_1"]
	c2 := canceledSessions["sess_overlap_2"]
	mu.Unlock()
	if c1 == 0 {
		t.Errorf("expected stale sess_overlap_1 to be canceled, got %d", c1)
	}
	if c2 != 0 {
		t.Errorf("fresh session sess_overlap_2 was erroneously canceled! count=%d", c2)
	}
}

// 13: Already-paid orders retain ReleaseOrder behavior without claiming or initiating another payment.
func TestPackage04_AlreadyPaidOrder_ReleasesWithoutPayment(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var claimCalls int32
	var initCalls int32
	releasedCh := make(chan string, 1)

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_already_paid_1",
				Status:          string(contracts.JobStatusPaidWaiting),
				TotalPricePaise: 1200,
				TotalPages:      4,
				TotalSheets:     2,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			atomic.AddInt32(&claimCalls, 1)
			return nil, errors.New("must not claim already paid order")
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initCalls, 1)
			return nil, errors.New("must not initiate payment on already paid order")
		},
		releaseFn: func(ctx context.Context, orderID string) error {
			releasedCh <- orderID
			return nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		RenderScreen:      rnd.Render,
		LookupClient:      client,
	}, sim)
	if err != nil {
		t.Fatalf("NewController failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	defer ctrl.Stop()

	sim.EmitDigits("4444")
	sim.EmitSubmit()

	select {
	case oid := <-releasedCh:
		if oid != "ord_already_paid_1" {
			t.Fatalf("expected ReleaseOrder for ord_already_paid_1, got %q", oid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ReleaseOrder on PAID_WAITING order")
	}

	waitUntil(t, time.Second, "return to StateIdle after release", func() bool {
		return ctrl.GetState() == StateIdle
	})

	if atomic.LoadInt32(&claimCalls) != 0 {
		t.Errorf("expected 0 ClaimOrder calls for already-paid order, got %d", atomic.LoadInt32(&claimCalls))
	}
	if atomic.LoadInt32(&initCalls) != 0 {
		t.Errorf("expected 0 InitiatePayment calls for already-paid order, got %d", atomic.LoadInt32(&initCalls))
	}
}
