package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func TestPackage5C_AvailabilityEndpointAndDirectAdmission(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-avail"
	ctx := context.Background()

	getAvail := func() (int, bool, string) {
		resp, err := http.Get(fmt.Sprintf("%s/api/k/%s/availability", ts.URL, kioskID))
		if err != nil {
			t.Fatalf("GET availability failed: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Available bool `json:"available"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Available, resp.Header.Get("Cache-Control")
	}

	// 1. No connection -> 503 available=false, Cache-Control: no-store
	code, avail, cc := getAvail()
	if code != http.StatusServiceUnavailable || avail || cc != "no-store" {
		t.Fatalf("expected 503 available=false no-store when disconnected, got %d %v %q", code, avail, cc)
	}

	// Direct conversion & preview requests cannot bypass unavailability
	tmpDir := t.TempDir()
	pdfPath := createTestPDFWithPages(t, tmpDir, 1)
	pdfBytes, _ := os.ReadFile(pdfPath)
	postConv := func() int {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		part, _ := w.CreateFormFile("file", "doc.pdf")
		_, _ = part.Write(pdfBytes)
		_ = w.WriteField("kiosk_id", kioskID)
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/conversions", body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/conversions failed: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := postConv(); c != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on direct /api/conversions while unavailable, got %d", c)
	}

	// Connect WS
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// 2. Healthy connected kiosk -> 200 available=true
	now := time.Now().UTC()
	hHealthy := makeHealthyHealth(50, 150, now)
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hHealthy})
	time.Sleep(40 * time.Millisecond)

	code, avail, cc = getAvail()
	if code != http.StatusOK || !avail || cc != "no-store" {
		t.Fatalf("expected 200 available=true no-store when healthy, got %d %v %q", code, avail, cc)
	}

	// 3. Healthy busy printer (State: PRINTING) remains available for mobile preparation
	now = now.Add(50 * time.Millisecond)
	hPrinting := makeHealthyHealth(45, 150, now)
	hPrinting.State = "PRINTING"
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hPrinting})
	time.Sleep(40 * time.Millisecond)

	code, avail, _ = getAvail()
	if code != http.StatusOK || !avail {
		t.Fatalf("expected healthy busy PRINTING kiosk to remain available, got %d %v", code, avail)
	}
	if c := postConv(); c != http.StatusAccepted {
		t.Fatalf("expected 202 on /api/conversions while PRINTING, got %d", c)
	}

	// 4. Hardware fault -> 503 available=false
	now = now.Add(50 * time.Millisecond)
	hFault := makeHealthyHealth(45, 150, now)
	hFault.Online = false
	hFault.State = "ERROR"
	hFault.EvaluatedHealth = contracts.EvaluatedHealthFault
	hFault.PaperJam = true
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hFault})
	time.Sleep(40 * time.Millisecond)

	if code, avail, _ = getAvail(); code != http.StatusServiceUnavailable || avail {
		t.Fatalf("expected 503 available=false on hardware fault, got %d %v", code, avail)
	}

	// 5. Uncalibrated inventory -> 503 available=false
	now = now.Add(50 * time.Millisecond)
	hUncalib := makeHealthyHealth(45, 150, now)
	hUncalib.InventoryCalibrated = false
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hUncalib})
	time.Sleep(40 * time.Millisecond)

	if code, avail, _ = getAvail(); code != http.StatusServiceUnavailable || avail {
		t.Fatalf("expected 503 available=false when uncalibrated, got %d %v", code, avail)
	}

	// 6. Stale evidence (>6s) -> 503 available=false
	staleTS := time.Now().UTC().Add(-10 * time.Second)
	hStale := makeHealthyHealth(45, 150, staleTS)
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hStale})
	time.Sleep(40 * time.Millisecond)

	if code, avail, _ = getAvail(); code != http.StatusServiceUnavailable || avail {
		t.Fatalf("expected 503 available=false on stale evidence, got %d %v", code, avail)
	}

	// 7. Recovery -> 200 available=true
	now = time.Now().UTC()
	hRecovered := makeHealthyHealth(45, 150, now)
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hRecovered})
	time.Sleep(40 * time.Millisecond)

	if code, avail, _ = getAvail(); code != http.StatusOK || !avail {
		t.Fatalf("expected 200 available=true after recovery, got %d %v", code, avail)
	}
	_ = app
}

