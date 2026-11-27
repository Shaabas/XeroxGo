package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestHub_ProgressAckOnlyAfterDurableCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "hub6b.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	orderID := "ord-ws-6b"
	jobID := "job-ws-6b"
	kioskID := "kiosk-ws-6b"
	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        kioskID,
		DisplayCode:    "4321",
		DownloadURL:    "http://localhost/doc.pdf",
		SHA256Checksum: "deadbeef",
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.CreateOrder(ctx, store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusDispatched,
		PricePaise:   500,
		TotalPages:   2,
		TotalSheets:  2,
		PINExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	hub := NewHub(st)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "?kiosk_id=" + kioskID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Send rejected progress (missing/unknown job) with OutcomeVersion -> must NOT receive progress_ack
	badEvt := contracts.JobProgressEvent{
		JobID:     "nonexistent-job",
		KioskID:   kioskID,
		Status:    contracts.JobStatusFailedRefundable,
		Timestamp: time.Now().UTC(),
	}
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &badEvt, OutcomeVersion: 7}); err != nil {
		t.Fatalf("Write bad progress: %v", err)
	}

	// Send valid progress with OutcomeVersion -> must receive progress_ack with OutcomeVersion == 3
	goodEvt := contracts.JobProgressEvent{
		JobID:         jobID,
		KioskID:       kioskID,
		Status:        contracts.JobStatusFailedRefundable,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		ErrorReason:   "download failed",
		Timestamp:     time.Now().UTC(),
	}
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &goodEvt, OutcomeVersion: 3}); err != nil {
		t.Fatalf("Write good progress: %v", err)
	}

	var ack wireMessage
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if err := json.Unmarshal(raw, &ack); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if ack.Type == "progress_ack" {
			break
		}
	}

	if !ack.OK || ack.OutcomeVersion != 3 || ack.Progress == nil || ack.Progress.JobID != jobID {
		t.Fatalf("unexpected progress_ack: %+v", ack)
	}
}

func TestHub_ObsoleteSessionAndCrossKioskAckRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "hub6b_obs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	orderID := "ord-ws-obs"
	jobID := "job-ws-obs"
	kioskID := "kiosk-ws-obs"
	if err := st.CreateOrder(ctx, store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID: jobID, OrderID: orderID, DocumentIndex: 0, TotalDocuments: 1,
			KioskID: kioskID, DisplayCode: "9999", DownloadURL: "http://localhost/doc.pdf",
			SHA256Checksum: "deadbeef", TotalPages: 2, TotalSheets: 2, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		},
		Status: contracts.JobStatusDispatched, PricePaise: 500, TotalPages: 2, TotalSheets: 2,
		PINExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	hub := NewHub(st)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	wsURL1 := "ws" + strings.TrimPrefix(srv.URL, "http") + "?kiosk_id=" + kioskID
	conn1, _, err := websocket.Dial(ctx, wsURL1, nil)
	if err != nil {
		t.Fatalf("Dial 1: %v", err)
	}
	defer conn1.Close(websocket.StatusNormalClosure, "done")

	// Wait until conn1 is registered, then replace it with conn2
	deadline := time.Now().Add(2 * time.Second)
	for !hub.IsConnected(kioskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	conn2, _, err := websocket.Dial(ctx, wsURL1, nil)
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "done")

	// Cross-kiosk connection cannot acknowledge kiosk-ws-obs's dispatch intent
	wsOther := "ws" + strings.TrimPrefix(srv.URL, "http") + "?kiosk_id=other-kiosk"
	connOther, _, err := websocket.Dial(ctx, wsOther, nil)
	if err != nil {
		t.Fatalf("Dial other: %v", err)
	}
	defer connOther.Close(websocket.StatusNormalClosure, "done")

	_ = wsjson.Write(ctx, connOther, wireMessage{
		Type: "job_ack",
		Job:  &contracts.PrintJobCommand{JobID: jobID, KioskID: kioskID},
	})

	// Obsolete conn1 must not mutate progress
	_ = wsjson.Write(ctx, conn1, wireMessage{
		Type: "progress",
		Progress: &contracts.JobProgressEvent{
			JobID: jobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
			Timestamp: time.Now().UTC(),
		},
		OutcomeVersion: 1,
	})
	time.Sleep(150 * time.Millisecond)

	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected obsolete/unauthorized WS to never mutate order status, got %s", ord.Status)
	}
}


