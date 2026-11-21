package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RefundProvider is separate from Provider so existing payment mocks and
// payment-only consumers need not change to support refunds.
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

func (p *RazorpayProvider) Refund(ctx context.Context, paymentID string, amountPaise int64, idempotencyKey string) (RefundResult, error) {
	if p == nil || paymentID == "" || amountPaise <= 0 || len(idempotencyKey) < 10 {
		return RefundResult{}, errors.New("razorpay refund requires payment id, positive amount, durable idempotency key")
	}
	body, err := json.Marshal(map[string]interface{}{"amount": amountPaise, "speed": "normal"})
	if err != nil { return RefundResult{}, err }
	target := p.BaseURL + "/v1/payments/" + url.PathEscape(paymentID) + "/refund"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil { return RefundResult{}, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refund-Idempotency", idempotencyKey)
	req.SetBasicAuth(p.KeyID, p.KeySecret)
	resp, err := p.HTTPClient.Do(req)
	if err != nil { return RefundResult{}, err }
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil { return RefundResult{}, err }
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return RefundResult{}, fmt.Errorf("razorpay refund HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseRefundResponse(raw, paymentID, amountPaise)
}

func (p *RazorpayProvider) GetRefund(ctx context.Context, refundID string) (RefundResult, error) {
	if strings.TrimSpace(refundID) == "" { return RefundResult{}, errors.New("refund id required") }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.BaseURL+"/v1/refunds/"+url.PathEscape(refundID), nil)
	if err != nil { return RefundResult{}, err }
	req.SetBasicAuth(p.KeyID, p.KeySecret)
	resp, err := p.HTTPClient.Do(req)
	if err != nil { return RefundResult{}, err }
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil { return RefundResult{}, err }
	if resp.StatusCode != http.StatusOK {
		return RefundResult{}, fmt.Errorf("razorpay fetch refund HTTP %d", resp.StatusCode)
	}
	result, err := parseRefundResponse(raw, "", 0)
	if err != nil { return RefundResult{}, err }
	if result.ID != refundID { return RefundResult{}, errors.New("razorpay refund ID mismatch") }
	return result, nil
}

func parseRefundResponse(raw []byte, expectedPaymentID string, expectedAmount int64) (RefundResult, error) {
	var v struct {
		ID string `json:"id"`
		PaymentID string `json:"payment_id"`
		Amount int64 `json:"amount"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &v); err != nil { return RefundResult{}, err }
	if v.ID == "" || v.PaymentID == "" || v.Amount <= 0 {
		return RefundResult{}, errors.New("razorpay refund response incomplete")
	}
	if expectedPaymentID != "" && v.PaymentID != expectedPaymentID {
		return RefundResult{}, errors.New("razorpay refund payment mismatch")
	}
	if expectedAmount > 0 && v.Amount != expectedAmount {
		return RefundResult{}, errors.New("razorpay refund amount mismatch")
	}
	switch strings.ToLower(v.Status) {
	case "pending", "processed", "failed":
		return RefundResult{ID:v.ID, PaymentID:v.PaymentID, Amount:v.Amount, Status:strings.ToLower(v.Status)}, nil
	default:
		return RefundResult{}, fmt.Errorf("unsupported razorpay refund status %q", v.Status)
	}
}
