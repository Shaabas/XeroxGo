package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPackage6B_CloudOrderPauseFailAndDispatchCancellation(t *testing.T) {
	ctx := context.Background()
	st, cleanup := openConcurrencyTestStore(t, "pkg6b_cloud")
	defer cleanup()

	now := time.Now().UTC()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	orderID := "ord-6b-" + suffix
	kioskID := "kiosk-6b-" + suffix

	docs := []OrderDocument{
		{JobID: orderID + "-0", OrderID: orderID, DocumentIndex: 0, OriginalFilename: "d0.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000"},
		{JobID: orderID + "-1", OrderID: orderID, DocumentIndex: 1, OriginalFilename: "d1.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/1.pdf", SHA256Checksum: "1111"},
		{JobID: orderID + "-2", OrderID: orderID, DocumentIndex: 2, OriginalFilename: "d2.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/2.pdf", SHA256Checksum: "2222"},
	}
	cmd := contracts.PrintJobCommand{
		JobID:          orderID,
		OrderID:        orderID,
		TotalDocuments: 3,
		KioskID:        kioskID,
		DisplayCode:    "9012",
		DownloadURL:    "http://localhost/0.pdf",
		SHA256Checksum: "0000",
		TotalPages:     6,
		TotalSheets:    6,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.CreateOrder(ctx, Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   1200,
		TotalPages:   6,
		TotalSheets:  6,
		Documents:    docs,
		PINExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, now)
	if err != nil || sess == nil {
		t.Fatalf("CreateOrGetKioskSession: %v", err)
	}

	capRes, err := st.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID:   "pay_" + suffix,
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		Provider:    "razorpay",
		AmountPaise: 1200,
		Status:      "captured",
	}, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch: %v", err)
	}
	if !capRes.DispatchEligible || len(capRes.Commands) != 3 {
		t.Fatalf("expected 3 dispatch commands, got eligible=%v len=%d", capRes.DispatchEligible, len(capRes.Commands))
	}

	// Doc 0 completes
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[0].JobID, KioskID: kioskID, Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 0: %v", err)
	}

	// Doc 1 pauses with hardware error
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusPausedError,
		PagesPrinted: 1, SheetsPrinted: 1, ErrorReason: "paper jam", Timestamp: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 1 PAUSED_ERROR: %v", err)
	}

	ord, err := st.GetOrder(ctx, orderID)
	if err != nil || ord == nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected order status PAUSED_ERROR, got %s", ord.Status)
	}

	// Pending dispatch intents for paused order must be canceled, reject MarkDispatchIntentSent, and never be returned
	if err := st.MarkDispatchIntentSent(ctx, docs[2].JobID); err == nil {
		t.Fatalf("expected MarkDispatchIntentSent on paused order to fail")
	}
	if err := st.ResetUnacknowledgedDispatchIntents(ctx, kioskID); err != nil {
		t.Fatalf("ResetUnacknowledgedDispatchIntents: %v", err)
	}
	pending, err := st.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders: %v", err)
	}
	for _, p := range pending {
		if p.OrderID == orderID {
			t.Fatalf("paused order %s must never be returned by GetPendingDispatchOrders", orderID)
		}
	}

	// Finalize doc 1 and doc 2 as FAILED_REFUNDABLE
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
		PagesPrinted: 1, SheetsPrinted: 1, ErrorReason: "canceled", Timestamp: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 1 FAILED_REFUNDABLE: %v", err)
	}
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[2].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
		PagesPrinted: 0, SheetsPrinted: 0, ErrorReason: "order stopped", Timestamp: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 2 FAILED_REFUNDABLE: %v", err)
	}

	ord, err = st.GetOrder(ctx, orderID)
	if err != nil || ord == nil {
		t.Fatalf("GetOrder after fail: %v", err)
	}
	if ord.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected order status FAILED_REFUNDABLE, got %s", ord.Status)
	}

	// Terminal FAILED_REFUNDABLE immutability
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusPausedError,
		PagesPrinted: 1, SheetsPrinted: 1, ErrorReason: "regress", Timestamp: now.Add(4 * time.Second),
	}); err == nil {
		t.Fatalf("expected FAILED_REFUNDABLE -> PAUSED_ERROR regression to be rejected")
	}

	// Concurrent progress updates + dispatch checks against same store (exercises PostgreSQL under XEROXGO_TEST_POSTGRES_URL)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = st.GetPendingDispatchOrders(ctx, kioskID)
			_ = st.ResetUnacknowledgedDispatchIntents(ctx, kioskID)
			_ = st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
				JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusPrinting,
				PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(5 * time.Second),
			})
		}()
	}
	wg.Wait()
}

