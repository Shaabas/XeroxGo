package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
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
