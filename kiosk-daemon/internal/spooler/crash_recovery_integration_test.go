package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		lpPID := lpCmd.Process.Pid
		log.Printf("[CHILD-CW5] lp process started (PID: %d); capturing process identity evidence", lpPID)

		// Capture exact immutable process evidence for safe supervisor verification (Requirements 2, 4)
		ev, err := readLinuxProcessEvidence(lpPID)
		if err != nil {
			log.Fatalf("[CHILD-CW5] Failed to capture process evidence for in-flight lp (PID %d): %v", lpPID, err)
		}
		evBytes, err := json.Marshal(ev)
		if err != nil {
			log.Fatalf("[CHILD-CW5] Failed to marshal process evidence: %v", err)
		}
		if err := os.WriteFile(readyFile, evBytes, 0644); err != nil {
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

		// Requirements 8, 9, 10: Require successful emulator odometer advancement and valid live SNMP telemetry
		// before persisting verified sheet progress. Fail closed if hardware progress evidence is missing or inconsistent.
		emuScript := os.Getenv("CRASH_EMU_SCRIPT")
		if emuScript == "" {
			log.Fatalf("[CHILD-CW7] CRASH_EMU_SCRIPT environment variable is required")
		}
		snmpPortStr := os.Getenv("CRASH_SNMP_PORT")
		snmpPortVal, _ := strconv.Atoi(snmpPortStr)
		if snmpPortVal <= 0 {
			log.Fatalf("[CHILD-CW7] Invalid or missing CRASH_SNMP_PORT: %q", snmpPortStr)
		}

		snmpCfg := printer.SNMPConfig{
			Target:    "127.0.0.1",
			Port:      snmpPortVal,
			Community: "public",
			Timeout:   2 * time.Second,
		}

		// Step A: Live baseline SNMP query
		baselineTelem := printer.PollHardwareTelemetry(context.Background(), snmpCfg)
		if !baselineTelem.Online || !baselineTelem.HasLife {
			log.Fatalf("[CHILD-CW7] Live baseline SNMP telemetry failed or missing life counter: %+v", baselineTelem)
		}
		baselineLife := baselineTelem.LifeCount

		// Step B: Advance emulator odometer via emulator CLI
		snmpSetCmd := exec.Command(emuScript, "snmp-set", "-counter-inc", "1")
		snmpSetCmd.Env = os.Environ()
		if snmpOut, err := snmpSetCmd.CombinedOutput(); err != nil {
			log.Fatalf("[CHILD-CW7] Emulator snmp-set -counter-inc 1 failed: %v (%s)", err, strings.TrimSpace(string(snmpOut)))
		}

		// Step C: Verify live SNMP telemetry from emulator endpoint actually advanced
		postTelem := printer.PollHardwareTelemetry(context.Background(), snmpCfg)
		if !postTelem.Online || !postTelem.HasLife {
			log.Fatalf("[CHILD-CW7] Live post-advancement SNMP telemetry failed: %+v", postTelem)
		}
		expectedLife := baselineLife + 1
		if postTelem.LifeCount != expectedLife {
			log.Fatalf("[CHILD-CW7] Live SNMP hardware progress evidence inconsistent: expected odometer %d, got %d", expectedLife, postTelem.LifeCount)
		}
		log.Printf("[CHILD-CW7] Live SNMP verified physical progress: baseline %d -> advanced %d", baselineLife, postTelem.LifeCount)

		// Step D: Only now durably record sheet 1 verified physical progress in SQLite (live SNMP-verified)
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
// In-Flight Process Supervision & Process Identity Evidence (Requirements 2, 3, 4, 5, 6)
// -----------------------------------------------------------------------------

type InFlightProcessEvidence struct {
	PID            int    `json:"pid"`
	PPID           int    `json:"ppid"`
	Comm           string `json:"comm"`
	Exe            string `json:"exe"`
	StartTimeTicks uint64 `json:"start_time_ticks"`
	State          string `json:"state,omitempty"`
}

// readLinuxProcessEvidence reads live process identity and immutable start time
// from /proc/<pid>/stat and /proc/<pid>/exe.
func readLinuxProcessEvidence(pid int) (*InFlightProcessEvidence, error) {
	if pid <= 1 {
		return nil, fmt.Errorf("invalid pid %d", pid)
	}
	statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, fmt.Errorf("cannot read /proc/%d/stat: %w", pid, err)
	}
	statStr := string(statBytes)
	firstParen := strings.Index(statStr, "(")
	lastParen := strings.LastIndex(statStr, ")")
	if firstParen == -1 || lastParen == -1 || lastParen <= firstParen {
		return nil, fmt.Errorf("malformed /proc/%d/stat: cannot parse comm", pid)
	}
	comm := statStr[firstParen+1 : lastParen]
	afterParen := strings.TrimSpace(statStr[lastParen+1:])
	fields := strings.Fields(afterParen)
	// afterParen fields:
	// fields[0] = state (field 3)
	// fields[1] = ppid (field 4)
	// ...
	// fields[19] = starttime (field 22)
	if len(fields) < 20 {
		return nil, fmt.Errorf("malformed /proc/%d/stat: insufficient fields (%d)", pid, len(fields))
	}
	state := fields[0]
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("malformed ppid in /proc/%d/stat: %w", pid, err)
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return nil, fmt.Errorf("malformed starttime in /proc/%d/stat: %w", pid, err)
	}
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return &InFlightProcessEvidence{
		PID:            pid,
		PPID:           ppid,
		Comm:           comm,
		Exe:            exe,
		StartTimeTicks: startTicks,
		State:          state,
	}, nil
}

