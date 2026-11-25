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
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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
		releaseFn: func(ctx context.Context, orderID, sessionID string) error {
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
				ExpiresAt:        time.Now().Add(1 * time.Second).Format(time.RFC3339Nano),
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
				ExpiresAt:        time.Now().Add(10 * time.Second).Format(time.RFC3339Nano),
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
		rem := ctrl.checkoutRemainingSecondsLocked()
		ctrl.mu.Unlock()
		return rem > 0 && rem < 10
	})
	ctrl.mu.Lock()
	remBeforeRetry := ctrl.checkoutRemainingSecondsLocked()
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
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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
					ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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
					ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
					SecondsRemaining: 120,
				}, nil
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_overlap_2",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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

// 13: Already-paid orders must claim a valid session and pass session_id to ReleaseOrder without initiating payment.
func TestPackage04_PaidOrder_ClaimsAndReleasesWithSessionID(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var claimCalls int32
	var initCalls int32
	type releaseCall struct {
		orderID   string
		sessionID string
	}
	releasedCh := make(chan releaseCall, 1)

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
			return &cloud.SessionDetails{
				SessionID:        "sess_paid_release_1",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
				SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initCalls, 1)
			return nil, errors.New("must not initiate payment on already paid order")
		},
		releaseFn: func(ctx context.Context, orderID, sessionID string) error {
			releasedCh <- releaseCall{orderID: orderID, sessionID: sessionID}
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
	case rel := <-releasedCh:
		if rel.orderID != "ord_already_paid_1" {
			t.Fatalf("expected ReleaseOrder orderID=ord_already_paid_1, got %q", rel.orderID)
		}
		if rel.sessionID != "sess_paid_release_1" {
			t.Fatalf("expected ReleaseOrder sessionID=sess_paid_release_1, got %q", rel.sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ReleaseOrder on PAID_WAITING order")
	}

	waitUntil(t, time.Second, "return to StateIdle after release", func() bool {
		return ctrl.GetState() == StateIdle
	})

	if atomic.LoadInt32(&claimCalls) != 1 {
		t.Errorf("expected 1 ClaimOrder call before release, got %d", atomic.LoadInt32(&claimCalls))
	}
	if atomic.LoadInt32(&initCalls) != 0 {
		t.Errorf("expected 0 InitiatePayment calls for already-paid order, got %d", atomic.LoadInt32(&initCalls))
	}
}

// Paid-order release failure renders a visible error without silently returning to idle or auto-retrying printing.
func TestPackage04_PaidOrder_ReleaseFailureShowsVisibleErrorWithoutAutoRetry(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var releaseCalls int32
	var initCalls int32
	canceledCh := make(chan string, 1)

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_paid_rel_fail",
				Status:          string(contracts.JobStatusPaidWaiting),
				TotalPricePaise: 1200,
				TotalPages:      4,
				TotalSheets:     2,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_paid_rel_fail",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
				SecondsRemaining: 120,
			}, nil
		},
		releaseFn: func(ctx context.Context, orderID, sessionID string) error {
			atomic.AddInt32(&releaseCalls, 1)
			return cloud.ErrInsufficientPaper
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initCalls, 1)
			return nil, errors.New("must not initiate payment")
		},
		cancelFn: func(ctx context.Context, sessionID string) error {
			canceledCh <- sessionID
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

	waitUntil(t, time.Second, "visible release failure rendered", func() bool {
		last := rnd.LastState()
		return ctrl.GetState() == StatePaymentPending && last.PINError != ""
	})

	last := rnd.LastState()
	if last.QRContent != "" {
		t.Errorf("expected empty QRContent on paid-order release failure, got %q", last.QRContent)
	}
	if last.PINErrorRetry {
		t.Errorf("expected PINErrorRetry=false on paid-order release failure")
	}
	if !last.PrintReleaseFailed {
		t.Errorf("expected PrintReleaseFailed=true on paid-order release failure")
	}

	// Pressing Confirm or waiting must not auto-retry print release or initiate payment.
	sim.EmitSubmit()
	time.Sleep(60 * time.Millisecond)

	if ctrl.GetState() != StatePaymentPending {
		t.Errorf("expected StatePaymentPending to stay visible on release error, got %s", ctrl.GetState())
	}
	if got := atomic.LoadInt32(&releaseCalls); got != 1 {
		t.Errorf("expected exactly 1 ReleaseOrder attempt without auto-retry, got %d", got)
	}
	if got := atomic.LoadInt32(&initCalls); got != 0 {
		t.Errorf("expected 0 InitiatePayment calls, got %d", got)
	}

	// Esc clears back to idle and cancels the session safely.
	sim.EmitCancel()
	waitUntil(t, time.Second, "idle on Esc after release error", func() bool {
		return ctrl.GetState() == StateIdle
	})
	select {
	case sid := <-canceledCh:
		if sid != "sess_paid_rel_fail" {
			t.Errorf("expected sess_paid_rel_fail canceled on Esc, got %q", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session cancellation after release error Esc")
	}
}

// Delayed claim response consumes remaining cloud ExpiresAt time, and a delayed claim past ExpiresAt is rejected.
func TestPackage04_DelayedClaimResponse_ConsumesCloudDeadlineAndRejectsExpired(t *testing.T) {
	t.Run("partial consumption reduces checkout countdown", func(t *testing.T) {
		sim := input.NewSimulatedSource(16)
		defer sim.Close()
		rnd := &mockRenderer{}

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
		nowFn := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		advanceTime := func(d time.Duration) {
			clockMu.Lock()
			now = now.Add(d)
			clockMu.Unlock()
		}

		claimStarted := make(chan struct{})
		claimRelease := make(chan struct{})
		claimIssuedAt := now
		token := "dmo_1111222233334444555566667777888899990000aaaabbbb"
		expectedURL := "https://xeroxgo.vercel.app/pay/" + token

		client := &mockLookupClient{
			lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
				return &cloud.OrderDetails{
					OrderID:         "ord_delayed_claim",
					TotalPricePaise: 500,
					TotalPages:      2,
					TotalSheets:     1,
					Copies:          1,
				}, nil
			},
			claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
				close(claimStarted)
				<-claimRelease
				return &cloud.SessionDetails{
					SessionID:        "sess_delayed_claim",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					ExpiresAt:        claimIssuedAt.Add(10 * time.Second).Format(time.RFC3339Nano),
					SecondsRemaining: 10,
				}, nil
			},
			initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
				return &cloud.PaymentAttemptDetails{
					AttemptID: "att_delayed_claim",
					SessionID: sessionID,
					DemoToken: token,
					QRPayload: expectedURL,
				}, nil
			},
		}

		ctrl, err := NewController(Config{
			InactivityTimeout: 10 * time.Second,
			PaymentTimeout:    120 * time.Second,
			PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
			Now:               nowFn,
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

		sim.EmitDigits("5555")
		sim.EmitSubmit()

		<-claimStarted
		// Simulate 4s in-flight delay before claim response is processed.
		advanceTime(4 * time.Second)
		close(claimRelease)

		waitUntil(t, time.Second, "QR rendered after delayed claim", func() bool {
			return rnd.LastState().QRContent == expectedURL
		})

		last := rnd.LastState()
		if last.TimeoutSeconds != 6 {
			t.Errorf("expected remaining TimeoutSeconds=6 after 4s claim delay on 10s ExpiresAt, got %d", last.TimeoutSeconds)
		}
	})

	t.Run("delayed claim arriving past ExpiresAt is rejected", func(t *testing.T) {
		sim := input.NewSimulatedSource(16)
		defer sim.Close()
		rnd := &mockRenderer{}

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
		nowFn := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		advanceTime := func(d time.Duration) {
			clockMu.Lock()
			now = now.Add(d)
			clockMu.Unlock()
		}

		claimStarted := make(chan struct{})
		claimRelease := make(chan struct{})
		claimIssuedAt := now
		var initCalls int32

		client := &mockLookupClient{
			lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
				return &cloud.OrderDetails{
					OrderID:         "ord_delayed_expired",
					TotalPricePaise: 500,
					TotalPages:      1,
					TotalSheets:     1,
				}, nil
			},
			claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
				close(claimStarted)
				<-claimRelease
				return &cloud.SessionDetails{
					SessionID:        "sess_delayed_expired",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					ExpiresAt:        claimIssuedAt.Add(10 * time.Second).Format(time.RFC3339Nano),
					SecondsRemaining: 10,
				}, nil
			},
			initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
				atomic.AddInt32(&initCalls, 1)
				return nil, errors.New("must not initiate expired delayed claim")
			},
		}

		ctrl, err := NewController(Config{
			InactivityTimeout: 10 * time.Second,
			PaymentTimeout:    120 * time.Second,
			Now:               nowFn,
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

		sim.EmitDigits("5556")
		sim.EmitSubmit()

		<-claimStarted
		advanceTime(12 * time.Second)
		close(claimRelease)

		waitUntil(t, time.Second, "expired delayed claim error rendered", func() bool {
			return ctrl.GetPINError() != ""
		})

		if ctrl.GetCurrentSession() != nil {
			t.Errorf("expected nil currentSession for expired delayed claim, got %+v", ctrl.GetCurrentSession())
		}
		if atomic.LoadInt32(&initCalls) != 0 {
			t.Errorf("expected 0 InitiatePayment calls, got %d", atomic.LoadInt32(&initCalls))
		}
		if rnd.LastState().QRContent != "" {
			t.Errorf("expected empty QRContent, got %q", rnd.LastState().QRContent)
		}
	})
}

