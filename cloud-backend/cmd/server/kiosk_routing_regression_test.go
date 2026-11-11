package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

// setupRoutingRegressionServer starts an httptest server with the exact routing logic from main.go.
func setupRoutingRegressionServer(t *testing.T, demoMode bool) (*appServer, *httptest.Server, *store.Store, string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "xeroxgo_routing_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	dbPath := filepath.Join(tempDir, "routing_test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("failed to open store: %v", err)
	}

	hub := ws.NewHub(st)
	kioskSecret := "dev_secret_kiosk_routing_123456789"
	hub.SetKioskSecret([]byte(kioskSecret))

	cfg := &serverConfig{
		addr:               ":0",
		dbPath:             dbPath,
		jobsDir:            filepath.Join(tempDir, "jobs"),
		pricePerSheetPaise: 500,
		devMode:            true,
		demoMode:           demoMode,
		downloadSecret:     []byte("dev_secret_download_1234567890"),
		webhookSecret:      []byte("dev_secret_webhook_1234567890"),
		kioskSecret:        []byte(kioskSecret),
	}

	app := &appServer{
		cfg:        cfg,
		db:         st,
		hub:        hub,
		convWake:   make(chan struct{}, 1),
		pinLimiter: NewPINRateLimiter(st, 10, 1*time.Minute, 1*time.Minute),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/upload"):
			app.handleUpload(w, r)
		case strings.HasSuffix(path, "/recover"):
			app.handleRecoverJob(w, r)
		case strings.HasSuffix(path, "/orders/lookup"), strings.HasSuffix(path, "/pin/lookup"):
			app.handlePINLookup(w, r)
		case strings.HasSuffix(path, "/claim"), strings.HasSuffix(path, "/confirm"):
			app.handleOrderClaim(w, r)
		case strings.HasSuffix(path, "/release"):
			app.handleOrderRelease(w, r)
		case strings.HasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		case strings.HasSuffix(path, "/initiate-payment"):
			app.handleInitiatePayment(w, r)
		case strings.Contains(path, "/sessions/") && (strings.HasSuffix(path, "/cancel") || strings.HasSuffix(path, "/cancel-session")):
			app.handleSessionCancel(w, r)
		case strings.Contains(path, "/sessions/"):
			app.handleGetSession(w, r)
		case strings.HasSuffix(path, "/refill"):
			app.handleKioskRefill(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/pay/", app.handleDemoPayRoutes)
	mux.HandleFunc("/ws", hub.ServeHTTP)

	ts := httptest.NewServer(mux)

	cleanup := func() {
		ts.Close()
		_ = st.Close()
		_ = os.RemoveAll(tempDir)
	}

	return app, ts, st, kioskSecret, cleanup
}

func createRoutingTestOrder(t *testing.T, st *store.Store, kioskID, orderID, pin string, sheets int, pricePaise int64) {
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

	ord := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   pricePaise,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(15 * time.Minute),
		ClaimToken:   "token_" + orderID,
		CreatedAt:    now,
	}

	if err := st.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create routing test order: %v", err)
	}
}

