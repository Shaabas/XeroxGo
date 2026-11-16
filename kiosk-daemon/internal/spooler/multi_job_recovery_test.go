package spooler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func sampleCommand(jobID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-multi-test",
		DisplayCode:    "1234",
		DownloadURL:    "https://storage.example.com/" + jobID + ".pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
}

// TestMultiJobRecovery_FourJobsMixedStates verifies Requirements 4, 5, 6, 7, 8, 9, 10, 11, 12, 13:
// 1. All 4 incomplete records are examined without loss or silent completion.
// 2. Job 1 (PAUSED_ERROR, held in CUPS): preserved in PAUSED_ERROR with original CUPS ID, immutable settings, and verified sheets.
// 3. Job 2 (PRINTING, CUPS completed without physical proof): safely demoted to PAUSED_ERROR (no silent completion).
// 4. Job 3 (ambiguous submission, no CUPS ID): safely paused in PAUSED_ERROR without resubmission.
// 5. Job 4 (definitely unsubmitted): deferred safely rather than immediately printed because unresolved jobs exist.
func TestMultiJobRecovery_FourJobsMixedStates(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_job_4_mixed.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Seed 4 jobs with deterministic timestamps:
	// Job 1: PAUSED_ERROR with CUPS ID DCPT310-101, 1 sheet verified out of 2. CUPS state: pending-held.
	cmd1 := sampleCommand("job-1-paused-held")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob 1 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-101")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd1.JobID,
		KioskID:       cmd1.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		ErrorReason:   "paper jam during print",
		Timestamp:     time.Now().Add(-40 * time.Minute),
	})

	// Job 2: PRINTING with CUPS ID DCPT310-102, 0 verified physical sheets. CUPS reports completed.
	cmd2 := sampleCommand("job-2-printing-cups-done")
	cmd2.TotalSheets = 1
	cmd2.TotalPages = 1
	if err := store.SaveJob(ctx, cmd2, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob 2 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd2.JobID)
	_ = store.RecordJobSubmission(ctx, cmd2.JobID, queueName+"-102")
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd2.JobID,
		KioskID:       cmd2.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		Timestamp:     time.Now().Add(-30 * time.Minute),
	})

	// Job 3: Ambiguous submission intent (submitted=1, no cups ID).
	cmd3 := sampleCommand("job-3-ambiguous-lp-crash")
	cmd3.TotalSheets = 1
	cmd3.TotalPages = 1
	if err := store.SaveJob(ctx, cmd3, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob 3 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd3.JobID)
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:     cmd3.JobID,
		KioskID:   cmd3.KioskID,
		Status:    contracts.JobStatusSpooling,
		Timestamp: time.Now().Add(-20 * time.Minute),
	})

	// Job 4: Definitely unsubmitted (submitted=0, cups_id="", 0 sheets).
	cmd4 := sampleCommand("job-4-definitely-unsubmitted")
	cmd4.TotalSheets = 1
	cmd4.TotalPages = 1
	if err := store.SaveJob(ctx, cmd4, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob 4 failed: %v", err)
	}
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:     cmd4.JobID,
		KioskID:   cmd4.KioskID,
		Status:    contracts.JobStatusSpooling,
		Timestamp: time.Now().Add(-10 * time.Minute),
	})

	// Mock CUPS runner:
	// - DCPT310-101: pending-held, 1 sheet completed
	// - DCPT310-102: completed, 1 sheet in CUPS counter (but 0 verified physical)
	var lpPrintCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for i, arg := range args {
				if arg == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					return []byte(""), nil
				}
			}
			lpPrintCalls.Add(1)
			return []byte("request id is DCPT310-999 (1 file(s))"), nil
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "/jobs/101") {
					return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 1\n"), nil
				}
				if strings.Contains(arg, "/jobs/102") {
					return []byte("job-state (enum) = completed\njob-media-sheets-completed (integer) = 1\n"), nil
				}
			}
		}
		if name == "lpstat" {
			return []byte(""), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-multi-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(1 * time.Second)

	// Requirement 13: Verify every single record is examined
	// Job 1 assertions:
	j1, err := store.GetJob(ctx, cmd1.JobID)
	if err != nil || j1 == nil {
		t.Fatalf("failed to get Job 1: %v", err)
	}
	if j1.Status != contracts.JobStatusPausedError {
		t.Errorf("Job 1: expected status PAUSED_ERROR, got %s", j1.Status)
	}
	if j1.CUPSJobID != queueName+"-101" {
		t.Errorf("Job 1: expected CUPS ID %s-101 preserved, got %s", queueName, j1.CUPSJobID)
	}
	if j1.SheetsPrinted != 1 {
		t.Errorf("Job 1: expected 1 verified sheet preserved, got %d", j1.SheetsPrinted)
	}
	if !strings.Contains(j1.ErrorReason, "pending-held") {
		t.Errorf("Job 1: expected error reason mentioning pending-held, got: %s", j1.ErrorReason)
	}

	// Job 2 assertions:
	j2, err := store.GetJob(ctx, cmd2.JobID)
	if err != nil || j2 == nil {
		t.Fatalf("failed to get Job 2: %v", err)
	}
	if j2.Status != contracts.JobStatusPausedError {
		t.Errorf("Job 2: expected status PAUSED_ERROR (missing physical proof), got %s", j2.Status)
	}
	if j2.CUPSJobID != queueName+"-102" {
		t.Errorf("Job 2: expected CUPS ID %s-102 preserved, got %s", queueName, j2.CUPSJobID)
	}
	if !strings.Contains(j2.ErrorReason, "physical evidence insufficient") {
		t.Errorf("Job 2: expected insufficient physical proof reason, got: %s", j2.ErrorReason)
	}

	// Job 3 assertions:
	j3, err := store.GetJob(ctx, cmd3.JobID)
	if err != nil || j3 == nil {
		t.Fatalf("failed to get Job 3: %v", err)
	}
	if j3.Status != contracts.JobStatusPausedError {
		t.Errorf("Job 3: expected status PAUSED_ERROR, got %s", j3.Status)
	}
	if !strings.Contains(j3.ErrorReason, "interrupted during printer submission") {
		t.Errorf("Job 3: expected interrupted submission error, got: %s", j3.ErrorReason)
	}

	// Job 4 assertions:
	// Requirement 9: Definitely unsubmitted job must be deferred, NOT dispatched to printer
	j4, err := store.GetJob(ctx, cmd4.JobID)
	if err != nil || j4 == nil {
		t.Fatalf("failed to get Job 4: %v", err)
	}
	if j4.SubmittedToPrinter {
		t.Errorf("safety violation: Job 4 submitted to printer despite unresolved jobs: %+v", j4)
	}
	if j4.CUPSJobID != "" {
		t.Errorf("safety violation: Job 4 assigned CUPS ID despite unresolved jobs: %s", j4.CUPSJobID)
	}

	// Check client memory: Job 4 must be deferred in client.deferredJobs
	client.deferredMu.Lock()
	foundDeferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd4.JobID {
			foundDeferred = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !foundDeferred {
		t.Errorf("expected Job 4 to be deferred in client.deferredJobs, but not found")
	}

	// Requirement 6: Zero print submissions to lp occurred during recovery
	if lpPrintCalls.Load() != 0 {
		t.Errorf("safety violation: expected 0 print lp calls during recovery, got %d", lpPrintCalls.Load())
	}
}

// TestMultiJobRecovery_MultipleActiveCUPSIDs_NoConcurrentPrinting verifies Requirement 8 & 12:
// When multiple jobs have active CUPS IDs (e.g. Job 1 is processing, Job 2 is pending in CUPS):
// 1. At most ONE job resumes monitoring.
// 2. Additional active jobs are placed on hold in CUPS to prevent concurrent printing.
// 3. Definitely-unsubmitted jobs are deferred.
func TestMultiJobRecovery_MultipleActiveCUPSIDs_NoConcurrentPrinting(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_active_cups.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: PRINTING, CUPS ID DCPT310-201, processing in CUPS
	cmd1 := sampleCommand("job-1-active-processing")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob 1 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-201")

	// Job 2: PRINTING, CUPS ID DCPT310-202, pending in CUPS
	cmd2 := sampleCommand("job-2-active-pending")
	cmd2.TotalSheets = 2
	cmd2.TotalPages = 2
	if err := store.SaveJob(ctx, cmd2, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob 2 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd2.JobID)
	_ = store.RecordJobSubmission(ctx, cmd2.JobID, queueName+"-202")

	// Job 3: SPOOLING, definitely unsubmitted
	cmd3 := sampleCommand("job-3-unsubmitted")
	if err := store.SaveJob(ctx, cmd3, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob 3 failed: %v", err)
	}

	var holdCalled atomic.Int32
	var heldJobIDs []string
	var holdMu sync.Mutex

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for i, arg := range args {
				if arg == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					holdCalled.Add(1)
					holdMu.Lock()
					if i > 1 && args[i-2] == "-i" {
						heldJobIDs = append(heldJobIDs, args[i-1])
					}
					holdMu.Unlock()
					return []byte(""), nil
				}
			}
			return []byte("request id is DCPT310-999 (1 file(s))"), nil
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "/jobs/201") {
					return []byte("job-state (enum) = processing\njob-media-sheets-completed (integer) = 0\n"), nil
				}
				if strings.Contains(arg, "/jobs/202") {
					return []byte("job-state (enum) = pending\njob-media-sheets-completed (integer) = 0\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-multi-active",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(1 * time.Second)

	// Requirement 8: At most ONE job resumes monitoring
	client.activeMu.Lock()
	activeCount := len(client.activeJobs)
	isJob1Active := client.activeJobs[cmd1.JobID]
	isJob2Active := client.activeJobs[cmd2.JobID]
	client.activeMu.Unlock()

	if activeCount > 1 {
		t.Fatalf("concurrency violation: expected at most 1 active job, got %d", activeCount)
	}
	if !isJob1Active {
		t.Errorf("expected Job 1 (%s) to be actively resumed", cmd1.JobID)
	}
	if isJob2Active {
		t.Errorf("concurrency violation: Job 2 (%s) was also activated concurrently", cmd2.JobID)
	}

	// Job 2 was placed on hold in CUPS and recorded as PAUSED_ERROR
	j2, _ := store.GetJob(ctx, cmd2.JobID)
	if j2 == nil || j2.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected Job 2 to be paused in PAUSED_ERROR, got: %+v", j2)
	}
	if !strings.Contains(j2.ErrorReason, "multiple active CUPS jobs detected") {
		t.Errorf("expected multiple active CUPS jobs reason, got: %s", j2.ErrorReason)
	}

	// Hold request was executed for Job 2
	if holdCalled.Load() < 1 {
		t.Errorf("expected hold request for extra active job, got %d calls", holdCalled.Load())
	}

	// Job 3 is deferred safely
	client.deferredMu.Lock()
	job3Deferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd3.JobID {
			job3Deferred = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !job3Deferred {
		t.Errorf("expected unsubmitted Job 3 to be deferred while active job is printing")
	}
}

// TestMultiJobRecovery_JobLevelFailure_ContinuesExaminingRemainingRecords verifies Requirement 15:
// When an error occurs while reconciling Job 1 (e.g. CUPS query error), the recovery loop
// DOES NOT abort and continues examining Job 2 and Job 3.
func TestMultiJobRecovery_JobLevelFailure_ContinuesExaminingRemainingRecords(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_job_failure.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: PAUSED_ERROR with broken CUPS job (will fail query)
	cmd1 := sampleCommand("job-1-failing-query")
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob 1 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-401")
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:       cmd1.JobID,
		KioskID:     cmd1.KioskID,
		Status:      contracts.JobStatusPausedError,
		ErrorReason: "interrupted",
		Timestamp:   time.Now().UTC(),
	})

	// Job 2: PAUSED_ERROR with valid CUPS job (will succeed query)
	cmd2 := sampleCommand("job-2-valid-held")
	if err := store.SaveJob(ctx, cmd2, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob 2 failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd2.JobID)
	_ = store.RecordJobSubmission(ctx, cmd2.JobID, queueName+"-402")
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:       cmd2.JobID,
		KioskID:     cmd2.KioskID,
		Status:      contracts.JobStatusPausedError,
		ErrorReason: "interrupted",
		Timestamp:   time.Now().UTC(),
	})

	// Job 3: SPOOLING, unsubmitted
	cmd3 := sampleCommand("job-3-unsubmitted")
	if err := store.SaveJob(ctx, cmd3, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob 3 failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "/jobs/401") {
					return nil, fmt.Errorf("connection refused: mock cups down")
				}
				if strings.Contains(arg, "/jobs/402") {
					return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 0\n"), nil
				}
			}
		}
		if name == "lpstat" {
			return nil, fmt.Errorf("lpstat error: mock cups down")
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-failure-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(1 * time.Second)

	// Job 1 failed query, but was preserved in PAUSED_ERROR with error notice
	j1, _ := store.GetJob(ctx, cmd1.JobID)
	if j1 == nil || j1.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected Job 1 to remain in PAUSED_ERROR, got: %+v", j1)
	}
	if !strings.Contains(j1.ErrorReason, "error querying CUPS") {
		t.Errorf("expected Job 1 error reason mentioning CUPS query error, got: %s", j1.ErrorReason)
	}

	// Requirement 15: Job 2 was STILL successfully examined and reconciled
	j2, _ := store.GetJob(ctx, cmd2.JobID)
	if j2 == nil || j2.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected Job 2 to be examined and preserved in PAUSED_ERROR, got: %+v", j2)
	}
	if !strings.Contains(j2.ErrorReason, "pending-held") {
		t.Errorf("expected Job 2 error reason mentioning pending-held, got: %s", j2.ErrorReason)
	}

	// Job 3 was STILL examined and deferred
	client.deferredMu.Lock()
	job3Found := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd3.JobID {
			job3Found = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !job3Found {
		t.Errorf("expected Job 3 to be deferred despite Job 1 query failure")
	}
}

