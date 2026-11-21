package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func openPaymentLedgerTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "captures.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPaymentLedger_DistinctCapturesOnlyFundOneOrder(t *testing.T) {
	s := openPaymentLedgerTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, kiosk := "ord_one_funding", "kiosk_one_funding"
	createTestOrderWithSheets(t, s, ctx, id, kiosk, "1357", 2, contracts.JobStatusPendingPayment, now)
	session, err := s.CreateOrGetKioskSession(ctx, kiosk, id, 5*time.Minute, now)
	if err != nil { t.Fatal(err) }
	pay := func(paymentID string, when time.Time) *PaymentCaptureResult {
		t.Helper()
		result, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
			PaymentID: paymentID, OrderID: id, KioskID: kiosk,
			SessionID: session.SessionID, AmountPaise: 400, Provider: "mock", Status: "captured",
		}, when)
		if err != nil { t.Fatal(err) }
		return result
	}
	first := pay("pay_funding_1", now)
	if !first.DispatchEligible || first.AlreadyProcessed || first.ExcessCapture {
		t.Fatalf("first payment must authorize exactly one dispatch: %+v", first)
	}
	second := pay("pay_surplus_2", now.Add(time.Millisecond))
	if second.DispatchEligible || !second.ExcessCapture {
		t.Fatalf("second distinct capture must not dispatch: %+v", second)
	}
	replay := pay("pay_surplus_2", now.Add(time.Second))
	if replay.ExcessCapture || !replay.AlreadyProcessed || replay.DispatchEligible {
		t.Fatalf("duplicate notification must not create new refund: %+v", replay)
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM payment_refund_requests WHERE order_id = ?", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("want exactly one surplus refund, got %d: %v", count, err)
	}
	if primary, err := s.GetRefundRequest(ctx, id); err != nil || primary != nil {
		t.Fatalf("excess refund must not block printing: %+v %v", primary, err)
	}
	summary, err := s.GetExcessRefundSummary(ctx, id)
	if err != nil || summary.Total != 1 || summary.Unresolved != 1 || summary.Processed != 0 {
		t.Fatalf("new surplus refund summary incorrect: %+v %v", summary, err)
	}
	ready, err := s.ListActionableRefunds(ctx, now.Add(time.Hour))
	if err != nil || len(ready) != 1 || ready[0].PaymentID != "pay_surplus_2" ||
		ready[0].Reason != RefundReasonExcess || !ready[0].SafeStopVerified {
		t.Fatalf("excess refund should be immediately actionable: %+v %v", ready, err)
	}
	claimed, err := s.ClaimRefundPayment(ctx, "pay_surplus_2", now.Add(time.Hour))
	if err != nil || !claimed { t.Fatalf("claim surplus refund: %v %v", claimed, err) }
	if err := s.RecordRefundResultForPayment(ctx, "pay_surplus_2", "rfnd_surplus", RefundProcessed, ""); err != nil {
		t.Fatal(err)
	}
	summary, err = s.GetExcessRefundSummary(ctx, id)
	if err != nil || summary.Processed != 1 || summary.Unresolved != 0 {
		t.Fatalf("processed refund summary incorrect: %+v %v", summary, err)
	}
	ord, err := s.GetOrder(ctx, id)
	if err != nil || ord.Status != contracts.JobStatusDispatched {
		t.Fatalf("refund of surplus must preserve legitimate printing: %+v %v", ord, err)
	}
	inUse, err := s.IsPINInUse(ctx, kiosk, "1357", now)
	if err != nil || !inUse { t.Fatal("surplus refund revoked the authorized PIN") }

	if err := s.UpdateOrderStatus(ctx, id, contracts.JobStatusCompleted); err != nil { t.Fatal(err) }
	third := pay("pay_surplus_after_complete_3", now.Add(2*time.Second))
	if !third.ExcessCapture || third.DispatchEligible { t.Fatalf("late capture must be refunded: %+v", third) }
	unexpired, err := s.GetExpiredOrderIDs(ctx, now.Add(time.Hour), now.Add(time.Hour))
	if err != nil { t.Fatal(err) }
	for _, x := range unexpired {
		if x == id { t.Fatal("cleanup would delete completed order with unresolved surplus money") }
	}
}

