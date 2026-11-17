package spooler

import (
	"context"
	"database/sql"
	"fmt"
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
	_ "modernc.org/sqlite"

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
	var is202Held atomic.Bool
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
						if strings.Contains(args[i-1], "202") {
							is202Held.Store(true)
						}
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
					if is202Held.Load() {
						return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 0\n"), nil
					}
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

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-seq-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for Job A admission to active queue
	var isJobAActive, isJobBActive bool
	var totalActive int
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(10 * time.Millisecond) {
		client.activeMu.Lock()
		isJobAActive = client.activeJobs[cmdA.JobID]
		isJobBActive = client.activeJobs[cmdB.JobID]
		totalActive = len(client.activeJobs)
		client.activeMu.Unlock()
		if isJobAActive {
			break
		}
	}

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
	jobBDeferred := false
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(10 * time.Millisecond) {
		client.deferredMu.Lock()
		jobBDeferred = false
		for _, dj := range client.deferredJobs {
			if dj.JobID == cmdB.JobID {
				jobBDeferred = true
				break
			}
		}
		client.deferredMu.Unlock()
		if jobBDeferred {
			break
		}
	}

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

// TestMultiJobRecovery_Negative_NewCommandDuringUnresolvedRecovery verifies Requirements 2 & 4:
// When an unresolved job exists in SQLite, fresh incoming print commands are blocked by the print barrier,
// while duplicate commands for existing jobs continue to be safely acknowledged.
func TestMultiJobRecovery_Negative_NewCommandDuringUnresolvedRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unresolved_barrier.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed unresolved job in PAUSED_ERROR
	unresCmd := sampleCommand("job-unresolved-1")
	if err := store.SaveJob(ctx, unresCmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, unresCmd.JobID)
	_ = store.RecordJobSubmission(ctx, unresCmd.JobID, "DCPT310-501")
	_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:       unresCmd.JobID,
		KioskID:     unresCmd.KioskID,
		Status:      contracts.JobStatusPausedError,
		ErrorReason: "paper jam; operator reconciliation required",
		Timestamp:   time.Now().UTC(),
	})

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-barrier-test",
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

	// 1. Fresh new incoming command arrives: MUST be rejected by unresolved print barrier
	freshCmd := sampleCommand("job-fresh-new")
	client.handleIncomingJob(freshCmd)

	storedFresh, _ := store.GetJob(ctx, freshCmd.JobID)
	if storedFresh != nil {
		t.Fatalf("security violation: fresh command was saved to store while unresolved jobs exist: %+v", storedFresh)
	}

	client.activeMu.Lock()
	freshIsActive := client.activeJobs[freshCmd.JobID]
	client.activeMu.Unlock()
	if freshIsActive {
		t.Fatalf("security violation: fresh command was marked active while unresolved jobs exist")
	}

	// 2. Safe duplicate acknowledgment of existing unresolved job: MUST be permitted without resubmission
	client.handleIncomingJob(unresCmd)
	storedUnres, _ := store.GetJob(ctx, unresCmd.JobID)
	if storedUnres.Status != contracts.JobStatusPausedError {
		t.Errorf("expected duplicate unresCmd to remain PAUSED_ERROR, got: %s", storedUnres.Status)
	}
}

// TestMultiJobRecovery_Negative_StartupDBFailure_EntersFailClosedBarrier verifies Requirements 5 & 6:
// If startup GetIncompleteJobs fails, the client enters durable fail-closed mode and does not accept printing.
func TestMultiJobRecovery_Negative_StartupDBFailure_EntersFailClosedBarrier(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db_failure_barrier.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	ctx := context.Background()
	cmd := sampleCommand("job-pre-seeded")
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusSpooling)

	// Close database to force startup query failure
	store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-db-fail",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelRecov()
	_ = client.Run(recovCtx)

	// Assert: durable in-process fail-closed recovery barrier is active
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Fatalf("expected recovery barrier to be active on startup DB failure")
	}
	if !strings.Contains(reason, "database error") {
		t.Errorf("expected barrier reason mentioning database error, got: %s", reason)
	}

	// Incoming new print command must be rejected
	newCmd := sampleCommand("job-new-during-db-barrier")
	client.handleIncomingJob(newCmd)

	client.activeMu.Lock()
	isActive := client.activeJobs[newCmd.JobID]
	client.activeMu.Unlock()
	if isActive {
		t.Errorf("expected new command to be rejected by fail-closed barrier")
	}
}