// TestMultiJobRecovery_RepeatedStartupRecovery_Idempotent verifies Requirement 14:
// Running startup recovery multiple times on the same database is strictly idempotent.
func TestMultiJobRecovery_RepeatedStartupRecovery_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_idempotent.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	cmd1 := sampleCommand("job-idem-1")
	cmd1.TotalSheets = 3
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusPausedError)
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-301")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd1.JobID,
		KioskID:       cmd1.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "jam",
		Timestamp:     time.Now().Add(-10 * time.Minute),
	})

	cmd2 := sampleCommand("job-idem-2")
	_ = store.SaveJob(ctx, cmd2, contracts.JobStatusSpooling)

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 2\n"), nil
		}
		return []byte(""), nil
	}

	cfg := ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-idem-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          runner,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	}

	// Run Pass 1
	c1, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient 1 failed: %v", err)
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	go func() { _ = c1.Run(ctx1) }()
	time.Sleep(500 * time.Millisecond)

	j1AfterPass1, _ := store.GetJob(ctx, cmd1.JobID)
	j2AfterPass1, _ := store.GetJob(ctx, cmd2.JobID)

	// Run Pass 2 on the same store
	c2, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient 2 failed: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	go func() { _ = c2.Run(ctx2) }()
	time.Sleep(500 * time.Millisecond)

	j1AfterPass2, _ := store.GetJob(ctx, cmd1.JobID)
	j2AfterPass2, _ := store.GetJob(ctx, cmd2.JobID)

	// State must be completely identical between passes
	if j1AfterPass1.Status != j1AfterPass2.Status || j1AfterPass1.SheetsPrinted != j1AfterPass2.SheetsPrinted || j1AfterPass1.CUPSJobID != j1AfterPass2.CUPSJobID {
		t.Errorf("Job 1 mutated between recovery passes: pass 1=%+v, pass 2=%+v", j1AfterPass1, j1AfterPass2)
	}
	if j2AfterPass1.Status != j2AfterPass2.Status || j2AfterPass1.SubmittedToPrinter != j2AfterPass2.SubmittedToPrinter {
		t.Errorf("Job 2 mutated between recovery passes: pass 1=%+v, pass 2=%+v", j2AfterPass1, j2AfterPass2)
	}
}

