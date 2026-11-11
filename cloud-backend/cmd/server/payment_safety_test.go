package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func setupSafetyTestApp(t *testing.T, kioskID, orderID, pin string, status contracts.JobStatus, pricePaise int64, pinExpiresAt time.Time) (*appServer, string, string, *httptest.Server, *store.Store) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "safety_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
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
		ClaimToken:   "rec_credential_test_123",
	}
	if err := st.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to seed test order: %v", err)
	}

	kioskSecret := "kiosk-sec-123"
	webhookSecret := "whsec_safety_test_32_bytes_long"
	app := &appServer{
		cfg: &serverConfig{
			kioskSecret:   []byte(kioskSecret),
			webhookSecret: []byte(webhookSecret),
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
		case stringsHasSuffix(path, "/claim"):
			app.handleOrderClaim(w, r)
		case stringsHasSuffix(path, "/release"):
			app.handleOrderRelease(w, r)
		case stringsHasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		case stringsContains(path, "/sessions/") && stringsHasSuffix(path, "/cancel"):
			app.handleSessionCancel(w, r)
		case stringsHasSuffix(path, "/initiate-payment"):
			app.handleInitiatePayment(w, r)
		case stringsContains(path, "/sessions/"):
			app.handleGetSession(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case stringsHasSuffix(path, "/status"):
			app.handleOrderStatus(w, r)
		case stringsHasSuffix(path, "/simulate-payment"):
			app.handleSimulatePayment(w, r)
		case stringsHasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if app.hub != nil {
			app.hub.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})

	return app, kioskSecret, webhookSecret, ts, st
}

func generateSignedWebhookPayload(orderID, eventID, sessionID, kioskID string, amount int64, secret string) ([]byte, string) {
	payload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       eventID,
					"order_id": orderID,
					"amount":   amount,
					"status":   "captured",
					"notes": map[string]interface{}{
						"order_id":   orderID,
						"session_id": sessionID,
						"kiosk_id":   kioskID,
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	return body, sig
}

// 1. Test Ghost Printing Prevention on Late Payment (No Session)
func TestPaymentSafety_LatePaymentNoSession_PreservesPaidWaitingNoDispatch(t *testing.T) {
	kioskID := "kiosk-safe-1"
	orderID := "ord_late_001"
	pin := "1234"
	pricePaise := int64(500)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, _, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// User never visited kiosk or session expired. Payment webhook arrives.
	body, sig := generateSignedWebhookPayload(orderID, "pay_late_123", "", kioskID, pricePaise, whSec)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from webhook, got %d", resp.StatusCode)
	}

	var res map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if res["status"] != "paid_unprinted" {
		t.Fatalf("expected status 'paid_unprinted' (ghost printing prevented), got %q", res["status"])
	}

	// Verify order status is preserved as PAID_WAITING (not dispatched, not lost)
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("failed to get order: %v", err)
	}
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING, got %s", ord.Status)
	}
}

// 2. Test Ghost Printing Prevention when Customer Canceled Kiosk Session
func TestPaymentSafety_CanceledSession_NoGhostPrinting(t *testing.T) {
	kioskID := "kiosk-safe-2"
	orderID := "ord_cancel_001"
	pin := "2345"
	pricePaise := int64(750)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// 1. Kiosk claims order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to claim order: %v, status: %d", err, claimResp.StatusCode)
	}
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessID := claimBody["session_id"].(string)

	// 2. Customer cancels session
	cancelReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/sessions/%s/cancel", ts.URL, kioskID, sessID), nil)
	cancelReq.Header.Set("Authorization", "Bearer "+kSec)
	cancelResp, err := http.DefaultClient.Do(cancelReq)
	if err != nil || cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to cancel session: %v", err)
	}
	cancelResp.Body.Close()

	// 3. Delayed payment webhook arrives
	body, sig := generateSignedWebhookPayload(orderID, "pay_after_cancel", sessID, kioskID, pricePaise, whSec)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook request failed: %v, code %d", err, resp.StatusCode)
	}
	var res map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	if res["status"] != "paid_unprinted" {
		t.Fatalf("expected paid_unprinted, got %q", res["status"])
	}

	// Verify order is preserved as PAID_WAITING
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected PAID_WAITING, got %s", ord.Status)
	}
}

// 3. Test Concurrent Webhooks: Atomic transition prevents duplicate print authorizations
func TestPaymentSafety_ConcurrentWebhooks_AtomicSingleDispatch(t *testing.T) {
	kioskID := "kiosk-safe-3"
	orderID := "ord_conc_001"
	pin := "3456"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Active session exists at the kiosk
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to claim order: %v", err)
	}
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessID := claimBody["session_id"].(string)

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	var dispatchedCount int32
	var duplicateOrProcessedCount int32

	for i := 0; i < concurrency; i++ {
		eventID := fmt.Sprintf("evt_conc_%d", i) // distinct event IDs to test database-level transition safety
		go func(eID string) {
			defer wg.Done()
			body, sig := generateSignedWebhookPayload(orderID, eID, sessID, kioskID, pricePaise, whSec)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Razorpay-Signature", sig)

			r, reqErr := http.DefaultClient.Do(req)
			if reqErr != nil {
				return
			}
			defer r.Body.Close()

			var m map[string]string
			_ = json.NewDecoder(r.Body).Decode(&m)
			if m["status"] == "dispatched" {
				atomic.AddInt32(&dispatchedCount, 1)
			} else if m["status"] == "already_processed" || m["status"] == "duplicate" {
				atomic.AddInt32(&duplicateOrProcessedCount, 1)
			}
		}(eventID)
	}

	wg.Wait()

	// Exactly ONE webhook can authorize dispatch!
	if dispatchedCount != 1 {
		t.Fatalf("expected exactly 1 dispatched webhook, got %d (concurrent duplicates=%d)", dispatchedCount, duplicateOrProcessedCount)
	}

	// Order status must be DISPATCHED
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected DISPATCHED order status, got %s", ord.Status)
	}
}

