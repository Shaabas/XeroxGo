package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func setupTestAppWithOrder(t *testing.T, kioskID, orderID, pin string, status contracts.JobStatus, pricePaise int64, pinExpiresAt time.Time) (*appServer, string, *httptest.Server) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "app_session_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     4,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    pin,
	}
	ord := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       status,
		PricePaise:   pricePaise,
		PINExpiresAt: pinExpiresAt,
	}
	if err := st.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to seed test order: %v", err)
	}

	secret := "test-secret-4321"
	app := &appServer{
		cfg: &serverConfig{
			kioskSecret:   []byte(secret),
			webhookSecret: []byte("whsec_test"),
			devMode:       true,
		},
		db: st,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case stringsHasSuffix(path, "/orders/lookup"):
			app.handlePINLookup(w, r)
		case stringsHasSuffix(path, "/claim"), stringsHasSuffix(path, "/confirm"):
			app.handleOrderClaim(w, r)
		case stringsContains(path, "/sessions/") && stringsHasSuffix(path, "/cancel"), stringsHasSuffix(path, "/cancel-session"):
			app.handleSessionCancel(w, r)
		case stringsContains(path, "/sessions/"):
			app.handleGetSession(w, r)
		case stringsHasSuffix(path, "/initiate-payment"):
			app.handleInitiatePayment(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})

	return app, secret, ts
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func stringsContains(s, substr string) bool {
	return len(s) >= len(substr) && bytes.Contains([]byte(s), []byte(substr))
}

func TestTransactionSession_ClaimSuccessAndIdempotency(t *testing.T) {
	now := time.Now().UTC()
	_, secret, ts := setupTestAppWithOrder(t, "kiosk-001", "ord_claim_01", "1234", contracts.JobStatusPendingPayment, 1800, now.Add(10*time.Minute))

	claimURL := ts.URL + "/api/k/kiosk-001/orders/ord_claim_01/claim"

	// 1. Claim without Auth -> 401 Unauthorized
	req, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Claim with Auth -> 200 OK
	req, _ = http.NewRequest(http.MethodPost, claimURL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on claim, got %d", resp.StatusCode)
	}

	var sess1 sessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sess1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	resp.Body.Close()

	if sess1.SessionID == "" {
		t.Errorf("expected non-empty session ID")
	}
	if sess1.OrderID != "ord_claim_01" {
		t.Errorf("expected order_id ord_claim_01, got %s", sess1.OrderID)
	}
	if sess1.TotalPricePaise != 1800 {
		t.Errorf("expected price 1800 paise, got %d", sess1.TotalPricePaise)
	}
	if sess1.Status != "PAYMENT_PENDING" {
		t.Errorf("expected status PAYMENT_PENDING, got %s", sess1.Status)
	}

	// 3. Repeat claim -> Idempotent, returns exact same session
	req2, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second claim failed: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on repeat claim, got %d", resp2.StatusCode)
	}
	var sess2 sessionResponse
	_ = json.NewDecoder(resp2.Body).Decode(&sess2)
	resp2.Body.Close()

	if sess2.SessionID != sess1.SessionID {
		t.Errorf("idempotent claim returned divergent session: %s vs %s", sess2.SessionID, sess1.SessionID)
	}
}

func TestTransactionSession_ConflictAndCompetingClaims(t *testing.T) {
	now := time.Now().UTC()
	_, secret, ts := setupTestAppWithOrder(t, "kiosk-comp-1", "ord_comp_01", "5566", contracts.JobStatusPendingPayment, 1200, now.Add(10*time.Minute))

	// Claim from legitimate kiosk
	claimURL1 := ts.URL + "/api/k/kiosk-comp-1/orders/ord_comp_01/claim"
	req1, _ := http.NewRequest(http.MethodPost, claimURL1, nil)
	req1.Header.Set("Authorization", "Bearer "+secret)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("first claim failed: %v, status: %d", err, resp1.StatusCode)
	}
	resp1.Body.Close()

	// Attempt claim from competing kiosk -> 409 Conflict (or 403)
	claimURL2 := ts.URL + "/api/k/kiosk-comp-2/orders/ord_comp_01/claim"
	req2, _ := http.NewRequest(http.MethodPost, claimURL2, nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("competing claim failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict && resp2.StatusCode != http.StatusForbidden {
		t.Errorf("expected 409 Conflict or 403 Forbidden, got %d", resp2.StatusCode)
	}
}