// TestMultiJobRecovery_Negative_FullJobQueue_ReleasesReservationAndDefers verifies Requirement 7:
// When queue admission fails (channel full), the active reservation is released and the job is preserved in retryable state.
func TestMultiJobRecovery_Negative_FullJobQueue_ReleasesReservationAndDefers(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "full_queue.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed unsubmitted job
	cmd1 := sampleCommand("job-unsubmitted-queue-full")
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-full-queue",
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

	// Pre-fill client.jobQueue to full capacity (16 slots) so admission fails
	for len(client.jobQueue) < cap(client.jobQueue) {
		client.jobQueue <- sampleCommand(fmt.Sprintf("dummy-queue-blocker-%d", len(client.jobQueue)))
	}

	// Run startup recovery with canceled context so workers don't drain the dummy job
	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelRecov()
	_ = client.Run(recovCtx)

	// Verify Requirement 7:
	// 1. Active reservation was RELEASED
	client.activeMu.Lock()
	isActive := client.activeJobs[cmd1.JobID]
	client.activeMu.Unlock()
	if isActive {
		t.Errorf("reservation leak: expected active reservation for %s to be released on queue full", cmd1.JobID)
	}

	// 2. Preserved in retryable state: in SQLite as SPOOLING, in memory in deferredJobs
	stored, err := store.GetJob(ctx, cmd1.JobID)
	if err != nil || stored == nil {
		t.Fatalf("failed to get stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusSpooling {
		t.Errorf("expected stored status to remain retryable SPOOLING, got: %s", stored.Status)
	}

	client.deferredMu.Lock()
	foundInDeferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd1.JobID {
			foundInDeferred = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !foundInDeferred {
		t.Errorf("expected unadmitted job %s to be preserved in deferredJobs for retry", cmd1.JobID)
	}
}

// TestMultiJobRecovery_Negative_MultipleActiveCUPSJobs_HoldFailure_BlocksFreshSubmissions verifies Requirements 8, 9 & 10:
// If an additional hold fails or remains processing, fresh submissions are blocked (recovery barrier activated),
// physical output uncertainty is flagged, and the primary active job is contained in PAUSED_ERROR rather than resumed.
func TestMultiJobRecovery_Negative_MultipleActiveCUPSJobs_HoldFailure_BlocksFreshSubmissions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hold_failure_active.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: PRINTING, CUPS ID DCPT310-601, processing in CUPS
	cmd1 := sampleCommand("job-1-primary-processing")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting)
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-601")

	// Job 2: PRINTING, CUPS ID DCPT310-602, processing in CUPS
	cmd2 := sampleCommand("job-2-additional-processing")
	cmd2.TotalSheets = 2
	cmd2.TotalPages = 2
	_ = store.SaveJob(ctx, cmd2, contracts.JobStatusPrinting)
	_ = store.MarkJobSubmitting(ctx, cmd2.JobID)
	_ = store.RecordJobSubmission(ctx, cmd2.JobID, queueName+"-602")

	// Runner: lp hold fails for 602 (simulating printer communication timeout or CUPS error)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for _, arg := range args {
				if arg == "-H" {
					return nil, fmt.Errorf("lp: scheduler communication error")
				}
			}
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "/jobs/601") || strings.Contains(arg, "/jobs/602") {
					return []byte("job-state (enum) = processing\njob-media-sheets-completed (integer) = 0\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-hold-fail",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelRecov()
	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(300 * time.Millisecond)

	// 1. Requirement 10: Fresh submissions blocked by recovery barrier
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Fatalf("expected recovery barrier to be active after failed hold on additional active job")
	}
	if !strings.Contains(reason, "uncontained active CUPS jobs") {
		t.Errorf("expected barrier reason mentioning uncontained active CUPS jobs, got: %s", reason)
	}

	// 2. Requirement 8 & 9: Primary job 1 was NOT resumed into monitoring; it was also paused in PAUSED_ERROR
	client.activeMu.Lock()
	isJob1Active := client.activeJobs[cmd1.JobID]
	client.activeMu.Unlock()
	if isJob1Active {
		t.Errorf("safety violation: primary active job 1 was resumed despite uncontained additional active job")
	}

	j1, _ := store.GetJob(ctx, cmd1.JobID)
	if j1 == nil || j1.Status != contracts.JobStatusPausedError {
		t.Errorf("expected Job 1 to be contained in PAUSED_ERROR, got: %+v", j1)
	}
	if !strings.Contains(j1.ErrorReason, "uncontained concurrent output") && !strings.Contains(j1.ErrorReason, "multiple active CUPS jobs") {
		t.Errorf("expected Job 1 error reason mentioning multiple active jobs / uncontained output, got: %s", j1.ErrorReason)
	}

	// 3. Additional job 2 was paused in PAUSED_ERROR flagging physical output uncertainty
	j2, _ := store.GetJob(ctx, cmd2.JobID)
	if j2 == nil || j2.Status != contracts.JobStatusPausedError {
		t.Errorf("expected Job 2 to be paused in PAUSED_ERROR, got: %+v", j2)
	}
	if !strings.Contains(j2.ErrorReason, "hold request failed") {
		t.Errorf("expected Job 2 error reason mentioning hold request failed, got: %s", j2.ErrorReason)
	}

	// 4. Fresh incoming command is rejected by barrier
	freshCmd := sampleCommand("job-new-blocked-by-uncontained")
	client.handleIncomingJob(freshCmd)
	storedFresh, _ := store.GetJob(ctx, freshCmd.JobID)
	if storedFresh != nil {
		t.Errorf("security violation: fresh command accepted during uncontained barrier: %+v", storedFresh)
	}
}

