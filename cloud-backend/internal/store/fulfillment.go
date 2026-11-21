package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A paid order is not subject to the five-minute unpaid PIN deadline.
// 2100 is within Go's signed nanosecond range and provides a durable PIN lease.
var paidPINDeadline = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// PaidPINExpiry is used when securely rotating a paid order's lost/expired PIN.
// It is NOT a payment session timeout and is revoked at fulfillment closure.
func PaidPINExpiry() time.Time { return paidPINDeadline }

// A refund must not be submitted until the Pi has confirmed that every CUPS
// job for the affected order is canceled, aborted, or was never submitted.
// A jammed/held job is NOT a safe-stop confirmation.
const (
	RefundRequired   = "REQUIRED"
	RefundRequesting = "REQUESTING"
	RefundPending    = "PENDING"
	RefundProcessed  = "PROCESSED"
	RefundFailed     = "FAILED"
)

type RefundRequest struct {
	OrderID          string
	PaymentID        string
	Provider         string
	AmountPaise      int64
	Status           string
	IdempotencyKey   string
	ProviderRefundID string
	SafeStopVerified bool
	LastError        string
	Reason           string
}

func (s *Store) ensureFulfillmentSchema(ctx context.Context) error {
	// Both statements work in the Postgres and SQLite development stores.
	const schema = `
CREATE TABLE IF NOT EXISTS kiosk_service_flags (
    kiosk_id TEXT PRIMARY KEY,
    out_of_service INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    updated_at_ns BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS refund_requests (
    order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
    payment_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    amount_paise BIGINT NOT NULL CHECK(amount_paise > 0),
    status TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    provider_refund_id TEXT NOT NULL DEFAULT '',
    safe_stop_verified INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refund_requests_status ON refund_requests(status, updated_at_ns);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil { return err }
	return s.migratePaymentRefundSchema(ctx)
}

func (s *Store) BlockKiosk(ctx context.Context, kioskID, reason string) error {
	if strings.TrimSpace(kioskID) == "" {
		return errors.New("kiosk id required")
	}
	if strings.TrimSpace(reason) == "" { reason = "printer fault requires attendant inspection" }
	_, err := s.db.ExecContext(ctx, s.rebind(`
INSERT INTO kiosk_service_flags(kiosk_id, out_of_service, reason, updated_at_ns)
VALUES (?, 1, ?, ?)
ON CONFLICT(kiosk_id) DO UPDATE
SET out_of_service = 1, reason = excluded.reason, updated_at_ns = excluded.updated_at_ns;`),
		kioskID, reason, time.Now().UTC().UnixNano())
	return err
}

func (s *Store) GetKioskBlock(ctx context.Context, kioskID string) (bool, string, error) {
	var active int
	var reason string
	err := s.db.QueryRowContext(ctx, s.rebind(
		"SELECT out_of_service, reason FROM kiosk_service_flags WHERE kiosk_id = ?"), kioskID).Scan(&active, &reason)
	if errors.Is(err, sql.ErrNoRows) { return false, "", nil }
	return active != 0, reason, err
}

// Only an authenticated operator endpoint may clear a sticky kiosk fault.
func (s *Store) ClearKioskBlock(ctx context.Context, kioskID string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		"UPDATE kiosk_service_flags SET out_of_service = 0, reason = '', updated_at_ns = ? WHERE kiosk_id = ?"),
		time.Now().UTC().UnixNano(), kioskID)
	return err
}

// Promote a still-valid unpaid PIN when captured payment is persisted.
// Expired/reused PINs are never resurrected: they require an authenticated
// recovery rotation after capture.
func (s *Store) extendPaidPINInTx(ctx context.Context, tx *sql.Tx, orderID string, nowNS int64) error {
	until := paidPINDeadline.UnixNano()
	// Keep a paid PIN only if it is still reserved for THIS order. Extending
	// orders.display_code without a live matching reservation could revive a
	// code already recycled for a different customer.
	res, err := tx.ExecContext(ctx, s.rebind(`
UPDATE pin_reservations SET expires_at_ns = ?
WHERE order_id = ? AND expires_at_ns > ?
AND kiosk_id = (SELECT kiosk_id FROM orders WHERE order_id = ?)
AND pin = (SELECT display_code FROM orders WHERE order_id = ?);`),
		until, orderID, nowNS, orderID, orderID)
	if err != nil { return fmt.Errorf("promote paid pin reservation: %w", err) }
	n, err := res.RowsAffected()
	if err != nil { return err }
	if n != 1 {
		// Late capture is still valid payment, but must use secure recovery
		// token rotation instead of resurrecting the old PIN.
		return nil
	}
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders SET pin_expires_at_ns = ?, updated_at_ns = ?
WHERE order_id = ? AND display_code <> '' AND pin_expires_at_ns > ?;`),
		until, nowNS, orderID, nowNS)
	return err
}

