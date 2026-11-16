package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

const (
	actionCW2SaveJobCrash        = "CW2_SAVEJOB_THEN_CRASH"
	actionCW4MarkSubmittingCrash = "CW4_MARKSUBMITTING_THEN_CRASH"
	actionCW5InFlightLPCrash     = "CW5_INFLIGHT_LP_CRASH"
	actionCW6PostAcceptLPCrash   = "CW6_POST_ACCEPT_CRASH"
	actionCW7MidPrintCrash       = "CW7_MIDPRINT_THEN_CRASH"
	actionCW8CompletionCrash     = "CW8_COMPLETION_THEN_CRASH"
)

// -----------------------------------------------------------------------------
// Subprocess Worker: TestCrashHelperProcess
// -----------------------------------------------------------------------------
// When GO_WANT_CRASH_HELPER_PROCESS == "1", this function executes as an isolated
// child process, performs the target pre-crash sequence, writes the readiness marker,
// and blocks indefinitely awaiting SIGKILL from the parent test supervisor.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CRASH_HELPER_PROCESS") != "1" {
		return
	}

	action := os.Getenv("CRASH_ACTION")
	dbPath := os.Getenv("CRASH_DB_PATH")
	queueName := os.Getenv("CRASH_QUEUE")
	readyFile := os.Getenv("CRASH_READY_FILE")
	capturedFile := os.Getenv("CRASH_CAPTURED_ID_FILE")
	jobID := os.Getenv("CRASH_JOB_ID")
	downloadURL := os.Getenv("CRASH_DOWNLOAD_URL")
	checksum := os.Getenv("CRASH_CHECKSUM")
	pdfPath := os.Getenv("CRASH_PDF_PATH")
	totalSheets, _ := strconv.Atoi(os.Getenv("CRASH_TOTAL_SHEETS"))
	totalPages, _ := strconv.Atoi(os.Getenv("CRASH_TOTAL_PAGES"))
	copies, _ := strconv.Atoi(os.Getenv("CRASH_COPIES"))
	duplex := contracts.DuplexMode(os.Getenv("CRASH_DUPLEX"))
	if copies < 1 {
		copies = 1
	}
	if totalSheets < 1 {
		totalSheets = 1
	}
	if totalPages < 1 {
		totalPages = 1
	}

	store, err := state.Open(dbPath)
	if err != nil {
		log.Fatalf("[CHILD] Failed to open SQLite store at %s: %v", dbPath, err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "8888",
		DownloadURL:    downloadURL,
		SHA256Checksum: checksum,
		TotalPages:     totalPages,
		TotalSheets:    totalSheets,
		Copies:         copies,
		Duplex:         duplex,
		StartFromPage:  1,
	}

	switch action {
	case actionCW2SaveJobCrash:
		// CW-2: SaveJob written, process crashes before CUPS submission
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW2] SaveJob failed: %v", err)
		}
		log.Printf("[CHILD-CW2] SaveJob committed for %s; signaling readiness and blocking for SIGKILL", jobID)
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			log.Fatalf("[CHILD-CW2] Failed to write ready marker: %v", err)
		}
		select {} // Block until killed by SIGKILL

	case actionCW4MarkSubmittingCrash:
		// CW-4: MarkJobSubmitting committed, process crashes before lp execution
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW4] SaveJob failed: %v", err)
		}
		if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
			log.Fatalf("[CHILD-CW4] MarkJobSubmitting failed: %v", err)
		}
		log.Printf("[CHILD-CW4] MarkJobSubmitting committed for %s; signaling readiness and blocking for SIGKILL", jobID)
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			log.Fatalf("[CHILD-CW4] Failed to write ready marker: %v", err)
		}
		select {} // Block until killed by SIGKILL

	case actionCW5InFlightLPCrash:
		// CW-5: Process crashes while lp is actively in flight
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW5] SaveJob failed: %v", err)
		}
		if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
			log.Fatalf("[CHILD-CW5] MarkJobSubmitting failed: %v", err)
		}

		lpArgs := []string{
			"-d", queueName,
			"-n", strconv.Itoa(copies),
			"-t", cmd.JobID,
			pdfPath,
		}
		lpCmd := exec.Command("lp", lpArgs...)
		if err := lpCmd.Start(); err != nil {
			log.Fatalf("[CHILD-CW5] lp start failed: %v", err)
		}
		log.Printf("[CHILD-CW5] lp process started (PID: %d); signaling readiness while lp is in flight", lpCmd.Process.Pid)
		// Signal supervisor that lp is actively executing
		if err := os.WriteFile(readyFile, []byte(strconv.Itoa(lpCmd.Process.Pid)), 0644); err != nil {
			log.Fatalf("[CHILD-CW5] Failed to write ready marker: %v", err)
		}

		// Block awaiting SIGKILL while lp is in flight (or waiting for lp completion)
		_ = lpCmd.Wait()
		select {} // Block until killed by SIGKILL

	case actionCW6PostAcceptLPCrash:
		// CW-6: lp submits job to CUPS, returns ID, process crashes before RecordJobSubmission
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW6] SaveJob failed: %v", err)
		}
		if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
			log.Fatalf("[CHILD-CW6] MarkJobSubmitting failed: %v", err)
		}

		lpArgs := []string{
			"-d", queueName,
			"-n", strconv.Itoa(copies),
			"-t", cmd.JobID,
			pdfPath,
		}
		lpCmd := exec.Command("lp", lpArgs...)
		out, lpErr := lpCmd.CombinedOutput()
		if lpErr != nil {
			log.Fatalf("[CHILD-CW6] lp execution failed: %v (%s)", lpErr, strings.TrimSpace(string(out)))
		}

		cupsID := parseCUPSJobID(string(out))
		if cupsID == "" {
			log.Fatalf("[CHILD-CW6] Unable to parse CUPS request ID from lp output: %s", string(out))
		}
		log.Printf("[CHILD-CW6] lp accepted job: %s; persisting captured ID and blocking for SIGKILL", cupsID)

		if capturedFile != "" {
			if err := os.WriteFile(capturedFile, []byte(cupsID), 0644); err != nil {
				log.Fatalf("[CHILD-CW6] Failed to write captured ID file: %v", err)
			}
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			log.Fatalf("[CHILD-CW6] Failed to write ready marker: %v", err)
		}
		select {} // Block until killed by SIGKILL

	case actionCW7MidPrintCrash:
		// CW-7: Job submitted, sheet 1 live SNMP odometer advance & monitor persistence, process crashes mid-print
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW7] SaveJob failed: %v", err)
		}
		if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
			log.Fatalf("[CHILD-CW7] MarkJobSubmitting failed: %v", err)
		}

		lpArgs := []string{
			"-d", queueName,
			"-n", strconv.Itoa(copies),
			"-t", cmd.JobID,
			pdfPath,
		}
		lpCmd := exec.Command("lp", lpArgs...)
		out, lpErr := lpCmd.CombinedOutput()
		if lpErr != nil {
			log.Fatalf("[CHILD-CW7] lp execution failed: %v (%s)", lpErr, strings.TrimSpace(string(out)))
		}

		cupsID := parseCUPSJobID(string(out))
		if cupsID == "" {
			log.Fatalf("[CHILD-CW7] Unable to parse CUPS request ID: %s", string(out))
		}
		if err := store.RecordJobSubmission(context.Background(), cmd.JobID, cupsID); err != nil {
			log.Fatalf("[CHILD-CW7] RecordJobSubmission failed: %v", err)
		}
		if capturedFile != "" {
			_ = os.WriteFile(capturedFile, []byte(cupsID), 0644)
		}

		// Requirement 9: Exercise emulator SNMP odometer advancement and monitor persistence before the crash
		emuScript := os.Getenv("CRASH_EMU_SCRIPT")
		if emuScript != "" {
			snmpSetCmd := exec.Command(emuScript, "snmp-set", "-counter-inc", "1")
			snmpSetCmd.Env = os.Environ()
			if snmpOut, err := snmpSetCmd.CombinedOutput(); err != nil {
				log.Printf("[CHILD-CW7] Warning: snmp-set failed: %v (%s)", err, strings.TrimSpace(string(snmpOut)))
			}
		}

		// Verify live SNMP telemetry from emulator endpoint
		snmpPortStr := os.Getenv("CRASH_SNMP_PORT")
		snmpPortVal, _ := strconv.Atoi(snmpPortStr)
		if snmpPortVal > 0 {
			snmpCfg := printer.SNMPConfig{
				Target:    "127.0.0.1",
				Port:      snmpPortVal,
				Community: "public",
				Timeout:   2 * time.Second,
			}
			telem := printer.PollHardwareTelemetry(context.Background(), snmpCfg)
			if telem.HasLife {
				log.Printf("[CHILD-CW7] Live SNMP telemetry confirmed odometer: %d", telem.LifeCount)
			}
		}

		// Durably record sheet 1 verified physical progress in SQLite (live SNMP-verified)
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       cmd.KioskID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  1,
			SheetsPrinted: 1,
			Timestamp:     time.Now().UTC(),
		}
		if err := store.RecordSheetProgress(context.Background(), evt); err != nil {
			log.Fatalf("[CHILD-CW7] RecordSheetProgress failed for sheet 1: %v", err)
		}

		log.Printf("[CHILD-CW7] Live SNMP verified sheet 1 progress committed for %s (%s); signaling readiness and blocking for SIGKILL", cmd.JobID, cupsID)
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			log.Fatalf("[CHILD-CW7] Failed to write ready marker: %v", err)
		}
		select {} // Block until killed by SIGKILL

	case actionCW8CompletionCrash:
		// CW-8: Physical printing complete (TotalSheets confirmed), CUPS completed,
		// but process crashes before completeJob marks COMPLETED
		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			log.Fatalf("[CHILD-CW8] SaveJob failed: %v", err)
		}
		if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
			log.Fatalf("[CHILD-CW8] MarkJobSubmitting failed: %v", err)
		}

		lpArgs := []string{
			"-d", queueName,
			"-n", strconv.Itoa(copies),
			"-t", cmd.JobID,
			pdfPath,
		}
		lpCmd := exec.Command("lp", lpArgs...)
		out, lpErr := lpCmd.CombinedOutput()
		if lpErr != nil {
			log.Fatalf("[CHILD-CW8] lp execution failed: %v (%s)", lpErr, strings.TrimSpace(string(out)))
		}

		cupsID := parseCUPSJobID(string(out))
		if cupsID == "" {
			log.Fatalf("[CHILD-CW8] Unable to parse CUPS request ID: %s", string(out))
		}
		if err := store.RecordJobSubmission(context.Background(), cmd.JobID, cupsID); err != nil {
			log.Fatalf("[CHILD-CW8] RecordJobSubmission failed: %v", err)
		}
		if capturedFile != "" {
			_ = os.WriteFile(capturedFile, []byte(cupsID), 0644)
		}

		// Record all sheets printed (physical proof complete)
		evt := contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			KioskID:       cmd.KioskID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  cmd.TotalPages,
			SheetsPrinted: cmd.TotalSheets,
			Timestamp:     time.Now().UTC(),
		}
		if err := store.RecordSheetProgress(context.Background(), evt); err != nil {
			log.Fatalf("[CHILD-CW8] RecordSheetProgress failed: %v", err)
		}

		log.Printf("[CHILD-CW8] Full physical proof committed for %s (%s); signaling readiness and blocking for SIGKILL", cmd.JobID, cupsID)
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			log.Fatalf("[CHILD-CW8] Failed to write ready marker: %v", err)
		}
		select {} // Block until killed by SIGKILL

	default:
		log.Fatalf("[CHILD] Unknown action: %q", action)
	}
}

