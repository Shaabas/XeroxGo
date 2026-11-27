package spooler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/pkg/contracts"
)

func TestPackage6B_DefinitelyUnsubmittedFailure_FinalizesWithoutInventoryInvalidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "unsub.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	orderID := "ord-unsub-1"
	doc0 := contracts.PrintJobCommand{
		JobID: "job-unsub-0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "1111", DownloadURL: "http://127.0.0.1:1/missing.pdf",
		SHA256Checksum: strings.Repeat("0", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	doc1 := contracts.PrintJobCommand{
		JobID: "job-unsub-1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "1111", DownloadURL: "http://127.0.0.1:1/missing2.pdf",
		SHA256Checksum: strings.Repeat("1", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	for _, cmd := range []contracts.PrintJobCommand{doc0, doc1} {
		if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
			t.Fatalf("SaveJob %s: %v", cmd.JobID, err)
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-1",
		StateStore:       st,
		CUPSQueue:        "DCPT310",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://example.com/qr",
		ScreenSink:       noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client.handleJobError(ctx, doc0, 0, 0, "download failed: connection refused")

	j0, _ := st.GetJob(ctx, doc0.JobID)
	j1, _ := st.GetJob(ctx, doc1.JobID)
	if j0 == nil || j0.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected doc 0 FAILED_REFUNDABLE, got %+v", j0)
	}
	if j1 == nil || j1.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected unsubmitted sibling doc 1 FAILED_REFUNDABLE, got %+v", j1)
	}

	_, calibrated, err := st.GetPaperSupply(ctx)
	if err != nil {
		t.Fatalf("GetPaperSupply: %v", err)
	}
	if !calibrated {
		t.Fatalf("expected inventory calibration to remain valid for definitely-unsubmitted failure")
	}
}

func TestPackage6B_SubmittedFailure_ActiveFaultPausesAndInvalidatesInventory_ThenRecoversToFailedRefundable(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "subfail.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	orderID := "ord-sub-1"
	makeDoc := func(id string, idx int) contracts.PrintJobCommand {
		return contracts.PrintJobCommand{
			JobID: id, OrderID: orderID, DocumentIndex: idx, TotalDocuments: 3,
			KioskID: "kiosk-1", DisplayCode: "2222", DownloadURL: "http://localhost/doc.pdf",
			SHA256Checksum: strings.Repeat("a", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		}
	}
	d0, d1, d2 := makeDoc("job-sub-0", 0), makeDoc("job-sub-1", 1), makeDoc("job-sub-2", 2)
	for _, d := range []contracts.PrintJobCommand{d0, d1, d2} {
		if err := st.SaveJob(ctx, d, contracts.JobStatusDispatched); err != nil {
			t.Fatalf("SaveJob %s: %v", d.JobID, err)
		}
	}
	if err := st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID: d0.JobID, KioskID: "kiosk-1", Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateProgress d0: %v", err)
	}
	if err := st.MarkJobSubmittingWithBaseline(ctx, d1.JobID, true, 100, "1.1", 8, time.Now().UTC()); err != nil {
		t.Fatalf("MarkJobSubmittingWithBaseline: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, d1.JobID, "DCPT310-42"); err != nil {
		t.Fatalf("RecordJobSubmission: %v", err)
	}

	var cancelCalled atomic.Int32
	var cupsState atomic.Value
	cupsState.Store("processing")

	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "cancel":
			cancelCalled.Add(1)
			cupsState.Store("canceled")
			return []byte(""), nil
		case "ipptool":
			return []byte("job-state (enum) = " + cupsState.Load().(string) + "\n"), nil
		case "lpstat":
			if cupsState.Load().(string) == "canceled" {
				return []byte(""), nil
			}
			return []byte("DCPT310-42 user 1024 Mon\n"), nil
		default:
			return []byte(""), nil
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-1",
		StateStore:       st,
		CUPSQueue:        "DCPT310",
		CUPSRunner:       runner,
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://example.com/qr",
		ScreenSink:       noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Simulate active hardware fault -> must stay PAUSED_ERROR and invalidate inventory calibration
	client.setHardwareFault(true, "paper jam", "Paper jam")
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample:  true,
		Seq:        1,
		AcquiredAt: time.Now().UTC(),
		Health: contracts.PrinterHealth{
			PaperJam:        true,
			EvaluatedHealth: contracts.EvaluatedHealthFault,
			ExecutionSafe:   false,
			UpdatedAt:       time.Now().UTC(),
		},
	}
	client.snapshotMu.Unlock()

	client.handleJobError(ctx, d1, 1, 1, "paper feed jam detected in printer")

	j0, _ := st.GetJob(ctx, d0.JobID)
	j1, _ := st.GetJob(ctx, d1.JobID)
	j2, _ := st.GetJob(ctx, d2.JobID)
	if j0.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected COMPLETED prefix preserved on d0, got %s", j0.Status)
	}
	if j1.Status != contracts.JobStatusPausedError || j2.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected d1 and d2 PAUSED_ERROR while fault active, got %s and %s", j1.Status, j2.Status)
	}
	_, calibrated, _ := st.GetPaperSupply(ctx)
	if calibrated {
		t.Fatalf("expected is_calibrated=false when physical consumption is uncertain")
	}

	// Clear hardware fault -> healthy quiescent snapshot, CUPS already canceled & queue empty
	client.setHardwareFault(false, "", "")
	nowFresh := time.Now().UTC()
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample:  true,
		Seq:        2,
		AcquiredAt: nowFresh,
		Health: contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			EvaluatedHealth: contracts.EvaluatedHealthHealthy,
			ExecutionSafe:   true,
			UpdatedAt:       nowFresh,
			EvidenceFreshness: &contracts.HealthEvidenceFreshness{
				CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &nowFresh},
				NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &nowFresh},
				SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &nowFresh},
			},
		},
	}
	client.snapshotMu.Unlock()

	client.reconcileInterruptedCUPSJob(ctx)

	if cancelCalled.Load() == 0 {
		t.Fatalf("expected CancelAndVerifyCUPSJob to request cancel on quiescent recovery")
	}

	j0, _ = st.GetJob(ctx, d0.JobID)
	j1, _ = st.GetJob(ctx, d1.JobID)
	j2, _ = st.GetJob(ctx, d2.JobID)
	if j0.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected d0 COMPLETED preserved, got %s", j0.Status)
	}
	if j1.Status != contracts.JobStatusFailedRefundable || j2.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected d1 and d2 finalized to FAILED_REFUNDABLE on quiescent recovery, got %s (reason=%q) and %s (reason=%q)", j1.Status, j1.ErrorReason, j2.Status, j2.ErrorReason)
	}
}

