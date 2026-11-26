package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

// setupDemoTestServer initializes an isolated in-memory/temp-file cloud server with mock hub.
func setupDemoTestServer(t *testing.T) (*appServer, *httptest.Server, *store.Store, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "xeroxgo_demo_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	dbPath := filepath.Join(tempDir, "cloud_test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("failed to open store: %v", err)
	}

	hub := ws.NewHub(st)
	pinLimiter := NewPINRateLimiter(st, 10, 1*time.Minute, 1*time.Minute)

	cfg := &serverConfig{
		addr:           ":0",
		dbPath:         dbPath,
		jobsDir:        filepath.Join(tempDir, "jobs"),
		devMode:        true,
		downloadSecret: []byte("dev_secret_download_1234567890"),
		kioskSecret:    []byte("dev_secret_kiosk_1234567890"),
	}

	app := &appServer{
		cfg:        cfg,
		db:         st,
		hub:        hub,
		convWake:   make(chan struct{}, 1),
		pinLimiter: pinLimiter,
	}

	ts := httptest.NewServer(app.routes())

	cleanup := func() {
		ts.Close()
		_ = st.Close()
		_ = os.RemoveAll(tempDir)
	}

	return app, ts, st, cleanup
}

func createTestOrderAndSession(t *testing.T, st *store.Store, kioskID, orderID, pin string, sheets int, pricePaise int64) *store.KioskSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DisplayCode:    pin,
		DownloadURL:    "http://test.local/download",
		SHA256Checksum: "checksum123",
		TotalPages:     sheets,
		TotalSheets:    sheets,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	order := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   pricePaise,
		PINExpiresAt: now.Add(15 * time.Minute),
	}

	if err := st.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSession failed: %v", err)
	}

	return sess
}

func TestDemoCheckoutAlwaysUsesXeroxGoPage(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-demo-gate"
	orderID := "ord_gate_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "1111", 2, 1000)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initiate payment request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", resp.StatusCode)
	}

	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)

	qrPayload, _ := initResp["qr_payload"].(string)
	demoToken, _ := initResp["demo_token"].(string)

	if !strings.HasPrefix(demoToken, "dmo_") {
		t.Errorf("expected demo_token starting with 'dmo_', got: %s", demoToken)
	}
	if !strings.Contains(qrPayload, "/pay/"+demoToken) {
		t.Errorf("expected qr_payload to contain /pay/%s, got: %s", demoToken, qrPayload)
	}
}

func TestDemoCheckoutPage_Rendering(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-render-test"
	orderID := "ord_render_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "3333", 4, 1850)

	// Initiate payment to generate token
	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initiate payment failed: %v", err)
	}
	defer resp.Body.Close()

	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	demoToken := initResp["demo_token"].(string)

	// Request GET /pay/{token}
	checkoutURL := fmt.Sprintf("%s/pay/%s", ts.URL, demoToken)
	cResp, err := http.Get(checkoutURL)
	if err != nil {
		t.Fatalf("GET /pay/%s failed: %v", demoToken, err)
	}
	defer cResp.Body.Close()

	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", cResp.StatusCode)
	}

	bodyBuf := make([]byte, 32*1024)
	n, _ := cResp.Body.Read(bodyBuf)
	bodyStr := string(bodyBuf[:n])

	// Check for authoritative amount (1850 paise = ₹18.50)
	if !strings.Contains(bodyStr, "18.50") {
		t.Errorf("checkout HTML missing expected price ₹18.50")
	}
	// Check for Order ID
	if !strings.Contains(bodyStr, orderID) {
		t.Errorf("checkout HTML missing orderID %s", orderID)
	}
	// Check for Kiosk ID
	if !strings.Contains(bodyStr, kioskID) {
		t.Errorf("checkout HTML missing kioskID %s", kioskID)
	}
	// Check for Pay button
	if !strings.Contains(bodyStr, "Pay ₹18.50 (Demo Mode)") {
		t.Errorf("checkout HTML missing Pay button text")
	}
	// Check for Demo notice
	if !strings.Contains(bodyStr, "Demo Mode Notice") {
		t.Errorf("checkout HTML missing Demo Mode notice")
	}
}

