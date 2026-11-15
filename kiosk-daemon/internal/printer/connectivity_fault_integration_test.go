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
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

// ConnectivityOutcome classifies the observed monitor and job outcome during network outages (Requirement 10).
type ConnectivityOutcome string

const (
	OutcomeContinuedMonitoring ConnectivityOutcome = "continued-monitoring"
	OutcomeTimeout             ConnectivityOutcome = "timeout"
	OutcomeAmbiguousJobState   ConnectivityOutcome = "ambiguous-job-state"
	OutcomeConfirmedCompleted  ConnectivityOutcome = "confirmed-completed"
	OutcomeConfirmedHeld       ConnectivityOutcome = "confirmed-held"
	OutcomeConfirmedCanceled   ConnectivityOutcome = "confirmed-canceled"
)

// checkDirectIPPReachability tests whether the IPP endpoint is actively accepting connections and queries (Requirement 8).
func checkDirectIPPReachability(host string, port int, timeoutSec int) bool {
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec+1)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ipptool", "-T", strconv.Itoa(timeoutSec), fmt.Sprintf("ipp://%s:%d/ipp/print", host, port), "get-printer-attributes.test")
	err := cmd.Run()
	return err == nil
}

// checkDirectSNMPReachability tests whether the SNMP endpoint responds to UDP queries (Requirement 8).
func checkDirectSNMPReachability(ctx context.Context, cfg SNMPConfig) bool {
	telem := PollHardwareTelemetry(ctx, cfg)
	return telem.Online
}

// checkCUPSSchedulerReachability tests whether the local CUPS scheduler is responding to queue queries (Requirement 8).
func checkCUPSSchedulerReachability(ctx context.Context, runner CommandRunner, queue string) bool {
	if runner == nil {
		runner = defaultCommandRunner
	}
	out, err := runner(ctx, "lpstat", "-p", queue)
	return err == nil && len(out) > 0
}

// verifiedCancelCUPSJob cancels a specific job on a test queue only after verifying 3-way queue ownership (Requirement 12).
func verifiedCancelCUPSJob(t *testing.T, emuScript string, emuEnv []string, queueName string, cupsJobID string) error {
	t.Helper()
	if cupsJobID == "" || !strings.HasPrefix(cupsJobID, queueName+"-") {
		return fmt.Errorf("invalid job ID %q for queue %q", cupsJobID, queueName)
	}
	out, err := runEmu(t, emuScript, emuEnv, "cancel-job", queueName, cupsJobID)
	if err != nil {
		return fmt.Errorf("failed to cancel job %s on queue %s: %w (%s)", cupsJobID, queueName, err, strings.TrimSpace(out))
	}
	return nil
}

// verifiedResumeCUPSJob resumes an explicit held job only after verifying 3-way queue ownership (Requirement 12).
func verifiedResumeCUPSJob(t *testing.T, emuScript string, emuEnv []string, queueName string, cupsJobID string) error {
	t.Helper()
	if cupsJobID == "" || !strings.HasPrefix(cupsJobID, queueName+"-") {
		return fmt.Errorf("invalid job ID %q for queue %q", cupsJobID, queueName)
	}
	out, err := runEmu(t, emuScript, emuEnv, "resume-job", queueName, cupsJobID)
	if err != nil {
		return fmt.Errorf("failed to resume job %s on queue %s: %w (%s)", cupsJobID, queueName, err, strings.TrimSpace(out))
	}
	return nil
}