// 4. Test Duplicate Webhooks: Replaying same event ID returns idempotent duplicate status
func TestPaymentSafety_DuplicateWebhooks_Idempotent(t *testing.T) {
	kioskID := "kiosk-safe-4"
	orderID := "ord_dup_001"
	pin := "4567"
	pricePaise := int64(400)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Claim order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	cResp, _ := http.DefaultClient.Do(claimReq)
	var claimBody map[string]interface{}
	_ = json.NewDecoder(cResp.Body).Decode(&claimBody)
	cResp.Body.Close()
	sessID := claimBody["session_id"].(string)

	eventID := "pay_fixed_event_1"
	body, sig := generateSignedWebhookPayload(orderID, eventID, sessID, kioskID, pricePaise, whSec)

	// 1st request
	req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Razorpay-Signature", sig)
	resp1, _ := http.DefaultClient.Do(req1)
	var res1 map[string]string
	_ = json.NewDecoder(resp1.Body).Decode(&res1)
	resp1.Body.Close()
	if res1["status"] != "dispatched" {
		t.Fatalf("expected first request dispatched, got %v", res1)
	}

	// 2nd identical request
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Razorpay-Signature", sig)
	resp2, _ := http.DefaultClient.Do(req2)
	var res2 map[string]string
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	resp2.Body.Close()
	if res2["status"] != "duplicate" && res2["status"] != "already_processed" {
		t.Fatalf("expected duplicate or already_processed for replayed webhook, got %v", res2)
	}
}

// 5. Test Expired PIN Recovery & Release for Paid-but-Unprinted Orders
func TestPaymentSafety_ExpiredPIN_RecoveryAndRelease(t *testing.T) {
	kioskID := "kiosk-safe-5"
	orderID := "ord_recover_001"
	oldPIN := "5678"
	pricePaise := int64(600)

	// Initial order created with already-expired PIN timestamp (e.g. 1 hour ago)
	expiredTime := time.Now().UTC().Add(-1 * time.Hour)
	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, oldPIN, contracts.JobStatusPendingPayment, pricePaise, expiredTime)

	// User paid late (payment captures despite expired PIN)
	body, sig := generateSignedWebhookPayload(orderID, "pay_late_exp_pin", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Verify order is PAID_WAITING
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected PAID_WAITING, got %s", ord.Status)
	}

	// Step 1: Kiosk PIN lookup with old expired PIN MUST fail
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID), bytes.NewReader([]byte(fmt.Sprintf(`{"pin":%q}`, oldPIN))))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, _ := http.DefaultClient.Do(lookupReq)
	if lookupResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for expired PIN lookup, got %d", lookupResp.StatusCode)
	}
	lookupResp.Body.Close()

	// Step 2: Customer requests fresh 4-digit PIN via recovery endpoint
	recReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/%s/recovery-pin", ts.URL, orderID), nil)
	recReq.Header.Set("X-Recovery-Token", "rec_credential_test_123")
	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil || recResp.StatusCode != http.StatusOK {
		t.Fatalf("recovery PIN request failed: %v, status: %d", err, recResp.StatusCode)
	}
	var recBody map[string]interface{}
	_ = json.NewDecoder(recResp.Body).Decode(&recBody)
	recResp.Body.Close()

	newPIN := recBody["display_code"].(string)
	if len(newPIN) != 4 {
		t.Fatalf("expected 4-digit recovery PIN, got %q", newPIN)
	}

	// Step 3: Customer enters new 4-digit PIN at kiosk -> lookup succeeds!
	lookupReq2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID), bytes.NewReader([]byte(fmt.Sprintf(`{"pin":%q}`, newPIN))))
	lookupReq2.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq2.Header.Set("Content-Type", "application/json")
	lookupResp2, err := http.DefaultClient.Do(lookupReq2)
	if err != nil || lookupResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for renewed PIN lookup, got %d", lookupResp2.StatusCode)
	}
	var lookupData map[string]interface{}
	_ = json.NewDecoder(lookupResp2.Body).Decode(&lookupData)
	lookupResp2.Body.Close()
	if lookupData["status"] != "PAID_WAITING" {
		t.Fatalf("expected status PAID_WAITING in lookup, got %v", lookupData["status"])
	}

	// Step 4: Kiosk claims the order session
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v, status: %d", err, claimResp.StatusCode)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	// Step 5: Kiosk releases physical print dispatch
	releaseReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/release", ts.URL, kioskID, orderID), bytes.NewReader([]byte(fmt.Sprintf(`{"session_id":%q}`, sessionID))))
	releaseReq.Header.Set("Authorization", "Bearer "+kSec)
	releaseReq.Header.Set("Content-Type", "application/json")
	releaseResp, err := http.DefaultClient.Do(releaseReq)
	if err != nil || releaseResp.StatusCode != http.StatusOK {
		t.Fatalf("release failed: %v, status: %d", err, releaseResp.StatusCode)
	}
	var releaseData map[string]interface{}
	_ = json.NewDecoder(releaseResp.Body).Decode(&releaseData)
	releaseResp.Body.Close()

	if releaseData["status"] != "DISPATCHED" {
		t.Fatalf("expected DISPATCHED status from release, got %v", releaseData["status"])
	}

	// Verify order status in DB is DISPATCHED
	ordFinal, _ := st.GetOrder(context.Background(), orderID)
	if ordFinal.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected final order status DISPATCHED, got %s", ordFinal.Status)
	}
}

// 6. Test Mismatched Session Notes: Notes specifying wrong session ID cannot authorize print
func TestPaymentSafety_MismatchedSessionNotes_Prevented(t *testing.T) {
	kioskID := "kiosk-safe-6"
	orderID := "ord_mismatch_001"
	pin := "6789"
	pricePaise := int64(800)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Active session created at kiosk
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessID := claimBody["session_id"].(string)

	// Webhook arrives with a fraudulent or stale session_id in notes
	body, sig := generateSignedWebhookPayload(orderID, "pay_spoofed_session", "sess_stale_or_spoofed_999", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)

	var res map[string]string
	_ = json.NewDecoder(whResp.Body).Decode(&res)
	whResp.Body.Close()

	if res["status"] != "paid_unprinted" {
		t.Fatalf("expected paid_unprinted for session mismatch, got %q", res["status"])
	}

	// Order is preserved in PAID_WAITING
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected PAID_WAITING, got %s", ord.Status)
	}

	// Now release with the genuine session ID -> succeeds!
	relReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/release", ts.URL, kioskID, orderID), bytes.NewReader([]byte(fmt.Sprintf(`{"session_id":%q}`, sessID))))
	relReq.Header.Set("Authorization", "Bearer "+kSec)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, _ := http.DefaultClient.Do(relReq)
	if relResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK releasing with genuine session, got %d", relResp.StatusCode)
	}
	relResp.Body.Close()
}