func TestDemoCheckoutPage_JSONEndpoint(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-json-test"
	orderID := "ord_json_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "5555", 3, 600)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	_ = resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	jsonReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/pay/%s", ts.URL, demoToken), nil)
	jsonReq.Header.Set("Accept", "application/json")
	jResp, err := http.DefaultClient.Do(jsonReq)
	if err != nil {
		t.Fatalf("GET /pay/ with json accept failed: %v", err)
	}
	defer jResp.Body.Close()

	if jResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", jResp.StatusCode)
	}
	var data map[string]interface{}
	if err := json.NewDecoder(jResp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if data["status"] != "ACTIVE" {
		t.Errorf("status = %v, want ACTIVE", data["status"])
	}
	if data["order_id"] != orderID {
		t.Errorf("order_id = %v, want %s", data["order_id"], orderID)
	}
	if data["amount_rupees"] != "6.00" {
		t.Errorf("amount_rupees = %v, want 6.00", data["amount_rupees"])
	}
}

func TestDemoCheckoutPage_ExpiredAndAlreadyPaid(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-status-test"
	orderID := "ord_status_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "4444", 2, 900)

	// Initiate payment
	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	_ = resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	// Case A: Manually expire the attempt
	pastTime := time.Now().UTC().Add(-10 * time.Minute)
	_, _ = st.DB().ExecContext(context.Background(), "UPDATE payment_attempts SET expires_at_ns = ? WHERE demo_token = ?;", pastTime.UnixNano(), demoToken)

	expResp, err := http.Get(fmt.Sprintf("%s/pay/%s", ts.URL, demoToken))
	if err != nil {
		t.Fatalf("GET /pay/ failed: %v", err)
	}
	defer expResp.Body.Close()
	if expResp.StatusCode != http.StatusGone {
		t.Errorf("expected HTTP 410 Gone for expired session, got %d", expResp.StatusCode)
	}

	// Case B: Mark order and attempt PAID
	orderID2 := "ord_status_002"
	sess2 := createTestOrderAndSession(t, st, kioskID, orderID2, "5555", 2, 900)
	initURL2 := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess2.SessionID)
	req2, _ := http.NewRequest(http.MethodPost, initURL2, nil)
	req2.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp2, _ := http.DefaultClient.Do(req2)
	var initResp2 map[string]interface{}
	_ = json.NewDecoder(resp2.Body).Decode(&initResp2)
	_ = resp2.Body.Close()
	demoToken2 := initResp2["demo_token"].(string)

	// Simulate payment capture
	_, _ = st.CapturePaymentAndCheckDispatch(context.Background(), store.PaymentRecord{
		PaymentID:   "pay_test_002",
		OrderID:     orderID2,
		KioskID:     kioskID,
		SessionID:   sess2.SessionID,
		AmountPaise: 900,
		Provider:    "mock",
		Status:      "captured",
	}, time.Now().UTC())
	_ = st.MarkPaymentAttemptPaid(context.Background(), initResp2["attempt_id"].(string), "pay_test_002")

	paidResp, err := http.Get(fmt.Sprintf("%s/pay/%s", ts.URL, demoToken2))
	if err != nil {
		t.Fatalf("GET /pay/ failed: %v", err)
	}
	defer paidResp.Body.Close()

	if paidResp.StatusCode != http.StatusOK {
		t.Errorf("expected HTTP 200 for already paid session, got %d", paidResp.StatusCode)
	}
	buf := make([]byte, 16*1024)
	n, _ := paidResp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "Payment Already Received") {
		t.Errorf("expected 'Payment Already Received' in HTML response")
	}
}

func TestDemoPaymentSubmit_DispatchedAndIdempotency(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-dispatch-test"
	orderID := "ord_disp_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "6666", 3, 1500)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	_ = resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	// 1. Submit POST /pay/{token} via JSON
	payURL := fmt.Sprintf("%s/pay/%s", ts.URL, demoToken)
	payReq, _ := http.NewRequest(http.MethodPost, payURL, nil)
	payReq.Header.Set("Accept", "application/json")

	payResp, err := http.DefaultClient.Do(payReq)
	if err != nil {
		t.Fatalf("POST /pay/ failed: %v", err)
	}
	defer payResp.Body.Close()

	if payResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", payResp.StatusCode)
	}

	var submitData map[string]interface{}
	_ = json.NewDecoder(payResp.Body).Decode(&submitData)

	if submitData["status"] != "dispatched" {
		t.Errorf("expected status 'dispatched', got: %v", submitData["status"])
	}

	// Verify database state: order should be DISPATCHED, attempt PAID
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil || ord == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusDispatched {
		t.Errorf("expected order status DISPATCHED, got: %s", ord.Status)
	}

	att, err := st.GetPaymentAttemptByToken(context.Background(), demoToken)
	if err != nil || att == nil {
		t.Fatalf("GetPaymentAttemptByToken failed: %v", err)
	}
	if att.Status != "PAID" {
		t.Errorf("expected payment attempt status PAID, got: %s", att.Status)
	}

	// 2. Test Idempotency with Duplicate Clicks
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dupReq, _ := http.NewRequest(http.MethodPost, payURL, nil)
			dupReq.Header.Set("Accept", "application/json")
			dupResp, err := http.DefaultClient.Do(dupReq)
			if err != nil {
				return
			}
			defer dupResp.Body.Close()
			if dupResp.StatusCode != http.StatusOK {
				t.Errorf("duplicate request failed with %d", dupResp.StatusCode)
			}
			var dupData map[string]interface{}
			_ = json.NewDecoder(dupResp.Body).Decode(&dupData)
			if dupData["status"] != "already_processed" {
				t.Errorf("expected status 'already_processed', got: %v", dupData["status"])
			}
		}()
	}
	wg.Wait()
}

