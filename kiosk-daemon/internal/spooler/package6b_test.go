package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
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
	nowFresh := time.Now().UTC().Add(2 * time.Second)
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

func TestPackage6B_HealthyButPrinting_And_PreCancellationTelemetry_CannotFinalize(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "quiesce.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-quiesce-1", OrderID: "ord-quiesce-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1212", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("c", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	if err := st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 100, "1.1", 8, time.Now().UTC()); err != nil {
		t.Fatalf("MarkJobSubmittingWithBaseline: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, cmd.JobID, "DCPT310-77"); err != nil {
		t.Fatalf("RecordJobSubmission: %v", err)
	}

	var canceledNow atomic.Bool
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "cancel":
			canceledNow.Store(true)
			return []byte(""), nil
		case "ipptool":
			if canceledNow.Load() {
				return []byte("job-state (enum) = canceled\n"), nil
			}
			return []byte("job-state (enum) = processing\n"), nil
		case "lpstat":
			if canceledNow.Load() {
				return []byte(""), nil
			}
			return []byte("DCPT310-77 user 1024 Mon\n"), nil
		default:
			return []byte(""), nil
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		CUPSRunner: runner, DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Case 1: EvaluatedHealthHealthy but State == "PRINTING" -> must stay PAUSED_ERROR
	now := time.Now().UTC()
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample: true, Seq: 1, AcquiredAt: now,
		Health: contracts.PrinterHealth{
			Online: true, State: "PRINTING", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
			ExecutionSafe: true, UpdatedAt: now,
			EvidenceFreshness: &contracts.HealthEvidenceFreshness{
				CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
			},
		},
	}
	client.snapshotMu.Unlock()

	stored, _ := st.GetJob(ctx, cmd.JobID)
	status := client.reconcileSubmittedOrInterruptedJob(ctx, stored, "interrupted", false)
	if status != contracts.JobStatusPausedError {
		t.Fatalf("expected healthy-but-PRINTING telemetry to stay PAUSED_ERROR, got %s", status)
	}

	// Case 2: Pre-cancellation IDLE telemetry (UpdatedAt strictly before cancel) -> must stay PAUSED_ERROR
	preCancel := time.Now().UTC().Add(-2 * time.Second)
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample: true, Seq: 2, AcquiredAt: preCancel,
		Health: contracts.PrinterHealth{
			Online: true, State: "IDLE", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
			ExecutionSafe: true, UpdatedAt: preCancel,
			EvidenceFreshness: &contracts.HealthEvidenceFreshness{
				CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &preCancel},
				NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &preCancel},
				SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &preCancel},
			},
		},
	}
	client.snapshotMu.Unlock()

	stored, _ = st.GetJob(ctx, cmd.JobID)
	status = client.reconcileSubmittedOrInterruptedJob(ctx, stored, "interrupted", false)
	if status != contracts.JobStatusPausedError {
		t.Fatalf("expected pre-cancellation telemetry to stay PAUSED_ERROR, got %s", status)
	}
}

func TestPackage6B_ActiveFaultWithResumedProcessingCUPS_AttemptsSafeTermination(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "resumed_proc.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-resproc-1", OrderID: "ord-resproc-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1313", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("d", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusDispatched)
	_ = st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 100, "1.1", 8, time.Now().UTC())
	_ = st.RecordJobSubmission(ctx, cmd.JobID, "DCPT310-88")

	var cancelCalled atomic.Int32
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "cancel":
			cancelCalled.Add(1)
			return []byte(""), nil
		case "ipptool":
			if cancelCalled.Load() == 0 {
				return []byte("job-state (enum) = processing\n"), nil
			}
			return []byte("job-state (enum) = canceled\n"), nil
		default:
			return []byte(""), nil
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		CUPSRunner: runner, DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client.setHardwareFault(true, "paper jam", "Paper jam")
	stored, _ := st.GetJob(ctx, cmd.JobID)
	status := client.reconcileSubmittedOrInterruptedJob(ctx, stored, "paper jam", false)
	if cancelCalled.Load() == 0 {
		t.Fatalf("expected active resumable processing CUPS job to be canceled even during hardware fault")
	}
	if status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to stay PAUSED_ERROR while fault active, got %s", status)
	}
}

