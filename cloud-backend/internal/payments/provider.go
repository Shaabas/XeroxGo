package payments

import (
	"context"
	"time"
)

// CreatePaymentRequest defines the parameters required to initiate a payment attempt.
type CreatePaymentRequest struct {
	AttemptID   string
	OrderID     string
	KioskID     string
	SessionID   string
	AmountPaise int64
	Currency    string
	Description string
	ExpiresAt   time.Time
	Notes       map[string]string
}

// CreatePaymentResult contains the outcome of payment initiation from the provider.
type CreatePaymentResult struct {
	Provider          string
	ProviderOrderID   string // e.g. "qr_..." or "order_..."
	ProviderPaymentID string
	QRPayload         string // Scannable UPI deep-link URI: "upi://pay?..."
	QRImageURL        string
	Status            string // "CREATED"
	ExpiresAt         time.Time
}

// Provider defines the interface for creating payments and verifying webhooks.
type Provider interface {
	Name() string
	CreatePayment(ctx context.Context, req CreatePaymentRequest) (*CreatePaymentResult, error)
	VerifyWebhookSignature(payload []byte, signature string) bool
}