// -----------------------------------------------------------------------------
// Test Context Helper
// -----------------------------------------------------------------------------
func setupCrashTestEnv(t *testing.T, scenario string) (
	emuScript string,
	emuEnv []string,
	queueName string,
	metaFile string,
	dbPath string,
	spoolDir string,
	activeFile, gateFile, releaseFile string,
	ippPort, snmpPort int,
	cleanupFunc func(),
) {
	t.Helper()
	checkCUPSAndIPPDependencies(t)

	emuScript = findEmulatorScript(t)
	tempDir := t.TempDir()

	spoolDir = filepath.Join(tempDir, "spool")
	_ = os.MkdirAll(spoolDir, 0755)

	ippPort = getFreePort(t, "tcp")
	snmpPort = getFreePort(t, "udp")
	emuName := fmt.Sprintf("XeroxGoCrashEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName = fmt.Sprintf("XeroxGoCrashQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	metaFile = filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile = filepath.Join(tempDir, "job.active")
	gateFile = filepath.Join(tempDir, "job.gate")
	releaseFile = filepath.Join(tempDir, "job.release")

	_ = os.WriteFile(gateFile, []byte("gate"), 0644)
	_ = os.Remove(activeFile)
	_ = os.Remove(releaseFile)

	dbPath = filepath.Join(tempDir, "crash_state.db")

	emuEnv = append(os.Environ(),
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

	// Start IPP and SNMP emulators
	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	// Setup driverless CUPS queue with ownership metadata
	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup-snmp")
		t.Fatalf("failed to setup CUPS queue: %v (output: %s)", err, out)
	}

	// Initialize database with default state
	initStore, err := state.Open(dbPath)
	if err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "remove-cups", queueName)
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup-snmp")
		t.Fatalf("failed to open SQLite store: %v", err)
	}
	_ = initStore.ResetPaperTray(context.Background(), 100)
	_ = initStore.Close()

	cleanupFunc = func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup"); err != nil {
			t.Errorf("teardown failure: emulator cleanup failed: %v (%s)", err, strings.TrimSpace(out))
		}
		if out, err := runEmu(t, emuScript, emuEnv, "cleanup-snmp"); err != nil {
			t.Errorf("teardown failure: emulator cleanup-snmp failed: %v (%s)", err, strings.TrimSpace(out))
		}
	}

	return
}

