package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMockProvider_Lifecycle(t *testing.T) {
	mockSecret := "whsec_mock_test_secret"
	p := NewMockProvider(mockSecret)
	if p.Name() != "mock" {
		t.Fatalf("expected name mock, got %s", p.Name())
	}

	ctx := context.Background()
	req := CreatePaymentRequest{
		AttemptID:   "att_test_001",
		OrderID:     "ord_test_001",
		KioskID:     "kiosk-01",
		SessionID:   "sess_test_001",
		AmountPaise: 2400,
		Currency:    "INR",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}

	res, err := p.CreatePayment(ctx, req)
	if err != nil {
		t.Fatalf("CreatePayment failed: %v", err)
	}

	if res.Provider != "mock" {
		t.Errorf("expected provider mock, got %s", res.Provider)
	}
	if !strings.HasPrefix(res.ProviderOrderID, "mock_qr_") {
		t.Errorf("expected ProviderOrderID prefix mock_qr_, got %s", res.ProviderOrderID)
	}
	if !strings.HasPrefix(res.QRPayload, "upi://pay?") {
		t.Errorf("expected QRPayload to start with upi://pay?, got %s", res.QRPayload)
	}
	if !strings.Contains(res.QRPayload, "am=24.00") {
		t.Errorf("expected QRPayload to contain am=24.00, got %s", res.QRPayload)
	}

	// Signature verification
	rawPayload := []byte(`{"event":"payment.captured"}`)
	validSig := testHMACSHA256(rawPayload, mockSecret)
	if !p.VerifyWebhookSignature(rawPayload, validSig) {
		t.Errorf("expected valid signature to pass verification")
	}
	if p.VerifyWebhookSignature(rawPayload, "invalid_sig") {
		t.Errorf("expected invalid signature to fail verification")
	}

	// Simulation of provider failure
	p.SetFailCreate(true, "insufficient provider balance")
	_, err = p.CreatePayment(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "insufficient provider balance") {
		t.Fatalf("expected simulated error, got %v", err)
	}
}

func TestMockProvider_InputValidation(t *testing.T) {
	p := NewMockProvider("secret")
	ctx := context.Background()

	badCases := []struct {
		name string
		req  CreatePaymentRequest
	}{
		{"zero amount", CreatePaymentRequest{OrderID: "o1", SessionID: "s1", AmountPaise: 0}},
		{"negative amount", CreatePaymentRequest{OrderID: "o1", SessionID: "s1", AmountPaise: -100}},
		{"empty order id", CreatePaymentRequest{OrderID: "", SessionID: "s1", AmountPaise: 500}},
		{"empty session id", CreatePaymentRequest{OrderID: "o1", SessionID: "", AmountPaise: 500}},
	}

	for _, bc := range badCases {
		t.Run(bc.name, func(t *testing.T) {
			_, err := p.CreatePayment(ctx, bc.req)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", bc.name)
			}
		})
	}
}

func TestRazorpayProvider_DynamicQRSuccess(t *testing.T) {
	keyID := "rzp_test_key123"
	keySecret := "secret123"
	whSecret := "whsec_test_abc"

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != keyID || p != keySecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/v1/payments/qr_codes" {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":        "qr_rzp_mock_123",
				"entity":    "qr_code",
				"status":    "active",
				"payload":   "upi://pay?pa=rzp@upi&pn=XeroxGo&am=15.00&tr=qr_rzp_mock_123",
				"image_url": "https://api.razorpay.com/v1/payments/qr_codes/qr_rzp_mock_123/image",
				"close_by":  time.Now().Add(15 * time.Minute).Unix(),
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	provider, err := NewRazorpayProvider(RazorpayConfig{
		KeyID:         keyID,
		KeySecret:     keySecret,
		WebhookSecret: whSecret,
		BaseURL:       mockServer.URL,
	})
	if err != nil {
		t.Fatalf("NewRazorpayProvider failed: %v", err)
	}

	res, err := provider.CreatePayment(context.Background(), CreatePaymentRequest{
		AttemptID:   "att_rzp_01",
		OrderID:     "ord_rzp_01",
		KioskID:     "kiosk-rzp-01",
		SessionID:   "sess_rzp_01",
		AmountPaise: 1500,
		Currency:    "INR",
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreatePayment failed: %v", err)
	}

	if res.Provider != "razorpay" {
		t.Errorf("expected provider razorpay, got %s", res.Provider)
	}
	if res.ProviderOrderID != "qr_rzp_mock_123" {
		t.Errorf("expected qr_rzp_mock_123, got %s", res.ProviderOrderID)
	}
	if !strings.HasPrefix(res.QRPayload, "upi://pay?") {
		t.Errorf("expected QRPayload to start with upi://pay?, got %s", res.QRPayload)
	}
	if !provider.VerifyWebhookSignature([]byte("hello"), testHMACSHA256([]byte("hello"), whSecret)) {
		t.Errorf("expected webhook signature verification to pass")
	}
}

func TestRazorpayProvider_OrderFallback(t *testing.T) {
	keyID := "rzp_test_key123"
	keySecret := "secret123"

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/payments/qr_codes" {
			// Simulate QR codes feature not activated on merchant account
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":        "BAD_REQUEST_ERROR",
					"description": "QR codes feature is not enabled for your account",
				},
			})
			return
		}
		if r.URL.Path == "/v1/orders" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "order_rzp_fallback_456",
				"amount":   1000,
				"currency": "INR",
				"status":   "created",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	provider, err := NewRazorpayProvider(RazorpayConfig{
		KeyID:      keyID,
		KeySecret:  keySecret,
		BaseURL:    mockServer.URL,
	})
	if err != nil {
		t.Fatalf("NewRazorpayProvider failed: %v", err)
	}

	res, err := provider.CreatePayment(context.Background(), CreatePaymentRequest{
		AttemptID:   "att_fb_01",
		OrderID:     "ord_fb_01",
		KioskID:     "kiosk-fb-01",
		SessionID:   "sess_fb_01",
		AmountPaise: 1000,
		Currency:    "INR",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreatePayment fallback failed: %v", err)
	}

	if res.ProviderOrderID != "order_rzp_fallback_456" {
		t.Errorf("expected order_rzp_fallback_456, got %s", res.ProviderOrderID)
	}
	if !strings.HasPrefix(res.QRPayload, "upi://pay?") {
		t.Errorf("expected QRPayload to start with upi://pay?, got %s", res.QRPayload)
	}
}

func TestRazorpayProvider_RejectsMissingCredentials(t *testing.T) {
	_, err := NewRazorpayProvider(RazorpayConfig{
		KeyID:     "",
		KeySecret: "secret",
	})
	if err == nil {
		t.Errorf("expected error for empty KeyID, got nil")
	}

	_, err = NewRazorpayProvider(RazorpayConfig{
		KeyID:     "key",
		KeySecret: "",
	})
	if err == nil {
		t.Errorf("expected error for empty KeySecret, got nil")
	}
}

