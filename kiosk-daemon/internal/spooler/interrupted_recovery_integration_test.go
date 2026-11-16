package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

var cupsJobIDRegex = regexp.MustCompile(`(?i)request id is ([a-zA-Z0-9_\-\.]+)`)

func parseCUPSJobID(output string) string {
	match := cupsJobIDRegex.FindStringSubmatch(output)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

// Helper: check prerequisites for CUPS and IPP integration tests
func checkCUPSAndIPPDependencies(t *testing.T) {
	t.Helper()
	for _, cmd := range []string{"lp", "lpadmin", "lpstat", "ipptool", "snmpget", "cancel"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("skipping test: required command %q not found in PATH", cmd)
		}
	}
	out, err := exec.Command("lpstat", "-r").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "is running") {
		t.Skip("skipping test: CUPS scheduler daemon is not running")
	}
}

// Helper: locate emulator.sh
func findEmulatorScript(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../../tools/printer-emulator/emulator.sh",
		"../../tools/printer-emulator/emulator.sh",
		"tools/printer-emulator/emulator.sh",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				return abs
			}
		}
	}
	t.Fatalf("emulator.sh script not found in search paths: %v", candidates)
	return ""
}

// Helper: locate testdata/sample.pdf
func findSamplePDF(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../../tools/printer-emulator/testdata/sample.pdf",
		"../../tools/printer-emulator/testdata/sample.pdf",
		"tools/printer-emulator/testdata/sample.pdf",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				return abs
			}
		}
	}
	t.Fatalf("sample.pdf fixture not found in search paths: %v", candidates)
	return ""
}

// Helper: allocate free TCP or UDP port
func getFreePort(t *testing.T, network string) int {
	t.Helper()
	switch network {
	case "tcp":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to allocate free TCP port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return port
	case "udp":
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to allocate free UDP port: %v", err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		_ = conn.Close()
		return port
	default:
		t.Fatalf("unsupported network %q", network)
		return 0
	}
}

// Helper: run emulator CLI command with given environment
func runEmu(t *testing.T, script string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runEmuIgnoreErr(script string, env []string, args ...string) string {
	cmd := exec.Command(script, args...)
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// teardownCUPSQueue safely removes the test CUPS queue with metadata preservation
func teardownCUPSQueue(t *testing.T, emuScript string, emuEnv []string, queueName, metaFile string) {
	t.Helper()
	if queueName == "" {
		return
	}
	out, err := runEmu(t, emuScript, emuEnv, "verify-queue", queueName)
	if err != nil {
		t.Logf("teardown: queue %q is not verified as emulator-owned; skipping deletion (%s)", queueName, strings.TrimSpace(out))
		return
	}
	delOut, delErr := runEmu(t, emuScript, emuEnv, "remove-cups", queueName)
	if delErr != nil {
		t.Errorf("teardown failure: verified queue %q removal failed: %v (%s). Preserving ownership metadata at %s for audit.",
			queueName, delErr, strings.TrimSpace(delOut), metaFile)
		return
	}
	_ = os.Remove(metaFile)
}

// ActiveJobMarker contains parsed fields from EMU_JOB_ACTIVE_FILE
type ActiveJobMarker struct {
	JobName   string
	IPPJobID  string
	PID       int
	State     string
	Timestamp string
}

func parseActiveJobMarker(filePath string) (ActiveJobMarker, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ActiveJobMarker{}, err
	}
	marker := ActiveJobMarker{}
	hasJobName := false
	hasPID := false
	hasState := false

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "JOB_NAME=") {
			marker.JobName = strings.TrimPrefix(line, "JOB_NAME=")
			if marker.JobName != "" {
				hasJobName = true
			}
		} else if strings.HasPrefix(line, "IPP_JOB_ID=") {
			marker.IPPJobID = strings.TrimPrefix(line, "IPP_JOB_ID=")
		} else if strings.HasPrefix(line, "PID=") {
			pidVal, pErr := strconv.Atoi(strings.TrimPrefix(line, "PID="))
			if pErr == nil && pidVal > 0 {
				marker.PID = pidVal
				hasPID = true
			}
		} else if strings.HasPrefix(line, "STATE=") {
			marker.State = strings.TrimPrefix(line, "STATE=")
			if marker.State != "" {
				hasState = true
			}
		} else if strings.HasPrefix(line, "TIMESTAMP=") {
			marker.Timestamp = strings.TrimPrefix(line, "TIMESTAMP=")
		}
	}
	if !hasJobName || !hasPID || !hasState {
		return marker, errors.New("incomplete active job marker")
	}
	return marker, nil
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func validateActiveJobCustom(
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

	if marker.JobName != expectedJobID {
		return fmt.Errorf("marker job ID mismatch: got %q, want %q", marker.JobName, expectedJobID)
	}

	if marker.State != "processing" {
		return fmt.Errorf("marker state is not processing (got %q)", marker.State)
	}

	if !isProcessAlive(marker.PID) {
		return fmt.Errorf("active marker PID %d is not running", marker.PID)
	}

	cmdlineBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", marker.PID))
	cmdline := strings.ReplaceAll(string(cmdlineBytes), "\x00", " ")
	if !strings.Contains(cmdline, "processor.sh") {
		return fmt.Errorf("marker PID %d is not processor.sh (cmdline: %q)", marker.PID, cmdline)
	}

	if cupsJobID != "" && queueName != "" {
		info, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cupsJobID)
		if qErr != nil {
			return fmt.Errorf("failed to query CUPS job state for %s: %w", cupsJobID, qErr)
		}
		if info.State != printer.CUPSJobStateProcessing {
			return fmt.Errorf("CUPS job %s is not in processing state (got %s)", cupsJobID, info.State)
		}
	}

	return nil
}