func TestPackage6B_CloudAggregationUnresolvedSiblingsAndRetention(t *testing.T) {
	ctx := context.Background()
	st, cleanup := openConcurrencyTestStore(t, "pkg6b_unres")
	defer cleanup()

	now := time.Now().UTC()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	orderID := "ord-unres-" + suffix
	kioskID := "kiosk-unres-" + suffix

	docs := []OrderDocument{
		{JobID: orderID + "-0", OrderID: orderID, DocumentIndex: 0, OriginalFilename: "d0.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000"},
		{JobID: orderID + "-1", OrderID: orderID, DocumentIndex: 1, OriginalFilename: "d1.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/1.pdf", SHA256Checksum: "1111"},
	}
	cmd := contracts.PrintJobCommand{
		JobID: orderID, OrderID: orderID, TotalDocuments: 2, KioskID: kioskID, DisplayCode: "7788",
		DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000", TotalPages: 4, TotalSheets: 4,
		Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.CreateOrder(ctx, Order{
		OrderID: orderID, Command: cmd, Status: contracts.JobStatusPendingPayment,
		PricePaise: 800, TotalPages: 4, TotalSheets: 4, Documents: docs, PINExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSession: %v", err)
	}
	if _, err := st.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay_" + suffix, OrderID: orderID, KioskID: kioskID, SessionID: sess.SessionID,
		Provider: "razorpay", AmountPaise: 800, Status: "captured",
	}, now); err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch: %v", err)
	}

	// Acknowledge doc 0
	if err := st.MarkDispatchIntentAcknowledgedForKiosk(ctx, kioskID, docs[0].JobID); err != nil {
		t.Fatalf("MarkDispatchIntentAcknowledgedForKiosk: %v", err)
	}

	// Doc 0 fails to FAILED_REFUNDABLE while doc 1 is still DISPATCHED (unresolved) -> order must aggregate to PAUSED_ERROR!
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[0].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "canceled", Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 0: %v", err)
	}

	ord, err := st.GetOrder(ctx, orderID)
	if err != nil || ord.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected order with failed doc + unresolved sibling to aggregate to PAUSED_ERROR, got %s (err=%v)", ord.Status, err)
	}

	// Acknowledged failed dispatch intent for doc 0 must be CANCELED (never eligible for reactivation)
	dStatus, _, err := st.GetJobDispatchIntentStatus(ctx, docs[0].JobID)
	if err != nil || dStatus != "CANCELED" {
		t.Fatalf("expected acknowledged failed dispatch intent CANCELED, got %q (err=%v)", dStatus, err)
	}

	// PAUSED_ERROR must be excluded from retention cleanup
	expired, err := st.GetExpiredOrderIDs(ctx, now.Add(48*time.Hour), now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("GetExpiredOrderIDs: %v", err)
	}
	for _, eid := range expired {
		if eid == orderID {
			t.Fatalf("PAUSED_ERROR order %s must be excluded from GetExpiredOrderIDs", orderID)
		}
	}

	// Once doc 1 is also resolved to FAILED_REFUNDABLE, order transitions to FAILED_REFUNDABLE and session closes
	if err := st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "stopped", Timestamp: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("UpdateJobProgress doc 1: %v", err)
	}
	ord, _ = st.GetOrder(ctx, orderID)
	if ord.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected order status FAILED_REFUNDABLE after all docs resolved, got %s", ord.Status)
	}

	// FAILED_REFUNDABLE must also be excluded from retention cleanup until Package 7 refund reconciliation
	expiredAfterFail, err := st.GetExpiredOrderIDs(ctx, now.Add(48*time.Hour), now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("GetExpiredOrderIDs after FAILED_REFUNDABLE: %v", err)
	}
	for _, eid := range expiredAfterFail {
		if eid == orderID {
			t.Fatalf("FAILED_REFUNDABLE order %s must be excluded from GetExpiredOrderIDs", orderID)
		}
	}
}


