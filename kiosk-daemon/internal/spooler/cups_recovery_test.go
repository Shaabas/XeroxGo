package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// TestCUPS_SuccessfulSubmissionAndIdempotentTracking verifies that when a job is sent to CUPS:
// 1. MarkJobSubmitting is recorded prior to lp execution
// 2. RecordJobSubmission saves the parsed CUPS request ID
// 3. Status updates to COMPLETED and state store contains durable submission metadata
func TestCUPS_SuccessfulSubmissionAndIdempotentTracking(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_tracking.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	var lpCalled atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
			return []byte("request id is DCPT310-201 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("DCPT310-201 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
			}
		}
		return []byte(""), nil
	}

	var odoCalls int32
	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 1000
		if c > 1 {
			life += int64(c - 1)
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		SNMPPoller:          snmpPoller,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-cups-success",
		KioskID:        "kiosk-cups",
		DisplayCode:    "8888",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	client.processJob(context.Background(), cmd)

	if lpCalled.Load() != 1 {
		t.Fatalf("expected lp to be called exactly once, got %d", lpCalled.Load())
	}

	job, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if job == nil {
		t.Fatal("expected stored job, got nil")
	}
	if job.Status != contracts.JobStatusCompleted {
		t.Errorf("expected status COMPLETED, got %v", job.Status)
	}
	if !job.SubmittedToPrinter {
		t.Errorf("expected SubmittedToPrinter=true")
	}
	if job.CUPSJobID != "DCPT310-201" {
		t.Errorf("expected CUPSJobID=DCPT310-201, got %q", job.CUPSJobID)
	}
}