// 7. Test Server & Daemon Restart: Persistent DB retains state; re-entering PIN safely claims & releases
func TestPaymentSafety_RestartPreservationAndReconciliation(t *testing.T) {
	kioskID := "kiosk-safe-7"
	orderID := "ord_restart_001"
	pin := "7890"
	pricePaise := int64(950)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "restart_test.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    pin,
	}
	ord := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   pricePaise,
		PINExpiresAt: pinExpiry,
		ClaimToken:   "rec_credential_restart_123",
	}
	if err := st1.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to seed test order: %v", err)
	}

	kioskSecret := "kiosk-sec-restart"
	whSecret := "whsec_restart_test_secret"

	app1 := &appServer{
		cfg: &serverConfig{
			kioskSecret:   []byte(kioskSecret),
			webhookSecret: []byte(whSecret),
			devMode:       true,
		},
		db: st1,
	}

	mux1 := http.NewServeMux()
	mux1.HandleFunc("/api/webhooks/razorpay", app1.handleRazorpayWebhook)
	ts1 := httptest.NewServer(mux1)

	// Payment clears before customer reaches kiosk (or while kiosk was down)
	body, sig := generateSignedWebhookPayload(orderID, "pay_restart_evt", "", kioskID, pricePaise, whSecret)
	whReq, _ := http.NewRequest(http.MethodPost, ts1.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Shutdown server 1 and close store
	ts1.Close()
	_ = st1.Close()

	// -------------------------------------------------------------
	// SERVER / DAEMON CRASH & RESTART RECONCILIATION
	// -------------------------------------------------------------
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store on restart: %v", err)
	}
	defer st2.Close()

	app2 := &appServer{
		cfg: &serverConfig{
			kioskSecret:   []byte(kioskSecret),
			webhookSecret: []byte(whSecret),
			devMode:       true,
		},
		db: st2,
	}

	mux2 := http.NewServeMux()
	mux2.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case stringsHasSuffix(path, "/orders/lookup"):
			app2.handlePINLookup(w, r)
		case stringsHasSuffix(path, "/claim"):
			app2.handleOrderClaim(w, r)
		case stringsHasSuffix(path, "/release"):
			app2.handleOrderRelease(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	ts2 := httptest.NewServer(mux2)
	defer ts2.Close()

	// Customer stands in front of rebooted kiosk, enters 4-digit PIN
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts2.URL, kioskID), bytes.NewReader([]byte(fmt.Sprintf(`{"pin":%q}`, pin))))
	lookupReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("lookup after restart failed: %v, status: %d", err, lookupResp.StatusCode)
	}
	var lookupData map[string]interface{}
	_ = json.NewDecoder(lookupResp.Body).Decode(&lookupData)
	lookupResp.Body.Close()
	if lookupData["status"] != "PAID_WAITING" {
		t.Fatalf("expected status PAID_WAITING, got %v", lookupData["status"])
	}

	// Rebooted kiosk claims session
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts2.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim after restart failed: %v, status: %d", err, claimResp.StatusCode)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessID := claimData["session_id"].(string)

	// Rebooted kiosk releases print
	relReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/release", ts2.URL, kioskID, orderID), bytes.NewReader([]byte(fmt.Sprintf(`{"session_id":%q}`, sessID))))
	relReq.Header.Set("Authorization", "Bearer "+kioskSecret)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, err := http.DefaultClient.Do(relReq)
	if err != nil || relResp.StatusCode != http.StatusOK {
		t.Fatalf("release after restart failed: %v, status: %d", err, relResp.StatusCode)
	}
	relResp.Body.Close()

	// Verify order status is DISPATCHED
	ordFinal, err := st2.GetOrder(context.Background(), orderID)
	if err != nil || ordFinal.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected order status DISPATCHED after restart, got %v (err=%v)", ordFinal, err)
	}
}

// 8. Test Recovery PIN Requires Valid Recovery Credential (Claim Token)
func TestPaymentSafety_RecoveryPIN_RequiresValidCredential(t *testing.T) {
	kioskID := "kiosk-cred-1"
	orderID := "ord_cred_001"
	pin := "1111"
	pricePaise := int64(500)
	pinExpiry := time.Now().UTC().Add(-1 * time.Hour) // expired PIN

	_, kSec, whSec, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Capture payment into PAID_WAITING
	body, sig := generateSignedWebhookPayload(orderID, "pay_cred_test", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// 1. Attempt recovery without any credential -> 401 Unauthorized
	reqNoCred, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/%s/recovery-pin", ts.URL, orderID), nil)
	respNoCred, err := http.DefaultClient.Do(reqNoCred)
	if err != nil || respNoCred.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without recovery token, got %v (status %d)", err, respNoCred.StatusCode)
	}
	respNoCred.Body.Close()

	// 2. Attempt recovery with wrong credential -> 403 Forbidden
	reqBadCred, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/%s/recovery-pin", ts.URL, orderID), nil)
	reqBadCred.Header.Set("X-Recovery-Token", "wrong_token_abcdef")
	respBadCred, err := http.DefaultClient.Do(reqBadCred)
	if err != nil || respBadCred.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with invalid recovery token, got %v (status %d)", err, respBadCred.StatusCode)
	}
	respBadCred.Body.Close()

	// 3. Attempt via kiosk-scoped endpoint without recovery token (even with valid kiosk bearer secret) -> 401 Unauthorized
	reqKioskNoCred, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/recovery-pin", ts.URL, kioskID, orderID), nil)
	reqKioskNoCred.Header.Set("Authorization", "Bearer "+kSec)
	respKioskNoCred, err := http.DefaultClient.Do(reqKioskNoCred)
	if err != nil || respKioskNoCred.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized on kiosk endpoint without recovery token, got %v (status %d)", err, respKioskNoCred.StatusCode)
	}
	respKioskNoCred.Body.Close()

	// 4. Attempt via kiosk-scoped endpoint with valid recovery token -> 200 OK
	reqKioskValid, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/recovery-pin", ts.URL, kioskID, orderID), nil)
	reqKioskValid.Header.Set("Authorization", "Bearer "+kSec)
	reqKioskValid.Header.Set("X-Recovery-Token", "rec_credential_test_123")
	respKioskValid, err := http.DefaultClient.Do(reqKioskValid)
	if err != nil || respKioskValid.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on kiosk endpoint with valid recovery token, got %v (status %d)", err, respKioskValid.StatusCode)
	}
	var resValid map[string]interface{}
	_ = json.NewDecoder(respKioskValid.Body).Decode(&resValid)
	respKioskValid.Body.Close()
	newPIN := resValid["display_code"].(string)
	if len(newPIN) != 4 {
		t.Fatalf("expected 4-digit renewed PIN, got %v", resValid["display_code"])
	}

	// Verify order can now be looked up with the new PIN
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID), bytes.NewReader([]byte(fmt.Sprintf(`{"pin":%q}`, newPIN))))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK looking up renewed PIN, got %v (status %d)", err, lookupResp.StatusCode)
	}
	lookupResp.Body.Close()
}