func TestPackage6B_CompletedCUPSWithFullVerifiedSheets_WithoutCounterEvidence_CannotComplete(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "comp6b_nocounter.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-comp6b-nocnt", OrderID: "ord-comp6b-nocnt", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1414", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("e", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusDispatched)
	_ = st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 100, "1.1", 8, time.Now().UTC())
	_ = st.RecordJobSubmission(ctx, cmd.JobID, "DCPT310-99")
	_ = st.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: "kiosk-1", Status: contracts.JobStatusPausedError,
		PagesPrinted: 2, SheetsPrinted: 2, ErrorReason: "transient error after last sheet", Timestamp: time.Now().UTC(),
	})

	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\n"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		CUPSRunner: runner, DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	now := time.Now().UTC().Add(50 * time.Millisecond)
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample: true, Seq: 1, AcquiredAt: now,
		Health: contracts.PrinterHealth{
			Online: true, State: "IDLE", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
			ExecutionSafe: true, UpdatedAt: now,
			EvidenceFreshness: &contracts.HealthEvidenceFreshness{
				CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
			},
		},
	}
	client.snapshotMu.Unlock()

	stored, _ := st.GetJob(ctx, cmd.JobID)
	status := client.reconcileSubmittedOrInterruptedJob(ctx, stored, "reconcile", true)
	if status == contracts.JobStatusCompleted {
		t.Fatalf("must NOT complete without fresh SNMP counter evidence even when saved sheets == TotalSheets")
	}
	if status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected completed CUPS + empty queue + fresh quiescence without counter proof to finalize FAILED_REFUNDABLE, got %s", status)
	}
	after, _ := st.GetJob(ctx, cmd.JobID)
	if after.SheetsPrinted != 2 {
		t.Fatalf("expected saved SheetsPrinted=2 preserved without fabrication, got %d", after.SheetsPrinted)
	}
}

func TestPackage6B_InterruptedCUPS_WithValidCounterEvidence_CompletesAndFinalizesSiblings(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "comp6b_valid.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	orderID := "ord-comp6b-valid"
	cmd0 := contracts.PrintJobCommand{
		JobID: "job-comp6b-v0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "1415", DownloadURL: "http://localhost/doc0.pdf",
		SHA256Checksum: strings.Repeat("e", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: "job-comp6b-v1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "1415", DownloadURL: "http://localhost/doc1.pdf",
		SHA256Checksum: strings.Repeat("f", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd0, contracts.JobStatusDispatched)
	_ = st.SaveJob(ctx, cmd1, contracts.JobStatusDispatched)
	if err := st.MarkJobSubmittingWithBaseline(ctx, cmd0.JobID, true, 100, "1.1", 8, time.Now().UTC()); err != nil {
		t.Fatalf("MarkJobSubmittingWithBaseline: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, cmd0.JobID, "DCPT310-100"); err != nil {
		t.Fatalf("RecordJobSubmission: %v", err)
	}
	_ = st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd1.JobID, KioskID: "kiosk-1", Status: contracts.JobStatusPausedError,
		ErrorReason: "interrupted", Timestamp: time.Now().UTC(),
	})

	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\n"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		CUPSRunner: runner, DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		SNMPPoller: func(_ context.Context) printer.HardwareTelemetry {
			return printer.HardwareTelemetry{
				Online: true, HasLife: true, LifeCount: 102, CounterIndex: "1.1", CounterUnit: 8,
				HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC(),
			}
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	now := time.Now().UTC().Add(50 * time.Millisecond)
	client.snapshotMu.Lock()
	client.latestSnap = telemetrySnapshot{
		HasSample: true, Seq: 1, AcquiredAt: now,
		Health: contracts.PrinterHealth{
			Online: true, State: "IDLE", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
			ExecutionSafe: true, UpdatedAt: now,
			EvidenceFreshness: &contracts.HealthEvidenceFreshness{
				CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
			},
		},
	}
	client.snapshotMu.Unlock()

	stored0, _ := st.GetJob(ctx, cmd0.JobID)
	status := client.reconcileSubmittedOrInterruptedJob(ctx, stored0, "reconcile", true)
	if status != contracts.JobStatusCompleted {
		t.Fatalf("expected valid counter evidence + completed CUPS to reconcile to COMPLETED, got %s", status)
	}
	stored1, _ := st.GetJob(ctx, cmd1.JobID)
	if stored1.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected unsubmitted PAUSED_ERROR sibling to finalize to FAILED_REFUNDABLE, got %s", stored1.Status)
	}
}

type failingFinalizeStore struct {
	StateStore
	failErr error
}

func (f *failingFinalizeStore) FinalizeOrderFailure(ctx context.Context, orderID, jobID string, event contracts.JobProgressEvent, invalidateInventory bool) ([]state.StoredJob, error) {
	return nil, f.failErr
}