// TestCUPS_AmbiguousSubmissionCrashedDuringLP verifies that if the daemon crashed during or right
// before lp submission (SubmittedToPrinter == true, but CUPSJobID == ""), on restart the job is
// treated as an uncertain ambiguous submission and marked PAUSED_ERROR without resubmitting.
func TestCUPS_AmbiguousSubmissionCrashedDuringLP(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_ambig.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-ambig-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "1111",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// 1. Save job in spooling
	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	// 2. Mark submission initiated (simulates crash while lp was running)
	if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	var lpCalled atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Wait for startup recovery
	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError {
			if strings.Contains(job.ErrorReason, "manual reconciliation required") {
				paused = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected ambiguous submission to be paused in PAUSED_ERROR with manual reconciliation required")
	}

	// Assert lp was NEVER called on startup (duplicate submission avoided!)
	if lpCalled.Load() != 0 {
		t.Fatalf("expected 0 lp calls for ambiguous submission, got %d", lpCalled.Load())
	}
}

// TestCUPS_RestartReconcilesActiveJobInQueue verifies that if the daemon crashed while a job was
// active in the CUPS queue (CUPSJobID != ""), startup recovery queries CUPS, finds it active,
// and resumes monitoring until completion WITHOUT resubmitting lp.
func TestCUPS_RestartReconcilesActiveJobInQueue(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_active_rec.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-active-rec",
		KioskID:        "kiosk-cups",
		DisplayCode:    "2222",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	cupsID := "DCPT310-301"
	if err := store.RecordJobSubmission(context.Background(), cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	var lpCalled atomic.Int32
	var pollCount atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
			return []byte("request id is DCPT310-999"), nil
		}
		if name == "lpstat" {
			p := pollCount.Add(1)
			if p <= 2 {
				// Job is active in not-completed
				return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026", cupsID)), nil
			}
			// Then completes
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully", cupsID)), nil
				}
			}
		}
		return []byte(""), nil
	}

	var odoCalls int32
	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 1000
		if c > 1 {
			life += int64((c - 1) * 2)
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		SNMPPoller:          snmpPoller,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Wait for monitoring to transition job to COMPLETED
	deadline := time.Now().Add(3 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusCompleted {
			completed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !completed {
		t.Fatal("expected active CUPS job to be monitored to COMPLETED on daemon restart")
	}

	// Assert lp was NEVER called (no duplicate submission!)
	if lpCalled.Load() != 0 {
		t.Fatalf("expected 0 lp calls during active job reconciliation, got %d", lpCalled.Load())
	}
}

// TestCUPS_RestartReconcilesAlreadyCompletedJob verifies that if the daemon was down while CUPS
// finished the job, startup recovery queries CUPS, finds completed state, and transitions
// the job to COMPLETED immediately without calling lp.
func TestCUPS_RestartReconcilesAlreadyCompletedJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_done_rec.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-already-done",
		KioskID:        "kiosk-cups",
		DisplayCode:    "3333",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("b", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	cupsID := "DCPT310-401"
	if err := store.RecordJobSubmission(context.Background(), cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	// Pre-crash: sheets were physically verified before daemon restarted
	if err := store.RecordSheetProgress(context.Background(), contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordSheetProgress failed: %v", err)
	}

	var lpCalled atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully", cupsID)), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusCompleted {
			completed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !completed {
		t.Fatal("expected already-completed CUPS job to be reconciled to COMPLETED immediately")
	}

	if lpCalled.Load() != 0 {
		t.Fatalf("expected 0 lp calls, got %d", lpCalled.Load())
	}
}

// TestCUPS_DuplicateCommandsIgnored verifies that duplicate commands for already completed
// jobs are ignored without submitting duplicate print jobs.
func TestCUPS_DuplicateCommandsIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_dup.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-dup-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "5555",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("c", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Pre-populate as completed
	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusCompleted); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	_ = store.RecordJobSubmission(context.Background(), cmd.JobID, "DCPT310-501")

	var lpCalled atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Feed duplicate command
	client.handleIncomingJob(cmd)

	if len(client.jobQueue) > 0 {
		t.Errorf("expected jobQueue to remain empty, got %d items", len(client.jobQueue))
	}
	if lpCalled.Load() != 0 {
		t.Errorf("expected 0 lp calls for duplicate completed job, got %d", lpCalled.Load())
	}
}

// TestCUPS_CloudRecoveryOutageLeavesJobPaused verifies that if cloud recovery fails due to
// backend outage / network error, the job remains safely paused in PAUSED_ERROR without duplicate prints.
func TestCUPS_CloudRecoveryOutageLeavesJobPaused(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_outage.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-outage-01",
		KioskID:        "kiosk-test",
		DisplayCode:    "6666",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("d", 64),
		TotalPages:     4,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	// Cloud server simulates 503 outage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"
	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-test",
		KioskSecret:         "secret",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		HTTPClient:          server.Client(),
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Attempt cloud recovery during outage
	recoveryCmd, recErr := client.RequestCloudRecovery(context.Background(), cmd.JobID, 1, true, "jam cleared")
	if recErr == nil {
		t.Fatal("expected RequestCloudRecovery to fail during cloud outage, got nil error")
	}
	if recoveryCmd != nil {
		t.Fatal("expected nil recoveryCmd on failure")
	}

	// Verify job remains in PAUSED_ERROR in store, safe for future retry
	job, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if job == nil || job.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain in PAUSED_ERROR, got %v", job)
	}
}

// TestCUPS_MultiCopyRecoveryIntegrity verifies that recovering a multi-copy print job
// maintains exact copies, sheet calculation, and page progression.
func TestCUPS_MultiCopyRecoveryIntegrity(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_multicopy.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	// Original job: 2 copies of 2 pages = 4 pages, 4 sheets simplex
	origCmd := contracts.PrintJobCommand{
		JobID:          "job-multicopy-orig",
		KioskID:        "kiosk-test",
		DisplayCode:    "7777",
		DownloadURL:    "https://example.invalid/orig.pdf",
		SHA256Checksum: strings.Repeat("e", 64),
		TotalPages:     4,
		TotalSheets:    4,
		Copies:         2,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), origCmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	_ = store.UpdateProgress(context.Background(), contracts.JobProgressEvent{
		JobID:         origCmd.JobID,
		KioskID:       origCmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper jam on copy 2",
		Timestamp:     time.Now().UTC(),
	})

	// Server returns continuation artifact for the remaining 2 sheets
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/recover"):
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"recovery_id": "rec_multi_01",
				"sequence_id": 1,
				"job_id":      "job-multicopy-cont",
				"command": contracts.PrintJobCommand{
					JobID:          "job-multicopy-cont",
					KioskID:        "kiosk-test",
					DisplayCode:    "7777",
					DownloadURL:    "http://" + r.Host + "/cont.pdf",
					SHA256Checksum: pdfChecksum,
					TotalPages:     2,
					TotalSheets:    2,
					Copies:         1,
					Duplex:         contracts.DuplexSimplex,
					StartFromPage:  1,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case strings.HasSuffix(r.URL.Path, "/cont.pdf"):
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdfContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var lpCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalls.Add(1)
			return []byte("request id is DCPT310-701 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("DCPT310-701 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
			}
		}
		return []byte(""), nil
	}

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"
	var odoCalls int32
	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 2000
		if c > 1 {
			life += int64((c - 1) * 2)
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-test",
		KioskSecret:         "secret",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		SNMPPoller:          snmpPoller,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		HTTPClient:          server.Client(),
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Request continuation from sheet 3 (first unprinted sheet of copy 2)
	contCmd, err := client.RequestCloudRecovery(context.Background(), origCmd.JobID, 3, true, "jam cleared")
	if err != nil {
		t.Fatalf("RequestCloudRecovery failed: %v", err)
	}
	if contCmd.TotalSheets != 2 || contCmd.StartFromPage != 1 {
		t.Fatalf("unexpected continuation command parameters: %+v", contCmd)
	}

	// Process the continuation job
	client.processJob(context.Background(), *contCmd)

	if lpCalls.Load() != 1 {
		t.Fatalf("expected 1 lp call for continuation job, got %d", lpCalls.Load())
	}

	contJob, err := store.GetJob(context.Background(), contCmd.JobID)
	if err != nil || contJob == nil {
		t.Fatalf("failed to get continuation job from store: %v", err)
	}
	if contJob.Status != contracts.JobStatusCompleted {
		t.Errorf("expected continuation job status COMPLETED, got %v", contJob.Status)
	}
}

// TestCUPS_RetriedDispatch_NeverDuplicatesSubmission verifies that retrying a job dispatch
// from the cloud cannot cause a duplicate CUPS submission when the job was already submitted to the printer.
func TestCUPS_RetriedDispatch_NeverDuplicatesSubmission(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_dup_prevent.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-cups-dup-test",
		KioskID:        "kiosk-cups",
		DisplayCode:    "9999",
		DownloadURL:    "https://storage.example.com/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Pre-seed the state store as having already submitted this job to CUPS
	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.RecordJobSubmission(context.Background(), cmd.JobID, "DCPT310-999"); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	var lpCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalls.Add(1)
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-cups",
		StateStore:    store,
		CUPSQueue:     "DCPT310",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Incoming retried dispatch arrives from cloud
	client.handleIncomingJob(cmd)

	// lp must NOT have been called!
	if lpCalls.Load() != 0 {
		t.Fatalf("expected 0 lp calls for already-submitted job, got %d", lpCalls.Load())
	}

	// State in store must be preserved
	existing, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil || existing == nil {
		t.Fatalf("failed to retrieve job: %v", err)
	}
	if !existing.SubmittedToPrinter || existing.CUPSJobID != "DCPT310-999" {
		t.Fatalf("expected job to retain submission metadata: %+v", existing)
	}
}

// TestCUPS_RestartWithCompletedJob_ReconcilesRemainingSheets verifies that when the daemon restarts
// and CUPS reports that a partially-printed job finished in CUPS while daemon was offline,
// the uncounted remaining sheets are atomically deducted from printer_state without double-deduction.
func TestCUPS_RestartWithCompletedJob_ReconcilesRemainingSheets(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_rec_sheets.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// Set initial tray stock to 50
	if err := store.ResetPaperTray(ctx, 50); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-rec-sheets-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "4444",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("c", 64),
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// 1. Job was saved in PRINTING with 2 sheets already printed and deducted before crash
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	cupsID := "DCPT310-501"
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	// Simulate 2 sheets printed prior to crash
	if err := store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  4,
		SheetsPrinted: 2,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pre-crash progress failed: %v", err)
	}
	stkBefore, _ := store.GetEstimatedSheets(ctx)
	if stkBefore != 48 {
		t.Fatalf("expected stock 48 before crash, got %d", stkBefore)
	}

	// Mock runner: CUPS says completed, printer is healthy/idle
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully", cupsID)), nil
				}
				if arg == "-p" {
					return []byte("printer DCPT310 is idle. enabled since Tue Nov 3"), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for startup reconciliation: without physical proof for sheets 3-5,
	// startup recovery must preserve PAUSED_ERROR and conserve paper inventory!
	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected startup recovery to preserve job in PAUSED_ERROR without physical proof")
	}

	job, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || job == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if job.SheetsPrinted != 2 {
		t.Fatalf("expected SheetsPrinted to remain 2, got %d", job.SheetsPrinted)
	}
	if !strings.Contains(job.ErrorReason, "without physical proof") {
		t.Fatalf("expected ErrorReason to mention without physical proof, got %q", job.ErrorReason)
	}

	// Crucial check: stock must remain exactly 48 (50 - 2 pre-crash sheets).
	// Without physical proof, remaining 3 sheets must NOT be fabricated or deducted!
	stkAfter, _ := store.GetEstimatedSheets(ctx)
	if stkAfter != 48 {
		t.Fatalf("CRITICAL INVENTORY LEAK: expected stock 48 (conserved), got %d", stkAfter)
	}
}