func TestPackage6B_OutcomeSyncRetransmitsOnReconnectWithOriginalTimestamp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "sync6b.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-sync-1", OrderID: "ord-sync-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-sync", DisplayCode: "3333", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	origTS := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	if _, err := st.FinalizeOrderFailure(ctx, cmd.OrderID, cmd.JobID, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: origTS,
	}, false); err != nil {
		t.Fatalf("FinalizeOrderFailure: %v", err)
	}

	gotProgressCh := make(chan wireMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), c, &raw); err != nil {
				return
			}
			var m wireMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			if m.Type == "progress" && m.Progress != nil && m.Progress.JobID == cmd.JobID {
				select {
				case gotProgressCh <- m:
				default:
				}
				_ = wsjson.Write(r.Context(), c, wireMessage{
					Type:           "progress_ack",
					Progress:       &contracts.JobProgressEvent{JobID: cmd.JobID, KioskID: cmd.KioskID, Status: m.Progress.Status},
					OutcomeVersion: m.OutcomeVersion,
					OK:             true,
				})
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws" + strings.TrimPrefix(srv.URL, "http"),
		KioskID:          cmd.KioskID,
		StateStore:       st,
		CUPSQueue:        "DCPT310",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://example.com/qr",
		ScreenSink:       noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	go client.handleConnection(ctx, conn)

	select {
	case msg := <-gotProgressCh:
		if msg.OutcomeVersion == 0 || !msg.Progress.Timestamp.Equal(origTS) {
			t.Fatalf("expected original timestamp %v and nonzero outcome_version, got %+v", origTS, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for pending outcome sync")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := st.GetPendingOutcomeSyncJobs(ctx)
		if err == nil && len(pending) == 0 {
			cancel()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected pending outcome sync jobs to be marked synced after progress_ack")
}
