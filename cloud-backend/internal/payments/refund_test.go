package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRazorpayRefund_UsesDurableIdempotencyKey(t *testing.T) {
	var called int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/payments/pay_captured_1/refund" {
			t.Errorf("wrong path or method %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Refund-Idempotency") != "xeroxgo_refund_ord_123456" {
			t.Errorf("missing durable refund idempotency header")
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "test-key" || pass != "test-secret" { t.Error("bad API credentials") }
		var body struct {
			Amount int64 `json:"amount"`
			Speed string `json:"speed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil { t.Error(err) }
		if body.Amount != 1200 || body.Speed != "normal" { t.Errorf("bad refund body: %+v", body) }
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"rfnd_test_1","payment_id":"pay_captured_1","amount":1200,"status":"processed"}`))
	}))
	defer srv.Close()
	p, err := NewRazorpayProvider(RazorpayConfig{KeyID:"test-key", KeySecret:"test-secret", BaseURL:srv.URL})
	if err != nil { t.Fatal(err) }
	for i := 0; i < 2; i++ {
		got, err := p.Refund(context.Background(), "pay_captured_1", 1200, "xeroxgo_refund_ord_123456")
		if err != nil || got.Status != "processed" { t.Fatalf("refund: %+v %v", got, err) }
	}
	if called != 2 { t.Fatalf("expected two identical retry calls, got %d", called) }
}

func TestMockRefund_ExactlyOnceForSameKey(t *testing.T) {
	p := NewMockProvider("test")
	for i:=0; i<3; i++ {
		got, err := p.Refund(context.Background(), "pay_mock_1", 1500, "xeroxgo_refund_1")
		if err != nil || got.Status != "processed" { t.Fatalf("mock: %+v %v", got, err) }
	}
	if p.RefundedCount != 1 { t.Fatalf("mock duplicate refunds: %d", p.RefundedCount) }
	if _, err := p.Refund(context.Background(), "pay_other", 1500, "xeroxgo_refund_1"); err == nil {
		t.Fatal("reusing refund key for another payment must fail")
	}
}
