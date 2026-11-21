package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

// A successful capture followed by a WebSocket outage must retain the payment,
// paper reservation and all dispatch intents. A reconnect must replay the same
// immutable JobID and stop replaying after the Pi durably acknowledges it.
func TestDispatchDelivery_OfflinePaymentReconnectsWithoutRefundOrDoublePrint(t *testing.T) {
	const kioskID, orderID = "kiosk_dispatch_outage", "ord_dispatch_outage"
	app, kioskSecret, webhookSecret, srv, st := setupSafetyTestApp(t,
		kioskID, orderID, "6392", contracts.JobStatusPendingPayment, 400,
		time.Now().UTC().Add(5*time.Minute))
	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kioskSecret))
	app.hub = hub
	now := time.Now().UTC()
	session, err := st.CreateOrGetKioskSession(context.Background(), kioskID, orderID, 5*time.Minute, now)
	if err != nil { t.Fatal(err) }

	body, signature := generateSignedWebhookPayload(orderID, "pay_dispatch_outage", session.SessionID, kioskID, 400, webhookSecret)
	sendWebhook := func() string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/webhooks/razorpay", bytes.NewReader(body))
		if err != nil { t.Fatal(err) }
		req.Header.Set("X-Razorpay-Signature", signature)
		resp, err := http.DefaultClient.Do(req)
		if err != nil { t.Fatal(err) }
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK { t.Fatalf("webhook HTTP %d",resp.StatusCode) }
		var data map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil { t.Fatal(err) }
		return data["status"]
	}
	if got := sendWebhook(); got != "dispatch_pending" {
		t.Fatalf("offline payment must be preserved for reconnect, got %q",got)
	}
	ord, err := st.GetOrder(context.Background(),orderID)
	if err != nil || ord.Status != contracts.JobStatusDispatched {
		t.Fatalf("payment authorization lost: %+v %v",ord,err)
	}
	if blocked, _, err := st.GetKioskBlock(context.Background(),kioskID); err != nil || blocked {
		t.Fatalf("transport outage must not mark kiosk faulty: blocked=%v %v",blocked,err)
	}
	if refund, err := st.GetRefundRequest(context.Background(),orderID); err != nil || refund != nil {
		t.Fatalf("transport outage must not create refund: %+v %v",refund,err)
	}
	intents, err := st.GetDispatchIntentsByOrder(context.Background(),orderID)
	if err != nil || len(intents)!=1 || intents[0].Status!="PENDING" {
		t.Fatalf("durable intent was removed before reconnect: %+v %v",intents,err)
	}
	pendingStatus := statusForOrder(t,app,orderID)
	if pendingStatus.Status != "DISPATCHED" || !pendingStatus.DispatchPending ||
		pendingStatus.AwaitingDelivery != 1 {
		t.Fatalf("paid offline order must advertise queued delivery, not printing: %+v",pendingStatus)
	}
	if got := sendWebhook(); got != "already_processed" {
		t.Fatalf("duplicate provider notification must not send again: %q",got)
	}
	if next, err := st.GetPendingDispatchOrders(context.Background(),kioskID); err != nil || len(next)!=1 {
		t.Fatalf("offline order not recoverable: %+v %v",next,err)
	}

	u, _ := url.Parse(srv.URL)
	u.Scheme="ws"; u.Path="/ws"
	q:=u.Query();q.Set("kiosk_id",kioskID);u.RawQuery=q.Encode()
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
	defer cancel()
	conn,_,err:=websocket.Dial(ctx,u.String(),&websocket.DialOptions{HTTPHeader:http.Header{
		"Authorization": []string{"Bearer "+kioskSecret},
	}})
	if err != nil { t.Fatal(err) }
	defer conn.CloseNow()

	var received wireMsg
	for {
		if err := wsjson.Read(ctx,conn,&received);err!=nil{t.Fatalf("reconnect delivery not received: %v",err)}
		if received.Type!="sync_ads"{break}
	}
	if received.Type!="job" || received.Job==nil ||
		received.Job.JobID!="job_"+orderID || received.Job.OrderID!=orderID{
		t.Fatalf("reconnect must deliver same immutable job: %+v",received)
	}
	ack:=wireMsg{Type:"job_ack",Progress:&contracts.JobProgressEvent{
		JobID:received.Job.JobID,KioskID:kioskID,Status:contracts.JobStatusSpooling,
		PagesPrinted:0,SheetsPrinted:0,Timestamp:time.Now().UTC(),
	}}
	if err:=wsjson.Write(ctx,conn,ack);err!=nil{t.Fatal(err)}
	deadline:=time.Now().Add(2*time.Second)
	var status string
	for time.Now().Before(deadline) {
		status,_,err=st.GetJobDispatchIntentStatus(context.Background(),received.Job.JobID)
		if err==nil && status=="ACKNOWLEDGED"{break}
		time.Sleep(10*time.Millisecond)
	}
	if status!="ACKNOWLEDGED"{t.Fatalf("durable Pi ack not received: %q %v",status,err)}
	deliveredStatus := statusForOrder(t,app,orderID)
	if deliveredStatus.DispatchPending || deliveredStatus.AwaitingDelivery != 0 {
		t.Fatalf("acknowledged Pi delivery still shown as queued: %+v",deliveredStatus)
	}
	pending,err:=st.GetPendingDispatchOrders(context.Background(),kioskID)
	if err!=nil || len(pending)!=0 {
		t.Fatalf("acknowledged job would be redundantly sent: %+v %v",pending,err)
	}
	if refund, err := st.GetRefundRequest(context.Background(),orderID); err != nil || refund != nil {
		t.Fatalf("reconnection incorrectly created refund: %+v %v",refund,err)
	}
}

