package spooler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

func createTestClient(t *testing.T, cfg ClientConfig) (*Client, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}

	if cfg.CloudURL == "" {
		cfg.CloudURL = "ws://127.0.0.1:8080/ws"
	}
	if cfg.KioskID == "" {
		cfg.KioskID = "kiosk-shadow-test"
	}
	if cfg.StateStore == nil {
		cfg.StateStore = store
	}
	if cfg.DownloadDir == "" {
		cfg.DownloadDir = tmpDir
	}
	if cfg.IdleQRContent == "" {
		cfg.IdleQRContent = "https://kiosk-shadow"
	}
	if cfg.ScreenSink == nil {
		cfg.ScreenSink = noopSink
	}
	if cfg.CUPSRunner == nil {
		cfg.CUPSRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(""), nil
		}
	}

	client, err := NewClient(cfg)
	if err != nil {
		store.Close()
		t.Fatalf("NewClient failed: %v", err)
	}

	cleanup := func() {
		store.Close()
	}
	return client, cleanup
}

func TestShadowHealth_DisabledByDefault(t *testing.T) {
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue: "TestQueue",
	})
	defer cleanup()

	if client.cfg.EnableShadowHealth {
		t.Fatal("expected EnableShadowHealth to be false by default")
	}

	if report := client.LatestShadowReport(); report != nil {
		t.Fatalf("expected LatestShadowReport() to be nil initially, got %+v", report)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = client.Run(ctx)

	if report := client.LatestShadowReport(); report != nil {
		t.Fatalf("expected LatestShadowReport() to remain nil when disabled, got %+v", report)
	}
}

func TestShadowHealth_RawPJLMode_Skipped(t *testing.T) {
	var cupsCalled, snmpCalled, ippCalled atomic.Bool

	client, cleanup := createTestClient(t, ClientConfig{
		PrinterAddr:        "192.168.1.100:9100",
		CUPSQueue:          "", // Raw PJL mode (empty queue)
		EnableShadowHealth: true,
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			cupsCalled.Store(true)
			return contracts.PrinterHealth{Online: true, State: "IDLE"}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			snmpCalled.Store(true)
			return contracts.PrinterHealth{Online: true, State: "IDLE"}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			ippCalled.Store(true)
			return printer.IPPPrinterStateResult{State: printer.IPPPrinterStateIdle}, nil
		},
	})
	defer cleanup()

	report := client.RunShadowHealthCheck(context.Background())
	if report != nil {
		t.Fatalf("expected nil report for raw PJL mode (empty CUPS queue), got %+v", report)
	}

	if cupsCalled.Load() || snmpCalled.Load() || ippCalled.Load() {
		t.Fatalf("expected no external queries in raw PJL mode, got cups=%t, snmp=%t, ipp=%t",
			cupsCalled.Load(), snmpCalled.Load(), ippCalled.Load())
	}
}

func TestShadowHealth_HealthyObservations(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				UpdatedAt:       now,
			}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				UpdatedAt:       now,
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:            true,
				State:             "IDLE",
				PaperTrayStatus:   "OK",
				TonerLevelPercent: 85,
				DrumLifePercent:   90,
				UpdatedAt:         now,
			}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateIdle,
				AcceptingJobs:     printer.AcceptingJobsAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        now,
			}, nil
		},
	})
	defer cleanup()

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	if report.CUPSQueue != "BrotherDCP" {
		t.Errorf("expected CUPSQueue 'BrotherDCP', got %q", report.CUPSQueue)
	}
	if report.Reconciled.EffectiveState != "IDLE" {
		t.Errorf("expected EffectiveState 'IDLE', got %q", report.Reconciled.EffectiveState)
	}
	if !report.Reconciled.Online {
		t.Error("expected Reconciled.Online to be true")
	}
	if len(report.Reconciled.BlockingFaults) > 0 {
		t.Errorf("expected no blocking faults, got %v", report.Reconciled.BlockingFaults)
	}
	if report.Differences.HasDifferences() {
		t.Errorf("expected no differences for healthy matching sources, got %+v", report.Differences)
	}
	if report.CUPSSample.Stale || !report.CUPSSample.Available {
		t.Errorf("expected CUPS sample valid and available, got %+v", report.CUPSSample)
	}
	if report.SNMPSample.Stale || !report.SNMPSample.Available {
		t.Errorf("expected SNMP sample valid and available, got %+v", report.SNMPSample)
	}
	if report.IPPSample.Stale || !report.IPPSample.Available {
		t.Errorf("expected IPP sample valid and available, got %+v", report.IPPSample)
	}

	// Verify production state is completely unaffected and healthy
	if client.isHardwareFaultActive() {
		t.Error("expected hardware fault to be false")
	}
}

