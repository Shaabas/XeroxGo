package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestFulfillment_NeverDispatchedPaymentAutoRefundGate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "never_dispatched.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk_no_dispatch"
	orderID := "ord_no_dispatch"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"7481",2,contracts.JobStatusPaidWaiting,now)
	_, err = s.db.ExecContext(ctx, s.rebind(`
INSERT INTO payments(payment_id, order_id, kiosk_id, session_id, amount_paise, status, provider, created_at_ns)
VALUES(?, ?, ?, '', 400, 'captured', 'mock', ?)`), "pay_no_dispatch", orderID, kioskID, now.Add(-3*time.Minute).UnixNano())
	if err != nil { t.Fatal(err) }
	candidates, err := s.ListPaidWithoutDispatch(ctx, now.Add(-2*time.Minute), now)
	if err != nil || len(candidates)!=1 || candidates[0]!=orderID {
		t.Fatalf("expected unfulfilled paid order: %v / %v",candidates,err)
	}
	ok,err := s.FailNeverDispatchedPaidOrder(ctx,orderID,now)
	if err != nil || !ok { t.Fatalf("cannot mark never-dispatched paid refund: %v %v",ok,err) }
	again,err := s.FailNeverDispatchedPaidOrder(ctx,orderID,now)
	if err != nil || again { t.Fatalf("duplicate transition must not succeed: %v %v",again,err) }
	ref,err := s.GetRefundRequest(ctx,orderID)
	if err != nil || ref == nil || ref.Status!=RefundRequired || !ref.SafeStopVerified || ref.AmountPaise!=400 {
		t.Fatalf("expected exactly one safe, full refund: %+v / %v",ref,err)
	}
	ord,err := s.GetOrder(ctx,orderID)
	if err != nil || ord.Status!=contracts.JobStatusPausedError { t.Fatalf("expected order paused for refund: %+v %v",ord,err) }
}

func TestFulfillment_PaidWithDispatchNeverAutoSafeRefund(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "already_sent.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID,orderID:="kiosk_sent","ord_sent"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"7482",2,contracts.JobStatusPaidWaiting,now)
	_,err=s.db.ExecContext(ctx,s.rebind(`
INSERT INTO payments(payment_id,order_id,kiosk_id,session_id,amount_paise,status,provider,created_at_ns)
VALUES (?, ?, ?, '',400,'captured','mock', ?)`),"pay_sent",orderID,kioskID,now.Add(-3*time.Minute).UnixNano())
	if err!=nil {t.Fatal(err)}
	_,err=s.db.ExecContext(ctx,s.rebind(`
INSERT INTO dispatch_intents(job_id,order_id,kiosk_id,session_id,status,created_at_ns,updated_at_ns)
VALUES (?, ?, ?, '', 'SENT', ?, ?)`),"job_"+orderID,orderID,kioskID,now.UnixNano(),now.UnixNano())
	if err!=nil {t.Fatal(err)}
	ids,err:=s.ListPaidWithoutDispatch(ctx,now.Add(-2*time.Minute),now)
	if err!=nil || len(ids)!=0 {t.Fatalf("sent job must never be auto-refund-safe: %v %v",ids,err)}
	ok,err:=s.FailNeverDispatchedPaidOrder(ctx,orderID,now)
	if err!=nil || ok {t.Fatalf("cannot mark sent job safe without CUPS proof: %v %v",ok,err)}
}
