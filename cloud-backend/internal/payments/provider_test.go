package payments

import (
	"context"
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

