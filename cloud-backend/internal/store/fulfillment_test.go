package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestFulfillment_PaidPINAndRefundSafetyLedger(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fulfillment.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID, orderID := "kiosk-fault-test", "ord_fault_test"
	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "4721", 3, contracts.JobStatusPendingPayment, now)
	// The PIN is still valid at capture time.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { t.Fatal(err) }
	if err := s.extendPaidPINInTx(ctx, tx, orderID, now.UnixNano()); err != nil { t.Fatal(err) }
	if err := tx.Commit(); err != nil { t.Fatal(err) }
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil { t.Fatal(err) }
	if ord.PINExpiresAt.Year() != 2100 { t.Fatalf("paid PIN expiry=%v, want 2100", ord.PINExpiresAt) }
	active, err := s.IsPINInUse(ctx, kioskID, "4721", now.Add(24*time.Hour))
	if err != nil || !active { t.Fatalf("paid PIN unexpectedly released: %v %v", active, err) }

	// Without verified capture, a fault must not create a monetary refund.
	if err := s.RecordRefundRequired(ctx, orderID); err != nil { t.Fatal(err) }
	empty, err := s.GetRefundRequest(ctx, orderID)
	if err != nil || empty != nil { t.Fatalf("unpaid order got refund record: %+v err=%v", empty, err) }

	_, err = s.db.ExecContext(ctx, s.rebind(`
INSERT INTO payments(payment_id, order_id, kiosk_id, session_id, amount_paise, status, provider, created_at_ns)
VALUES (?, ?, ?, '', 600, 'captured', 'mock', ?);`), "pay_fault", orderID, kioskID, now.UnixNano())
	if err != nil { t.Fatal(err) }
	if err := s.RecordRefundRequired(ctx, orderID); err != nil { t.Fatal(err) }
	if err := s.RecordRefundRequired(ctx, orderID); err != nil { t.Fatal(err) }
	r, err := s.GetRefundRequest(ctx, orderID)
	if err != nil || r == nil || r.AmountPaise != 600 || r.SafeStopVerified {
		t.Fatalf("refund must be full amount and unsafe initially: %+v %v", r, err)
	}
	if pending, err := s.ListActionableRefunds(ctx, now); err != nil || len(pending) != 0 {
		t.Fatalf("unsafe refund wrongly executable: %v %v", pending, err)
	}
	if err := s.BlockKiosk(ctx, kioskID, "paper jam"); err != nil { t.Fatal(err) }
	blocked, _, err := s.GetKioskBlock(ctx, kioskID)
	if err != nil || !blocked { t.Fatal("jam must stick until operator restoration") }
	if err := s.BlockKiosk(ctx, kioskID, "jam still active"); err != nil { t.Fatal(err) }
	blocked, _, _ = s.GetKioskBlock(ctx, kioskID)
	if !blocked { t.Fatal("healthy polling must not clear outage") }
	if err := s.ConfirmRefundSafeStop(ctx, orderID); err != nil { t.Fatal(err) }
	if actionable, err := s.ListActionableRefunds(ctx, now); err != nil || len(actionable) != 1 {
		t.Fatalf("safe stop should unlock one refund: %v %v", actionable, err)
	}
	claimed, err := s.ClaimRefund(ctx, orderID, now)
	if err != nil || !claimed { t.Fatalf("claim refund: %v %v", claimed, err) }
	claimed, err = s.ClaimRefund(ctx, orderID, now)
	if err != nil || claimed { t.Fatalf("second concurrent claim must be rejected: %v %v", claimed, err) }
	if err := s.RecordRefundResult(ctx, orderID, "rfnd_demo_1", RefundPending, ""); err != nil { t.Fatal(err) }
	if err := s.RecordRefundResult(ctx, orderID, "rfnd_demo_1", RefundProcessed, ""); err != nil { t.Fatal(err) }
	ord, err = s.GetOrder(ctx, orderID)
	if err != nil { t.Fatal(err) }
	if ord.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("refunded order not terminal: %s", ord.Status)
	}
	active, err = s.IsPINInUse(ctx, kioskID, "4721", now)
	if err != nil || active { t.Fatalf("refunded PIN must be released: %v %v", active, err) }
}

func TestFulfillment_ExpiredPINCannotResurrectOnLatePayment(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "late.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	now := time.Now().UTC()
	o := makeTestOrderWithSheets("ord_late_pin", "kiosk_late", "1800", 1, contracts.JobStatusPendingPayment, now)
	o.PINExpiresAt = now.Add(-time.Second)
	if err := s.CreateOrder(context.Background(), o); err != nil { t.Fatal(err) }
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil { t.Fatal(err) }
	if err := s.extendPaidPINInTx(context.Background(), tx, o.OrderID, now.UnixNano()); err != nil { t.Fatal(err) }
	if err := tx.Commit(); err != nil { t.Fatal(err) }
	got, err := s.GetOrder(context.Background(), o.OrderID)
	if err != nil { t.Fatal(err) }
	if got.PINExpiresAt.After(now) {
		t.Fatal("late capture resurrected an expired or possibly reused PIN")
	}
	_ = errors.New // keep the test independent of provider errors
}
