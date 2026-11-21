package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// A paid order without a dispatch intent is waiting for the customer, not
// necessarily failed. The old worker incorrectly refunded it after two minutes.
func TestFulfillment_PaidWaitingRetainedAndReclaimableAfterSessionExpires(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "paid_reclaim.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	const kioskID, orderID = "kiosk_paid_reclaim", "ord_paid_reclaim"
	order := makeTestOrderWithSheets(orderID, kioskID, "7481", 3, contracts.JobStatusPendingPayment, now)
	order.PINExpiresAt = now.Add(5 * time.Minute)
	if err := s.CreateOrder(ctx, order); err != nil { t.Fatal(err) }
	oldSession, err := s.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, now)
	if err != nil { t.Fatal(err) }

	// A verified payment with no matching release session must remain recoverable.
	res, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay_paid_reclaim", OrderID: orderID, KioskID: kioskID,
		SessionID: "", AmountPaise: 600, Provider: "mock", Status: "captured",
	}, now)
	if err != nil { t.Fatal(err) }
	if res.DispatchEligible || res.OrderStatus != contracts.JobStatusPaidWaiting {
		t.Fatalf("payment must be retained without unattended printing: %+v", res)
	}

	later := now.Add(20 * time.Minute) // Past both the old two-minute rule and unpaid deadline.
	got, err := s.GetOrder(ctx, orderID)
	if err != nil || got.Status != contracts.JobStatusPaidWaiting || got.PINExpiresAt.Year() != 2100 {
		t.Fatalf("paid order lost its entitlement or valid PIN: %+v %v", got, err)
	}
	if r, err := s.GetRefundRequest(ctx, orderID); err != nil || r != nil {
		t.Fatalf("mere wait must not create fulfillment refund: %+v %v", r, err)
	}
	expired, err := s.GetExpiredOrderIDs(ctx, later, later)
	if err != nil { t.Fatal(err) }
	for _, id := range expired {
		if id == orderID { t.Fatal("retention cleanup would delete unpaid-for printing after verified capture") }
	}
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", later)
	if err != nil || committed != 3 {
		t.Fatalf("paid waiting must keep its reserved paper: committed=%d err=%v", committed, err)
	}

	newSession, err := s.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, later)
	if err != nil { t.Fatalf("paid customer could not reclaim after session expiration: %v", err) }
	if newSession.SessionID == oldSession.SessionID {
		t.Fatal("expired session was incorrectly reused")
	}
	if _, err := s.ReleasePaidOrder(ctx, kioskID, orderID, newSession.SessionID, later); err != nil {
		t.Fatalf("paid customer could not authorize their first print: %v", err)
	}
	got, err = s.GetOrder(ctx, orderID)
	if err != nil || got.Status != contracts.JobStatusDispatched {
		t.Fatalf("paid order not correctly released: %+v %v", got, err)
	}
}

// A capture arriving after the original PIN's expiry cannot revive that PIN.
// The customer's secret recovery credential can rotate to a paid PIN instead.
func TestFulfillment_LateCaptureCanRecoverWithoutAnotherPaymentOrTimeout(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "late_recovery.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	const orderID, kioskID = "ord_late_recovery", "kiosk_late_recovery"
	order := makeTestOrderWithSheets(orderID, kioskID, "3851", 2, contracts.JobStatusPendingPayment, now)
	order.PINExpiresAt = now.Add(-time.Second)
	order.ClaimToken = "only_test_recovery_credential"
	if err := s.CreateOrder(ctx, order); err != nil { t.Fatal(err) }
	res, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay_late_recovery", OrderID: orderID, KioskID: kioskID,
		AmountPaise: 400, Provider: "mock", Status: "captured",
	}, now)
	if err != nil { t.Fatal(err) }
	if res.DispatchEligible || res.OrderStatus != contracts.JobStatusPaidWaiting {
		t.Fatalf("late capture cannot authorize unattended print: %+v", res)
	}
	current, err := s.GetOrder(ctx, orderID)
	if err != nil || current.PINExpiresAt.After(now) {
		t.Fatalf("late capture revived expired PIN: %+v %v", current, err)
	}

	if err := s.RenewOrderPIN(ctx, orderID, "9237", "wrong_token", PaidPINExpiry()); err != ErrUnauthorized {
		t.Fatalf("wrong recovery credential accepted: %v", err)
	}
	if err := s.RenewOrderPIN(ctx, orderID, "9237", order.ClaimToken, PaidPINExpiry()); err != nil {
		t.Fatalf("paid order cannot rotate PIN: %v", err)
	}
	current, err = s.GetOrder(ctx, orderID)
	if err != nil || current.PINExpiresAt.Year() != 2100 || current.Command.DisplayCode != "9237" {
		t.Fatalf("recovered PIN must not have unpaid expiry: %+v %v", current, err)
	}
	if used, err := s.IsPINInUse(ctx, kioskID, "3851", now.Add(time.Hour)); err != nil || used {
		t.Fatalf("expired old PIN was revived: %v %v", used, err)
	}
	if used, err := s.IsPINInUse(ctx, kioskID, "9237", now.Add(time.Hour)); err != nil || !used {
		t.Fatalf("rotated paid PIN not reserved: %v %v", used, err)
	}
	if refund, err := s.GetRefundRequest(ctx, orderID); err != nil || refund != nil {
		t.Fatalf("late captured payment incorrectly marked for refund: %+v %v", refund, err)
	}
	later := now.Add(40 * time.Minute)
	session, err := s.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, later)
	if err != nil { t.Fatalf("recovered paid PIN cannot be claimed after former timeout: %v", err) }
	if _, err := s.ReleasePaidOrder(ctx, kioskID, orderID, session.SessionID, later); err != nil {
		t.Fatalf("recovered paid order did not dispatch: %v", err)
	}
}
