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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func setupPackage5BTestServer(t *testing.T) (*appServer, *httptest.Server, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "p5b.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("mkdir jobs failed: %v", err)
	}

	kioskSecret := "p5b-kiosk-secret-123456"
	cfg := &serverConfig{
		addr:           "localhost:0",
		dbPath:         dbPath,
		jobsDir:        jobsDir,
		kioskSecret:    []byte(kioskSecret),
		downloadSecret: []byte("p5b-download-secret-123456"),
		devMode:        false,
		duplexEnabled:  false,
	}

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kioskSecret))
	_ = st.SetDetailedPricing(context.Background(), 300, 500, 800, 1000, false)
	app := &appServer{
		cfg:      cfg,
		db:       st,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := app.routes()

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return app, ts, kioskSecret
}

func makeHealthyHealth(sheets int, capacity int, now time.Time) contracts.PrinterHealth {
	return contracts.PrinterHealth{
		Online:                 true,
		State:                  "IDLE",
		EvaluatedHealth:        contracts.EvaluatedHealthHealthy,
		PaperTrayStatus:        "OK",
		EstimatedSheets:        sheets,
		ConfiguredTrayCapacity: capacity,
		InventoryCalibrated:    true,
		ExecutionSafe:          true,
		TonerLevelKnown:        true,
		TonerLevelPercent:      90,
		DrumLifeKnown:          true,
		DrumLifePercent:        95,
		EvidenceFreshness:      contracts.BuildUniformEvidenceFreshness(now, contracts.DefaultTelemetryMaxAge),
		UpdatedAt:              now,
	}
}