// 9. Test Missing Session Binding Rejects Physical Print Authorization
func TestPaymentSafety_MissingSessionBinding_RejectsDispatch(t *testing.T) {
	kioskID := "kiosk-sess-binding"
	orderID := "ord_missing_sess_001"
	pin := "2222"
	pricePaise := int64(800)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Active session claimed at kiosk
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	claimResp.Body.Close()

	// Webhook arrives with empty session_id in notes
	body, sig := generateSignedWebhookPayload(orderID, "pay_empty_sess", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)

	var res map[string]string
	_ = json.NewDecoder(whResp.Body).Decode(&res)
	whResp.Body.Close()

	// Must reject dispatch because session binding is missing
	if res["status"] != "paid_unprinted" {
		t.Fatalf("expected paid_unprinted due to missing session binding, got %q", res["status"])
	}

	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING, got %s", ord.Status)
	}
}

// 10. Test Paid-Order Release Requires Connected Kiosk Presence
func TestPaymentSafety_ReleaseRequiresConnectedKioskPresence(t *testing.T) {
	kioskID := "kiosk-presence-1"
	orderID := "ord_presence_001"
	pin := "3333"
	pricePaise := int64(900)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Attach hub to app to enforce live connection check
	hub := ws.NewHub(st)
	app.hub = hub

	// Payment is captured into PAID_WAITING
	body, sig := generateSignedWebhookPayload(orderID, "pay_pres_test", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Create an active session in DB
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessID := claimData["session_id"].(string)

	// Attempt release when kiosk is offline (no WebSocket connection in hub)
	relReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/release", ts.URL, kioskID, orderID), bytes.NewReader([]byte(fmt.Sprintf(`{"session_id":%q}`, sessID))))
	relReq.Header.Set("Authorization", "Bearer "+kSec)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, err := http.DefaultClient.Do(relReq)
	if err != nil {
		t.Fatalf("release request error: %v", err)
	}
	defer relResp.Body.Close()

	if relResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when kiosk is offline, got %d", relResp.StatusCode)
	}

	// Order must still be PAID_WAITING, session must still be PAYMENT_PENDING
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING, got %s", ord.Status)
	}
	sess, _ := st.GetActiveKioskSession(context.Background(), kioskID, orderID, time.Now().UTC())
	if sess == nil {
		t.Fatalf("expected active session to remain uncompleted")
	}
}

// 11. Test Dispatch Intent Durable Persistence and WebSocket Reconnect Reconciliation
func TestPaymentSafety_DispatchIntentReconciliation_OnKioskReconnect(t *testing.T) {
	kioskID := "kiosk-reconnect-1"
	orderID := "ord_recon_001"
	pin := "4444"
	pricePaise := int64(1200)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kSec))
	app.hub = hub

	// Active session claimed
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	cResp, _ := http.DefaultClient.Do(claimReq)
	var cData map[string]interface{}
	_ = json.NewDecoder(cResp.Body).Decode(&cData)
	cResp.Body.Close()
	sessID := cData["session_id"].(string)

	// Payment captured with valid session note, but kiosk is currently offline.
	// Order moves to DISPATCHED, intent is recorded as PENDING in dispatch_intents table.
	body, sig := generateSignedWebhookPayload(orderID, "pay_recon_evt", sessID, kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Verify pending intent exists in dispatch_intents
	pending, err := st.GetPendingDispatchOrders(context.Background(), kioskID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending dispatch order, got %d (err=%v)", len(pending), err)
	}
	if pending[0].JobID != "job_"+orderID {
		t.Fatalf("expected pending job_id 'job_%s', got %s", orderID, pending[0].JobID)
	}

	// Now kiosk connects via WebSocket
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + kSec},
		},
	}
	wsConn, _, err := websocket.Dial(ctx, u.String(), dialOpts)
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Wait for reconciliation: hub sends pending job command and marks intent SENT
	deadline := time.Now().Add(3 * time.Second)
	reconciled := false
	for time.Now().Before(deadline) {
		p, _ := st.GetPendingDispatchOrders(context.Background(), kioskID)
		if len(p) == 0 {
			reconciled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !reconciled {
		t.Fatalf("expected pending dispatch intent to be reconciled upon kiosk reconnect")
	}
}

func TestPaymentSafety_RecoveryPIN_QueryParamRejected(t *testing.T) {
	kioskID := "kiosk-qp-1"
	orderID := "ord_queryparam_001"
	pin := "1234"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, _, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	defer ts.Close()
	defer st.Close()
	_ = app

	claimToken := "rec_credential_test_123"

	// Attempt recovery using URL query param -> MUST be rejected (401 Unauthorized)
	queryURL := fmt.Sprintf("%s/api/orders/%s/recovery-pin?recovery_token=%s", ts.URL, orderID, claimToken)
	req1, _ := http.NewRequest(http.MethodPost, queryURL, nil)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for query param token, got %d", resp1.StatusCode)
	}

	// Now pay the order so recovery PIN can be issued
	body, sig := generateSignedWebhookPayload(orderID, "pay_qp_evt", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Attempt using X-Recovery-Token header -> MUST succeed (200 OK)
	headerURL := fmt.Sprintf("%s/api/orders/%s/recovery-pin", ts.URL, orderID)
	req2, _ := http.NewRequest(http.MethodPost, headerURL, nil)
	req2.Header.Set("X-Recovery-Token", claimToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for X-Recovery-Token header, got %d", resp2.StatusCode)
	}

	// Attempt using Authorization: Bearer <token> -> MUST also succeed
	req3, _ := http.NewRequest(http.MethodPost, headerURL, nil)
	req3.Header.Set("Authorization", "Bearer "+claimToken)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Authorization: Bearer token, got %d", resp3.StatusCode)
	}
}

