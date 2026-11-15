package printer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

// isProcessAlive checks whether a process exists and is active.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// ActiveJobMarker contains parsed fields from EMU_JOB_ACTIVE_FILE.
type ActiveJobMarker struct {
	JobName   string
	IPPJobID  string
	PID       int
	State     string
	Timestamp string
}

// parseActiveJobMarker parses the authoritative key=value structure in EMU_JOB_ACTIVE_FILE.
func parseActiveJobMarker(filePath string) (ActiveJobMarker, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ActiveJobMarker{}, err
	}
	marker := ActiveJobMarker{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "JOB_NAME=") {
			marker.JobName = strings.TrimPrefix(line, "JOB_NAME=")
		} else if strings.HasPrefix(line, "IPP_JOB_ID=") {
			marker.IPPJobID = strings.TrimPrefix(line, "IPP_JOB_ID=")
		} else if strings.HasPrefix(line, "PID=") {
			marker.PID, _ = strconv.Atoi(strings.TrimPrefix(line, "PID="))
		} else if strings.HasPrefix(line, "STATE=") {
			marker.State = strings.TrimPrefix(line, "STATE=")
		} else if strings.HasPrefix(line, "TIMESTAMP=") {
			marker.Timestamp = strings.TrimPrefix(line, "TIMESTAMP=")
		}
	}
	if marker.JobName == "" || marker.PID <= 0 || marker.State == "" {
		return marker, errors.New("incomplete active job marker")
	}
	return marker, nil
}

// validateActiveJob validates that the active-job marker is authoritative, job-specific,
// not stale, not completed, matching the expected job ID, with an active processor PID,
// and that CUPS confirms the job is actively processing (Requirements 6, 7, 8).
func validateActiveJob(
	ctx context.Context,
	markerPath string,
	expectedJobID string,
	queueName string,
	cupsJobID string,
) error {
	marker, err := parseActiveJobMarker(markerPath)
	if err != nil {
		return fmt.Errorf("invalid or unreadable active-job marker: %w", err)
	}

	// 1. Job-specific check: reject mismatched job ID (Requirements 6, 7)
	if marker.JobName != expectedJobID {
		return fmt.Errorf("marker job ID mismatch: got %q, want %q", marker.JobName, expectedJobID)
	}

	// 2. Reject completed or non-processing markers (Requirement 7)
	if marker.State != "processing" {
		return fmt.Errorf("marker state is not processing (got %q)", marker.State)
	}

	// 3. Validate processor PID is active and alive (Requirements 7, 8)
	if !isProcessAlive(marker.PID) {
		return fmt.Errorf("processor PID %d is not running (stale marker)", marker.PID)
	}

	// 4. Validate actual CUPS job state before injection (Requirement 8)
	if cupsJobID != "" && queueName != "" {
		info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cupsJobID)
		if qErr != nil {
			return fmt.Errorf("failed to query CUPS job state for %s: %w", cupsJobID, qErr)
		}
		if info.State != CUPSJobStateProcessing && info.State != CUPSJobStatePending {
			return fmt.Errorf("CUPS job %s is not in active state (got %s)", cupsJobID, info.State)
		}
	}

	return nil
}

