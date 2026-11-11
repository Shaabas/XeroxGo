package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func newTestPINServer(t *testing.T, kioskSecret string) (*appServer, *store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pin_test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &serverConfig{
		kioskSecret: []byte(kioskSecret),
		jobsDir:     t.TempDir(),
	}
	app := &appServer{
		cfg:        cfg,
		db:         db,
		pinLimiter: NewPINRateLimiter(db, 5, 2*time.Minute, 2*time.Minute),
	}
	return app, db, dbPath
}

func createTestOrder(t *testing.T, db *store.Store, orderID, jobID, kioskID, pin string, status contracts.JobStatus, expiry time.Time) store.Order {
	t.Helper()
	now := time.Now().UTC()
	ord := store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    pin,
			DownloadURL:    "https://storage.internal/secret-pdf-token",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         2,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:       status,
		PricePaise:   600, // 6.00 INR
		ColorMode:    "BW",
		CreatedAt:    now,
		UpdatedAt:    now,
		PINExpiresAt: expiry,
	}
	if err := db.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to create test order: %v", err)
	}
	return ord
}

// 1. Verify 4-digit numeric PIN format and randomness
func TestPIN_FourDigitFormatAndCryptoRand(t *testing.T) {
	rgx := regexp.MustCompile(`^\d{4}$`)
	seen := make(map[string]bool)

	for i := 0; i < 500; i++ {
		code, err := randomDisplayCode()
		if err != nil {
			t.Fatalf("randomDisplayCode failed: %v", err)
		}
		if len(code) != 4 {
			t.Fatalf("expected 4 characters, got %d (%q)", len(code), code)
		}
		if !rgx.MatchString(code) {
			t.Fatalf("expected 4 numeric digits, got %q", code)
		}
		seen[code] = true
	}

	if len(seen) < 450 {
		t.Fatalf("insufficient entropy in randomDisplayCode: saw only %d unique codes out of 500", len(seen))
	}
}

// 2. Verify collision-free PIN generation and atomic reservation retry
func TestPIN_AtomicUniquenessAndCollisionRetry(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")
	ctx := context.Background()

	// Fill a known PIN on kiosk-01
	createTestOrder(t, db, "ord_coll_1", "job_coll_1", "kiosk-01", "1234", contracts.JobStatusPendingPayment, time.Now().Add(15*time.Minute))

	// Create another order using createOrderWithPINRetry: must automatically avoid "1234" and succeed
	newOrd := store.Order{
		OrderID: "ord_coll_2",
		Command: contracts.PrintJobCommand{
			JobID:          "job_coll_2",
			KioskID:        "kiosk-01",
			DownloadURL:    "https://storage.internal/test.pdf",
			SHA256Checksum: "abc",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   200,
		ColorMode:    "BW",
		CreatedAt:    time.Now().UTC(),
		PINExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}

	if err := app.createOrderWithPINRetry(ctx, &newOrd, 25); err != nil {
		t.Fatalf("createOrderWithPINRetry failed: %v", err)
	}
	if newOrd.Command.DisplayCode == "1234" {
		t.Fatalf("createOrderWithPINRetry collided with existing active PIN")
	}
	if len(newOrd.Command.DisplayCode) != 4 {
		t.Fatalf("expected 4-digit PIN, got %s", newOrd.Command.DisplayCode)
	}
}

// 3. Test Strict Authentication: require Authorization: Bearer, disallow query params
func TestPINLookup_StrictBearerAuthentication(t *testing.T) {
	app, db, _ := newTestPINServer(t, "super-secret-kiosk-key")
	createTestOrder(t, db, "ord_auth_1", "job_auth_1", "kiosk-auth", "4567", contracts.JobStatusPendingPayment, time.Now().Add(15*time.Minute))

	body := `{"pin":"4567"}`

	// Case 1: Missing auth header -> 401
	req1 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-auth/orders/lookup", strings.NewReader(body))
	w1 := httptest.NewRecorder()
	app.handlePINLookup(w1, req1)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for missing secret, got %d", w1.Code)
	}

	// Case 2: Query param authentication is rejected -> 401
	req2 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-auth/orders/lookup?token=super-secret-kiosk-key", strings.NewReader(body))
	w2 := httptest.NewRecorder()
	app.handlePINLookup(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("query-param authentication MUST be rejected with 401, got %d", w2.Code)
	}

	// Case 3: Invalid bearer token -> 401
	req3 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-auth/orders/lookup", strings.NewReader(body))
	req3.Header.Set("Authorization", "Bearer invalid-token")
	w3 := httptest.NewRecorder()
	app.handlePINLookup(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid token, got %d", w3.Code)
	}

	// Case 4: Valid Authorization: Bearer <secret> -> 200
	req4 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-auth/orders/lookup", strings.NewReader(body))
	req4.Header.Set("Authorization", "Bearer super-secret-kiosk-key")
	w4 := httptest.NewRecorder()
	app.handlePINLookup(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid bearer token, got %d (%s)", w4.Code, w4.Body.String())
	}
}