func TestPackage5C_AutomaticUnpaidInvalidationAndCaptureSafety(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-invalidation"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	now := time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, now)})
	time.Sleep(40 * time.Millisecond)

	// Create an unpaid order with valid 10-min PIN and active checkout session + payment attempt
	ordID := "ord-5c-unpaid"
	ord := store.Order{
		OrderID:      ordID,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   300,
		TotalPages:   2,
		TotalSheets:  2,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []store.OrderDocument{
			{
				JobID:            "job-5c-unpaid",
				OrderID:          ordID,
				DocumentIndex:    0,
				OriginalFilename: "doc.pdf",
				DownloadURL:      "http://example.com/doc.pdf",
				SHA256Checksum:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Duplex:           contracts.DuplexSimplex,
				ColorMode:        "BW",
				TotalPages:       2,
				TotalSheets:      2,
				Copies:           1,
				PricePaise:       300,
			},
		},
		Command: contracts.PrintJobCommand{
			JobID:          "job-5c-unpaid",
			OrderID:        ordID,
			KioskID:        kioskID,
			DisplayCode:    "5511",
			DownloadURL:    "http://example.com/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     2,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			ColorMode:      "BW",
			StartFromPage:  1,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}
	att := store.PaymentAttempt{
		AttemptID:      "att-5c-unpaid",
		DemoToken:      "tok-5c-unpaid",
		OrderID:        ordID,
		SessionID:      sess.SessionID,
		KioskID:        kioskID,
		Provider:       "mock",
		AmountPaise:    300,
		Currency:       "INR",
		Status:         "CREATED",
		IdempotencyKey: "idem-5c-unpaid",
		ExpiresAt:      now.Add(2 * time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := app.db.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	// 1. Trigger hardware fault -> Hub must automatically cancel unpaid session without any customer request
	now = now.Add(50 * time.Millisecond)
	hFault := makeHealthyHealth(50, 150, now)
	hFault.Online = false
	hFault.State = "ERROR"
	hFault.EvaluatedHealth = contracts.EvaluatedHealthFault
	hFault.CoverOpen = true
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hFault})
	time.Sleep(60 * time.Millisecond)

	// Verify session was automatically canceled and payment attempt cannot capture
	gotSess, err := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
	if err != nil || gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
		t.Fatalf("expected session automatically canceled on fault, got %+v err=%v", gotSess, err)
	}

	reqPay, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/tok-5c-unpaid", nil)
	reqPay.Header.Set("Accept", "application/json")
	respPay, err := http.DefaultClient.Do(reqPay)
	if err != nil {
		t.Fatalf("POST /pay failed: %v", err)
	}
	respPay.Body.Close()
	if respPay.StatusCode != http.StatusServiceUnavailable && respPay.StatusCode != http.StatusGone {
		t.Fatalf("expected 503 or 410 rejecting canceled/unavailable payment, got %d", respPay.StatusCode)
	}

	// 2. On recovery, original unexpired PIN can claim a NEW checkout session
	now = time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, now)})
	time.Sleep(50 * time.Millisecond)

	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, ordID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+secret)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("claim after recovery failed: %v", err)
	}
	claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK claiming new session with original PIN after recovery, got %d", claimResp.StatusCode)
	}

	// 3. Disconnect automatically cancels the new unpaid session
	_ = conn.Close(websocket.StatusNormalClosure, "disconnect")
	time.Sleep(80 * time.Millisecond)
	activeSess, _ := app.db.GetActiveKioskSession(ctx, kioskID, ordID, time.Now().UTC())
	if activeSess != nil {
		t.Fatalf("expected no active unpaid session after WS disconnect, got %+v", activeSess)
	}

	// 4. Paid entitlement survives faults/disconnects and status remains retrievable
	paidOrdID := "ord-5c-paid"
	paidOrd := ord
	paidOrd.OrderID = paidOrdID
	paidOrd.Command.OrderID = paidOrdID
	paidOrd.Command.JobID = "job-5c-paid"
	paidOrd.Command.DisplayCode = "5522"
	paidOrd.Documents = []store.OrderDocument{ord.Documents[0]}
	paidOrd.Documents[0].OrderID = paidOrdID
	paidOrd.Documents[0].JobID = "job-5c-paid"
	if err := app.db.CreateOrder(ctx, paidOrd); err != nil {
		t.Fatalf("CreateOrder(paidOrd) failed: %v", err)
	}
	// Reconnect healthy briefly to create session and capture payment
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 2 failed: %v", err)
	}
	defer conn2.CloseNow()
	now = time.Now().UTC()
	_ = wsjson.Write(ctx, conn2, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, now)})
	time.Sleep(40 * time.Millisecond)

	paidSess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, paidOrdID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck(paid) failed: %v", err)
	}
	paidAtt := store.PaymentAttempt{
		AttemptID:      "att-5c-paid",
		DemoToken:      "tok-5c-paid",
		OrderID:        paidOrdID,
		SessionID:      paidSess.SessionID,
		KioskID:        kioskID,
		Provider:       "mock",
		AmountPaise:    300,
		Currency:       "INR",
		Status:         "CREATED",
		IdempotencyKey: "idem-5c-paid",
		ExpiresAt:      now.Add(2 * time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := app.db.CreatePaymentAttempt(ctx, paidAtt); err != nil {
		t.Fatalf("CreatePaymentAttempt(paid) failed: %v", err)
	}
	reqPaid, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/tok-5c-paid", nil)
	reqPaid.Header.Set("Accept", "application/json")
	respPaid, err := http.DefaultClient.Do(reqPaid)
	if err != nil || respPaid.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK capturing paid order, got err=%v resp=%v", err, respPaid)
	}
	respPaid.Body.Close()

	// Disconnect now -> paid order entitlement MUST survive and remain retrievable!
	_ = conn2.Close(websocket.StatusNormalClosure, "disconnect after paid")
	time.Sleep(60 * time.Millisecond)

	gotPaidOrd, err := app.db.GetOrder(ctx, paidOrdID)
	if err != nil || gotPaidOrd == nil || (gotPaidOrd.Status != contracts.JobStatusPaidWaiting && gotPaidOrd.Status != contracts.JobStatusDispatched) {
		t.Fatalf("expected paid order entitlement preserved after disconnect, got %+v err=%v", gotPaidOrd, err)
	}
	reqStatus, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/orders/%s/status", ts.URL, paidOrdID), nil)
	respStatus, err := http.DefaultClient.Do(reqStatus)
	if err != nil || respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on order status while unavailable, got err=%v resp=%v", err, respStatus)
	}
	respStatus.Body.Close()
}