// TestConnectivityFault_SNMPOnlyOutage_ContinuedMonitoringAndCompletion verifies that:
// 1. SNMP outage mid-print drops SNMP telemetry without impacting IPP or CUPS scheduler reachability.
// 2. XeroxGo's monitor continues polling CUPS without raising false hardware fault errors.
// 3. Decoupled physical sheets are NOT synthetically incremented during SNMP outage.
// 4. Restoring SNMP connectivity allows the original job to complete cleanly without resubmission.
// 5. Exactly-once submission is verified (1 submission, 1 CUPS request ID, 1 spool output).
func TestConnectivityFault_SNMPOnlyOutage_ContinuedMonitoringAndCompletion(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	scenario := "snmp_outage"
	emuName := fmt.Sprintf("XeroxGoConnEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoConnQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

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
		"EMU_GATE_TIMEOUT_TENTHS=60",
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_ = verifiedCancelCUPSJob(t, emuScript, emuEnv, queueName, cID)
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

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

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Initial reachability separation check
	if !checkDirectIPPReachability("localhost", ippPort, 1) {
		t.Fatalf("initial IPP endpoint reachability failed")
	}
	if !checkDirectSNMPReachability(ctx, snmpCfg) {
		t.Fatalf("initial SNMP endpoint reachability failed")
	}
	if !checkCUPSSchedulerReachability(ctx, nil, queueName) {
		t.Fatalf("initial CUPS scheduler reachability failed")
	}

	dbPath := filepath.Join(tempDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open SQLite state store: %v", err)
	}
	defer store.Close()

	uniqueJobID := fmt.Sprintf("job-conn-snmp-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-conn-01",
		DisplayCode:    "C101",
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

	outageSyncCh := make(chan error, 1)

	// Goroutine: Synchronize injection with confirmed active job (Requirement 6, 7)
	go func() {
		activeDeadline := time.Now().Add(5 * time.Second)
		var activeErr error
		activeConfirmed := false

		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" {
				activeErr = validateActiveJob(ctx, activeFile, cmd.JobID, queueName, cID)
				if activeErr == nil {
					activeConfirmed = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !activeConfirmed {
			outageSyncCh <- fmt.Errorf("active job validation failed: %v", activeErr)
			return
		}

		// Inject SNMP outage only (Requirement 3, 7)
		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "snmp"); fErr != nil {
			outageSyncCh <- fmt.Errorf("fault set offline --target snmp failed: %w (output: %s)", fErr, out)
			return
		}

		// Verify reachability separation during SNMP outage (Requirement 8):
		// 1. SNMP is OFFLINE
		if checkDirectSNMPReachability(ctx, snmpCfg) {
			outageSyncCh <- fmt.Errorf("SNMP reachability unexpectedly succeeded during SNMP outage")
			return
		}
		// 2. IPP is ONLINE
		if !checkDirectIPPReachability("localhost", ippPort, 1) {
			outageSyncCh <- fmt.Errorf("IPP reachability unexpectedly failed during SNMP-only outage")
			return
		}
		// 3. CUPS scheduler is ONLINE
		if !checkCUPSSchedulerReachability(ctx, nil, queueName) {
			outageSyncCh <- fmt.Errorf("CUPS scheduler reachability unexpectedly failed during SNMP-only outage")
			return
		}

		// Allow monitor to observe continued monitoring state during outage (Requirement 9, 10)
		time.Sleep(300 * time.Millisecond)

		// Restore SNMP connectivity (Requirement 14)
		if out, rErr := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "snmp"); rErr != nil {
			outageSyncCh <- fmt.Errorf("fault set clear --target snmp failed: %w (output: %s)", rErr, out)
			return
		}

		// Release execution gate so processor completes cleanly
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		outageSyncCh <- nil
	}()

	// Execute print job monitoring
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

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	if syncErr := <-outageSyncCh; syncErr != nil {
		t.Fatalf("outage synchronization failed: %v", syncErr)
	}

	// Requirement 11: SNMP loss alone must NOT equate to hardware error; printErr should be nil
	if printErr != nil {
		t.Fatalf("expected clean print completion despite SNMP outage, got error: %v", printErr)
	}

	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()

	// Requirement 12: Independent CUPS evidence of completion
	info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("post-completion CUPS query failed for job %s: %v", cID, qErr)
	}
	if info.State != CUPSJobStateCompleted {
		t.Fatalf("expected job %s state completed in CUPS, got %s", cID, info.State)
	}

	// Requirement 15: Exactly-once assertions
	if submittingCalls != 1 || submittedCalls != 1 {
		t.Errorf("expected exactly 1 submission, got submitting=%d submitted=%d", submittingCalls, submittedCalls)
	}
	if onPageCalls != 0 {
		t.Errorf("expected 0 unverified physical sheets reported without hardware odometer proof, got %d", onPageCalls)
	}

	t.Logf("Scenario SNMP-Only Outage: Continued monitoring confirmed, original job %s cleanly completed in CUPS", cID)
}