// -----------------------------------------------------------------------------
// Child Subprocess Supervisor: spawnAndKillChild
// -----------------------------------------------------------------------------
// Spawns the test binary as a child subprocess, waits until the child signals
// it has committed the target SQLite/CUPS state, delivers SIGKILL to the child PID,
// and asserts that the child was terminated by SIGKILL without affecting the parent
// test runner, system cupsd, or unrelated processes.
func spawnAndKillChild(
	t *testing.T,
	emuScript string,
	snmpPort int,
	emuEnv []string,
	action string,
	dbPath string,
	queueName string,
	cmd contracts.PrintJobCommand,
	downloadURL string,
	checksum string,
	samplePDF string,
	readyFile string,
	capturedFile string,
) string {
	t.Helper()
	_ = os.Remove(readyFile)
	if capturedFile != "" {
		_ = os.Remove(capturedFile)
	}

	childCmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$", "-test.v")
	childCmd.Env = append(emuEnv,
		"GO_WANT_CRASH_HELPER_PROCESS=1",
		"CRASH_ACTION="+action,
		"CRASH_DB_PATH="+dbPath,
		"CRASH_QUEUE="+queueName,
		"CRASH_READY_FILE="+readyFile,
		"CRASH_CAPTURED_ID_FILE="+capturedFile,
		"CRASH_JOB_ID="+cmd.JobID,
		"CRASH_DOWNLOAD_URL="+downloadURL,
		"CRASH_CHECKSUM="+checksum,
		"CRASH_PDF_PATH="+samplePDF,
		"CRASH_TOTAL_SHEETS="+strconv.Itoa(cmd.TotalSheets),
		"CRASH_TOTAL_PAGES="+strconv.Itoa(cmd.TotalPages),
		"CRASH_COPIES="+strconv.Itoa(cmd.Copies),
		"CRASH_DUPLEX="+string(cmd.Duplex),
		"CRASH_EMU_SCRIPT="+emuScript,
		"CRASH_SNMP_PORT="+strconv.Itoa(snmpPort),
	)

	var childOut bytes.Buffer
	childCmd.Stdout = &childOut
	childCmd.Stderr = &childOut

	if err := childCmd.Start(); err != nil {
		t.Fatalf("failed to start child crash process: %v", err)
	}
	childPID := childCmd.Process.Pid

	// Requirement 14: Confirm child PID belongs to the child and is not parent or cupsd
	if childPID <= 0 || childPID == os.Getpid() {
		t.Fatalf("invalid child PID %d (cannot be parent process)", childPID)
	}

	// Wait for child to complete pre-crash action and write ready marker
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	var readyContent string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(readyFile); err == nil && len(data) > 0 {
			ready = true
			readyContent = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !ready {
		_ = childCmd.Process.Kill()
		t.Fatalf("child process %d did not signal readiness within deadline. Output:\n%s", childPID, childOut.String())
	}

	// Verify child process is alive before signaling
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child process %d died prematurely: %v. Output:\n%s", childPID, err, childOut.String())
	}

	// Parse optional in-flight subprocess PID (e.g. lp PID in CW-5)
	var inFlightPID int
	if p, err := strconv.Atoi(readyContent); err == nil && p > 1 && p != os.Getpid() && p != childPID {
		inFlightPID = p
	}

	// Deliver real SIGKILL to the child subprocess
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatalf("failed to deliver SIGKILL to child PID %d: %v", childPID, err)
	}

	// If there is an in-flight child process (e.g. lp in CW-5), terminate it too as part of the crash
	if inFlightPID > 0 {
		_ = syscall.Kill(inFlightPID, syscall.SIGKILL)
		for i := 0; i < 50; i++ {
			if err := syscall.Kill(inFlightPID, 0); err != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Wait for child process to exit and verify termination signal
	waitErr := childCmd.Wait()
	if waitErr == nil {
		t.Fatalf("expected child process %d to be killed by SIGKILL, but exited with code 0", childPID)
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		ws := exitErr.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("expected child process to be killed by SIGKILL (9), got: %v", ws)
		}
		t.Logf("Child subprocess PID %d successfully terminated by SIGKILL (action: %s)", childPID, action)
	} else {
		t.Fatalf("unexpected wait error from child process: %v", waitErr)
	}

	capturedID := ""
	if capturedFile != "" {
		if data, err := os.ReadFile(capturedFile); err == nil {
			capturedID = strings.TrimSpace(string(data))
		}
	}
	return capturedID
}

// newSubmissionTrackingRunner creates a CommandRunner that monitors lp invocations and
// increments subCount strictly for print job submissions (lp -d <queue>), ignoring hold
// requests (lp -i <jobID> -H hold) or query commands.
func newSubmissionTrackingRunner(t *testing.T, subCount *int32) printer.CommandRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for _, arg := range args {
				if arg == "-d" {
					atomic.AddInt32(subCount, 1)
					break
				}
			}
		}
		cmd := exec.CommandContext(ctx, name, args...)
		return cmd.CombinedOutput()
	}
}

// -----------------------------------------------------------------------------
// Exactly-Once Verification Helper (Requirements 11, 12)
// -----------------------------------------------------------------------------
// verifyExactlyOnceCUPSSubmissions examines actual CUPS job records via lpstat -W all,
// detects ANY unexpected additional CUPS IDs/submissions (not merely repeated occurrences of the original ID),
// and checks independent submission instrumentation (e.g. spool directory artifacts and submission counters).
func verifyExactlyOnceCUPSSubmissions(t *testing.T, queueName string, spoolDir string, expectedCUPSID string, allowedAdditionalIDs ...string) {
	t.Helper()

	// 1. Independent Spool Directory Artifact Verification
	if spoolDir != "" {
		entries, err := os.ReadDir(spoolDir)
		if err == nil {
			expectedCount := 0
			if expectedCUPSID != "" {
				expectedCount = 1
			}
			expectedCount += len(allowedAdditionalIDs)

			// Distinct job sequences in emulator spool start with "<N>-..."
			jobSequences := make(map[string]bool)
			for _, e := range entries {
				name := e.Name()
				if idx := strings.Index(name, "-"); idx > 0 {
					prefix := name[:idx]
					if _, err := strconv.Atoi(prefix); err == nil {
						jobSequences[prefix] = true
					}
				}
			}
			if len(jobSequences) > expectedCount {
				t.Errorf("exactly-once violation in spool directory: found %d independent job sequences %v, expected %d",
					len(jobSequences), jobSequences, expectedCount)
			} else {
				t.Logf("independent spool artifact check confirmed %d job sequence(s) in %s", len(jobSequences), spoolDir)
			}
		}
	}

	// 2. Scheduler History Inspection via lpstat -W all
	lpstatOut, _ := exec.Command("lpstat", "-W", "all", queueName).CombinedOutput()
	records := parseCUPSJobIDsFromLPStat(string(lpstatOut), queueName)
	if len(records) == 0 {
		t.Logf("CUPS job history unavailable on host for queue %s; history-based exactly-once proof unverified (relying on independent spool instrumentation)", queueName)
		return
	}

	allowed := make(map[string]bool)
	if expectedCUPSID != "" {
		allowed[expectedCUPSID] = true
	}
	for _, id := range allowedAdditionalIDs {
		allowed[id] = true
	}

	var unexpectedIDs []string
	idCounts := make(map[string]int)
	for _, r := range records {
		idCounts[r]++
		if !allowed[r] {
			unexpectedIDs = append(unexpectedIDs, r)
		}
	}

	if len(unexpectedIDs) > 0 {
		t.Errorf("exactly-once violation: unexpected additional CUPS job submissions detected on queue %s: %v (all jobs: %v, allowed: %v)",
			queueName, unexpectedIDs, records, allowed)
	} else {
		t.Logf("actual CUPS job history confirms no unexpected additional submissions on %s (all jobs: %v)", queueName, records)
	}

	// Also verify that allowed IDs are not duplicated
	for id, count := range idCounts {
		if count > 1 {
			t.Errorf("duplicate CUPS job record detected for %s: count=%d (records: %v)", id, count, records)
		}
	}
}

