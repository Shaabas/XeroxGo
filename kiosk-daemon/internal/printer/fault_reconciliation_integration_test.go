package printer

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// TestFaultReconciliation_Integration tests XeroxGo's ReconcilePrinterHealth
// against live telemetry produced by the D2/D3 snmp_responder emulator process
// and corresponding IPP/CUPS observations across all fault injection scenarios.
func TestFaultReconciliation_Integration(t *testing.T) {
	binPath := buildOrFindSNMPResponder(t)

	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "snmp_state.json")
	pidFile := filepath.Join(tempDir, "snmp_responder.pid")
	portFile := filepath.Join(tempDir, "snmp_responder.port")

	// Start snmp_responder serve on temporary loopback UDP port (dynamic port 0)
	cmd := exec.Command(binPath, "serve",
		"-host", "127.0.0.1",
		"-port", "0",
		"-community", "public",
		"-state", stateFile,
		"-pid", pidFile,
		"-port-file", portFile,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start snmp_responder binary process: %v", err)
	}

	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}()

	// Wait for portFile to be written
	var boundPort int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			pStr := strings.TrimSpace(string(data))
			if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
				boundPort = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if boundPort <= 0 {
		t.Fatalf("snmp_responder process did not write bound port within timeout")
	}

	cfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      boundPort,
		Community: "public",
		Timeout:   2 * time.Second,
		Retries:   1,
	}

	ctx := context.Background()

	// Wait for responder readiness
	ready := false
	for time.Now().Before(deadline) {
		if life := PollMarkerLifeCount(ctx, cfg); life >= 0 {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("snmp_responder on port %d was not ready within timeout", boundPort)
	}

	evalTime := time.Now()
	freshTime := evalTime.Add(-2 * time.Second)

	// Helper to run snmp_responder set command
	setSNMP := func(t *testing.T, args ...string) {
		t.Helper()
		cmdArgs := append([]string{"set", "-state", stateFile}, args...)
		setCmd := exec.Command(binPath, cmdArgs...)
		if out, err := setCmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to set snmp state (%v): %s", err, string(out))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 1. Healthy Agreement
	t.Run("Scenario_HealthyAgreement", func(t *testing.T) {
		setSNMP(t, "-status", "idle", "-clear-all-errors", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if !snmpHealth.Online || snmpHealth.State != "IDLE" {
			t.Fatalf("SNMP expected healthy IDLE, got Online=%v State=%s", snmpHealth.Online, snmpHealth.State)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateIdle,
			RawState:          "idle",
			AcceptingJobs:     AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if !res.IsReady() {
			t.Errorf("expected IsReady = true")
		}
		if len(res.Contradictions) > 0 {
			t.Errorf("expected 0 contradictions, got %v", res.Contradictions)
		}
	})

	// 2. Coordinated Paper Jam
	t.Run("Scenario_PaperJamAgreement", func(t *testing.T) {
		setSNMP(t, "-status", "other", "-clear-all-errors", "-set-error", "paper_jam", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if !snmpHealth.PaperJam {
			t.Fatalf("SNMP expected PaperJam=true, got false")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if !res.HasBlockingFaults() {
			t.Errorf("expected blocking faults, got none")
		}
		if res.IsReady() {
			t.Errorf("expected IsReady = false")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "paper jam") {
			t.Errorf("expected paper jam in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	// 3. Coordinated Cover Open
	t.Run("Scenario_CoverOpenAgreement", func(t *testing.T) {
		setSNMP(t, "-status", "other", "-clear-all-errors", "-set-error", "cover_open", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if !snmpHealth.CoverOpen {
			t.Fatalf("SNMP expected CoverOpen=true, got false")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"cover-open-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "cover-open-error", Keyword: "cover-open", Severity: SeverityError},
			},
			CoverOpen:         true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.CoverOpen {
			t.Errorf("expected CoverOpen = true")
		}
		if !sliceHasItemContaining(res.BlockingFaults, "cover") {
			t.Errorf("expected cover open in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	// 4. Coordinated Paper Empty
	t.Run("Scenario_PaperEmptyAgreement", func(t *testing.T) {
		setSNMP(t, "-status", "other", "-clear-all-errors", "-set-error", "no_paper", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if snmpHealth.PaperTrayStatus != "EMPTY" {
			t.Fatalf("SNMP expected PaperTrayStatus='EMPTY', got %q", snmpHealth.PaperTrayStatus)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "EMPTY",
			EstimatedSheets: 0,
			UpdatedAt:       freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-empty-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-empty-error", Keyword: "media-empty", Severity: SeverityError},
			},
			MediaEmpty:        true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("PaperTrayStatus = %q, want EMPTY", res.PaperTrayStatus)
		}
		if !sliceHasItemContaining(res.BlockingFaults, "media empty") {
			t.Errorf("expected media empty in BlockingFaults, got %v", res.BlockingFaults)
		}
	})

	// 5. Disagreement: IPP reports Paper Jam while SNMP reports Healthy
	t.Run("Scenario_IPPOnlyPaperJamContradiction", func(t *testing.T) {
		// SNMP healthy
		setSNMP(t, "-status", "idle", "-clear-all-errors", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if snmpHealth.PaperJam {
			t.Fatalf("SNMP expected PaperJam=false")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		// IPP reports Jam
		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateStopped,
			RawReasons:        []string{"media-jam-error"},
			ParsedReasons: []IPPPrinterReason{
				{Raw: "media-jam-error", Keyword: "media-jam", Severity: SeverityError},
			},
			PaperJam:          true,
			HasHardwareFault:  true,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		// Fault must NOT be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR (fault must not be suppressed)", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if len(res.Contradictions) == 0 {
			t.Errorf("expected contradiction / mismatch logged, got none")
		}
	})

	// 6. Disagreement: SNMP reports Paper Jam while IPP reports Healthy
	t.Run("Scenario_SNMPOnlyPaperJamContradiction", func(t *testing.T) {
		// SNMP reports Jam
		setSNMP(t, "-status", "other", "-clear-all-errors", "-set-error", "paper_jam", "-offline", "false")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if !snmpHealth.PaperJam {
			t.Fatalf("SNMP expected PaperJam=true")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		// IPP reports healthy idle
		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateIdle,
			RawState:          "idle",
			RawReasons:        []string{"none"},
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		// Fault must NOT be suppressed
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected PaperJam = true")
		}
		if len(res.Contradictions) == 0 {
			t.Errorf("expected contradiction / mismatch logged, got none")
		}
	})

	// 7. Controlled Offline Communication Loss
	t.Run("Scenario_OfflineCommunicationLoss", func(t *testing.T) {
		// Drop SNMP UDP packets
		setSNMP(t, "-offline", "true")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if snmpHealth.Online {
			t.Errorf("expected SNMP Online=false during packet drop")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:    false,
			State:     "OFFLINE",
			UpdatedAt: freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			EndpointReachable: false,
			QueueAccessible:   false,
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.Online {
			t.Errorf("expected reconciled Online=false")
		}
		if res.EffectiveState != "OFFLINE" {
			t.Errorf("EffectiveState = %q, want OFFLINE", res.EffectiveState)
		}
	})

	// 8. Recovery
	t.Run("Scenario_RecoveryToHealthy", func(t *testing.T) {
		// Restore SNMP communication and healthy state
		setSNMP(t, "-offline", "false", "-status", "idle", "-clear-all-errors")

		snmpHealth := PollHealthConfig(ctx, cfg, 250)
		if !snmpHealth.Online || snmpHealth.State != "IDLE" {
			t.Fatalf("expected SNMP recovered Online=true State=IDLE, got Online=%v State=%s", snmpHealth.Online, snmpHealth.State)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		ippHealth := IPPPrinterStateResult{
			Source:            CapabilitySourceCUPSQueue,
			Queue:             "XeroxGoEmuQueue",
			State:             IPPPrinterStateIdle,
			RawState:          "idle",
			RawReasons:        []string{"none"},
			AcceptingJobs:     AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			StatusCode:        "successful-ok",
			ObservedAt:        freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if !res.Online {
			t.Errorf("expected recovered Online=true")
		}
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults on recovery, got %v", res.BlockingFaults)
		}
		if !res.IsReady() {
			t.Errorf("expected IsReady = true on recovery")
		}
	})
}

// -----------------------------------------------------------------------------
// Live IPP + SNMP Integration Tests (exercising ipptool + ParseIPPPrinterState)
// -----------------------------------------------------------------------------

func checkIPPDependencies(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ipptool"); err != nil {
		t.Skip("ipptool command not found in PATH; skipping live IPP integration test")
	}
	found := false
	if _, err := exec.LookPath("ippeveprinter"); err == nil {
		found = true
	} else {
		candidates := []string{
			filepath.Join(os.Getenv("HOME"), ".local/bin/ippeveprinter"),
			"/usr/bin/ippeveprinter",
			"/usr/sbin/ippeveprinter",
			"/usr/local/bin/ippeveprinter",
			"/usr/lib/cups/command/ippeveprinter",
		}
		for _, c := range candidates {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() && (fi.Mode().Perm()&0111 != 0) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Skip("ippeveprinter binary not found; skipping live IPP integration test")
	}
}

func findEmulatorScript(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../../tools/printer-emulator/emulator.sh",
		"../../tools/printer-emulator/emulator.sh",
		"./tools/printer-emulator/emulator.sh",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
	}
	t.Fatalf("could not locate tools/printer-emulator/emulator.sh")
	return ""
}

func getFreePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to resolve udp addr: %v", err)
		}
		l, err := net.ListenUDP("udp", addr)
		if err != nil {
			t.Fatalf("failed to listen on udp: %v", err)
		}
		defer l.Close()
		return l.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on tcp: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func runEmu(t *testing.T, emuScript string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(emuScript, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runEmuIgnoreErr(emuScript string, env []string, args ...string) string {
	cmd := exec.Command(emuScript, args...)
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// TestFaultReconciliation_LiveIPPAndSNMP exercises real live IPP queries using ipptool,
// XeroxGo's ParseIPPPrinterState() parser, real live SNMP queries via PollHealthConfig(),
// and ReconcilePrinterHealth() across healthy, paper-jam, paper-empty, offline, and recovery.
func TestFaultReconciliation_LiveIPPAndSNMP(t *testing.T) {
	checkIPPDependencies(t)

	emuScript := findEmulatorScript(t)
	tempDir := t.TempDir()
	spoolDir := filepath.Join(tempDir, "spool")
	if err := os.MkdirAll(spoolDir, 0755); err != nil {
		t.Fatalf("failed to create spool dir: %v", err)
	}

	ippPort := getFreePort(t, "tcp")
	snmpPort := getFreePort(t, "udp")

	emuEnv := append(os.Environ(),
		fmt.Sprintf("EMU_PORT=%d", ippPort),
		fmt.Sprintf("SNMP_PORT=%d", snmpPort),
		"EMU_HOST=localhost",
		"SNMP_HOST=127.0.0.1",
		"EMU_NAME=XeroxGoEmuTest",
		"EMU_CUPS_QUEUE=XeroxGoEmuTest",
		fmt.Sprintf("EMU_SPOOL_DIR=%s", spoolDir),
		fmt.Sprintf("EMU_PID_FILE=%s", filepath.Join(tempDir, "ipp.pid")),
		fmt.Sprintf("EMU_LOG_FILE=%s", filepath.Join(tempDir, "ipp.log")),
		fmt.Sprintf("EMU_LOCK_DIR=%s", filepath.Join(tempDir, ".ipp_lock")),
		fmt.Sprintf("SNMP_PID_FILE=%s", filepath.Join(tempDir, "snmp.pid")),
		fmt.Sprintf("SNMP_LOG_FILE=%s", filepath.Join(tempDir, "snmp.log")),
		fmt.Sprintf("SNMP_STATE_FILE=%s", filepath.Join(tempDir, "snmp_state.json")),
		fmt.Sprintf("SNMP_LOCK_DIR=%s", filepath.Join(tempDir, ".snmp_lock")),
		fmt.Sprintf("EMU_FAULT_STATE_FILE=%s", filepath.Join(tempDir, "fault_state.json")),
		fmt.Sprintf("EMU_IPP_PAUSED_FILE=%s", filepath.Join(tempDir, "ipp_paused.flag")),
		fmt.Sprintf("EMU_META_FILE=%s", filepath.Join(tempDir, "cups.meta")),
	)

	// Start both emulators
	if out, err := runEmu(t, emuScript, emuEnv, "start"); err != nil {
		t.Fatalf("failed to start IPP emulator: %v (output: %s)", err, out)
	}
	if out, err := runEmu(t, emuScript, emuEnv, "start-snmp"); err != nil {
		_ = runEmuIgnoreErr(emuScript, emuEnv, "cleanup")
		t.Fatalf("failed to start SNMP emulator: %v (output: %s)", err, out)
	}

	defer func() {
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup")
		_, _ = runEmu(t, emuScript, emuEnv, "cleanup-snmp")
	}()

	snmpCfg := SNMPConfig{
		Target:    "127.0.0.1",
		Port:      snmpPort,
		Community: "public",
		Timeout:   1 * time.Second,
		Retries:   1,
	}

	ippURI := fmt.Sprintf("ipp://localhost:%d/ipp/print", ippPort)
	ctx := context.Background()

	// Wait for SNMP readiness
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
		t.Fatalf("SNMP emulator on port %d was not ready within timeout", snmpPort)
	}

	// 1. Healthy Scenario
	t.Run("Scenario_Healthy", func(t *testing.T) {
		if out, err := runEmu(t, emuScript, emuEnv, "fault-set", "healthy"); err != nil {
			t.Fatalf("fault-set healthy failed: %v (%s)", err, out)
		}

		evalTime := time.Now()
		freshTime := evalTime.Add(-1 * time.Second)

		// Real IPP query via ipptool
		cmd := exec.CommandContext(ctx, "ipptool", "-T", "2", "-tv", ippURI, "get-printer-attributes.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipptool query failed: %v (%s)", err, string(out))
		}

		ippHealth := ParseIPPPrinterState(out, "XeroxGoEmuTest")
		if ippHealth.State != IPPPrinterStateIdle {
			t.Errorf("IPP State = %q, want idle", ippHealth.State)
		}
		if len(ippHealth.RawReasons) != 0 {
			t.Errorf("IPP RawReasons = %v, want empty", ippHealth.RawReasons)
		}
		if ippHealth.AcceptingJobs != AcceptingJobsAccepting {
			t.Errorf("IPP AcceptingJobs = %v, want ACCEPTING", ippHealth.AcceptingJobs)
		}

		// Real SNMP query
		snmpHealth := PollHealthConfig(ctx, snmpCfg, 250)
		if !snmpHealth.Online || snmpHealth.State != "IDLE" {
			t.Fatalf("SNMP expected Online=true State=IDLE, got Online=%v State=%s", snmpHealth.Online, snmpHealth.State)
		}
		if snmpHealth.PaperJam || snmpHealth.PaperTrayStatus != "OK" {
			t.Errorf("SNMP expected no faults, got Jam=%v Tray=%s", snmpHealth.PaperJam, snmpHealth.PaperTrayStatus)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults, got %v", res.BlockingFaults)
		}
		if !res.IsReady() {
			t.Errorf("expected IsReady = true")
		}
	})

	// 2. Paper-Jam Scenario
	t.Run("Scenario_PaperJam", func(t *testing.T) {
		if out, err := runEmu(t, emuScript, emuEnv, "fault-set", "paper-jam"); err != nil {
			t.Fatalf("fault-set paper-jam failed: %v (%s)", err, out)
		}

		evalTime := time.Now()
		freshTime := evalTime.Add(-1 * time.Second)

		// Real IPP query via ipptool
		cmd := exec.CommandContext(ctx, "ipptool", "-T", "2", "-tv", ippURI, "get-printer-attributes.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipptool query failed: %v (%s)", err, string(out))
		}

		ippHealth := ParseIPPPrinterState(out, "XeroxGoEmuTest")
		if !ippHealth.PaperJam {
			t.Errorf("expected IPP PaperJam = true, got %v (reasons: %v)", ippHealth.PaperJam, ippHealth.RawReasons)
		}

		// Real SNMP query
		snmpHealth := PollHealthConfig(ctx, snmpCfg, 250)
		if !snmpHealth.Online || !snmpHealth.PaperJam {
			t.Fatalf("SNMP expected Online=true PaperJam=true, got Online=%v PaperJam=%v State=%s",
				snmpHealth.Online, snmpHealth.PaperJam, snmpHealth.State)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if !res.PaperJam {
			t.Errorf("expected reconciled PaperJam = true")
		}
		if !res.HasBlockingFaults() {
			t.Errorf("expected HasBlockingFaults = true")
		}
		if res.IsReady() {
			t.Errorf("expected IsReady = false")
		}
	})

	// 3. Paper-Empty Scenario
	t.Run("Scenario_PaperEmpty", func(t *testing.T) {
		if out, err := runEmu(t, emuScript, emuEnv, "fault-set", "paper-empty"); err != nil {
			t.Fatalf("fault-set paper-empty failed: %v (%s)", err, out)
		}

		evalTime := time.Now()
		freshTime := evalTime.Add(-1 * time.Second)

		// Real IPP query via ipptool
		cmd := exec.CommandContext(ctx, "ipptool", "-T", "2", "-tv", ippURI, "get-printer-attributes.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipptool query failed: %v (%s)", err, string(out))
		}

		ippHealth := ParseIPPPrinterState(out, "XeroxGoEmuTest")
		if !ippHealth.MediaEmpty {
			t.Errorf("expected IPP MediaEmpty = true, got %v (reasons: %v)", ippHealth.MediaEmpty, ippHealth.RawReasons)
		}

		// Real SNMP query
		snmpHealth := PollHealthConfig(ctx, snmpCfg, 250)
		if !snmpHealth.Online || snmpHealth.PaperTrayStatus != "EMPTY" {
			t.Fatalf("SNMP expected Online=true PaperTrayStatus=EMPTY, got Online=%v Tray=%v State=%s",
				snmpHealth.Online, snmpHealth.PaperTrayStatus, snmpHealth.State)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.EffectiveState != "ERROR" {
			t.Errorf("EffectiveState = %q, want ERROR", res.EffectiveState)
		}
		if res.PaperTrayStatus != "EMPTY" {
			t.Errorf("expected reconciled PaperTrayStatus = EMPTY, got %q", res.PaperTrayStatus)
		}
		if !res.HasBlockingFaults() {
			t.Errorf("expected HasBlockingFaults = true")
		}
		if res.IsReady() {
			t.Errorf("expected IsReady = false")
		}
	})

	// 4. Offline Scenario
	t.Run("Scenario_Offline", func(t *testing.T) {
		if out, err := runEmu(t, emuScript, emuEnv, "fault-set", "offline"); err != nil {
			t.Fatalf("fault-set offline failed: %v (%s)", err, out)
		}

		evalTime := time.Now()
		freshTime := evalTime.Add(-1 * time.Second)

		// Real IPP query via ipptool with short timeout (expected to fail/time out due to SIGSTOP)
		shortCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		cmd := exec.CommandContext(shortCtx, "ipptool", "-T", "1", "-tv", ippURI, "get-printer-attributes.test")
		out, err := cmd.CombinedOutput()
		cancel()

		var ippHealth IPPPrinterStateResult
		if err != nil {
			ippHealth = IPPPrinterStateResult{
				Source:            CapabilitySourceCUPSQueue,
				Queue:             "XeroxGoEmuTest",
				EndpointReachable: false,
				QueueAccessible:   false,
				ObservedAt:        freshTime,
				Error:             err.Error(),
			}
		} else {
			ippHealth = ParseIPPPrinterState(out, "XeroxGoEmuTest")
		}

		if ippHealth.EndpointReachable {
			t.Errorf("expected IPP EndpointReachable = false during offline")
		}

		// Real SNMP query (packets dropped)
		snmpHealth := PollHealthConfig(ctx, snmpCfg, 250)
		if snmpHealth.Online {
			t.Errorf("expected SNMP Online = false during offline packet drop")
		}

		cupsHealth := contracts.PrinterHealth{
			Online:    false,
			State:     "OFFLINE",
			UpdatedAt: freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if res.Online {
			t.Errorf("expected reconciled Online = false")
		}
		if res.EffectiveState != "OFFLINE" {
			t.Errorf("EffectiveState = %q, want OFFLINE", res.EffectiveState)
		}
		if res.IsReady() {
			t.Errorf("expected IsReady = false")
		}
	})

	// 5. Recovery Scenario
	t.Run("Scenario_Recovery", func(t *testing.T) {
		if out, err := runEmu(t, emuScript, emuEnv, "fault-set", "recovery"); err != nil {
			t.Fatalf("fault-set recovery failed: %v (%s)", err, out)
		}

		evalTime := time.Now()
		freshTime := evalTime.Add(-1 * time.Second)

		// Real IPP query via ipptool
		cmd := exec.CommandContext(ctx, "ipptool", "-T", "2", "-tv", ippURI, "get-printer-attributes.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipptool query failed after recovery: %v (%s)", err, string(out))
		}

		ippHealth := ParseIPPPrinterState(out, "XeroxGoEmuTest")
		if ippHealth.State != IPPPrinterStateIdle {
			t.Errorf("IPP State = %q, want idle after recovery", ippHealth.State)
		}
		if len(ippHealth.RawReasons) != 0 {
			t.Errorf("IPP RawReasons = %v, want empty after recovery", ippHealth.RawReasons)
		}

		// Real SNMP query
		snmpHealth := PollHealthConfig(ctx, snmpCfg, 250)
		if !snmpHealth.Online || snmpHealth.State != "IDLE" {
			t.Fatalf("expected SNMP recovered Online=true State=IDLE, got Online=%v State=%s",
				snmpHealth.Online, snmpHealth.State)
		}
		if snmpHealth.PaperJam || snmpHealth.PaperTrayStatus != "OK" {
			t.Errorf("expected zero error flags after recovery, got Jam=%v Tray=%v",
				snmpHealth.PaperJam, snmpHealth.PaperTrayStatus)
		}

		cupsHealth := contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: 200,
			UpdatedAt:       freshTime,
		}

		res := ReconcilePrinterHealth(cupsHealth, ippHealth, snmpHealth, evalTime)
		if !res.Online {
			t.Errorf("expected reconciled Online = true")
		}
		if res.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE", res.EffectiveState)
		}
		if res.HasBlockingFaults() {
			t.Errorf("expected no blocking faults after recovery, got %v", res.BlockingFaults)
		}
		if !res.IsReady() {
			t.Errorf("expected IsReady = true")
		}
	})
}