func TestPackage5C_FreshHubRestartInvalidatesPersistedUnpaidCheckout(t *testing.T) {
	app1, _, secret1 := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-restart"
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed an unpaid checkout session in DB as if active before backend restart
	ordID := "ord-5c-restart"
	if err := app1.db.CreateOrder(ctx, store.Order{
		OrderID:      ordID,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   300,
		TotalPages:   1,
		TotalSheets:  1,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []store.OrderDocument{{
			JobID: "job-5c-restart", OrderID: ordID, DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
		}},
		Command: contracts.PrintJobCommand{
			JobID: "job-5c-restart", OrderID: ordID, KioskID: kioskID, DisplayCode: "7741",
			TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := app1.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	// Simulate backend restart: fresh Hub on the same DB
	hub2 := ws.NewHub(app1.db)
	hub2.SetKioskSecret([]byte(secret1))
	ts2 := httptest.NewServer(hub2)
	t.Cleanup(ts2.Close)
	wsURL := strings.Replace(ts2.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret1
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial after restart failed: %v", err)
	}
	defer conn.CloseNow()

	// Even before or right as healthy telemetry arrives, the persisted unpaid session must be invalidated
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
	time.Sleep(50 * time.Millisecond)

	gotSess, err := app1.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
	if err != nil || gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
		t.Fatalf("expected persisted unpaid session canceled on new connection after restart, got %+v err=%v", gotSess, err)
	}
}

func TestPackage5C_DelayedTelemetryAuthoritativeExpiry_AndUnresolvedCalibrationInvalidation(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-expiry"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send delayed telemetry whose observation is already 5.8s old (only ~200ms of freshness left!)
	delayedObs := time.Now().UTC().Add(-5800 * time.Millisecond)
	hDelayed := makeHealthyHealth(50, 150, delayedObs)
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hDelayed})
	time.Sleep(30 * time.Millisecond)

	now := time.Now().UTC()
	ordID := "ord-5c-expiry"
	if err := app.db.CreateOrder(ctx, store.Order{
		OrderID:      ordID,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   300,
		TotalPages:   1,
		TotalSheets:  1,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []store.OrderDocument{{
			JobID: "job-5c-expiry", OrderID: ordID, DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
		}},
		Command: contracts.PrintJobCommand{
			JobID: "job-5c-expiry", OrderID: ordID, KioskID: kioskID, DisplayCode: "7742",
			TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	// Wait ~350ms with NO further frames -> silent expiry must invalidate the unpaid session
	// using the earliest authoritative expiry (NOT restarting a full 6s window)!
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gs, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
		if gs != nil && gs.Status == store.KioskSessionCanceled {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	gotSess, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
	if gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
		t.Fatalf("expected silent telemetry expiry at earliest authoritative deadline to cancel unpaid session, got %+v", gotSess)
	}

	// Now recover and create a new session, then start an unresolved refill -> must invalidate unpaid checkout!
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
	time.Sleep(40 * time.Millisecond)
	sess2, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck(2) failed: %v", err)
	}
	shortCtx, shortCancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer shortCancel()
	_ = app.hub.SendRefill(shortCtx, kioskID, 10)
	time.Sleep(40 * time.Millisecond)
	gotSess2, _ := app.db.GetKioskSession(ctx, sess2.SessionID, time.Now().UTC())
	if gotSess2 == nil || gotSess2.Status != store.KioskSessionCanceled {
		t.Fatalf("expected unresolved calibration to invalidate active unpaid checkout, got %+v", gotSess2)
	}
}

func TestPackage5C_StaleTimerAndConnectionReplacementPreservesNewSession(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-stale-timer"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 1 failed: %v", err)
	}
	defer conn1.CloseNow()

	// Send near-expiring telemetry on conn1 (~180ms remaining)
	delayedObs := time.Now().UTC().Add(-5820 * time.Millisecond)
	_ = wsjson.Write(ctx, conn1, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, delayedObs)})
	time.Sleep(30 * time.Millisecond)

	// Immediately replace with conn2 and send fresh healthy telemetry
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 2 failed: %v", err)
	}
	defer conn2.CloseNow()
	_ = wsjson.Write(ctx, conn2, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
	time.Sleep(40 * time.Millisecond)

	now := time.Now().UTC()
	ordID := "ord-5c-stale-timer"
	if err := app.db.CreateOrder(ctx, store.Order{
		OrderID:      ordID,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   300,
		TotalPages:   1,
		TotalSheets:  1,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []store.OrderDocument{{
			JobID: "job-5c-stale-timer", OrderID: ordID, DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
		}},
		Command: contracts.PrintJobCommand{
			JobID: "job-5c-stale-timer", OrderID: ordID, KioskID: kioskID, DisplayCode: "7743",
			TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sessNew, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	// Wait past conn1's old expiry deadline (~250ms): conn1's stale timer MUST NOT cancel sessNew!
	time.Sleep(250 * time.Millisecond)
	gotSess, err := app.db.GetKioskSession(ctx, sessNew.SessionID, time.Now().UTC())
	if err != nil || gotSess == nil || gotSess.Status != store.KioskSessionPaymentPending {
		t.Fatalf("expected new session on conn2 to remain PAYMENT_PENDING after conn1 timer deadline, got %+v err=%v", gotSess, err)
	}
}

func TestPackage5C_ConcurrentCaptureVsCancellationOrderings(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5c-race"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
	time.Sleep(40 * time.Millisecond)

	for i := 0; i < 6; i++ {
		now := time.Now().UTC()
		ordID := fmt.Sprintf("ord-5c-race-%d", i)
		tok := fmt.Sprintf("tok-5c-race-%d", i)
		if err := app.db.CreateOrder(ctx, store.Order{
			OrderID:      ordID,
			Status:       contracts.JobStatusPendingPayment,
			PricePaise:   300,
			TotalPages:   1,
			TotalSheets:  1,
			ColorMode:    "BW",
			PINExpiresAt: now.Add(10 * time.Minute),
			Documents: []store.OrderDocument{{
				JobID: fmt.Sprintf("job-5c-race-%d", i), OrderID: ordID, DocumentIndex: 0,
				OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
				TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
			}},
			Command: contracts.PrintJobCommand{
				JobID: fmt.Sprintf("job-5c-race-%d", i), OrderID: ordID, KioskID: kioskID, DisplayCode: fmt.Sprintf("88%02d", i),
				TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
			},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateOrder(%d) failed: %v", i, err)
		}
		sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
		if err != nil {
			t.Fatalf("CreateOrGetKioskSessionWithPaperCheck(%d) failed: %v", i, err)
		}
		att := store.PaymentAttempt{
			AttemptID:      fmt.Sprintf("att-5c-race-%d", i),
			DemoToken:      tok,
			OrderID:        ordID,
			SessionID:      sess.SessionID,
			KioskID:        kioskID,
			Provider:       "mock",
			AmountPaise:    300,
			Currency:       "INR",
			Status:         "CREATED",
			IdempotencyKey: fmt.Sprintf("idem-5c-race-%d", i),
			ExpiresAt:      now.Add(2 * time.Minute),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := app.db.CreatePaymentAttempt(ctx, att); err != nil {
			t.Fatalf("CreatePaymentAttempt(%d) failed: %v", i, err)
		}

		startCh := make(chan struct{})
		payStatusCh := make(chan int, 1)
		go func() {
			<-startCh
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/"+tok, nil)
			req.Header.Set("Accept", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				payStatusCh <- -1
				return
			}
			resp.Body.Close()
			payStatusCh <- resp.StatusCode
		}()
		go func() {
			<-startCh
			_ = app.db.CancelActiveUnpaidSessionsForKiosk(ctx, kioskID, time.Now().UTC())
		}()
		close(startCh)

		payStatus := <-payStatusCh
		gotSess, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
		gotOrd, _ := app.db.GetOrder(ctx, ordID)
		if payStatus == http.StatusOK {
			if gotSess == nil || gotSess.Status != store.KioskSessionCompleted || gotOrd == nil || gotOrd.Status == contracts.JobStatusPendingPayment {
				t.Fatalf("iteration %d: HTTP 200 capture must commit paid order & COMPLETED session, got sess=%+v ord=%+v", i, gotSess, gotOrd)
			}
		} else if payStatus == http.StatusGone || payStatus == http.StatusServiceUnavailable {
			if gotSess == nil || gotSess.Status != store.KioskSessionCanceled || gotOrd == nil || gotOrd.Status != contracts.JobStatusPendingPayment {
				t.Fatalf("iteration %d: rejected capture (%d) must leave session CANCELED and order PENDING_PAYMENT, got sess=%+v ord=%+v", i, payStatus, gotSess, gotOrd)
			}
		} else {
			t.Fatalf("iteration %d: unexpected POST /pay status %d", i, payStatus)
		}
	}
}

func TestPackage5C_RefillResolutionRestoresExpiryTimerAndRejectsStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		acceptOK       bool
		alreadyStaleAt time.Duration
	}{
		{name: "accepted_refill", acceptOK: true, alreadyStaleAt: -5200 * time.Millisecond},
		{name: "rejected_refill", acceptOK: false, alreadyStaleAt: -5200 * time.Millisecond},
		{name: "accepted_refill_already_stale", acceptOK: true, alreadyStaleAt: -5920 * time.Millisecond},
		{name: "rejected_refill_already_stale", acceptOK: false, alreadyStaleAt: -5920 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, ts, secret := setupPackage5BTestServer(t)
			kioskID := "kiosk-5c-" + tc.name
			ctx := context.Background()

			wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
			conn, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Fatalf("ws dial failed: %v", err)
			}
			defer conn.CloseNow()

			obsAt := time.Now().UTC().Add(tc.alreadyStaleAt)
			_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, obsAt)})
			time.Sleep(40 * time.Millisecond)

			// Start SendRefill in background and respond from kiosk mock.
			refillErrCh := make(chan error, 1)
			go func() {
				rCtx, rCancel := context.WithTimeout(ctx, 2*time.Second)
				defer rCancel()
				refillErrCh <- app.hub.SendRefill(rCtx, kioskID, 80)
			}()

			var reqMsg struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
			}
			for reqMsg.Type != "refill" {
				if err := wsjson.Read(ctx, conn, &reqMsg); err != nil {
					t.Fatalf("failed reading refill request: %v", err)
				}
			}
			if tc.alreadyStaleAt < -5900*time.Millisecond {
				// Let cached evidence cross the 6s window before ack arrives.
				time.Sleep(100 * time.Millisecond)
			}
			ack := map[string]interface{}{
				"type":       "refill_ack",
				"request_id": reqMsg.RequestID,
				"ok":         tc.acceptOK,
			}
			if tc.acceptOK {
				ack["calibrated"] = true
				ack["sheets"] = 80
				ack["inventory_seq"] = uint64(2)
			} else {
				ack["error"] = "operator rejected"
			}
			if err := wsjson.Write(ctx, conn, ack); err != nil {
				t.Fatalf("failed writing refill_ack: %v", err)
			}
			refillErr := <-refillErrCh
			if tc.acceptOK && refillErr != nil {
				t.Fatalf("expected nil SendRefill err on accept, got %v", refillErr)
			}
			if !tc.acceptOK && refillErr == nil {
				t.Fatalf("expected non-nil SendRefill err on reject")
			}

			if tc.alreadyStaleAt < -5900*time.Millisecond {
				_, _, _, ready, reason := app.hub.EvaluateKioskReadiness(kioskID)
				if ready {
					t.Fatalf("expected kiosk to remain unavailable when cached evidence is already stale at %s, got ready=true (%s)", tc.name, reason)
				}
				return
			}

			// Evidence still has ~700ms remaining: create an unpaid checkout now.
			now := time.Now().UTC()
			ordID := "ord-" + tc.name
			if err := app.db.CreateOrder(ctx, store.Order{
				OrderID:      ordID,
				Status:       contracts.JobStatusPendingPayment,
				PricePaise:   300,
				TotalPages:   1,
				TotalSheets:  1,
				ColorMode:    "BW",
				PINExpiresAt: now.Add(10 * time.Minute),
				Documents: []store.OrderDocument{{
					JobID: "job-" + ordID, OrderID: ordID, DocumentIndex: 0,
					OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
					TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
				}},
				Command: contracts.PrintJobCommand{
					JobID: "job-" + ordID, OrderID: ordID, KioskID: kioskID, DisplayCode: "7755",
					TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
				},
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateOrder failed: %v", err)
			}
			sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
			if err != nil {
				t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
			}

			// Wait silently (no more health frames) -> restored expiry timer MUST cancel sess!
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				gs, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
				if gs != nil && gs.Status == store.KioskSessionCanceled {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			gotSess, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
			if gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
				t.Fatalf("expected silent telemetry expiry after %s to cancel unpaid checkout, got %+v", tc.name, gotSess)
			}
		})
	}
}