// setupRecoveryTestEnv configures the isolated emulator, CUPS queue, and SQLite store
func setupRecoveryTestEnv(t *testing.T, scenario string) (
	emuScript string,
	emuEnv []string,
	queueName string,
	metaFile string,
	store *state.Store,
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
	emuName := fmt.Sprintf("XeroxGoRecovEmu_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)
	queueName = fmt.Sprintf("XeroxGoRecovQ_%s_%d_%d", scenario, os.Getpid(), time.Now().UnixNano()%100000)

	metaFile = filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_cups_meta_%s.meta", queueName))
	_ = os.Remove(metaFile)

	activeFile = filepath.Join(tempDir, "job.active")
	gateFile = filepath.Join(tempDir, "job.gate")
	releaseFile = filepath.Join(tempDir, "job.release")

	_ = os.WriteFile(gateFile, []byte("gate"), 0644)
	_ = os.Remove(activeFile)
	_ = os.Remove(releaseFile)

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

	// Setup driverless CUPS queue
	if out, err := runEmu(t, emuScript, emuEnv, "setup-cups"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup-snmp")
		t.Fatalf("failed to setup CUPS queue: %v (output: %s)", err, out)
	}

	dbPath := filepath.Join(tempDir, "state.db")
	var err error
	store, err = state.Open(dbPath)
	if err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "remove-cups", queueName)
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup-snmp")
		t.Fatalf("failed to open SQLite store: %v", err)
	}

	cleanupFunc = func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if store != nil {
			_ = store.Close()
		}
		teardownCUPSQueue(t, emuScript, emuEnv, queueName, metaFile)
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup")
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup-snmp")
	}

	return
}

