package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestRefundWorker_ExcessCaptureIsRefundedOnceWithoutStoppingPrinting(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "worker_extra.db"))
	if err != nil { t.Fatal(err) }
	defer st.Close()
	now := time.Now().UTC()
	const orderID, kioskID = "ord_worker_excess", "kiosk_worker_excess"
	order := store.Order{
		OrderID: orderID, Status: contracts.JobStatusPendingPayment,
		PricePaise: 200, PINExpiresAt: now.Add(5*time.Minute),
		Command: contracts.PrintJobCommand{
			JobID: "job_worker_excess", OrderID: orderID, KioskID: kioskID,
			DisplayCode: "6839", TotalPages: 1, TotalSheets: 1,
			Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		},
	}
	if err := st.CreateOrder(ctx, order); err != nil { t.Fatal(err) }
	for _, paymentID := range []string{"pay_worker_funding","pay_worker_surplus"} {
		result, err := st.CapturePaymentAndCheckDispatch(ctx, store.PaymentRecord{
			PaymentID: paymentID, OrderID: orderID, KioskID: kioskID,
			Provider: "mock", Status: "captured", AmountPaise: 200,
		}, now)
		if err != nil { t.Fatal(err) }
		if paymentID == "pay_worker_surplus" && (!result.ExcessCapture || result.DispatchEligible) {
			t.Fatalf("second capture must not dispatch: %+v",result)
		}
	}
	mock := payments.NewMockProvider("worker_test")
	app := &appServer{
		cfg: &serverConfig{webhookSecret: []byte("worker_test")},
		db: st,
		paymentProvider: mock,
	}
	if err := app.processRefundsOnce(ctx); err != nil { t.Fatal(err) }
	if mock.RefundedCount != 1 { t.Fatalf("expected one excess refund, got %d",mock.RefundedCount) }
	statusReq := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/status", nil)
	statusResponse := httptest.NewRecorder()
	app.handleOrderStatus(statusResponse, statusReq)
	if statusResponse.Code != http.StatusOK { t.Fatalf("order status HTTP %d", statusResponse.Code) }
	var state map[string]interface{}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &state); err != nil { t.Fatal(err) }
	if state["excess_refunds_total"] != float64(1) || state["excess_refunds_processed"] != float64(1) ||
		state["excess_refunds_unresolved"] != float64(0) {
		t.Fatalf("customer status omits finished surplus refund: %+v", state)
	}
	ref, err := st.GetRefundRequest(ctx, orderID)
	if err != nil || ref != nil { t.Fatalf("surplus refund should not trigger fulfillment failure: %+v %v", ref, err) }
	ord, err := st.GetOrder(ctx, orderID)
	if err != nil || ord.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("surplus refund must preserve paid entitlement: %+v %v",ord,err)
	}
	if err := app.processRefundsOnce(ctx); err != nil { t.Fatal(err) }
	if mock.RefundedCount != 1 { t.Fatalf("refund duplicated on repeated worker run: %d",mock.RefundedCount) }
}