func TestTransactionSession_RequireActiveSessionForPaymentInitiation(t *testing.T) {
	now := time.Now().UTC()
	_, secret, ts := setupTestAppWithOrder(t, "kiosk-pay-01", "ord_pay_01", "7788", contracts.JobStatusPendingPayment, 2500, now.Add(10*time.Minute))

	payURL := ts.URL + "/api/k/kiosk-pay-01/orders/ord_pay_01/initiate-payment"

	// 1. Attempt payment initiation BEFORE claiming session -> must fail with 409 Conflict!
	req, _ := http.NewRequest(http.MethodPost, payURL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initiate payment request failed: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict without active session, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Claim order session
	claimURL := ts.URL + "/api/k/kiosk-pay-01/orders/ord_pay_01/claim"
	claimReq, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	claimReq.Header.Set("Authorization", "Bearer "+secret)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v", err)
	}
	claimResp.Body.Close()

	// 3. Attempt payment initiation AFTER claiming session -> succeeds with 200 OK!
	reqAfter, _ := http.NewRequest(http.MethodPost, payURL, nil)
	reqAfter.Header.Set("Authorization", "Bearer "+secret)
	respAfter, err := http.DefaultClient.Do(reqAfter)
	if err != nil {
		t.Fatalf("initiate payment failed: %v", err)
	}
	if respAfter.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK with active session, got %d", respAfter.StatusCode)
	}

	var payResp map[string]interface{}
	_ = json.NewDecoder(respAfter.Body).Decode(&payResp)
	respAfter.Body.Close()

	if payResp["status"] != "PAYMENT_PENDING" {
		t.Errorf("expected status PAYMENT_PENDING, got %v", payResp["status"])
	}
	if int64(payResp["amount_paise"].(float64)) != 2500 {
		t.Errorf("expected amount 2500 paise, got %v", payResp["amount_paise"])
	}
}

func TestTransactionSession_CancellationFlow(t *testing.T) {
	now := time.Now().UTC()
	_, secret, ts := setupTestAppWithOrder(t, "kiosk-can-01", "ord_can_01", "3344", contracts.JobStatusPendingPayment, 1500, now.Add(10*time.Minute))

	// Claim session
	claimURL := ts.URL + "/api/k/kiosk-can-01/orders/ord_can_01/claim"
	req, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, _ := http.DefaultClient.Do(req)
	var sess sessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()

	// Cancel session
	cancelURL := ts.URL + "/api/k/kiosk-can-01/sessions/" + sess.SessionID + "/cancel"
	canReq, _ := http.NewRequest(http.MethodPost, cancelURL, nil)
	canReq.Header.Set("Authorization", "Bearer "+secret)
	canResp, err := http.DefaultClient.Do(canReq)
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if canResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK on cancel, got %d", canResp.StatusCode)
	}
	canResp.Body.Close()

	// Payment initiation must now fail because session was cancelled!
	payURL := ts.URL + "/api/k/kiosk-can-01/orders/ord_can_01/initiate-payment"
	payReq, _ := http.NewRequest(http.MethodPost, payURL, nil)
	payReq.Header.Set("Authorization", "Bearer "+secret)
	payResp, _ := http.DefaultClient.Do(payReq)
	if payResp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict after cancellation, got %d", payResp.StatusCode)
	}
	payResp.Body.Close()
}

func TestTransactionSession_ReadOnlyInvariant(t *testing.T) {
	now := time.Now().UTC()
	app, secret, ts := setupTestAppWithOrder(t, "kiosk-ro-01", "ord_ro_01", "9012", contracts.JobStatusPendingPayment, 1400, now.Add(10*time.Minute))

	// 1. Claim session
	claimURL := ts.URL + "/api/k/kiosk-ro-01/orders/ord_ro_01/claim"
	req, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v", err)
	}
	resp.Body.Close()

	// Verify order status in database is STILL PENDING_PAYMENT
	orderAfter, err := app.db.GetOrder(context.Background(), "ord_ro_01")
	if err != nil {
		t.Fatalf("failed to get order: %v", err)
	}
	if orderAfter.Status != contracts.JobStatusPendingPayment {
		t.Fatalf("INVARIANT VIOLATION: order status mutated to %s by session claim", orderAfter.Status)
	}
}