func TestDispatchDelivery_ReleaseWithUnavailableHubReturnsAcceptedPending(t *testing.T) {
	const kioskID,orderID="kiosk_release_pending","ord_release_pending"
	app,kioskSecret,_,srv,st:=setupSafetyTestApp(t,
		kioskID,orderID,"7194",contracts.JobStatusPendingPayment,400,
		time.Now().UTC().Add(5*time.Minute))
	capture,err:=st.CapturePaymentAndCheckDispatch(context.Background(),store.PaymentRecord{
		PaymentID:"pay_release_pending",OrderID:orderID,KioskID:kioskID,
		AmountPaise:400,Provider:"mock",Status:"captured",
	},time.Now().UTC())
	if err!=nil || capture.OrderStatus!=contracts.JobStatusPaidWaiting{
		t.Fatalf("paid waiting not prepared: %+v %v",capture,err)
	}
	session,err:=st.CreateOrGetKioskSession(context.Background(),kioskID,orderID,5*time.Minute,time.Now().UTC())
	if err!=nil{t.Fatal(err)}
	payload,_:=json.Marshal(map[string]string{"session_id":session.SessionID})
	req,err:=http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/k/%s/orders/%s/release",srv.URL,kioskID,orderID),bytes.NewReader(payload))
	if err!=nil{t.Fatal(err)}
	req.Header.Set("Authorization","Bearer "+kioskSecret)
	resp,err:=http.DefaultClient.Do(req)
	if err!=nil{t.Fatal(err)}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{t.Fatal(err)}
	if resp.StatusCode!=http.StatusAccepted || data["status"]!="DISPATCH_PENDING"{
		t.Fatalf("offline release must be accepted and queued: HTTP %d %+v",resp.StatusCode,data)
	}
	ord,err:=st.GetOrder(context.Background(),orderID)
	if err!=nil || ord.Status!=contracts.JobStatusDispatched {
		t.Fatalf("paid entitlement not preserved: %+v %v",ord,err)
	}
	list,err:=st.GetPendingDispatchOrders(context.Background(),kioskID)
	if err!=nil || len(list)!=1 {
		t.Fatalf("paid release lost its durable dispatch intent: %+v %v",list,err)
	}
	if refund,err:=st.GetRefundRequest(context.Background(),orderID);err!=nil||refund!=nil {
		t.Fatalf("offline release fabricated refund: %+v %v",refund,err)
	}
	_ = app
}
