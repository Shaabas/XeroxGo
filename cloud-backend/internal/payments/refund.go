package payments

// RefundResult describes the outcome of a simulated refund. No funds move.
type RefundResult struct {
	ID        string
	PaymentID string
	Amount    int64
	Status    string // pending | processed | failed
}
