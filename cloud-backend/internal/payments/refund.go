package payments

import "context"

// RefundProvider is implemented by the sole demo payment simulator.
type RefundProvider interface {
	Refund(ctx context.Context, paymentID string, amountPaise int64, idempotencyKey string) (RefundResult, error)
	GetRefund(ctx context.Context, refundID string) (RefundResult, error)
}

type RefundResult struct {
	ID        string
	PaymentID string
	Amount    int64
	Status    string // pending | processed | failed
}