func TestPaymentLedger_ConcurrentDistinctCapturesAndNoDoubleDispatch(t *testing.T) {
	s := openPaymentLedgerTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, kiosk := "ord_concurrent_captures", "kiosk_concurrent_captures"
	createTestOrderWithSheets(t, s, ctx, id, kiosk, "2468", 3, contracts.JobStatusPendingPayment, now)
	session, err := s.CreateOrGetKioskSession(ctx, kiosk, id, 5*time.Minute, now)
	if err != nil { t.Fatal(err) }
	const total = 8
	results := make([]*PaymentCaptureResult, total)
	errs := make([]error, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
				PaymentID: fmt.Sprintf("pay_racing_%02d", i), OrderID: id, KioskID: kiosk,
				SessionID: session.SessionID, AmountPaise: 600, Provider: "mock", Status: "captured",
			}, now)
		}(i)
	}
	wg.Wait()
	var dispatched, extras int
	for i, result := range results {
		if errs[i] != nil { t.Fatalf("capture %d: %v", i, errs[i]) }
		if result.DispatchEligible { dispatched++ }
		if result.ExcessCapture { extras++ }
	}
	if dispatched != 1 || extras != total-1 {
		t.Fatalf("expected one authorized dispatch and %d independent refunds, got dispatch=%d surplus=%d",
			total-1, dispatched, extras)
	}
	var funded, refunded int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM order_funding_payments WHERE order_id = ?", id).Scan(&funded); err != nil { t.Fatal(err) }
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM payment_refund_requests WHERE order_id = ? AND reason = 'EXCESS_CAPTURE'", id).Scan(&refunded); err != nil { t.Fatal(err) }
	if funded != 1 || refunded != total-1 {
		t.Fatalf("ledger accounting mismatch: funding=%d surplus=%d", funded, refunded)
	}
}

func TestPaymentLedger_PrimaryFailureRequiresStopButExcessDoesNot(t *testing.T) {
	s := openPaymentLedgerTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, kiosk := "ord_two_refunds", "kiosk_two_refunds"
	createTestOrderWithSheets(t, s, ctx, id, kiosk, "3981", 2, contracts.JobStatusPendingPayment, now)
	for i := 0; i < 2; i++ {
		_, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
			PaymentID: fmt.Sprintf("pay_fail_%d", i), OrderID: id, KioskID: kiosk,
			AmountPaise: 400, Provider: "mock", Status: "captured",
		}, now.Add(time.Duration(i)*time.Second))
		if err != nil { t.Fatal(err) }
	}
	if err := s.RecordRefundRequired(ctx, id); err != nil { t.Fatal(err) }
	ready, err := s.ListActionableRefunds(ctx, now.Add(time.Hour))
	if err != nil || len(ready) != 1 || ready[0].Reason != RefundReasonExcess {
		t.Fatalf("unsafe fulfillment payment must not refund yet: %+v %v", ready, err)
	}
	if err := s.ConfirmRefundSafeStop(ctx, id); err != nil { t.Fatal(err) }
	ready, err = s.ListActionableRefunds(ctx, now.Add(time.Hour))
	if err != nil || len(ready) != 2 { t.Fatalf("safe stop must make both captures refundable: %+v %v", ready, err) }
	seen := make(map[string]bool)
	for _, item := range ready {
		if seen[item.IdempotencyKey] { t.Fatal("two monetary refunds share an idempotency key") }
		seen[item.IdempotencyKey] = true
	}
}

func TestPaymentLedger_LegacyPendingRefundMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(dbPath)
	if err != nil { t.Fatal(err) }
	ctx := context.Background()
	now := time.Now().UTC()
	id, kiosk := "ord_legacy_payment", "kiosk_legacy_payment"
	createTestOrderWithSheets(t, s, ctx, id, kiosk, "9624", 2, contracts.JobStatusPaidWaiting, now)
	for i := 0; i < 2; i++ {
		_, err := s.db.ExecContext(ctx, s.rebind(
			"INSERT INTO payments(payment_id,order_id,kiosk_id,session_id,amount_paise,status,provider,created_at_ns) VALUES(?,?,?,'',400,'captured','mock',?)"),
			fmt.Sprintf("pay_legacy_%d", i), id, kiosk, now.Add(time.Duration(i)*time.Second).UnixNano())
		if err != nil { t.Fatal(err) }
	}
	_, err = s.db.ExecContext(ctx, s.rebind(
		"INSERT INTO refund_requests(order_id,payment_id,provider,amount_paise,status,idempotency_key,provider_refund_id,safe_stop_verified,created_at_ns,updated_at_ns) VALUES(?,?,'mock',400,'PENDING',?,'rfnd_existing',1,?,?)"),
		id, "pay_legacy_0", "xeroxgo_refund_"+id, now.UnixNano(), now.UnixNano())
	if err != nil { t.Fatal(err) }
	if err := s.Close(); err != nil { t.Fatal(err) }
	s, err = Open(dbPath)
	if err != nil { t.Fatal(err) }
	defer s.Close()
	primary, err := s.GetRefundRequest(ctx, id)
	if err != nil || primary == nil || primary.PaymentID != "pay_legacy_0" ||
		primary.ProviderRefundID != "rfnd_existing" || primary.Status != RefundPending {
		t.Fatalf("migration lost existing provider refund identity: %+v %v", primary, err)
	}
	// Historic surplus money might already have been refunded manually.
	// Startup must never automatically submit a second provider refund.
	ready, err := s.ListActionableRefunds(ctx, now.Add(time.Hour))
	if err != nil || len(ready) != 0 {
		t.Fatalf("historic excess capture was automatically refundable: %+v %v", ready, err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx,
		"SELECT status FROM payment_refund_requests WHERE payment_id=?",
		"pay_legacy_1").Scan(&status); err != nil || status != "RECONCILIATION_REQUIRED" {
		t.Fatalf("historic surplus lost safe manual reconciliation state: %q %v", status, err)
	}
	reviews, err := s.ListHistoricalCaptureReviews(ctx)
	if err != nil || len(reviews) != 1 ||
		reviews[0].PaymentID != "pay_legacy_1" || reviews[0].AmountPaise != 400 {
		t.Fatalf("historic surplus not visible for operator reconciliation: %+v %v", reviews, err)
	}
}

