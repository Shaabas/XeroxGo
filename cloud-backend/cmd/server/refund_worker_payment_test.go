package main

import (
	"context"
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