// Missing, malformed, past ExpiresAt, or zero SecondsRemaining must be rejected safely without initiating payment.
func TestPackage04_InvalidSessionDeadline_RejectedSafely(t *testing.T) {
	now := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		expiresAt        string
		secondsRemaining int64
	}{
		{name: "missing expires_at", expiresAt: "", secondsRemaining: 120},
		{name: "malformed expires_at", expiresAt: "not-a-timestamp", secondsRemaining: 120},
		{name: "past expires_at", expiresAt: now.Add(-1 * time.Second).Format(time.RFC3339Nano), secondsRemaining: 10},
		{name: "equal now expires_at", expiresAt: now.Format(time.RFC3339Nano), secondsRemaining: 10},
		{name: "zero seconds remaining", expiresAt: now.Add(60 * time.Second).Format(time.RFC3339Nano), secondsRemaining: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sim := input.NewSimulatedSource(16)
			defer sim.Close()
			rnd := &mockRenderer{}

			var initCalls int32
			client := &mockLookupClient{
				lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
					return &cloud.OrderDetails{
						OrderID:         "ord_invalid_deadline",
						TotalPricePaise: 500,
						TotalPages:      1,
						TotalSheets:     1,
					}, nil
				},
				claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
					return &cloud.SessionDetails{
						SessionID:        "sess_invalid_deadline",
						OrderID:          orderID,
						Status:           "PAYMENT_PENDING",
						ExpiresAt:        tc.expiresAt,
						SecondsRemaining: tc.secondsRemaining,
					}, nil
				},
				initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
					atomic.AddInt32(&initCalls, 1)
					return nil, errors.New("must not initiate for invalid deadline")
				},
			}

			ctrl, err := NewController(Config{
				InactivityTimeout: 10 * time.Second,
				PaymentTimeout:    120 * time.Second,
				Now:               func() time.Time { return now },
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

			sim.EmitDigits("6666")
			sim.EmitSubmit()

			waitUntil(t, time.Second, "expired deadline error rendered", func() bool {
				return ctrl.GetPINError() != ""
			})

			if ctrl.GetCurrentSession() != nil {
				t.Errorf("expected nil currentSession on invalid deadline, got %+v", ctrl.GetCurrentSession())
			}
			if atomic.LoadInt32(&initCalls) != 0 {
				t.Errorf("expected 0 InitiatePayment calls, got %d", atomic.LoadInt32(&initCalls))
			}
			if rnd.LastState().QRContent != "" {
				t.Errorf("expected empty QRContent, got %q", rnd.LastState().QRContent)
			}
		})
	}
}