func TestPaymentLedger_WrongAmountCaptureRefundedNotApplied(t *testing.T) {
	s := openPaymentLedgerTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, kiosk := "ord_wrong_amount", "kiosk_wrong_amount"
	createTestOrderWithSheets(t, s, ctx, id, kiosk, "6583", 2, contracts.JobStatusPendingPayment, now)
	wrong, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay_wrong_amount", OrderID: id, KioskID: kiosk,
		AmountPaise: 100, Provider: "mock", Status: "captured",
	}, now)
	if err != nil || wrong == nil || !wrong.ExcessCapture || wrong.DispatchEligible {
		t.Fatalf("wrong captured amount must be refund-only: %+v %v",wrong,err)
	}
	var funded int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM order_funding_payments WHERE order_id=?",id).Scan(&funded); err != nil || funded!=0 {
		t.Fatalf("wrong amount claimed funding: %d %v",funded,err)
	}
	correct, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay_correct_amount", OrderID: id, KioskID: kiosk,
		AmountPaise: 400, Provider: "mock", Status: "captured",
	}, now.Add(time.Second))
	if err != nil || correct == nil || correct.ExcessCapture {
		t.Fatalf("later exact payment must be legitimate funding: %+v %v",correct,err)
	}
	ready, err := s.ListActionableRefunds(ctx,now.Add(time.Hour))
	if err != nil || len(ready)!=1 || ready[0].PaymentID!="pay_wrong_amount" || ready[0].AmountPaise!=100 {
		t.Fatalf("wrong amount should be independently refunded in full: %+v %v",ready,err)
	}
}


func TestPaymentLedger_RestartBackfillsWrongAmountWithoutFundingOrder(t *testing.T) {
	dbPath:=filepath.Join(t.TempDir(),"wrong_amount_restart.db")
	s,err:=Open(dbPath)
	if err!=nil{t.Fatal(err)}
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="ord_restart_wrong_amount","kiosk_restart_wrong_amount"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"3295",2,contracts.JobStatusPendingPayment,now)
	_,err=s.db.ExecContext(ctx,s.rebind(`
INSERT INTO payments(payment_id,order_id,kiosk_id,session_id,amount_paise,status,provider,created_at_ns)
VALUES(?,?,?,'',100,'captured','mock',?);`),
		"pay_wrong_restart",orderID,kioskID,now.UnixNano())
	if err!=nil{t.Fatal(err)}
	if err:=s.Close();err!=nil{t.Fatal(err)}
	s,err=Open(dbPath)
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	var funded int
	if err:=s.db.QueryRowContext(ctx,"SELECT COUNT(*) FROM order_funding_payments WHERE order_id=?",orderID).Scan(&funded);err!=nil{
		t.Fatal(err)
	}
	if funded!=0{t.Fatal("restart wrongly turned underpayment into print authorization")}
	ready,err:=s.ListActionableRefunds(ctx,now.Add(time.Hour))
	if err!=nil||len(ready)!=0{
		t.Fatalf("historic underpayment needs provider reconciliation, not automatic refund: %+v %v",ready,err)
	}
	var historicStatus string
	if err:=s.db.QueryRowContext(ctx,
		"SELECT status FROM payment_refund_requests WHERE payment_id=?",
		"pay_wrong_restart").Scan(&historicStatus);err!=nil||historicStatus!="RECONCILIATION_REQUIRED"{
		t.Fatalf("historic underpayment not queued for manual reconciliation: %q %v",historicStatus,err)
	}
	exact,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_correct_restart",OrderID:orderID,KioskID:kioskID,
		AmountPaise:400,Status:"captured",Provider:"mock",
	},now.Add(time.Second))
	if err!=nil||exact.ExcessCapture||exact.OrderStatus!=contracts.JobStatusPaidWaiting{
		t.Fatalf("later legitimate payment not accepted: %+v %v",exact,err)
	}
}