// -----------------------------------------------------------------------------
// Test 1: Mid-Print Paper-Jam Recovery
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_PaperJam_PreservesHeldJobAndPausedError(t *testing.T) {
	emuScript, emuEnv, queueName, _, store, _, activeFile, _, releaseFile, _, snmpPort, teardown :=
		setupRecoveryTestEnv(t, "paper-jam")
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

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	_ = store.ResetPaperTray(ctx, 100)

	snmpCfg := printer.SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 strings.Replace(server.URL, "http://", "ws://", 1) + "/ws",
		KioskID:                  "kiosk-recov-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		SNMPPoller:               func(c context.Context) printer.HardwareTelemetry { return printer.PollHardwareTelemetry(c, snmpCfg) },
		HTTPClient:               server.Client(),
		HealthPollInterval:       100 * time.Millisecond,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	uniqueJobID := fmt.Sprintf("job-recov-jam-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-recov-01",
		DisplayCode:    "J101",
		DownloadURL:    server.URL + "/doc.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Background thread: synchronize fault injection once job is actively processing in CUPS
	faultDone := make(chan error, 1)
	var capturedCUPSID string
	var idMu sync.Mutex

	go func() {
		deadline := time.Now().Add(6 * time.Second)
		var cID string
		for time.Now().Before(deadline) {
			cID = client.GetCUPSJobID(cmd.JobID)
			if cID != "" {
				idMu.Lock()
				capturedCUPSID = cID
				idMu.Unlock()
				if err := validateActiveJobCustom(ctx, activeFile, cmd.JobID, queueName, cID); err == nil {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		if cID == "" {
			faultDone <- errors.New("timeout waiting for active CUPS job ID")
			return
		}

		// Inject paper-jam fault
		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "paper-jam"); fErr != nil {
			faultDone <- fmt.Errorf("fault set paper-jam failed: %v (%s)", fErr, out)
			return
		}
		faultDone <- nil
	}()

	// Process job - monitor will detect paper-jam and hold job in CUPS
	client.processJob(ctx, cmd)

	if fErr := <-faultDone; fErr != nil {
		t.Fatalf("fault injection failure: %v", fErr)
	}

	idMu.Lock()
	cID := capturedCUPSID
	idMu.Unlock()
	if cID == "" {
		t.Fatal("expected captured CUPS job ID, got empty")
	}

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_, _ = exec.Command("cancel", cID).CombinedOutput()
		}
	})

	// 1. Verify job is placed in PAUSED_ERROR in SQLite
	storedJob, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || storedJob == nil {
		t.Fatalf("failed to query stored job: %v", err)
	}
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s (reason: %q)", storedJob.Status, storedJob.ErrorReason)
	}
	if storedJob.CUPSJobID != cID {
		t.Fatalf("expected stored CUPSJobID %s, got %s", cID, storedJob.CUPSJobID)
	}

	// 2. Independently verify CUPS state is pending-held
	cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("failed to query CUPS job info for %s: %v", cID, qErr)
	}
	if cupsInfo.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected CUPS job state pending-held, got %s", cupsInfo.State)
	}

	// 3. Clear fault in emulator (printer returns to healthy)
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear"); err != nil {
		t.Fatalf("failed to clear fault: %v (%s)", err, out)
	}

	// 4. Execute spooler runtime reconciliation
	client.reconcileInterruptedCUPSJob(ctx)

	// 5. Requirements 7, 8: Assert reconciliation NEVER releases the held job or resubmits lp
	cupsInfoAfter, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if qErr != nil {
		t.Fatalf("failed to query CUPS job info after reconciliation: %v", qErr)
	}
	if cupsInfoAfter.State != printer.CUPSJobStateHeld {
		t.Fatalf("CRITICAL SAFETY VIOLATION: reconciliation released held job! State: %s", cupsInfoAfter.State)
	}

	// 6. Assert SQLite status remains PAUSED_ERROR and verified physical sheets remain preserved
	storedJobAfter, _ := store.GetJob(ctx, cmd.JobID)
	if storedJobAfter.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain in PAUSED_ERROR, got %s", storedJobAfter.Status)
	}
	if storedJobAfter.SheetsPrinted != 0 {
		t.Fatalf("expected 0 sheets printed for interrupted job, got %d", storedJobAfter.SheetsPrinted)
	}

	t.Logf("Paper-jam recovery validated: Job %s (CUPS ID %s) remains safely held in CUPS and PAUSED_ERROR in SQLite", cmd.JobID, cID)
}

