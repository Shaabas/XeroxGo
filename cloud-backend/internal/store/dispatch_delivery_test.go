package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// A write that never reached the Pi and a write that reached the Pi but lost
// its acknowledgement both retain the same durable command identity.
func TestDispatchDelivery_MultiDocumentPartialTransportFailureReplaysSafely(t *testing.T) {
	s,err:=Open(filepath.Join(t.TempDir(),"partial_delivery.db"))
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="ord_two_delivery","kiosk_two_delivery"
	docs:=make([]OrderDocument,0,3)
	for i:=0;i<3;i++{
		docs=append(docs,OrderDocument{
			JobID:fmt.Sprintf("job_delivery_%d",i),OrderID:orderID,DocumentIndex:i,
			OriginalFilename:fmt.Sprintf("file_%d.pdf",i),
			DownloadURL:fmt.Sprintf("https://storage.test/delivery_%d.pdf",i),
			SHA256Checksum:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:1,TotalSheets:1,Copies:1,Duplex:contracts.DuplexSimplex,
			ColorMode:"BW",PageRange:"",Orientation:"AUTO",PagesPerSheet:1,
			PricePaise:200,Status:contracts.JobStatusPendingPayment,
		})
	}
	o:=Order{
		OrderID:orderID,Documents:docs,TotalPages:3,TotalSheets:3,PricePaise:600,
		Status:contracts.JobStatusPendingPayment,PINExpiresAt:now.Add(5*time.Minute),
		Command:contracts.PrintJobCommand{
			JobID:docs[0].JobID,OrderID:orderID,KioskID:kioskID,
			DisplayCode:"7492",DownloadURL:docs[0].DownloadURL,
			SHA256Checksum:docs[0].SHA256Checksum,TotalPages:1,TotalSheets:1,
			Copies:1,Duplex:contracts.DuplexSimplex,StartFromPage:1,
		},
	}
	if err:=s.CreateOrder(ctx,o);err!=nil{t.Fatal(err)}
	session,err:=s.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err!=nil{t.Fatal(err)}
	paid,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_three_delivery",OrderID:orderID,KioskID:kioskID,
		SessionID:session.SessionID,AmountPaise:600,Provider:"mock",Status:"captured",
	},now)
	if err!=nil || !paid.DispatchEligible || len(paid.Commands)!=3 {
		t.Fatalf("multi-document payment must create three intents: %+v %v",paid,err)
	}

	// First write may have been accepted; second never acknowledged; third
	// was not even attempted. The system must preserve all immutable IDs.
	if err:=s.MarkDispatchIntentSent(ctx,docs[0].JobID);err!=nil{t.Fatal(err)}
	if err:=s.RecordDispatchDeliveryFailure(ctx,docs[1].JobID,"simulated broken socket");err!=nil{t.Fatal(err)}
	sum,err:=s.GetOrderDispatchSummary(ctx,orderID)
	if err!=nil||sum.TotalCount!=3||sum.SentCount!=1||sum.PendingCount!=2{
		t.Fatalf("transport error lost/changed intents: %+v %v",sum,err)
	}
	if refund,err:=s.GetRefundRequest(ctx,orderID);err!=nil||refund!=nil{
		t.Fatalf("transport error produced an unjustified refund: %+v %v",refund,err)
	}

	// A reconnect will reset unacknowledged SENT entries and resend the
	// original JobIDs; the Pi will dedupe by its own persistent store.
	if err:=s.ResetUnacknowledgedDispatchIntents(ctx,kioskID);err!=nil{t.Fatal(err)}
	cmds,err:=s.GetPendingDispatchOrders(ctx,kioskID)
	if err!=nil||len(cmds)!=3{t.Fatalf("expected three immutable jobs to reconcile: %+v %v",cmds,err)}
	for i,cmd:=range cmds{
		if cmd.JobID!=docs[i].JobID||cmd.DocumentIndex!=i||cmd.TotalDocuments!=3{
			t.Fatalf("replayed document %d changed identity: %+v",i,cmd)
		}
	}
	if err:=s.MarkDispatchIntentAcknowledged(ctx,docs[0].JobID);err!=nil{t.Fatal(err)}
	if err:=s.MarkDispatchIntentSent(ctx,docs[1].JobID);err!=nil{t.Fatal(err)}
	if err:=s.ResetUnacknowledgedDispatchIntents(ctx,kioskID);err!=nil{t.Fatal(err)}
	remaining,err:=s.GetPendingDispatchOrders(ctx,kioskID)
	if err!=nil||len(remaining)!=2{t.Fatalf("acknowledged document replayed: %+v %v",remaining,err)}
	if remaining[0].JobID!=docs[1].JobID||remaining[1].JobID!=docs[2].JobID{
		t.Fatalf("incorrect order or duplicate after reconnect: %+v",remaining)
	}
}