func TestPaymentWebhook_KnownAttemptWrongAmountCreatesRefundOnly(t *testing.T) {
	app, _, st, _ := setupPaperTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const orderID, kioskID = "ord_webhook_amount", "kiosk_webhook_amount"
	order := store.Order{
		OrderID: orderID, Status: contracts.JobStatusPendingPayment,
		PricePaise: 400, PINExpiresAt: now.Add(5*time.Minute),
		Command: contracts.PrintJobCommand{
			JobID: "job_webhook_amount", OrderID: orderID, KioskID: kioskID,
			DisplayCode: "2783", TotalPages: 2, TotalSheets: 2,
			Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		},
	}
	if err := st.CreateOrder(ctx, order); err != nil { t.Fatal(err) }
	session, err := st.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err != nil { t.Fatal(err) }
	attempt := store.PaymentAttempt{
		AttemptID: "att_wrong_amount", OrderID: orderID, KioskID: kioskID,
		SessionID: session.SessionID, Provider: "mock",
		ProviderOrderID: "mock_order_wrong_amount", AmountPaise: 400,
		Currency: "INR", Status: "CREATED", ExpiresAt: now.Add(5*time.Minute),
	}
	if err := st.CreatePaymentAttempt(ctx, attempt); err != nil { t.Fatal(err) }
	raw := []byte(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_webhook_wrong_amount","order_id":"mock_order_wrong_amount","amount":100,"currency":"INR","status":"captured"}}}}`)
	mac := hmac.New(sha256.New, app.cfg.webhookSecret)
	_, _ = mac.Write(raw)
	req := httptest.NewRequest(http.MethodPost,"/api/webhooks/razorpay",bytes.NewReader(raw))
	req.Header.Set("X-Razorpay-Signature",hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	app.handleRazorpayWebhook(w,req)
	if w.Code != http.StatusOK {
		t.Fatalf("signed amount mismatch should be accounted and refunded, got %d: %s",w.Code,w.Body.String())
	}
	var status map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(),&status);err!=nil{t.Fatal(err)}
	if status["status"]!="excess_payment_refund_required"{
		t.Fatalf("wrong amount was not classified as refund-only: %+v",status)
	}
	orderAfter,err:=st.GetOrder(ctx,orderID)
	if err!=nil||orderAfter.Status!=contracts.JobStatusPendingPayment{
		t.Fatalf("wrong amount authorized printing: %+v %v",orderAfter,err)
	}
	ready,err:=st.ListActionableRefunds(ctx,now.Add(time.Hour))
	if err!=nil||len(ready)!=1||ready[0].PaymentID!="pay_webhook_wrong_amount"||ready[0].AmountPaise!=100{
		t.Fatalf("wrong amount missing from financial ledger: %+v %v",ready,err)
	}
	att,err:=st.GetPaymentAttemptByProviderOrder(ctx,"mock","mock_order_wrong_amount")
	if err!=nil||att==nil||att.Status!="CREATED"{
		t.Fatalf("underpayment wrongly marked valid checkout attempt paid: %+v %v",att,err)
	}
	// The first underpayment must not prevent a second real captured payment
	// of the correct amount from funding and dispatching the same order.
	correct:=[]byte(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_webhook_correct_amount","order_id":"mock_order_wrong_amount","amount":400,"currency":"INR","status":"captured"}}}}`)
	mac=hmac.New(sha256.New,app.cfg.webhookSecret)
	_,_=mac.Write(correct)
	req2:=httptest.NewRequest(http.MethodPost,"/api/webhooks/razorpay",bytes.NewReader(correct))
	req2.Header.Set("X-Razorpay-Signature",hex.EncodeToString(mac.Sum(nil)))
	w2:=httptest.NewRecorder()
	app.handleRazorpayWebhook(w2,req2)
	if w2.Code!=http.StatusOK || !bytes.Contains(w2.Body.Bytes(),[]byte("dispatch_pending")){
		t.Fatalf("second correctly funded payment not queued: HTTP %d %s",w2.Code,w2.Body.String())
	}
	ord,err:=st.GetOrder(ctx,orderID)
	if err!=nil||ord.Status!=contracts.JobStatusDispatched{
		t.Fatalf("correct capture did not authorize printing: %+v %v",ord,err)
	}
}

func TestRefundWorker_PaidWaitingNeverExpiresByAgeOrSession(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "paid_no_timeout.db")
	st, err := store.Open(dbPath)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	const orderID, kioskID = "ord_paid_no_timeout", "kiosk_paid_no_timeout"
	order := store.Order{
		OrderID: orderID,
		Status: contracts.JobStatusPendingPayment,
		PricePaise: 400,
		PINExpiresAt: now.Add(5 * time.Minute),
		Command: contracts.PrintJobCommand{
			JobID: "job_paid_no_timeout", OrderID: orderID, KioskID: kioskID,
			DisplayCode: "5139", TotalPages: 2, TotalSheets: 2,
			Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		},
	}
	if err := st.CreateOrder(ctx, order); err != nil { t.Fatal(err) }

	// Record a genuine captured payment from long before the worker runs.
	// There is no active kiosk session or dispatch intent. The old worker
	// would classify this otherwise healthy order as PAUSED_ERROR and refund it.
	past := now.Add(-15 * time.Minute)
	capture, err := st.CapturePaymentAndCheckDispatch(ctx, store.PaymentRecord{
		PaymentID: "pay_paid_no_timeout", OrderID: orderID, KioskID: kioskID,
		AmountPaise: 400, Provider: "mock", Status: "captured",
	}, past)
	if err != nil { t.Fatal(err) }
	if capture.DispatchEligible || capture.OrderStatus != contracts.JobStatusPaidWaiting {
		t.Fatalf("late waiting capture unexpectedly dispatched: %+v", capture)
	}
	mock := payments.NewMockProvider("paid_no_timeout")
	app := &appServer{
		cfg: &serverConfig{webhookSecret: []byte("paid_no_timeout")},
		db: st, paymentProvider: mock,
	}
	for i := 0; i < 3; i++ {
		if err := app.processRefundsOnce(ctx); err != nil { t.Fatalf("worker pass %d: %v",i,err) }
	}
	if mock.RefundedCount != 0 {
		t.Fatalf("worker refunded healthy paid order solely because of elapsed time: %d", mock.RefundedCount)
	}
	refund, err := st.GetRefundRequest(ctx, orderID)
	if err != nil || refund != nil {
		t.Fatalf("healthy paid waiting has an unexpected refund obligation: %+v %v", refund, err)
	}
	stored, err := st.GetOrder(ctx, orderID)
	if err != nil || stored.Status != contracts.JobStatusPaidWaiting || stored.PINExpiresAt.Year() != 2100 {
		t.Fatalf("worker revoked paid fulfillment/PIN: %+v %v", stored, err)
	}
	committed, err := st.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if err != nil || committed != 2 {
		t.Fatalf("worker released paid paper commitment: %d %v",committed,err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/status",nil)
	w := httptest.NewRecorder()
	app.handleOrderStatus(w,req)
	if w.Code != http.StatusOK { t.Fatalf("status request failed: %d",w.Code) }
	var state map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(),&state);err!=nil{t.Fatal(err)}
	if state["status"] != string(contracts.JobStatusPaidWaiting) {
		t.Fatalf("customer status hides recoverable paid order: %+v", state)
	}

	// Explicit failure does still enter the normal safe-stop-gated refund
	// lifecycle; a mere elapsed deadline never manufactures the obligation.
	if err := st.RecordRefundRequired(ctx,orderID); err != nil { t.Fatal(err) }
	if err := app.processRefundsOnce(ctx); err != nil { t.Fatal(err) }
	if mock.RefundedCount != 0 {
		t.Fatal("unsafe terminal refund was sent without physical safe-stop")
	}
	if err := st.ConfirmRefundSafeStop(ctx,orderID); err != nil { t.Fatal(err) }
	if err := app.processRefundsOnce(ctx); err != nil { t.Fatal(err) }
	if mock.RefundedCount != 1 {
		t.Fatalf("explicit safely stopped failure did not refund exactly once: %d",mock.RefundedCount)
	}
}