func TestShadowHealth_ConflictingSources_DivergenceDetected(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			// Production poller sees healthy queue
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				UpdatedAt:       now,
			}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				UpdatedAt:       now,
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				UpdatedAt:       now,
			}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			// IPP reveals paper jam fault
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateStopped,
				AcceptingJobs:     printer.AcceptingJobsNotAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				HasHardwareFault:  true,
				PaperJam:          true,
				RawReasons:        []string{"media-jam-error"},
				ParsedReasons: []printer.IPPPrinterReason{
					{
						Raw:      "media-jam-error",
						Keyword:  "media-jam",
						Severity: printer.SeverityError,
					},
				},
				ObservedAt: now,
			}, nil
		},
	})
	defer cleanup()

	// Initial production health check sets baseline healthy
	prodHealth := client.CheckPrinterHealth(context.Background())
	if !prodHealth.Online || client.isHardwareFaultActive() {
		t.Fatal("expected production to be initially healthy")
	}

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	// Shadow reconciliation should detect the critical paper jam
	if report.Reconciled.EffectiveState != "ERROR" {
		t.Errorf("expected reconciled EffectiveState ERROR, got %q", report.Reconciled.EffectiveState)
	}
	if !report.Reconciled.HasBlockingFaults() {
		t.Fatal("expected reconciled blocking faults to be present")
	}
	if !report.Reconciled.PaperJam {
		t.Error("expected Reconciled.PaperJam to be true")
	}
	hasJam := false
	for _, f := range report.Reconciled.BlockingFaults {
		if f == "paper jam" || f == printer.FaultPaperJam {
			hasJam = true
			break
		}
	}
	if !hasJam {
		t.Errorf("expected 'paper jam' in blocking faults, got %v", report.Reconciled.BlockingFaults)
	}

	// Differences between shadow and production should be detected and attributed
	if !report.Differences.FaultDiff {
		t.Error("expected Differences.FaultDiff to be true")
	}
	if !report.Differences.StateDiff {
		t.Error("expected Differences.StateDiff to be true")
	}
	if len(report.Differences.Details) == 0 {
		t.Error("expected discrepancy details to be logged")
	}

	// Contradiction between IPP fault and CUPS idle queue should be recorded
	if len(report.Reconciled.Contradictions) == 0 {
		t.Error("expected contradictions to be recorded")
	}

	// CRITICAL ZERO-EFFECT CHECK:
	// Shadow reconciliation MUST NOT alter production hardwareFault or faultReason!
	if client.isHardwareFaultActive() {
		t.Error("CRITICAL: shadow mode set hardwareFault in production!")
	}
	if client.getFaultReason() != "" {
		t.Errorf("CRITICAL: shadow mode set faultReason in production: %q", client.getFaultReason())
	}
	for _, screen := range client.RenderHistory() {
		if screen.Mode == ui_ipc.ModeError {
			t.Errorf("CRITICAL: shadow mode triggered ModeError screen in production render history: %+v", screen)
		}
	}
}

func TestShadowHealth_StaleObservations(t *testing.T) {
	evalTime := time.Now().UTC()
	staleTimestamp := evalTime.Add(-45 * time.Second) // 45s > MaxIPPObservationAge (30s)

	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: evalTime}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: staleTimestamp,
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: staleTimestamp,
			}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateStopped,
				HasHardwareFault:  true,
				PaperJam:          true,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        staleTimestamp,
			}, nil
		},
	})
	defer cleanup()

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	// Verify staleness is explicitly captured and reported
	if !report.CUPSSample.Stale {
		t.Error("expected CUPS sample to be marked stale")
	}
	if report.CUPSSample.Available {
		t.Error("expected stale CUPS sample to not be available as current evidence")
	}

	if !report.SNMPSample.Stale {
		t.Error("expected SNMP sample to be marked stale")
	}
	if report.SNMPSample.Available {
		t.Error("expected stale SNMP sample to not be available as current evidence")
	}

	if !report.IPPSample.Stale {
		t.Error("expected IPP sample to be marked stale")
	}
	if report.IPPSample.Available {
		t.Error("expected stale IPP sample to not be available as current evidence")
	}

	// Reconciled health must reject stale IPP fault evidence
	hasStaleWarning := false
	for _, w := range report.Reconciled.NonblockingWarnings {
		if strings.Contains(strings.ToLower(w), "stale") {
			hasStaleWarning = true
			break
		}
	}
	for _, d := range report.Reconciled.DiagnosticDetails {
		if d.Code == printer.WarningStaleIPP {
			hasStaleWarning = true
			break
		}
	}
	if !hasStaleWarning {
		t.Errorf("expected stale IPP warning, got warnings=%v details=%+v", report.Reconciled.NonblockingWarnings, report.Reconciled.DiagnosticDetails)
	}

	// Zero effect on production state
	if client.isHardwareFaultActive() {
		t.Error("expected production hardwareFault to remain false")
	}
}

