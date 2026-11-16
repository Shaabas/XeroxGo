package spooler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func sampleUnlinkedCommand(jobID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-unlinked-test",
		DisplayCode:    "8888",
		DownloadURL:    "https://storage.example.com/" + jobID + ".pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}
}

// TestUnlinkedRecovery_SingleCandidateDiscovered verifies Requirements 2, 3, 4, 5, 6, 7, 8, 9, 12, 14:
// 1. Unlinked SQLite job (submitted_to_printer=1, cups_job_id="") is detected.
// 2. Active CUPS queue is enumerated using authoritative attributes.
// 3. Exactly 1 matching candidate is found; candidate ID, state, title, and user are captured for diagnostics.
// 4. Candidate ID is NOT bound to SQLite job (cups_job_id remains empty "").
// 5. Affected job is kept in PAUSED_ERROR and recovery barrier is activated.
// 6. Unrelated CUPS jobs on the queue are untouched.
// 7. Zero new lp print dispatches occur.
func TestUnlinkedRecovery_SingleCandidateDiscovered(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_single.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "TestQueue"
	targetJobID := "job-cw6-unlinked-single"

	cmd := sampleUnlinkedCommand(targetJobID)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	var modifyingCalls atomic.Int32
	var lpPrintCalls atomic.Int32

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "cancel" {
			modifyingCalls.Add(1)
			return []byte(""), nil
		}
		if name == "lp" {
			for i, arg := range args {
				if arg == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					modifyingCalls.Add(1)
					return []byte(""), nil
				}
			}
			lpPrintCalls.Add(1)
			return []byte("request id is TestQueue-999 (1 file(s))"), nil
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					// 2 jobs: 1 candidate match, 1 unrelated job
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/101
        job-id (integer) = 101
        job-name (nameWithoutLanguage) = ` + targetJobID + `
        job-originating-user-name (nameWithoutLanguage) = kiosk-user
        job-state (enum) = processing
        job-media-sheets-completed (integer) = 0
        -- separator --
        job-uri (uri) = ipp://localhost:631/jobs/999
        job-id (integer) = 999
        job-name (nameWithoutLanguage) = other-customer-job
        job-originating-user-name (nameWithoutLanguage) = other-user
        job-state (enum) = pending
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	// Wait for recovery barrier to activate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 1. Verify SQLite state: PAUSED_ERROR, cups_job_id empty
	stored, err := store.GetJob(context.Background(), targetJobID)
	if err != nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusPausedError {
		t.Errorf("expected stored status PAUSED_ERROR, got %s", stored.Status)
	}
	if stored.CUPSJobID != "" {
		t.Errorf("safety violation: candidate CUPS ID was automatically bound to stored job: %q", stored.CUPSJobID)
	}

	// 2. Verify candidate diagnostics recorded
	if !strings.Contains(stored.ErrorReason, "TestQueue-101") ||
		!strings.Contains(stored.ErrorReason, "ownership unverified") ||
		!strings.Contains(stored.ErrorReason, "physical output uncertain") {
		t.Errorf("expected diagnostic details in stored error reason, got: %s", stored.ErrorReason)
	}

	// 3. Verify recovery barrier is active
	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Errorf("expected recovery barrier to be active, got false")
	}
	if !strings.Contains(reason, "TestQueue-101") {
		t.Errorf("expected candidate ID in barrier reason, got: %s", reason)
	}

	// 4. Verify no modifying calls were made to CUPS
	if modifyingCalls.Load() > 0 {
		t.Errorf("safety violation: issued %d modifying calls to CUPS during discovery", modifyingCalls.Load())
	}
	if lpPrintCalls.Load() > 0 {
		t.Errorf("safety violation: issued %d new lp print calls", lpPrintCalls.Load())
	}
}

