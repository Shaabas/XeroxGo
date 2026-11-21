package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MockProvider provides a simulated payment provider for local development, testing,
// and environments without live Razorpay credentials.
type MockProvider struct {
	mu            sync.Mutex
	WebhookSecret string
	FailCreate    bool
	FailReason    string
	CustomQR      string
	CreatedCount  int
	RefundedCount int
	Refunds map[string]RefundResult
}

// NewMockProvider initializes a new MockProvider with a webhook secret.
func NewMockProvider(webhookSecret string) *MockProvider {
	return &MockProvider{
		WebhookSecret: strings.TrimSpace(webhookSecret),
	}
}

// Name returns the provider identifier.
func (m *MockProvider) Name() string {
	return "mock"
}

// SetFailCreate configures the mock provider to simulate creation failure.
func (m *MockProvider) SetFailCreate(fail bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FailCreate = fail
	m.FailReason = reason
}

// CreatePayment generates a deterministic mock UPI QR payload and provider order ID.
func (m *MockProvider) CreatePayment(ctx context.Context, req CreatePaymentRequest) (*CreatePaymentResult, error) {
	if ctx == nil {
		return nil, errors.New("mock provider: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mock provider: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.FailCreate {
		reason := m.FailReason
		if reason == "" {
			reason = "simulated provider creation error"
		}
		return nil, errors.New(reason)
	}

	if req.AmountPaise <= 0 {
		return nil, fmt.Errorf("mock provider: invalid amount_paise %d", req.AmountPaise)
	}
	if strings.TrimSpace(req.OrderID) == "" {
		return nil, errors.New("mock provider: order_id cannot be empty")
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, errors.New("mock provider: session_id cannot be empty")
	}

	m.CreatedCount++
	providerOrderID := fmt.Sprintf("mock_qr_%s", req.AttemptID)
	if providerOrderID == "mock_qr_" {
		providerOrderID = fmt.Sprintf("mock_qr_%s_%d", req.OrderID, time.Now().UnixNano())
	}

	qrPayload := m.CustomQR
	if qrPayload == "" {
		rupees := float64(req.AmountPaise) / 100.0
		pn := url.QueryEscape("XeroxGo Kiosk")
		tn := url.QueryEscape(fmt.Sprintf("Print Order %s", req.OrderID))
		qrPayload = fmt.Sprintf("upi://pay?pa=xeroxgo.kiosk@upi&pn=%s&am=%.2f&cu=INR&tr=%s&tn=%s",
			pn, rupees, providerOrderID, tn)
	}

	expiresAt := req.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(10 * time.Minute)
	}

	return &CreatePaymentResult{
		Provider:        "mock",
		ProviderOrderID: providerOrderID,
		QRPayload:       qrPayload,
		QRImageURL:      fmt.Sprintf("https://mock-payments.xeroxgo.internal/qr/%s.png", providerOrderID),
		Status:          "CREATED",
		ExpiresAt:       expiresAt,
	}, nil
}

// VerifyWebhookSignature verifies HMAC signature with the mock provider's secret.
func (m *MockProvider) VerifyWebhookSignature(payload []byte, signature string) bool {
	return VerifyWebhookSignature(payload, signature, m.WebhookSecret)
}


// Refund returns an idempotent simulated refund. No external money is moved.
func (m *MockProvider) Refund(ctx context.Context, paymentID string, amountPaise int64, key string) (RefundResult, error) {
	if err := ctx.Err(); err != nil { return RefundResult{}, err }
	if paymentID == "" || amountPaise <= 0 || key == "" { return RefundResult{}, errors.New("invalid mock refund") }
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Refunds == nil { m.Refunds = make(map[string]RefundResult) }
	if prev, exists := m.Refunds[key]; exists {
		if prev.PaymentID != paymentID || prev.Amount != amountPaise {
			return RefundResult{}, errors.New("idempotency key reused with different refund")
		}
		return prev, nil
	}
	r := RefundResult{ID:"rfnd_mock_"+key, PaymentID:paymentID, Amount:amountPaise, Status:"processed"}
	m.Refunds[key] = r
	m.RefundedCount++
	return r, nil
}
func (m *MockProvider) GetRefund(ctx context.Context, refundID string) (RefundResult, error) {
	if err := ctx.Err(); err != nil { return RefundResult{}, err }
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.Refunds { if r.ID == refundID { return r, nil } }
	return RefundResult{}, errors.New("refund not found")
}