func TestPaymentSafety_ReleaseRequiresExplicitSessionID(t *testing.T) {
	kioskID := "kiosk-relsess-1"
	orderID := "ord_rel_sess_001"
	pin := "1234"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	defer ts.Close()
	defer st.Close()

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kSec))
	app.hub = hub

	claimToken := "rec_credential_test_123"

	// Pay the order (moves to PAID_WAITING since no kiosk session exists)
	body, sig := generateSignedWebhookPayload(orderID, "pay_rel_sess_evt", "", kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Connect kiosk via WS so physical presence check passes
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Call release with empty session_id -> MUST return 400 Bad Request
	relURL := fmt.Sprintf("%s/api/k/%s/orders/%s/release", ts.URL, kioskID, orderID)
	badBody := `{"session_id": ""}`
	req1, _ := http.NewRequest(http.MethodPost, relURL, strings.NewReader(badBody))
	req1.Header.Set("Authorization", "Bearer "+kSec)
	req1.Header.Set("X-Recovery-Token", claimToken)
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty session_id, got %d", resp1.StatusCode)
	}
}

func TestPaymentSafety_ReconnectSkipsCanceledOrExpiredSessionJob(t *testing.T) {
	kioskID := "kiosk-cancel-reconn-1"
	orderID := "ord_cancel_reconn_001"
	pin := "1234"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	defer ts.Close()
	defer st.Close()

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kSec))
	app.hub = hub

	// Kiosk claims order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	cResp, _ := http.DefaultClient.Do(claimReq)
	var cData map[string]interface{}
	_ = json.NewDecoder(cResp.Body).Decode(&cData)
	cResp.Body.Close()
	sessID := cData["session_id"].(string)

	// Payment arrives while kiosk is offline -> records dispatch intent in PENDING
	body, sig := generateSignedWebhookPayload(orderID, "pay_cancel_reconn_evt", sessID, kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Customer cancels the kiosk session before kiosk reconnects
	cancelReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/sessions/%s/cancel", ts.URL, kioskID, sessID), nil)
	cancelReq.Header.Set("Authorization", "Bearer "+kSec)
	canResp, err := http.DefaultClient.Do(cancelReq)
	if err != nil || canResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to cancel session: err=%v, code=%d", err, canResp.StatusCode)
	}
	canResp.Body.Close()

	// Kiosk now connects via WebSocket
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Give the hub a moment to run reconciliation
	time.Sleep(100 * time.Millisecond)

	// Verify that the order was NOT dispatched, intent was CANCELED, and order is in PAID_WAITING
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING after reconnect with canceled session, got %s", ord.Status)
	}

	pending, err := st.GetPendingDispatchOrders(context.Background(), kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending dispatch orders, got %d", len(pending))
	}
}