func TestPackage5B_RefillEndpointValidationAndAckRequired(t *testing.T) {
	_, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-refill-api"

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send initial health with ConfiguredTrayCapacity = 150
	now := time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": makeHealthyHealth(10, 150, now),
	})
	time.Sleep(50 * time.Millisecond)

	postRefill := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/refill", ts.URL, kioskID), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST refill failed: %v", err)
		}
		return resp
	}

	// 1. Reject missing, fractional, negative, and >150 counts without 250 fallback
	for _, badBody := range []string{`{}`, `{"sheets": null}`, `{"sheets": 10.5}`, `{"sheets": -1}`, `{"sheets": 151}`, `{"sheets": 250}`} {
		resp := postRefill(badBody)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for %s, got %d", badBody, resp.StatusCode)
		}
	}

	// 2. If Pi rejects refill_ack, endpoint must fail (409 Conflict)
	go func() {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Sheets    int    `json:"sheets"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err == nil && msg.Type == "refill" {
			_ = wsjson.Write(ctx, conn, map[string]interface{}{
				"type":       "refill_ack",
				"request_id": msg.RequestID,
				"ok":         false,
				"error":      "active job in progress",
			})
		}
	}()
	respReject := postRefill(`{"sheets": 150}`)
	_ = respReject.Body.Close()
	if respReject.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when Pi rejects refill, got %d", respReject.StatusCode)
	}
}

func TestPackage5B_CloudCapabilityRejectionWithoutSilentConversion(t *testing.T) {
	app, ts, _ := setupPackage5BTestServer(t)
	app.cfg.devMode = true
	tmpDir := t.TempDir()
	pdfPath := createTestPDFWithPages(t, tmpDir, 2)
	pdfBytes, _ := os.ReadFile(pdfPath)

	postUpload := func(fields map[string]string) int {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		part, _ := w.CreateFormFile("file", "doc.pdf")
		_, _ = part.Write(pdfBytes)
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/k/kiosk-5b-cap/upload", body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload failed: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := postUpload(map[string]string{"color_mode": "COLOR"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for COLOR under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"duplex": "DUPLEX_LONG_EDGE"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DUPLEX_LONG_EDGE under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"duplex": "DUPLEX_SHORT_EDGE"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DUPLEX_SHORT_EDGE under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"paper_size": "LETTER"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for LETTER paper_size under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"copies": "2", "duplex": "SIMPLEX", "color_mode": "BW", "pages_per_sheet": "2"}); code != http.StatusOK {
		t.Fatalf("expected 200 for BW/SIMPLEX/2-copies/2-up under demo policy, got %d", code)
	}

	// Multi-doc metadata with non-A4 paper_size must be rejected
	postMultiUpload := func(metadata string) int {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		part1, _ := w.CreateFormFile("file", "doc1.pdf")
		_, _ = part1.Write(pdfBytes)
		part2, _ := w.CreateFormFile("file", "doc2.pdf")
		_, _ = part2.Write(pdfBytes)
		_ = w.WriteField("metadata", metadata)
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/k/kiosk-5b-cap/upload", body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("multi-upload failed: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := postMultiUpload(`[{"copies":1,"paper_size":"LETTER"},{"copies":1,"paper_size":"A4"}]`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-A4 paper_size in multi-file metadata, got %d", code)
	}
	if code := postMultiUpload(`[{"copies":1,"duplex":"DUPLEX_LONG_EDGE"},{"copies":1,"duplex":"SIMPLEX"}]`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DUPLEX in multi-file metadata, got %d", code)
	}
	if code := postMultiUpload(`[{"copies":1,"color_mode":"COLOR"},{"copies":1,"color_mode":"BW"}]`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for COLOR in multi-file metadata, got %d", code)
	}

	// Verify /api/pricing advertises duplex_enabled=false and color_enabled=false
	resp, err := http.Get(ts.URL + "/api/pricing")
	if err != nil {
		t.Fatalf("GET /api/pricing failed: %v", err)
	}
	defer resp.Body.Close()
	var pricingResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&pricingResp)
	if pricingResp["color_enabled"] != false || pricingResp["duplex_enabled"] != false {
		t.Fatalf("expected color_enabled=false and duplex_enabled=false, got %v", pricingResp)
	}
}

func TestPackage5B_ColorPricingDoesNotOverrideDemoPolicy(t *testing.T) {
	app, ts, _ := setupPackage5BTestServer(t)
	app.cfg.devMode = true

	// Explicitly enable color in the database pricing configuration
	if err := app.db.SetDetailedPricing(context.Background(), 300, 500, 800, 1000, true); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	// 1. /api/pricing must still advertise color_enabled=false under demo policy
	resp, err := http.Get(ts.URL + "/api/pricing")
	if err != nil {
		t.Fatalf("GET /api/pricing failed: %v", err)
	}
	defer resp.Body.Close()
	var pricingResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&pricingResp)
	if pricingResp["color_enabled"] != false {
		t.Fatalf("expected color_enabled=false even when Pricing.ColorEnabled is true in DB, got %v", pricingResp["color_enabled"])
	}

	// 2. Upload with color_mode=COLOR must still be rejected
	tmpDir := t.TempDir()
	pdfPath := createTestPDFWithPages(t, tmpDir, 1)
	pdfBytes, _ := os.ReadFile(pdfPath)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("file", "doc.pdf")
	_, _ = part.Write(pdfBytes)
	_ = w.WriteField("color_mode", "COLOR")
	_ = w.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/k/kiosk-5b-color/upload", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request failed: %v", err)
	}
	_ = upResp.Body.Close()
	if upResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for COLOR upload under demo policy despite DB color=true, got %d", upResp.StatusCode)
	}
}

func TestPackage5B_AdmissionRejectsUnsupportedCapabilities(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	ctx := context.Background()

	// Connect kiosk with healthy telemetry
	kioskID := "kiosk-5b-admit-cap"
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	now := time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": makeHealthyHealth(100, 150, now),
	})
	time.Sleep(50 * time.Millisecond)

	// Create an order in DB with an unsupported DUPLEX document
	orderID := "ord_test_unsupported_cap"
	order := store.Order{
		OrderID:     orderID,
		Status:      contracts.JobStatusUploaded,
		PricePaise:  300,
		TotalPages:  2,
		TotalSheets: 1,
		Documents: []store.OrderDocument{
			{
				JobID:            "job-doc-0",
				OrderID:          orderID,
				DocumentIndex:    0,
				OriginalFilename: "doc0.pdf",
				Duplex:           contracts.DuplexLongEdge, // Unsupported duplex
				ColorMode:        "BW",
				TotalPages:       2,
				TotalSheets:      1,
				Copies:           1,
				PricePaise:       300,
			},
		},
		Command: contracts.PrintJobCommand{
			JobID:       "job-doc-0",
			OrderID:     orderID,
			KioskID:     kioskID,
			DisplayCode: "C99X",
			TotalPages:  2,
			TotalSheets: 1,
			Copies:      1,
			Duplex:      contracts.DuplexLongEdge,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Attempt to claim order: must be rejected with 400 CAPABILITY_UNSUPPORTED
	claimURL := fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID)
	claimReq, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	claimReq.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for unsupported duplex document admission, got %d", resp.StatusCode)
	}
	var errBody map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	if errBody["code"] != "CAPABILITY_UNSUPPORTED" {
		t.Fatalf("expected code CAPABILITY_UNSUPPORTED, got %s", errBody["code"])
	}
}

func TestPackage5B_RefillConfirmationDetailedBinding(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-refill-binding"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	now := time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": makeHealthyHealth(100, 150, now),
	})
	time.Sleep(50 * time.Millisecond)

	type ackResponse struct {
		ok           bool
		calibrated   bool
		sheets       *int
		inventorySeq uint64
	}
	ackCh := make(chan ackResponse, 10)

	go func() {
		for {
			var msg struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Sheets    *int   `json:"sheets"`
			}
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type == "refill" {
				resp, open := <-ackCh
				if !open {
					return
				}
				ack := map[string]interface{}{
					"type":          "refill_ack",
					"request_id":    msg.RequestID,
					"ok":            resp.ok,
					"calibrated":    resp.calibrated,
					"inventory_seq": resp.inventorySeq,
				}
				if resp.sheets != nil {
					ack["sheets"] = *resp.sheets
				}
				_ = wsjson.Write(ctx, conn, ack)
			}
		}
	}()

	ptr := func(i int) *int { return &i }

	// 1. Zero-sheet confirmation succeeds
	ackCh <- ackResponse{ok: true, calibrated: true, sheets: ptr(0), inventorySeq: 1}
	if err := app.hub.SendRefill(ctx, kioskID, 0); err != nil {
		t.Fatalf("expected zero-sheet refill confirmation to succeed, got %v", err)
	}
	h, ok := app.hub.LatestHealth(kioskID)
	if !ok || h.EstimatedSheets != 0 {
		t.Fatalf("expected cached health sheets=0 after zero-sheet refill, got sheets=%d ok=%v", h.EstimatedSheets, ok)
	}

	// 2. Wrong count returns error and retains pending calibration until resolved
	ackCh <- ackResponse{ok: true, calibrated: true, sheets: ptr(40), inventorySeq: 2}
	if err := app.hub.SendRefill(ctx, kioskID, 80); err == nil {
		t.Fatal("expected error when returned sheet count (40) differs from requested count (80)")
	}
	now = now.Add(time.Second)
	hResolve2 := makeHealthyHealth(40, 150, now)
	hResolve2.InventorySeq = 2
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hResolve2})
	time.Sleep(40 * time.Millisecond)

	// 3. Missing calibration flag returns error and retains pending calibration until resolved
	ackCh <- ackResponse{ok: true, calibrated: false, sheets: ptr(80), inventorySeq: 3}
	if err := app.hub.SendRefill(ctx, kioskID, 80); err == nil {
		t.Fatal("expected error when calibrated flag is false")
	}
	now = now.Add(time.Second)
	hResolve3 := makeHealthyHealth(40, 150, now)
	hResolve3.InventorySeq = 3
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hResolve3})
	time.Sleep(40 * time.Millisecond)

	// 4. Timeout returns error and retains pending calibration until resolved
	timeoutCtx, cancelTimeout := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelTimeout()
	if err := app.hub.SendRefill(timeoutCtx, kioskID, 50); err == nil {
		t.Fatal("expected timeout error when no ack is returned")
	}
	// Drain the un-acked refill message sent during timeout test by rejecting it
	ackCh <- ackResponse{ok: false, calibrated: false, sheets: nil}
	time.Sleep(40 * time.Millisecond)

	// 5. Downward calibration immediately updates cached kiosk inventory and blocks reservations
	now = now.Add(time.Second)
	hPreDown := makeHealthyHealth(100, 150, now)
	hPreDown.InventorySeq = 4
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": hPreDown,
	})
	time.Sleep(50 * time.Millisecond)

	ackCh <- ackResponse{ok: true, calibrated: true, sheets: ptr(2), inventorySeq: 5}
	if err := app.hub.SendRefill(ctx, kioskID, 2); err != nil {
		t.Fatalf("downward refill to 2 sheets failed: %v", err)
	}

	// Immediately, without waiting for another telemetry frame, attempt to claim an order requiring 5 sheets
	downwardOrderID := "ord_downward_paper_check"
	downwardOrder := store.Order{
		OrderID:     downwardOrderID,
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  300,
		TotalPages:  5,
		TotalSheets: 5,
		Documents: []store.OrderDocument{
			{
				JobID:            "job-downward",
				OrderID:          downwardOrderID,
				DocumentIndex:    0,
				OriginalFilename: "doc.pdf",
				Duplex:           contracts.DuplexSimplex,
				ColorMode:        "BW",
				TotalPages:       5,
				TotalSheets:      5,
				Copies:           1,
				PricePaise:       300,
			},
		},
		Command: contracts.PrintJobCommand{
			JobID:       "job-downward",
			OrderID:     downwardOrderID,
			KioskID:     kioskID,
			DisplayCode: "D55X",
			TotalPages:  5,
			TotalSheets: 5,
			Copies:      1,
			Duplex:      contracts.DuplexSimplex,
			ColorMode:   "BW",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, downwardOrder); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	claimURL := fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, downwardOrderID)
	claimReq, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	claimReq.Header.Set("Authorization", "Bearer "+secret)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("claim request failed: %v", err)
	}
	defer claimResp.Body.Close()
	var conflictBody map[string]string
	_ = json.NewDecoder(claimResp.Body).Decode(&conflictBody)
	if claimResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict due to insufficient paper after downward calibration, got %d (body: %v)", claimResp.StatusCode, conflictBody)
	}
	if conflictBody["code"] != "INSUFFICIENT_PAPER" {
		t.Fatalf("expected code INSUFFICIENT_PAPER, got %s", conflictBody["code"])
	}
}

func TestPackage5B_ConservativeCapacityAdmissionValidation(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-capacity-eval"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	now := time.Now().UTC()

	// 1. Out-of-bounds telemetry (EstimatedSheets > ConfiguredTrayCapacity) is rejected
	h1 := makeHealthyHealth(200, 150, now) // Out of bounds: 200 > 150
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h1,
	})
	time.Sleep(50 * time.Millisecond)

	stock, condErr := app.checkPrinterCondition(kioskID)
	if condErr == nil || condErr.Code != "MALFORMED_TELEMETRY" {
		t.Fatalf("expected MALFORMED_TELEMETRY for estimated sheets 200 > 150, got stock=%d err=%+v", stock, condErr)
	}

	// 2. Non-positive tray capacity is rejected
	now = now.Add(time.Second)
	h2 := makeHealthyHealth(50, 0, now) // Invalid capacity 0
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h2,
	})
	time.Sleep(50 * time.Millisecond)

	stock, condErr = app.checkPrinterCondition(kioskID)
	if condErr == nil || condErr.Code != "MALFORMED_TELEMETRY" {
		t.Fatalf("expected MALFORMED_TELEMETRY for ConfiguredTrayCapacity=0, got stock=%d err=%+v", stock, condErr)
	}

	// 3. Contradictory physical capacity caps effective stock conservatively
	now = now.Add(time.Second)
	h3 := makeHealthyHealth(100, 150, now)
	h3.InputTrayCapacity = 30 // Verified physical SNMP capacity is only 30
	h3.InputTrayCapacityKnown = true
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h3,
	})
	time.Sleep(50 * time.Millisecond)

	stock, condErr = app.checkPrinterCondition(kioskID)
	if condErr != nil {
		t.Fatalf("expected checkPrinterCondition to succeed, got %v", condErr)
	}
	if stock != 30 {
		t.Fatalf("expected effective stock to be conservatively capped to physical capacity 30, got %d", stock)
	}
}

func TestPackage5B_RefillPreservesFaultAndHandlesConcurrentAndStaleAndTimeout(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-refill-consistency"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// 1. Preservation of physical fault state (EMPTY must NOT become OK on refill_ack)
	t0 := time.Now().UTC()
	faultHealth := makeHealthyHealth(0, 150, t0)
	faultHealth.Online = false
	faultHealth.State = "ERROR"
	faultHealth.EvaluatedHealth = contracts.EvaluatedHealthFault
	faultHealth.PaperTrayStatus = "EMPTY"
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": faultHealth,
	})
	time.Sleep(50 * time.Millisecond)

	type refillReqMsg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Sheets    *int   `json:"sheets"`
	}
	refillCh := make(chan refillReqMsg, 10)
	go func() {
		for {
			var msg refillReqMsg
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type == "refill" {
				refillCh <- msg
			}
		}
	}()

	// Helper to send ack for next refill with authoritative Pi inventory_seq
	sendAck := func(req refillReqMsg, sheets int, invSeq uint64) {
		_ = wsjson.Write(ctx, conn, map[string]interface{}{
			"type":          "refill_ack",
			"request_id":    req.RequestID,
			"ok":            true,
			"calibrated":    true,
			"sheets":        sheets,
			"inventory_seq": invSeq,
		})
	}

	go func() {
		req := <-refillCh
		sendAck(req, *req.Sheets, 1)
	}()
	if err := app.hub.SendRefill(ctx, kioskID, 50); err != nil {
		t.Fatalf("SendRefill(50) failed: %v", err)
	}
	hAfterFaultRefill, ok := app.hub.LatestHealth(kioskID)
	if !ok {
		t.Fatal("expected LatestHealth to be present")
	}
	if hAfterFaultRefill.EstimatedSheets != 50 || !hAfterFaultRefill.InventoryCalibrated {
		t.Fatalf("expected EstimatedSheets=50, calibrated=true; got %d, %v", hAfterFaultRefill.EstimatedSheets, hAfterFaultRefill.InventoryCalibrated)
	}
	if hAfterFaultRefill.PaperTrayStatus != "EMPTY" || hAfterFaultRefill.EvaluatedHealth != contracts.EvaluatedHealthFault {
		t.Fatalf("expected physical fault state EMPTY/FAULT preserved after refill, got %s/%s", hAfterFaultRefill.PaperTrayStatus, hAfterFaultRefill.EvaluatedHealth)
	}
	if !hAfterFaultRefill.UpdatedAt.Equal(t0) {
		t.Fatalf("expected UpdatedAt preserved (%v), got %v", t0, hAfterFaultRefill.UpdatedAt)
	}

	// 2. Old telemetry acquisition overlapping downward calibration cannot overwrite confirmed count
	t1 := time.Now().UTC()
	h1 := makeHealthyHealth(100, 150, t1)
	h1.InventorySeq = 1
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h1,
	})
	time.Sleep(50 * time.Millisecond)

	go func() {
		req := <-refillCh
		sendAck(req, *req.Sheets, 2)
	}()
	if err := app.hub.SendRefill(ctx, kioskID, 10); err != nil {
		t.Fatalf("SendRefill(10) failed: %v", err)
	}

	// Now an older telemetry frame (acquired before refill, InventorySeq=1 < 2) arrives
	staleFrame := makeHealthyHealth(100, 150, t1.Add(time.Millisecond))
	staleFrame.InventorySeq = 1
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": staleFrame,
	})
	time.Sleep(50 * time.Millisecond)

	hAfterStale, _ := app.hub.LatestHealth(kioskID)
	if hAfterStale.EstimatedSheets != 10 {
		t.Fatalf("expected stale pre-calibration frame not to overwrite confirmed 10 sheets, got %d", hAfterStale.EstimatedSheets)
	}

	// 3. Only one unresolved calibration is permitted per active kiosk connection
	errCh1 := make(chan error, 1)
	go func() { errCh1 <- app.hub.SendRefill(ctx, kioskID, 40) }()
	reqFirst := <-refillCh
	// While first refill is still unresolved, a concurrent second refill MUST be rejected without transmitting
	if err2 := app.hub.SendRefill(ctx, kioskID, 15); err2 == nil {
		t.Fatal("expected concurrent second SendRefill to be rejected while first is unresolved")
	}
	select {
	case unexpected := <-refillCh:
		t.Fatalf("expected no second refill transmission while first is unresolved, got %+v", unexpected)
	default:
	}
	sendAck(reqFirst, *reqFirst.Sheets, 3)
	if e1 := <-errCh1; e1 != nil {
		t.Fatalf("first SendRefill failed: %v", e1)
	}
	go func() {
		req := <-refillCh
		sendAck(req, *req.Sheets, 4)
	}()
	if err2 := app.hub.SendRefill(ctx, kioskID, 15); err2 != nil {
		t.Fatalf("subsequent SendRefill after resolution failed: %v", err2)
	}
	hConcurrent, _ := app.hub.LatestHealth(kioskID)
	if hConcurrent.EstimatedSheets != 15 {
		t.Fatalf("expected final cached sheets 15, got %d", hConcurrent.EstimatedSheets)
	}

	// 4. Timeout after possible durable application blocks reservations using obsolete larger stock
	t2 := time.Now().UTC()
	h2 := makeHealthyHealth(100, 150, t2)
	h2.InventorySeq = 4
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h2,
	})
	time.Sleep(50 * time.Millisecond)

	shortCtx, shortCancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer shortCancel()
	if err := app.hub.SendRefill(shortCtx, kioskID, 5); err == nil {
		t.Fatal("expected timeout error from SendRefill")
	}
	<-refillCh // drain un-acked refill message

	// While calibration outcome is unconfirmed, checkPrinterCondition must NOT authorize reservations using obsolete 100 sheets
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr == nil {
		t.Fatalf("expected reservation admission to block while calibration outcome is unconfirmed, got stock=%d", stock)
	}

	// Authoritative Pi telemetry with advanced InventorySeq (5 > 4) clears uncertainty
	t3 := time.Now().UTC().Add(100 * time.Millisecond)
	h3 := makeHealthyHealth(5, 150, t3)
	h3.InventorySeq = 5
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h3,
	})
	time.Sleep(50 * time.Millisecond)

	stock, condErr := app.checkPrinterCondition(kioskID)
	if condErr != nil || stock != 5 {
		t.Fatalf("expected authoritative advanced-sequence telemetry to restore admission with 5 sheets, got stock=%d err=%+v", stock, condErr)
	}
}

func TestPackage5B_RefillRejectsUnavailableConfiguredCapacity(t *testing.T) {
	_, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-no-cap"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// No health telemetry received yet -> configured capacity unavailable -> must reject without 150 fallback
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/refill", ts.URL, kioskID), strings.NewReader(`{"sheets": 100}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST refill failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when configured tray capacity is unavailable, got %d", resp.StatusCode)
	}

	// Send telemetry with ConfiguredTrayCapacity=150 and smaller physical InputTrayCapacity=40
	now := time.Now().UTC()
	h := makeHealthyHealth(10, 150, now)
	h.InputTrayCapacity = 40
	h.InputTrayCapacityKnown = true
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":   "health",
		"health": h,
	})
	time.Sleep(50 * time.Millisecond)

	reqOver, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/refill", ts.URL, kioskID), strings.NewReader(`{"sheets": 50}`))
	reqOver.Header.Set("Authorization", "Bearer "+secret)
	reqOver.Header.Set("Content-Type", "application/json")
	respOver, err := http.DefaultClient.Do(reqOver)
	if err != nil {
		t.Fatalf("POST refill failed: %v", err)
	}
	respOver.Body.Close()
	if respOver.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when refill (50) exceeds smaller physical capacity (40), got %d", respOver.StatusCode)
	}
}