func TestDispatchDelivery_UseFundingPaymentDespiteLaterSurplusCapture(t *testing.T) {
	s,err:=Open(filepath.Join(t.TempDir(),"funding_reconnect.db"))
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="ord_funding_delivery","kiosk_funding_delivery"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"8253",2,contracts.JobStatusPendingPayment,now)
	session,err:=s.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err!=nil{t.Fatal(err)}
	primary,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_funding_delivery",OrderID:orderID,KioskID:kioskID,
		SessionID:session.SessionID,AmountPaise:400,Provider:"mock",Status:"captured",
	},now)
	if err!=nil||!primary.DispatchEligible{t.Fatalf("primary not authorized: %+v %v",primary,err)}
	// A real extra capture for a different amount must be refunded, but it
	// cannot invalidate the legitimate order's reconnect.
	extra,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_extra_100",OrderID:orderID,KioskID:kioskID,
		AmountPaise:100,Provider:"mock",Status:"captured",
	},now.Add(time.Second))
	if err!=nil||!extra.ExcessCapture{t.Fatalf("surplus not accounted: %+v %v",extra,err)}
	pending,err:=s.GetPendingDispatchOrders(ctx,kioskID)
	if err!=nil||len(pending)!=1||pending[0].JobID!="job_"+orderID {
		t.Fatalf("reconnect selected wrong captured payment: %+v %v",pending,err)
	}
	if refund,err:=s.GetRefundRequest(ctx,orderID);err!=nil||refund!=nil{
		t.Fatalf("excess payment refund incorrectly blocked fulfillment: %+v %v",refund,err)
	}
}

func TestDispatchDelivery_LateSessionCancelCannotErasePaidDispatchIntents(t *testing.T) {
	s,err:=Open(filepath.Join(t.TempDir(),"paid_session_cancel.db"))
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="ord_stale_cancel","kiosk_stale_cancel"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"1937",2,contracts.JobStatusPendingPayment,now)
	session,err:=s.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err!=nil{t.Fatal(err)}
	paid,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_stale_cancel",OrderID:orderID,KioskID:kioskID,
		SessionID:session.SessionID,AmountPaise:400,Provider:"mock",Status:"captured",
	},now)
	if err!=nil||!paid.DispatchEligible{t.Fatalf("paid order not authorized: %+v %v",paid,err)}
	// A delayed cancel from a prior kiosk UI timer must not cancel a session
	// already consumed by verified successful payment.
	if err:=s.CancelKioskSession(ctx,session.SessionID,kioskID,now.Add(time.Second));err!=nil{
		t.Fatalf("stale cancel failed: %v",err)
	}
	got,err:=s.GetOrder(ctx,orderID)
	if err!=nil||got.Status!=contracts.JobStatusDispatched{
		t.Fatalf("stale cancel rolled back paid authorization: %+v %v",got,err)
	}
	intents,err:=s.GetDispatchIntentsByOrder(ctx,orderID)
	if err!=nil||len(intents)!=1||intents[0].Status!="PENDING"{
		t.Fatalf("stale cancel erased pending physical job: %+v %v",intents,err)
	}
	sess,err:=s.GetKioskSession(ctx,session.SessionID,now.Add(time.Second))
	if err!=nil||sess.Status!=KioskSessionCompleted{
		t.Fatalf("paid kiosk session was canceled after completion: %+v %v",sess,err)
	}
	pending,err:=s.GetPendingDispatchOrders(ctx,kioskID)
	if err!=nil||len(pending)!=1{
		t.Fatalf("paid dispatch lost its reconnect path: %+v %v",pending,err)
	}
}

func TestDispatchDelivery_OnlyOwningKioskCanAcknowledgeJob(t *testing.T) {
	s,err:=Open(filepath.Join(t.TempDir(),"kiosk_acks.db"))
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="ord_ack_ownership","kiosk_ack_owner"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"2853",2,contracts.JobStatusPendingPayment,now)
	session,err:=s.CreateOrGetKioskSession(ctx,kioskID,orderID,5*time.Minute,now)
	if err!=nil{t.Fatal(err)}
	paid,err:=s.CapturePaymentAndCheckDispatch(ctx,PaymentRecord{
		PaymentID:"pay_ack_ownership",OrderID:orderID,KioskID:kioskID,
		SessionID:session.SessionID,AmountPaise:400,Provider:"mock",Status:"captured",
	},now)
	if err!=nil||!paid.DispatchEligible{t.Fatalf("missing authorized job: %+v %v",paid,err)}
	jobID:="job_"+orderID
	if err:=s.MarkDispatchAcknowledgedForKiosk(ctx,"kiosk_other",jobID);err!=nil{t.Fatal(err)}
	status,_,err:=s.GetJobDispatchIntentStatus(ctx,jobID)
	if err!=nil||status!="PENDING"{
		t.Fatalf("foreign kiosk ack suppressed valid replay: %q %v",status,err)
	}
	if err:=s.MarkDispatchAcknowledgedForKiosk(ctx,kioskID,jobID);err!=nil{t.Fatal(err)}
	status,_,err=s.GetJobDispatchIntentStatus(ctx,jobID)
	if err!=nil||status!="ACKNOWLEDGED"{
		t.Fatalf("owning kiosk could not ack: %q %v",status,err)
	}
}