func TestShadowHealth_MissingTimestamps(t *testing.T) {
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE"}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: time.Time{}, // zero timestamp
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: time.Time{}, // zero timestamp
			}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateIdle,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        time.Time{}, // zero timestamp
			}, nil
		},
	})
	defer cleanup()

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	if !report.CUPSSample.Stale || report.CUPSSample.Available {
		t.Errorf("expected CUPS with zero timestamp to be stale and unavailable: %+v", report.CUPSSample)
	}
	if !report.SNMPSample.Stale || report.SNMPSample.Available {
		t.Errorf("expected SNMP with zero timestamp to be stale and unavailable: %+v", report.SNMPSample)
	}
	if !report.IPPSample.Stale || report.IPPSample.Available {
		t.Errorf("expected IPP with zero timestamp to be stale and unavailable: %+v", report.IPPSample)
	}
}

func TestShadowHealth_QueryFailures_Resilience(t *testing.T) {
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: time.Now().UTC()}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:       false,
				State:        "UNKNOWN",
				ErrorMessage: "lpstat: command not found",
				UpdatedAt:    time.Now().UTC(),
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:       false,
				State:        "UNKNOWN",
				ErrorMessage: "snmp: connection refused",
				UpdatedAt:    time.Now().UTC(),
			}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateUnknown,
				EndpointReachable: false,
				QueueAccessible:   false,
				ObservedAt:        time.Now().UTC(),
				Error:             "ipptool: server unreachable",
			}, errors.New("ipptool: server unreachable")
		},
	})
	defer cleanup()

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport despite query failures")
	}

	if len(report.QueryErrors) != 3 {
		t.Errorf("expected 3 query errors captured, got %d: %v", len(report.QueryErrors), report.QueryErrors)
	}

	// Daemon must not crash, and production health checking must continue normally
	h := client.CheckPrinterHealth(context.Background())
	if !h.Online || h.State != "IDLE" {
		t.Errorf("expected production health check to remain healthy and unaffected, got %+v", h)
	}
}

func TestShadowHealth_ContextCancellation(t *testing.T) {
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	report := client.RunShadowHealthCheck(ctx)
	if report != nil {
		t.Fatalf("expected nil report for canceled context, got %+v", report)
	}
}

func TestShadowHealth_ZeroEffectOnActivePrinting(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "PRINTING", UpdatedAt: now}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "PRINTING", UpdatedAt: now}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "PRINTING", UpdatedAt: now}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateStopped,
				EndpointReachable: true,
				QueueAccessible:   true,
				HasHardwareFault:  true,
				PaperJam:          true,
				RawReasons:        []string{"media-jam-error"},
				ParsedReasons: []printer.IPPPrinterReason{
					{
						Raw:      "media-jam-error",
						Keyword:  "media-jam",
						Severity: printer.SeverityError,
					},
				},
				ObservedAt: now,
			}, nil
		},
	})
	defer cleanup()

	// Simulate an active print job in progress
	activeJobID := "job-active-shadow-test"
	client.activeMu.Lock()
	client.activeJobs[activeJobID] = true
	client.activeMu.Unlock()

	// Run shadow health check while active job is present
	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	// Shadow reconciliation detects fault
	if !report.Reconciled.HasBlockingFaults() {
		t.Error("expected shadow reconciliation to detect blocking fault")
	}

	// Active job MUST remain active and untouched
	client.activeMu.Lock()
	stillActive := client.activeJobs[activeJobID]
	client.activeMu.Unlock()

	if !stillActive {
		t.Errorf("CRITICAL: active job %s was removed or altered by shadow health check!", activeJobID)
	}

	if client.isHardwareFaultActive() {
		t.Error("CRITICAL: shadow health check altered hardwareFault during active print job")
	}
}

func TestShadowHealth_PeriodicLoopExecution(t *testing.T) {
	now := time.Now().UTC()
	var checkCount atomic.Int32

	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:            "BrotherDCP",
		EnableShadowHealth:   true,
		ShadowHealthInterval: 30 * time.Millisecond,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			checkCount.Add(1)
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceCUPSQueue,
				Queue:             queue,
				State:             printer.IPPPrinterStateIdle,
				AcceptingJobs:     printer.AcceptingJobsAccepting,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        now,
			}, nil
		},
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(ctx)
	}()

	// Wait for ticker to fire multiple times
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-runDone

	if client.LatestShadowReport() == nil {
		t.Fatal("expected LatestShadowReport() to be set by periodic shadow loop")
	}
	if checkCount.Load() < 2 {
		t.Errorf("expected at least 2 shadow checks during loop execution, got %d", checkCount.Load())
	}
}
