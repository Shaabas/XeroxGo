package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPPINClient_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/k/kiosk-test/orders/lookup" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["pin"] != "1234" {
			http.Error(w, "invalid pin", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(OrderDetails{
			OrderID:          "ord_test_001",
			KioskID:          "kiosk-test",
			Status:           "PENDING_PAYMENT",
			TotalPricePaise:  2500,
			TotalPages:       10,
			TotalSheets:      5,
			Copies:           1,
			Duplex:           "long_edge",
			ColorMode:        "mono",
			PINExpiresAt:     time.Now().Add(10 * time.Minute).Format(time.RFC3339),
			SecondsRemaining: 600,
		})
	}))
	defer ts.Close()

	client, err := NewHTTPPINClient(ts.URL, "kiosk-test", "secret-123", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	details, err := client.LookupOrderByPIN(context.Background(), "1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if details.OrderID != "ord_test_001" {
		t.Errorf("expected orderID ord_test_001, got %s", details.OrderID)
	}
	if details.TotalPricePaise != 2500 {
		t.Errorf("expected price 2500, got %d", details.TotalPricePaise)
	}
	if details.TotalSheets != 5 {
		t.Errorf("expected total sheets 5, got %d", details.TotalSheets)
	}
}

func TestHTTPPINClient_Errors(t *testing.T) {
	var statusCode int
	var respHeaderKey, respHeaderVal string
	var respBody string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respHeaderKey != "" {
			w.Header().Set(respHeaderKey, respHeaderVal)
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(respBody))
	}))
	defer ts.Close()

	client, err := NewHTTPPINClient(ts.URL, "kiosk-err", "sec", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// 1. 404 Not Found -> ErrInvalidOrExpiredPIN
	statusCode = http.StatusNotFound
	respBody = `{"error": "invalid or expired PIN"}`
	_, err = client.LookupOrderByPIN(context.Background(), "0000")
	if !errors.Is(err, ErrInvalidOrExpiredPIN) {
		t.Errorf("expected ErrInvalidOrExpiredPIN, got %v", err)
	}

	// 2. 401 Unauthorized -> ErrUnauthorized
	statusCode = http.StatusUnauthorized
	respBody = `{"error": "unauthorized"}`
	_, err = client.LookupOrderByPIN(context.Background(), "1234")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}

	// 3. 429 Rate Limit
	statusCode = http.StatusTooManyRequests
	respHeaderKey = "Retry-After"
	respHeaderVal = "45"
	respBody = `{"error": "too many attempts", "retry_after_seconds": 45}`
	_, err = client.LookupOrderByPIN(context.Background(), "1234")
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected RateLimitError, got %v", err)
	}
	if rle.RetryAfterSeconds != 45 {
		t.Errorf("expected 45 seconds retry-after, got %d", rle.RetryAfterSeconds)
	}
}

func TestHTTPPINClient_NetworkTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	client, err := NewHTTPPINClient(ts.URL, "kiosk-timeout", "", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = client.LookupOrderByPIN(ctx, "1111")
	var netErr *NetworkError
	if !errors.As(err, &netErr) {
		t.Errorf("expected NetworkError on timeout, got %v", err)
	}
}