// TestMultiJobRecovery_DatabaseErrorHandling verifies Requirement 14:
// When the database encounters an error during startup recovery, recovery fails closed safely.
func TestMultiJobRecovery_DatabaseErrorHandling(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_db_err.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	// Close store immediately to induce database error on query
	store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-dberr-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelRecov()

	// Must not panic or crash
	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(300 * time.Millisecond)

	client.activeMu.Lock()
	activeCount := len(client.activeJobs)
	client.activeMu.Unlock()

	if activeCount != 0 {
		t.Errorf("expected 0 active jobs on database error, got %d", activeCount)
	}
}

// TestMultiJobRecovery_DefinitelyUnsubmittedWithNoUnresolvedJobs_AdmitsSequentially verifies Requirement 8 & 9:
// When 2 definitely-unsubmitted jobs exist and ZERO unresolved jobs exist:
// 1. The first job is admitted to c.jobQueue.
// 2. The second job is deferred to c.deferredJobs.
// 3. No concurrent printing occurs.
func TestMultiJobRecovery_DefinitelyUnsubmittedWithNoUnresolvedJobs_AdmitsSequentially(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_unsubmitted_sequential.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 sample"))
	}))
	defer pdfServer.Close()

	cmdA := sampleCommand("job-A-unsubmitted")
	cmdA.DownloadURL = pdfServer.URL + "/jobA.pdf"
	_ = store.SaveJob(ctx, cmdA, contracts.JobStatusSpooling)
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:     cmdA.JobID,
		KioskID:   cmdA.KioskID,
		Status:    contracts.JobStatusSpooling,
		Timestamp: time.Now().Add(-10 * time.Minute),
	})

	cmdB := sampleCommand("job-B-unsubmitted")
	cmdB.DownloadURL = pdfServer.URL + "/jobB.pdf"
	_ = store.SaveJob(ctx, cmdB, contracts.JobStatusSpooling)
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:     cmdB.JobID,
		KioskID:   cmdB.KioskID,
		Status:    contracts.JobStatusSpooling,
		Timestamp: time.Now().Add(-5 * time.Minute),
	})

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-seq-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(300 * time.Millisecond)

	// Check active and deferred:
	// Job A was admitted to queue
	client.activeMu.Lock()
	isJobAActive := client.activeJobs[cmdA.JobID]
	isJobBActive := client.activeJobs[cmdB.JobID]
	totalActive := len(client.activeJobs)
	client.activeMu.Unlock()

	if totalActive > 1 {
		t.Fatalf("concurrency violation: expected at most 1 active job, got %d", totalActive)
	}
	if !isJobAActive {
		t.Errorf("expected Job A to be admitted to active queue")
	}
	if isJobBActive {
		t.Errorf("concurrency violation: Job B was also admitted concurrently")
	}

	// Job B is deferred in c.deferredJobs
	client.deferredMu.Lock()
	jobBDeferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmdB.JobID {
			jobBDeferred = true
			break
		}
	}
	client.deferredMu.Unlock()

	if !jobBDeferred {
		t.Errorf("expected Job B to be deferred for sequential processing")
	}
}