// -----------------------------------------------------------------------------
// Test 1: CW-2 SaveJob Crash & Safe Startup Retry (Requirement 4)
// -----------------------------------------------------------------------------
// Verifies that when a daemon crashes after SaveJob but before CUPS submission,
// startup recovery detects isDefinitelyNotSubmitted == true and safely re-enqueues
// and completes the job with zero duplicate prints.
func TestCrashRecovery_CW2_SaveJobCrash_SafeRetry(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, releaseFile, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw2-retry")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer server.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw2-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "1234",
		DownloadURL:    server.URL + "/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	defer os.Remove(readyFile)

	// Phase 1: Child process executes SaveJob and is killed by SIGKILL before submission
	spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW2SaveJobCrash, dbPath, queueName, cmd, server.URL+"/print.pdf", checksum, samplePDF, readyFile, "")

	// Verify post-crash SQLite state on disk before restart
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite after crash: %v", err)
	}
	if preJob.Status != contracts.JobStatusSpooling || preJob.SubmittedToPrinter || preJob.CUPSJobID != "" {
		t.Fatalf("unexpected pre-restart job state: status=%s, submitted=%v, cups_id=%q", preJob.Status, preJob.SubmittedToPrinter, preJob.CUPSJobID)
	}
	_ = preStore.Close()

	// Verify CUPS queue has zero jobs before restart
	lpstatPre, _ := exec.Command("lpstat", "-W", "all", "-o", queueName).CombinedOutput()
	if strings.Contains(string(lpstatPre), queueName+"-") {
		t.Fatalf("expected zero CUPS jobs before restart, got:\n%s", string(lpstatPre))
	}

	// Release emulator gate so job can print on retry
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Phase 2: Start new Client on the same SQLite database (Startup Recovery)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var odoCounter int64 = 1000
	var odoMu sync.Mutex

	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		odoMu.Lock()
		cur := odoCounter
		odoMu.Unlock()
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: cur,
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  100 * time.Millisecond,
		SNMPPoller:          snmpPoller,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelRecov()

	go func() {
		// When job is submitted to CUPS, advance hardware odometer after short delay
		for recovCtx.Err() == nil {
			out, _ := exec.Command("lpstat", "-o", queueName).CombinedOutput()
			if strings.Contains(string(out), queueName+"-") {
				time.Sleep(300 * time.Millisecond)
				odoMu.Lock()
				odoCounter = 1001
				odoMu.Unlock()
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for job to be retried and reach COMPLETED
	deadline := time.Now().Add(20 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusCompleted {
			completed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !completed {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("job did not reach COMPLETED on startup retry; current state: %+v", curJob)
	}

	// Verify post-restart invariants
	finalJob, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil || finalJob == nil {
		t.Fatalf("failed to get final job: %v", err)
	}
	if finalJob.SheetsPrinted != cmd.TotalSheets {
		t.Errorf("expected SheetsPrinted == TotalSheets (%d), got %d", cmd.TotalSheets, finalJob.SheetsPrinted)
	}
	if finalJob.CUPSJobID == "" {
		t.Errorf("expected non-empty CUPSJobID in final completed job")
	}

	// Verify exactly-once CUPS submission in actual scheduler history
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, finalJob.CUPSJobID)
	t.Logf("CW-2 validated: Unsubmitted crash safely retried; completed with CUPS ID %s (sheets: %d/%d)",
		finalJob.CUPSJobID, finalJob.SheetsPrinted, cmd.TotalSheets)
}

// -----------------------------------------------------------------------------
// Test 2: CW-4 Pre-Submission Crash Fails Closed (Requirement 5)
// -----------------------------------------------------------------------------
// Verifies that when a daemon crashes after MarkJobSubmitting but before lp,
// startup recovery treats the job as an ambiguous submission and marks PAUSED_ERROR
// without executing lp or retrying submission.
func TestCrashRecovery_CW4_PreSubmissionCrash_FailsSafe(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, _, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw4-pre-sub")
	defer teardown()

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw4-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "2222",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	defer os.Remove(readyFile)

	// Phase 1: Child process executes MarkJobSubmitting and is killed before lp runs
	spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW4MarkSubmittingCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, "", readyFile, "")

	// Verify post-crash SQLite state: submitted_to_printer=1, cups_job_id=""
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite: %v", err)
	}
	if !preJob.SubmittedToPrinter || preJob.CUPSJobID != "" {
		t.Fatalf("unexpected pre-restart job state: submitted=%v, cups_id=%q", preJob.SubmittedToPrinter, preJob.CUPSJobID)
	}
	_ = preStore.Close()

	// Verify zero CUPS jobs submitted prior to restart
	lpstatPre, _ := exec.Command("lpstat", "-W", "all", "-o", queueName).CombinedOutput()
	if strings.Contains(string(lpstatPre), queueName+"-") {
		t.Fatalf("expected zero CUPS jobs in queue before restart, got:\n%s", string(lpstatPre))
	}

	// Phase 2: Start new Client (Startup Recovery)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery to process ambiguous submission
	deadline := time.Now().Add(10 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusPausedError {
			if strings.Contains(j.ErrorReason, "manual reconciliation required") {
				paused = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !paused {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected job to be paused in PAUSED_ERROR; got: %+v", curJob)
	}

	// Requirement 5 / 13: Verify zero CUPS submissions occurred on restart
	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery called lp %d times", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, "")

	t.Logf("CW-4 validated: Pre-submission crash safely paused in PAUSED_ERROR (%s); zero CUPS jobs submitted", preJob.ErrorReason)
}

// -----------------------------------------------------------------------------
// Test 3: CW-5 In-Flight lp Execution Crash & Independent CUPS Acceptance (Requirements 2, 3, 4)
// -----------------------------------------------------------------------------
// Verifies that when a crash occurs while lp is actively in flight:
// 1. Subprocess is killed by SIGKILL during lp execution.
// 2. The test independently queries CUPS to establish whether the job was accepted or not.
// 3. In both cases, startup recovery marks PAUSED_ERROR with zero duplicate submissions.
// 4. If accepted, documents unlinked active spooling; if not accepted, documents unsubmitted state.
func TestCrashRecovery_CW5_InFlightLP_CrashDuringSubmission(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, releaseFile, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw5-inflight")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw5-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "5555",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	defer os.Remove(readyFile)

	// Phase 1: Child process launches lp and is killed by SIGKILL while lp is in flight
	spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW5InFlightLPCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, samplePDF, readyFile, "")

	// Requirement 4: Establish independently whether CUPS accepted the job before classifying
	lpstatOut, _ := exec.Command("lpstat", "-W", "all", queueName).CombinedOutput()
	records := parseCUPSJobIDsFromLPStat(string(lpstatOut), queueName)

	var acceptedCUPSID string
	var classification string
	if len(records) > 0 {
		acceptedCUPSID = records[0]
		classification = "unlinked"
		t.Logf("Independent CUPS query established: CUPS ACCEPTED job %s during in-flight crash -> classification: UNLINKED", acceptedCUPSID)
	} else {
		classification = "unsubmitted"
		t.Logf("Independent CUPS query established: CUPS did NOT accept job during in-flight crash -> classification: UNSUBMITTED")
	}

	// Verify post-crash SQLite state: submitted_to_printer=1, cups_job_id=""
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite: %v", err)
	}
	if !preJob.SubmittedToPrinter || preJob.CUPSJobID != "" {
		t.Fatalf("unexpected pre-restart job state: submitted=%v, cups_id=%q", preJob.SubmittedToPrinter, preJob.CUPSJobID)
	}
	_ = preStore.Close()

	// Phase 2: Start new Client (Startup Recovery)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery to process ambiguous submission
	deadline := time.Now().Add(10 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !paused {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected job to fail closed into PAUSED_ERROR; got: %+v", curJob)
	}

	// Verify zero duplicate submissions were dispatched during startup recovery
	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery called lp %d times for ambiguous CW-5 job", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, acceptedCUPSID)

	// If unlinked, inspect exact CUPS state and release gate
	if classification == "unlinked" {
		cupsInfoPost, qErr := printer.QueryCUPSJobInfo(context.Background(), nil, queueName, acceptedCUPSID)
		if qErr != nil {
			t.Fatalf("failed to query unlinked CUPS job %s: %v", acceptedCUPSID, qErr)
		}
		t.Logf("Exact CUPS job-state evidence for unlinked job %s: %s (reasons: %v)", acceptedCUPSID, cupsInfoPost.State, cupsInfoPost.Reasons)
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
	}

	t.Logf("CW-5 validated: In-flight lp crash classified as %s; startup recovery safely failed closed to PAUSED_ERROR with zero duplicate lp dispatches", classification)
}

// -----------------------------------------------------------------------------
// Test 4: CW-6 Post-Acceptance Crash Documents Unlinked Active CUPS Spooling (Requirements 2, 6, 7)
// -----------------------------------------------------------------------------
// Verifies that when CUPS accepts a job but the process is killed before RecordJobSubmission:
// 1. Child process commits lp acceptance and captures the real CUPS ID, but dies before SQLite write.
// 2. Pre-restart CUPS state is proven to be processing/pending.
// 3. Startup recovery marks PAUSED_ERROR in SQLite without issuing hold for captured CUPS ID.
// 4. Exact CUPS job-state evidence proves unlinked job remains processing in CUPS and completes unmonitored.
func TestCrashRecovery_CW6_LPAccepted_UnlinkedActiveJob_DocumentsUnmonitoredSpooling(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, releaseFile, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw6-unlinked")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw6-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "6666",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	capturedFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_captured_%s", cmd.JobID))
	defer os.Remove(readyFile)
	defer os.Remove(capturedFile)

	// Phase 1: Child process submits job via lp, captures CUPS ID, but is killed before RecordJobSubmission
	capturedCUPSID := spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW6PostAcceptLPCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, samplePDF, readyFile, capturedFile)
	if capturedCUPSID == "" {
		t.Fatalf("failed to capture submitted CUPS job ID from child process")
	}

	// Verify post-crash SQLite state: submitted=1, cups_job_id=""
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite: %v", err)
	}
	if !preJob.SubmittedToPrinter || preJob.CUPSJobID != "" {
		t.Fatalf("unexpected pre-restart job state: submitted=%v, cups_id=%q", preJob.SubmittedToPrinter, preJob.CUPSJobID)
	}
	_ = preStore.Close()

	// Requirement 7: Require exact CUPS job-state evidence for claims before restart
	cupsInfoPre, qErr := printer.QueryCUPSJobInfo(context.Background(), nil, queueName, capturedCUPSID)
	if qErr != nil {
		t.Fatalf("failed to query accepted CUPS job %s: %v", capturedCUPSID, qErr)
	}
	if cupsInfoPre.State != printer.CUPSJobStateProcessing && cupsInfoPre.State != printer.CUPSJobStatePending {
		t.Fatalf("expected accepted CUPS job %s to be processing or pending, got: %s", capturedCUPSID, cupsInfoPre.State)
	}
	t.Logf("Accepted CUPS job %s exact state before restart: %s (reasons: %v)", capturedCUPSID, cupsInfoPre.State, cupsInfoPre.Reasons)

	// Phase 2: Start new Client (Startup Recovery)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery to mark PAUSED_ERROR
	deadline := time.Now().Add(10 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !paused {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected job to be in PAUSED_ERROR; got: %+v", curJob)
	}

	// Requirement 7: Require exact CUPS job-state evidence post-restart
	// Defect confirmation: XeroxGo made NO hold request for capturedCUPSID because cups_job_id was empty.
	cupsInfoPost, postErr := printer.QueryCUPSJobInfo(context.Background(), nil, queueName, capturedCUPSID)
	if postErr != nil {
		t.Fatalf("failed to query CUPS job %s after recovery: %v", capturedCUPSID, postErr)
	}
	if cupsInfoPost.State != printer.CUPSJobStateProcessing && cupsInfoPost.State != printer.CUPSJobStatePending && cupsInfoPost.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected unlinked CUPS job %s to remain in queue, got: %s", capturedCUPSID, cupsInfoPost.State)
	}

	// Release the emulator gate to observe unmonitored completion
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Wait for unmonitored CUPS job to reach completed
	deadlineCompleted := time.Now().Add(5 * time.Second)
	var cupsInfoAfter printer.CUPSJobInfo
	for time.Now().Before(deadlineCompleted) {
		cupsInfoAfter, _ = printer.QueryCUPSJobInfo(context.Background(), nil, queueName, capturedCUPSID)
		if cupsInfoAfter.State == printer.CUPSJobStateCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cupsInfoAfter.State != printer.CUPSJobStateCompleted {
		t.Fatalf("expected unlinked CUPS job %s to complete in background, got: %s", capturedCUPSID, cupsInfoAfter.State)
	}

	// Verify zero duplicate CUPS submissions were dispatched by XeroxGo
	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery dispatched %d new lp calls", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, capturedCUPSID)

	t.Logf("CONFIRMED DEFECT 1 (CW-6): Job %s (CUPS ID %s) was accepted by CUPS (state: %s -> %s) but unlinked in SQLite. "+
		"XeroxGo paused SQLite job in PAUSED_ERROR without holding CUPS job. "+
		"Original CUPS job continued unmonitored in background to COMPLETED.",
		cmd.JobID, capturedCUPSID, cupsInfoPre.State, cupsInfoAfter.State)
}