// runMidPrintFaultScenario verifies that XeroxGo detects hardware faults during real
// active CUPS jobs, places the job on hold in CUPS, returns the proper interruption error,
// preserves the original job identity in SQLite, and prevents unmonitored sheet delivery.
func runMidPrintFaultScenario(t *testing.T, scenario string, expectedErrMsg string) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	emuName := fmt.Sprintf("XeroxGoMidEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoMidQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	// Ownership metadata file placed outside t.TempDir() for audit protection
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

	// Ensure execution gate is primed before print submission (Requirement 6, 9)
	_ = os.WriteFile(gateFile, []byte("gate"), 0644)
	_ = os.Remove(activeFile)
	_ = os.Remove(releaseFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", spoolDir),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_META_FILE=%s", metaFile),
		fmt.Sprintf("SNMP_PID_FILE=%s", filepath.Join(tempDir, "snmp.pid")),
		fmt.Sprintf("SNMP_LOG_FILE=%s", filepath.Join(tempDir, "snmp.log")),
		fmt.Sprintf("SNMP_STATE_FILE=%s", filepath.Join(tempDir, "snmp_state.json")),
		fmt.Sprintf("SNMP_LOCK_DIR=%s", filepath.Join(tempDir, ".snmp_lock")),
		fmt.Sprintf("EMU_FAULT_STATE_FILE=%s", filepath.Join(tempDir, "fault_state.json")),
		fmt.Sprintf("EMU_IPP_PAUSED_FILE=%s", filepath.Join(tempDir, "ipp_paused.flag")),
		fmt.Sprintf("EMU_JOB_ACTIVE_FILE=%s", activeFile),
		fmt.Sprintf("EMU_JOB_GATE_FILE=%s", gateFile),
		fmt.Sprintf("EMU_JOB_RELEASE_FILE=%s", releaseFile),
		"EMU_GATE_TIMEOUT_TENTHS=60", // 6.0s safety timeout (Requirement 9)
	)

	// Start IPP and SNMP emulators
	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	// Guaranteed teardown (Requirements 10, 11, 12)
	t.Cleanup(func() {
		// Ensure gate is released so processor finishes cleanly
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)

		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		// Requirement 11: Cancel only captured test job IDs after validating destination belongs to emulator
		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			if _, err := runEmu(t, emuScript, emuEnv, "verify-queue", queueName); err == nil {
				_, _ = exec.Command("cancel", cID).CombinedOutput()
			} else {
				t.Logf("Skipping cancel for job %s: queue %s ownership unverified", cID, queueName)
			}
		}
		// Requirement 10: No unverified queue-wide cancel <queue>

		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

	// Setup driverless CUPS queue pointing to emulator IPP endpoint
	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup driverless CUPS queue %q: %v (output: %s)", queueName, err, out)
	}

	snmpCfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Wait for SNMP responder readiness
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if life := PollMarkerLifeCount(ctx, snmpCfg); life >= 0 {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("SNMP emulator on port %d not ready within timeout", snmpPort)
	}

	// Initialize real SQLite state store for durable job recording verification
	dbPath := filepath.Join(tempDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open SQLite state store: %v", err)
	}
	defer store.Close()

	uniqueJobID := fmt.Sprintf("job-midprint-%s-%d-%d", scenario, os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-midprint-01",
		DisplayCode:    "M999",
		DownloadURL:    "https://example.local/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("failed to insert initial job in store: %v", err)
	}

	submittingCalls := 0
	submittedCalls := 0
	onPageCalls := 0

	hooks := &CUPSPrintHooks{
		OnSubmitting: func() error {
			submittingCalls++
			return store.MarkJobSubmitting(ctx, cmd.JobID)
		},
		OnSubmitted: func(cupsID string) error {
			submittedCalls++
			jobMu.Lock()
			capturedCUPSJobID = cupsID
			jobMu.Unlock()
			return store.RecordJobSubmission(ctx, cmd.JobID, cupsID)
		},
		TelemetryPoller: func(c context.Context) HardwareTelemetry {
			return PollHardwareTelemetry(c, snmpCfg)
		},
	}

	// Channel to signal injector status
	faultInjectedCh := make(chan error, 1)

	// Goroutine: Synchronize fault injection with confirmed active job (Requirements 6, 7, 8)
	go func() {
		// Wait for job to be captured and processor to reach authoritative active state
		activeDeadline := time.Now().Add(5 * time.Second)
		var activeErr error
		activeConfirmed := false

		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" {
				// Authoritatively validate active job marker and actual CUPS state
				activeErr = validateActiveJob(ctx, activeFile, cmd.JobID, queueName, cID)
				if activeErr == nil {
					activeConfirmed = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !activeConfirmed {
			faultInjectedCh <- fmt.Errorf("active job validation failed: %v", activeErr)
			return
		}

		// Requirement 8: Inject fault through existing D3 fault controller
		out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", scenario)
		if fErr != nil {
			faultInjectedCh <- fmt.Errorf("fault set %s failed: %w (output: %s)", scenario, fErr, out)
			return
		}

		// Confirm live SNMP telemetry reflection
		telem := PollHardwareTelemetry(ctx, snmpCfg)
		switch scenario {
		case "paper-jam":
			if !telem.Flags.Jammed {
				faultInjectedCh <- fmt.Errorf("SNMP telemetry does not reflect Jammed after injection")
				return
			}
		case "cover-open":
			if !telem.Flags.DoorOpen {
				faultInjectedCh <- fmt.Errorf("SNMP telemetry does not reflect DoorOpen after injection")
				return
			}
		case "paper-empty":
			if !telem.Flags.NoPaper && !telem.Flags.InputTrayEmpty {
				faultInjectedCh <- fmt.Errorf("SNMP telemetry does not reflect NoPaper after injection")
				return
			}
		}

		faultInjectedCh <- nil
	}()

	// Execute print job through existing CUPS function (Requirement 2: production monitor executes hold attempt)
	printErr := PrintPDFWithCUPSHooksCustom(
		ctx,
		nil,
		queueName,
		cmd,
		samplePDF,
		func(sheets int) { onPageCalls++ },
		50*time.Millisecond,
		hooks,
	)

	// Release processor gate after print attempt finishes/fails
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Check injector result
	injectorErr := <-faultInjectedCh
	if injectorErr != nil {
		t.Fatalf("fault injection synchronization failed: %v", injectorErr)
	}

	// Requirement 10: Verify the correct hardware-interruption error
	if printErr == nil {
		t.Fatalf("expected hardware interruption error for scenario %q, got nil", scenario)
	}
	if !strings.Contains(printErr.Error(), expectedErrMsg) {
		t.Errorf("expected error containing %q, got %q", expectedErrMsg, printErr.Error())
	}

	// Requirement 2 & 3: Post-fault verification queries real CUPS job WITHOUT issuing another hold
	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()
	if cID == "" {
		t.Fatalf("expected non-empty captured CUPS job ID")
	}

	jobInfo, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("post-fault CUPS query failed for job %s: %v", cID, qErr)
	}

	// Requirement 4 & 5: Report confirmed-held, still-processing, already-completed, and hold-failed accurately
	var outcome string
	switch jobInfo.State {
	case CUPSJobStateHeld:
		outcome = "confirmed-held"
	case CUPSJobStateProcessing, CUPSJobStatePending:
		outcome = "still-processing"
	case CUPSJobStateCompleted:
		outcome = "already-completed"
	case CUPSJobStateCanceled, CUPSJobStateAborted:
		outcome = "hold-failed"
	default:
		outcome = "unknown"
	}
	t.Logf("Scenario %s Post-Fault CUPS Outcome: %s (State=%s, Reasons=%v)",
		scenario, outcome, jobInfo.State, jobInfo.Reasons)

	// Must never claim confirmed-held if the outcome is not confirmed held
	if outcome != "confirmed-held" {
		t.Fatalf("expected confirmed-held outcome, got %s (state=%s, reasons=%v)",
			outcome, jobInfo.State, jobInfo.Reasons)
	}

	// Requirement 13: Assert original CUPS job ID remains traceable in SQLite and no second job is submitted
	storedJob, err := store.GetJob(ctx, cmd.JobID)
	if err != nil {
		t.Fatalf("failed to retrieve job from state store: %v", err)
	}
	if storedJob.CUPSJobID != cID {
		t.Errorf("stored CUPS job ID %q does not match captured ID %q", storedJob.CUPSJobID, cID)
	}
	if submittingCalls != 1 || submittedCalls != 1 {
		t.Errorf("submission hooks called multiple times: submitting=%d, submitted=%d (want 1)", submittingCalls, submittedCalls)
	}

	// Verify only 1 CUPS job record exists for this queue
	lpstatOut, _ := exec.Command("lpstat", "-W", "all", queueName).CombinedOutput()
	cupsRecords := parseCUPSJobIDsFromLPStat(string(lpstatOut), queueName)
	if len(cupsRecords) > 0 {
		if len(cupsRecords) != 1 || cupsRecords[0] != cID {
			t.Errorf("CUPS records show multiple or mismatched jobs for queue %s: %v", queueName, cupsRecords)
		}
	}

	// Requirement 14: Assert no physical-sheet progress without explicit simulated odometer advance
	if onPageCalls != 0 {
		t.Errorf("expected 0 physical sheet delivery callbacks during hardware fault, got %d", onPageCalls)
	}
	telemAfter := PollHardwareTelemetry(ctx, snmpCfg)
	if telemAfter.HasLife && telemAfter.LifeCount != 1000 {
		t.Errorf("odometer unexpectedly changed: got %d, want 1000", telemAfter.LifeCount)
	}
}