func TestPackage6B_FailedSQLiteTx_RetainsFailClosedBarrier(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	realSt, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "failtx.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer realSt.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-failtx-1", OrderID: "ord-failtx-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1515", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("f", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = realSt.SaveJob(ctx, cmd, contracts.JobStatusDispatched)

	wrapped := &failingFinalizeStore{StateStore: realSt, failErr: errors.New("forced sqlite tx failure")}
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client.finalizeAndEmitOrderFailure(ctx, cmd.OrderID, cmd.JobID, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: time.Now().UTC(),
	}, false)

	if !client.isPersistenceBlocked() {
		t.Fatalf("expected fail-closed persistence barrier to remain active when FinalizeOrderFailure fails")
	}
	stored, _ := realSt.GetJob(ctx, cmd.JobID)
	if stored.Status == contracts.JobStatusFailedRefundable {
		t.Fatalf("failed FinalizeOrderFailure must not authorize FAILED_REFUNDABLE")
	}
}

func TestPackage6B_LostCompletedOutcome_RetransmitsWithOriginalTimestamp_And_StaleAckIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "comp_sync.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-compsync-1", OrderID: "ord-compsync-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-compsync", DisplayCode: "1616", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("1", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusPrinting)
	origTS := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Millisecond)
	if err := st.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: origTS,
	}); err != nil {
		t.Fatalf("RecordSheetProgress: %v", err)
	}
	// Bump outcome_version to 2 via UpdateProgress idempotent COMPLETED, so v1 is stale
	_ = st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: origTS,
	})

	// Stale ack for v1 must NOT clear v2
	_ = st.MarkOutcomeSynced(ctx, cmd.JobID, 1)
	pending, _ := st.GetPendingOutcomeSyncJobs(ctx)
	if len(pending) != 1 || pending[0].OutcomeVersion != 2 {
		t.Fatalf("expected v2 still pending after stale v1 ack, got %+v", pending)
	}

	var seenAttempts atomic.Int32
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
				n := seenAttempts.Add(1)
				if !m.Progress.Timestamp.Equal(origTS) || m.Progress.Status != contracts.JobStatusCompleted {
					t.Errorf("expected original COMPLETED timestamp %v, got %+v", origTS, m.Progress)
				}
				// Drop first attempt's ack to exercise connected retry, ack second attempt
				if n >= 2 {
					_ = wsjson.Write(r.Context(), c, wireMessage{
						Type:           "progress_ack",
						Progress:       &contracts.JobProgressEvent{JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusCompleted},
						OutcomeVersion: m.OutcomeVersion,
						OK:             true,
					})
				}
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient(ClientConfig{
		CloudURL: "ws" + strings.TrimPrefix(srv.URL, "http"), KioskID: cmd.KioskID, StateStore: st,
		CUPSQueue: "DCPT310", DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := st.GetPendingOutcomeSyncJobs(ctx)
		if err == nil && len(p) == 0 {
			cancel()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected lost COMPLETED outcome to retransmit and clear after connected retry ack")
}


type failingPendingOutcomeStore struct {
	StateStore
	failErr error
}

func (f *failingPendingOutcomeStore) GetPendingOutcomeSyncJobs(ctx context.Context) ([]state.StoredJob, error) {
	return nil, f.failErr
}

func TestPackage6B_IdleConnection_ReceivesNewPendingOutcomeViaSignalAndAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "idle_signal.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	var acked atomic.Bool
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
			if m.Type == "progress" && m.Progress != nil && m.OutcomeVersion > 0 {
				acked.Store(true)
				_ = wsjson.Write(r.Context(), c, wireMessage{
					Type:           "progress_ack",
					Progress:       &contracts.JobProgressEvent{JobID: m.Progress.JobID, KioskID: m.Progress.KioskID, Status: m.Progress.Status},
					OutcomeVersion: m.OutcomeVersion,
					OK:             true,
				})
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient(ClientConfig{
		CloudURL: "ws" + strings.TrimPrefix(srv.URL, "http"), KioskID: "kiosk-idle-sig", StateStore: st,
		CUPSQueue: "DCPT310", DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
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

	// Allow initial syncPendingOutcomes pass on idle connection (0 pending outcomes).
	time.Sleep(150 * time.Millisecond)

	cmd := contracts.PrintJobCommand{
		JobID: "job-idle-sig-1", OrderID: "ord-idle-sig-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-idle-sig", DisplayCode: "1717", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("a", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusPrinting)
	if err := st.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusCompleted,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordSheetProgress: %v", err)
	}
	client.signalOutcomeSync()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := st.GetPendingOutcomeSyncJobs(ctx)
		if err == nil && len(p) == 0 && acked.Load() {
			cancel()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected idle connection to wake on signalOutcomeSync and clear pending outcome")
}

func TestPackage6B_PendingOutcomeQueryFailure_BlocksAdmission(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	realSt, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "penderr.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer realSt.Close()

	wrapped := &failingPendingOutcomeStore{StateStore: realSt, failErr: errors.New("sqlite read error")}
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	safe, reason := client.computeExecutionSafety(ctx, "")
	if safe {
		t.Fatalf("expected computeExecutionSafety to fail closed when GetPendingOutcomeSyncJobs fails")
	}
	if !strings.Contains(strings.ToLower(reason), "pending") {
		t.Fatalf("expected pending-outcome reason, got %q", reason)
	}

	cmd := contracts.PrintJobCommand{
		JobID: "job-penderr-1", OrderID: "ord-penderr-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1818", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	client.handleIncomingJob(cmd)
	if j, err := realSt.GetJob(ctx, cmd.JobID); err != nil || j != nil {
		t.Fatalf("expected handleIncomingJob to refuse admission when GetPendingOutcomeSyncJobs fails (got job=%+v, err=%v)", j, err)
	}
}

type selectiveFailFinalizeStore struct {
	StateStore
	mu      sync.Mutex
	failJob map[string]error
}

func (s *selectiveFailFinalizeStore) FinalizeOrderFailure(ctx context.Context, orderID, jobID string, event contracts.JobProgressEvent, invalidateInventory bool) ([]state.StoredJob, error) {
	s.mu.Lock()
	err := s.failJob[jobID]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.StateStore.FinalizeOrderFailure(ctx, orderID, jobID, event, invalidateInventory)
}

func TestPackage6B_ThreeHealthyHealthCheckCycles_DoNotClearUnresolvedPersistenceBarrier(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	realSt, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "barrier_hw.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer realSt.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-bar-1", OrderID: "ord-bar-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "1919", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("c", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = realSt.SaveJob(ctx, cmd, contracts.JobStatusDispatched)

	wrapped := &selectiveFailFinalizeStore{
		StateStore: realSt,
		failJob:    map[string]error{cmd.JobID: errors.New("forced sqlite finalize failure")},
	}
	var faultActive atomic.Bool
	faultActive.Store(true)
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockHealth: func(_ context.Context, sheets int) contracts.PrinterHealth {
			now := time.Now().UTC()
			if faultActive.Load() {
				return contracts.PrinterHealth{
					Online: false, State: "ERROR", PaperJam: true, EvaluatedHealth: contracts.EvaluatedHealthFault,
					EstimatedSheets: sheets, InventoryCalibrated: true, UpdatedAt: now,
					EvidenceFreshness: &contracts.HealthEvidenceFreshness{
						CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
						NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
						SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
					},
				}
			}
			return contracts.PrinterHealth{
				Online: true, State: "IDLE", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
				EstimatedSheets: sheets, InventoryCalibrated: true, ExecutionSafe: true, UpdatedAt: now,
				EvidenceFreshness: &contracts.HealthEvidenceFreshness{
					CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
					NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
					SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				},
			}
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Drive hardware fault through actual CheckPrinterHealth path, then fail FinalizeOrderFailure.
	_ = client.CheckPrinterHealth(ctx)
	client.finalizeAndEmitOrderFailure(ctx, cmd.OrderID, cmd.JobID, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: time.Now().UTC(),
	}, false)
	if !client.isPersistenceBlocked() {
		t.Fatalf("expected persistence barrier active after FinalizeOrderFailure error")
	}

	// Run 3 healthy CheckPrinterHealth cycles while FinalizeOrderFailure still fails.
	faultActive.Store(false)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}
	if !client.isPersistenceBlocked() {
		t.Fatalf("expected persistence barrier to remain active across 3 healthy CheckPrinterHealth cycles while unresolved")
	}
	if safe, _ := client.computeExecutionSafety(ctx, ""); safe {
		t.Fatalf("expected execution safety blocked while persistence barrier unresolved")
	}

	// Unblock FinalizeOrderFailure and run CheckPrinterHealth -> retry resolves and clears barrier.
	wrapped.mu.Lock()
	delete(wrapped.failJob, cmd.JobID)
	wrapped.mu.Unlock()
	_ = client.CheckPrinterHealth(ctx)

	if client.isPersistenceBlocked() {
		t.Fatalf("expected persistence barrier to clear only after FinalizeOrderFailure retry succeeded")
	}
	stored, _ := realSt.GetJob(ctx, cmd.JobID)
	if stored.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected retried job to reach FAILED_REFUNDABLE, got %s", stored.Status)
	}
}

func TestPackage6B_UnrelatedFinalization_DoesNotClearPersistenceBarrier(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	realSt, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "barrier_unrelated.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer realSt.Close()

	cmdA := contracts.PrintJobCommand{
		JobID: "job-bar-a", OrderID: "ord-bar-a", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "2020", DownloadURL: "http://localhost/a.pdf",
		SHA256Checksum: strings.Repeat("a", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmdB := contracts.PrintJobCommand{
		JobID: "job-bar-b", OrderID: "ord-bar-b", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "2021", DownloadURL: "http://localhost/b.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = realSt.SaveJob(ctx, cmdA, contracts.JobStatusDispatched)
	_ = realSt.SaveJob(ctx, cmdB, contracts.JobStatusDispatched)

	wrapped := &selectiveFailFinalizeStore{
		StateStore: realSt,
		failJob:    map[string]error{cmdA.JobID: errors.New("sqlite busy for A")},
	}
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client.finalizeAndEmitOrderFailure(ctx, cmdA.OrderID, cmdA.JobID, contracts.JobProgressEvent{
		JobID: cmdA.JobID, KioskID: cmdA.KioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "err A", Timestamp: time.Now().UTC(),
	}, false)
	if !client.isPersistenceBlocked() {
		t.Fatalf("expected barrier active for job A")
	}

	// Unrelated job B finalizes successfully -> must NOT clear job A's barrier.
	client.finalizeAndEmitOrderFailure(ctx, cmdB.OrderID, cmdB.JobID, contracts.JobProgressEvent{
		JobID: cmdB.JobID, KioskID: cmdB.KioskID, Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "err B", Timestamp: time.Now().UTC(),
	}, false)
	if !client.isPersistenceBlocked() {
		t.Fatalf("unrelated job B finalization must NOT clear job A's persistence barrier")
	}
}

func TestPackage6B_VerifyPostTerminationQuiescence_NoRecursiveDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "deadlock_quiesce.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-dlq-1", OrderID: "ord-dlq-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "2121", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("d", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusDispatched)
	_ = st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 100, "1.1", 8, time.Now().UTC())
	_ = st.RecordJobSubmission(ctx, cmd.JobID, "DCPT310-212")

	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			return []byte("job-state (enum) = canceled\n"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		CUPSRunner: runner, DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockHealth: func(_ context.Context, sheets int) contracts.PrinterHealth {
			now := time.Now().UTC()
			return contracts.PrinterHealth{
				Online: true, State: "IDLE", EvaluatedHealth: contracts.EvaluatedHealthHealthy,
				EstimatedSheets: sheets, InventoryCalibrated: true, ExecutionSafe: true, UpdatedAt: now,
				EvidenceFreshness: &contracts.HealthEvidenceFreshness{
					CUPS:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
					NativeIPP: contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
					SNMP:      contracts.SourceFreshness{Available: true, Fresh: true, ObservedAt: &now},
				},
			}
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = client.CheckPrinterHealth(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		stored, _ := st.GetJob(ctx, cmd.JobID)
		status := client.reconcileSubmittedOrInterruptedJob(ctx, stored, "interrupted", false)
		if status != contracts.JobStatusFailedRefundable {
			t.Errorf("expected canceled + empty queue + fresh MockHealth to finalize FAILED_REFUNDABLE, got %s", status)
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("deadlock detected between CheckPrinterHealth and reconcileSubmittedOrInterruptedJob")
	}
}


func TestPackage6B_RestartSafety_UnsubmittedFinalizeFailure_CannotReprint(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "restart_unsub.db")

	st1, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("openCalibratedTestStore #1: %v", err)
	}

	orderID := "ord-restart-unsub"
	cmd0 := contracts.PrintJobCommand{
		JobID: "job-ru-0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2323", DownloadURL: "http://127.0.0.1:1/0.pdf",
		SHA256Checksum: strings.Repeat("a", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: "job-ru-1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2323", DownloadURL: "http://127.0.0.1:1/1.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st1.SaveJob(ctx, cmd0, contracts.JobStatusDispatched)
	_ = st1.SaveJob(ctx, cmd1, contracts.JobStatusDispatched)

	wrapped1 := &selectiveFailFinalizeStore{
		StateStore: st1,
		failJob:    map[string]error{cmd0.JobID: errors.New("forced FinalizeOrderFailure error before restart")},
	}
	client1, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: wrapped1, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		TrustedOrigins: []string{"http://127.0.0.1:1"},
		MockHealth:     testHealthyMockHealth,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient #1: %v", err)
	}

	// Processing starts (DISPATCHED -> SPOOLING), download fails, and FinalizeOrderFailure fails.
	client1.processJob(ctx, cmd0)
	_ = st1.Close()

	// Reopen SQLite DB with a fresh Client (in-memory pendingFinalizations is empty).
	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open #2: %v", err)
	}
	defer st2.Close()

	var printed atomic.Bool
	client2, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:8080/ws", KioskID: "kiosk-1", StateStore: st2, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockCUPSPrint: func(_ context.Context, _ string, _ contracts.PrintJobCommand, _ string, _ func(int)) error {
			printed.Store(true)
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient #2: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()
	go func() {
		_ = client2.Run(runCtx)
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		j0, _ := st2.GetJob(ctx, cmd0.JobID)
		j1, _ := st2.GetJob(ctx, cmd1.JobID)
		if j0 != nil && j1 != nil &&
			(j0.Status == contracts.JobStatusPausedError || j0.Status == contracts.JobStatusFailedRefundable) &&
			(j1.Status == contracts.JobStatusPausedError || j1.Status == contracts.JobStatusFailedRefundable) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	runCancel()
	if printed.Load() {
		t.Fatalf("unresolved failed order must NEVER be printed again after restart")
	}

	j0, _ := st2.GetJob(ctx, cmd0.JobID)
	j1, _ := st2.GetJob(ctx, cmd1.JobID)
	if j0 == nil || (j0.Status != contracts.JobStatusPausedError && j0.Status != contracts.JobStatusFailedRefundable) {
		t.Fatalf("expected job-ru-0 stopped after restart, got %+v", j0)
	}
	if j1 == nil || (j1.Status != contracts.JobStatusPausedError && j1.Status != contracts.JobStatusFailedRefundable) {
		t.Fatalf("expected sibling job-ru-1 stopped after restart, got %+v", j1)
	}

	// Subsequent incoming sibling/re-dispatch for the failed order must also be rejected.
	cmd2 := contracts.PrintJobCommand{
		JobID: "job-ru-2", OrderID: orderID, DocumentIndex: 2, TotalDocuments: 3,
		KioskID: "kiosk-1", DisplayCode: "2323", DownloadURL: "http://localhost/2.pdf",
		SHA256Checksum: strings.Repeat("c", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	client2.handleIncomingJob(cmd2)
	if j2, _ := st2.GetJob(ctx, cmd2.JobID); j2 != nil {
		t.Fatalf("expected incoming job for failed order to be rejected after restart, got %+v", j2)
	}
}


type failSpoolBoundaryStore struct {
	StateStore
	failSpoolErr error
	failIncErr   error
}

func (f *failSpoolBoundaryStore) BeginJobSpooling(ctx context.Context, jobID string) error {
	if f.failSpoolErr != nil {
		return f.failSpoolErr
	}
	return f.StateStore.BeginJobSpooling(ctx, jobID)
}

func (f *failSpoolBoundaryStore) GetIncompleteJobs(ctx context.Context) ([]state.StoredJob, error) {
	if f.failIncErr != nil {
		return nil, f.failIncErr
	}
	return f.StateStore.GetIncompleteJobs(ctx)
}

func TestPackage6B_Restart_UntouchedDispatchedJobs_ResumeNormally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "resume_dispatched.db")
	st, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}

	pdfBytes := []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")
	sum := sha256.Sum256(pdfBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pdfBytes)
	}))
	defer srv.Close()

	orderID := "ord-resume-disp"
	cmd0 := contracts.PrintJobCommand{
		JobID: "job-rd-0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2424", DownloadURL: srv.URL + "/0.pdf",
		SHA256Checksum: hex.EncodeToString(sum[:]), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: "job-rd-1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2424", DownloadURL: srv.URL + "/1.pdf",
		SHA256Checksum: hex.EncodeToString(sum[:]), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd0, contracts.JobStatusCompleted)
	_ = st.SaveJob(ctx, cmd1, contracts.JobStatusDispatched)
	_ = st.Close()

	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer st2.Close()

	var printedDoc1 atomic.Bool
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: st2, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		TrustedOrigins: []string{srv.URL},
		MockHealth:     testHealthyMockHealth,
		MockCUPSPrint: func(_ context.Context, _ string, cmd contracts.PrintJobCommand, _ string, onPage func(int)) error {
			if cmd.JobID == cmd1.JobID {
				printedDoc1.Store(true)
			}
			if onPage != nil {
				onPage(cmd.TotalSheets)
			}
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()
	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		j1, _ := st2.GetJob(ctx, cmd1.JobID)
		if j1 != nil && j1.Status == contracts.JobStatusCompleted && printedDoc1.Load() {
			runCancel()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected untouched DISPATCHED job to resume and complete after restart (printed=%v)", printedDoc1.Load())
}

func TestPackage6B_ProcessingBoundaryFailure_PreventsDownloadAndPrint(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "boundary_fail.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	var downloaded, printed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded.Store(true)
		_, _ = w.Write([]byte("%PDF-1.4\n"))
	}))
	defer srv.Close()

	cmd := contracts.PrintJobCommand{
		JobID: "job-bf-1", OrderID: "ord-bf-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "2525", DownloadURL: srv.URL + "/doc.pdf",
		SHA256Checksum: strings.Repeat("a", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusDispatched)

	wrapped := &failSpoolBoundaryStore{StateStore: st, failSpoolErr: errors.New("sqlite busy on BeginJobSpooling")}
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		TrustedOrigins: []string{srv.URL},
		MockCUPSPrint: func(_ context.Context, _ string, _ contracts.PrintJobCommand, _ string, _ func(int)) error {
			printed.Store(true)
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	client.processJob(ctx, cmd)
	if downloaded.Load() || printed.Load() {
		t.Fatalf("expected BeginJobSpooling failure to prevent downloading (%v) and printing (%v)", downloaded.Load(), printed.Load())
	}
	if !client.isPersistenceBlocked() {
		t.Fatalf("expected persistence barrier active after BeginJobSpooling failure")
	}
	if safe, _ := client.computeExecutionSafety(ctx, ""); safe {
		t.Fatalf("expected execution safety blocked after BeginJobSpooling failure")
	}
}

func TestPackage6B_Restart_CompletedPrefixPreserved_StartedOrderStopsWithoutReprint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "started_prefix.db")
	st, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}

	orderID := "ord-started-pfx"
	cmd0 := contracts.PrintJobCommand{
		JobID: "job-sp-0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 3,
		KioskID: "kiosk-1", DisplayCode: "2626", DownloadURL: "http://localhost/0.pdf",
		SHA256Checksum: strings.Repeat("a", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: "job-sp-1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 3,
		KioskID: "kiosk-1", DisplayCode: "2626", DownloadURL: "http://localhost/1.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd2 := contracts.PrintJobCommand{
		JobID: "job-sp-2", OrderID: orderID, DocumentIndex: 2, TotalDocuments: 3,
		KioskID: "kiosk-1", DisplayCode: "2626", DownloadURL: "http://localhost/2.pdf",
		SHA256Checksum: strings.Repeat("c", 64), TotalPages: 1, TotalSheets: 1, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd0, contracts.JobStatusCompleted)
	_ = st.SaveJob(ctx, cmd1, contracts.JobStatusSpooling)
	_ = st.SaveJob(ctx, cmd2, contracts.JobStatusDispatched)
	_ = st.Close()

	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer st2.Close()

	var printed atomic.Bool
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: st2, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockHealth: testHealthyMockHealth,
		MockCUPSPrint: func(_ context.Context, _ string, _ contracts.PrintJobCommand, _ string, _ func(int)) error {
			printed.Store(true)
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 2*time.Second)
	defer runCancel()
	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(1800 * time.Millisecond)
	for time.Now().Before(deadline) {
		j1, _ := st2.GetJob(ctx, cmd1.JobID)
		j2, _ := st2.GetJob(ctx, cmd2.JobID)
		if j1 != nil && j2 != nil && j1.Status == contracts.JobStatusFailedRefundable && j2.Status == contracts.JobStatusFailedRefundable {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	runCancel()

	if printed.Load() {
		t.Fatalf("started SPOOLING order must never reprint on restart")
	}
	j0, _ := st2.GetJob(ctx, cmd0.JobID)
	j1, _ := st2.GetJob(ctx, cmd1.JobID)
	j2, _ := st2.GetJob(ctx, cmd2.JobID)
	if j0.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected completed prefix doc 0 preserved, got %s", j0.Status)
	}
	if j1.Status != contracts.JobStatusFailedRefundable || j2.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected unsubmitted SPOOLING + DISPATCHED docs finalized FAILED_REFUNDABLE, got %s and %s", j1.Status, j2.Status)
	}
	_, cal, _ := st2.GetPaperSupply(ctx)
	if !cal {
		t.Fatalf("expected paper calibration preserved on definitely unsubmitted interruption")
	}
}

func TestPackage6B_Restart_PotentiallySubmittedJob_NeverAutoReprints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pot_submitted.db")
	st, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID: "job-ps-1", OrderID: "ord-ps-1", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-1", DisplayCode: "2727", DownloadURL: "http://localhost/doc.pdf",
		SHA256Checksum: strings.Repeat("d", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusSpooling)
	_ = st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 100, "1.1", 8, time.Now().UTC())
	_ = st.Close()

	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer st2.Close()

	var printed atomic.Bool
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: st2, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockHealth: testHealthyMockHealth,
		MockCUPSPrint: func(_ context.Context, _ string, _ contracts.PrintJobCommand, _ string, _ func(int)) error {
			printed.Store(true)
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer runCancel()
	go func() {
		_ = client.Run(runCtx)
	}()
	time.Sleep(300 * time.Millisecond)
	runCancel()

	if printed.Load() {
		t.Fatalf("potentially submitted job must NEVER automatically reprint")
	}
	j, _ := st2.GetJob(ctx, cmd.JobID)
	if j.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected potentially submitted job without CUPS ID to remain PAUSED_ERROR, got %s", j.Status)
	}
}