func TestPackage5B_DemoPayAndDispatchPolicyValidation(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-demopay-policy"
	ctx := context.Background()
	now := time.Now().UTC()

	// 1. Missing order data fails closed in policy validation and handleDemoPaymentSubmit
	if polErr := validateOrderAgainstPolicy(nil, app.policy()); polErr == nil {
		t.Fatal("expected validateOrderAgainstPolicy(nil) to fail closed")
	}

	// 2. Unsupported print mode (DUPLEX_LONG_EDGE) fails closed before payment capture
	badOrdID := "ord-bad-duplex"
	badOrd := store.Order{
		OrderID:     badOrdID,
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  300,
		TotalPages:  2,
		TotalSheets: 1,
		ColorMode:   "BW",
		Documents: []store.OrderDocument{
			{
				JobID:            "job-bad-duplex",
				OrderID:          badOrdID,
				DocumentIndex:    0,
				OriginalFilename: "doc.pdf",
				DownloadURL:      "http://example.com/doc.pdf",
				SHA256Checksum:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Duplex:           contracts.DuplexLongEdge,
				ColorMode:        "BW",
				TotalPages:       2,
				TotalSheets:      1,
				Copies:           1,
				PricePaise:       300,
			},
		},
		Command: contracts.PrintJobCommand{
			JobID:          "job-bad-duplex",
			OrderID:        badOrdID,
			KioskID:        kioskID,
			DisplayCode:    "9991",
			DownloadURL:    "http://example.com/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			ColorMode:      "BW",
			StartFromPage:  1,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, badOrd); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := app.db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, badOrdID, 100, 10*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}
	badAtt := store.PaymentAttempt{
		AttemptID:      "att-bad-duplex",
		DemoToken:      "tok-bad-duplex",
		OrderID:        badOrdID,
		SessionID:      sess.SessionID,
		KioskID:        kioskID,
		Provider:       "mock",
		AmountPaise:    300,
		Currency:       "INR",
		Status:         "ACTIVE",
		IdempotencyKey: "idem-bad-duplex",
		ExpiresAt:      now.Add(5 * time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := app.db.CreatePaymentAttempt(ctx, badAtt); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}
	reqBad, _ := http.NewRequest(http.MethodPost, ts.URL+"/pay/tok-bad-duplex", nil)
	reqBad.Header.Set("Accept", "application/json")
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatalf("POST /pay failed: %v", err)
	}
	respBad.Body.Close()
	if respBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for unsupported duplex order on demo pay, got %d", respBad.StatusCode)
	}

	// 3. Reconnect reconciliation cannot bypass configured product policy
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// SendJob directly with DUPLEX_LONG_EDGE must be rejected by Hub policy
	if err := app.hub.SendJob(ctx, badOrd.Command); err == nil {
		t.Fatal("expected Hub.SendJob to reject command violating configured product policy")
	}
}