// 4. Test Success Payload and Privacy (NO PDF URL, NO Checksum)
func TestPINLookup_SuccessPayloadAndPrivacy(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")
	expiry := time.Now().Add(12 * time.Minute)
	ord := createTestOrder(t, db, "ord_safe_1", "job_safe_1", "kiosk-safe", "1122", contracts.JobStatusPendingPayment, expiry)

	body := `{"pin":"1122"}`
	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-safe/orders/lookup", strings.NewReader(body))
	w := httptest.NewRecorder()
	app.handlePINLookup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	rawJSON := w.Body.String()
	if strings.Contains(rawJSON, "download_url") || strings.Contains(rawJSON, "secret-pdf-token") {
		t.Fatalf("LEAK DETECTED: download_url present in response: %s", rawJSON)
	}
	if strings.Contains(rawJSON, "sha256_checksum") || strings.Contains(rawJSON, ord.Command.SHA256Checksum) {
		t.Fatalf("LEAK DETECTED: sha256_checksum present in response: %s", rawJSON)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if res["order_id"] != "ord_safe_1" {
		t.Errorf("order_id = %v, want ord_safe_1", res["order_id"])
	}
	if res["kiosk_id"] != "kiosk-safe" {
		t.Errorf("kiosk_id = %v, want kiosk-safe", res["kiosk_id"])
	}
	if res["status"] != "PENDING_PAYMENT" {
		t.Errorf("status = %v, want PENDING_PAYMENT", res["status"])
	}
	if int64(res["total_price_paise"].(float64)) != 600 {
		t.Errorf("total_price_paise = %v, want 600", res["total_price_paise"])
	}
	if int(res["total_pages"].(float64)) != 4 {
		t.Errorf("total_pages = %v, want 4", res["total_pages"])
	}
	if int(res["total_sheets"].(float64)) != 2 {
		t.Errorf("total_sheets = %v, want 2", res["total_sheets"])
	}
	if int(res["copies"].(float64)) != 2 {
		t.Errorf("copies = %v, want 2", res["copies"])
	}
}

// 5. Test Uniform Failure: unknown, expired, completed, or cross-kiosk PINs ALL return identical 404
//    and NEVER leak order_id or whether the order existed
func TestPINLookup_UniformFailureNoExistenceLeak(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")

	// 1. Expired order
	createTestOrder(t, db, "ord_exp_leak", "job_exp_leak", "kiosk-probe", "1001", contracts.JobStatusPendingPayment, time.Now().Add(-5*time.Minute))

	// 2. Completed order
	createTestOrder(t, db, "ord_comp_leak", "job_comp_leak", "kiosk-probe", "1002", contracts.JobStatusCompleted, time.Now().Add(15*time.Minute))

	// 3. Failed order
	createTestOrder(t, db, "ord_fail_leak", "job_fail_leak", "kiosk-probe", "1003", contracts.JobStatusFailedRefundable, time.Now().Add(15*time.Minute))

	// 4. Order on other kiosk
	createTestOrder(t, db, "ord_other_leak", "job_other_leak", "kiosk-other", "1004", contracts.JobStatusPendingPayment, time.Now().Add(15*time.Minute))

	cases := []struct {
		name string
		pin  string
	}{
		{"UnknownPIN", "9999"},
		{"ExpiredPIN", "1001"},
		{"CompletedPIN", "1002"},
		{"FailedPIN", "1003"},
		{"OtherKioskPIN", "1004"},
	}

	for _, tc := range cases {
		body := fmt.Sprintf(`{"pin":%q}`, tc.pin)
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-probe/orders/lookup", strings.NewReader(body))
		w := httptest.NewRecorder()
		app.handlePINLookup(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("[%s] expected uniform 404 Not Found, got %d", tc.name, w.Code)
		}

		raw := w.Body.String()
		if strings.Contains(raw, "ord_") || strings.Contains(raw, "job_") {
			t.Errorf("[%s] ORACLE LEAK: response revealed internal order_id: %s", tc.name, raw)
		}

		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["error"] != "invalid or expired PIN" {
			t.Errorf("[%s] expected 'invalid or expired PIN', got %v", tc.name, resp["error"])
		}
	}
}

// 6. Test Boundary Timestamp Expiry
func TestPINLookup_BoundaryTimestampExpiry(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")

	// Set order expiry to exactly 50 milliseconds in the future
	expiryTime := time.Now().UTC().Add(50 * time.Millisecond)
	createTestOrder(t, db, "ord_boundary", "job_boundary", "kiosk-bound", "6543", contracts.JobStatusPendingPayment, expiryTime)

	// Immediate lookup before expiry -> succeeds
	body := `{"pin":"6543"}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-bound/orders/lookup", strings.NewReader(body))
	w1 := httptest.NewRecorder()
	app.handlePINLookup(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 before boundary expiry, got %d: %s", w1.Code, w1.Body.String())
	}

	// Wait past boundary
	time.Sleep(75 * time.Millisecond)

	// Lookup after boundary -> rejected uniformly
	req2 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-bound/orders/lookup", strings.NewReader(body))
	w2 := httptest.NewRecorder()
	app.handlePINLookup(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after boundary expiry, got %d", w2.Code)
	}
}

// 7. Test Durable Rate Limiting Survives Server Restart
func TestPINLookup_DurableRateLimitingSurvivesServerRestart(t *testing.T) {
	app1, db1, dbPath := newTestPINServer(t, "")

	// Trigger 5 failed attempts on Server 1
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"pin":"000%d"}`, i)
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-restart/orders/lookup", strings.NewReader(body))
		w := httptest.NewRecorder()
		app1.handlePINLookup(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("attempt %d on server 1 failed with %d", i, w.Code)
		}
	}

	// Server 1 should now return 429
	req1 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-restart/orders/lookup", strings.NewReader(`{"pin":"0006"}`))
	w1 := httptest.NewRecorder()
	app1.handlePINLookup(w1, req1)
	if w1.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on server 1, got %d", w1.Code)
	}

	// Close Server 1 and simulate Server 2 starting with the same database
	_ = db1.Close()

	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer db2.Close()

	app2 := &appServer{
		cfg:        &serverConfig{jobsDir: t.TempDir()},
		db:         db2,
		pinLimiter: NewPINRateLimiter(db2, 5, 2*time.Minute, 2*time.Minute),
	}

	// Server 2 MUST immediately reject with 429 Too Many Requests (durable persistence!)
	req2 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-restart/orders/lookup", strings.NewReader(`{"pin":"0007"}`))
	w2 := httptest.NewRecorder()
	app2.handlePINLookup(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("DURABILITY VIOLATION: server restart bypassed rate limiting! Expected 429, got %d", w2.Code)
	}
	retryHeader := w2.Header().Get("Retry-After")
	if retryHeader == "" {
		t.Fatalf("expected Retry-After header on restarted server 429 response")
	}
}