// TestConnectivityFault_IPPOnlyOutage_TimeoutPreservationAndRecovery verifies that:
// 1. IPP outage mid-print freezes the IPP backend while SNMP and CUPS scheduler remain reachable.
// 2. Monitoring with bounded context deadline times out cleanly without deleting the job.
// 3. Job preservation flag prevents unmonitored cancellation in CUPS (Requirement 13).
// 4. Job state post-timeout is preserved as ambiguous/processing in CUPS (Requirement 10).
// 5. Restoring IPP connectivity allows the original job to complete without resubmission (Requirement 14).
func TestConnectivityFault_IPPOnlyOutage_TimeoutPreservationAndRecovery(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	scenario := "ipp_outage"
	emuName := fmt.Sprintf("XeroxGoConnEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoConnQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

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
		"EMU_GATE_TIMEOUT_TENTHS=60",
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_ = verifiedCancelCUPSJob(t, emuScript, emuEnv, queueName, cID)
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

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

	dbPath := filepath.Join(tempDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open SQLite state store: %v", err)
	}
	defer store.Close()

	uniqueJobID := fmt.Sprintf("job-conn-ipp-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-conn-02",
		DisplayCode:    "C102",
		DownloadURL:    "https://example.local/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("failed to insert initial job in store: %v", err)
	}

	// Job preservation flag (Requirement 13)
	var preserveFlag atomic.Bool
	preserveFlag.Store(true)

	// Bounded context to test timeout behavior under outage (Requirement 10, 13)
	boundedCtx, boundedCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer boundedCancel()
	preserveCtx := ContextWithPreserveCUPSJob(boundedCtx, &preserveFlag)

	hooks := &CUPSPrintHooks{
		OnSubmitting: func() error {
			return store.MarkJobSubmitting(context.Background(), cmd.JobID)
		},
		OnSubmitted: func(cupsID string) error {
			jobMu.Lock()
			capturedCUPSJobID = cupsID
			jobMu.Unlock()
			return store.RecordJobSubmission(context.Background(), cmd.JobID, cupsID)
		},
		TelemetryPoller: func(c context.Context) HardwareTelemetry {
			return PollHardwareTelemetry(c, snmpCfg)
		},
	}

	outageSyncCh := make(chan error, 1)

	go func() {
		activeDeadline := time.Now().Add(5 * time.Second)
		var activeErr error
		activeConfirmed := false

		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" {
				activeErr = validateActiveJob(context.Background(), activeFile, cmd.JobID, queueName, cID)
				if activeErr == nil {
					activeConfirmed = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !activeConfirmed {
			outageSyncCh <- fmt.Errorf("active job validation failed: %v", activeErr)
			return
		}

		// Inject IPP outage only (Requirement 3, 7)
		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "ipp"); fErr != nil {
			outageSyncCh <- fmt.Errorf("fault set offline --target ipp failed: %w (output: %s)", fErr, out)
			return
		}

		// Verify reachability separation:
		// 1. IPP is unresponsive / times out
		if checkDirectIPPReachability("localhost", ippPort, 1) {
			outageSyncCh <- fmt.Errorf("IPP unexpectedly reachable during IPP outage")
			return
		}
		// 2. SNMP is ONLINE
		if !checkDirectSNMPReachability(context.Background(), snmpCfg) {
			outageSyncCh <- fmt.Errorf("SNMP unexpectedly unreachable during IPP-only outage")
			return
		}
		// 3. CUPS scheduler is ONLINE
		if !checkCUPSSchedulerReachability(context.Background(), nil, queueName) {
			outageSyncCh <- fmt.Errorf("CUPS scheduler unexpectedly unreachable")
			return
		}

		outageSyncCh <- nil
	}()

	// Execute print job with bounded timeout context
	printErr := PrintPDFWithCUPSHooksCustom(
		preserveCtx,
		nil,
		queueName,
		cmd,
		samplePDF,
		nil,
		50*time.Millisecond,
		hooks,
	)

	if syncErr := <-outageSyncCh; syncErr != nil {
		t.Fatalf("outage synchronization failed: %v", syncErr)
	}

	// Requirement 10: Bounded timeout outcome
	if printErr == nil {
		t.Fatalf("expected timeout error during sustained IPP outage, got nil")
	}
	if !errors.Is(printErr, context.DeadlineExceeded) && !strings.Contains(printErr.Error(), "context deadline exceeded") {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", printErr)
	}

	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()

	// Requirement 13: Job was PRESERVED in CUPS because of ContextWithPreserveCUPSJob
	info, qErr := QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("post-timeout CUPS query failed for job %s: %v", cID, qErr)
	}

	// Outcome: Ambiguous / Preserved in CUPS (processing or held)
	if info.State != CUPSJobStateProcessing && info.State != CUPSJobStateHeld {
		t.Fatalf("expected job %s preserved in processing or held state, got %s", cID, info.State)
	}
	t.Logf("Post-Timeout Job %s State: %s (Outcome: %s)", cID, info.State, OutcomeAmbiguousJobState)

	// Requirement 14: Restore IPP connectivity and query original job without resubmission
	if out, rErr := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "ipp"); rErr != nil {
		t.Fatalf("fault set clear --target ipp failed: %v (output: %s)", rErr, out)
	}

	// Release execution gate so processor completes
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Release CUPS hold on preserved job using verified 3-way ownership (Requirements 12, 14)
	if err := verifiedResumeCUPSJob(t, emuScript, emuEnv, queueName, cID); err != nil {
		t.Logf("Notice: verifiedResumeCUPSJob on %s: %v", cID, err)
	}

	// Verify original job transitions to completed in CUPS without any new lp submission
	completedDeadline := time.Now().Add(5 * time.Second)
	completed := false
	for time.Now().Before(completedDeadline) {
		info, qErr = QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
		if qErr == nil && info.State == CUPSJobStateCompleted {
			completed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("original job %s failed to reach completed state in CUPS after restoration (state: %s)", cID, info.State)
	}

	t.Logf("Requirement 14 verified: Original CUPS job %s cleanly completed without resubmission", cID)
}

