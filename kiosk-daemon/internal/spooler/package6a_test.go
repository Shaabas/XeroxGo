package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func TestPackage6A_BaselineDurabilityAndResumedMonitoringIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "package6a.db")
	st, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("open store failed: %v", err)
	}
	ctx := context.Background()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-6a-resume",
		OrderID:        "ord-6a-resume",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-6a",
		DisplayCode:    "6060",
		DownloadURL:    "https://example.invalid/doc.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     3,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	obsAt := time.Now().UTC().Truncate(time.Second)
	if err := st.MarkJobSubmittingWithBaseline(ctx, cmd.JobID, true, 5000, "1.1", 7, obsAt); err != nil {
		t.Fatalf("MarkJobSubmittingWithBaseline failed: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, cmd.JobID, "HPTank-661"); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	// Simulate 1 sheet already recorded before crash
	if err := st.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID: cmd.JobID, KioskID: cmd.KioskID, Status: contracts.JobStatusPrinting,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordSheetProgress(1) failed: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen SQLite store: original baseline MUST survive close/reopen.
	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store failed: %v", err)
	}
	defer st2.Close()

	reopened, err := st2.GetJob(ctx, cmd.JobID)
	if err != nil || reopened == nil {
		t.Fatalf("GetJob after reopen failed: %v", err)
	}
	if !reopened.BaselineCounterSet || reopened.BaselineCounterValue != 5000 || reopened.BaselineCounterIndex != "1.1" || reopened.BaselineCounterUnit != 7 {
		t.Fatalf("original baseline did not survive close/reopen: %+v", reopened)
	}

	var lpCalled atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Store(true)
		}
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-661 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	// Hardware counter is at 5003 (3 impressions from original baseline 5000).
	// If resumed monitoring wrongly replaced baseline with 5003, delta would be 0 and fail!
	poller := func(ctx context.Context) printer.HardwareTelemetry {
		return printer.HardwareTelemetry{
			Online:         true,
			HasLife:        true,
			LifeCount:      5003,
			CounterIndex:   "1.1",
			CounterUnit:    7,
			HasCounterMeta: true,
			HasFlags:       true,
			StatusState:    "IDLE",
			ObservedAt:     time.Now().UTC(),
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-6a",
		StateStore:       st2,
		CUPSQueue:        "HPTank",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://kiosk.test/ready",
		ScreenSink:       func(*image.RGBA) error { return nil },
		CUPSRunner:       runner,
		SNMPPoller:       poller,
		MockCUPSHealth:   testHealthyCUPSHealth,
		MockIPPState:     testHealthyIPPState,
		MockSNMPHealth:   testHealthySNMPHealth,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.resumeCUPSMonitoring(ctx, reopened)

	if lpCalled.Load() {
		t.Fatalf("resumed monitoring must NEVER resubmit via lp")
	}
	finalJob, _ := st2.GetJob(ctx, cmd.JobID)
	if finalJob == nil || finalJob.Status != contracts.JobStatusCompleted || finalJob.SheetsPrinted != 3 || finalJob.PagesPrinted != 3 {
		t.Fatalf("expected COMPLETED 3/3 using original baseline across restart, got %+v", finalJob)
	}
	// Stock started at 150 (openCalibratedTestStore default); 1 sheet deducted before restart + 2 remaining = 147 (idempotent, not 146!).
	remSheets, _ := st2.GetEstimatedSheets(ctx)
	if remSheets != 147 {
		t.Fatalf("expected idempotent inventory 147 sheets (150 - 3), got %d", remSheets)
	}
}

func TestPackage6A_AmbiguousSubmissionNeverReplays(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "package6a_ambig.db")
	st, err := openCalibratedTestStore(t, dbPath)
	if err != nil {
		t.Fatalf("open store failed: %v", err)
	}
	defer st.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfContent)
	checksum := hex.EncodeToString(sum[:])

	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfSrv.Close()

	var lpCount atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCount.Add(1)
			return []byte("ambiguous stdout without request id"), nil
		}
		return []byte(""), nil
	}
	poller := func(ctx context.Context) printer.HardwareTelemetry {
		return printer.HardwareTelemetry{
			Online: true, HasLife: true, LifeCount: 3000, CounterIndex: "1.1", CounterUnit: 7, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC(),
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-6a-ambig",
		StateStore:       st,
		CUPSQueue:        "HPTank",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://kiosk.test/ready",
		ScreenSink:       func(*image.RGBA) error { return nil },
		HTTPClient:       pdfSrv.Client(),
		CUPSRunner:       runner,
		SNMPPoller:       poller,
		MockCUPSHealth:   testHealthyCUPSHealth,
		MockIPPState:     testHealthyIPPState,
		MockSNMPHealth:   testHealthySNMPHealth,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID: "job-6a-ambig", OrderID: "ord-6a-ambig", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-6a-ambig", DisplayCode: "6161", DownloadURL: pdfSrv.URL + "/doc.pdf", SHA256Checksum: checksum,
		TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
	}
	ctx := context.Background()
	client.processJob(ctx, cmd)
	// Second invocation must refuse to replay after ambiguous submission!
	client.processJob(ctx, cmd)

	if lpCount.Load() != 1 {
		t.Fatalf("expected lp invoked at most once on ambiguous submission, got %d", lpCount.Load())
	}
	saved, _ := st.GetJob(ctx, cmd.JobID)
	if saved == nil || !saved.SubmittedToPrinter || saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected SubmittedToPrinter=true and PAUSED_ERROR, got %+v", saved)
	}
}

