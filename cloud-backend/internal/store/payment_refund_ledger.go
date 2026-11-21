package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	RefundReasonFulfillment = "FULFILLMENT_FAILED"
	RefundReasonExcess = "EXCESS_CAPTURE"
)

// Razorpay's refund idempotency key permits ASCII letters, digits, '-' and '_'.
func refundIdempotencyKey(paymentID string) string {
	sum := sha256.Sum256([]byte(paymentID))
	return "xeroxgo_pay_" + hex.EncodeToString(sum[:16])
}

// Additive, idempotent migration. Preserve the old order-scoped refund table
// for audit and copy all in-flight statuses, refund IDs and idempotency keys.
func (s *Store) migratePaymentRefundSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return err }
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS order_funding_payments(
	order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
	payment_id TEXT NOT NULL UNIQUE,
	created_at_ns BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS payment_refund_requests(
	payment_id TEXT PRIMARY KEY,
	order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	amount_paise BIGINT NOT NULL CHECK(amount_paise > 0),
	reason TEXT NOT NULL CHECK(reason IN ('FULFILLMENT_FAILED','EXCESS_CAPTURE')),
	status TEXT NOT NULL,
	idempotency_key TEXT NOT NULL UNIQUE,
	provider_refund_id TEXT NOT NULL DEFAULT '',
	safe_stop_verified INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at_ns BIGINT NOT NULL,
	updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_refunds_order ON payment_refund_requests(order_id,reason);
CREATE INDEX IF NOT EXISTS idx_payment_refunds_worker ON payment_refund_requests(status,updated_at_ns);
`)
	if err != nil { return fmt.Errorf("refund migration schema: %w", err) }
	// The old obligation's payment is authoritative for orders already
	// being refunded, including ambiguous same-timestamp captures.
	_, err = tx.ExecContext(ctx, `
INSERT INTO order_funding_payments(order_id,payment_id,created_at_ns)
SELECT order_id,payment_id,created_at_ns FROM refund_requests WHERE payment_id <> ''
ON CONFLICT(order_id) DO NOTHING;`)
	if err != nil { return fmt.Errorf("refund migration legacy funding: %w", err) }
	_, err = tx.ExecContext(ctx, `
INSERT INTO order_funding_payments(order_id,payment_id,created_at_ns)
SELECT p.order_id,p.payment_id,p.created_at_ns FROM payments p
WHERE LOWER(p.status) IN ('captured','paid')
AND NOT EXISTS (
	SELECT 1 FROM payments x WHERE x.order_id=p.order_id
	AND LOWER(x.status) IN ('captured','paid')
	AND (x.created_at_ns < p.created_at_ns OR
	(x.created_at_ns=p.created_at_ns AND x.payment_id < p.payment_id))
)
ON CONFLICT(order_id) DO NOTHING;`)
	if err != nil { return fmt.Errorf("refund migration funding: %w", err) }
	_, err = tx.ExecContext(ctx, `
INSERT INTO payment_refund_requests(
	payment_id,order_id,provider,amount_paise,reason,status,idempotency_key,
	provider_refund_id,safe_stop_verified,attempts,last_error,created_at_ns,updated_at_ns)
SELECT payment_id,order_id,provider,amount_paise,'FULFILLMENT_FAILED',status,idempotency_key,
	provider_refund_id,safe_stop_verified,attempts,last_error,created_at_ns,updated_at_ns
FROM refund_requests WHERE payment_id <> ''
ON CONFLICT(payment_id) DO NOTHING;`)
	if err != nil { return fmt.Errorf("refund migration legacy obligations: %w", err) }
	rows, err := tx.QueryContext(ctx, `
SELECT p.payment_id,p.order_id,p.provider,p.amount_paise,p.created_at_ns
FROM payments p JOIN order_funding_payments f ON f.order_id=p.order_id
WHERE LOWER(p.status) IN ('captured','paid') AND p.payment_id <> f.payment_id;`)
	if err != nil { return err }
	type item struct { id, orderID, provider string; amount, at int64 }
	var extra []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id,&x.orderID,&x.provider,&x.amount,&x.at); err != nil {
			rows.Close(); return err
		}
		extra=append(extra,x)
	}
	if err := rows.Err(); err != nil { rows.Close(); return err }
	rows.Close()
	for _, x := range extra {
		if x.amount <= 0 { return fmt.Errorf("invalid historic captured amount %s",x.id) }
		_, err = tx.ExecContext(ctx,s.rebind(`
INSERT INTO payment_refund_requests(
	payment_id,order_id,provider,amount_paise,reason,status,idempotency_key,
	safe_stop_verified,created_at_ns,updated_at_ns)
VALUES (?,?,?,?,'EXCESS_CAPTURE','REQUIRED',?,1,?,?)
ON CONFLICT(payment_id) DO NOTHING;`),
			x.id,x.orderID,x.provider,x.amount,refundIdempotencyKey(x.id),x.at,x.at)
		if err != nil { return fmt.Errorf("backfill excess payment %s: %w",x.id,err) }
	}
	return tx.Commit()
}

// A surplus capture must not silently succeed against an incomplete order.
// It uses the same required print-configuration invariant as GetOrder, but
// checks it inside the capture transaction (avoids SQLite connection deadlock).
func (s *Store) ensureRefundableOrderSnapshot(ctx context.Context, tx *sql.Tx, orderID string) error {
	var found int
	err := tx.QueryRowContext(ctx, s.rebind(
		"SELECT 1 FROM print_configs WHERE order_id=?"), orderID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: order %s has no print configuration", ErrOrderNotFound, orderID)
	}
	return err
}

// Atomic with the new payment row: no surplus capture can be acknowledged
// without also creating its independent refund obligation.
func (s *Store) recordExcessCaptureInTx(ctx context.Context,tx *sql.Tx,p PaymentRecord,kioskID string,nowNS int64) error {
	_, err := tx.ExecContext(ctx,s.rebind(`
INSERT INTO payments(payment_id,order_id,kiosk_id,session_id,amount_paise,status,provider,created_at_ns)
VALUES(?,?,?,?,?,?,?,?);`),p.PaymentID,p.OrderID,kioskID,p.SessionID,p.AmountPaise,p.Status,p.Provider,nowNS)
	if err != nil { return err }
	_, err = tx.ExecContext(ctx,s.rebind(`
INSERT INTO payment_webhooks(webhook_id,received_at_ns) VALUES(?,?)
ON CONFLICT(webhook_id) DO NOTHING;`),p.PaymentID,nowNS)
	if err != nil { return err }
	_, err = tx.ExecContext(ctx,s.rebind(`
INSERT INTO payment_refund_requests(
	payment_id,order_id,provider,amount_paise,reason,status,idempotency_key,
	safe_stop_verified,created_at_ns,updated_at_ns)
VALUES(?,?,?,?,'EXCESS_CAPTURE','REQUIRED',?,1,?,?)
ON CONFLICT(payment_id) DO NOTHING;`),p.PaymentID,p.OrderID,p.Provider,p.AmountPaise,refundIdempotencyKey(p.PaymentID),nowNS,nowNS)
	return err
}

func (s *Store) ClaimRefundPayment(ctx context.Context,paymentID string,retryBefore time.Time)(bool,error) {
	res,err:=s.db.ExecContext(ctx,s.rebind(`
UPDATE payment_refund_requests SET status='REQUESTING',attempts=attempts+1,updated_at_ns=?,last_error=''
WHERE payment_id=? AND safe_stop_verified=1 AND
(status IN ('REQUIRED','FAILED') OR (status='REQUESTING' AND updated_at_ns < ?));`),
		time.Now().UTC().UnixNano(),paymentID,retryBefore.UnixNano())
	if err!=nil{return false,err}
	n,err:=res.RowsAffected()
	return n==1,err
}

// Excess-capture refunds must never close or revoke a legitimate print order.
func (s *Store) RecordRefundResultForPayment(ctx context.Context,paymentID,providerRefundID,status,errorText string)error {
	if status!=RefundPending && status!=RefundProcessed && status!=RefundFailed {
		return errors.New("unsupported refund result")
	}
	tx,err:=s.db.BeginTx(ctx,nil)
	if err!=nil{return err}
	defer tx.Rollback()
	var reason,orderID string
	err=tx.QueryRowContext(ctx,s.rebind(
		"SELECT reason,order_id FROM payment_refund_requests WHERE payment_id=?"),paymentID).Scan(&reason,&orderID)
	if err!=nil{return err}
	now:=time.Now().UTC().UnixNano()
	res,err:=tx.ExecContext(ctx,s.rebind(`
UPDATE payment_refund_requests SET status=?,provider_refund_id=?,last_error=?,updated_at_ns=?
WHERE payment_id=? AND safe_stop_verified=1
AND status IN ('REQUESTING','PENDING','FAILED');`),status,providerRefundID,errorText,now,paymentID)
	if err!=nil{return err}
	n,err:=res.RowsAffected()
	if err!=nil{return err}
	if n!=1{return fmt.Errorf("refund for %s: invalid state or missing safe stop",paymentID)}
	if status==RefundProcessed && reason==RefundReasonFulfillment {
		_,err=tx.ExecContext(ctx,s.rebind(`
UPDATE orders SET status='FAILED_REFUNDABLE',updated_at_ns=? WHERE order_id=?
AND status NOT IN ('COMPLETED','FAILED_REFUNDABLE');`),now,orderID)
		if err!=nil{return err}
		_,err=tx.ExecContext(ctx,s.rebind("DELETE FROM pin_reservations WHERE order_id=?"),orderID)
		if err!=nil{return err}
	}
	return tx.Commit()
}


// Public checkout exposes only aggregate refund counts; payment IDs and
// provider credentials never appear in the unauthenticated order-status API.
type ExcessRefundSummary struct {
	Total int
	Processed int
	Unresolved int
}

func (s *Store) GetExcessRefundSummary(ctx context.Context, orderID string) (ExcessRefundSummary, error) {
	var out ExcessRefundSummary
	err := s.db.QueryRowContext(ctx, s.rebind(`
SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='PROCESSED' THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN status <> 'PROCESSED' THEN 1 ELSE 0 END),0)
FROM payment_refund_requests WHERE order_id=? AND reason='EXCESS_CAPTURE';`),
		orderID).Scan(&out.Total,&out.Processed,&out.Unresolved)
	return out, err
}