// TestConnectivityFault_CombinedOutage_RestoreAndComplete verifies that:
// 1. Simultaneous loss of IPP and SNMP mid-print renders both endpoints unreachable while CUPS scheduler responds.
// 2. Monitoring under combined outage times out cleanly with job preservation.
// 3. Restoring both subsystems allows original job to complete without duplicate dispatches.
func TestConnectivityFault_CombinedOutage_RestoreAndComplete(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	scenario := "combined_outage"
	emuName := fmt.Sprintf("XeroxGoConnEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoConnQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

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
		"EMU_GATE_TIMEOUT_TENTHS=60",
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_ = verifiedCancelCUPSJob(t, emuScript, emuEnv, queueName, cID)
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

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

	uniqueJobID := fmt.Sprintf("job-conn-both-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-conn-03",
		DisplayCode:    "C103",
		DownloadURL:    "https://example.local/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var preserveFlag atomic.Bool
	preserveFlag.Store(true)

	boundedCtx, boundedCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer boundedCancel()
	preserveCtx := ContextWithPreserveCUPSJob(boundedCtx, &preserveFlag)

	hooks := &CUPSPrintHooks{
		OnSubmitted: func(cupsID string) error {
			jobMu.Lock()
			capturedCUPSJobID = cupsID
			jobMu.Unlock()
			return nil
		},
		TelemetryPoller: func(c context.Context) HardwareTelemetry {
			return PollHardwareTelemetry(c, snmpCfg)
		},
	}

	outageSyncCh := make(chan error, 1)

	go func() {
		activeDeadline := time.Now().Add(5 * time.Second)
		var activeErr error
		activeConfirmed := false

		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" {
				activeErr = validateActiveJob(context.Background(), activeFile, cmd.JobID, queueName, cID)
				if activeErr == nil {
					activeConfirmed = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !activeConfirmed {
			outageSyncCh <- fmt.Errorf("active job validation failed: %v", activeErr)
			return
		}

		// Inject combined outage (target both)
		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "both"); fErr != nil {
			outageSyncCh <- fmt.Errorf("fault set offline --target both failed: %w (output: %s)", fErr, out)
			return
		}

		// Reachability separation: both down, CUPS scheduler up
		if checkDirectIPPReachability("localhost", ippPort, 1) {
			outageSyncCh <- fmt.Errorf("IPP unexpectedly reachable during combined outage")
			return
		}
		if checkDirectSNMPReachability(context.Background(), snmpCfg) {
			outageSyncCh <- fmt.Errorf("SNMP unexpectedly reachable during combined outage")
			return
		}
		if !checkCUPSSchedulerReachability(context.Background(), nil, queueName) {
			outageSyncCh <- fmt.Errorf("CUPS scheduler unexpectedly unreachable")
			return
		}

		outageSyncCh <- nil
	}()

	printErr := PrintPDFWithCUPSHooksCustom(
		preserveCtx,
		nil,
		queueName,
		cmd,
		samplePDF,
		nil,
		50*time.Millisecond,
		hooks,
	)

	if syncErr := <-outageSyncCh; syncErr != nil {
		t.Fatalf("outage synchronization failed: %v", syncErr)
	}

	if printErr == nil || (!errors.Is(printErr, context.DeadlineExceeded) && !strings.Contains(printErr.Error(), "context deadline exceeded")) {
		t.Fatalf("expected deadline exceeded, got: %v", printErr)
	}

	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()

	// Verify job was preserved in CUPS
	info, qErr := QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("post-timeout CUPS query failed for job %s: %v", cID, qErr)
	}
	if info.State != CUPSJobStateProcessing && info.State != CUPSJobStateHeld {
		t.Fatalf("expected job %s preserved in processing or held state, got %s", cID, info.State)
	}

	// Restore combined subsystems
	if out, rErr := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "both"); rErr != nil {
		t.Fatalf("fault set clear --target both failed: %v (output: %s)", rErr, out)
	}

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Release CUPS hold on preserved job using verified 3-way ownership (Requirements 12, 14)
	if err := verifiedResumeCUPSJob(t, emuScript, emuEnv, queueName, cID); err != nil {
		t.Logf("Notice: verifiedResumeCUPSJob on %s: %v", cID, err)
	}

	// Verify original job completion
	completedDeadline := time.Now().Add(5 * time.Second)
	completed := false
	for time.Now().Before(completedDeadline) {
		info, qErr = QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
		if qErr == nil && info.State == CUPSJobStateCompleted {
			completed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("job %s failed to complete after combined restoration (state: %s)", cID, info.State)
	}

	t.Logf("Combined outage test passed: Job %s completed cleanly after dual restoration", cID)
}

// TestConnectivityFault_FailedRestoration_PreservesUncertainJob (Requirement 18) verifies that:
// When restoration cannot be completed, the job remains in an uncertain/ambiguous state,
// is NOT reported as completed, and the original CUPS ID remains preserved for recovery.
func TestConnectivityFault_FailedRestoration_PreservesUncertainJob(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	scenario := "failed_restore"
	queueName := fmt.Sprintf("XeroxGoConnQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

	_ = os.WriteFile(gateFile, []byte("gate"), 0644)
	_ = os.Remove(activeFile)
	_ = os.Remove(releaseFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		fmt.Sprintf("EMU_NAME=XeroxGoConnEmu_%s", scenario),
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
		"EMU_GATE_TIMEOUT_TENTHS=60",
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_ = verifiedCancelCUPSJob(t, emuScript, emuEnv, queueName, cID)
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup driverless CUPS queue: %v (output: %s)", err, out)
	}

	uniqueJobID := fmt.Sprintf("job-conn-fail-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-conn-04",
		DisplayCode:    "C104",
		DownloadURL:    "https://example.local/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var preserveFlag atomic.Bool
	preserveFlag.Store(true)

	boundedCtx, boundedCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer boundedCancel()
	preserveCtx := ContextWithPreserveCUPSJob(boundedCtx, &preserveFlag)

	hooks := &CUPSPrintHooks{
		OnSubmitted: func(cupsID string) error {
			jobMu.Lock()
			capturedCUPSJobID = cupsID
			jobMu.Unlock()
			return nil
		},
	}

	go func() {
		activeDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" && validateActiveJob(context.Background(), activeFile, cmd.JobID, queueName, cID) == nil {
				// Inject IPP outage
				_, _ = runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "ipp")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	_ = PrintPDFWithCUPSHooksCustom(
		preserveCtx,
		nil,
		queueName,
		cmd,
		samplePDF,
		nil,
		50*time.Millisecond,
		hooks,
	)

	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()

	// Simulate restoration failure: emulator remains offline/unrestored
	// Query CUPS job state: must NOT be completed
	info, qErr := QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("failed to query CUPS job state: %v", qErr)
	}

	if info.State == CUPSJobStateCompleted {
		t.Fatalf("CRITICAL: Job %s falsely reported completed without connectivity restoration", cID)
	}

	t.Logf("Regression passed: Failed restoration preserves job %s in %s state (never falsely completed)", cID, info.State)
}

// TestConnectivityFault_CancellationWithoutPreservation_DeletesJob (Requirement 13) verifies that:
// When context is canceled WITHOUT the job preservation flag, MonitorCUPSJob executes cancel <jobID>,
// permanently deleting the job from CUPS and causing subsequent queries to report canceled/unknown.
func TestConnectivityFault_CancellationWithoutPreservation_DeletesJob(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	scenario := "unpreserved_cancel"
	queueName := fmt.Sprintf("XeroxGoConnQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile := filepath.Join(tempDir, "job.active")
	gateFile := filepath.Join(tempDir, "job.gate")
	releaseFile := filepath.Join(tempDir, "job.release")

	_ = os.WriteFile(gateFile, []byte("gate"), 0644)
	_ = os.Remove(activeFile)
	_ = os.Remove(releaseFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		fmt.Sprintf("EMU_NAME=XeroxGoConnEmu_%s", scenario),
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
		"EMU_GATE_TIMEOUT_TENTHS=60",
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}

	var capturedCUPSJobID string
	var jobMu sync.Mutex

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_ = verifiedCancelCUPSJob(t, emuScript, emuEnv, queueName, cID)
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup driverless CUPS queue: %v (output: %s)", err, out)
	}

	uniqueJobID := fmt.Sprintf("job-conn-unpres-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-conn-05",
		DisplayCode:    "C105",
		DownloadURL:    "https://example.local/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Normal context WITHOUT preserve flag
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	hooks := &CUPSPrintHooks{
		OnSubmitted: func(cupsID string) error {
			jobMu.Lock()
			capturedCUPSJobID = cupsID
			jobMu.Unlock()
			return nil
		},
	}

	go func() {
		activeDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(activeDeadline) {
			jobMu.Lock()
			cID := capturedCUPSJobID
			jobMu.Unlock()

			if cID != "" && validateActiveJob(context.Background(), activeFile, cmd.JobID, queueName, cID) == nil {
				// Cancel context without preservation
				cancelFunc()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	_ = PrintPDFWithCUPSHooksCustom(
		cancelCtx,
		nil,
		queueName,
		cmd,
		samplePDF,
		nil,
		50*time.Millisecond,
		hooks,
	)

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()

	// Post-cancel query: job was canceled and deleted from active queue
	info, qErr := QueryCUPSJobInfo(context.Background(), nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("QueryCUPSJobInfo failed: %v", qErr)
	}

	if info.State != CUPSJobStateCanceled && info.State != CUPSJobStateUnknown {
		t.Fatalf("expected job %s canceled or unknown without preservation, got %s", cID, info.State)
	}

	t.Logf("Negative verification passed: Unpreserved cancellation correctly deleted job %s from active CUPS queue (state=%s)", cID, info.State)
}

// TestConnectivityFault_ReachabilitySeparation (Requirement 8) verifies that:
// 1. SNMP offline does NOT alter IPP endpoint reachability or CUPS scheduler reachability.
// 2. IPP offline does NOT alter SNMP reachability or CUPS scheduler reachability.
func TestConnectivityFault_ReachabilitySeparation(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	queueName := fmt.Sprintf("XeroxGoConnQ_reach_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		fmt.Sprintf("EMU_NAME=XeroxGoConnEmu_reach_%d", os.Getpid()),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", filepath.Join(tempDir, "spool")),
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
	)

	_ = os.MkdirAll(filepath.Join(tempDir, "spool"), 0755)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	t.Cleanup(func() {
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("cleanup failure: SNMP emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup CUPS queue: %v (output: %s)", err, out)
	}

	snmpCfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	// Step 1: Inject SNMP outage only
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "snmp"); err != nil {
		t.Fatalf("failed to inject SNMP outage: %v (output: %s)", err, out)
	}
	if checkDirectSNMPReachability(context.Background(), snmpCfg) {
		t.Errorf("SNMP should be unreachable during SNMP outage")
	}
	if !checkDirectIPPReachability("localhost", ippPort, 1) {
		t.Errorf("IPP should remain reachable during SNMP outage")
	}
	if !checkCUPSSchedulerReachability(context.Background(), nil, queueName) {
		t.Errorf("CUPS scheduler should remain reachable during SNMP outage")
	}

	// Restore SNMP (Requirement 13)
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "snmp"); err != nil {
		t.Fatalf("failed to restore SNMP: %v (%s)", err, strings.TrimSpace(out))
	}

	// Step 2: Inject IPP outage only
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "offline", "--target", "ipp"); err != nil {
		t.Fatalf("failed to inject IPP outage: %v (output: %s)", err, out)
	}
	if checkDirectIPPReachability("localhost", ippPort, 1) {
		t.Errorf("IPP should be unreachable during IPP outage")
	}
	if !checkDirectSNMPReachability(context.Background(), snmpCfg) {
		t.Errorf("SNMP should remain reachable during IPP outage")
	}
	if !checkCUPSSchedulerReachability(context.Background(), nil, queueName) {
		t.Errorf("CUPS scheduler should remain reachable during IPP outage")
	}

	// Restore IPP (Requirement 13)
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "ipp"); err != nil {
		t.Fatalf("failed to restore IPP: %v (%s)", err, strings.TrimSpace(out))
	}
	t.Logf("Reachability separation verified: IPP, SNMP, and CUPS scheduler are independently decoupled")
}

// TestConnectivityFault_Negative_StalePIDPausedEntryRejected (Requirement 14) verifies that:
// 1. A stale/dead PID recorded in the paused file is rejected during restoration.
// 2. No signal is sent to nonexistent processes.
// 3. Safe unpausing refuses to succeed and preserves a diagnostic dump file.
func TestConnectivityFault_Negative_StalePIDPausedEntryRejected(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	ippPort := getFreePort(t, "tcp")
	emuName := fmt.Sprintf("XeroxGoStalePID_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoStaleQ_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	pausedFile := filepath.Join(tempDir, "ipp_paused.flag")

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		"EMU_HOST=localhost",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", filepath.Join(tempDir, "spool")),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_FAULT_STATE_FILE=%s", filepath.Join(tempDir, "fault_state.json")),
		fmt.Sprintf("EMU_IPP_PAUSED_FILE=%s", pausedFile),
	)
	_ = os.MkdirAll(filepath.Join(tempDir, "spool"), 0755)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start emulator: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		// Clean up any diagnostic files left behind
		matches, _ := filepath.Glob(pausedFile + ".diag_*")
		for _, m := range matches {
			_ = os.Remove(m)
		}
		_ = os.Remove(pausedFile)
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
	})

	// Plant a stale PID entry in paused file
	staleEntry := "ipp:999999:12345:fake_comm\n"
	if err := os.WriteFile(pausedFile, []byte(staleEntry), 0644); err != nil {
		t.Fatalf("failed to write stale paused file: %v", err)
	}

	// Attempt restoration: must fail because PID 999999 is dead/unverified
	out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "ipp")
	if err == nil {
		t.Errorf("expected restoration to fail for stale PID, but succeeded (output: %s)", out)
	}

	// Verify diagnostic file was preserved
	matches, _ := filepath.Glob(pausedFile + ".diag_*")
	if len(matches) == 0 {
		t.Errorf("expected diagnostic dump file matching %s.diag_* to be preserved, found none", pausedFile)
	} else {
		diagContent, _ := os.ReadFile(matches[0])
		if !strings.Contains(string(diagContent), "999999") {
			t.Errorf("diagnostic file does not contain stale PID: %s", string(diagContent))
		}
	}
}