// -----------------------------------------------------------------------------
// Test 2: Mid-Print Paper-Empty Recovery
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_PaperEmpty_PreservesHeldJobAndPausedError(t *testing.T) {
	emuScript, emuEnv, queueName, _, store, _, activeFile, _, releaseFile, _, snmpPort, teardown :=
		setupRecoveryTestEnv(t, "paper-empty")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, _ := os.ReadFile(samplePDF)
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	_ = store.ResetPaperTray(ctx, 100)

	snmpCfg := printer.SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 strings.Replace(server.URL, "http://", "ws://", 1) + "/ws",
		KioskID:                  "kiosk-recov-02",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		SNMPPoller:               func(c context.Context) printer.HardwareTelemetry { return printer.PollHardwareTelemetry(c, snmpCfg) },
		HTTPClient:               server.Client(),
		HealthPollInterval:       100 * time.Millisecond,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	uniqueJobID := fmt.Sprintf("job-recov-empty-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-recov-02",
		DisplayCode:    "E102",
		DownloadURL:    server.URL + "/doc.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var capturedCUPSID string
	var idMu sync.Mutex
	faultDone := make(chan error, 1)

	go func() {
		deadline := time.Now().Add(6 * time.Second)
		var cID string
		for time.Now().Before(deadline) {
			cID = client.GetCUPSJobID(cmd.JobID)
			if cID != "" {
				idMu.Lock()
				capturedCUPSID = cID
				idMu.Unlock()
				if err := validateActiveJobCustom(ctx, activeFile, cmd.JobID, queueName, cID); err == nil {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		if cID == "" {
			faultDone <- errors.New("timeout waiting for active CUPS job ID")
			return
		}

		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "paper-empty"); fErr != nil {
			faultDone <- fmt.Errorf("fault set paper-empty failed: %v (%s)", fErr, out)
			return
		}
		faultDone <- nil
	}()

	client.processJob(ctx, cmd)
	if fErr := <-faultDone; fErr != nil {
		t.Fatalf("fault injection failure: %v", fErr)
	}

	idMu.Lock()
	cID := capturedCUPSID
	idMu.Unlock()

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_, _ = exec.Command("cancel", cID).CombinedOutput()
		}
	})

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob == nil || storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %v", storedJob)
	}

	cupsInfo, _ := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if cupsInfo.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected CUPS job state pending-held, got %s", cupsInfo.State)
	}

	// Clear fault (paper refill)
	_, _ = runEmu(t, emuScript, emuEnv, "fault", "set", "clear")

	// Runtime reconciliation
	client.reconcileInterruptedCUPSJob(ctx)

	cupsInfoAfter, _ := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if cupsInfoAfter.State != printer.CUPSJobStateHeld {
		t.Fatalf("held job was released after paper-empty recovery! State: %s", cupsInfoAfter.State)
	}
	storedAfter, _ := store.GetJob(ctx, cmd.JobID)
	if storedAfter.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain PAUSED_ERROR, got %s", storedAfter.Status)
	}
}

// -----------------------------------------------------------------------------
// Test 3: Mid-Print Cover-Open Recovery
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_CoverOpen_PreservesHeldJobAndPausedError(t *testing.T) {
	emuScript, emuEnv, queueName, _, store, _, activeFile, _, releaseFile, _, snmpPort, teardown :=
		setupRecoveryTestEnv(t, "cover-open")
	defer teardown()

	samplePDF := findSamplePDF(t)
	pdfBytes, _ := os.ReadFile(samplePDF)
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	_ = store.ResetPaperTray(ctx, 100)

	snmpCfg := printer.SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 strings.Replace(server.URL, "http://", "ws://", 1) + "/ws",
		KioskID:                  "kiosk-recov-03",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		SNMPPoller:               func(c context.Context) printer.HardwareTelemetry { return printer.PollHardwareTelemetry(c, snmpCfg) },
		HTTPClient:               server.Client(),
		HealthPollInterval:       100 * time.Millisecond,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	uniqueJobID := fmt.Sprintf("job-recov-cover-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-recov-03",
		DisplayCode:    "C103",
		DownloadURL:    server.URL + "/doc.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var capturedCUPSID string
	var idMu sync.Mutex
	faultDone := make(chan error, 1)

	go func() {
		deadline := time.Now().Add(6 * time.Second)
		var cID string
		for time.Now().Before(deadline) {
			cID = client.GetCUPSJobID(cmd.JobID)
			if cID != "" {
				idMu.Lock()
				capturedCUPSID = cID
				idMu.Unlock()
				if err := validateActiveJobCustom(ctx, activeFile, cmd.JobID, queueName, cID); err == nil {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		if cID == "" {
			faultDone <- errors.New("timeout waiting for active CUPS job ID")
			return
		}

		if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "cover-open"); fErr != nil {
			faultDone <- fmt.Errorf("fault set cover-open failed: %v (%s)", fErr, out)
			return
		}
		faultDone <- nil
	}()

	client.processJob(ctx, cmd)
	if fErr := <-faultDone; fErr != nil {
		t.Fatalf("fault injection failure: %v", fErr)
	}

	idMu.Lock()
	cID := capturedCUPSID
	idMu.Unlock()

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if cID != "" && strings.HasPrefix(cID, queueName+"-") {
			_, _ = exec.Command("cancel", cID).CombinedOutput()
		}
	})

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob == nil || storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %v", storedJob)
	}

	cupsInfo, _ := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if cupsInfo.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected CUPS job state pending-held, got %s", cupsInfo.State)
	}

	// Clear fault (cover closed)
	_, _ = runEmu(t, emuScript, emuEnv, "fault", "set", "clear")

	client.reconcileInterruptedCUPSJob(ctx)

	cupsInfoAfter, _ := printer.QueryCUPSJobInfo(ctx, nil, queueName, cID)
	if cupsInfoAfter.State != printer.CUPSJobStateHeld {
		t.Fatalf("held job was released after cover-open recovery! State: %s", cupsInfoAfter.State)
	}
	storedAfter, _ := store.GetJob(ctx, cmd.JobID)
	if storedAfter.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain PAUSED_ERROR, got %s", storedAfter.Status)
	}
}