// TestMidPrintHardwareFault_PaperJam verifies mid-print paper-jam detection and CUPS job hold.
func TestMidPrintHardwareFault_PaperJam(t *testing.T) {
	runMidPrintFaultScenario(t, "paper-jam", "paper feed jam detected in printer")
}

// TestMidPrintHardwareFault_PaperEmpty verifies mid-print out-of-paper detection and CUPS job hold.
func TestMidPrintHardwareFault_PaperEmpty(t *testing.T) {
	runMidPrintFaultScenario(t, "paper-empty", "printer paper tray is empty")
}

// TestMidPrintHardwareFault_CoverOpen verifies mid-print cover-open detection and CUPS job hold.
func TestMidPrintHardwareFault_CoverOpen(t *testing.T) {
	runMidPrintFaultScenario(t, "cover-open", "printer cover is open")
}

// TestMidPrintFault_RejectStaleAndMismatchedMarkers verifies that active-job validation
// strictly rejects stale markers, completed markers, dead PIDs, and mismatched job IDs (Requirements 6, 7).
func TestMidPrintFault_RejectStaleAndMismatchedMarkers(t *testing.T) {
	tempDir := t.TempDir()
	markerFile := filepath.Join(tempDir, "job_active.marker")

	ctx := context.Background()
	expectedJob := "job-midprint-expected"

	// Subtest 1: Missing marker file
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err == nil {
		t.Errorf("expected error for non-existent marker file, got nil")
	}

	// Subtest 2: Incomplete/malformed marker
	_ = os.WriteFile(markerFile, []byte("JUNK_CONTENT=true\n"), 0644)
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err == nil {
		t.Errorf("expected error for malformed marker, got nil")
	}

	// Subtest 3: Mismatched job ID
	mismatchedContent := fmt.Sprintf("JOB_NAME=job-other-123\nIPP_JOB_ID=1\nPID=%d\nSTATE=processing\n", os.Getpid())
	_ = os.WriteFile(markerFile, []byte(mismatchedContent), 0644)
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected job ID mismatch error, got: %v", err)
	}

	// Subtest 4: Completed marker (not processing)
	completedContent := fmt.Sprintf("JOB_NAME=%s\nIPP_JOB_ID=1\nPID=%d\nSTATE=completed\n", expectedJob, os.Getpid())
	_ = os.WriteFile(markerFile, []byte(completedContent), 0644)
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err == nil || !strings.Contains(err.Error(), "not processing") {
		t.Errorf("expected completed marker rejection error, got: %v", err)
	}

	// Subtest 5: Stale marker with dead PID
	deadPID := 9999999 // Very unlikely to exist
	staleContent := fmt.Sprintf("JOB_NAME=%s\nIPP_JOB_ID=1\nPID=%d\nSTATE=processing\n", expectedJob, deadPID)
	_ = os.WriteFile(markerFile, []byte(staleContent), 0644)
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("expected dead PID stale marker rejection error, got: %v", err)
	}

	// Subtest 6: Authoritative marker with live PID passes marker-level validation (no CUPS queue check)
	validContent := fmt.Sprintf("JOB_NAME=%s\nIPP_JOB_ID=1\nPID=%d\nSTATE=processing\n", expectedJob, os.Getpid())
	_ = os.WriteFile(markerFile, []byte(validContent), 0644)
	if err := validateActiveJob(ctx, markerFile, expectedJob, "", ""); err != nil {
		t.Errorf("expected valid marker to pass without CUPS check, got: %v", err)
	}
}

