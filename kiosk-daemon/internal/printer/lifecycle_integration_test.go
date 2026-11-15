package printer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

// checkCUPSAndIPPDependencies verifies that all required CUPS CLI utilities,
// IPP emulator binaries, and the local CUPS scheduler service are operational.
func checkCUPSAndIPPDependencies(t *testing.T) {
	t.Helper()
	checkIPPDependencies(t)

	for _, tool := range []string{"lp", "lpadmin", "lpstat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s utility not found in PATH; skipping CUPS lifecycle integration test", tool)
		}
	}

	// Verify CUPS scheduler is actively running
	out, err := exec.Command("lpstat", "-r").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "scheduler is running") {
		t.Skipf("CUPS scheduler is not running (%v: %s); skipping CUPS lifecycle integration test", err, strings.TrimSpace(string(out)))
	}
}

// findSamplePDF locates the sample.pdf test fixture.
func findSamplePDF(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../../tools/printer-emulator/testdata/sample.pdf",
		"../../tools/printer-emulator/testdata/sample.pdf",
		"./tools/printer-emulator/testdata/sample.pdf",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
	}
	t.Fatalf("could not locate tools/printer-emulator/testdata/sample.pdf")
	return ""
}

// TestCUPSPrintLifecycle_RealEmulatorSubmissionAndTracking exercises the complete
// real-world XeroxGo print lifecycle against the live D1 IPP and D2 SNMP emulators:
//  1. Dedicated driverless CUPS queue backed by live ippeveprinter.
//  2. Live SNMP responder serving RFC MIBs and hardware telemetry.
//  3. Exact XeroxGo CUPS submission via PrintPDFWithCUPSHooksCustom.
//  4. Durable pre- and post-submission hooks writing to SQLite state.Store.
//  5. Capture and verification of real CUPS request ID (queue-jobNum).
//  6. Live job monitoring transitioning to CUPSJobStateCompleted.
//  7. Assertion of exactly one submission attempt per job.
//  8. Verification of genuine spool artifact receipt in emulator spool directory.
//  9. Verification that physical sheets delivered is NOT incremented solely by CUPS completion.
// 10. Querying real SNMP hardware telemetry via PollHardwareTelemetry.
// 11. Preserving system queue isolation and 3-way URI ownership protections.
func TestCUPSPrintLifecycle_RealEmulatorSubmissionAndTracking(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	if err := os.MkdirAll(spoolDir, 0755); err != nil {
		t.Fatalf("failed to create spool dir: %v", err)
	}

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	emuName := fmt.Sprintf("XeroxGoLifeEmu_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoLife_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)

	metaFile := filepath.Join(tempDir, "cups.meta")
	ippPIDFile := filepath.Join(tempDir, "ipp.pid")
	ippLogFile := filepath.Join(tempDir, "ipp.log")
	snmpPIDFile := filepath.Join(tempDir, "snmp.pid")
	snmpLogFile := filepath.Join(tempDir, "snmp.log")
	snmpStateFile := filepath.Join(tempDir, "snmp_state.json")
	faultStateFile := filepath.Join(tempDir, "fault_state.json")

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		fmt.Sprintf("EMU_NAME=%s", emuName),
		fmt.Sprintf("EMU_CUPS_QUEUE=%s", queueName),
		fmt.Sprintf("EMU_SPOOL_DIR=%s", spoolDir),
		fmt.Sprintf("EMU_PID_FILE=%s", ippPIDFile),
		fmt.Sprintf("EMU_LOG_FILE=%s", ippLogFile),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("EMU_META_FILE=%s", metaFile),
		fmt.Sprintf("SNMP_PID_FILE=%s", snmpPIDFile),
		fmt.Sprintf("SNMP_LOG_FILE=%s", snmpLogFile),
		fmt.Sprintf("SNMP_STATE_FILE=%s", snmpStateFile),
		fmt.Sprintf("SNMP_LOCK_DIR=%s", filepath.Join(tempDir, ".snmp_lock")),
		fmt.Sprintf("EMU_FAULT_STATE_FILE=%s", faultStateFile),
		fmt.Sprintf("EMU_IPP_PAUSED_FILE=%s", filepath.Join(tempDir, "ipp_paused.flag")),
	)

	// Start IPP and SNMP emulators
	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	// Register guaranteed teardown
	t.Cleanup(func() {
		// Clean up CUPS test queue with verified 3-way URI check
		if _, err := runEmu(t, emuScript, emuEnv, "remove-cups"); err != nil {
			t.Logf("cleanup warning: remove-cups: %v", err)
		}
		// Clean up IPP emulator and verified spool files
		if _, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Logf("cleanup warning: IPP cleanup: %v", err)
		}
		// Clean up SNMP emulator
		if _, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Logf("cleanup warning: SNMP cleanup: %v", err)
		}
	})

	// Setup driverless CUPS queue pointing to emulator IPP endpoint
	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup driverless CUPS queue %q: %v (output: %s)", queueName, err, out)
	}

	// Verify CUPS queue is registered and idle
	lpstatOut, lpstatErr := exec.Command("lpstat", "-p", queueName).CombinedOutput()
	if lpstatErr != nil || !strings.Contains(string(lpstatOut), queueName) {
		t.Fatalf("CUPS queue %q was not registered properly: %v (output: %s)", queueName, lpstatErr, string(lpstatOut))
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

	cmd := contracts.PrintJobCommand{
		JobID:          "job-live-lifecycle-001",
		KioskID:        "kiosk-live-01",
		DisplayCode:    "X742",
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

	// Tracking counters to assert exactly one submission attempt per accepted job
	submittingCalls := 0
	submittedCalls := 0
	var capturedCUPSJobID string
	var onPageCalls []int

	hooks := &CUPSPrintHooks{
		OnSubmitting: func() error {
			submittingCalls++
			return store.MarkJobSubmitting(ctx, cmd.JobID)
		},
		OnSubmitted: func(cupsJobID string) error {
			submittedCalls++
			capturedCUPSJobID = cupsJobID
			return store.RecordJobSubmission(ctx, cmd.JobID, cupsJobID)
		},
		SNMPConfig: &snmpCfg,
		TelemetryPoller: func(pollCtx context.Context) HardwareTelemetry {
			return PollHardwareTelemetry(pollCtx, snmpCfg)
		},
	}

	onPage := func(sheetsCompleted int) {
		onPageCalls = append(onPageCalls, sheetsCompleted)
	}

	// Execute real CUPS submission and monitoring
	pollInterval := 100 * time.Millisecond
	printErr := PrintPDFWithCUPSHooksCustom(ctx, nil, queueName, cmd, samplePDF, onPage, pollInterval, hooks)
	if printErr != nil {
		t.Fatalf("PrintPDFWithCUPSHooksCustom failed: %v", printErr)
	}

	// Requirement 11: Assert exactly one submission per accepted test job
	if submittingCalls != 1 {
		t.Errorf("OnSubmitting called %d times, want exactly 1", submittingCalls)
	}
	if submittedCalls != 1 {
		t.Errorf("OnSubmitted called %d times, want exactly 1", submittedCalls)
	}

	// Requirement 5: Capture and verify real CUPS request ID format
	expectedPrefix := queueName + "-"
	if !strings.HasPrefix(capturedCUPSJobID, expectedPrefix) {
		t.Errorf("captured CUPS job ID %q does not match expected prefix %q", capturedCUPSJobID, expectedPrefix)
	}
	idPattern := regexp.MustCompile(fmt.Sprintf("^%s-[0-9]+$", regexp.QuoteMeta(queueName)))
	if !idPattern.MatchString(capturedCUPSJobID) {
		t.Errorf("captured CUPS job ID %q does not match regex pattern %q", capturedCUPSJobID, idPattern.String())
	}

	// Requirement 10: Verify durable recording of CUPS submission ID in state store
	storedJob, err := store.GetJob(ctx, cmd.JobID)
	if err != nil {
		t.Fatalf("failed to retrieve stored job from state store: %v", err)
	}
	if !storedJob.SubmittedToPrinter {
		t.Errorf("expected stored job SubmittedToPrinter = true, got false")
	}
	if storedJob.CUPSJobID != capturedCUPSJobID {
		t.Errorf("expected stored job CUPSJobID = %q, got %q", capturedCUPSJobID, storedJob.CUPSJobID)
	}

	// Requirement 7: Verify completion using CUPS job state query
	jobInfo, queryErr := QueryCUPSJobInfo(ctx, nil, queueName, capturedCUPSJobID)
	if queryErr != nil {
		t.Fatalf("QueryCUPSJobInfo failed for %s: %v", capturedCUPSJobID, queryErr)
	}
	if jobInfo.State != CUPSJobStateCompleted {
		t.Errorf("CUPS job %s state = %q, want %q", capturedCUPSJobID, jobInfo.State, CUPSJobStateCompleted)
	}

	// Requirement 13 & 9: Physical sheet delivery is decoupled from CUPS completion.
	// Since ippeveprinter writes the file directly without advancing physical hardware odometer,
	// hardware odometer remains unchanged and onPage must NEVER be called solely because CUPS completed.
	if len(onPageCalls) != 0 {
		t.Errorf("physical sheets marked delivered (%v) solely due to CUPS completion; physical delivery must require hardware odometer proof", onPageCalls)
	}

	// Verify genuine spool artifact receipt in emulator spool directory
	spoolArtifacts, err := filepath.Glob(filepath.Join(spoolDir, "1-*"))
	if err != nil || len(spoolArtifacts) == 0 {
		t.Errorf("expected spool artifacts in %s matching '1-*', got %v (err: %v)", spoolDir, spoolArtifacts, err)
	}

	// Requirement 8: Query actual SNMP emulator through XeroxGo telemetry client
	telem := PollHardwareTelemetry(ctx, snmpCfg)
	if !telem.Online {
		t.Errorf("expected SNMP emulator Online = true, got false (%s)", telem.ErrorMsg)
	}
	if !telem.HasLife {
		t.Errorf("expected SNMP emulator HasLife = true, got false")
	}
	if telem.LifeCount != 1000 {
		t.Errorf("expected baseline SNMP marker life count 1000, got %d", telem.LifeCount)
	}
	if telem.Flags.Jammed || telem.Flags.DoorOpen || telem.Flags.NoPaper {
		t.Errorf("expected healthy zero error flags, got Jammed=%v DoorOpen=%v NoPaper=%v",
			telem.Flags.Jammed, telem.Flags.DoorOpen, telem.Flags.NoPaper)
	}
}

// TestCUPSPrintLifecycle_SyntheticOdometerAdvancement verifies that SNMP odometer
// updates are accurately reflected by PollHardwareTelemetry, but explicitly treats
// the emulator odometer as synthetic telemetry rather than proof of mechanical sheet ejection.
func TestCUPSPrintLifecycle_SyntheticOdometerAdvancement(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	binPath := buildOrFindSNMPResponder(t)
	tempDir := t.TempDir()

	stateFile := filepath.Join(tempDir, "snmp_state.json")
	pidFile := filepath.Join(tempDir, "snmp_responder.pid")
	portFile := filepath.Join(tempDir, "snmp_responder.port")

	cmd := exec.Command(binPath, "serve",
		"-host", "127.0.0.1",
		"-port", "0",
		"-community", "public",
		"-state", stateFile,
		"-pid", pidFile,
		"-port-file", portFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start snmp_responder: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()
		}
	}()

	var boundPort int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			var p int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &p); err == nil && p > 0 {
				boundPort = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if boundPort <= 0 {
		t.Fatalf("snmp_responder failed to write bound port")
	}

	snmpCfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      boundPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	ctx := context.Background()

	// Initial baseline odometer query
	telem1 := PollHardwareTelemetry(ctx, snmpCfg)
	if !telem1.Online || !telem1.HasLife || telem1.LifeCount != 1000 {
		t.Fatalf("expected initial life count 1000, got Online=%v Life=%d", telem1.Online, telem1.LifeCount)
	}

	// Update counter synthetically via snmp_responder CLI
	setCmd := exec.Command(binPath, "set", "-state", stateFile, "-counter", "1005")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to set counter: %v (%s)", err, string(out))
	}

	// Verify updated telemetry reflection
	telem2 := PollHardwareTelemetry(ctx, snmpCfg)
	if !telem2.Online || !telem2.HasLife {
		t.Fatalf("expected online telemetry after update, got Online=%v", telem2.Online)
	}
	if telem2.LifeCount != 1005 {
		t.Errorf("expected synthetic life count 1005, got %d", telem2.LifeCount)
	}

	// Document explicit limitation:
	// In the local software emulator, this counter is synthetic and driven by state configuration.
	// Physical paper ejection cannot be guaranteed solely by software odometer increments.
}

