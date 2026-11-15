package printer

import (
	"context"
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