func TestPackage6B_ConcurrentDispatchAuthorizationAndFailureFinalization(t *testing.T) {
	ctx := context.Background()
	st, cleanup := openConcurrencyTestStore(t, "pkg6b_conc_auth")
	defer cleanup()

	now := time.Now().UTC()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	orderID := "ord-6b-conc-" + suffix
	kioskID := "kiosk-6b-conc-" + suffix

	docs := []OrderDocument{
		{JobID: orderID + "-0", OrderID: orderID, DocumentIndex: 0, OriginalFilename: "d0.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000"},
		{JobID: orderID + "-1", OrderID: orderID, DocumentIndex: 1, OriginalFilename: "d1.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/1.pdf", SHA256Checksum: "1111"},
	}
	cmd := contracts.PrintJobCommand{
		JobID: orderID, OrderID: orderID, TotalDocuments: 2, KioskID: kioskID, DisplayCode: "9191",
		DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000", TotalPages: 4, TotalSheets: 4, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.CreateOrder(ctx, Order{
		OrderID: orderID, Command: cmd, Status: contracts.JobStatusPendingPayment, PricePaise: 800,
		TotalPages: 4, TotalSheets: 4, Documents: docs, PINExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSession: %v", err)
	}

	// Capture initial payment and dispatch doc 0
	res, err := st.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID: "pay-" + suffix, OrderID: orderID, KioskID: kioskID, SessionID: sess.SessionID,
		AmountPaise: 800, Provider: "mock", Status: "CAPTURED",
	}, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch: %v", err)
	}
	if !res.DispatchEligible {
		t.Fatalf("expected initial payment capture to authorize dispatch")
	}

	// Race ReleasePaidOrder and duplicate CapturePaymentAndCheckDispatch against failure finalization of doc 0 and doc 1.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _ = st.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now.Add(time.Second))
	}()
	go func() {
		defer wg.Done()
		_, _ = st.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
			PaymentID: "pay-dup-" + suffix, OrderID: orderID, KioskID: kioskID, SessionID: sess.SessionID,
			AmountPaise: 800, Provider: "mock", Status: "CAPTURED",
		}, now.Add(time.Second))
	}()
	go func() {
		defer wg.Done()
		_ = st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
			JobID: docs[0].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
			ErrorReason: "jam", Timestamp: now.Add(2 * time.Second),
		})
		_ = st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
			JobID: docs[1].JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
			ErrorReason: "stopped", Timestamp: now.Add(2 * time.Second),
		})
	}()
	wg.Wait()

	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected final order status FAILED_REFUNDABLE (never reactivated), got %s", ord.Status)
	}
	for _, d := range docs {
		ds, _, err := st.GetJobDispatchIntentStatus(ctx, d.JobID)
		if err != nil {
			t.Fatalf("GetJobDispatchIntentStatus(%s): %v", d.JobID, err)
		}
		if ds != "CANCELED" {
			t.Fatalf("expected dispatch intent %s to be CANCELED, got %s", d.JobID, ds)
		}
	}
	if err := st.MarkDispatchIntentSent(ctx, docs[1].JobID); err == nil {
		t.Fatalf("expected MarkDispatchIntentSent on canceled/stopped order to fail")
	}
}