// TestUnlinkedRecovery_ZeroMatches verifies Requirement 10:
// 1. Active queue has 0 matching jobs. Completed history has 0 matching jobs.
// 2. Stored job is paused in PAUSED_ERROR with zero-matches diagnostic.
// 3. cups_job_id remains empty. Recovery barrier is active.
func TestUnlinkedRecovery_ZeroMatches(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_zero.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "TestQueue"
	targetJobID := "job-cw6-zero-matches"

	cmd := sampleUnlinkedCommand(targetJobID)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
				if strings.Contains(arg, "get-completed-jobs.test") {
					return []byte(`    Get completed jobs                                                   [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stored, err := store.GetJob(context.Background(), targetJobID)
	if err != nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusPausedError {
		t.Errorf("expected stored status PAUSED_ERROR, got %s", stored.Status)
	}
	if stored.CUPSJobID != "" {
		t.Errorf("expected empty cups_job_id, got %q", stored.CUPSJobID)
	}
	if !strings.Contains(stored.ErrorReason, "0 candidate CUPS jobs") {
		t.Errorf("expected 0 candidate CUPS jobs in error reason, got: %s", stored.ErrorReason)
	}

	blocked, _ := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Errorf("expected recovery barrier to be active")
	}
}

// TestUnlinkedRecovery_TitleCollision_MultipleCandidates verifies Requirements 4, 10, 13:
// 1. Multiple active CUPS jobs share the same title (title collision).
// 2. Both candidates are recorded in the diagnostic message.
// 3. No candidate is bound to SQLite (cups_job_id remains empty).
// 4. Job is in PAUSED_ERROR and recovery barrier is active.
func TestUnlinkedRecovery_TitleCollision_MultipleCandidates(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_collision.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "TestQueue"
	targetJobID := "job-cw6-collision"

	cmd := sampleUnlinkedCommand(targetJobID)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/201
        job-id (integer) = 201
        job-name (nameWithoutLanguage) = ` + targetJobID + `
        job-originating-user-name (nameWithoutLanguage) = user1
        job-state (enum) = processing
        -- separator --
        job-uri (uri) = ipp://localhost:631/jobs/202
        job-id (integer) = 202
        job-name (nameWithoutLanguage) = ` + targetJobID + `
        job-originating-user-name (nameWithoutLanguage) = user2
        job-state (enum) = pending-held
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stored, err := store.GetJob(context.Background(), targetJobID)
	if err != nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusPausedError {
		t.Errorf("expected stored status PAUSED_ERROR, got %s", stored.Status)
	}
	if stored.CUPSJobID != "" {
		t.Errorf("expected empty cups_job_id, got %q", stored.CUPSJobID)
	}
	if !strings.Contains(stored.ErrorReason, "title collision/duplicate detected") ||
		!strings.Contains(stored.ErrorReason, "TestQueue-201") ||
		!strings.Contains(stored.ErrorReason, "TestQueue-202") {
		t.Errorf("expected title collision and candidate IDs in error reason, got: %s", stored.ErrorReason)
	}

	blocked, _ := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Errorf("expected recovery barrier to be active")
	}
}

// TestUnlinkedRecovery_StaleJobInHistory verifies Requirements 4, 10, 13:
// 1. Active queue has 0 matches; completed history has a job with matching title.
// 2. Candidate in history is documented without physical proof.
// 3. Job is paused in PAUSED_ERROR and NOT automatically completed or adopted.
func TestUnlinkedRecovery_StaleJobInHistory(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_stale.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "TestQueue"
	targetJobID := "job-cw6-stale-history"

	cmd := sampleUnlinkedCommand(targetJobID)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					// 0 active jobs
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
				if strings.Contains(arg, "get-completed-jobs.test") {
					// 1 completed job with matching title
					return []byte(`    Get completed jobs                                                   [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/301
        job-id (integer) = 301
        job-name (nameWithoutLanguage) = ` + targetJobID + `
        job-originating-user-name (nameWithoutLanguage) = old-user
        job-state (enum) = completed
        job-media-sheets-completed (integer) = 1
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stored, err := store.GetJob(context.Background(), targetJobID)
	if err != nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusPausedError {
		t.Errorf("expected stored status PAUSED_ERROR, got %s", stored.Status)
	}
	if stored.CUPSJobID != "" {
		t.Errorf("expected empty cups_job_id, got %q", stored.CUPSJobID)
	}
	if !strings.Contains(stored.ErrorReason, "history") ||
		!strings.Contains(stored.ErrorReason, "TestQueue-301") ||
		!strings.Contains(stored.ErrorReason, "ownership unverified") {
		t.Errorf("expected history candidate details in error reason, got: %s", stored.ErrorReason)
	}

	blocked, _ := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Errorf("expected recovery barrier to be active")
	}
}

// TestUnlinkedRecovery_CUPSEnumerationFailure verifies Requirement 11:
// 1. CUPS runner fails with error during enumeration.
// 2. Fails closed to PAUSED_ERROR and activates recovery barrier.
func TestUnlinkedRecovery_CUPSEnumerationFailure(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_err.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "FailingQueue"
	targetJobID := "job-cw6-enum-fail"

	cmd := sampleUnlinkedCommand(targetJobID)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("cupsd scheduler is stopped")
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stored, err := store.GetJob(context.Background(), targetJobID)
	if err != nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if stored.Status != contracts.JobStatusPausedError {
		t.Errorf("expected stored status PAUSED_ERROR, got %s", stored.Status)
	}
	if !strings.Contains(stored.ErrorReason, "error querying CUPS queue jobs") {
		t.Errorf("expected CUPS query error in error reason, got: %s", stored.ErrorReason)
	}

	blocked, reason := client.IsRecoveryBarrierActive()
	if !blocked {
		t.Errorf("expected recovery barrier to be active")
	}
	if !strings.Contains(reason, "error querying CUPS queue jobs") {
		t.Errorf("expected query error in barrier reason, got: %s", reason)
	}
}

// TestUnlinkedRecovery_BlocksSubsequentPrinting verifies Requirement 8, 9, 14:
// 1. Job 1 is unlinked (submitted=1, cups_id="").
// 2. Job 2 is definitely unsubmitted (submitted=0, cups_id="").
// 3. Startup recovery pauses Job 1 with candidate diagnostics and activates recovery barrier.
// 4. Job 2 is safely deferred, NOT printed.
// 5. New print command from cloud is rejected by print barrier.
// 6. Zero new lp print calls occur.
func TestUnlinkedRecovery_BlocksSubsequentPrinting(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unlinked_barrier.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	queueName := "TestQueue"

	// Seed Job 1: unlinked
	cmd1 := sampleUnlinkedCommand("job-1-unlinked")
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob 1 failed: %v", err)
	}
	if err := store.MarkJobSubmitting(ctx, cmd1.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting 1 failed: %v", err)
	}

	// Seed Job 2: definitely unsubmitted
	cmd2 := sampleUnlinkedCommand("job-2-unsubmitted")
	if err := store.SaveJob(ctx, cmd2, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob 2 failed: %v", err)
	}

	var lpCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalls.Add(1)
			return []byte("request id is TestQueue-999 (1 file(s))"), nil
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/101
        job-id (integer) = 101
        job-name (nameWithoutLanguage) = job-1-unlinked
        job-originating-user-name (nameWithoutLanguage) = operator
        job-state (enum) = processing
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-unlinked-test",
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

	// Wait for Job 1 to transition to PAUSED_ERROR and barrier to activate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocked, _ := client.IsRecoveryBarrierActive(); blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify Job 1 is PAUSED_ERROR, cups_id=""
	j1, _ := store.GetJob(context.Background(), cmd1.JobID)
	if j1 == nil || j1.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected Job 1 in PAUSED_ERROR, got: %+v", j1)
	}

	// Verify Job 2 is deferred, not in activeJobs
	client.deferredMu.Lock()
	deferredCount := len(client.deferredJobs)
	client.deferredMu.Unlock()
	if deferredCount != 1 {
		t.Errorf("expected 1 deferred job, got %d", deferredCount)
	}

	// Verify new incoming command from cloud is rejected by print barrier
	cmd3 := sampleUnlinkedCommand("job-3-incoming")
	client.handleIncomingJob(cmd3)

	// Verify cmd3 was NOT admitted to queue
	j3, _ := store.GetJob(context.Background(), cmd3.JobID)
	if j3 != nil {
		t.Errorf("expected cmd3 to not be saved/admitted, got: %+v", j3)
	}

	// Verify zero lp print calls were dispatched
	if lpCalls.Load() > 0 {
		t.Errorf("safety violation: issued %d lp print calls", lpCalls.Load())
	}
}