func TestPaymentSafety_KioskAckTransitionsIntentToAcknowledged(t *testing.T) {
	kioskID := "kiosk-ack-1"
	orderID := "ord_ack_001"
	pin := "1234"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	defer ts.Close()
	defer st.Close()

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kSec))
	app.hub = hub

	// Connect kiosk via WS
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Claim order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	cResp, _ := http.DefaultClient.Do(claimReq)
	var cData map[string]interface{}
	_ = json.NewDecoder(cResp.Body).Decode(&cData)
	cResp.Body.Close()
	sessID := cData["session_id"].(string)

	// Pay order -> job is dispatched to connected kiosk
	body, sig := generateSignedWebhookPayload(orderID, "pay_ack_evt", sessID, kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, _ := http.DefaultClient.Do(whReq)
	whResp.Body.Close()

	// Read job message from kiosk websocket (skipping any initial sync_ads messages)
	var jobCmd *contracts.PrintJobCommand
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	for {
		var rawMsg struct {
			Type string                     `json:"type"`
			Job  *contracts.PrintJobCommand `json:"job"`
		}
		if err := wsjson.Read(readCtx, wsConn, &rawMsg); err != nil {
			t.Fatalf("failed reading websocket message: %v", err)
		}
		if rawMsg.Type == "job" && rawMsg.Job != nil {
			jobCmd = rawMsg.Job
			break
		}
	}

	// Kiosk sends back explicit acknowledgement (or spooling progress)
	ackMsg := map[string]interface{}{
		"type": "job_ack",
		"progress": map[string]interface{}{
			"job_id":   jobCmd.JobID,
			"kiosk_id": kioskID,
			"status":   string(contracts.JobStatusSpooling),
		},
	}
	if err := wsjson.Write(ctx, wsConn, ackMsg); err != nil {
		t.Fatalf("failed to write ack over websocket: %v", err)
	}

	// Wait for cloud to process ack
	deadline := time.Now().Add(2 * time.Second)
	var ackedNS int64
	var intentStatus string
	for time.Now().Before(deadline) {
		intentStatus, ackedNS, _ = st.GetDispatchIntentStatus(context.Background(), orderID)
		if intentStatus == "ACKNOWLEDGED" && ackedNS > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if intentStatus != "ACKNOWLEDGED" || ackedNS == 0 {
		t.Fatalf("expected dispatch intent status ACKNOWLEDGED with acknowledged_at_ns > 0, got status=%s, ackedNS=%d",
			intentStatus, ackedNS)
	}
}

// 16. Test Complete Integrated Flow: Upload -> PIN -> Lookup -> Confirm -> Payment Webhook -> Dispatch -> Ack -> Printing -> Completion
func TestPaymentSafety_CompleteEndToEndSimulatedFlow(t *testing.T) {
	kioskID := "kiosk-e2e-flow"
	orderID := "ord_e2e_complete_001"
	pin := "7788"
	pricePaise := int64(2400)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	defer ts.Close()
	defer st.Close()

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kSec))
	app.hub = hub

	// 1. PIN Lookup from Kiosk
	lookupURL := fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID)
	lookupBody := fmt.Sprintf(`{"pin":%q}`, pin)
	lReq, _ := http.NewRequest(http.MethodPost, lookupURL, strings.NewReader(lookupBody))
	lReq.Header.Set("Authorization", "Bearer "+kSec)
	lReq.Header.Set("Content-Type", "application/json")
	lResp, err := http.DefaultClient.Do(lReq)
	if err != nil || lResp.StatusCode != http.StatusOK {
		t.Fatalf("pin lookup failed: err=%v, code=%d", err, lResp.StatusCode)
	}
	var lookupData map[string]interface{}
	_ = json.NewDecoder(lResp.Body).Decode(&lookupData)
	lResp.Body.Close()
	if lookupData["order_id"] != orderID {
		t.Fatalf("expected order_id %q, got %v", orderID, lookupData["order_id"])
	}
	if int64(lookupData["total_price_paise"].(float64)) != pricePaise {
		t.Fatalf("expected price %d, got %v", pricePaise, lookupData["total_price_paise"])
	}

	// 2. Kiosk Confirms / Claims Order
	claimURL := fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID)
	cReq, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+kSec)
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil || cResp.StatusCode != http.StatusOK {
		t.Fatalf("order claim failed: err=%v, code=%d", err, cResp.StatusCode)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(cResp.Body).Decode(&claimData)
	cResp.Body.Close()
	sessID := claimData["session_id"].(string)
	if sessID == "" {
		t.Fatalf("expected non-empty session_id")
	}

	// 3. Kiosk Connects via WebSocket
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// 4. Payment Captured via Signed Razorpay Webhook
	paymentID := "pay_e2e_evt_999"
	whBody, sig := generateSignedWebhookPayload(orderID, paymentID, sessID, kioskID, pricePaise, whSec)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(whBody))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil || whResp.StatusCode != http.StatusOK {
		t.Fatalf("webhook failed: err=%v, code=%d", err, whResp.StatusCode)
	}
	var whRespData map[string]interface{}
	_ = json.NewDecoder(whResp.Body).Decode(&whRespData)
	whResp.Body.Close()
	if whRespData["status"] != "dispatched" {
		t.Fatalf("expected webhook status dispatched, got %v", whRespData["status"])
	}

	// 5. Verify Dispatch Intent is recorded as SENT
	intentStatus, _, _ := st.GetDispatchIntentStatus(context.Background(), orderID)
	if intentStatus != "SENT" && intentStatus != "ACKNOWLEDGED" {
		t.Fatalf("expected dispatch intent status SENT or ACKNOWLEDGED, got %s", intentStatus)
	}

	// 6. Kiosk reads dispatched job command from WebSocket
	var jobCmd *contracts.PrintJobCommand
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	for {
		var rawMsg struct {
			Type string                     `json:"type"`
			Job  *contracts.PrintJobCommand `json:"job"`
		}
		if err := wsjson.Read(readCtx, wsConn, &rawMsg); err != nil {
			t.Fatalf("failed reading job over ws: %v", err)
		}
		if rawMsg.Type == "job" && rawMsg.Job != nil {
			jobCmd = rawMsg.Job
			break
		}
	}
	if jobCmd.JobID != "job_"+orderID {
		t.Fatalf("expected jobID job_%s, got %s", orderID, jobCmd.JobID)
	}

	// 7. Kiosk sends acknowledgement (job_ack)
	ackMsg := map[string]interface{}{
		"type": "job_ack",
		"progress": map[string]interface{}{
			"job_id":   jobCmd.JobID,
			"kiosk_id": kioskID,
			"status":   string(contracts.JobStatusSpooling),
		},
	}
	if err := wsjson.Write(ctx, wsConn, ackMsg); err != nil {
		t.Fatalf("failed to write job_ack: %v", err)
	}

	// Wait for intent to become ACKNOWLEDGED
	deadline := time.Now().Add(2 * time.Second)
	var ackedNS int64
	for time.Now().Before(deadline) {
		intentStatus, ackedNS, _ = st.GetDispatchIntentStatus(context.Background(), orderID)
		if intentStatus == "ACKNOWLEDGED" && ackedNS > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if intentStatus != "ACKNOWLEDGED" {
		t.Fatalf("expected dispatch intent ACKNOWLEDGED, got %s", intentStatus)
	}

	// 8. Duplicate Webhook arrives while in progress -> MUST be ignored idempotently
	whReqDup, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(whBody))
	whReqDup.Header.Set("Content-Type", "application/json")
	whReqDup.Header.Set("X-Razorpay-Signature", sig)
	whRespDup, err := http.DefaultClient.Do(whReqDup)
	if err != nil || whRespDup.StatusCode != http.StatusOK {
		t.Fatalf("duplicate webhook failed: err=%v, code=%d", err, whRespDup.StatusCode)
	}
	var dupData map[string]interface{}
	_ = json.NewDecoder(whRespDup.Body).Decode(&dupData)
	whRespDup.Body.Close()
	if dupData["status"] != "duplicate" && dupData["status"] != "already_processed" {
		t.Fatalf("expected duplicate webhook status duplicate or already_processed, got %v", dupData["status"])
	}

	// 9. Kiosk sends PRINTING progress
	printProg := map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         jobCmd.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusPrinting),
			"pages_printed":  1,
			"sheets_printed": 1,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	_ = wsjson.Write(ctx, wsConn, printProg)
	time.Sleep(50 * time.Millisecond)

	// 10. Kiosk sends COMPLETED progress
	compProg := map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         jobCmd.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusCompleted),
			"pages_printed":  jobCmd.TotalPages,
			"sheets_printed": jobCmd.TotalSheets,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	_ = wsjson.Write(ctx, wsConn, compProg)
	time.Sleep(50 * time.Millisecond)

	// Verify order status is COMPLETED and dispatch_intents is COMPLETED
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil || ord.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected order status COMPLETED, got %v (err=%v)", ord.Status, err)
	}
	intentStatus, _, _ = st.GetDispatchIntentStatus(context.Background(), orderID)
	if intentStatus != "COMPLETED" {
		t.Fatalf("expected dispatch intent status COMPLETED, got %s", intentStatus)
	}

	// 11. Kiosk disconnects and reconnects: completed order must NOT be re-dispatched
	_ = wsConn.Close(websocket.StatusNormalClosure, "done")
	time.Sleep(50 * time.Millisecond)

	pendingAfter, err := st.GetPendingDispatchOrders(context.Background(), kioskID)
	if err != nil || len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending orders after completion, got %d", len(pendingAfter))
	}
}

// =========================================================================
// PAYMENT PHASE 1: RAZORPAY & MOCK PROVIDER INITIATION AND WEBHOOK TESTS
// =========================================================================

func TestPaymentPhase1_InitiatePayment_ActiveSessionRequired(t *testing.T) {
	kioskID := "kiosk-p1-init"
	orderID := "ord_p1_init_001"
	pin := "2345"
	pricePaise := int64(750)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, _, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Case 1: Unauthenticated request -> 401 Unauthorized
	reqNoAuth, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	respNoAuth, err := http.DefaultClient.Do(reqNoAuth)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for missing bearer token, got %d", respNoAuth.StatusCode)
	}

	// Case 2: Authenticated request, but NO active kiosk transaction session -> 409 Conflict
	reqNoSess, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	reqNoSess.Header.Set("Authorization", "Bearer "+kSec)
	respNoSess, err := http.DefaultClient.Do(reqNoSess)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer respNoSess.Body.Close()
	if respNoSess.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for absent session, got %d", respNoSess.StatusCode)
	}

	var errBody map[string]string
	_ = json.NewDecoder(respNoSess.Body).Decode(&errBody)
	if !strings.Contains(errBody["error"], "valid active transaction session required") {
		t.Fatalf("unexpected error response body: %v", errBody)
	}
}

