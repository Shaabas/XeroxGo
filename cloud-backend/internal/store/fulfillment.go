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
	_, err := s.db.ExecContext(ctx, schema)
	return err
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
	res, err := tx.ExecContext(ctx, s.rebind(`
UPDATE orders SET pin_expires_at_ns = ?, updated_at_ns = ?
WHERE order_id = ? AND display_code <> '' AND pin_expires_at_ns > ?;`),
		until, nowNS, orderID, nowNS)
	if err != nil { return fmt.Errorf("promote paid pin: %w", err) }
	affected, err := res.RowsAffected()
	if err != nil { return err }
	if affected == 0 { return nil }
	// Guard against a PIN that has already been independently released or
	// reused: do not overwrite another customer's reservation.
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE pin_reservations SET expires_at_ns = ?
WHERE order_id = ? AND expires_at_ns > ?;`), until, orderID, nowNS)
	return err
}

// Creates one durable full-refund obligation for a captured payment. A failure
// does not mean a refund has occurred. Submission is safety gated separately.
func (s *Store) RecordRefundRequired(ctx context.Context, orderID string) error {
	var paymentID, provider string
	var amount int64
	err := s.db.QueryRowContext(ctx, s.rebind(`
SELECT payment_id, provider, amount_paise FROM payments
WHERE order_id = ? AND status IN ('captured', 'CAPTURED')
ORDER BY created_at_ns ASC LIMIT 1;`), orderID).Scan(&paymentID, &provider, &amount)
	if errors.Is(err, sql.ErrNoRows) { return nil } // unpaid orders need no refund
	if err != nil { return err }
	if amount <= 0 { return errors.New("invalid captured amount") }
	now := time.Now().UTC().UnixNano()
	_, err = s.db.ExecContext(ctx, s.rebind(`
INSERT INTO refund_requests(order_id, payment_id, provider, amount_paise, status, idempotency_key, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(order_id) DO NOTHING;`),
		orderID, paymentID, provider, amount, RefundRequired, "xeroxgo_refund_"+orderID, now, now)
	return err
}

func (s *Store) GetRefundRequest(ctx context.Context, orderID string) (*RefundRequest, error) {
	var v RefundRequest
	var safe int
	err := s.db.QueryRowContext(ctx, s.rebind(`
SELECT order_id, payment_id, provider, amount_paise, status, idempotency_key, provider_refund_id,
safe_stop_verified, last_error FROM refund_requests WHERE order_id = ?;`), orderID).Scan(
		&v.OrderID, &v.PaymentID, &v.Provider, &v.AmountPaise, &v.Status,
		&v.IdempotencyKey, &v.ProviderRefundID, &safe, &v.LastError)
	if errors.Is(err, sql.ErrNoRows) { return nil, nil }
	v.SafeStopVerified = safe != 0
	return &v, err
}

// Pi emits safe-stop only AFTER verifying every child job is safely canceled
// or never submitted. It is not sufficient to receive PAUSED_ERROR alone.
func (s *Store) ConfirmRefundSafeStop(ctx context.Context, orderID string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE refund_requests SET safe_stop_verified = 1, updated_at_ns = ?
WHERE order_id = ? AND status IN ('REQUIRED', 'FAILED');`),
		time.Now().UTC().UnixNano(), orderID)
	return err
}

func (s *Store) ListActionableRefunds(ctx context.Context, olderThan time.Time) ([]RefundRequest, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`
SELECT order_id, payment_id, provider, amount_paise, status, idempotency_key,
provider_refund_id, safe_stop_verified, last_error
FROM refund_requests WHERE safe_stop_verified = 1
AND ((status IN ('REQUIRED', 'FAILED')) OR (status = 'REQUESTING' AND updated_at_ns < ?))
ORDER BY created_at_ns ASC LIMIT 50;`), olderThan.UnixNano())
	if err != nil { return nil, err }
	defer rows.Close()
	var out []RefundRequest
	for rows.Next() {
		var v RefundRequest
		var safe int
		if err := rows.Scan(&v.OrderID, &v.PaymentID, &v.Provider, &v.AmountPaise, &v.Status,
			&v.IdempotencyKey, &v.ProviderRefundID, &safe, &v.LastError); err != nil { return nil, err }
		v.SafeStopVerified = safe != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// Compare-and-swap prevents two replicas from concurrently submitting the
// same refund. Crashes can retry the same provider idempotency key after 60s.
func (s *Store) ClaimRefund(ctx context.Context, orderID string, retryBefore time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE refund_requests SET status = 'REQUESTING', attempts = attempts + 1,
updated_at_ns = ?, last_error = ''
WHERE order_id = ? AND safe_stop_verified = 1
AND (status IN ('REQUIRED', 'FAILED') OR (status = 'REQUESTING' AND updated_at_ns < ?));`),
		time.Now().UTC().UnixNano(), orderID, retryBefore.UnixNano())
	if err != nil { return false, err }
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) RecordRefundResult(ctx context.Context, orderID, providerRefundID, status, errorText string) error {
	if status != RefundPending && status != RefundProcessed && status != RefundFailed {
		return errors.New("unsupported refund result")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return err }
	defer tx.Rollback()
	now := time.Now().UTC().UnixNano()
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE refund_requests SET status = ?, provider_refund_id = ?, last_error = ?, updated_at_ns = ?
WHERE order_id = ? AND status IN ('REQUESTING','PENDING','FAILED');`),
		status, providerRefundID, errorText, now, orderID)
	if err != nil { return err }
	if status == RefundProcessed {
		// Once the provider reports processed, do not allow the order to print.
		_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders SET status = 'FAILED_REFUNDABLE', updated_at_ns = ? WHERE order_id = ?
AND status NOT IN ('COMPLETED', 'FAILED_REFUNDABLE');`), now, orderID)
		if err != nil { return err }
		_, err = tx.ExecContext(ctx, s.rebind("DELETE FROM pin_reservations WHERE order_id = ?"), orderID)
		if err != nil { return err }
	}
	return tx.Commit()
}

// Pending refunds need provider reconciliation: a successful API response does
// not necessarily mean funds have completed their return.
func (s *Store) ListPendingRefunds(ctx context.Context, olderThan time.Time) ([]RefundRequest, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`
SELECT order_id, payment_id, provider, amount_paise, status, idempotency_key,
provider_refund_id, safe_stop_verified, last_error
FROM refund_requests WHERE status = 'PENDING' AND provider_refund_id <> ''
AND updated_at_ns < ? ORDER BY updated_at_ns ASC LIMIT 50;`), olderThan.UnixNano())
	if err != nil { return nil, err }
	defer rows.Close()
	var items []RefundRequest
	for rows.Next() {
		var r RefundRequest
		var confirmed int
		if err := rows.Scan(&r.OrderID, &r.PaymentID, &r.Provider, &r.AmountPaise,
			&r.Status, &r.IdempotencyKey, &r.ProviderRefundID, &confirmed, &r.LastError); err != nil {
			return nil, err
		}
		r.SafeStopVerified = confirmed != 0
		items = append(items, r)
	}
	return items, rows.Err()
}

// A kiosk with an unconfirmed stop must not be restored to service.
func (s *Store) HasUnsafeRefundStops(ctx context.Context, kioskID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(`
SELECT COUNT(*) FROM refund_requests rr
JOIN orders o ON o.order_id = rr.order_id
WHERE o.kiosk_id = ? AND rr.safe_stop_verified = 0
AND rr.status <> 'PROCESSED';`), kioskID).Scan(&n)
	return n > 0, err
}