// -----------------------------------------------------------------------------
// Test 4: Mid-Print Connectivity Outage Recovery
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_ConnectivityOutage_PreservesHeldJobAndPausedError(t *testing.T) {
	emuScript, emuEnv, queueName, _, store, _, activeFile, _, releaseFile, _, snmpPort, teardown :=
		setupRecoveryTestEnv(t, "offline")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_ = store.ResetPaperTray(ctx, 100)

	uniqueJobID := fmt.Sprintf("job-recov-conn-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-recov-04",
		DisplayCode:    "O104",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// 1. Submit directly via lp to establish active job in CUPS with gate active
	lpOut, err := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	if err != nil {
		t.Fatalf("lp submission failed: %v (%s)", err, lpOut)
	}
	cupsID := parseCUPSJobID(string(lpOut))
	if cupsID == "" {
		t.Fatalf("failed to parse CUPS request ID from lp output: %s", lpOut)
	}

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if cupsID != "" && strings.HasPrefix(cupsID, queueName+"-") {
			_, _ = exec.Command("cancel", cupsID).CombinedOutput()
		}
	})

	// 2. Wait for processor to be actively processing
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := validateActiveJobCustom(ctx, activeFile, cmd.JobID, queueName, cupsID); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// 3. Inject connectivity outage (freeze IPP and SNMP endpoints)
	if out, fErr := runEmu(t, emuScript, emuEnv, "fault", "set", "offline"); fErr != nil {
		t.Fatalf("fault set offline failed: %v (%s)", fErr, out)
	}

	// 4. Hold active job in CUPS to prevent unmonitored execution during outage
	holdRes := printer.HoldAndVerifyCUPSJob(ctx, nil, queueName, cupsID)
	if holdRes.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected CUPS job to be held during connectivity outage, got %s (err: %v)", holdRes.State, holdRes.HoldErr)
	}

	// 5. Record job in SQLite state store as PAUSED_ERROR (interrupted by connectivity outage)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		ErrorReason:   "network connectivity lost during printing; CUPS job placed on hold, manual reconciliation required",
		Timestamp:     time.Now().UTC(),
	})

	snmpCfg := printer.SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-recov-04",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		SNMPPoller:               func(c context.Context) printer.HardwareTelemetry { return printer.PollHardwareTelemetry(c, snmpCfg) },
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	// 6. Restore connectivity (unfreeze emulator)
	if out, err := runEmu(t, emuScript, emuEnv, "fault", "set", "clear"); err != nil {
		t.Fatalf("failed to restore connectivity: %v (%s)", err, out)
	}

	// 7. Execute spooler runtime reconciliation after connectivity restoration
	client.reconcileInterruptedCUPSJob(ctx)

	// 8. Assert reconciliation NEVER releases the held job in CUPS
	cupsInfoAfter, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cupsID)
	if qErr != nil {
		t.Fatalf("failed to query CUPS job info after reconciliation: %v", qErr)
	}
	if cupsInfoAfter.State != printer.CUPSJobStateHeld {
		t.Fatalf("CRITICAL SAFETY VIOLATION: reconciliation released held job after connectivity restoration! State: %s", cupsInfoAfter.State)
	}

	// 9. Assert SQLite status remains PAUSED_ERROR
	storedAfter, _ := store.GetJob(ctx, cmd.JobID)
	if storedAfter == nil || storedAfter.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain in PAUSED_ERROR after connectivity recovery, got %v", storedAfter)
	}
	if storedAfter.SheetsPrinted != 0 {
		t.Fatalf("expected 0 sheets printed for interrupted job, got %d", storedAfter.SheetsPrinted)
	}

	t.Logf("Connectivity recovery validated: Job %s (CUPS ID %s) remains held in CUPS and PAUSED_ERROR in SQLite after network restoration", cmd.JobID, cupsID)
}