func TestPackage6A_StartupReconciliation_CannotBypassBaselineOrVerification(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "package6a_rec.db"))
	if err != nil {
		t.Fatalf("open store failed: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	cmd := contracts.PrintJobCommand{
		JobID: "job-6a-nobase", OrderID: "ord-6a-nobase", DocumentIndex: 0, TotalDocuments: 1,
		KioskID: "kiosk-6a", DisplayCode: "7777", DownloadURL: "https://example.invalid/doc.pdf",
		SHA256Checksum: strings.Repeat("b", 64), TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1,
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	// Mark submitted WITHOUT baseline
	if err := st.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}
	if err := st.RecordJobSubmission(ctx, cmd.JobID, "HPTank-777"); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-777 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n"), nil
				}
			}
		}
		return []byte(""), nil
	}
	poller := func(ctx context.Context) printer.HardwareTelemetry {
		return printer.HardwareTelemetry{
			Online: true, HasLife: true, LifeCount: 1002, CounterIndex: "1.1", CounterUnit: 8, HasCounterMeta: true, HasFlags: true, StatusState: "IDLE", ObservedAt: time.Now().UTC(),
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-6a",
		StateStore:       st,
		CUPSQueue:        "HPTank",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://kiosk.test/ready",
		ScreenSink:       func(*image.RGBA) error { return nil },
		CUPSRunner:       runner,
		SNMPPoller:       poller,
		MockCUPSHealth:   testHealthyCUPSHealth,
		MockIPPState:     testHealthyIPPState,
		MockSNMPHealth:   testHealthySNMPHealth,
		CapabilityPolicy: &printer.PrinterCapabilityPolicy{RequireA4: true, AllowMultiCopy: true},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()
	go func() {
		_ = client.Run(runCtx)
	}()

	paused := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, err := st.GetJob(ctx, cmd.JobID)
		if err == nil && j != nil && (j.Status == contracts.JobStatusPausedError || j.Status == contracts.JobStatusFailedRefundable) {
			paused = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	runCancel()

	finalJob, err := st.GetJob(ctx, cmd.JobID)
	if !paused || err != nil || finalJob == nil || finalJob.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected FAILED_REFUNDABLE when baseline is missing on quiescent startup reconciliation, got %+v (err=%v)", finalJob, err)
	}
	if !strings.Contains(finalJob.ErrorReason, "counter baseline") {
		t.Fatalf("expected missing counter baseline error reason, got %q", finalJob.ErrorReason)
	}
	remSheets, _ := st.GetEstimatedSheets(ctx)
	if remSheets != 150 {
		t.Fatalf("expected inventory conserved at 150, got %d", remSheets)
	}
}