// TestKioskRouting_InitiatePaymentExactPINClientURL tests the exact URL invoked by
// kiosk-daemon/internal/cloud/pin_client.go:
// POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment
// It strictly verifies that the endpoint does NOT return HTTP 405 Method Not Allowed.
func TestKioskRouting_InitiatePaymentExactPINClientURL(t *testing.T) {
	_, ts, st, kioskSecret, cleanup := setupRoutingRegressionServer(t, true)
	defer cleanup()

	kioskID := "kiosk-route-test"
	orderID := "ord_route_001"
	pin := "1234"
	sheets := 2
	pricePaise := int64(1000)

	createRoutingTestOrder(t, st, kioskID, orderID, pin, sheets, pricePaise)

	// Claim order to create active session
	claimURL := fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID)
	claimReq, err := http.NewRequest(http.MethodPost, claimURL, nil)
	if err != nil {
		t.Fatalf("failed to create claim request: %v", err)
	}
	claimReq.Header.Set("Authorization", "Bearer "+kioskSecret)

	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("claim request failed: %v", err)
	}
	defer claimResp.Body.Close()

	if claimResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(claimResp.Body)
		t.Fatalf("claim expected 200 OK, got %d: %s", claimResp.StatusCode, string(body))
	}

	var claimData struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(claimResp.Body).Decode(&claimData); err != nil {
		t.Fatalf("failed to decode claim response: %v", err)
	}
	sessionID := claimData.SessionID

	// Exact URL called by pin_client.go:
	exactURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessionID)

	req, err := http.NewRequest(http.MethodPost, exactURL, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+kioskSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	// Critical Assertion: MUST NOT BE 405 Method Not Allowed!
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("REGRESSION FAILURE: Endpoint %s returned HTTP 405 Method Not Allowed!", exactURL)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var att struct {
		AttemptID string `json:"attempt_id"`
		QRPayload string `json:"qr_payload"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(bodyBytes, &att); err != nil {
		t.Fatalf("failed to parse PaymentAttemptDetails JSON: %v", err)
	}

	if att.QRPayload == "" {
		t.Fatal("expected non-empty QRPayload in response")
	}
	if !strings.Contains(att.QRPayload, "/pay/dmo_") {
		t.Errorf("QRPayload %q does not contain expected demo checkout route", att.QRPayload)
	}
}

// TestKioskRouting_OrderInitiatePaymentBackwardCompatibility verifies that
// POST /api/k/{kiosk_id}/orders/{order_id}/initiate-payment remains functional and does not return 405.
func TestKioskRouting_OrderInitiatePaymentBackwardCompatibility(t *testing.T) {
	_, ts, st, kioskSecret, cleanup := setupRoutingRegressionServer(t, true)
	defer cleanup()

	kioskID := "kiosk-compat-test"
	orderID := "ord_compat_001"
	pin := "5678"

	createRoutingTestOrder(t, st, kioskID, orderID, pin, 1, 500)

	// Claim order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	claimResp.Body.Close()

	// Call /orders/{order_id}/initiate-payment
	compatURL := fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID)
	req, _ := http.NewRequest(http.MethodPost, compatURL, nil)
	req.Header.Set("Authorization", "Bearer "+kioskSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("compat request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("expected 200 OK, got 405 Method Not Allowed on %s", compatURL)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}
}

// TestKioskRouting_InvalidExpiredCanceledSessions asserts that non-200 responses return 409 or 400, never 405.
func TestKioskRouting_InvalidExpiredCanceledSessions(t *testing.T) {
	_, ts, st, kioskSecret, cleanup := setupRoutingRegressionServer(t, true)
	defer cleanup()

	kioskID := "kiosk-invalid-test"

	// 1. Invalid Non-Existent Session
	badURL := fmt.Sprintf("%s/api/k/%s/sessions/sess_doesnotexist/initiate-payment", ts.URL, kioskID)
	req1, _ := http.NewRequest(http.MethodPost, badURL, nil)
	req1.Header.Set("Authorization", "Bearer "+kioskSecret)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode == http.StatusMethodNotAllowed {
		t.Errorf("invalid session returned 405 Method Not Allowed, want 400 or 409")
	}
	if resp1.StatusCode != http.StatusBadRequest && resp1.StatusCode != http.StatusConflict {
		t.Errorf("invalid session returned %d, want 400 or 409", resp1.StatusCode)
	}

	// 2. Canceled Session
	orderID := "ord_cancel_001"
	createRoutingTestOrder(t, st, kioskID, orderID, "9999", 2, 1000)

	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessionID := claimBody["session_id"].(string)

	// Cancel the session
	cancelReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/sessions/%s/cancel", ts.URL, kioskID, sessionID), nil)
	cancelReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	cancelResp, _ := http.DefaultClient.Do(cancelReq)
	cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel returned %d, want 200", cancelResp.StatusCode)
	}

	// Attempt Initiate Payment on Canceled Session
	canURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessionID)
	req2, _ := http.NewRequest(http.MethodPost, canURL, nil)
	req2.Header.Set("Authorization", "Bearer "+kioskSecret)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusMethodNotAllowed {
		t.Errorf("canceled session returned 405 Method Not Allowed, want 409 Conflict")
	}
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("canceled session returned %d, want 409 Conflict", resp2.StatusCode)
	}

	// 3. Expired Session
	orderIDExp := "ord_expired_001"
	createRoutingTestOrder(t, st, kioskID, orderIDExp, "8888", 2, 1000)

	claimReqExp, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderIDExp), nil)
	claimReqExp.Header.Set("Authorization", "Bearer "+kioskSecret)
	claimRespExp, _ := http.DefaultClient.Do(claimReqExp)
	var claimExpBody map[string]interface{}
	_ = json.NewDecoder(claimRespExp.Body).Decode(&claimExpBody)
	claimRespExp.Body.Close()
	sessionIDExp := claimExpBody["session_id"].(string)

	// Fast-forward session expiry in DB to 5 minutes ago
	pastTime := time.Now().UTC().Add(-5 * time.Minute)
	_, _ = st.DB().ExecContext(context.Background(), "UPDATE kiosk_sessions SET expires_at_ns = ? WHERE session_id = ?;", pastTime.UnixNano(), sessionIDExp)

	expURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessionIDExp)
	req3, _ := http.NewRequest(http.MethodPost, expURL, nil)
	req3.Header.Set("Authorization", "Bearer "+kioskSecret)
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode == http.StatusMethodNotAllowed {
		t.Errorf("expired session returned 405 Method Not Allowed, want 409 Conflict")
	}
	if resp3.StatusCode != http.StatusConflict {
		t.Errorf("expired session returned %d, want 409 Conflict", resp3.StatusCode)
	}
}

// TestKioskRouting_PINClientExactURLPattern tests the exact URL patterns and payloads executed by
// kiosk-daemon/internal/cloud/pin_client.go to verify zero HTTP 405 errors and full round-trip fidelity.
func TestKioskRouting_PINClientExactURLPattern(t *testing.T) {
	_, ts, st, kioskSecret, cleanup := setupRoutingRegressionServer(t, true)
	defer cleanup()

	kioskID := "kiosk-pattern-test"
	orderID := "ord_pattern_001"
	pin := "4321"

	createRoutingTestOrder(t, st, kioskID, orderID, pin, 3, 1500)

	baseURL := fmt.Sprintf("%s/api/k/%s", ts.URL, kioskID)

	// 1. PIN Lookup: POST /api/k/{kiosk_id}/orders/lookup
	lookupBody, _ := json.Marshal(map[string]string{"pin": pin})
	lookupReq, _ := http.NewRequest(http.MethodPost, baseURL+"/orders/lookup", strings.NewReader(string(lookupBody)))
	lookupReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := ts.Client().Do(lookupReq)
	if err != nil {
		t.Fatalf("Lookup request failed: %v", err)
	}
	defer lookupResp.Body.Close()
	if lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("Lookup returned %d, want 200", lookupResp.StatusCode)
	}

	// 2. Claim Order: POST /api/k/{kiosk_id}/orders/{order_id}/claim
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/orders/%s/claim", baseURL, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	claimResp, err := ts.Client().Do(claimReq)
	if err != nil {
		t.Fatalf("Claim request failed: %v", err)
	}
	defer claimResp.Body.Close()
	var claimRes struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimRes)
	if claimRes.SessionID == "" {
		t.Fatal("Claim returned empty session ID")
	}

	// 3. Initiate Payment: POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment
	initURL := fmt.Sprintf("%s/sessions/%s/initiate-payment", baseURL, claimRes.SessionID)
	initReq, _ := http.NewRequest(http.MethodPost, initURL, nil)
	initReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	initResp, err := ts.Client().Do(initReq)
	if err != nil {
		t.Fatalf("InitiatePayment request failed: %v", err)
	}
	defer initResp.Body.Close()

	if initResp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("FATAL 405 on %s", initURL)
	}
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("InitiatePayment returned %d, want 200", initResp.StatusCode)
	}

	var att struct {
		AttemptID string `json:"attempt_id"`
		QRPayload string `json:"qr_payload"`
		Status    string `json:"status"`
	}
	_ = json.NewDecoder(initResp.Body).Decode(&att)
	if !strings.Contains(att.QRPayload, "/pay/dmo_") {
		t.Fatalf("QRPayload missing demo prefix: %s", att.QRPayload)
	}

	// 4. Get Session: GET /api/k/{kiosk_id}/sessions/{session_id}
	getReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/sessions/%s", baseURL, claimRes.SessionID), nil)
	getReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	getResp, err := ts.Client().Do(getReq)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GetSession returned %d, want 200", getResp.StatusCode)
	}

	// 5. Cancel Session: POST /api/k/{kiosk_id}/sessions/{session_id}/cancel
	canReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/sessions/%s/cancel", baseURL, claimRes.SessionID), nil)
	canReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	canResp, err := ts.Client().Do(canReq)
	if err != nil {
		t.Fatalf("CancelSession failed: %v", err)
	}
	defer canResp.Body.Close()
	if canResp.StatusCode != http.StatusOK {
		t.Fatalf("CancelSession returned %d, want 200", canResp.StatusCode)
	}
}
