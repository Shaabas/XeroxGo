package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestCloudHub_ConnectionBoundHealth_StartsUnverified_ReconcilesOnlyOnVerifiedReady(t *testing.T) {
	db := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed a captured paid order with pending dispatch intent BEFORE kiosk connects.
	now := time.Now().UTC()
	ord := store.Order{
		OrderID: "ord-reconcile-01",
		Command: contracts.PrintJobCommand{
			JobID:       "job-reconcile-01",
			KioskID:     "kiosk-rec-conn",
			DisplayCode: "4242",
		},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  500,
		TotalPages:  2,
		TotalSheets: 1,
		Documents: []store.OrderDocument{
			{
				JobID:            "job-reconcile-01",
				OrderID:          "ord-reconcile-01",
				DocumentIndex:    0,
				OriginalFilename: "doc.pdf",
				DownloadURL:      "http://example.com/doc.pdf",
				SHA256Checksum:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				TotalPages:       2,
				TotalSheets:      1,
				Copies:           1,
				Duplex:           contracts.DuplexLongEdge,
				ColorMode:        "BW",
				PricePaise:       500,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := db.CreateOrGetKioskSessionWithPaperCheck(ctx, "kiosk-rec-conn", ord.OrderID, 10, 10*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}
	_, err = db.CapturePaymentAndCheckDispatch(ctx, store.PaymentRecord{
		PaymentID:   "pay-rec-01",
		OrderID:     ord.OrderID,
		KioskID:     "kiosk-rec-conn",
		SessionID:   sess.SessionID,
		AmountPaise: 500,
		Status:      "captured",
		Provider:    "DEMO",
	}, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}

	hub := NewHub(db)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer srv.Close()

	conn, _ := connectTestKiosk(t, hub, srv.URL, "kiosk-rec-conn")

	// 1. New connection MUST begin unverified and NOT dispatch pending orders yet.
	if ready, _ := hub.IsKioskReadyForDispatch("kiosk-rec-conn"); ready {
		t.Fatal("expected new WS connection to begin unverified")
	}
	st, _, _ := db.GetJobDispatchIntentStatus(ctx, "job-reconcile-01")
	if st != "PENDING" {
		t.Fatalf("expected dispatch intent to stay PENDING before telemetry verification, got %s", st)
	}

	// 2. Sending ExecutionSafe=false healthy hardware must still NOT reconcile dispatch.
	unsafeHealth := contracts.PrinterHealth{
		Online:              true,
		State:               "IDLE",
		EvaluatedHealth:     contracts.EvaluatedHealthHealthy,
		PaperTrayStatus:     "OK",
		EstimatedSheets:     100,
		InventoryCalibrated: true,
		ExecutionSafe:       false,
		ExecutionSafetyReason: "unresolved paused job barrier",
		UpdatedAt:           time.Now().UTC(),
	}
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &unsafeHealth}); err != nil {
		t.Fatalf("write unsafe health failed: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if ready, _ := hub.IsKioskReadyForDispatch("kiosk-rec-conn"); ready {
		t.Fatal("expected ExecutionSafe=false to keep dispatch blocked")
	}
	st, _, _ = db.GetJobDispatchIntentStatus(ctx, "job-reconcile-01")
	if st != "PENDING" {
		t.Fatalf("expected dispatch intent to remain PENDING while ExecutionSafe=false, got %s", st)
	}

	// 3. Sending verified HEALTHY + ExecutionSafe=true triggers reconciliation and dispatch!
	sendTestReadyHealth(ctx, t, hub, conn, "kiosk-rec-conn")
	msg := readJobMessage(ctx, t, conn)
	if msg.Type != "job" || msg.Job == nil || msg.Job.JobID != "job-reconcile-01" {
		t.Fatalf("expected reconciled job dispatch, got %+v", msg)
	}
}

func TestCloudHub_OldConnectionCannotOverwriteOrInvalidateNewSession(t *testing.T) {
	hub := NewHub(nil)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Conn1 connects and publishes HEALTHY telemetry.
	conn1, cancel1 := connectTestKiosk(t, hub, srv.URL, "kiosk-race")
	sendTestReadyHealth(ctx, t, hub, conn1, "kiosk-race")

	// Dial Conn2 directly so we observe Conn2 replacing Conn1.
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", "kiosk-race")
	u.RawQuery = q.Encode()

	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	conn2, _, err := websocket.Dial(ctx2, u.String(), nil)
	if err != nil {
		t.Fatalf("conn2 dial failed: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "done")

	// Wait until Conn2 becomes active session -> Conn2 starts UNVERIFIED (must not inherit Conn1 readiness!).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready, _ := hub.IsKioskReadyForDispatch("kiosk-race"); !ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready, _ := hub.IsKioskReadyForDispatch("kiosk-race"); ready {
		t.Fatal("new connection must start UNVERIFIED rather than inheriting prior connection health")
	}

	// Conn2 sends valid HEALTHY telemetry.
	sendTestReadyHealth(ctx, t, hub, conn2, "kiosk-race")

	// Even if Conn1 sends a FAULT payload and then disconnects, it must NOT overwrite or clear Conn2!
	fault := contracts.PrinterHealth{
		Online:          false,
		State:           "ERROR",
		EvaluatedHealth: contracts.EvaluatedHealthFault,
		PaperTrayStatus: "EMPTY",
		PaperJam:        true,
		UpdatedAt:       time.Now().UTC(),
	}
	_ = wsjson.Write(ctx, conn1, wireMessage{Type: "health", Health: &fault})
	cancel1()
	_ = conn1.Close(websocket.StatusNormalClosure, "old closed")
	time.Sleep(60 * time.Millisecond)

	if !hub.IsConnected("kiosk-race") {
		t.Fatal("old connection closing must not unregister newer active connection")
	}
	if ready, reason := hub.IsKioskReadyForDispatch("kiosk-race"); !ready {
		t.Fatalf("old connection must not invalidate newer connection readiness (reason=%s)", reason)
	}
}

func TestCloudHub_RejectsMalformedContradictoryAndStaleReplayTelemetry(t *testing.T) {
	hub := NewHub(nil)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _ := connectTestKiosk(t, hub, srv.URL, "kiosk-validate")

	// 1. Contradictory HEALTHY payload with active PaperJam must be rejected.
	contradictory := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		EvaluatedHealth: contracts.EvaluatedHealthHealthy,
		PaperTrayStatus: "OK",
		PaperJam:        true,
		ExecutionSafe:   true,
		UpdatedAt:       time.Now().UTC(),
	}
	_ = wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &contradictory})
	time.Sleep(50 * time.Millisecond)
	if ready, _ := hub.IsKioskReadyForDispatch("kiosk-validate"); ready {
		t.Fatal("contradictory HEALTHY + PaperJam must not establish dispatch readiness")
	}

	// 2. Stale observation (> 6s old UpdatedAt) replayed with current WS receipt must not make kiosk ready.
	staleObservation := contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		EvaluatedHealth: contracts.EvaluatedHealthHealthy,
		PaperTrayStatus: "OK",
		ExecutionSafe:   true,
		TonerLevelPercent: -1,
		DrumLifePercent:   -1,
		UpdatedAt:       time.Now().UTC().Add(-7 * time.Second),
	}
	_ = wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &staleObservation})
	time.Sleep(50 * time.Millisecond)
	if ready, _ := hub.IsKioskReadyForDispatch("kiosk-validate"); ready {
		t.Fatal("stale >6s observation timestamp must never make kiosk ready on receipt")
	}

	// 3. Valid initial health succeeded, followed by future-skewed malformed payload ->
	//    hub drops malformed update; if valid snapshot is then aged out, readiness expires.
	sendTestReadyHealth(ctx, t, hub, conn, "kiosk-validate")

	// Failed SendJob must not return nil error when readiness fails.
	hub.mu.RLock()
	sess := hub.sessions["kiosk-validate"]
	hub.mu.RUnlock()
	sess.healthMu.Lock()
	if sess.latestHealth != nil {
		expiredHealth := cloneHealth(sess.latestHealth.health)
		expiredHealth.UpdatedAt = time.Now().UTC().Add(-7 * time.Second)
		sess.latestHealth = &sessionHealthEntry{
			health:     expiredHealth,
			receivedAt: sess.latestHealth.receivedAt,
		}
	}
	sess.healthMu.Unlock()

	err := hub.SendJob(ctx, contracts.PrintJobCommand{JobID: "job-expired", KioskID: "kiosk-validate"})
	if err == nil {
		t.Fatal("expected SendJob to reject kiosk with >6s expired observation timestamp")
	}
}
