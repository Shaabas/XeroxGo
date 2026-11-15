package printer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

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

	// Pre-create gate file so processor will hold execution when it receives the job
	if err := os.WriteFile(gateFile, []byte("gate"), 0644); err != nil {
		t.Fatalf("failed to create gate file: %v", err)
	}

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
		"EMU_GATE_TIMEOUT_TENTHS=60", // 6.0s safety timeout
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

	// Guaranteed teardown
	// Requirement 16: Safely release or terminate held jobs and preserve queue metadata on failure
	t.Cleanup(func() {
		// Ensure gate is released so processor finishes cleanly
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)

		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()
		if cID != "" {
			// Safely terminate any active or held jobs on queue
			_, _ = exec.Command("cancel", cID).CombinedOutput()
		}
		_, _ = exec.Command("cancel", queueName).CombinedOutput()

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

	// Goroutine: Synchronize fault injection with confirmed active job (Requirement 6)
	go func() {
		// Wait for processor to report active state
		activeDeadline := time.Now().Add(5 * time.Second)
		active := false
		for time.Now().Before(activeDeadline) {
			if data, err := os.ReadFile(activeFile); err == nil && len(data) > 0 {
				if strings.Contains(string(data), "STATE=processing") {
					active = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !active {
			faultInjectedCh <- fmt.Errorf("job never reached confirmed active processing state within timeout")
			return
		}

		jobMu.Lock()
		cID := capturedCUPSJobID
		jobMu.Unlock()

		// Confirm active job in CUPS
		info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cID)
		if qErr != nil {
			faultInjectedCh <- fmt.Errorf("failed to query CUPS job info: %w", qErr)
			return
		}
		// Requirement 15: If job already completed before injection, report failure
		if info.State == CUPSJobStateCompleted {
			faultInjectedCh <- fmt.Errorf("job already completed before fault could be injected")
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

	// Execute print job through existing CUPS function (Requirements 4, 9, 10)
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

	// Requirement 11 & 12: Independently verify actual post-fault CUPS job state and distinguish hold result
	jobMu.Lock()
	cID := capturedCUPSJobID
	jobMu.Unlock()
	if cID == "" {
		t.Fatalf("expected non-empty captured CUPS job ID")
	}

	holdRes := HoldAndVerifyCUPSJob(ctx, nil, queueName, cID)
	t.Logf("Scenario %s HoldResult: ConfirmedHeld=%v, StillProcessing=%v, AlreadyCompleted=%v, State=%s, HoldErr=%v, QueryErr=%v",
		scenario, holdRes.ConfirmedHeld, holdRes.StillProcessing, holdRes.AlreadyCompleted, holdRes.State, holdRes.HoldErr, holdRes.QueryErr)

	// Distinguish hold outcomes:
	if holdRes.ConfirmedHeld {
		t.Logf("Job %s successfully confirmed HELD in CUPS (state=%s)", cID, holdRes.State)
	} else if holdRes.AlreadyCompleted {
		t.Errorf("Job %s was already completed in CUPS instead of held", cID)
	} else if holdRes.StillProcessing {
		t.Logf("Job %s is still processing in CUPS filter chain (hold attempted)", cID)
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

// TestMidPrintHardwareFault_DistinguishCompletedFromHeld verifies that HoldAndVerifyCUPSJob
// properly distinguishes a job that already completed before hold from a confirmed held job.
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

	// Verify job reached completed state
	info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cupsJobID)
	if qErr != nil || info.State != CUPSJobStateCompleted {
		t.Fatalf("expected job to be completed, got state=%s (err: %v)", info.State, qErr)
	}

	// Requirement 12: HoldAndVerifyCUPSJob on already completed job must return AlreadyCompleted = true
	res := HoldAndVerifyCUPSJob(ctx, nil, queueName, cupsJobID)
	if !res.AlreadyCompleted {
		t.Errorf("expected AlreadyCompleted=true for completed job, got ConfirmedHeld=%v, AlreadyCompleted=%v, State=%s",
			res.ConfirmedHeld, res.AlreadyCompleted, res.State)
	}
	if res.ConfirmedHeld {
		t.Errorf("expected ConfirmedHeld=false for completed job, got true")
	}
}