func TestPaymentPhase1_InitiatePayment_SuccessAndIdempotency(t *testing.T) {
	kioskID := "kiosk-p1-idem"
	orderID := "ord_p1_idem_001"
	pin := "3456"
	pricePaise := int64(600)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, _, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// 1. Claim session
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v, status %d", err, claimResp.StatusCode)
	}
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessionID := claimBody["session_id"].(string)

	// 2. First Initiate Payment Call
	initReq1, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq1.Header.Set("Authorization", "Bearer "+kSec)
	initResp1, err := http.DefaultClient.Do(initReq1)
	if err != nil {
		t.Fatalf("initiate payment 1 failed: %v", err)
	}
	defer initResp1.Body.Close()
	if initResp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for initiate payment 1, got %d", initResp1.StatusCode)
	}

	var initBody1 map[string]interface{}
	_ = json.NewDecoder(initResp1.Body).Decode(&initBody1)

	attemptID1 := initBody1["attempt_id"].(string)
	providerOrderID1 := initBody1["provider_order_id"].(string)
	qrPayload1 := initBody1["qr_payload"].(string)
	amountPaise1 := int64(initBody1["amount_paise"].(float64))
	currency1 := initBody1["currency"].(string)

	if !strings.HasPrefix(attemptID1, "att_") {
		t.Fatalf("expected attempt_id to start with att_, got %s", attemptID1)
	}
	if !strings.HasPrefix(qrPayload1, "upi://pay?") {
		t.Fatalf("expected qr_payload to be UPI URI, got %s", qrPayload1)
	}
	if amountPaise1 != pricePaise {
		t.Fatalf("expected amount %d, got %d", pricePaise, amountPaise1)
	}
	if currency1 != "INR" {
		t.Fatalf("expected currency INR, got %s", currency1)
	}

	// Verify attempt persisted in database
	attInDB, err := st.GetPaymentAttemptByProviderOrder(context.Background(), "", providerOrderID1)
	if err != nil || attInDB == nil {
		t.Fatalf("expected payment attempt in DB: %v", err)
	}
	if attInDB.SessionID != sessionID || attInDB.Status != "CREATED" {
		t.Fatalf("persisted attempt has wrong session or status: %+v", attInDB)
	}

	// 3. Second Initiate Payment Call (IDEMPOTENCY CHECK)
	initReq2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq2.Header.Set("Authorization", "Bearer "+kSec)
	initResp2, err := http.DefaultClient.Do(initReq2)
	if err != nil {
		t.Fatalf("initiate payment 2 failed: %v", err)
	}
	defer initResp2.Body.Close()
	if initResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for initiate payment 2, got %d", initResp2.StatusCode)
	}

	var initBody2 map[string]interface{}
	_ = json.NewDecoder(initResp2.Body).Decode(&initBody2)

	attemptID2 := initBody2["attempt_id"].(string)
	providerOrderID2 := initBody2["provider_order_id"].(string)
	qrPayload2 := initBody2["qr_payload"].(string)

	if attemptID1 != attemptID2 {
		t.Fatalf("idempotency violated: expected same attempt_id, got %s vs %s", attemptID1, attemptID2)
	}
	if providerOrderID1 != providerOrderID2 {
		t.Fatalf("idempotency violated: expected same provider_order_id, got %s vs %s", providerOrderID1, providerOrderID2)
	}
	if qrPayload1 != qrPayload2 {
		t.Fatalf("idempotency violated: expected same qr_payload, got %s vs %s", qrPayload1, qrPayload2)
	}
}

func TestPaymentPhase1_InitiatePayment_RetryAfterExpiry(t *testing.T) {
	kioskID := "kiosk-p1-exp"
	orderID := "ord_p1_exp_001"
	pin := "4567"
	pricePaise := int64(800)
	pinExpiry := time.Now().UTC().Add(30 * time.Minute)

	_, kSec, _, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// 1. Claim session with short expiry
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessionID := claimBody["session_id"].(string)

	// 2. Initiate payment first time
	initReq1, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq1.Header.Set("Authorization", "Bearer "+kSec)
	initResp1, _ := http.DefaultClient.Do(initReq1)
	var initBody1 map[string]interface{}
	_ = json.NewDecoder(initResp1.Body).Decode(&initBody1)
	initResp1.Body.Close()
	attemptID1 := initBody1["attempt_id"].(string)

	// 3. Simulate attempt expiry in DB
	pastNS := time.Now().UTC().Add(-5 * time.Minute).UnixNano()
	_, _ = st.DB().ExecContext(context.Background(), "UPDATE payment_attempts SET expires_at_ns = ? WHERE attempt_id = ?;", pastNS, attemptID1)

	// 4. Initiate payment again: expired attempt must be replaced by a fresh new attempt
	initReq2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq2.Header.Set("Authorization", "Bearer "+kSec)
	initResp2, _ := http.DefaultClient.Do(initReq2)
	if initResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for retry after expiry, got %d", initResp2.StatusCode)
	}
	var initBody2 map[string]interface{}
	_ = json.NewDecoder(initResp2.Body).Decode(&initBody2)
	initResp2.Body.Close()
	attemptID2 := initBody2["attempt_id"].(string)

	if attemptID1 == attemptID2 {
		t.Fatalf("expected new attempt_id after expiry, but got same: %s", attemptID1)
	}
	_ = sessionID
}

func TestPaymentPhase1_Webhook_MismatchedAmount_Rejected(t *testing.T) {
	kioskID := "kiosk-p1-amt"
	orderID := "ord_p1_amt_001"
	pin := "5678"
	pricePaise := int64(1000)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Claim and initiate
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	claimResp.Body.Close()

	initReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq.Header.Set("Authorization", "Bearer "+kSec)
	initResp, _ := http.DefaultClient.Do(initReq)
	var initBody map[string]interface{}
	_ = json.NewDecoder(initResp.Body).Decode(&initBody)
	initResp.Body.Close()
	providerOrderID := initBody["provider_order_id"].(string)

	// Webhook with tampered underpaid amount (500 paise vs expected 1000)
	tamperedPayload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_tampered_phase1",
					"order_id": providerOrderID,
					"amount":   500, // Tampered!
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(tamperedPayload)
	mac := hmac.New(sha256.New, []byte(whSec))
	mac.Write(rawBytes)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for mismatched amount, got %d", resp.StatusCode)
	}

	// Verify order status unchanged
	ord, _ := st.GetOrder(context.Background(), orderID)
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Fatalf("expected order status PENDING_PAYMENT, got %s", ord.Status)
	}
}