// TestMidPrintFault_FailedHoldAttempt verifies that HoldAndVerifyCUPSJob accurately reports
// hold-failed / error outcomes and does not claim ConfirmedHeld when the hold attempt fails (Requirements 4, 5, 13).
func TestMidPrintFault_FailedHoldAttempt(t *testing.T) {
	ctx := context.Background()

	// Calling HoldAndVerifyCUPSJob with an invalid/non-existent job ID
	invalidJobID := "nonexistent-queue-999999"
	res := HoldAndVerifyCUPSJob(ctx, nil, "nonexistent-queue", invalidJobID)

	if res.ConfirmedHeld {
		t.Errorf("expected ConfirmedHeld=false for failed hold attempt, got true")
	}
	if res.AlreadyCompleted {
		t.Errorf("expected AlreadyCompleted=false for non-existent job, got true")
	}
	if res.HoldErr == nil {
		t.Errorf("expected non-nil HoldErr for invalid job ID, got nil")
	}
}

// TestMidPrintHardwareFault_DistinguishCompletedFromHeld verifies that HoldAndVerifyCUPSJob
// properly distinguishes a job that already completed before hold from a confirmed held job (Requirements 4, 13).
func TestMidPrintHardwareFault_DistinguishCompletedFromHeld(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	emuName := fmt.Sprintf("XeroxGoDistEmu_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoDistQ_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		"EMU_HOST=localhost",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", spoolDir),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_META_FILE=%s", metaFile),
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start emulator: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup")
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup CUPS queue: %v (%s)", err, out)
	}

	ctx := context.Background()

	// Submit a fast job that will complete immediately (no gate)
	cmd := contracts.PrintJobCommand{
		JobID:       fmt.Sprintf("job-completed-%d", os.Getpid()),
		Copies:      1,
		TotalSheets: 1,
	}

	hooks := &CUPSPrintHooks{}
	var cupsJobID string
	hooks.OnSubmitted = func(id string) error {
		cupsJobID = id
		return nil
	}

	err := PrintPDFWithCUPSHooksCustom(ctx, nil, queueName, cmd, samplePDF, nil, 50*time.Millisecond, hooks)
	if err != nil {
		t.Fatalf("print submission failed: %v", err)
	}

	// Verify job reached completed state via QueryCUPSJobInfo
	info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cupsJobID)
	if qErr != nil || info.State != CUPSJobStateCompleted {
		t.Fatalf("expected job to be completed, got state=%s (err: %v)", info.State, qErr)
	}

	// Requirement 4, 13: HoldAndVerifyCUPSJob on already completed job must return AlreadyCompleted = true and ConfirmedHeld = false
	res := HoldAndVerifyCUPSJob(ctx, nil, queueName, cupsJobID)
	if !res.AlreadyCompleted {
		t.Errorf("expected AlreadyCompleted=true for completed job, got ConfirmedHeld=%v, AlreadyCompleted=%v, State=%s",
			res.ConfirmedHeld, res.AlreadyCompleted, res.State)
	}
	if res.ConfirmedHeld {
		t.Errorf("expected ConfirmedHeld=false for completed job, got true")
	}
}