func TestDemoPaymentSubmit_AbsentOrExpiredSession_Holding(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-hold-test"
	orderID := "ord_hold_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "7777", 2, 800)

	// Initiate payment while session is active
	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	_ = resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	// Customer walks away or session is canceled on kiosk before payment completes
	_ = st.CancelKioskSession(context.Background(), sess.SessionID, kioskID, time.Now().UTC())

	// Late payment arrives from phone
	payURL := fmt.Sprintf("%s/pay/%s", ts.URL, demoToken)
	payReq, _ := http.NewRequest(http.MethodPost, payURL, nil)
	payReq.Header.Set("Accept", "application/json")

	payResp, err := http.DefaultClient.Do(payReq)
	if err != nil {
		t.Fatalf("POST /pay/ failed: %v", err)
	}
	defer payResp.Body.Close()

	if payResp.StatusCode != http.StatusGone {
		t.Fatalf("expected HTTP 410 Gone for payment after canceled session, got %d", payResp.StatusCode)
	}

	var submitData map[string]interface{}
	_ = json.NewDecoder(payResp.Body).Decode(&submitData)

	if submitData["error"] == nil || submitData["error"] == "" {
		t.Errorf("expected error message in response, got: %v", submitData)
	}

	// Verify order state: rejected payment must not capture or transition to PAID_WAITING; order remains PENDING_PAYMENT
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil || ord == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status PENDING_PAYMENT, got: %s", ord.Status)
	}
}

func TestAcceptance_StrictTokenLookup(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-strict-token"
	orderID := "ord_strict_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "1234", 2, 800)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initiate payment failed: %v", err)
	}
	defer resp.Body.Close()

	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	demoToken := initResp["demo_token"].(string)
	attemptID := initResp["attempt_id"].(string)

	// 1. Valid lookup strictly by demo_token
	att, err := st.GetPaymentAttemptByToken(context.Background(), demoToken)
	if err != nil || att == nil || att.AttemptID != attemptID {
		t.Fatalf("expected GetPaymentAttemptByToken with demo_token to succeed: %v", err)
	}

	// 2. Lookup with session_id MUST return nil
	attBySess, err := st.GetPaymentAttemptByToken(context.Background(), sess.SessionID)
	if err != nil || attBySess != nil {
		t.Fatalf("expected GetPaymentAttemptByToken with session_id to return nil, got: %v, err: %v", attBySess, err)
	}

	// 3. Lookup with attempt_id MUST return nil
	attByAtt, err := st.GetPaymentAttemptByToken(context.Background(), attemptID)
	if err != nil || attByAtt != nil {
		t.Fatalf("expected GetPaymentAttemptByToken with attempt_id to return nil, got: %v, err: %v", attByAtt, err)
	}

	// 4. HTTP GET /pay/{session_id} and /pay/{attempt_id} must return 404
	for _, invalidID := range []string{sess.SessionID, attemptID, "non_existent_token"} {
		chkReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/pay/%s", ts.URL, invalidID), nil)
		chkResp, err := http.DefaultClient.Do(chkReq)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if chkResp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for invalid token %q, got %d", invalidID, chkResp.StatusCode)
		}
		chkResp.Body.Close()

		postReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/pay/%s", ts.URL, invalidID), nil)
		postReq.Header.Set("Accept", "application/json")
		postResp, err := http.DefaultClient.Do(postReq)
		if err != nil {
			t.Fatalf("post request failed: %v", err)
		}
		if postResp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 on post for invalid token %q, got %d", invalidID, postResp.StatusCode)
		}
		postResp.Body.Close()
	}
}