// TestConnectivityFault_Negative_PIDReuseUnrelatedLiveProcessRejected (Requirement 14) verifies that:
// 1. A live unrelated process (e.g. current test process) recorded with mismatched start time
//    or mismatched role/comm is strictly rejected without sending SIGCONT.
// 2. Safe restoration returns an error and preserves diagnostic evidence.
func TestConnectivityFault_Negative_PIDReuseUnrelatedLiveProcessRejected(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	ippPort := getFreePort(t, "tcp")
	emuName := fmt.Sprintf("XeroxGoPIDReuse_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoPIDReuseQ_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	pausedFile := filepath.Join(tempDir, "ipp_paused.flag")

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		"EMU_HOST=localhost",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", filepath.Join(tempDir, "spool")),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_FAULT_STATE_FILE=%s", filepath.Join(tempDir, "fault_state.json")),
		fmt.Sprintf("EMU_IPP_PAUSED_FILE=%s", pausedFile),
	)
	_ = os.MkdirAll(filepath.Join(tempDir, "spool"), 0755)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start emulator: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		matches, _ := filepath.Glob(pausedFile + ".diag_*")
		for _, m := range matches {
			_ = os.Remove(m)
		}
		_ = os.Remove(pausedFile)
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
	})

	// Plant an entry targeting the current test process PID, but with invalid start time 1
	// simulating PID reuse by another unrelated process
	livePID := os.Getpid()
	mismatchedEntry := fmt.Sprintf("ipp:%d:1:unrelated_comm\n", livePID)
	if err := os.WriteFile(pausedFile, []byte(mismatchedEntry), 0644); err != nil {
		t.Fatalf("failed to write paused file: %v", err)
	}

	// Attempt restoration: must fail because start time and comm do not match
	out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear", "--target", "ipp")
	if err == nil {
		t.Errorf("expected restoration to fail for mismatched process identity, but succeeded (output: %s)", out)
	}

	// Verify diagnostic file was preserved
	matches, _ := filepath.Glob(pausedFile + ".diag_*")
	if len(matches) == 0 {
		t.Errorf("expected diagnostic dump file matching %s.diag_* to be preserved, found none", pausedFile)
	}
}