// -----------------------------------------------------------------------------
// Test 5: Active or Pending Job Triggers Hold Attempt & Distinguishes Outcome
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_ActiveOrPendingJob_TriggersHoldAttempt(t *testing.T) {
	_, _, queueName, _, store, _, activeFile, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "active-hold")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-active-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-active-01",
		DisplayCode:    "A105",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Submit directly via lp to establish active job in CUPS with gate active
	lpOut, err := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	if err != nil {
		t.Fatalf("lp submission failed: %v (%s)", err, lpOut)
	}
	cupsID := parseCUPSJobID(string(lpOut))
	if cupsID == "" {
		t.Fatalf("failed to parse CUPS request ID from lp output: %s", lpOut)
	}

	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0644)
		if cupsID != "" {
			_, _ = exec.Command("cancel", cupsID).CombinedOutput()
		}
	})

	// Wait for processor to be actively processing
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := validateActiveJobCustom(ctx, activeFile, cmd.JobID, queueName, cupsID); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Save in SQLite as PAUSED_ERROR (simulating interruption before hold completed)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-active-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	// Execute runtime reconciliation while job is active
	client.reconcileInterruptedCUPSJob(ctx)

	// Verify that reconciliation triggered a hold and the job transitioned to pending-held
	cupsInfo, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cupsID)
	if qErr != nil {
		t.Fatalf("failed to query CUPS job info: %v", qErr)
	}
	if cupsInfo.State != printer.CUPSJobStateHeld {
		t.Fatalf("expected job to be transitioned to pending-held, got %s", cupsInfo.State)
	}

	// Verify SQLite reason updated with hold outcome
	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if !strings.Contains(storedJob.ErrorReason, "placed on hold") && !strings.Contains(storedJob.ErrorReason, "held") {
		t.Fatalf("expected error reason to record hold outcome, got: %q", storedJob.ErrorReason)
	}
}