// Retrying immediately before expiry preserves the authoritative deadline, then expires deterministically at deadline.
func TestPackage04_RetryBeforeExpiry_SucceedsWithoutExtendingDeadline(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceTime := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	var initCalls int32
	canceledCh := make(chan string, 1)
	token := "dmo_abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdef"
	expectedURL := "https://xeroxgo.vercel.app/pay/" + token

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_retry_before_exp",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_retry_before_exp",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        nowFn().Add(10 * time.Second).Format(time.RFC3339Nano),
				SecondsRemaining: 10,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			n := atomic.AddInt32(&initCalls, 1)
			if n == 1 {
				return &cloud.PaymentAttemptDetails{
					AttemptID: "att_bad",
					SessionID: sessionID,
					DemoToken: "bad",
				}, nil
			}
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_good",
				SessionID: sessionID,
				DemoToken: token,
				QRPayload: expectedURL,
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
		CountdownInterval: 25 * time.Millisecond,
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		Now:               nowFn,
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

	waitUntil(t, time.Second, "initial initiate failure", func() bool {
		return ctrl.GetPINError() != ""
	})

	// Advance clock near expiry (2s left) and trigger manual retry.
	advanceTime(8 * time.Second)
	sim.EmitSubmit()

	waitUntil(t, time.Second, "QR rendered on retry before expiry", func() bool {
		return rnd.LastState().QRContent == expectedURL
	})

	if got := rnd.LastState().TimeoutSeconds; got != 2 {
		t.Errorf("expected retry to retain 2s remaining without extending deadline, got %ds", got)
	}

	// Advance past authoritative deadline -> countdown expires and cancels session.
	advanceTime(3 * time.Second)
	select {
	case sid := <-canceledCh:
		if sid != "sess_retry_before_exp" {
			t.Errorf("expected sess_retry_before_exp canceled at deadline, got %q", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session expiry at deadline")
	}

	waitUntil(t, time.Second, "StateIdle after deadline expiry", func() bool {
		return ctrl.GetState() == StateIdle
	})
}

// Payment initiation response arriving after checkout deadline expires must be discarded without displaying a QR.
func TestPackage04_InitiateResponseAfterExpiry_DiscardedSafely(t *testing.T) {
	sim := input.NewSimulatedSource(16)
	defer sim.Close()
	rnd := &mockRenderer{}

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceTime := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	initStarted := make(chan struct{})
	initRelease := make(chan struct{})
	canceledCh := make(chan string, 1)
	lateToken := "dmo_fedcba0987654321fedcba0987654321fedcba0987654321"
	lateURL := "https://xeroxgo.vercel.app/pay/" + lateToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_init_after_exp",
				TotalPricePaise: 500,
				TotalPages:      1,
				TotalSheets:     1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			return &cloud.SessionDetails{
				SessionID:        "sess_init_after_exp",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        nowFn().Add(5 * time.Second).Format(time.RFC3339Nano),
				SecondsRemaining: 5,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			close(initStarted)
			<-initRelease
			return &cloud.PaymentAttemptDetails{
				AttemptID: "att_after_exp",
				SessionID: sessionID,
				DemoToken: lateToken,
				QRPayload: lateURL,
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
		CountdownInterval: time.Hour, // Isolate initiate response expiry check before countdown tick
		PaymentURLBase:    "https://xeroxgo.vercel.app/pay",
		Now:               nowFn,
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
	// Advance clock past session deadline before InitiatePayment completes.
	advanceTime(6 * time.Second)
	close(initRelease)

	select {
	case sid := <-canceledCh:
		if sid != "sess_init_after_exp" {
			t.Errorf("expected sess_init_after_exp canceled when initiate returned after expiry, got %q", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for late initiate session cancellation")
	}

	waitUntil(t, time.Second, "StateIdle after late initiate expiry", func() bool {
		return ctrl.GetState() == StateIdle
	})

	rnd.mu.Lock()
	for _, s := range rnd.states {
		if s.QRContent == lateURL {
			rnd.mu.Unlock()
			t.Fatalf("expired initiate QR %q was displayed", lateURL)
		}
	}
	rnd.mu.Unlock()
}

// Per-order claim semaphore: if Claim B times out waiting behind Claim A, Claim C still cannot bypass Claim A.
func TestPackage04_ClaimSerialization_TimedOutWaiterDoesNotBypassInFlightClaim(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()
	rnd := &mockRenderer{}

	var mu sync.Mutex
	var claimCalls int
	claim1Started := make(chan struct{})
	claim1Release := make(chan struct{})
	canceledSessions := make(map[string]int)

	freshToken := "dmo_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	freshURL := "https://xeroxgo.vercel.app/pay/" + freshToken

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_sem_timeout",
				TotalPricePaise: 700,
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
					SessionID:        "sess_sem_A",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
					SecondsRemaining: 120,
				}, nil
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_sem_C",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
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
				AttemptID: "att_sem_C",
				SessionID: sessionID,
				DemoToken: freshToken,
				QRPayload: freshURL,
			}, nil
		},
	}

	ctrl, err := NewController(Config{
		InactivityTimeout: 10 * time.Second,
		PaymentTimeout:    120 * time.Second,
		ClaimTimeout:      80 * time.Millisecond,
		CancelWaitTimeout: time.Second,
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

	// 1. Start Claim A and hold it inside ClaimOrder.
	sim.EmitDigits("9999")
	sim.EmitSubmit()
	<-claim1Started

	// 2. User cancels A and re-enters PIN -> Claim B queues behind A and times out at 80ms.
	sim.EmitCancel()
	waitUntil(t, time.Second, "StateIdle on Esc after Claim A", func() bool {
		return ctrl.GetState() == StateIdle
	})
	sim.EmitDigits("9999")
	sim.EmitSubmit()

	waitUntil(t, time.Second, "Claim B timed out waiting on Claim A", func() bool {
		return ctrl.GetPINError() != "" && ctrl.GetState() == StatePaymentPending
	})

	// 3. Before Claim A finishes, Claim B error screen retry starts Claim C!
	sim.EmitSubmit()
	waitUntil(t, time.Second, "Claim C entered claiming phase", func() bool {
		ctrl.mu.Lock()
		claiming := ctrl.isClaiming
		ctrl.mu.Unlock()
		return claiming && ctrl.GetPINError() == ""
	})

	// Wait slightly to prove Claim C is genuinely blocked on Claim A and did NOT bypass it.
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	callsBeforeRelease := claimCalls
	mu.Unlock()
	if callsBeforeRelease != 1 {
		t.Fatalf("Claim C bypassed in-flight Claim A! claimCalls=%d, want 1", callsBeforeRelease)
	}

	// 4. Release Claim A -> A cancels stale sess_sem_A, C acquires barrier and completes with sess_sem_C.
	close(claim1Release)

	waitUntil(t, 2*time.Second, "Claim C completes after Claim A releases", func() bool {
		sess := ctrl.GetCurrentSession()
		return sess != nil && sess.SessionID == "sess_sem_C" && rnd.LastState().QRContent == freshURL
	})

	mu.Lock()
	cA := canceledSessions["sess_sem_A"]
	cC := canceledSessions["sess_sem_C"]
	mu.Unlock()
	if cA == 0 {
		t.Errorf("expected stale sess_sem_A canceled, got %d", cA)
	}
	if cC != 0 {
		t.Errorf("fresh session sess_sem_C was erroneously canceled, count=%d", cC)
	}

	waitUntil(t, time.Second, "per-order claim map cleaned up", func() bool {
		ctrl.mu.Lock()
		n := len(ctrl.orderClaims)
		ctrl.mu.Unlock()
		return n == 0
	})
}

// Failed paid-order claim retains retry eligibility via Confirm, deduplicates in-flight retries, and releases without payment.
func TestPackage04_PaidOrder_ClaimFailureAllowsConfirmRetryAndReleases(t *testing.T) {
	sim := input.NewSimulatedSource(32)
	defer sim.Close()
	rnd := &mockRenderer{}

	var claimCalls int32
	var initCalls int32
	retryClaimStarted := make(chan struct{})
	retryClaimRelease := make(chan struct{})
	type releaseCall struct {
		orderID   string
		sessionID string
	}
	releasedCh := make(chan releaseCall, 1)

	client := &mockLookupClient{
		lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
			return &cloud.OrderDetails{
				OrderID:         "ord_paid_claim_retry",
				Status:          string(contracts.JobStatusPaidWaiting),
				TotalPricePaise: 1500,
				TotalPages:      3,
				TotalSheets:     2,
				Copies:          1,
			}, nil
		},
		claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
			n := atomic.AddInt32(&claimCalls, 1)
			if n == 1 {
				return nil, cloud.ErrInsufficientPaper
			}
			if n == 2 {
				close(retryClaimStarted)
				<-retryClaimRelease
			}
			return &cloud.SessionDetails{
				SessionID:        "sess_paid_retry_ok",
				OrderID:          orderID,
				Status:           "PAYMENT_PENDING",
				ExpiresAt:        time.Now().Add(120 * time.Second).Format(time.RFC3339Nano),
				SecondsRemaining: 120,
			}, nil
		},
		initiateFn: func(ctx context.Context, sessionID string) (*cloud.PaymentAttemptDetails, error) {
			atomic.AddInt32(&initCalls, 1)
			return nil, errors.New("must not initiate payment on PAID_WAITING order")
		},
		releaseFn: func(ctx context.Context, orderID, sessionID string) error {
			releasedCh <- releaseCall{orderID: orderID, sessionID: sessionID}
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

	sim.EmitDigits("4321")
	sim.EmitSubmit()

	waitUntil(t, time.Second, "paid-order claim failure rendered", func() bool {
		last := rnd.LastState()
		return ctrl.GetState() == StatePaymentPending && last.PINError != ""
	})

	last := rnd.LastState()
	if ctrl.GetCurrentSession() != nil {
		t.Fatalf("expected nil session on claim failure, got %+v", ctrl.GetCurrentSession())
	}
	if !last.PINErrorRetry {
		t.Errorf("expected PINErrorRetry=true when PAID_WAITING claim fails without session")
	}
	if last.PrintReleaseFailed {
		t.Errorf("expected PrintReleaseFailed=false when claim failed before session")
	}

	// Customer presses Confirm to retry claim; repeated presses during in-flight claim must be deduplicated.
	sim.EmitSubmit()
	<-retryClaimStarted
	sim.EmitSubmit()
	sim.EmitSubmit()
	time.Sleep(30 * time.Millisecond)

	if got := atomic.LoadInt32(&claimCalls); got != 2 {
		t.Fatalf("expected exactly 2 ClaimOrder calls during in-flight Confirm retry, got %d", got)
	}

	close(retryClaimRelease)

	select {
	case rel := <-releasedCh:
		if rel.orderID != "ord_paid_claim_retry" {
			t.Errorf("release orderID = %q, want ord_paid_claim_retry", rel.orderID)
		}
		if rel.sessionID != "sess_paid_retry_ok" {
			t.Errorf("release sessionID = %q, want sess_paid_retry_ok", rel.sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ReleaseOrder after Confirm retry")
	}

	waitUntil(t, time.Second, "StateIdle after retried paid-order release", func() bool {
		return ctrl.GetState() == StateIdle
	})

	if got := atomic.LoadInt32(&initCalls); got != 0 {
		t.Errorf("expected 0 InitiatePayment calls for PAID_WAITING retry flow, got %d", got)
	}
}

// Retaining a session after paid-order release failure bounds lifetime by authoritative deadline and expires cleanly.
func TestPackage04_PaidOrder_ReleaseFailureSessionExpiry(t *testing.T) {
	t.Run("retained session expires on bounded wall-clock inactivity deadline without extending expiry", func(t *testing.T) {
		sim := input.NewSimulatedSource(16)
		defer sim.Close()
		rnd := &mockRenderer{}

		canceledCh := make(chan string, 1)
		client := &mockLookupClient{
			lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
				return &cloud.OrderDetails{
					OrderID:         "ord_rel_exp_wall",
					Status:          string(contracts.JobStatusPaidWaiting),
					TotalPricePaise: 900,
					TotalPages:      2,
					TotalSheets:     1,
				}, nil
			},
			claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
				return &cloud.SessionDetails{
					SessionID:        "sess_rel_exp_wall",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					ExpiresAt:        time.Now().Add(200 * time.Millisecond).Format(time.RFC3339Nano),
					SecondsRemaining: 1,
				}, nil
			},
			releaseFn: func(ctx context.Context, orderID, sessionID string) error {
				return cloud.ErrPrinterError
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
			InactivityTimeout: 10 * time.Second, // Longer than session lifetime; must be bounded to ~200ms
			PaymentTimeout:    120 * time.Second,
			CountdownInterval: time.Hour, // Prove inactivity timer itself is bounded without waiting for ticks
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

		sim.EmitDigits("2424")
		sim.EmitSubmit()

		select {
		case sid := <-canceledCh:
			if sid != "sess_rel_exp_wall" {
				t.Errorf("expected sess_rel_exp_wall canceled at bounded deadline, got %q", sid)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for release-failure session expiration (inactivity timer was not bounded)")
		}

		waitUntil(t, time.Second, "StateIdle after bounded release-failure expiry", func() bool {
			return ctrl.GetState() == StateIdle
		})
	})

	t.Run("session whose deadline passes during ReleaseOrder expires immediately without lingering", func(t *testing.T) {
		sim := input.NewSimulatedSource(16)
		defer sim.Close()
		rnd := &mockRenderer{}

		var clockMu sync.Mutex
		now := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
		nowFn := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		advanceTime := func(d time.Duration) {
			clockMu.Lock()
			now = now.Add(d)
			clockMu.Unlock()
		}

		releaseStarted := make(chan struct{})
		releaseDone := make(chan struct{})
		canceledCh := make(chan string, 1)

		client := &mockLookupClient{
			lookupFn: func(ctx context.Context, pin string) (*cloud.OrderDetails, error) {
				return &cloud.OrderDetails{
					OrderID:         "ord_rel_exp_inflight",
					Status:          string(contracts.JobStatusPaidWaiting),
					TotalPricePaise: 900,
					TotalPages:      2,
					TotalSheets:     1,
				}, nil
			},
			claimFn: func(ctx context.Context, orderID string) (*cloud.SessionDetails, error) {
				return &cloud.SessionDetails{
					SessionID:        "sess_rel_exp_inflight",
					OrderID:          orderID,
					Status:           "PAYMENT_PENDING",
					ExpiresAt:        nowFn().Add(4 * time.Second).Format(time.RFC3339Nano),
					SecondsRemaining: 4,
				}, nil
			},
			releaseFn: func(ctx context.Context, orderID, sessionID string) error {
				close(releaseStarted)
				<-releaseDone
				return cloud.ErrPrinterOffline
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
			CountdownInterval: 25 * time.Millisecond,
			Now:               nowFn,
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

		sim.EmitDigits("2525")
		sim.EmitSubmit()

		<-releaseStarted
		// Advance clock past session's 4s deadline while ReleaseOrder is in flight.
		advanceTime(5 * time.Second)
		close(releaseDone)

		select {
		case sid := <-canceledCh:
			if sid != "sess_rel_exp_inflight" {
				t.Errorf("expected sess_rel_exp_inflight canceled immediately on post-deadline release failure, got %q", sid)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for immediate cancellation when deadline expired during release")
		}

		waitUntil(t, time.Second, "StateIdle immediately after expired release failure", func() bool {
			return ctrl.GetState() == StateIdle
		})
	})
}