// Creates one durable full-refund obligation for a captured payment. A failure
// does not mean a refund has occurred. Submission is safety gated separately.
func (s *Store) RecordRefundRequired(ctx context.Context, orderID string) error {
	var paymentID, provider string
	var amount int64
	// Only the capture funding the print order is subject to physical safe-stop.
	// The first historic capture is the fallback for older manually seeded data.
	err := s.db.QueryRowContext(ctx, s.rebind("\nSELECT p.payment_id,p.provider,p.amount_paise\nFROM payments p LEFT JOIN order_funding_payments f ON f.order_id=p.order_id\nWHERE p.order_id=? AND LOWER(p.status) IN ('captured','paid')\nAND (p.payment_id=f.payment_id OR (f.payment_id IS NULL AND NOT EXISTS(\n  SELECT 1 FROM payments older WHERE older.order_id=p.order_id\n  AND LOWER(older.status) IN ('captured','paid')\n  AND (older.created_at_ns < p.created_at_ns OR\n    (older.created_at_ns=p.created_at_ns AND older.payment_id < p.payment_id))\n)))\nLIMIT 1;"), orderID).Scan(&paymentID,&provider,&amount)
	if errors.Is(err,sql.ErrNoRows) { return nil }
	if err!=nil { return err }
	if amount<=0 { return errors.New("invalid captured amount") }
	var orderStatus string
	if err:=s.db.QueryRowContext(ctx,s.rebind("SELECT status FROM orders WHERE order_id=?"),orderID).Scan(&orderStatus);err!=nil{return err}
	if orderStatus=="COMPLETED" {return nil}
	now:=time.Now().UTC().UnixNano()
	_,err=s.db.ExecContext(ctx,s.rebind("\nINSERT INTO payment_refund_requests(\n payment_id,order_id,provider,amount_paise,reason,status,idempotency_key,\n created_at_ns,updated_at_ns)\nVALUES(?,?,?,?,'FULFILLMENT_FAILED','REQUIRED',?,?,?)\nON CONFLICT(payment_id) DO NOTHING;"),
		paymentID,orderID,provider,amount,refundIdempotencyKey(paymentID),now,now)
	return err
}
func (s *Store) GetRefundRequest(ctx context.Context, orderID string) (*RefundRequest,error) {
	var v RefundRequest
	var safe int
	err:=s.db.QueryRowContext(ctx,s.rebind("\nSELECT order_id,payment_id,provider,amount_paise,reason,status,idempotency_key,\nprovider_refund_id,safe_stop_verified,last_error\nFROM payment_refund_requests WHERE order_id=? AND reason='FULFILLMENT_FAILED'\nORDER BY created_at_ns ASC LIMIT 1;"),orderID).Scan(
		&v.OrderID,&v.PaymentID,&v.Provider,&v.AmountPaise,&v.Reason,&v.Status,
		&v.IdempotencyKey,&v.ProviderRefundID,&safe,&v.LastError)
	if errors.Is(err,sql.ErrNoRows){return nil,nil}
	if err!=nil{return nil,err}
	v.SafeStopVerified=safe!=0
	return &v,nil
}
// Pi emits safe-stop only AFTER verifying every child job is safely canceled
// or never submitted. It is not sufficient to receive PAUSED_ERROR alone.
func (s *Store) ConfirmRefundSafeStop(ctx context.Context,orderID string)error{
	_,err:=s.db.ExecContext(ctx,s.rebind("\nUPDATE payment_refund_requests SET safe_stop_verified=1,updated_at_ns=?\nWHERE order_id=? AND reason='FULFILLMENT_FAILED' AND status IN ('REQUIRED','FAILED');"),
		time.Now().UTC().UnixNano(),orderID)
	return err
}
func (s *Store) ListActionableRefunds(ctx context.Context,olderThan time.Time)([]RefundRequest,error){
	rows,err:=s.db.QueryContext(ctx,s.rebind("\nSELECT order_id,payment_id,provider,amount_paise,reason,status,idempotency_key,\nprovider_refund_id,safe_stop_verified,last_error\nFROM payment_refund_requests WHERE safe_stop_verified=1\nAND (status IN ('REQUIRED','FAILED') OR (status='REQUESTING' AND updated_at_ns < ?))\nORDER BY created_at_ns ASC LIMIT 50;"),olderThan.UnixNano())
	if err!=nil{return nil,err}
	defer rows.Close()
	var out []RefundRequest
	for rows.Next(){
		var v RefundRequest
		var safe int
		if err:=rows.Scan(&v.OrderID,&v.PaymentID,&v.Provider,&v.AmountPaise,&v.Reason,
			&v.Status,&v.IdempotencyKey,&v.ProviderRefundID,&safe,&v.LastError);err!=nil{return nil,err}
		v.SafeStopVerified=safe!=0
		out=append(out,v)
	}
	return out,rows.Err()
}
// Compare-and-swap prevents two replicas from concurrently submitting the
// same refund. Crashes can retry the same provider idempotency key after 60s.
func (s *Store) ClaimRefund(ctx context.Context,orderID string,retryBefore time.Time)(bool,error){
	r,err:=s.GetRefundRequest(ctx,orderID)
	if err!=nil||r==nil{return false,err}
	return s.ClaimRefundPayment(ctx,r.PaymentID,retryBefore)
}
func (s *Store) RecordRefundResult(ctx context.Context,orderID,providerRefundID,status,errorText string)error{
	r,err:=s.GetRefundRequest(ctx,orderID)
	if err!=nil{return err}
	if r==nil{return fmt.Errorf("no fulfillment refund for order %s",orderID)}
	return s.RecordRefundResultForPayment(ctx,r.PaymentID,providerRefundID,status,errorText)
}
// Pending refunds need provider reconciliation: a successful API response does
// not necessarily mean funds have completed their return.
func (s *Store) ListPendingRefunds(ctx context.Context,olderThan time.Time)([]RefundRequest,error){
	rows,err:=s.db.QueryContext(ctx,s.rebind("\nSELECT order_id,payment_id,provider,amount_paise,reason,status,idempotency_key,\nprovider_refund_id,safe_stop_verified,last_error\nFROM payment_refund_requests WHERE status='PENDING' AND provider_refund_id <> ''\nAND updated_at_ns < ? ORDER BY updated_at_ns ASC LIMIT 50;"),olderThan.UnixNano())
	if err!=nil{return nil,err}
	defer rows.Close()
	var out []RefundRequest
	for rows.Next(){
		var v RefundRequest
		var safe int
		if err:=rows.Scan(&v.OrderID,&v.PaymentID,&v.Provider,&v.AmountPaise,&v.Reason,
			&v.Status,&v.IdempotencyKey,&v.ProviderRefundID,&safe,&v.LastError);err!=nil{return nil,err}
		v.SafeStopVerified=safe!=0
		out=append(out,v)
	}
	return out,rows.Err()
}
// A kiosk with an unconfirmed stop must not be restored to service.
func (s *Store) HasUnsafeRefundStops(ctx context.Context,kioskID string)(bool,error){
	var n int
	err:=s.db.QueryRowContext(ctx,s.rebind("\nSELECT COUNT(*) FROM payment_refund_requests rr\nJOIN orders o ON o.order_id=rr.order_id\nWHERE o.kiosk_id=? AND rr.reason='FULFILLMENT_FAILED'\nAND rr.safe_stop_verified=0 AND rr.status <> 'PROCESSED';"),kioskID).Scan(&n)
	return n>0,err
}
// Paid PAID_WAITING orders must never be failed or refunded solely because
// their kiosk session expired or physical dispatch has not yet started.
// Terminal refund obligations are created by explicit failure paths using
// RecordRefundRequired (and verified safe-stop), not elapsed time.
//
// The former ListPaidWithoutDispatch and FailNeverDispatchedPaidOrder helpers
// were removed to prevent any caller from silently reintroducing that policy.

type KioskServiceFlag struct {
	KioskID      string `json:"kiosk_id"`
	Reason       string `json:"reason"`
	UpdatedAtNS  int64  `json:"updated_at_ns"`
}

func (s *Store) ListOutOfServiceKiosks(ctx context.Context) ([]KioskServiceFlag, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT kiosk_id, reason, updated_at_ns FROM kiosk_service_flags
WHERE out_of_service = 1 ORDER BY updated_at_ns DESC LIMIT 100;`)
	if err != nil { return nil, err }
	defer rows.Close()
	list := make([]KioskServiceFlag, 0)
	for rows.Next() {
		var x KioskServiceFlag
		if err := rows.Scan(&x.KioskID, &x.Reason, &x.UpdatedAtNS); err != nil { return nil, err }
		list = append(list, x)
	}
	return list, rows.Err()
}