// TestMultiJobRecovery_Negative_PersistenceFailure_WhilePausing_BlocksFreshPrinting verifies Requirements 1-9:
// When SQLite write fails while persisting paused state:
// 1. Recovery barrier is activated fail-closed.
// 2. Cloud is NOT sent an unpersisted progress update.
// 3. Unsubmitted jobs are deferred, NOT submitted.
// 4. Fresh print commands are blocked.
// 5. Active reservations are not leaked.
// 6. Original CUPS ID, print settings and counters are preserved in SQLite.
func TestMultiJobRecovery_Negative_PersistenceFailure_WhilePausing_BlocksFreshPrinting(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pause_persist_fail.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: Incomplete job requiring pause in recovery
	cmd1 := sampleCommand("job-pause-err-1")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-701")

	// Job 2: Definitely unsubmitted job
	cmd2 := sampleCommand("job-unsubmitted-2")
	if err := store.SaveJob(ctx, cmd2, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed for cmd2: %v", err)
	}

	// Capture cloud WebSocket messages
	var receivedMsgs []wireMessage
	var msgsMu sync.Mutex
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			msgsMu.Lock()
			receivedMsgs = append(receivedMsgs, msg)
			msgsMu.Unlock()
		}
	}))
	defer wsServer.Close()
	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	// Install SQLite trigger to fail update on Job 1
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite: %v", err)
	}
	defer rawDB.Close()
	_, err = rawDB.Exec(`CREATE TRIGGER fail_pause_update BEFORE UPDATE ON jobs WHEN NEW.job_id = 'job-pause-err-1' BEGIN SELECT RAISE(ABORT, 'injected sqlite error on pause update'); END;`)
	if err != nil {
		t.Fatalf("failed to create sqlite trigger: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 0\n"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-pause-fail-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRecov()
	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for recovery barrier to activate fail-closed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for unsubmitted job to be deferred in Phase 2
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client.deferredMu.Lock()
		deferCount := len(client.deferredJobs)
		client.deferredMu.Unlock()
		if deferCount > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 1. Recovery barrier activated fail-closed
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Fatalf("expected recovery barrier to be active after pause persistence failure")
	}
	if !strings.Contains(reason, "database error") {
		t.Errorf("expected barrier reason mentioning database error, got: %s", reason)
	}

	// 2. Cloud was NOT sent an unpersisted progress event for cmd1
	msgsMu.Lock()
	for _, m := range receivedMsgs {
		if m.Progress != nil && m.Progress.JobID == cmd1.JobID {
			t.Errorf("safety violation: cloud received progress event for job %s despite SQLite persistence failure: %+v", cmd1.JobID, m.Progress)
		}
	}
	msgsMu.Unlock()

	// 3. Unsubmitted job was deferred, NOT admitted to queue or submitted
	client.deferredMu.Lock()
	foundDeferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd2.JobID {
			foundDeferred = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !foundDeferred {
		t.Errorf("expected unsubmitted job %s to be deferred", cmd2.JobID)
	}
	if len(client.jobQueue) > 0 {
		t.Errorf("safety violation: unsubmitted job admitted to jobQueue despite persistence barrier")
	}

	// 4. Fresh command blocked by barrier
	freshCmd := sampleCommand("job-fresh-blocked")
	client.handleIncomingJob(freshCmd)
	storedFresh, _ := store.GetJob(ctx, freshCmd.JobID)
	if storedFresh != nil {
		t.Fatalf("safety violation: fresh command saved to database while barrier active")
	}

	// 5. Active reservations empty
	client.activeMu.Lock()
	activeCount := len(client.activeJobs)
	client.activeMu.Unlock()
	if activeCount != 0 {
		t.Errorf("expected 0 active reservations, got %d", activeCount)
	}

	// 6. Original CUPS ID and settings preserved in SQLite
	stored1, _ := store.GetJob(ctx, cmd1.JobID)
	if stored1 == nil || stored1.CUPSJobID != queueName+"-701" {
		t.Errorf("expected original CUPS ID %s preserved, got: %+v", queueName+"-701", stored1)
	}
}

// TestMultiJobRecovery_Negative_PersistenceFailure_WhileHolding_BlocksFreshPrinting verifies Requirements 1-10:
// When an extra active CUPS job is placed on hold but SQLite persistence of held state fails:
// 1. Recovery barrier is activated fail-closed.
// 2. Primary candidate is NOT resumed into monitoring; it is safely paused/held as well.
// 3. Cloud is NOT sent progress for the unpersisted hold.
// 4. No new jobs submitted and fresh incoming commands blocked.
// 5. Active reservations are not leaked.
func TestMultiJobRecovery_Negative_PersistenceFailure_WhileHolding_BlocksFreshPrinting(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hold_persist_fail.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: Primary active processing job
	cmd1 := sampleCommand("job-primary-act-1")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting)
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-801")

	// Job 2: Extra active job that needs hold containment
	cmd2 := sampleCommand("job-extra-act-2")
	cmd2.TotalSheets = 2
	cmd2.TotalPages = 2
	_ = store.SaveJob(ctx, cmd2, contracts.JobStatusPrinting)
	_ = store.MarkJobSubmitting(ctx, cmd2.JobID)
	_ = store.RecordJobSubmission(ctx, cmd2.JobID, queueName+"-802")

	// Capture cloud WebSocket messages
	var receivedMsgs []wireMessage
	var msgsMu sync.Mutex
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			msgsMu.Lock()
			receivedMsgs = append(receivedMsgs, msg)
			msgsMu.Unlock()
		}
	}))
	defer wsServer.Close()
	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	// Install SQLite trigger to fail update specifically for Job 2
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite: %v", err)
	}
	defer rawDB.Close()
	_, err = rawDB.Exec(`CREATE TRIGGER fail_hold_update BEFORE UPDATE ON jobs WHEN NEW.job_id = 'job-extra-act-2' BEGIN SELECT RAISE(ABORT, 'injected sqlite error on hold update'); END;`)
	if err != nil {
		t.Fatalf("failed to create sqlite trigger: %v", err)
	}

	var is802Held atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for i, arg := range args {
				if arg == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					is802Held.Store(true)
					return []byte(""), nil
				}
			}
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "/jobs/801") {
					return []byte("job-state (enum) = processing\njob-media-sheets-completed (integer) = 0\n"), nil
				}
				if strings.Contains(arg, "/jobs/802") {
					if is802Held.Load() {
						return []byte("job-state (enum) = pending-held\njob-media-sheets-completed (integer) = 0\n"), nil
					}
					return []byte("job-state (enum) = processing\njob-media-sheets-completed (integer) = 0\n"), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-hold-fail-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRecov()
	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for recovery barrier to activate fail-closed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 1. Recovery barrier is active
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Fatalf("expected recovery barrier to be active after hold persistence failure")
	}
	if !strings.Contains(reason, "database error") {
		t.Errorf("expected barrier reason mentioning database error, got: %s", reason)
	}

	// 2. Primary candidate was NOT resumed into monitoring
	client.activeMu.Lock()
	isJob1Active := client.activeJobs[cmd1.JobID]
	client.activeMu.Unlock()
	if isJob1Active {
		t.Fatalf("safety violation: primary active job 1 was resumed despite unpersisted hold on extra active job")
	}

	// 3. Cloud received NO unpersisted progress for job 2
	msgsMu.Lock()
	for _, m := range receivedMsgs {
		if m.Progress != nil && m.Progress.JobID == cmd2.JobID {
			t.Errorf("safety violation: cloud received progress for extra job %s despite SQLite failure: %+v", cmd2.JobID, m.Progress)
		}
	}
	msgsMu.Unlock()

	// 4. Active reservations empty
	client.activeMu.Lock()
	activeCount := len(client.activeJobs)
	client.activeMu.Unlock()
	if activeCount != 0 {
		t.Errorf("expected 0 active reservations, got %d", activeCount)
	}

	// 5. Fresh commands blocked
	freshCmd := sampleCommand("job-fresh-blocked-hold")
	client.handleIncomingJob(freshCmd)
	storedFresh, _ := store.GetJob(ctx, freshCmd.JobID)
	if storedFresh != nil {
		t.Fatalf("safety violation: fresh command saved while hold barrier active")
	}
}