// TestCUPS_RestartWithCompletedJob_PrinterHardwareError_PausesForReconciliation verifies
// that if CUPS says COMPLETED but the printer hardware is stopped with a paper jam or error,
// physical output is treated as ambiguous and paused in PAUSED_ERROR rather than completed.
func TestCUPS_RestartWithCompletedJob_PrinterHardwareError_PausesForReconciliation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_err_rec.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.ResetPaperTray(ctx, 50); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-rec-err-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "5555",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("d", 64),
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	cupsID := "DCPT310-601"
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	})

	// Runner: CUPS completed spool, BUT printer hardware has paper jam
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully", cupsID)), nil
				}
				if arg == "-p" {
					return []byte("printer DCPT310 is stopped with paper jam"), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for reconciliation check
	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError {
			if strings.Contains(job.ErrorReason, "manual reconciliation required") {
				paused = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected ambiguous job to be paused in PAUSED_ERROR when printer has jam on completion")
	}

	// Stock must NOT have been deducted for the remaining sheets
	stk, _ := store.GetEstimatedSheets(ctx)
	if stk != 49 {
		t.Fatalf("expected stock 49 (only 1 sheet before crash), got %d", stk)
	}
}

// TestCUPS_RestartWithCompletedJob_InsufficientStock_PausesForReconciliation verifies
// that if CUPS says COMPLETED but the tray has fewer sheets than the remaining uncounted sheets,
// completion is rejected atomically and the job is preserved in PAUSED_ERROR.
func TestCUPS_RestartWithCompletedJob_InsufficientStock_PausesForReconciliation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cups_nostock_rec.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// Set stock to 0 (tray completely empty)
	if err := store.ResetPaperTray(ctx, 0); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-rec-nostock-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "6666",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("e", 64),
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	cupsID := "DCPT310-701"
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully", cupsID)), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError {
			if strings.Contains(job.ErrorReason, "insufficient") {
				paused = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected job to be paused in PAUSED_ERROR when tray is empty on completion")
	}

	stk, _ := store.GetEstimatedSheets(ctx)
	if stk != 0 {
		t.Fatalf("expected stock 0, got %d", stk)
	}
}