func TestAcceptance_RemovedRoutes_Return404(t *testing.T) {
	_, ts, _, cleanup := setupDemoTestServer(t)
	defer cleanup()

	// 1. /api/orders/{id}/simulate-payment must return 404
	simReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/ord_123/simulate-payment", ts.URL), nil)
	simResp, err := http.DefaultClient.Do(simReq)
	if err != nil {
		t.Fatalf("simulate payment request failed: %v", err)
	}
	defer simResp.Body.Close()
	if simResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for removed /api/orders/{id}/simulate-payment, got %d", simResp.StatusCode)
	}

	// With query parameters ?force=1 or ?bypass=1 it must STILL return 404
	for _, query := range []string{"?force=1", "?bypass=1", "?force=1&bypass=1"} {
		qReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/ord_123/simulate-payment%s", ts.URL, query), nil)
		qResp, err := http.DefaultClient.Do(qReq)
		if err != nil {
			t.Fatalf("simulate payment query request failed: %v", err)
		}
		if qResp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for removed /api/orders/{id}/simulate-payment with query %s, got %d", query, qResp.StatusCode)
		}
		qResp.Body.Close()
	}

	// 2. /api/webhooks/razorpay must return 404
	whReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/webhooks/razorpay", ts.URL), nil)
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatalf("razorpay webhook request failed: %v", err)
	}
	defer whResp.Body.Close()
	if whResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for removed /api/webhooks/razorpay, got %d", whResp.StatusCode)
	}
}

func TestAcceptance_InvalidAndExpiredTokens(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-tokens-test"
	orderID := "ord_tokens_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "9876", 1, 500)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	// Set expires_at to the past
	past := time.Now().UTC().Add(-1 * time.Hour)
	_, err := st.DB().ExecContext(context.Background(), "UPDATE payment_attempts SET expires_at_ns = ? WHERE demo_token = ?", past.UnixNano(), demoToken)
	if err != nil {
		t.Fatalf("failed to expire attempt: %v", err)
	}

	// GET on expired token -> 410 Gone
	getReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/pay/%s", ts.URL, demoToken), nil)
	getReq.Header.Set("Accept", "application/json")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get expired failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusGone {
		t.Errorf("expected 410 Gone for expired token on GET, got %d", getResp.StatusCode)
	}

	// POST on expired token -> 410 Gone
	postReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/pay/%s", ts.URL, demoToken), nil)
	postReq.Header.Set("Accept", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("post expired failed: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusGone {
		t.Errorf("expected 410 Gone for expired token on POST, got %d", postResp.StatusCode)
	}
}

func TestAcceptance_ConcurrentSubmissions_SingleDispatch(t *testing.T) {
	_, ts, st, cleanup := setupDemoTestServer(t)
	defer cleanup()

	kioskID := "kiosk-concurrent-test"
	orderID := "ord_conc_001"
	sess := createTestOrderAndSession(t, st, kioskID, orderID, "4321", 2, 1000)

	initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sess.SessionID)
	req, _ := http.NewRequest(http.MethodPost, initURL, nil)
	req.Header.Set("Authorization", "Bearer dev_secret_kiosk_1234567890")
	resp, _ := http.DefaultClient.Do(req)
	var initResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&initResp)
	resp.Body.Close()
	demoToken := initResp["demo_token"].(string)

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	var dispatchedCount int32
	var alreadyProcessedCount int32

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			pReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/pay/%s", ts.URL, demoToken), nil)
			pReq.Header.Set("Accept", "application/json")
			pResp, err := http.DefaultClient.Do(pReq)
			if err != nil {
				return
			}
			defer pResp.Body.Close()
			var data map[string]interface{}
			_ = json.NewDecoder(pResp.Body).Decode(&data)
			if data["status"] == "dispatched" {
				atomic.AddInt32(&dispatchedCount, 1)
			} else if data["status"] == "already_processed" {
				atomic.AddInt32(&alreadyProcessedCount, 1)
			}
		}()
	}
	wg.Wait()

	if dispatchedCount != 1 {
		t.Fatalf("expected exactly 1 dispatch, got %d (alreadyProcessed=%d)", dispatchedCount, alreadyProcessedCount)
	}
	if alreadyProcessedCount != concurrency-1 {
		t.Fatalf("expected %d already_processed responses, got %d", concurrency-1, alreadyProcessedCount)
	}
}
