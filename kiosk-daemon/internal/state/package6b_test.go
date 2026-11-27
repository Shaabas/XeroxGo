package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestFinalizeOrderFailure_AtomicInventoryInvalidationAndSiblingStop(t *testing.T) {
	ctx := context.Background()
	st, err := OpenWithCapacity(filepath.Join(t.TempDir(), "state6b.db"), 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity: %v", err)
	}
	defer st.Close()

	if err := st.CalibratePaperTray(ctx, 100); err != nil {
		t.Fatalf("CalibratePaperTray: %v", err)
	}

	makeCmd := func(id string, idx int) contracts.PrintJobCommand {
		return contracts.PrintJobCommand{
			JobID:          id,
			OrderID:        "order-6b-1",
			DocumentIndex:  idx,
			TotalDocuments: 3,
			KioskID:        "kiosk-1",
			DisplayCode:    "1234",
			DownloadURL:    "http://localhost/doc.pdf",
			SHA256Checksum: "abc",
			TotalPages:     2,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
	}

	for i, id := range []string{"job-0", "job-1", "job-2"} {
		if err := st.SaveJob(ctx, makeCmd(id, i), contracts.JobStatusDispatched); err != nil {
			t.Fatalf("SaveJob %s: %v", id, err)
		}
	}

	// Doc 0 completed
	if err := st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-0", KioskID: "kiosk-1", Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateProgress job-0: %v", err)
	}

	// Doc 1 submitted to printer
	if err := st.MarkJobSubmittingWithBaseline(ctx, "job-1", true, 100, "1.1", 8, time.Now().UTC()); err != nil {
		t.Fatalf("MarkJobSubmittingWithBaseline: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, "job-1", "Xerox-11"); err != nil {
		t.Fatalf("RecordJobSubmission: %v", err)
	}

	// Failure on doc 1 with uncertain consumption -> PAUSED_ERROR + atomic inventory invalidation
	ts1 := time.Now().UTC().Truncate(time.Millisecond)
	updated, err := st.FinalizeOrderFailure(ctx, "order-6b-1", "job-1", contracts.JobProgressEvent{
		JobID: "job-1", KioskID: "kiosk-1", Status: contracts.JobStatusPausedError,
		PagesPrinted: 1, SheetsPrinted: 1, ErrorReason: "paper jam", Timestamp: ts1,
	}, true)
	if err != nil {
		t.Fatalf("FinalizeOrderFailure PAUSED_ERROR: %v", err)
	}
	if len(updated) != 3 {
		t.Fatalf("expected 3 order jobs, got %d", len(updated))
	}
	if updated[0].Status != contracts.JobStatusCompleted {
		t.Fatalf("expected doc 0 COMPLETED preserved, got %s", updated[0].Status)
	}
	if updated[1].Status != contracts.JobStatusPausedError || updated[2].Status != contracts.JobStatusPausedError {
		t.Fatalf("expected doc 1 & 2 PAUSED_ERROR, got %s and %s", updated[1].Status, updated[2].Status)
	}
	if updated[1].OutcomeVersion == 0 || updated[2].OutcomeVersion == 0 {
		t.Fatalf("expected outcome_version bumped on paused jobs")
	}

	_, calibrated, err := st.GetPaperSupply(ctx)
	if err != nil {
		t.Fatalf("GetPaperSupply: %v", err)
	}
	if calibrated {
		t.Fatalf("expected printer_state.is_calibrated=false after uncertain failure")
	}

	// Recalibration must fail while job-1 is PAUSED_ERROR
	if err := st.CalibratePaperTray(ctx, 80); err == nil {
		t.Fatalf("expected CalibratePaperTray to reject while PAUSED_ERROR exists")
	}

	// Later reconcile job-1 to FAILED_REFUNDABLE -> doc 0 stays COMPLETED, doc 1 & 2 become FAILED_REFUNDABLE
	ts2 := ts1.Add(5 * time.Second)
	updated, err = st.FinalizeOrderFailure(ctx, "order-6b-1", "job-1", contracts.JobProgressEvent{
		JobID: "job-1", KioskID: "kiosk-1", Status: contracts.JobStatusFailedRefundable,
		PagesPrinted: 1, SheetsPrinted: 1, ErrorReason: "canceled and cleared", Timestamp: ts2,
	}, true)
	if err != nil {
		t.Fatalf("FinalizeOrderFailure FAILED_REFUNDABLE: %v", err)
	}
	if updated[0].Status != contracts.JobStatusCompleted || updated[1].Status != contracts.JobStatusFailedRefundable || updated[2].Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("unexpected statuses after finalization: %s, %s, %s", updated[0].Status, updated[1].Status, updated[2].Status)
	}

	// Recalibration now succeeds!
	if err := st.CalibratePaperTray(ctx, 80); err != nil {
		t.Fatalf("expected CalibratePaperTray to succeed after FAILED_REFUNDABLE: %v", err)
	}

	// Pending outcome sync tracking
	pending, err := st.GetPendingOutcomeSyncJobs(ctx)
	if err != nil || len(pending) != 3 {
		t.Fatalf("expected 3 pending outcome sync jobs, got %d (err=%v)", len(pending), err)
	}
	for _, p := range pending {
		if err := st.MarkOutcomeSynced(ctx, p.Command.JobID, p.OutcomeVersion); err != nil {
			t.Fatalf("MarkOutcomeSynced %s: %v", p.Command.JobID, err)
		}
	}
	pendingAfter, err := st.GetPendingOutcomeSyncJobs(ctx)
	if err != nil || len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending outcome sync jobs after ack, got %d (err=%v)", len(pendingAfter), err)
	}
}