func TestPackage5C_ControlledCaptureVsAvailabilityCancellationBothOrderings(t *testing.T) {
	ctx := context.Background()

	// Ordering A: Availability-driven cancellation wins against payment capture.
	t.Run("availability_cancellation_wins", func(t *testing.T) {
		app, ts, secret := setupPackage5BTestServer(t)
		kioskID := "kiosk-5c-order-a"
		wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial failed: %v", err)
		}
		defer conn.CloseNow()
		_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
		time.Sleep(40 * time.Millisecond)

		now := time.Now().UTC()
		ordID := "ord-5c-order-a"
		if err := app.db.CreateOrder(ctx, store.Order{
			OrderID: ordID, Status: contracts.JobStatusPendingPayment, PricePaise: 300, TotalPages: 1, TotalSheets: 1, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
			Documents: []store.OrderDocument{{JobID: "job-5c-order-a", OrderID: ordID, DocumentIndex: 0, OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW", TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300}},
			Command:   contracts.PrintJobCommand{JobID: "job-5c-order-a", OrderID: ordID, KioskID: kioskID, DisplayCode: "8811", TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateOrder failed: %v", err)
		}
		sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
		if err != nil {
			t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
		}
		if err := app.db.CreatePaymentAttempt(ctx, store.PaymentAttempt{
			AttemptID: "att-5c-order-a", DemoToken: "tok-5c-order-a", OrderID: ordID, SessionID: sess.SessionID,
			KioskID: kioskID, Provider: "mock", AmountPaise: 300, Currency: "INR", Status: "CREATED",
			IdempotencyKey: "idem-5c-order-a", ExpiresAt: now.Add(2 * time.Minute), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreatePaymentAttempt failed: %v", err)
		}

		// Disconnect WS to trigger availability-driven cancellation first, then attempt capture.
		_ = conn.Close(websocket.StatusNormalClosure, "fault disconnect")
		time.Sleep(60 * time.Millisecond)

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/tok-5c-order-a", nil)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /pay failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusGone {
			t.Fatalf("expected 503 or 410 when availability cancellation won, got %d", resp.StatusCode)
		}
		gotSess, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
		gotOrd, _ := app.db.GetOrder(ctx, ordID)
		if gotSess == nil || gotSess.Status != store.KioskSessionCanceled || gotOrd == nil || gotOrd.Status != contracts.JobStatusPendingPayment {
			t.Fatalf("expected CANCELED session & PENDING_PAYMENT order, got sess=%+v ord=%+v", gotSess, gotOrd)
		}
	})

	// Ordering B: Payment capture wins before availability-driven cancellation, preserving paid entitlement & dispatch intent.
	t.Run("payment_capture_wins", func(t *testing.T) {
		app, ts, secret := setupPackage5BTestServer(t)
		kioskID := "kiosk-5c-order-b"
		wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial failed: %v", err)
		}
		defer conn.CloseNow()
		_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": makeHealthyHealth(50, 150, time.Now().UTC())})
		time.Sleep(40 * time.Millisecond)

		now := time.Now().UTC()
		ordID := "ord-5c-order-b"
		if err := app.db.CreateOrder(ctx, store.Order{
			OrderID: ordID, Status: contracts.JobStatusPendingPayment, PricePaise: 300, TotalPages: 1, TotalSheets: 1, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
			Documents: []store.OrderDocument{{JobID: "job-5c-order-b", OrderID: ordID, DocumentIndex: 0, OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW", TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300}},
			Command:   contracts.PrintJobCommand{JobID: "job-5c-order-b", OrderID: ordID, KioskID: kioskID, DisplayCode: "8822", TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateOrder failed: %v", err)
		}
		sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
		if err != nil {
			t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
		}
		if err := app.db.CreatePaymentAttempt(ctx, store.PaymentAttempt{
			AttemptID: "att-5c-order-b", DemoToken: "tok-5c-order-b", OrderID: ordID, SessionID: sess.SessionID,
			KioskID: kioskID, Provider: "mock", AmountPaise: 300, Currency: "INR", Status: "CREATED",
			IdempotencyKey: "idem-5c-order-b", ExpiresAt: now.Add(2 * time.Minute), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreatePaymentAttempt failed: %v", err)
		}

		// Hold LockKioskReservation? No, complete capture first while healthy, then immediately disconnect (availability-driven cancellation).
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/tok-5c-order-b", nil)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on capture before fault, got err=%v resp=%v", err, resp)
		}
		resp.Body.Close()

		// Now trigger availability-driven invalidation via WS disconnect: paid entitlement & dispatch intent MUST survive!
		_ = conn.Close(websocket.StatusNormalClosure, "post-capture fault")
		time.Sleep(60 * time.Millisecond)

		gotSess, _ := app.db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
		gotOrd, _ := app.db.GetOrder(ctx, ordID)
		intentStatus, _, _ := app.db.GetJobDispatchIntentStatus(ctx, "job-5c-order-b")
		if gotSess == nil || gotSess.Status != store.KioskSessionCompleted {
			t.Fatalf("expected COMPLETED session preserved after capture+disconnect, got %+v", gotSess)
		}
		if gotOrd == nil || gotOrd.Status == contracts.JobStatusPendingPayment {
			t.Fatalf("expected paid order status preserved after capture+disconnect, got %+v", gotOrd)
		}
		if intentStatus == "" {
			t.Fatalf("expected dispatch intent preserved after capture+disconnect, got empty")
		}
	})
}