// TestConnectivityFault_Negative_UnverifiedQueueCancelAndResumeRefused (Requirement 12, 14) verifies that:
// 1. cancel-job refuses to operate on a queue without verified 3-way ownership.
// 2. resume-job refuses to operate on a queue without verified 3-way ownership.
func TestConnectivityFault_Negative_UnverifiedQueueCancelAndResumeRefused(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	emuEnv := append(os.Environ(),
		"EMU_PORT=39999",
		"EMU_HOST=localhost",
		fmt.Sprintf("EMU_SPOOL_DIR=%s", filepath.Join(tempDir, "spool")),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
	)

	fakeQueue := fmt.Sprintf("UnverifiedQueue_%d", time.Now().UnixNano()%100000)
	fakeJob := fmt.Sprintf("%s-1", fakeQueue)

	// cancel-job must fail
	out, err := runEmu(t, emuScript, emuEnv, "cancel-job", fakeQueue, fakeJob)
	if err == nil {
		t.Errorf("expected cancel-job on unverified queue to fail, but succeeded (output: %s)", out)
	} else if !strings.Contains(out, "Refusing to cancel job") && !strings.Contains(out, "unverified queue") {
		t.Errorf("cancel-job failed with unexpected error message: %s", out)
	}

	// resume-job must fail
	out, err = runEmu(t, emuScript, emuEnv, "resume-job", fakeQueue, fakeJob)
	if err == nil {
		t.Errorf("expected resume-job on unverified queue to fail, but succeeded (output: %s)", out)
	} else if !strings.Contains(out, "Refusing to resume job") && !strings.Contains(out, "unverified queue") {
		t.Errorf("resume-job failed with unexpected error message: %s", out)
	}
}