// TestMultiJobRecovery_CommandReplayPostRecovery verifies Requirement 14:
// Tests command replay post-recovery for jobs in various states.
func TestMultiJobRecovery_CommandReplayPostRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "multi_replay.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 sample"))
	}))
	defer pdfServer.Close()

	// Seed completed job
	cmdComp := sampleCommand("job-replay-comp")
	cmdComp.DownloadURL = pdfServer.URL + "/comp.pdf"
	_ = store.SaveJob(ctx, cmdComp, contracts.JobStatusCompleted)
	_ = store.RecordJobSubmission(ctx, cmdComp.JobID, "DCPT310-901")

	// Seed paused job
	cmdPaused := sampleCommand("job-replay-paused")
	cmdPaused.DownloadURL = pdfServer.URL + "/paused.pdf"
	_ = store.SaveJob(ctx, cmdPaused, contracts.JobStatusPausedError)
	_ = store.MarkJobSubmitting(ctx, cmdPaused.JobID)
	_ = store.RecordJobSubmission(ctx, cmdPaused.JobID, "DCPT310-902")
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:       cmdPaused.JobID,
		KioskID:     cmdPaused.KioskID,
		Status:      contracts.JobStatusPausedError,
		ErrorReason: "interrupted",
		Timestamp:   time.Now().UTC(),
	})

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-replay-test",
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

	// 1. Replay completed job: must acknowledge without altering state or resubmitting
	client.handleIncomingJob(cmdComp)
	jComp, _ := store.GetJob(ctx, cmdComp.JobID)
	if jComp.Status != contracts.JobStatusCompleted {
		t.Errorf("expected completed job to remain COMPLETED, got: %s", jComp.Status)
	}

	// 2. Replay paused job: must acknowledge without altering state or resubmitting
	client.handleIncomingJob(cmdPaused)
	jPaused, _ := store.GetJob(ctx, cmdPaused.JobID)
	if jPaused.Status != contracts.JobStatusPausedError {
		t.Errorf("expected paused job to remain PAUSED_ERROR, got: %s", jPaused.Status)
	}

	// 3. Conflicting replay (mismatched checksum): must reject and preserve stored job
	conflictCmd := cmdComp
	conflictCmd.SHA256Checksum = strings.Repeat("f", 64)
	client.handleIncomingJob(conflictCmd)
	jAfterConflict, _ := store.GetJob(ctx, cmdComp.JobID)
	if jAfterConflict.Status != contracts.JobStatusCompleted || jAfterConflict.Command.SHA256Checksum == conflictCmd.SHA256Checksum {
		t.Errorf("conflicting replay corrupted stored job: %+v", jAfterConflict)
	}
}