// 8. Read-Only Invariant: Validating PIN never changes status or dispatches jobs
func TestPINLookup_ReadOnlyInvariant(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")
	createTestOrder(t, db, "ord_ro_1", "job_ro_1", "kiosk-ro", "3344", contracts.JobStatusPendingPayment, time.Now().Add(15*time.Minute))

	body := `{"pin":"3344"}`
	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-ro/orders/lookup", strings.NewReader(body))
	w := httptest.NewRecorder()
	app.handlePINLookup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	ordAfter, err := db.GetOrder(context.Background(), "ord_ro_1")
	if err != nil || ordAfter == nil {
		t.Fatalf("failed to retrieve order after lookup: %v", err)
	}
	if ordAfter.Status != contracts.JobStatusPendingPayment {
		t.Fatalf("INVARIANT VIOLATION: order status modified to %s after PIN lookup", ordAfter.Status)
	}
}

// 9. Concurrency Safety: 30 concurrent lookups
func TestPINLookup_ConcurrentLookups(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")
	createTestOrder(t, db, "ord_conc_1", "job_conc_1", "kiosk-conc", "6543", contracts.JobStatusPendingPayment, time.Now().Add(15*time.Minute))

	var wg sync.WaitGroup
	errCh := make(chan error, 30)

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reqBody := []byte(`{"pin":"6543"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-conc/orders/lookup", bytes.NewReader(reqBody))
			w := httptest.NewRecorder()
			app.handlePINLookup(w, req)

			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("unexpected status code %d: %s", w.Code, w.Body.String())
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent lookup failed: %v", err)
	}
}

// 10. Test handleOrderStatus Authoritative Expiry
func TestOrderStatus_AuthoritativeExpiry(t *testing.T) {
	app, db, _ := newTestPINServer(t, "")

	// 1. Order unexpired
	expiry := time.Now().UTC().Add(10 * time.Minute)
	createTestOrder(t, db, "ord_stat_active", "job_stat_active", "kiosk-st", "1122", contracts.JobStatusPendingPayment, expiry)

	req1 := httptest.NewRequest(http.MethodGet, "/api/orders/ord_stat_active/status", nil)
	w1 := httptest.NewRecorder()
	app.handleOrderStatus(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w1.Code)
	}
	var res1 map[string]interface{}
	_ = json.Unmarshal(w1.Body.Bytes(), &res1)
	if res1["status"] != "PENDING_PAYMENT" {
		t.Errorf("status = %v, want PENDING_PAYMENT", res1["status"])
	}
	if res1["pin_expires_at"] == "" {
		t.Errorf("expected pin_expires_at in response")
	}
	secRemaining := int64(res1["seconds_remaining"].(float64))
	if secRemaining < 590 || secRemaining > 605 {
		t.Errorf("unexpected seconds_remaining: %d", secRemaining)
	}

	// 2. Order expired
	pastExpiry := time.Now().UTC().Add(-2 * time.Minute)
	createTestOrder(t, db, "ord_stat_expired", "job_stat_expired", "kiosk-st", "3344", contracts.JobStatusPendingPayment, pastExpiry)

	req2 := httptest.NewRequest(http.MethodGet, "/api/orders/ord_stat_expired/status", nil)
	w2 := httptest.NewRecorder()
	app.handleOrderStatus(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var res2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &res2)
	if res2["status"] != "EXPIRED" {
		t.Errorf("expected status EXPIRED for expired order, got %v", res2["status"])
	}
	if int64(res2["seconds_remaining"].(float64)) != 0 {
		t.Errorf("expected seconds_remaining 0, got %v", res2["seconds_remaining"])
	}
}