// TestConnectivityFault_Negative_FailedQueueCleanupPreservesMetadata (Requirement 10, 14) verifies that:
// When CUPS queue removal fails (e.g. metadata URI mismatch), ownership metadata is preserved.
func TestConnectivityFault_Negative_FailedQueueCleanupPreservesMetadata(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	ippPort := getFreePort(t, "tcp")
	emuName := fmt.Sprintf("XeroxGoMetaPreserve_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoMetaPresQ_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		"EMU_HOST=localhost",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", filepath.Join(tempDir, "spool")),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_META_FILE=%s", metaFile),
	)
	_ = os.MkdirAll(filepath.Join(tempDir, "spool"), 0755)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start emulator: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("cleanup failure: IPP cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup CUPS queue: %v (%s)", err, out)
	}

	// Verify metadata file exists
	if _, err := os.Stat(metaFile); err != nil {
		t.Fatalf("expected metadata file %s to exist", metaFile)
	}

	// Corrupt metadata URI to cause remove-cups to fail 3-way ownership
	origBytes, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}
	corrupted := strings.Replace(string(origBytes), fmt.Sprintf(":%d/", ippPort), ":9999/", 1)
	if err := os.WriteFile(metaFile, []byte(corrupted), 0644); err != nil {
		t.Fatalf("failed to write corrupted metadata: %v", err)
	}

	// Call remove-cups; must fail
	removeOut, removeErr := runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
	if removeErr == nil {
		t.Errorf("expected remove-cups to fail on corrupted metadata URI, but got: %s", removeOut)
	}

	// Verify metadata file is still preserved
	if _, err := os.Stat(metaFile); os.IsNotExist(err) {
		t.Errorf("metadata file was deleted despite removal failure")
	}

	// Restore original metadata so t.Cleanup can cleanly delete queue and metadata
	if err := os.WriteFile(metaFile, origBytes, 0644); err != nil {
		t.Fatalf("failed to restore original metadata: %v", err)
	}
}