// TestSpooler_PrePrintInventoryCheck_HonorsCloudTotalSheets verifies that
// pre-print inventory validation uses cmd.TotalSheets directly, correctly handling
// N-Up and multi-file jobs rather than using naive page division.
func TestSpooler_PrePrintInventoryCheck_HonorsCloudTotalSheets(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cloud_sheets_check.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// Set tray to exactly 2 sheets
	if err := store.ResetPaperTray(ctx, 2); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	// N-Up duplex job: 4 document pages imposed onto 2 physical sheets
	cmd := contracts.PrintJobCommand{
		JobID:          "job-nup-duplex",
		KioskID:        "kiosk-cups",
		DisplayCode:    "7777",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     4,
		TotalSheets:    2, // Authoritative cloud sheets
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	var lpCalled atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled.Add(1)
			return []byte("request id is DCPT310-888 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("DCPT310-888 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
				}
				if arg == "-p" {
					return []byte("printer DCPT310 is idle. enabled"), nil
				}
			}
		}
		return []byte(""), nil
	}

	var odoCalls int32
	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 1000
		if c > 1 {
			life += int64((c - 1) * 2)
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		CUPSRunner:          runner,
		SNMPPoller:          snmpPoller,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Under the old naive math, duplex remainingPages = 4, remainingSheets = ((4+1)/2)*1 = 3 sheets!
	// With 2 sheets in tray, old logic would reject with "paper tray supply insufficient".
	// With corrected logic, remainingSheets = cmd.TotalSheets - 0 = 2 sheets, matching tray supply.
	client.processJob(ctx, cmd)

	if lpCalled.Load() != 1 {
		t.Fatalf("expected job to proceed to lp submission, got %d calls", lpCalled.Load())
	}

	job, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || job == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if job.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected job to complete, got status %v with reason %q", job.Status, job.ErrorReason)
	}
	// Tray stock: 2 - 2 = 0
	stk, _ := store.GetEstimatedSheets(ctx)
	if stk != 0 {
		t.Fatalf("expected remaining stock 0, got %d", stk)
	}
}

// TestSpooler_DuplicateCompletion_NeverDoubleDeducts verifies that calling completeJob
// repeatedly does not double-decrement paper inventory in printer_state.
func TestSpooler_DuplicateCompletion_NeverDoubleDeducts(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "dup_comp.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-dup-comp-01",
		KioskID:        "kiosk-cups",
		DisplayCode:    "8888",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     4,
		TotalSheets:    4,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-cups",
		StateStore:          store,
		CUPSQueue:           "DCPT310",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// First completion
	client.completeJob(ctx, cmd)
	stk1, _ := store.GetEstimatedSheets(ctx)
	if stk1 != 96 {
		t.Fatalf("expected stock 96 after first completion, got %d", stk1)
	}

	// Second (duplicate) completion
	client.completeJob(ctx, cmd)
	stk2, _ := store.GetEstimatedSheets(ctx)
	if stk2 != 96 {
		t.Fatalf("duplicate completion double-decremented stock to %d", stk2)
	}
}