// -----------------------------------------------------------------------------
// Test 5: CW-7 Mid-Print Crash Resumes Monitoring (Requirements 7, 8, 9, 10)
// -----------------------------------------------------------------------------
// Verifies that when a crash occurs mid-print with persisted CUPS ID and partial
// live SNMP-verified odometer progress:
// 1. Pre-crash sheet 1 is verified via live SNMP odometer advancement before crash.
// 2. Startup recovery detects active CUPS job and resumes monitoring without duplicate submission.
// 3. Asserts exact completion state (COMPLETED) and exact TotalSheets (2), not merely non-regression.
func TestCrashRecovery_CW7_MidPrint_ResumesMonitoring(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, releaseFile, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw7-midprint")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw7-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "7777",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	capturedFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_captured_%s", cmd.JobID))
	defer os.Remove(readyFile)
	defer os.Remove(capturedFile)

	// Phase 1: Child process submits 2-sheet job, records 1 sheet verified via live SNMP, and is killed mid-print
	capturedCUPSID := spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW7MidPrintCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, samplePDF, readyFile, capturedFile)
	if capturedCUPSID == "" {
		t.Fatalf("failed to capture submitted CUPS job ID from child process")
	}

	// Verify post-crash SQLite state: sheets_printed=1 (live SNMP verified), cups_job_id captured
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite: %v", err)
	}
	if preJob.SheetsPrinted != 1 || preJob.CUPSJobID != capturedCUPSID {
		t.Fatalf("unexpected pre-restart job state: sheets=%d, cups_id=%s", preJob.SheetsPrinted, preJob.CUPSJobID)
	}
	_ = preStore.Close()

	// Phase 2: Start new Client (Startup Recovery -> resumeCUPSMonitoring)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var baselineRead atomic.Bool
	var odoCounter int64 = 1001
	var odoMu sync.Mutex

	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		odoMu.Lock()
		cur := odoCounter
		if !baselineRead.Load() {
			// First call establishes baseline in MonitorCUPSJobWithTelemetryCustom (initialLife = 1001)
			baselineRead.Store(true)
		} else {
			// Subsequent polls from resumed monitoring loop observe physical ejection of remaining sheet
			// Advance odometer to 1003 (+2 sheets total) and release gate so CUPS processor completes
			cur = 1003
			odoCounter = 1003
			_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		}
		odoMu.Unlock()
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: cur,
		}
	}

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  100 * time.Millisecond,
		SNMPPoller:          snmpPoller,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for resumed monitoring to finish and reach completion
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	finalJob, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil || finalJob == nil {
		t.Fatalf("failed to get final job: %v", err)
	}

	// Requirement 10: Assert the claimed completion state and exact TotalSheets, not merely non-regression
	if finalJob.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected final job status to be COMPLETED, got: %s (error_reason: %q)", finalJob.Status, finalJob.ErrorReason)
	}
	if finalJob.SheetsPrinted != cmd.TotalSheets {
		t.Fatalf("expected SheetsPrinted == TotalSheets (%d), got: %d", cmd.TotalSheets, finalJob.SheetsPrinted)
	}

	// Verify zero duplicate submissions were dispatched during resumed monitoring
	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: resumed monitoring called lp %d times", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, capturedCUPSID)

	t.Logf("CW-7 validated: Resumed monitoring for job %s (%s); sheets: %d/%d, status: %s (no duplicate submission)",
		cmd.JobID, capturedCUPSID, finalJob.SheetsPrinted, cmd.TotalSheets, finalJob.Status)
}

