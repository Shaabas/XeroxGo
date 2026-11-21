package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestDemoPay_UsesDurableDispatchAndDoesNotFundExcessPayment(t *testing.T) {
	const orderID,kioskID="ord_demo_financial","kiosk_demo_financial"
	app,_,_,_,st:=setupSafetyTestApp(t,kioskID,orderID,"8574",
		contracts.JobStatusPendingPayment,400,time.Now().UTC().Add(5*time.Minute))
	app.cfg.demoMode=true
	ctx:=context.Background()
	now:=time.Now().UTC()
	sess,err:=st.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err!=nil{t.Fatal(err)}
	makeAttempt:=func(id,token string){
		t.Helper()
		if err:=st.CreatePaymentAttempt(ctx,store.PaymentAttempt{
			AttemptID:id,DemoToken:token,
			OrderID:orderID,KioskID:kioskID,SessionID:sess.SessionID,
			Provider:"mock_demo",ProviderOrderID:"mock_provider_"+id,
			AmountPaise:400,Currency:"INR",Status:"CREATED",
			ExpiresAt:now.Add(5*time.Minute),
		});err!=nil{t.Fatal(err)}
	}
	pay:=func(token string)string{
		t.Helper()
		w:=httptest.NewRecorder()
		req:=httptest.NewRequest(http.MethodPost,"/pay/"+token,nil)
		req.Header.Set("Accept","application/json")
		app.handleDemoPaymentSubmit(w,req,token)
		if w.Code!=http.StatusOK{t.Fatalf("demo checkout %s HTTP %d: %s",token,w.Code,w.Body.String())}
		var result map[string]interface{}
		if err:=json.Unmarshal(w.Body.Bytes(),&result);err!=nil{t.Fatal(err)}
		status,_:=result["status"].(string)
		return status
	}
	makeAttempt("att_demo_funding","demo_first")
	if status:=pay("demo_first");status!="dispatch_pending"{
		t.Fatalf("hub-less funded demo must keep delivery pending: %s",status)
	}
	makeAttempt("att_demo_extra","demo_extra")
	if status:=pay("demo_extra");status!="excess_payment_refund_required"{
		t.Fatalf("additional money must be refund-only: %s",status)
	}
	extra,err:=st.GetPaymentAttemptByToken(ctx,"demo_extra")
	if err!=nil||extra==nil||extra.Status=="PAID"{
		t.Fatalf("refund-only demo payment mislabeled funding success: %+v %v",extra,err)
	}
	refunds,err:=st.ListActionableRefunds(ctx,now.Add(time.Hour))
	if err!=nil||len(refunds)!=1||refunds[0].Reason!=store.RefundReasonExcess||
		refunds[0].PaymentID!="pay_demo_att_demo_extra"{
		t.Fatalf("excess demo capture lost independent refund: %+v %v",refunds,err)
	}
	order,err:=st.GetOrder(ctx,orderID)
	if err!=nil||order.Status!=contracts.JobStatusDispatched{
		t.Fatalf("extra demo payment changed print entitlement: %+v %v",order,err)
	}
}