func TestHTTPPINClient_ClaimAndSessionLifecycle(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sec-claim-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch {
		case r.URL.Path == "/api/k/kiosk-clm/orders/ord_test_01/claim":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(SessionDetails{
				SessionID:        "sess_test_101",
				OrderID:          "ord_test_01",
				KioskID:          "kiosk-clm",
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  1500,
				ExpiresAt:        time.Now().Add(5 * time.Minute).Format(time.RFC3339),
				SecondsRemaining: 300,
			})
		case r.URL.Path == "/api/k/kiosk-clm/orders/ord_conflict/claim":
			http.Error(w, `{"error":"order is already claimed"}`, http.StatusConflict)
		case r.URL.Path == "/api/k/kiosk-clm/orders/ord_no_paper/claim":
			http.Error(w, `{"error":"insufficient paper in kiosk tray","code":"INSUFFICIENT_PAPER"}`, http.StatusConflict)
		case r.URL.Path == "/api/k/kiosk-clm/sessions/sess_test_101/cancel":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/k/kiosk-clm/sessions/sess_test_101":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(SessionDetails{
				SessionID:        "sess_test_101",
				OrderID:          "ord_test_01",
				KioskID:          "kiosk-clm",
				Status:           "PAYMENT_PENDING",
				TotalPricePaise:  1500,
				SecondsRemaining: 250,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := NewHTTPPINClient(ts.URL, "kiosk-clm", "sec-claim-1", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// 1. Claim success
	sess, err := client.ClaimOrder(context.Background(), "ord_test_01")
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if sess.SessionID != "sess_test_101" {
		t.Errorf("session ID = %s, want sess_test_101", sess.SessionID)
	}
	if sess.TotalPricePaise != 1500 {
		t.Errorf("price = %d, want 1500", sess.TotalPricePaise)
	}

	// 2. Conflict
	_, err = client.ClaimOrder(context.Background(), "ord_conflict")
	if !errors.Is(err, ErrOrderAlreadyClaimed) {
		t.Errorf("expected ErrOrderAlreadyClaimed, got %v", err)
	}

	// 2b. Insufficient Paper Conflict
	_, err = client.ClaimOrder(context.Background(), "ord_no_paper")
	if !errors.Is(err, ErrInsufficientPaper) {
		t.Errorf("expected ErrInsufficientPaper, got %v", err)
	}

	// 3. Get session
	retrieved, err := client.GetSession(context.Background(), "sess_test_101")
	if err != nil {
		t.Fatalf("get session failed: %v", err)
	}
	if retrieved.SessionID != "sess_test_101" {
		t.Errorf("got session ID = %s, want sess_test_101", retrieved.SessionID)
	}

	// 4. Cancel session
	if err := client.CancelSession(context.Background(), "sess_test_101"); err != nil {
		t.Fatalf("cancel session failed: %v", err)
	}
}

func TestHTTPPINClient_InitiatePayment(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sec-init-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		switch r.URL.Path {
		case "/api/k/kiosk-init/sessions/sess_ok/initiate-payment":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(PaymentAttemptDetails{
				AttemptID:   "att_test_123",
				SessionID:   "sess_ok",
				OrderID:     "ord_test_01",
				KioskID:     "kiosk-init",
				Provider:    "mock",
				AmountPaise: 1500,
				Status:      "INITIATED",
				QRPayload:   "https://demo.pay/pay/tok_123",
				DemoToken:   "tok_123",
				ExpiresAt:   time.Now().Add(5 * time.Minute).Format(time.RFC3339),
			})
		case "/api/k/kiosk-init/sessions/sess_insufficient_paper/initiate-payment":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "insufficient paper in kiosk tray",
				"code":  "INSUFFICIENT_PAPER",
			})
		case "/api/k/kiosk-init/sessions/sess_printer_offline/initiate-payment":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "printer offline",
				"code":  "PRINTER_OFFLINE",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := NewHTTPPINClient(ts.URL, "kiosk-init", "sec-init-1", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// 1. Success
	attempt, err := client.InitiatePayment(context.Background(), "sess_ok")
	if err != nil {
		t.Fatalf("initiate payment failed: %v", err)
	}
	if attempt.AttemptID != "att_test_123" {
		t.Errorf("attempt ID = %s, want att_test_123", attempt.AttemptID)
	}
	if attempt.QRPayload != "https://demo.pay/pay/tok_123" {
		t.Errorf("qr payload = %s, want https://demo.pay/pay/tok_123", attempt.QRPayload)
	}
	if attempt.AmountPaise != 1500 {
		t.Errorf("amount paise = %d, want 1500", attempt.AmountPaise)
	}

	// 2. Insufficient paper conflict
	_, err = client.InitiatePayment(context.Background(), "sess_insufficient_paper")
	if !errors.Is(err, ErrInsufficientPaper) {
		t.Errorf("expected ErrInsufficientPaper, got %v", err)
	}

	// 3. Printer offline conflict
	_, err = client.InitiatePayment(context.Background(), "sess_printer_offline")
	if !errors.Is(err, ErrPrinterOffline) {
		t.Errorf("expected ErrPrinterOffline, got %v", err)
	}
}