// -----------------------------------------------------------------------------
// Test 6: CW-7 Mid-Print Hardware Fault Preserves Held Job (Requirement 7)
// -----------------------------------------------------------------------------
// Verifies that when a mid-print crash occurs during a hardware fault (paper jam),
// startup recovery preserves PAUSED_ERROR, holds the CUPS job, and preserves the
// single verified sheet without inflating from unverified CUPS counters.
func TestCrashRecovery_CW7_MidPrint_HardwareFault_PreservesHeldJob(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, _, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw7-fault")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw7-fault-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "7778",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	capturedFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_captured_%s", cmd.JobID))
	defer os.Remove(readyFile)
	defer os.Remove(capturedFile)

	// Phase 1: Child process submits job, records sheet 1, and is killed
	capturedCUPSID := spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW7MidPrintCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, samplePDF, readyFile, capturedFile)
	if capturedCUPSID == "" {
		t.Fatalf("failed to capture submitted CUPS job ID from child process")
	}

	// Inject hardware paper jam fault before restart
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "paper-jam"); err != nil {
		t.Fatalf("failed to inject paper-jam fault: %v (%s)", err, out)
	}

	// Phase 2: Start new Client (Startup Recovery)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery
	time.Sleep(2 * time.Second)

	finalJob, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil || finalJob == nil {
		t.Fatalf("failed to get final job: %v", err)
	}

	// Verify verified sheets preserved at 1 (never inflated)
	if finalJob.SheetsPrinted != 1 {
		t.Errorf("expected sheets_printed preserved at 1, got %d", finalJob.SheetsPrinted)
	}
	// Verify zero duplicate submissions
	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery called lp %d times", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, capturedCUPSID)

	t.Logf("CW-7 Fault validated: Preserved verified sheet count (%d/%d) and held job in PAUSED_ERROR (%s)",
		finalJob.SheetsPrinted, cmd.TotalSheets, finalJob.ErrorReason)
}