func TestPackage6B_ConcurrentFailureAndSubmissionBarrier_IdempotentDuplicate(t *testing.T) {
	ctx := context.Background()
	st, err := OpenWithCapacity(filepath.Join(t.TempDir(), "state6b_conc.db"), 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity: %v", err)
	}
	defer st.Close()

	if err := st.CalibratePaperTray(ctx, 100); err != nil {
		t.Fatalf("CalibratePaperTray: %v", err)
	}

	makeCmd := func(id string, idx int) contracts.PrintJobCommand {
		return contracts.PrintJobCommand{
			JobID: id, OrderID: "ord-conc-6b", DocumentIndex: idx, TotalDocuments: 2,
			KioskID: "kiosk-1", DisplayCode: "5678", DownloadURL: "http://localhost/doc.pdf",
			SHA256Checksum: "abc", TotalPages: 2, TotalSheets: 2, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		}
	}
	if err := st.SaveJob(ctx, makeCmd("cjob-0", 0), contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob 0: %v", err)
	}
	if err := st.SaveJob(ctx, makeCmd("cjob-1", 1), contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob 1: %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Millisecond)
	// First FinalizeOrderFailure marks cjob-0 FAILED_REFUNDABLE and stops cjob-1
	res1, err := st.FinalizeOrderFailure(ctx, "ord-conc-6b", "cjob-0", contracts.JobProgressEvent{
		JobID: "cjob-0", KioskID: "kiosk-1", Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: ts,
	}, false)
	if err != nil {
		t.Fatalf("FinalizeOrderFailure 1: %v", err)
	}
	v1 := res1[0].OutcomeVersion

	// Duplicate identical FinalizeOrderFailure must be idempotent (no version bump)
	res2, err := st.FinalizeOrderFailure(ctx, "ord-conc-6b", "cjob-0", contracts.JobProgressEvent{
		JobID: "cjob-0", KioskID: "kiosk-1", Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: ts.Add(time.Second),
	}, false)
	if err != nil {
		t.Fatalf("FinalizeOrderFailure 2: %v", err)
	}
	if res2[0].OutcomeVersion != v1 {
		t.Fatalf("expected idempotent duplicate outcome_version=%d, got %d", v1, res2[0].OutcomeVersion)
	}

	// MarkJobSubmittingWithBaseline must reject both failed job and sibling in failed order
	if err := st.MarkJobSubmittingWithBaseline(ctx, "cjob-0", true, 100, "1.1", 8, ts); err == nil {
		t.Fatalf("expected MarkJobSubmittingWithBaseline to reject FAILED_REFUNDABLE job")
	}
	if err := st.MarkJobSubmittingWithBaseline(ctx, "cjob-1", true, 100, "1.1", 8, ts); err == nil {
		t.Fatalf("expected MarkJobSubmittingWithBaseline to reject sibling of FAILED_REFUNDABLE order")
	}

	// Mismatched orderID must fail validation
	if _, err := st.FinalizeOrderFailure(ctx, "wrong-order", "cjob-0", contracts.JobProgressEvent{
		JobID: "cjob-0", KioskID: "kiosk-1", Status: contracts.JobStatusFailedRefundable,
		ErrorReason: "download failed", Timestamp: ts,
	}, false); err == nil {
		t.Fatalf("expected FinalizeOrderFailure to reject mismatched orderID")
	}
}