// -----------------------------------------------------------------------------
// Test 6: Completed CUPS Job Without Verified Physical Evidence (Requirement 10)
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_CompletedCUPSJob_WithoutPhysicalProof_PreservesPausedError(t *testing.T) {
	_, _, queueName, _, store, _, _, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "completed-no-proof")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-comp-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-comp-01",
		DisplayCode:    "C106",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("b", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Release gate so job completes in CUPS
	_ = os.WriteFile(releaseFile, []byte("release"), 0644)

	lpOut, err := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	if err != nil {
		t.Fatalf("lp submission failed: %v (%s)", err, lpOut)
	}
	cupsID := parseCUPSJobID(string(lpOut))

	// Wait for CUPS completion
	deadline := time.Now().Add(6 * time.Second)
	comp := false
	for time.Now().Before(deadline) {
		info, qErr := printer.QueryCUPSJobInfo(ctx, nil, queueName, cupsID)
		if qErr == nil && info.State == printer.CUPSJobStateCompleted {
			comp = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !comp {
		t.Fatalf("job %s did not reach completed state in CUPS", cupsID)
	}

	// Save in SQLite as PAUSED_ERROR with SheetsPrinted = 0 (interrupted before physical output)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-comp-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})
	if err != nil {
		t.Fatalf("failed to initialize spooler client: %v", err)
	}

	// Execute runtime reconciliation
	client.reconcileInterruptedCUPSJob(ctx)

	// Requirement 10: Assert completed CUPS jobs are NOT declared physically completed without sufficient verified evidence!
	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("CRITICAL DEFECT: completed CUPS job was declared completed without physical proof! Status: %s", storedJob.Status)
	}
	if storedJob.SheetsPrinted != 0 {
		t.Fatalf("expected SheetsPrinted=0 preserved, got %d", storedJob.SheetsPrinted)
	}
	if !strings.Contains(storedJob.ErrorReason, "without physical proof") || !strings.Contains(storedJob.ErrorReason, "manual reconciliation required") {
		t.Fatalf("expected explicit notice of unverified physical output, got: %q", storedJob.ErrorReason)
	}
}

// -----------------------------------------------------------------------------
// Test 7: Partial Physical Progress Preserves Verified Sheets & Ignores CUPS Counter
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_PartialPhysicalProgress_PreservesVerifiedOdometer(t *testing.T) {
	_, _, queueName, _, store, _, _, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "partial-progress")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-partial-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-part-01",
		DisplayCode:    "P107",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("c", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)
	lpOut, _ := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	cupsID := parseCUPSJobID(string(lpOut))

	// Pre-populate SQLite with verified 1 sheet printed (odometer confirmed 1 sheet)
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	_ = store.RecordJobSubmission(ctx, cmd.JobID, cupsID)
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  1,
		SheetsPrinted: 1, // 1 verified sheet
		ErrorReason:   "paper jam after sheet 1",
		Timestamp:     time.Now().UTC(),
	})

	client, _ := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-part-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})

	// Direct verification of reconcileSheetsAndPages invariant
	sheets, pages := reconcileSheetsAndPages(cmd, 1, 1, 2)
	if sheets != 1 || pages != 1 {
		t.Fatalf("reconcileSheetsAndPages promoted unverified CUPS counter! Got sheets=%d, pages=%d", sheets, pages)
	}

	// Runtime reconciliation
	client.reconcileInterruptedCUPSJob(ctx)

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob.SheetsPrinted != 1 || storedJob.PagesPrinted != 1 {
		t.Fatalf("expected preserved physical sheets=1, pages=1; got sheets=%d, pages=%d",
			storedJob.SheetsPrinted, storedJob.PagesPrinted)
	}
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain in PAUSED_ERROR, got %s", storedJob.Status)
	}
}

// -----------------------------------------------------------------------------
// Test 8: Canceled or Aborted CUPS Job
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_CanceledOrAbortedCUPSJob_PreservesPausedError(t *testing.T) {
	_, _, queueName, _, store, _, _, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "canceled-job")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-cancel-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-cancel-01",
		DisplayCode:    "X108",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("d", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)
	lpOut, _ := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	cupsID := parseCUPSJobID(string(lpOut))

	// Cancel job in CUPS
	_, _ = exec.Command("cancel", cupsID).CombinedOutput()

	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, cupsID)

	client, _ := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-cancel-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})

	client.reconcileInterruptedCUPSJob(ctx)

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected canceled job to remain in PAUSED_ERROR, got %s", storedJob.Status)
	}
	if !strings.Contains(storedJob.ErrorReason, "operator reconciliation required") && !strings.Contains(storedJob.ErrorReason, "manual reconciliation required") {
		t.Fatalf("expected operator reconciliation notice for canceled job, got: %q", storedJob.ErrorReason)
	}
}

// -----------------------------------------------------------------------------
// Test 9: Failed Query or Missing CUPS Job Fails Safely Without Reprinting
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_FailedQueryOrMissingJob_FailsSafely(t *testing.T) {
	_, _, queueName, _, store, _, _, _, _, _, _, teardown :=
		setupRecoveryTestEnv(t, "missing-job")
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-missing-01",
		KioskID:        "kiosk-missing",
		DisplayCode:    "M109",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("e", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, queueName+"-999999") // Non-existent CUPS ID

	client, _ := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-missing",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})

	// Must fail safely without crash, without changing state, without dispatching lp
	client.reconcileInterruptedCUPSJob(ctx)

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain safely in PAUSED_ERROR, got %s", storedJob.Status)
	}
}