func TestPackage6B_DuplicateAndLateSibling_CannotRestartFailedOrder(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "dup_late.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	orderID := "ord-duplate"
	cmd0 := contracts.PrintJobCommand{
		JobID: "job-dl-0", OrderID: orderID, DocumentIndex: 0, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2828", DownloadURL: "https://example.com/0.pdf",
		SHA256Checksum: strings.Repeat("e", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID: "job-dl-1", OrderID: orderID, DocumentIndex: 1, TotalDocuments: 2,
		KioskID: "kiosk-1", DisplayCode: "2828", DownloadURL: "https://example.com/1.pdf",
		SHA256Checksum: strings.Repeat("f", 64), TotalPages: 2, TotalSheets: 2, Copies: 1,
		Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	_ = st.SaveJob(ctx, cmd0, contracts.JobStatusSpooling)
	_, _ = st.FinalizeOrderFailure(ctx, orderID, cmd0.JobID, contracts.JobProgressEvent{
		JobID: cmd0.JobID, KioskID: "kiosk-1", Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: time.Now().UTC(),
	}, false)

	var printed atomic.Bool
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: st, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		TrustedOrigins: []string{"https://example.com"},
		MockHealth:     testHealthyMockHealth,
		MockCUPSPrint: func(_ context.Context, _ string, _ contracts.PrintJobCommand, _ string, _ func(int)) error {
			printed.Store(true)
			return nil
		},
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Valid duplicate cmd0 and valid late sibling cmd1 (passes URL/checksum/policy/index validation).
	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	if printed.Load() {
		t.Fatalf("duplicate/late sibling must never print for failed order")
	}
	if j1, _ := st.GetJob(ctx, cmd1.JobID); j1 != nil {
		t.Fatalf("expected late sibling for failed order to be rejected before saving, got %+v", j1)
	}
}

func TestPackage6B_FailedStartupStateRead_BlocksReadinessAndPrinting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "startup_read_fail.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore: %v", err)
	}
	defer st.Close()

	wrapped := &failSpoolBoundaryStore{StateStore: st, failIncErr: errors.New("sqlite disk I/O error")}
	client, err := NewClient(ClientConfig{
		CloudURL: "ws://127.0.0.1:1/ws", KioskID: "kiosk-1", StateStore: wrapped, CUPSQueue: "DCPT310",
		DownloadDir: tmpDir, IdleQRContent: "https://example.com/qr", ScreenSink: noopSink,
		MockHealth: testHealthyMockHealth,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer runCancel()
	_ = client.Run(runCtx)

	if safe, _ := client.computeExecutionSafety(context.Background(), ""); safe {
		t.Fatalf("expected execution safety blocked when startup GetIncompleteJobs failed")
	}
	ready, _, h := client.effectiveCustomerServiceReadiness(context.Background())
	if ready || h.ExecutionSafe {
		t.Fatalf("expected readiness and ExecutionSafe=false when startup recovery failed (ready=%v, safe=%v)", ready, h.ExecutionSafe)
	}
}
