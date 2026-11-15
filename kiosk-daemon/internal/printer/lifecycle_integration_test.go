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

// parseCUPSJobIDsFromLPStat extracts all job request IDs for the target queue from lpstat output.
func parseCUPSJobIDsFromLPStat(output string, queue string) []string {
	prefix := queue + "-"
	var ids []string
	lines := strings.Split(output, "\n")
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) > 0 && strings.HasPrefix(fields[0], prefix) {
			ids = append(ids, fields[0])
		}
	}
	return ids
}

// TestCUPSPrintLifecycle_RealEmulatorSubmissionAndTracking exercises the complete
// real-world XeroxGo print lifecycle against the live D1 IPP and D2 SNMP emulators:
//  1. Dedicated driverless CUPS queue backed by live ippeveprinter.
//  2. Live SNMP responder serving RFC MIBs and hardware telemetry.
//  3. Exact XeroxGo CUPS submission via PrintPDFWithCUPSHooksCustom.
//  4. Durable pre- and post-submission hooks writing to SQLite state.Store.
//  5. Capture and verification of real CUPS request ID (queue-jobNum).
//  6. Live job monitoring transitioning to CUPSJobStateCompleted.
//  7. Assertion of exactly one submission attempt per job (hooks, CUPS records, spool sequence).
//  8. Spool evidence distinguishing one submission with multiple artifacts (.pdf, .prn) from multiple submissions.
//  9. Verification that physical sheets delivered is NOT incremented solely by CUPS completion.
// 10. Querying real SNMP hardware telemetry via PollHardwareTelemetry.
// 11. Preserving system queue isolation and 3-way URI ownership protections.
// 12. Preserving recoverable ownership metadata outside t.TempDir() if queue deletion fails.
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

	// Requirement 4: Place ownership metadata file outside t.TempDir() so that if queue deletion
	// fails during teardown, recoverable ownership evidence is preserved on disk and not wiped out by Go's test runner.
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

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

	// Register guaranteed teardown:
	// Requirements 2, 4, 5: Verify queue ownership and never delete ownership metadata if queue deletion fails.
	t.Cleanup(func() {
		// Attempt verified removal of the test CUPS queue
		out, err := runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
		if err != nil {
			// Queue deletion failed: preserve ownership metadata outside tempDir and report error
			t.Errorf("cleanup failure: remove-cups failed for queue %q: %v (%s). Ownership metadata preserved at %s.",
				queueName, err, strings.TrimSpace(out), metaFile)
		} else {
			// Verified removal succeeded; ensure metadata file is removed
			_ = os.Remove(metaFile)
		}

		// Clean up IPP emulator and verified spool files
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Logf("cleanup warning: IPP cleanup: %v (%s)", err, out)
		}
		// Clean up SNMP emulator
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Logf("cleanup warning: SNMP cleanup: %v (%s)", err, out)
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

	// Requirement 7: Use a unique test job identity
	uniqueJobID := fmt.Sprintf("job-live-lifecycle-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)

	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
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

	// Requirement 6 & 11: Assert exactly one submission attempt via hooks
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

	// Requirement 7 & 9: Inspect actual CUPS job records to strengthen exactly-once assertion
	lpstatJobsCmd := exec.Command("lpstat", "-W", "all", queueName)
	lpstatJobsOut, _ := lpstatJobsCmd.CombinedOutput()
	cupsJobRecords := parseCUPSJobIDsFromLPStat(string(lpstatJobsOut), queueName)

	if len(cupsJobRecords) > 0 {
		// CUPS job history is retained on this host: verify exactly one job record was created
		if len(cupsJobRecords) != 1 {
			t.Errorf("CUPS job records indicate multiple submissions for queue %s: %v (want exactly 1: %s)",
				queueName, cupsJobRecords, capturedCUPSJobID)
		} else if cupsJobRecords[0] != capturedCUPSJobID {
			t.Errorf("CUPS job record %q does not match captured CUPS job ID %q", cupsJobRecords[0], capturedCUPSJobID)
		} else {
			t.Logf("CUPS job history confirms exactly one job (%s) was dispatched to queue %s", cupsJobRecords[0], queueName)
		}
	} else {
		// Requirement 9: CUPS job history not retained by cupsd; report limitation truthfully rather than claiming verification
		t.Logf("Notice: CUPS job history was empty for queue %s (PreserveJobHistory may be disabled in cupsd); exactly-once verification relies on atomic submission hooks, SQLite persistence record, and emulator spool artifact sequence rather than CUPS job history.", queueName)
	}

	// Requirement 7 & 8: Inspect emulator spool artifacts to distinguish one submission with
	// multiple artifacts (.pdf and .prn) from multiple independent submissions.
	spoolEntries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatalf("failed to read spool directory %s: %v", spoolDir, err)
	}

	sequenceMap := make(map[string][]string)
	for _, entry := range spoolEntries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Match artifacts associated with this print job
		if strings.Contains(name, uniqueJobID) && (strings.HasSuffix(name, ".pdf") || strings.HasSuffix(name, ".prn")) {
			parts := strings.SplitN(name, "-", 2)
			if len(parts) == 2 {
				seq := parts[0]
				sequenceMap[seq] = append(sequenceMap[seq], name)
			}
		}
	}

	if len(sequenceMap) == 0 {
		t.Fatalf("no spool artifacts found matching unique test job %s in %s", uniqueJobID, spoolDir)
	}

	// In a single submission, multiple artifact files (such as .pdf and .prn) share the SAME sequence prefix ("1-").
	// Multiple independent submissions would produce distinct sequence IDs ("1-", "2-", etc.).
	if len(sequenceMap) != 1 {
		t.Errorf("expected spool artifacts from exactly 1 submission sequence, got %d sequences: %v",
			len(sequenceMap), sequenceMap)
	} else {
		for seq, files := range sequenceMap {
			t.Logf("Spool evidence verified single submission (sequence %s) produced %d artifact file(s): %v",
				seq, len(files), files)
		}
	}

	// Requirement 7: Verify completion using CUPS job state query
	jobInfo, queryErr := QueryCUPSJobInfo(ctx, nil, queueName, capturedCUPSJobID)
	if queryErr != nil {
		t.Fatalf("QueryCUPSJobInfo failed for %s: %v", capturedCUPSJobID, queryErr)
	}
	if jobInfo.State != CUPSJobStateCompleted {
		t.Errorf("CUPS job %s state = %q, want %q", capturedCUPSJobID, jobInfo.State, CUPSJobStateCompleted)
	}

	// Requirement 11: Assert physical sheet delivery decoupling.
	// In XeroxGo architecture, physical sheet delivery is driven solely by hardware telemetry (odometer deltas).
	// CUPS completion alone must NEVER mark physical sheets delivered.
	// Since ippeveprinter writes the file directly without advancing physical hardware gears,
	// hardware odometer remains unchanged and onPage must NEVER be called solely because CUPS completed.
	if len(onPageCalls) != 0 {
		t.Errorf("physical sheets marked delivered (%v) solely due to CUPS completion; physical delivery must require hardware odometer proof", onPageCalls)
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
	if binPath == "" {
		t.Skip("snmp_responder binary not found or build skipped; skipping synthetic odometer test")
	}

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

	// Place metadata file outside t.TempDir() to preserve evidence on deletion failure
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

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
		out, err := runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
		if err != nil {
			t.Errorf("cleanup failure: remove-cups failed for queue %q: %v (%s). Metadata preserved at %s.",
				queueName, err, strings.TrimSpace(out), metaFile)
		} else {
			_ = os.Remove(metaFile)
		}
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

// TestCUPSQueueDeletion_FailurePreservesOwnershipMetadata verifies that if verified CUPS queue
// deletion fails (or is refused due to URI mismatch), ownership metadata is strictly preserved
// and not deleted, preventing orphaned unmanaged queues.
func TestCUPSQueueDeletion_FailurePreservesOwnershipMetadata(t *testing.T) {
	checkCUPSAndIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()

	ippPort := getFreePort(t, "tcp")
	emuName := fmt.Sprintf("XeroxGoPreserveEmu_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	queueName := fmt.Sprintf("XeroxGoPreserve_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)

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
		// Guaranteed final cleanup with matching URI
		_, _ = runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
		_ = os.Remove(metaFile)
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup")
	})

	// Setup driverless CUPS queue
	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		t.Fatalf("failed to setup CUPS queue: %v (%s)", err, out)
	}

	// Verify metadata file was created
	if _, err := os.Stat(metaFile); err != nil {
		t.Fatalf("expected metadata file %s to exist after setup-cups", metaFile)
	}

	// Corrupt metadata URI to point to an unrelated printer, simulating ownership mismatch
	originalMeta, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}
	corruptedMeta := strings.Replace(string(originalMeta), fmt.Sprintf(":%d/", ippPort), ":9999/", 1)
	if err := os.WriteFile(metaFile, []byte(corruptedMeta), 0644); err != nil {
		t.Fatalf("failed to corrupt metadata: %v", err)
	}

	// Attempt remove-cups: must fail due to 3-way URI mismatch
	removeOut, removeErr := runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
	if removeErr == nil {
		t.Errorf("expected remove-cups to fail on corrupted metadata URI, but succeeded: %s", removeOut)
	}

	// Requirement 2: Verify ownership metadata was NOT deleted and is still present
	if _, err := os.Stat(metaFile); os.IsNotExist(err) {
		t.Errorf("ownership metadata %s was deleted despite failed queue removal", metaFile)
	}

	// Verify queue was preserved in CUPS
	if lpstatOut, err := exec.Command("lpstat", "-p", queueName).CombinedOutput(); err != nil {
		t.Errorf("expected queue %s to still exist in CUPS after failed removal, got error: %v (%s)",
			queueName, err, strings.TrimSpace(string(lpstatOut)))
	}

	// Restore original valid metadata
	if err := os.WriteFile(metaFile, originalMeta, 0644); err != nil {
		t.Fatalf("failed to restore original metadata: %v", err)
	}

	// Verified removal should now succeed
	if cleanOut, cleanErr := runEmu(t, emuScript, emuEnv, "remove-cups", queueName); cleanErr != nil {
		t.Fatalf("remove-cups failed after restoring valid metadata: %v (%s)", cleanErr, cleanOut)
	}

	// Verify metadata file is deleted after successful removal
	if _, err := os.Stat(metaFile); !os.IsNotExist(err) {
		t.Errorf("expected metadata file %s to be deleted after successful remove-cups", metaFile)
	}
}