// TestMultiJobRecovery_Negative_PersistenceFailure_WhileCompleting_BlocksFreshPrinting verifies Requirements 1-9:
// When a job has full physical proof and CUPS completed, but SQLite fails during completeJob:
// 1. Recovery barrier is activated fail-closed.
// 2. Job is NOT marked resolved in startup scheduling.
// 3. Cloud is NOT sent JobStatusCompleted (no false completions!).
// 4. Definitely unsubmitted jobs are deferred, NOT submitted.
// 5. Fresh commands are blocked.
// 6. Active reservations are not leaked.
func TestMultiJobRecovery_Negative_PersistenceFailure_WhileCompleting_BlocksFreshPrinting(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "complete_persist_fail.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "DCPT310"

	// Job 1: Has full physical proof (2/2 sheets printed) and CUPS completed
	cmd1 := sampleCommand("job-complete-fail-1")
	cmd1.TotalSheets = 2
	cmd1.TotalPages = 2
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting)
	_ = store.MarkJobSubmitting(ctx, cmd1.JobID)
	_ = store.RecordJobSubmission(ctx, cmd1.JobID, queueName+"-901")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd1.JobID,
		KioskID:       cmd1.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		Timestamp:     time.Now().UTC(),
	})

	// Job 2: Definitely unsubmitted job
	cmd2 := sampleCommand("job-unsubmitted-def-2")
	_ = store.SaveJob(ctx, cmd2, contracts.JobStatusSpooling)

	// Capture cloud WebSocket messages
	var receivedMsgs []wireMessage
	var msgsMu sync.Mutex
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			msgsMu.Lock()
			receivedMsgs = append(receivedMsgs, msg)
			msgsMu.Unlock()
		}
	}))
	defer wsServer.Close()
	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	// Install SQLite trigger to fail update when status becomes COMPLETED
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite: %v", err)
	}
	defer rawDB.Close()
	_, err = rawDB.Exec(`CREATE TRIGGER fail_complete_update BEFORE UPDATE ON jobs WHEN NEW.status = 'COMPLETED' BEGIN SELECT RAISE(ABORT, 'injected sqlite error on complete'); END;`)
	if err != nil {
		t.Fatalf("failed to create sqlite trigger: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			return []byte("printer DCPT310 is idle. enabled since Thu Jan 01 00:00:00 1970"), nil
		}
		if name == "ipptool" {
			return []byte("job-state (enum) = completed\njob-media-sheets-completed (integer) = 2\n"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-complete-fail-test",
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

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRecov()
	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for recovery barrier to activate fail-closed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for unsubmitted job to be deferred in Phase 2
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client.deferredMu.Lock()
		deferCount := len(client.deferredJobs)
		client.deferredMu.Unlock()
		if deferCount > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 1. Recovery barrier is active
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Fatalf("expected recovery barrier to be active after completion persistence failure")
	}
	if !strings.Contains(reason, "database error") {
		t.Errorf("expected barrier reason mentioning database error, got: %s", reason)
	}

	// 2. Cloud was NEVER sent JobStatusCompleted (Requirement 4: no false completions!)
	msgsMu.Lock()
	for _, m := range receivedMsgs {
		if m.Progress != nil && m.Progress.JobID == cmd1.JobID && m.Progress.Status == contracts.JobStatusCompleted {
			t.Fatalf("safety violation: cloud received JobStatusCompleted despite SQLite completion failure: %+v", m.Progress)
		}
	}
	msgsMu.Unlock()

	// 3. Job is NOT marked COMPLETED in SQLite
	stored1, _ := store.GetJob(ctx, cmd1.JobID)
	if stored1 != nil && stored1.Status == contracts.JobStatusCompleted {
		t.Fatalf("safety violation: job 1 marked COMPLETED in SQLite despite trigger failure")
	}

	// 4. Unsubmitted job was deferred, NOT admitted to queue or submitted
	client.deferredMu.Lock()
	foundDeferred := false
	for _, dj := range client.deferredJobs {
		if dj.JobID == cmd2.JobID {
			foundDeferred = true
			break
		}
	}
	client.deferredMu.Unlock()
	if !foundDeferred {
		t.Errorf("expected unsubmitted job %s to be deferred", cmd2.JobID)
	}
	if len(client.jobQueue) > 0 {
		t.Errorf("safety violation: unsubmitted job admitted to jobQueue despite persistence failure")
	}

	// 5. Active reservations empty
	client.activeMu.Lock()
	activeCount := len(client.activeJobs)
	client.activeMu.Unlock()
	if activeCount != 0 {
		t.Errorf("expected 0 active reservations, got %d", activeCount)
	}

	// 6. Fresh commands blocked by barrier
	freshCmd := sampleCommand("job-fresh-blocked-complete")
	client.handleIncomingJob(freshCmd)
	storedFresh, _ := store.GetJob(ctx, freshCmd.JobID)
	if storedFresh != nil {
		t.Fatalf("safety violation: fresh command saved while complete barrier active")
	}
}