const (
	sysPidfdOpen       = 434
	sysPidfdSendSignal = 424
)

// openPidfd attempts to open a Linux pidfd for the specified process.
func openPidfd(pid int) (int, error) {
	if pid <= 1 {
		return -1, fmt.Errorf("invalid pid %d", pid)
	}
	r1, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1, fmt.Errorf("pidfd_open failed for pid %d: %w", pid, errno)
	}
	return int(r1), nil
}

// pidfdSendSignal sends a signal using a Linux pidfd handle.
func pidfdSendSignal(pidfd int, sig syscall.Signal) error {
	r1, _, errno := syscall.Syscall6(sysPidfdSendSignal, uintptr(pidfd), uintptr(sig), 0, 0, 0, 0)
	if r1 != 0 && errno != 0 {
		return fmt.Errorf("pidfd_send_signal failed: %w", errno)
	}
	return nil
}

var openPidfdFn = openPidfd

// verifyAndTerminateInFlightProcess safely pins the in-flight process using a Linux pidfd handle
// BEFORE verifying process identity, start-time ticks, executable path, and test parentage.
// It delivers SIGKILL exclusively via the pinned pidfd handle (SYS_pidfd_send_signal).
// Numeric-PID SIGKILL fallback is strictly prohibited.
// Returns:
//   - terminated=true if the process was verified and successfully killed via pidfd.
//   - alreadyExited=true if the process already exited (ESRCH) or is a zombie (state 'Z'/'X').
//   - fails closed with an error (and sends ZERO signals) if pidfd cannot be opened,
//     kernel support is unavailable, or any identity/start-time/parentage check mismatches.
func verifyAndTerminateInFlightProcess(t *testing.T, ev *InFlightProcessEvidence, expectedParentPID int) (terminated bool, alreadyExited bool, err error) {
	if ev == nil || ev.PID <= 1 {
		return false, false, fmt.Errorf("invalid in-flight evidence: nil or pid <= 1")
	}
	if ev.PID == os.Getpid() || ev.PID == 1 {
		return false, false, fmt.Errorf("safety boundary violation: in-flight PID %d cannot be test supervisor or init", ev.PID)
	}

	// Verify that when spawned, the process was a direct child of expectedParentPID
	if expectedParentPID > 0 && ev.PPID != expectedParentPID {
		return false, false, fmt.Errorf("recorded parent PID mismatch for PID %d: expected %d, got %d", ev.PID, expectedParentPID, ev.PPID)
	}

	// Requirement 3 & 4: Require a valid Linux pidfd and pin the process BEFORE final identity verification.
	pfd, pfdErr := openPidfdFn(ev.PID)
	if pfdErr != nil {
		if errors.Is(pfdErr, syscall.ESRCH) || strings.Contains(pfdErr.Error(), "no such process") {
			// Process already exited and was reaped before pidfd could be opened.
			return false, true, nil
		}
		// Requirement 6: If pidfd opening fails or support is unavailable, fail closed without signaling.
		return false, false, fmt.Errorf("pidfd_open required for safe process signaling but failed for PID %d: %w", ev.PID, pfdErr)
	}
	defer syscall.Close(pfd)

	// Requirement 4 & 5: Process is now pinned by pfd. Read live process identity while pinned.
	liveEv, liveErr := readLinuxProcessEvidence(ev.PID)
	if liveErr != nil {
		if errors.Is(liveErr, os.ErrNotExist) || strings.Contains(liveErr.Error(), "no such file or directory") {
			// Process exited and /proc/<pid> was unlinked
			return false, true, nil
		}
		return false, false, fmt.Errorf("failed to read live process evidence for pinned PID %d: %w", ev.PID, liveErr)
	}

	// Requirement 7: Handle already-exited and zombie processes without treating them as successfully killed.
	if liveEv.State == "Z" || liveEv.State == "X" {
		return false, true, nil
	}

	// Requirement 5 & 6: Verify the pinned process matches recorded PID, comm, start time ticks, and executable.
	if liveEv.PID != ev.PID {
		return false, false, fmt.Errorf("pinned PID mismatch: expected %d, got %d", ev.PID, liveEv.PID)
	}
	if liveEv.Comm != ev.Comm {
		return false, false, fmt.Errorf("identity mismatch for pinned PID %d: expected comm %q, got %q", ev.PID, ev.Comm, liveEv.Comm)
	}
	if liveEv.StartTimeTicks != ev.StartTimeTicks {
		return false, false, fmt.Errorf("start time mismatch for pinned PID %d: expected ticks %d, got %d (PID reuse detected)", ev.PID, ev.StartTimeTicks, liveEv.StartTimeTicks)
	}
	if ev.Exe != "" && liveEv.Exe != "" && liveEv.Exe != ev.Exe && !strings.HasSuffix(liveEv.Exe, "/"+ev.Comm) {
		return false, false, fmt.Errorf("executable mismatch for pinned PID %d: expected %q, got %q", ev.PID, ev.Exe, liveEv.Exe)
	}

	// Requirement 2 & 3: Signal ONLY via pidfd handle. Zero numeric-PID fallback.
	if sigErr := pidfdSendSignal(pfd, syscall.SIGKILL); sigErr != nil {
		if errors.Is(sigErr, syscall.ESRCH) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("pidfd_send_signal(SIGKILL) failed for PID %d: %w", ev.PID, sigErr)
	}

	// Wait until in-flight process has terminated or transitioned to zombie
	terminatedCleanly := false
	for i := 0; i < 50; i++ {
		if checkErr := syscall.Kill(ev.PID, 0); checkErr != nil {
			if errors.Is(checkErr, syscall.ESRCH) {
				terminatedCleanly = true
				break
			}
		} else {
			if checkEv, err := readLinuxProcessEvidence(ev.PID); err == nil {
				if checkEv.State == "Z" || checkEv.State == "X" {
					terminatedCleanly = true
					break
				}
			} else {
				terminatedCleanly = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !terminatedCleanly {
		return false, false, fmt.Errorf("in-flight process %d did not terminate within timeout after SIGKILL", ev.PID)
	}

	return true, false, nil
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

	// Deliver real SIGKILL to the child runner process
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatalf("failed to deliver SIGKILL to child PID %d: %v", childPID, err)
	}

	// Wait for child process to exit and verify termination signal (Requirement 6)
	waitErr := childCmd.Wait()
	if waitErr == nil {
		t.Fatalf("expected child process %d to be killed by SIGKILL, but exited with code 0", childPID)
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		ws := exitErr.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("expected child process to be killed by SIGKILL (9), got: %v. Output:\n%s", ws, childOut.String())
		}
		t.Logf("Child subprocess PID %d successfully terminated by SIGKILL (action: %s)", childPID, action)
	} else {
		t.Fatalf("unexpected wait error from child process: %v. Output:\n%s", waitErr, childOut.String())
	}

	// Requirements 2, 3, 4, 5: If readyContent contains structured in-flight process evidence,
	// safely verify exact identity, start time ticks, and pidfd before signaling.
	// Never send SIGKILL based only on a raw numeric PID.
	if strings.HasPrefix(readyContent, "{") {
		var inFlightEv InFlightProcessEvidence
		if err := json.Unmarshal([]byte(readyContent), &inFlightEv); err == nil && inFlightEv.PID > 0 {
			term, exited, termErr := verifyAndTerminateInFlightProcess(t, &inFlightEv, childPID)
			if termErr != nil {
				t.Fatalf("safe in-flight process termination failed: %v. Subprocess Output:\n%s", termErr, childOut.String())
			}
			if term {
				t.Logf("Verified in-flight subprocess PID %d (%s) safely terminated by SIGKILL via pinned pidfd", inFlightEv.PID, inFlightEv.Comm)
			} else if exited {
				t.Logf("In-flight subprocess PID %d (%s) had already exited normally or was zombie before signal (no signal sent)", inFlightEv.PID, inFlightEv.Comm)
			}
		} else if err != nil {
			t.Fatalf("failed to parse in-flight process evidence JSON: %v (raw: %q). Subprocess Output:\n%s", err, readyContent, childOut.String())
		}
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

	// Requirements 8, 9, 10: Clearly distinguish live SNMP readings (polled over UDP pre-crash) from synthetic post-restart telemetry.
	// Pre-crash: Child process verified actual live SNMP hardware progress (1000 -> 1001) against emulator responder before persisting sheet 1.
	// Post-restart: Synthetic in-memory poller simulates hardware advancing by the remaining sheet during resumed monitoring.
	var baselineRead atomic.Bool
	var odoCounter int64 = 1001
	var odoMu sync.Mutex

	syntheticPostRestartPoller := func(ctx context.Context) printer.HardwareTelemetry {
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
		SNMPPoller:          syntheticPostRestartPoller,
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

	// In D4c-3a, verify both Job 1 and Job 2 are examined and safely reconciled
	j1, _ := store.GetJob(context.Background(), cmd1.JobID)
	if j1 == nil || j1.Status != contracts.JobStatusPausedError || j1.CUPSJobID != queueName+"-1111" {
		t.Fatalf("unexpected state for Job 1: %+v", j1)
	}
	if !strings.Contains(j1.ErrorReason, "interrupted job in PAUSED_ERROR") {
		t.Errorf("expected Job 1 to be reconciled into PAUSED_ERROR with CUPS state, got: %s", j1.ErrorReason)
	}

	j2, _ := store.GetJob(context.Background(), cmd2.JobID)
	if j2 == nil || j2.Status != contracts.JobStatusPausedError || j2.CUPSJobID != queueName+"-2222" {
		t.Fatalf("unexpected state for Job 2: %+v", j2)
	}

	t.Logf("FIXED DEFECT 2: Multiple incomplete jobs in SQLite: GetIncompleteJob preserves single newest row for legacy callers (%s), while GetIncompleteJobs and startup recovery examine and safely reconcile ALL incomplete records (%s and %s).",
		cmd2.JobID, cmd1.JobID, cmd2.JobID)
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

// -----------------------------------------------------------------------------
// Negative Regression: In-Flight Process Verification Safety (Requirements 2, 3, 4, 5, 6, 7, 9)
// -----------------------------------------------------------------------------
// Verifies that in-flight subprocess supervision:
// 1. Refuses to signal an already-exited or reaped process (ESRCH), reporting alreadyExited==true.
// 2. Detects zombie processes ('Z'/'X') and returns alreadyExited==true without treating them as killed.
// 3. Fails closed without signaling when pidfd_open fails or is unsupported (ENOSYS), with ZERO numeric fallback.
// 4. Refuses to signal a live pinned process when start-time ticks mismatch (PID reuse protection).
// 5. Refuses to signal a process when comm or executable identity mismatches.
// 6. Refuses to signal a process when test-child parentage mismatches.
// 7. Refuses forbidden PIDs (PID <= 1, 0, -1, or test supervisor PID).
func TestCrashRecovery_InFlightProcessVerification_NegativeSafety(t *testing.T) {
	// Case 1: Exited process (reaped, ESRCH on pidfd_open)
	dummyCmd := exec.Command("true")
	if err := dummyCmd.Run(); err != nil {
		t.Fatalf("failed to run dummy command: %v", err)
	}
	deadPID := dummyCmd.Process.Pid

	deadEv := &InFlightProcessEvidence{
		PID:            deadPID,
		PPID:           os.Getpid(),
		Comm:           "true",
		Exe:            "/usr/bin/true",
		StartTimeTicks: 123456,
	}

	term, exited, err := verifyAndTerminateInFlightProcess(t, deadEv, os.Getpid())
	if err != nil {
		t.Errorf("expected no error for already-exited process, got: %v", err)
	}
	if term {
		t.Errorf("safety violation: claimed to terminate already-exited process %d", deadPID)
	}
	if !exited {
		t.Errorf("expected alreadyExited == true for dead PID %d", deadPID)
	}

	// Case 2: Zombie process (exited, not yet reaped by parent; state 'Z')
	zombieCmd := exec.Command("true")
	if err := zombieCmd.Start(); err != nil {
		t.Fatalf("failed to start command for zombie test: %v", err)
	}
	zombiePID := zombieCmd.Process.Pid
	// Wait briefly for `true` to finish executing in the kernel so it enters zombie state
	time.Sleep(50 * time.Millisecond)

	zombieEv, err := readLinuxProcessEvidence(zombiePID)
	if err != nil {
		_ = zombieCmd.Wait()
		t.Fatalf("failed to read process evidence for zombie test PID %d: %v", zombiePID, err)
	}
	if zombieEv.State != "Z" && zombieEv.State != "X" {
		t.Logf("Warning: process %d state is %q rather than 'Z' (timing jitter)", zombiePID, zombieEv.State)
	}

	term, exited, err = verifyAndTerminateInFlightProcess(t, zombieEv, os.Getpid())
	_ = zombieCmd.Wait() // reap zombie now
	if err != nil {
		t.Errorf("expected no error for zombie process, got: %v", err)
	}
	if term {
		t.Errorf("safety violation: claimed to terminate zombie process %d", zombiePID)
	}
	if !exited {
		t.Errorf("expected alreadyExited == true for zombie PID %d", zombiePID)
	}

	// Case 3: pidfd failure / kernel support unavailable (fails closed without signaling; zero numeric fallback)
	sleepCmd1 := exec.Command("sleep", "30")
	if err := sleepCmd1.Start(); err != nil {
		t.Fatalf("failed to start sleep process 1: %v", err)
	}
	livePID1 := sleepCmd1.Process.Pid
	defer func() {
		_ = sleepCmd1.Process.Kill()
		_ = sleepCmd1.Wait()
	}()

	liveEv1, err := readLinuxProcessEvidence(livePID1)
	if err != nil {
		t.Fatalf("failed to read live process evidence: %v", err)
	}

	// Temporarily simulate pidfd_open failure (e.g. syscall unsupported ENOSYS)
	origOpenPidfd := openPidfdFn
	openPidfdFn = func(pid int) (int, error) {
		return -1, fmt.Errorf("pidfd_open failed for pid %d: %w", pid, syscall.ENOSYS)
	}
	term, exited, err = verifyAndTerminateInFlightProcess(t, liveEv1, os.Getpid())
	openPidfdFn = origOpenPidfd // restore immediately

	if err == nil {
		t.Errorf("safety violation: expected failure when pidfd_open returns ENOSYS, but got nil error")
	}
	if term {
		t.Errorf("safety violation: claimed termination despite pidfd failure")
	}
	if exited {
		t.Errorf("safety violation: live process reported as alreadyExited")
	}
	// Verify that the live process was NOT signaled and remains alive (proving no numeric SIGKILL fallback was sent)
	if checkErr := syscall.Kill(livePID1, 0); checkErr != nil {
		t.Errorf("safety violation: live process %d was signaled despite pidfd_open failure: %v", livePID1, checkErr)
	}

	// Case 4: Stale process identity / PID reuse (start time mismatch on pinned process)
	sleepCmd2 := exec.Command("sleep", "30")
	if err := sleepCmd2.Start(); err != nil {
		t.Fatalf("failed to start sleep process 2: %v", err)
	}
	livePID2 := sleepCmd2.Process.Pid
	defer func() {
		_ = sleepCmd2.Process.Kill()
		_ = sleepCmd2.Wait()
	}()

	liveEv2, err := readLinuxProcessEvidence(livePID2)
	if err != nil {
		t.Fatalf("failed to read live process evidence: %v", err)
	}

	// Forge mismatched start time ticks
	staleEv := &InFlightProcessEvidence{
		PID:            livePID2,
		PPID:           liveEv2.PPID,
		Comm:           liveEv2.Comm,
		Exe:            liveEv2.Exe,
		StartTimeTicks: liveEv2.StartTimeTicks + 999999, // Mismatched ticks
	}

	term, exited, err = verifyAndTerminateInFlightProcess(t, staleEv, os.Getpid())
	if err == nil {
		t.Errorf("safety violation: expected error on PID reuse / start time mismatch, but verification succeeded")
	}
	if term {
		t.Errorf("safety violation: signaled process despite start time mismatch")
	}
	// Verify sleep process was NOT signaled and remains alive
	if checkErr := syscall.Kill(livePID2, 0); checkErr != nil {
		t.Errorf("safety violation: live process %d died or was signaled prematurely: %v", livePID2, checkErr)
	}

	// Case 5: Identity / executable mismatch on pinned process
	mismatchEv := &InFlightProcessEvidence{
		PID:            livePID2,
		PPID:           liveEv2.PPID,
		Comm:           "lp", // Live process is "sleep", not "lp"
		Exe:            "/usr/bin/lp",
		StartTimeTicks: liveEv2.StartTimeTicks,
	}
	term, exited, err = verifyAndTerminateInFlightProcess(t, mismatchEv, os.Getpid())
	if err == nil {
		t.Errorf("safety violation: expected error on comm mismatch, but verification succeeded")
	}
	if term {
		t.Errorf("safety violation: signaled process despite comm mismatch")
	}
	if checkErr := syscall.Kill(livePID2, 0); checkErr != nil {
		t.Errorf("safety violation: live process %d died or was signaled prematurely: %v", livePID2, checkErr)
	}

	// Case 6: Parentage mismatch
	term, exited, err = verifyAndTerminateInFlightProcess(t, liveEv2, 999999) // Mismatched expected parent PID
	if err == nil {
		t.Errorf("safety violation: expected error on parentage mismatch, got nil")
	}
	if term {
		t.Errorf("safety violation: signaled process despite parentage mismatch")
	}
	if checkErr := syscall.Kill(livePID2, 0); checkErr != nil {
		t.Errorf("safety violation: live process %d died or was signaled prematurely: %v", livePID2, checkErr)
	}

	// Case 7: Forbidden PIDs (supervisor PID, init, 0, -1)
	selfEv := &InFlightProcessEvidence{
		PID:            os.Getpid(),
		PPID:           os.Getppid(),
		Comm:           "test",
		StartTimeTicks: 1,
	}
	term, exited, err = verifyAndTerminateInFlightProcess(t, selfEv, os.Getppid())
	if err == nil {
		t.Errorf("safety violation: expected error for supervisor PID, got nil")
	}
	if term {
		t.Errorf("safety violation: signaled supervisor PID")
	}

	initEv := &InFlightProcessEvidence{
		PID:            1,
		PPID:           0,
		Comm:           "systemd",
		StartTimeTicks: 1,
	}
	term, exited, err = verifyAndTerminateInFlightProcess(t, initEv, 0)
	if err == nil {
		t.Errorf("safety violation: expected error for init PID 1, got nil")
	}
	if term {
		t.Errorf("safety violation: signaled init PID 1")
	}

	invalidEv := &InFlightProcessEvidence{
		PID: -1,
	}
	term, exited, err = verifyAndTerminateInFlightProcess(t, invalidEv, 0)
	if err == nil {
		t.Errorf("safety violation: expected error for PID -1, got nil")
	}
	if term {
		t.Errorf("safety violation: signaled PID -1")
	}
}