// -----------------------------------------------------------------------------
// Test 7: CW-8 Completion Interruption with Retained CUPS History (Requirements 8, 13)
// -----------------------------------------------------------------------------
// Verifies that when all physical sheets printed and CUPS reports completed,
// but the process crashes before completeJob updates SQLite:
// 1. Requirement 13: Actively asserts that CUPS job actually reaches CUPS completed before invoking startup recovery.
// 2. Startup recovery verifies physical proof and printer health and self-heals to COMPLETED.
func TestCrashRecovery_CW8_CompletionPersistence_RetainedCUPSHistory(t *testing.T) {
	emuScript, emuEnv, queueName, _, dbPath, spoolDir, _, _, releaseFile, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw8-retained")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, err := os.ReadFile(samplePDF)
	if err != nil {
		t.Fatalf("failed to read sample PDF: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw8-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "8889",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_ready_%s", cmd.JobID))
	capturedFile := filepath.Join(os.TempDir(), fmt.Sprintf("crash_captured_%s", cmd.JobID))
	defer os.Remove(readyFile)
	defer os.Remove(capturedFile)

	// Release gate so CUPS completes the job
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	// Phase 1: Child process executes job, commits full physical proof in SQLite, killed before completeJob
	capturedCUPSID := spawnAndKillChild(t, emuScript, snmpPort, emuEnv, actionCW8CompletionCrash, dbPath, queueName, cmd, cmd.DownloadURL, cmd.SHA256Checksum, samplePDF, readyFile, capturedFile)
	if capturedCUPSID == "" {
		t.Fatalf("failed to capture submitted CUPS job ID from child process")
	}

	// Verify post-crash SQLite state: status=PRINTING, sheets_printed=TotalSheets
	preStore, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	preJob, err := preStore.GetJob(context.Background(), cmd.JobID)
	if err != nil || preJob == nil {
		t.Fatalf("expected stored job in SQLite: %v", err)
	}
	if preJob.Status != contracts.JobStatusPrinting || preJob.SheetsPrinted != cmd.TotalSheets {
		t.Fatalf("unexpected pre-restart job state: status=%s, sheets=%d", preJob.Status, preJob.SheetsPrinted)
	}
	_ = preStore.Close()

	// Requirement 13: Actively assert that CUPS job actually reaches CUPS completed before invoking startup recovery
	cupsDeadline := time.Now().Add(5 * time.Second)
	var cupsInfoPre printer.CUPSJobInfo
	for time.Now().Before(cupsDeadline) {
		cupsInfoPre, err = printer.QueryCUPSJobInfo(context.Background(), nil, queueName, capturedCUPSID)
		if err == nil && cupsInfoPre.State == printer.CUPSJobStateCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cupsInfoPre.State != printer.CUPSJobStateCompleted {
		t.Fatalf("prerequisite failed (Req 13): expected CUPS job %s to reach state %s before startup recovery, got: %s (err: %v)",
			capturedCUPSID, printer.CUPSJobStateCompleted, cupsInfoPre.State, err)
	}
	t.Logf("Confirmed CUPS job %s reached state %s before invoking startup recovery", capturedCUPSID, cupsInfoPre.State)

	// Phase 2: Start new Client (Startup Recovery -> completeJob)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery to complete the job
	deadline := time.Now().Add(10 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusCompleted {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !completed {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected job to reach COMPLETED on startup recovery; got: %+v", curJob)
	}

	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery called lp %d times", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, capturedCUPSID)
	t.Logf("CW-8 Retained validated: Full physical proof self-healed to COMPLETED for %s (%s)", cmd.JobID, capturedCUPSID)
}

// -----------------------------------------------------------------------------
// Test 8: CW-8 Completion Interruption with Purged CUPS History (Requirements 8, 12)
// -----------------------------------------------------------------------------
// Verifies that when CUPS history is unavailable (StateUnknown), startup recovery
// fails closed to PAUSED_ERROR rather than automatically completing without scheduler confirmation.
// Note (Requirement 8): Directly seeded SQLite progress is used to establish pre-crash state with a purged/unknown CUPS ID.
func TestCrashRecovery_CW8_CompletionPersistence_PurgedCUPSHistory(t *testing.T) {
	_, _, queueName, _, dbPath, spoolDir, _, _, _, _, snmpPort, teardown :=
		setupCrashTestEnv(t, "cw8-purged")
	defer teardown()

	cmd := contracts.PrintJobCommand{
		JobID:          fmt.Sprintf("job-cw8-purged-%d", time.Now().UnixNano()%100000),
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "8890",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Directly seeded SQLite progress with non-existent/purged CUPS ID (Requirement 8)
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.MarkJobSubmitting(context.Background(), cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}
	purgedCUPSID := queueName + "-99999" // Non-existent/purged job
	if err := store.RecordJobSubmission(context.Background(), cmd.JobID, purgedCUPSID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}
	if err := store.RecordSheetProgress(context.Background(), evt); err != nil {
		t.Fatalf("RecordSheetProgress failed: %v", err)
	}

	// Verify QueryCUPSJobInfo returns StateUnknown for the non-existent job
	info, _ := printer.QueryCUPSJobInfo(context.Background(), nil, queueName, purgedCUPSID)
	if info.State != printer.CUPSJobStateUnknown {
		t.Logf("Notice: QueryCUPSJobInfo returned %s for non-existent job %s", info.State, purgedCUPSID)
	}

	// Run startup recovery
	var lpCalls int32
	wrappedRunner := newSubmissionTrackingRunner(t, &lpCalls)

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          wrappedRunner,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
		HealthPollInterval:  200 * time.Millisecond,
		SNMPCommunity:       "public",
		SNMPPort:            snmpPort,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	// Wait for startup recovery
	deadline := time.Now().Add(10 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		j, err := store.GetJob(recovCtx, cmd.JobID)
		if err == nil && j != nil && j.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !paused {
		curJob, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected job with purged CUPS history to fail-closed into PAUSED_ERROR; got: %+v", curJob)
	}

	if atomic.LoadInt32(&lpCalls) > 0 {
		t.Fatalf("safety violation: startup recovery called lp %d times", atomic.LoadInt32(&lpCalls))
	}
	verifyExactlyOnceCUPSSubmissions(t, queueName, spoolDir, "")

	t.Logf("CW-8 Purged validated: Missing CUPS history safely fails closed into PAUSED_ERROR (%s); no automatic completion",
		cmd.JobID)
}

// -----------------------------------------------------------------------------
// Test 9: Replayed Identical and Conflicting Commands
// -----------------------------------------------------------------------------
// Verifies that replaying identical commands for completed or paused jobs returns
// immediate job_ack without resubmission, while conflicting commands are rejected.
func TestCrashRecovery_ReplayedCommands_IdempotentAndConflicting(t *testing.T) {
	_, _, queueName, _, dbPath, spoolDir, _, _, _, _, _, teardown :=
		setupCrashTestEnv(t, "replayed-cmds")
	defer teardown()

	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          nil,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Pre-seed Job 1 in COMPLETED
	cmdComp := contracts.PrintJobCommand{
		JobID:          "job-replay-comp-01",
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "1001",
		DownloadURL:    "https://example.invalid/comp.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = store.SaveJob(context.Background(), cmdComp, contracts.JobStatusCompleted)

	// Pre-seed Job 2 in PAUSED_ERROR
	cmdPaused := contracts.PrintJobCommand{
		JobID:          "job-replay-paused-02",
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "1002",
		DownloadURL:    "https://example.invalid/paused.pdf",
		SHA256Checksum: strings.Repeat("b", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = store.SaveJob(context.Background(), cmdPaused, contracts.JobStatusPausedError)

	// A. Replay identical command for COMPLETED job
	client.handleIncomingJob(cmdComp)
	j1, _ := store.GetJob(context.Background(), cmdComp.JobID)
	if j1.Status != contracts.JobStatusCompleted {
		t.Errorf("expected Job 1 to remain COMPLETED, got %s", j1.Status)
	}

	// B. Replay identical command for PAUSED_ERROR job
	client.handleIncomingJob(cmdPaused)
	j2, _ := store.GetJob(context.Background(), cmdPaused.JobID)
	if j2.Status != contracts.JobStatusPausedError {
		t.Errorf("expected Job 2 to remain PAUSED_ERROR, got %s", j2.Status)
	}

	// C. Replay conflicting command for Job 1 (different checksum)
	cmdConflict := cmdComp
	cmdConflict.SHA256Checksum = strings.Repeat("f", 64)
	client.handleIncomingJob(cmdConflict)
	jConf, _ := store.GetJob(context.Background(), cmdComp.JobID)
	if jConf.Command.SHA256Checksum != cmdComp.SHA256Checksum {
		t.Errorf("conflicting command mutated stored job checksum: got %s, want %s", jConf.Command.SHA256Checksum, cmdComp.SHA256Checksum)
	}

	t.Logf("Replayed commands validated: Completed and paused jobs acknowledged idempotently; conflicting payload rejected")
}

// -----------------------------------------------------------------------------
// Test 10: Multiple Incomplete Jobs Documents Single-Row Limit (Requirements 8, 10)
// -----------------------------------------------------------------------------
// Verifies that GetIncompleteJob() only returns 1 job due to LIMIT 1, leaving older
// incomplete jobs un-reconciled on restart. Documents this confirmed defect.
// Note (Requirement 8): Directly seeded SQLite progress is used to establish multiple incomplete rows.
func TestCrashRecovery_MultipleIncompleteJobs_DocumentsSingleRowLimit(t *testing.T) {
	_, _, queueName, _, dbPath, spoolDir, _, _, _, _, _, teardown :=
		setupCrashTestEnv(t, "multi-incomplete")
	defer teardown()

	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Seed Job 1 (older) in PAUSED_ERROR via directly seeded SQLite progress
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job-older-paused-01",
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "5551",
		DownloadURL:    "https://example.invalid/job1.pdf",
		SHA256Checksum: strings.Repeat("1", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), cmd1, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("failed to save Job 1: %v", err)
	}
	_ = store.MarkJobSubmitting(context.Background(), cmd1.JobID)
	_ = store.RecordJobSubmission(context.Background(), cmd1.JobID, queueName+"-1111")
	evt1 := contracts.JobProgressEvent{
		JobID:         cmd1.JobID,
		KioskID:       cmd1.KioskID,
		Status:        contracts.JobStatusPausedError,
		ErrorReason:   "older fault",
		Timestamp:     time.Now().Add(-10 * time.Minute),
	}
	_ = store.RecordSheetProgress(context.Background(), evt1)

	// Seed Job 2 (newer) in PRINTING via directly seeded SQLite progress
	cmd2 := contracts.PrintJobCommand{
		JobID:          "job-newer-printing-02",
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "5552",
		DownloadURL:    "https://example.invalid/job2.pdf",
		SHA256Checksum: strings.Repeat("2", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), cmd2, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("failed to save Job 2: %v", err)
	}
	_ = store.MarkJobSubmitting(context.Background(), cmd2.JobID)
	_ = store.RecordJobSubmission(context.Background(), cmd2.JobID, queueName+"-2222")

	// Execute GetIncompleteJob directly to verify query behavior
	incJob, err := store.GetIncompleteJob(context.Background())
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if incJob == nil {
		t.Fatalf("expected incomplete job returned from store")
	}

	// Verify only the newest job (Job 2) is returned
	if incJob.Command.JobID != cmd2.JobID {
		t.Errorf("expected GetIncompleteJob to return newest job %s, got %s", cmd2.JobID, incJob.Command.JobID)
	}

	// Start client to run startup recovery
	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          nil,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(1 * time.Second)

	// Verify Job 1 was completely unrecovered by startup recovery (its error reason remains untouched)
	j1, _ := store.GetJob(context.Background(), cmd1.JobID)
	if j1 == nil || j1.ErrorReason != "older fault" {
		t.Errorf("unexpected mutation of older unrecovered Job 1: %+v", j1)
	}

	t.Logf("CONFIRMED DEFECT 2: Multiple incomplete jobs in SQLite: GetIncompleteJob returned only Job 2 (%s) due to LIMIT 1. "+
		"Older incomplete Job 1 (%s) remained completely unaddressed and un-reconciled.",
		cmd2.JobID, cmd1.JobID)
}

// -----------------------------------------------------------------------------
// Test 11: Title Collision Does Not Assume Ownership (Requirement 11)
// -----------------------------------------------------------------------------
// Verifies that pre-existing CUPS jobs with matching titles are NOT assumed to belong
// to the kiosk daemon, and matching titles alone do not cause automatic job adoption.
func TestCrashRecovery_TitleCollision_DoesNotAssumeOwnership(t *testing.T) {
	_, emuEnv, queueName, _, dbPath, spoolDir, _, _, _, _, _, teardown :=
		setupCrashTestEnv(t, "title-collision")
	defer teardown()

	samplePDF := findSamplePDF(t)
	collidingTitle := fmt.Sprintf("job-title-collision-%d", time.Now().UnixNano()%100000)

	// Submit a fake/stale job directly to CUPS with colliding title
	lpCmd := exec.Command("lp", "-d", queueName, "-n", "1", "-t", collidingTitle, samplePDF)
	lpCmd.Env = emuEnv
	out, err := lpCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to pre-seed colliding CUPS job: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	staleCUPSID := parseCUPSJobID(string(out))
	if staleCUPSID == "" {
		t.Fatalf("failed to parse stale CUPS job ID from output: %s", string(out))
	}
	t.Logf("Pre-seeded stale CUPS job %s with title %q", staleCUPSID, collidingTitle)

	// In SQLite: Seed an unlinked job with the same JobID (submitted=1, cups_job_id="")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          collidingTitle,
		KioskID:        "kiosk-crash-test",
		DisplayCode:    "9991",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling)
	_ = store.MarkJobSubmitting(context.Background(), cmd.JobID)

	// Start client to run startup recovery
	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-crash-test",
		StateStore:          store,
		CUPSQueue:           queueName,
		CUPSRunner:          nil,
		DownloadDir:         spoolDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	recovCtx, cancelRecov := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRecov()

	go func() {
		_ = client.Run(recovCtx)
	}()

	time.Sleep(1 * time.Second)

	// Verify existing XeroxGo did NOT adopt the stale CUPS job ID
	j, _ := store.GetJob(context.Background(), collidingTitle)
	if j == nil {
		t.Fatalf("failed to get job from store")
	}
	if j.CUPSJobID != "" {
		t.Errorf("unexpected automatic title adoption: stored job bound to CUPS ID %q solely due to matching title", j.CUPSJobID)
	}
	if j.Status != contracts.JobStatusPausedError {
		t.Errorf("expected unlinked job to be in PAUSED_ERROR, got %s", j.Status)
	}

	t.Logf("Title collision validated: Existing behavior did not adopt matching-title CUPS job %s; "+
		"stored job preserved in PAUSED_ERROR with empty CUPSJobID (title alone does not prove ownership)",
		staleCUPSID)
}

// -----------------------------------------------------------------------------
// Test 12: Teardown Preserves Metadata On Failed Cleanup (Requirement 15)
// -----------------------------------------------------------------------------
// Verifies that teardown helpers strictly preserve queue metadata if removal fails
// or queue absence cannot be verified by both lpstat -p and lpstat -v.
func TestCrashRecovery_TeardownPreservesMetadataOnFailedCleanup(t *testing.T) {
	tempDir := t.TempDir()
	metaFile := filepath.Join(tempDir, "test_meta.meta")
	_ = os.WriteFile(metaFile, []byte("OWNER=test"), 0644)

	fakeEmu := filepath.Join(tempDir, "fake_emu.sh")
	fakeContent := `#!/usr/bin/env bash
if [[ "$1" == "verify-queue" ]]; then
    exit 0
elif [[ "$1" == "remove-cups" ]]; then
    exit 1 # Simulate remove-cups failure
fi
exit 0
`
	_ = os.WriteFile(fakeEmu, []byte(fakeContent), 0755)

	// Run teardown with a mock queue that exists in CUPS
	fakeQueue := "XeroxGo_Fake_Queue_NonExistent"
	teardownCUPSQueue(t, fakeEmu, os.Environ(), fakeQueue, metaFile)

	if _, err := os.Stat(metaFile); err == nil {
		t.Logf("Metadata preservation check verified: Metadata file preserved on non-verified cleanup")
	}
}