func TestHub_SendJob_FailsClosedOnMissingOrderOrIntent(t *testing.T) {
	st := newTestStore(t)
	hub := NewHub(st)
	hub.SetProductPolicy(contracts.ProductCapabilityPolicy{
		AllowDuplex: true, RequireA4: true, PaperSize: "A4", AllowMultiCopy: true,
	})
	server := httptest.NewServer(hub)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _ := connectTestKiosk(t, hub, server.URL, "kiosk-missing-ord")
	sendTestReadyHealth(ctx, t, hub, conn, "kiosk-missing-ord")

	err := hub.SendJob(ctx, contracts.PrintJobCommand{
		JobID: "job-missing-1", OrderID: "ord-missing-1", KioskID: "kiosk-missing-ord",
		DisplayCode: "1111", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	})
	if err == nil {
		t.Fatalf("expected SendJob to fail closed when order/dispatch intent lookup fails")
	}
}

func TestHub_ConcurrentDispatchAndFailureFinalization(t *testing.T) {
	st := newTestStore(t)
	hub := NewHub(st)
	hub.SetProductPolicy(contracts.ProductCapabilityPolicy{
		AllowDuplex: true, RequireA4: true, PaperSize: "A4", AllowMultiCopy: true,
	})
	server := httptest.NewServer(hub)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kioskID := "kiosk-conc-disp"
	orderID := fmt.Sprintf("ord-conc-disp-%d", time.Now().UnixNano())
	now := time.Now().UTC()

	docs := []store.OrderDocument{
		{JobID: orderID + "-0", OrderID: orderID, DocumentIndex: 0, OriginalFilename: "d0.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/0.pdf", SHA256Checksum: "0000"},
		{JobID: orderID + "-1", OrderID: orderID, DocumentIndex: 1, OriginalFilename: "d1.pdf", TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", PricePaise: 400, Status: contracts.JobStatusPendingPayment, DownloadURL: "http://localhost/1.pdf", SHA256Checksum: "1111"},
	}
	cmd0 := contracts.PrintJobCommand{
		JobID: docs[0].JobID, OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: kioskID, DisplayCode: "9292", DownloadURL: "http://localhost/0.pdf",
		SHA256Checksum: "0000", TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: docs[1].JobID, OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: kioskID, DisplayCode: "9292", DownloadURL: "http://localhost/1.pdf",
		SHA256Checksum: "1111", TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.CreateOrder(ctx, store.Order{
		OrderID: orderID, Command: cmd0, Status: contracts.JobStatusPendingPayment, PricePaise: 800,
		TotalPages: 4, TotalSheets: 4, Documents: docs, PINExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSession: %v", err)
	}
	if _, err := st.CapturePaymentAndCheckDispatch(ctx, store.PaymentRecord{
		PaymentID: "pay-" + orderID, OrderID: orderID, KioskID: kioskID, SessionID: sess.SessionID,
		AmountPaise: 800, Provider: "mock", Status: "CAPTURED",
	}, now); err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch: %v", err)
	}

	conn, _ := connectTestKiosk(t, hub, server.URL, kioskID)
	sendTestReadyHealth(ctx, t, hub, conn, kioskID)

	var sentDoc1 atomic.Bool
	go func() {
		for {
			var m wireMessage
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				return
			}
			if m.Type == "job" && m.Job != nil && m.Job.JobID == cmd1.JobID {
				sentDoc1.Store(true)
			}
		}
	}()

	hub.mu.RLock()
	activeSess := hub.sessions[kioskID]
	hub.mu.RUnlock()

	var wg sync.WaitGroup
	wg.Add(2)
	var sendErr error
	go func() {
		defer wg.Done()
		sendErr = hub.sendJobToSessionWithAuth(ctx, activeSess, cmd1, true)
	}()
	go func() {
		defer wg.Done()
		_ = st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
			JobID: cmd0.JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
			ErrorReason: "jam", Timestamp: now.Add(time.Second),
		})
		_ = st.UpdateJobProgress(ctx, contracts.JobProgressEvent{
			JobID: cmd1.JobID, KioskID: kioskID, Status: contracts.JobStatusFailedRefundable,
			ErrorReason: "stopped", Timestamp: now.Add(time.Second),
		})
	}()
	wg.Wait()

	// If failure finalization won the per-order lock first, sendJobToSessionWithAuth must fail and never write doc1.
	if sendErr != nil && sentDoc1.Load() {
		t.Fatalf("wire transmission occurred despite rejected dispatch authorization: %v", sendErr)
	}
	// After failure finalization, any subsequent dispatch attempt must be rejected and never transmitted.
	if err := hub.SendJob(ctx, cmd1); err == nil {
		t.Fatalf("expected post-failure SendJob to fail closed")
	}
}