func TestPackage5B_AuthoritativeInventorySequenceAndUncertaintyRecovery(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-seq-ownership"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	type refillReqMsg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Sheets    *int   `json:"sheets"`
	}
	refillCh := make(chan refillReqMsg, 10)
	readLoop := func(c *websocket.Conn, ch chan<- refillReqMsg) {
		for {
			var msg refillReqMsg
			if err := wsjson.Read(ctx, c, &msg); err != nil {
				return
			}
			if msg.Type == "refill" {
				ch <- msg
			}
		}
	}
	go readLoop(conn, refillCh)

	now := time.Now().UTC()

	// 1. Sequence-zero initial telemetry & routine paper consumption at sequence 0
	h0 := makeHealthyHealth(80, 150, now)
	h0.InventorySeq = 0
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": h0})
	time.Sleep(40 * time.Millisecond)

	now = now.Add(50 * time.Millisecond)
	h0Consume := makeHealthyHealth(79, 150, now)
	h0Consume.InventorySeq = 0
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": h0Consume})
	time.Sleep(40 * time.Millisecond)
	if got, ok := app.hub.LatestHealth(kioskID); !ok || got.EstimatedSheets != 79 {
		t.Fatalf("expected sequence-0 initial and consumption update to 79 sheets, got %d (ok=%v)", got.EstimatedSheets, ok)
	}

	// 2. New-sequence telemetry received BEFORE acknowledgement processing does NOT cause double-increment
	// and same-sequence consumption update (49) must NOT be overwritten by delayed refill_ack (50).
	ackSeq1Done := make(chan struct{})
	go func() {
		defer close(ackSeq1Done)
		req := <-refillCh
		// Pi publishes health(seq=1, sheets=50) AND routine consumption health(seq=1, sheets=49)
		// BEFORE refill_ack(seq=1, sheets=50) is processed!
		hNewSeq := makeHealthyHealth(50, 150, time.Now().UTC())
		hNewSeq.InventorySeq = 1
		_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hNewSeq})
		hConsumed := makeHealthyHealth(49, 150, time.Now().UTC().Add(10*time.Millisecond))
		hConsumed.InventorySeq = 1
		_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hConsumed})
		_ = wsjson.Write(ctx, conn, map[string]interface{}{
			"type":          "refill_ack",
			"request_id":    req.RequestID,
			"ok":            true,
			"calibrated":    true,
			"sheets":        50,
			"inventory_seq": 1,
		})
	}()
	if err := app.hub.SendRefill(ctx, kioskID, 50); err != nil {
		t.Fatalf("SendRefill(50) failed: %v", err)
	}
	<-ackSeq1Done
	time.Sleep(40 * time.Millisecond)
	if got, ok := app.hub.LatestHealth(kioskID); !ok || got.EstimatedSheets != 49 || got.InventorySeq != 1 {
		t.Fatalf("expected same-sequence consumption (49) not overwritten by delayed ack (50), got sheets=%d seq=%d ok=%v", got.EstimatedSheets, got.InventorySeq, ok)
	}

	// Subsequent routine paper consumption within seq=1 (48 sheets) must also succeed!
	hSeq1Consume := makeHealthyHealth(48, 150, time.Now().UTC().Add(20*time.Millisecond))
	hSeq1Consume.InventorySeq = 1
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hSeq1Consume})
	time.Sleep(40 * time.Millisecond)
	if got, ok := app.hub.LatestHealth(kioskID); !ok || got.EstimatedSheets != 48 || got.InventorySeq != 1 {
		t.Fatalf("expected routine consumption within seq=1 to yield 48 sheets (seq=1), got sheets=%d seq=%d ok=%v", got.EstimatedSheets, got.InventorySeq, ok)
	}

	// 3. Older acknowledgement cannot overwrite newer inventory (seq=2 arrives before delayed ack for seq=1)
	go func() {
		req := <-refillCh
		// Newer calibration seq=2 (sheets=12) arrives via telemetry before delayed ack for seq=1
		hSeq2 := makeHealthyHealth(12, 150, time.Now().UTC().Add(40*time.Millisecond))
		hSeq2.InventorySeq = 2
		_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hSeq2})
		time.Sleep(30 * time.Millisecond)
		// Delayed ack carrying older seq=1 and requested 45 sheets
		_ = wsjson.Write(ctx, conn, map[string]interface{}{
			"type":          "refill_ack",
			"request_id":    req.RequestID,
			"ok":            true,
			"calibrated":    true,
			"sheets":        45,
			"inventory_seq": 1,
		})
	}()
	if err := app.hub.SendRefill(ctx, kioskID, 45); err == nil {
		// Note: SendRefill receives telemetry error (reported 12 != 45) because seq=2 resolved pendingRefill
	}
	if got, ok := app.hub.LatestHealth(kioskID); !ok || got.EstimatedSheets != 12 || got.InventorySeq != 2 {
		t.Fatalf("expected older ack (seq=1) not to overwrite newer inventory (sheets=12, seq=2), got sheets=%d seq=%d", got.EstimatedSheets, got.InventorySeq)
	}

	// 4. Fresh hardware telemetry arriving before an outstanding refill is applied (same seq=2, newer UpdatedAt)
	// must NOT clear pendingRefill uncertainty!
	inFlightDone := make(chan error, 1)
	go func() {
		inFlightDone <- app.hub.SendRefill(ctx, kioskID, 30)
	}()
	reqInFlight := <-refillCh
	hSameSeq := makeHealthyHealth(12, 150, time.Now().UTC().Add(80*time.Millisecond))
	hSameSeq.InventorySeq = 2
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": hSameSeq})
	time.Sleep(40 * time.Millisecond)
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr == nil {
		t.Fatalf("expected reservation admission blocked while refill is outstanding despite fresh same-seq telemetry, got stock=%d", stock)
	}
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":          "refill_ack",
		"request_id":    reqInFlight.RequestID,
		"ok":            true,
		"calibrated":    true,
		"sheets":        30,
		"inventory_seq": 3,
	})
	if err := <-inFlightDone; err != nil {
		t.Fatalf("in-flight SendRefill(30) failed: %v", err)
	}

	// 5. Unresolved timed-out calibration prevents transmission of another calibration,
	// and late matching outcome correctly resolves uncertainty.
	shortCtx, shortCancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer shortCancel()
	if err := app.hub.SendRefill(shortCtx, kioskID, 5); err == nil {
		t.Fatal("expected timeout on SendRefill(5)")
	}
	lateReq := <-refillCh
	// Another calibration MUST NOT be transmitted while the timed-out one is unresolved
	if err := app.hub.SendRefill(ctx, kioskID, 20); err == nil {
		t.Fatal("expected second SendRefill rejected while earlier timed-out refill is unresolved")
	}
	select {
	case unexpected := <-refillCh:
		t.Fatalf("expected no refill transmitted while prior refill unresolved, got %+v", unexpected)
	default:
	}
	// Late matching outcome arrives and resolves uncertainty
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":          "refill_ack",
		"request_id":    lateReq.RequestID,
		"ok":            true,
		"calibrated":    true,
		"sheets":        5,
		"inventory_seq": 4,
	})
	time.Sleep(40 * time.Millisecond)
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr != nil || stock != 5 {
		t.Fatalf("expected late matching outcome to resolve uncertainty with 5 sheets, got stock=%d err=%+v", stock, condErr)
	}

	// 6. Reconnect with authoritative current inventory restores clean readiness
	_ = conn.Close(websocket.StatusNormalClosure, "reconnect test")
	time.Sleep(40 * time.Millisecond)

	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("reconnect ws dial failed: %v", err)
	}
	defer conn2.CloseNow()
	refillCh2 := make(chan refillReqMsg, 10)
	go readLoop(conn2, refillCh2)

	// New connection starts unverified
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr == nil {
		t.Fatalf("expected new connection to start unverified, got stock=%d", stock)
	}

	// Pi sends authoritative current inventory on reconnect
	hReconnect := makeHealthyHealth(18, 150, time.Now().UTC().Add(120*time.Millisecond))
	hReconnect.InventorySeq = 4
	_ = wsjson.Write(ctx, conn2, map[string]interface{}{"type": "health", "health": hReconnect})
	time.Sleep(40 * time.Millisecond)
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr != nil || stock != 18 {
		t.Fatalf("expected reconnect with authoritative inventory (18, seq=4) to succeed, got stock=%d err=%+v", stock, condErr)
	}

	// 7. Concurrent reservation and downward calibration: reservation cannot commit using superseded 18-sheet stock
	refillDone := make(chan error, 1)
	go func() {
		refillDone <- app.hub.SendRefill(ctx, kioskID, 2)
	}()
	reqDown := <-refillCh2
	// While downward calibration is in-flight, reservation admission must block
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr == nil {
		t.Fatalf("expected concurrent reservation blocked during downward calibration, got stock=%d", stock)
	}
	_ = wsjson.Write(ctx, conn2, map[string]interface{}{
		"type":          "refill_ack",
		"request_id":    reqDown.RequestID,
		"ok":            true,
		"calibrated":    true,
		"sheets":        2,
		"inventory_seq": 5,
	})
	if err := <-refillDone; err != nil {
		t.Fatalf("downward SendRefill(2) failed: %v", err)
	}
	if stock, condErr := app.checkPrinterCondition(kioskID); condErr != nil || stock != 2 {
		t.Fatalf("expected post-calibration stock=2, got stock=%d err=%+v", stock, condErr)
	}
}