// TestCUPSJobQuery_LiveTransitions exercises QueryCUPSJobInfo directly against a real CUPS job
// dispatched to the emulator queue, verifying state parsing, reason attributes, and completed status.
func TestCUPSJobQuery_LiveTransitions(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	samplePDF := findSamplePDF(t)
	tempDir := t.TempDir()

	spoolDir := filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")
	emuName := fmt.Sprintf("XeroxGoQueryEmu_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoQuery_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)

	metaFile := filepath.Join(tempDir, "cups.meta")
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
	)

	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start emulator: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		_, _ = runEmu(t, emuScript, emuEnv, "remove-cups")
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup")
	})

	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup CUPS queue: %v (%s)", err, out)
	}

	// Submit via lp directly
	ctx := context.Background()
	lpCmd := exec.CommandContext(ctx, "lp", "-d", queueName, "-t", "direct-query-test", samplePDF)
	lpOut, err := lpCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lp submission failed: %v (%s)", err, string(lpOut))
	}

	match := cupsJobIDRegex.FindStringSubmatch(string(lpOut))
	if len(match) < 2 {
		t.Fatalf("failed to parse CUPS request id from lp output: %s", string(lpOut))
	}
	cupsJobID := match[1]

	// Poll QueryCUPSJobInfo until job completes in CUPS
	deadline := time.Now().Add(10 * time.Second)
	completed := false
	var lastInfo CUPSJobInfo
	for time.Now().Before(deadline) {
		info, qErr := QueryCUPSJobInfo(ctx, nil, queueName, cupsJobID)
		if qErr == nil {
			lastInfo = info
			if info.State == CUPSJobStateCompleted {
				completed = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("CUPS job %s did not reach completed state within timeout (last state: %s, reasons: %v)",
			cupsJobID, lastInfo.State, lastInfo.Reasons)
	}

	if lastInfo.State != CUPSJobStateCompleted {
		t.Errorf("expected final state CUPSJobStateCompleted, got %s", lastInfo.State)
	}
}