func TestPaymentPhase1_Webhook_InvalidCurrency_Rejected(t *testing.T) {
	kioskID := "kiosk-p1-curr"
	orderID := "ord_p1_curr_001"
	pin := "6789"
	pricePaise := int64(450)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, kSec, whSec, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Claim and initiate
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	claimResp.Body.Close()

	initReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq.Header.Set("Authorization", "Bearer "+kSec)
	initResp, _ := http.DefaultClient.Do(initReq)
	var initBody map[string]interface{}
	_ = json.NewDecoder(initResp.Body).Decode(&initBody)
	initResp.Body.Close()
	providerOrderID := initBody["provider_order_id"].(string)

	// Webhook with invalid currency USD
	badCurrPayload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_bad_curr_001",
					"order_id": providerOrderID,
					"amount":   pricePaise,
					"currency": "USD", // Invalid!
					"status":   "captured",
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(badCurrPayload)
	mac := hmac.New(sha256.New, []byte(whSec))
	mac.Write(rawBytes)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for non-INR currency, got %d", resp.StatusCode)
	}
}

func TestPaymentPhase1_Webhook_InvalidSignature_Rejected(t *testing.T) {
	kioskID := "kiosk-p1-sig"
	orderID := "ord_p1_sig_001"
	pin := "7890"
	pricePaise := int64(300)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, _, _, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	payload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_sig_fail_123",
					"order_id": "order_non_existent",
					"amount":   pricePaise,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", "forged_invalid_signature_hex_1234567890abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for forged signature, got %d", resp.StatusCode)
	}
}

func TestPaymentPhase1_Webhook_UnassociatedPayment_Rejected(t *testing.T) {
	kioskID := "kiosk-p1-unassoc"
	orderID := "ord_p1_unassoc_001"
	pin := "8901"
	pricePaise := int64(500)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	_, _, whSec, ts, _ := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)

	// Webhook for a totally unknown order/attempt
	payload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_unassoc_123",
					"order_id": "order_unknown_99999",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
					"notes": map[string]interface{}{
						"order_id": "order_unknown_99999",
					},
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(whSec))
	mac.Write(rawBytes)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for unassociated payment, got %d", resp.StatusCode)
	}
}

func TestPaymentPhase1_Webhook_FullLifecycleAndDispatch(t *testing.T) {
	kioskID := "kiosk-p1-e2e"
	orderID := "ord_p1_e2e_001"
	pin := "9012"
	pricePaise := int64(900)
	pinExpiry := time.Now().UTC().Add(15 * time.Minute)

	app, kSec, whSec, ts, st := setupSafetyTestApp(t, kioskID, orderID, pin, contracts.JobStatusPendingPayment, pricePaise, pinExpiry)
	app.hub = ws.NewHub(st)
	app.hub.SetKioskSecret([]byte(kSec))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Connect kiosk WebSocket
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()
	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + kSec},
		},
	}
	wsConn, _, err := websocket.Dial(ctx, u.String(), dialOpts)
	if err != nil {
		t.Fatalf("kiosk websocket dial failed: %v", err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")

	// Wait for kiosk to register with hub
	time.Sleep(50 * time.Millisecond)
	if !app.hub.IsConnected(kioskID) {
		t.Fatalf("kiosk %s is not connected to hub", kioskID)
	}

	// 2. Claim session
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v", err)
	}
	var claimBody map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimBody)
	claimResp.Body.Close()
	sessionID := claimBody["session_id"].(string)

	// 3. Initiate payment
	initReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
	initReq.Header.Set("Authorization", "Bearer "+kSec)
	initResp, err := http.DefaultClient.Do(initReq)
	if err != nil || initResp.StatusCode != http.StatusOK {
		t.Fatalf("initiate payment failed: %v", err)
	}
	var initBody map[string]interface{}
	_ = json.NewDecoder(initResp.Body).Decode(&initBody)
	initResp.Body.Close()

	attemptID := initBody["attempt_id"].(string)
	providerOrderID := initBody["provider_order_id"].(string)

	// 4. Send payment webhook
	paymentID := "pay_p1_e2e_captured_123"
	payload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       paymentID,
					"order_id": providerOrderID,
					"amount":   pricePaise,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(whSec))
	mac.Write(rawBytes)
	sig := hex.EncodeToString(mac.Sum(nil))

	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawBytes))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", sig)
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatalf("webhook failed: %v", err)
	}
	defer whResp.Body.Close()

	if whResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from webhook, got %d", whResp.StatusCode)
	}
	var whResult map[string]string
	_ = json.NewDecoder(whResp.Body).Decode(&whResult)
	if whResult["status"] != "dispatched" {
		t.Fatalf("expected status 'dispatched', got %q", whResult["status"])
	}

	// 5. Verify Kiosk receives the PrintJobCommand over WebSocket
	var jobCmd *contracts.PrintJobCommand
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	for {
		var rawMsg struct {
			Type string                     `json:"type"`
			Job  *contracts.PrintJobCommand `json:"job"`
		}
		if err := wsjson.Read(readCtx, wsConn, &rawMsg); err != nil {
			t.Fatalf("failed reading job over ws: %v", err)
		}
		if rawMsg.Type == "job" && rawMsg.Job != nil {
			jobCmd = rawMsg.Job
			break
		}
	}
	if jobCmd.JobID != "job_"+orderID {
		t.Fatalf("expected job_id %s, got %s", "job_"+orderID, jobCmd.JobID)
	}

	// 6. Verify attempt marked PAID in database
	attAfter, err := st.GetPaymentAttemptByProviderOrder(context.Background(), "", providerOrderID)
	if err != nil || attAfter == nil {
		t.Fatalf("failed to query attempt in DB: %v", err)
	}
	if attAfter.Status != "PAID" {
		t.Fatalf("expected attempt status PAID, got %s", attAfter.Status)
	}
	if attAfter.ProviderPaymentID != paymentID {
		t.Fatalf("expected provider_payment_id %s, got %s", paymentID, attAfter.ProviderPaymentID)
	}

	_ = attemptID
	_ = sessionID
}