// -----------------------------------------------------------------------------
// Test 10: Repeated Reconciliation Idempotence (Requirement 14)
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_RepeatedReconciliation_Idempotence(t *testing.T) {
	_, _, queueName, _, store, _, _, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "idempotence")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-idem-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-idem-01",
		DisplayCode:    "I110",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("f", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)
	lpOut, _ := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	cupsID := parseCUPSJobID(string(lpOut))

	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, cupsID)

	client, _ := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-idem-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})

	// Call reconciliation 5 times
	for i := 0; i < 5; i++ {
		client.reconcileInterruptedCUPSJob(ctx)
	}

	storedJob, _ := store.GetJob(ctx, cmd.JobID)
	if storedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status to remain PAUSED_ERROR across 5 reconciliations, got %s", storedJob.Status)
	}
	if storedJob.SheetsPrinted != 0 {
		t.Fatalf("expected SheetsPrinted=0 across repeated reconciliations, got %d", storedJob.SheetsPrinted)
	}
}

// -----------------------------------------------------------------------------
// Test 11: Startup Recovery of Incomplete Job (Requirements 1, 7, 13)
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_StartupRecovery_IncompleteJob(t *testing.T) {
	_, _, queueName, _, store, _, _, _, releaseFile, _, _, teardown :=
		setupRecoveryTestEnv(t, "startup-recov")
	defer teardown()

	samplePDF := findSamplePDF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	uniqueJobID := fmt.Sprintf("job-recov-boot-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := contracts.PrintJobCommand{
		JobID:          uniqueJobID,
		KioskID:        "kiosk-boot-01",
		DisplayCode:    "B111",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = os.WriteFile(releaseFile, []byte("release"), 0644)
	lpOut, _ := exec.Command("lp", "-d", queueName, "-n", "1", "-t", uniqueJobID, samplePDF).CombinedOutput()
	cupsID := parseCUPSJobID(string(lpOut))

	// Pre-populate as PAUSED_ERROR
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, cupsID)

	client, _ := NewClient(ClientConfig{
		CloudURL:                 "ws://127.0.0.1:8080/ws",
		KioskID:                  "kiosk-boot-01",
		StateStore:               store,
		CUPSQueue:                queueName,
		DownloadDir:              t.TempDir(),
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               noopSink,
		EnableCloudRecovery:      true,
		RequiredRecoveryHealthyCount: 1,
	})

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for startup recovery pass with deadline polling
	deadline := time.Now().Add(5 * time.Second)
	var storedJob *state.StoredJob
	for time.Now().Before(deadline) {
		job, err := store.GetJob(context.Background(), cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError && strings.Contains(job.ErrorReason, "reconciliation required") {
			storedJob = job
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if storedJob == nil {
		current, _ := store.GetJob(context.Background(), cmd.JobID)
		t.Fatalf("expected startup recovery to preserve PAUSED_ERROR with reconciliation required, got: %+v", current)
	}
}

// -----------------------------------------------------------------------------
// Test 12: Negative Regression — Failed Cleanup Preserves Ownership Metadata
// -----------------------------------------------------------------------------
func TestInterruptedRecovery_TeardownPreservesMetadataOnFailedCleanup(t *testing.T) {
	emuScript := findEmulatorScript(t)
	metaFile := filepath.Join(os.TempDir(), fmt.Sprintf("xeroxgo_mock_recov_meta_%d.meta", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(metaFile) })

	// Mismatched device URI: remove-cups must fail closed and preserve metadata
	content := fmt.Sprintf("QUEUE_NAME=MockRecovQueue_%d\nDEVICE_URI=ipp://192.168.1.99:631/ipp/print\nCREATED_AT=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(metaFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write mock metadata file: %v", err)
	}

	env := append(os.Environ(), fmt.Sprintf("EMU_META_FILE=%s", metaFile))
	queueName := fmt.Sprintf("MockRecovQueue_%d", os.Getpid())

	out, err := runEmu(t, emuScript, env, "remove-cups", queueName)
	if err == nil {
		t.Fatalf("expected remove-cups to fail on unowned mock queue, but succeeded: %s", out)
	}

	if _, err := os.Stat(metaFile); os.IsNotExist(err) {
		t.Fatalf("CRITICAL FAILURE: ownership metadata file was deleted despite remove-cups failure!")
	}
	t.Log("Verified: Ownership metadata strictly preserved when queue cleanup fails")
}