func TestPackage5B_ReservationTransactionAndCalibrationAtomicOrdering(t *testing.T) {
	app, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-atomic-order"
	ctx := context.Background()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	type refillReqMsg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Sheets    *int   `json:"sheets"`
	}
	refillCh := make(chan refillReqMsg, 10)
	go func() {
		for {
			var msg refillReqMsg
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type == "refill" {
				refillCh <- msg
			}
		}
	}()

	now := time.Now().UTC()
	h0 := makeHealthyHealth(100, 150, now)
	h0.InventorySeq = 1
	_ = wsjson.Write(ctx, conn, map[string]interface{}{"type": "health", "health": h0})
	time.Sleep(40 * time.Millisecond)

	// Create a 50-sheet order that would succeed at 100 sheets, but MUST fail at 2 sheets.
	ordID := "ord-atomic-overlap"
	ord := store.Order{
		OrderID:     ordID,
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  500,
		TotalPages:  50,
		TotalSheets: 50,
		ColorMode:   "BW",
		Documents: []store.OrderDocument{
			{
				JobID:            "job-atomic",
				OrderID:          ordID,
				DocumentIndex:    0,
				OriginalFilename: "doc.pdf",
				DownloadURL:      "http://example.com/doc.pdf",
				SHA256Checksum:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Duplex:           contracts.DuplexSimplex,
				ColorMode:        "BW",
				TotalPages:       50,
				TotalSheets:      50,
				Copies:           1,
				PricePaise:       500,
			},
		},
		Command: contracts.PrintJobCommand{
			JobID:          "job-atomic",
			OrderID:        ordID,
			KioskID:        kioskID,
			DisplayCode:    "8881",
			DownloadURL:    "http://example.com/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     50,
			TotalSheets:    50,
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

	// Synchronize between inventory read (100) and DB commit in handleOrderClaim
	readDone := make(chan struct{})
	proceedCommit := make(chan struct{})
	app.beforeReserveCommitFn = func() {
		close(readDone)
		<-proceedCommit
	}

	claimStatusCh := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, ordID), nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			claimStatusCh <- -1
			return
		}
		resp.Body.Close()
		claimStatusCh <- resp.StatusCode
	}()

	<-readDone
	// Reservation read 100 sheets and is paused before DB commit. Start downward calibration to 2 sheets.
	refillErrCh := make(chan error, 1)
	go func() {
		refillErrCh <- app.hub.SendRefill(ctx, kioskID, 2)
	}()

	// Calibration MUST NOT transmit refill while reservation boundary is active!
	select {
	case unexpected := <-refillCh:
		t.Fatalf("calibration started/transmitted before in-flight reservation committed: %+v", unexpected)
	case <-time.After(30 * time.Millisecond):
	}

	// Allow reservation to commit before calibration begins
	close(proceedCommit)
	app.beforeReserveCommitFn = nil
	if code := <-claimStatusCh; code != http.StatusOK {
		t.Fatalf("expected claim that started before calibration to commit with 200 OK, got %d", code)
	}

	// Now calibration begins and completes to 2 sheets
	reqRefill := <-refillCh
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":          "refill_ack",
		"request_id":    reqRefill.RequestID,
		"ok":            true,
		"calibrated":    true,
		"sheets":        2,
		"inventory_seq": 2,
	})
	if err := <-refillErrCh; err != nil {
		t.Fatalf("SendRefill(2) failed: %v", err)
	}

	// Subsequent order requiring 50 sheets MUST be rejected using the calibrated 2-sheet limit (409 INSUFFICIENT_PAPER)
	ord2ID := "ord-atomic-after"
	ord2 := ord
	ord2.OrderID = ord2ID
	ord2.Command.OrderID = ord2ID
	ord2.Command.JobID = "job-atomic-after"
	ord2.Command.DisplayCode = "8882"
	ord2.Documents = []store.OrderDocument{ord.Documents[0]}
	ord2.Documents[0].OrderID = ord2ID
	ord2.Documents[0].JobID = "job-atomic-after"
	if err := app.db.CreateOrder(ctx, ord2); err != nil {
		t.Fatalf("CreateOrder(ord2) failed: %v", err)
	}
	req2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, ord2ID), nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("claim ord2 failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for 50-sheet order after calibration to 2 sheets, got %d", resp2.StatusCode)
	}
}
