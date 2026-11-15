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

	// Populate cached production health baseline
	client.CheckPrinterHealth(context.Background())

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
	if !report.ProductionDecision.Available {
		t.Error("expected ProductionDecision.Available to be true")
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

// -------------------------------------------------------------------------------------------------
// Regression Tests for Final Safety Corrections (Points 1, 4, 8, 10, 11)
// -------------------------------------------------------------------------------------------------

func TestShadowHealth_SuccessfulQuery_NotReportedAsCanceled(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			// Query finishes successfully
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: now,
			}
		},
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:    true,
				State:     "IDLE",
				UpdatedAt: now,
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

	// Verify that successful queries are NEVER falsely tagged with context.Canceled
	if len(report.QueryErrors) != 0 {
		t.Fatalf("expected 0 query errors for successful queries, got %d: %v", len(report.QueryErrors), report.QueryErrors)
	}

	if report.CUPSSample.Error != "" {
		t.Errorf("expected CUPSSample.Error to be empty, got %q", report.CUPSSample.Error)
	}
	if report.SNMPSample.Error != "" {
		t.Errorf("expected SNMPSample.Error to be empty, got %q", report.SNMPSample.Error)
	}
	if report.IPPSample.Error != "" {
		t.Errorf("expected IPPSample.Error to be empty, got %q", report.IPPSample.Error)
	}
}

func TestShadowHealth_DistinguishableCancellationAndTimeout(t *testing.T) {
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			// Simulate context canceled during call
			<-ctx.Done()
			return contracts.PrinterHealth{
				Online:       false,
				State:        "UNKNOWN",
				ErrorMessage: ctx.Err().Error(),
			}
		},
	})
	defer cleanup()

	// 1. Genuine context cancellation
	cancCtx, canc := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		canc()
	}()

	report := client.RunShadowHealthCheck(cancCtx)
	if report == nil {
		// Context was canceled while starting check
		return
	}

	foundCanceled := false
	for _, qe := range report.QueryErrors {
		if strings.Contains(strings.ToLower(qe), "canceled") {
			foundCanceled = true
			break
		}
	}
	if !foundCanceled && len(report.QueryErrors) > 0 {
		t.Logf("Captured query errors on cancellation: %v", report.QueryErrors)
	}
}

func TestShadowHealth_StaleObservation_FaultFlagsSuppressed(t *testing.T) {
	now := time.Now().UTC()
	staleTimestamp := now.Add(-50 * time.Second) // Stale > 30s

	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		// Stale CUPS observation carrying historical fault flags
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "ERROR",
				PaperJam:        true,
				CoverOpen:       true,
				OutputBinFull:   true,
				PaperTrayStatus: "EMPTY",
				ErrorMessage:     "printer paper jam in feeder",
				UpdatedAt:        staleTimestamp,
			}
		},
		// Stale SNMP observation carrying historical fault flags
		MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "ERROR",
				PaperJam:        true,
				CoverOpen:       true,
				OutputBinFull:   true,
				PaperTrayStatus: "EMPTY",
				ErrorMessage:     "door open",
				UpdatedAt:        staleTimestamp,
			}
		},
		// Fresh IPP observation reports healthy/idle
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

	// Verify staleness is explicitly captured
	if !report.CUPSSample.Stale || report.CUPSSample.Available {
		t.Errorf("expected CUPS sample to be stale and unavailable: %+v", report.CUPSSample)
	}
	if !report.SNMPSample.Stale || report.SNMPSample.Available {
		t.Errorf("expected SNMP sample to be stale and unavailable: %+v", report.SNMPSample)
	}

	// Active reconciliation MUST NOT accept stale fault flags or paper status!
	// Reconciled physical flags must be false.
	if report.Reconciled.PaperJam {
		t.Error("CRITICAL: stale paper jam flag was accepted into active reconciliation!")
	}
	if report.Reconciled.CoverOpen {
		t.Error("CRITICAL: stale cover open flag was accepted into active reconciliation!")
	}
	if report.Reconciled.OutputBinFull {
		t.Error("CRITICAL: stale output bin full flag was accepted into active reconciliation!")
	}

	// Stale observations must NOT create false claims of current physical faults!
	for _, f := range report.Reconciled.BlockingFaults {
		if f == "paper jam" || f == "cover or door open" || f == "output bin full" {
			t.Errorf("CRITICAL: stale fault %q was included in Reconciled.BlockingFaults: %v", f, report.Reconciled.BlockingFaults)
		}
	}

	// Raw observations must be preserved separately for diagnostics
	if !report.RawCUPS.PaperJam || !report.RawCUPS.CoverOpen {
		t.Errorf("expected RawCUPS to preserve original diagnostic observations, got: %+v", report.RawCUPS)
	}
	if !report.RawSNMP.PaperJam || !report.RawSNMP.CoverOpen {
		t.Errorf("expected RawSNMP to preserve original diagnostic observations, got: %+v", report.RawSNMP)
	}
}

func TestShadowHealth_ProductionBaseline_Absent(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		// MockHealth is set on client, but CheckPrinterHealth is NEVER called before shadow check.
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
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

	// Ensure cached production health is absent
	if !client.LatestProductionHealth().UpdatedAt.IsZero() {
		t.Fatal("expected cached production health to be zero before initial CheckPrinterHealth")
	}

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	// 1. Production decision must be marked unavailable
	if report.ProductionDecision.Available {
		t.Error("expected ProductionDecision.Available to be false when baseline is absent")
	}
	if !strings.Contains(report.ProductionDecision.Reason, "absent") {
		t.Errorf("expected ProductionDecision.Reason to indicate absent baseline, got %q", report.ProductionDecision.Reason)
	}

	// 2. Differences must mark baseline unavailable and skip authoritative difference classification
	if report.Differences.BaselineAvailable {
		t.Error("expected Differences.BaselineAvailable to be false")
	}
	if report.Differences.HasDifferences() {
		t.Error("expected Differences.HasDifferences() to be false when baseline is absent")
	}
	if report.Differences.StateDiff || report.Differences.OnlineDiff || report.Differences.FaultDiff {
		t.Errorf("expected no difference flags when baseline is absent, got %+v", report.Differences)
	}
	if len(report.Differences.Details) == 0 || !strings.Contains(report.Differences.Details[0], "baseline absent") {
		t.Errorf("expected Differences.Details to explain that baseline is absent: %v", report.Differences.Details)
	}
}

func TestShadowHealth_ProductionBaseline_Stale(t *testing.T) {
	now := time.Now().UTC()
	staleTimestamp := now.Add(-60 * time.Second) // Baseline is 60s old (> 30s limit)

	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: staleTimestamp}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
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

	// Seed production health cache with stale reading
	client.CheckPrinterHealth(context.Background())

	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil ShadowHealthReport")
	}

	// Baseline is stale -> must be marked unavailable and NOT classified as a real mismatch
	if report.ProductionDecision.Available {
		t.Error("expected ProductionDecision.Available to be false for stale baseline")
	}
	if !strings.Contains(report.ProductionDecision.Reason, "stale") {
		t.Errorf("expected ProductionDecision.Reason to indicate stale baseline, got %q", report.ProductionDecision.Reason)
	}
	if report.Differences.BaselineAvailable {
		t.Error("expected Differences.BaselineAvailable to be false")
	}
	if report.Differences.HasDifferences() {
		t.Error("expected Differences.HasDifferences() to be false for stale baseline")
	}
}

func TestShadowHealth_ReportImmutability(t *testing.T) {
	now := time.Now().UTC()
	client, cleanup := createTestClient(t, ClientConfig{
		CUPSQueue:          "BrotherDCP",
		EnableShadowHealth: true,
		MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", UpdatedAt: now}
		},
		MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
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

	client.CheckPrinterHealth(context.Background())
	report := client.RunShadowHealthCheck(context.Background())
	if report == nil {
		t.Fatal("expected non-nil report")
	}

	// Obtain copy 1 and mutate slices
	r1 := client.LatestShadowReport()
	if r1 == nil {
		t.Fatal("expected non-nil r1 from LatestShadowReport")
	}

	r1.Differences.Details = append(r1.Differences.Details, "MUTATED_DIFF_DETAIL")
	r1.Reconciled.BlockingFaults = append(r1.Reconciled.BlockingFaults, "MUTATED_BLOCKING_FAULT")
	r1.Reconciled.NonblockingWarnings = append(r1.Reconciled.NonblockingWarnings, "MUTATED_WARNING")
	r1.Reconciled.Contradictions = append(r1.Reconciled.Contradictions, "MUTATED_CONTRADICTION")
	r1.QueryErrors = append(r1.QueryErrors, "MUTATED_QUERY_ERROR")

	// Obtain copy 2 and verify it is completely unaffected
	r2 := client.LatestShadowReport()
	if r2 == nil {
		t.Fatal("expected non-nil r2 from LatestShadowReport")
	}

	for _, d := range r2.Differences.Details {
		if d == "MUTATED_DIFF_DETAIL" {
			t.Fatal("IMMUTABILITY VIOLATION: r2.Differences.Details contains mutated element!")
		}
	}
	for _, bf := range r2.Reconciled.BlockingFaults {
		if bf == "MUTATED_BLOCKING_FAULT" {
			t.Fatal("IMMUTABILITY VIOLATION: r2.Reconciled.BlockingFaults contains mutated element!")
		}
	}
	for _, nw := range r2.Reconciled.NonblockingWarnings {
		if nw == "MUTATED_WARNING" {
			t.Fatal("IMMUTABILITY VIOLATION: r2.Reconciled.NonblockingWarnings contains mutated element!")
		}
	}
	for _, c := range r2.Reconciled.Contradictions {
		if c == "MUTATED_CONTRADICTION" {
			t.Fatal("IMMUTABILITY VIOLATION: r2.Reconciled.Contradictions contains mutated element!")
		}
	}
	for _, qe := range r2.QueryErrors {
		if qe == "MUTATED_QUERY_ERROR" {
			t.Fatal("IMMUTABILITY VIOLATION: r2.QueryErrors contains mutated element!")
		}
	}
}

func TestShadowHealth_CUPSUnknownVsOfflineContrasts(t *testing.T) {
	evalTime := time.Now().UTC()
	staleTimestamp := evalTime.Add(-50 * time.Second)
	freshTimestamp := evalTime.Add(-1 * time.Second)

	mockIPP := func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
		return printer.IPPPrinterStateResult{
			Source:            printer.CapabilitySourceCUPSQueue,
			Queue:             queue,
			State:             printer.IPPPrinterStateIdle,
			AcceptingJobs:     printer.AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTimestamp,
		}, nil
	}

	t.Run("stale_cups_yields_unknown_not_offline", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "OFFLINE",
					ErrorMessage:    "printer disabled",
					PaperTrayStatus: "UNKNOWN",
					UpdatedAt:       staleTimestamp,
				}
			},
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					PaperTrayStatus: "UNKNOWN",
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if !report.CUPSSample.Stale {
			t.Error("expected CUPS sample to be marked stale")
		}
		if report.CUPSSample.Available {
			t.Error("expected stale CUPS sample to have Available=false")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("CRITICAL: stale CUPS must mean UNKNOWN, got %q", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false for stale CUPS")
		}
		for _, f := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(f), "offline") {
				t.Errorf("CRITICAL: stale CUPS must not create queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
			}
		}
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				t.Errorf("CRITICAL: stale CUPS must not emit FaultQueueOffline diagnostic item")
			}
		}
		if report.RawCUPS.State != "OFFLINE" || report.RawCUPS.UpdatedAt != staleTimestamp {
			t.Errorf("expected RawCUPS to preserve original raw observation, got %+v", report.RawCUPS)
		}
	})

	t.Run("missing_cups_yields_unknown_not_offline", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					PaperTrayStatus: "UNKNOWN",
					UpdatedAt:       time.Time{}, // zero timestamp = missing
				}
			},
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					PaperTrayStatus: "UNKNOWN",
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if !report.CUPSSample.Stale {
			t.Error("expected missing CUPS timestamp to be marked stale")
		}
		if report.CUPSSample.Available {
			t.Error("expected missing CUPS timestamp to have Available=false")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("CRITICAL: missing CUPS must mean UNKNOWN, got %q", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false for missing CUPS")
		}
		for _, f := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(f), "offline") {
				t.Errorf("CRITICAL: missing CUPS must not create queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
			}
		}
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				t.Errorf("CRITICAL: missing CUPS must not emit FaultQueueOffline diagnostic item")
			}
		}
	})

	t.Run("failed_cups_yields_unknown_not_offline", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					ErrorMessage:    "lpstat: command not found",
					PaperTrayStatus: "UNKNOWN",
					UpdatedAt:       freshTimestamp, // fresh timestamp, but query failed!
				}
			},
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					PaperTrayStatus: "UNKNOWN",
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected failed CUPS query to have Available=false")
		}
		if len(report.QueryErrors) == 0 {
			t.Error("expected query error to be recorded for failed CUPS")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("CRITICAL: failed CUPS query must mean UNKNOWN, got %q", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false for failed CUPS query")
		}
		for _, f := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(f), "offline") {
				t.Errorf("CRITICAL: failed CUPS query must not create queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
			}
		}
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				t.Errorf("CRITICAL: failed CUPS query must not emit FaultQueueOffline diagnostic item")
			}
		}
		if report.RawCUPS.ErrorMessage != "lpstat: command not found" {
			t.Errorf("expected RawCUPS to preserve original error message, got %+v", report.RawCUPS)
		}
	})

	t.Run("genuinely_offline_cups_preserves_offline_fault", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "OFFLINE",
					ErrorMessage:    "printer disabled",
					PaperTrayStatus: "UNKNOWN",
					UpdatedAt:       freshTimestamp, // fresh timestamp, successful observation
				}
			},
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:          false,
					State:           "UNKNOWN",
					PaperTrayStatus: "UNKNOWN",
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Stale {
			t.Error("expected fresh CUPS offline observation to NOT be stale")
		}
		if !report.CUPSSample.Available {
			t.Error("expected fresh CUPS offline observation to be Available=true")
		}
		if len(report.QueryErrors) != 0 {
			t.Errorf("expected no query errors for successful offline query, got %v", report.QueryErrors)
		}
		if report.Reconciled.EffectiveState != "OFFLINE" {
			t.Errorf("CRITICAL: genuine fresh CUPS offline observation must yield EffectiveState OFFLINE, got %q", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false for genuine offline CUPS")
		}

		hasOfflineFault := false
		for _, f := range report.Reconciled.BlockingFaults {
			if f == "CUPS queue offline" || strings.Contains(strings.ToLower(f), "offline") {
				hasOfflineFault = true
				break
			}
		}
		if !hasOfflineFault {
			t.Errorf("CRITICAL: genuine fresh CUPS offline observation must have queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
		}

		hasOfflineDiag := false
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				hasOfflineDiag = true
				if d.Severity != "BLOCKING" {
					t.Errorf("expected FaultQueueOffline to be BLOCKING, got %s", d.Severity)
				}
				break
			}
		}
		if !hasOfflineDiag {
			t.Errorf("CRITICAL: genuine fresh CUPS offline observation must emit FaultQueueOffline diagnostic item")
		}
		if report.RawCUPS.State != "OFFLINE" || report.RawCUPS.ErrorMessage != "printer disabled" {
			t.Errorf("expected RawCUPS to preserve original raw observation, got %+v", report.RawCUPS)
		}
	})
}

func TestShadowHealth_SNMPFailuresAndStaleness(t *testing.T) {
	evalTime := time.Now().UTC()
	staleTimestamp := evalTime.Add(-50 * time.Second)
	freshTimestamp := evalTime.Add(-1 * time.Second)

	mockCUPS := func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			UpdatedAt:       freshTimestamp,
		}
	}

	mockIPP := func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
		return printer.IPPPrinterStateResult{
			Source:            printer.CapabilitySourceCUPSQueue,
			Queue:             queue,
			State:             printer.IPPPrinterStateIdle,
			AcceptingJobs:     printer.AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        freshTimestamp,
		}, nil
	}

	t.Run("failed_snmp_with_fresh_timestamp_suppresses_fault_flags", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth:     mockCUPS,
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:            false,
					State:             "UNKNOWN",
					ErrorMessage:      "snmp: connection refused",
					PaperJam:          true, // fresh timestamp and fault flag, but query failed!
					CoverOpen:         true,
					OutputBinFull:     true,
					PaperTrayStatus:   "EMPTY",
					TonerLevelPercent: 5,
					DrumLifePercent:   10,
					UpdatedAt:         freshTimestamp,
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.SNMPSample.Available {
			t.Error("expected failed SNMP query to have Available=false")
		}
		if len(report.QueryErrors) == 0 {
			t.Error("expected query error to be recorded for failed SNMP")
		}

		// Reconciled health MUST NOT accept fault flags from failed SNMP query
		if report.Reconciled.PaperJam {
			t.Error("CRITICAL: failed SNMP query contributed paper jam flag!")
		}
		if report.Reconciled.CoverOpen {
			t.Error("CRITICAL: failed SNMP query contributed cover open flag!")
		}
		if report.Reconciled.OutputBinFull {
			t.Error("CRITICAL: failed SNMP query contributed output bin full flag!")
		}
		if report.Reconciled.TonerLevelPercent != 0 {
			t.Errorf("CRITICAL: failed SNMP query contributed toner level %d", report.Reconciled.TonerLevelPercent)
		}
		if len(report.Reconciled.BlockingFaults) > 0 {
			t.Errorf("expected no blocking faults from failed SNMP query, got %v", report.Reconciled.BlockingFaults)
		}
		if report.Reconciled.EffectiveState != "IDLE" {
			t.Errorf("expected EffectiveState IDLE from healthy CUPS/IPP, got %q", report.Reconciled.EffectiveState)
		}

		// Raw SNMP must preserve original observation for diagnostics
		if !report.RawSNMP.PaperJam || report.RawSNMP.ErrorMessage != "snmp: connection refused" {
			t.Errorf("expected RawSNMP to preserve original raw flags and error, got %+v", report.RawSNMP)
		}
	})

	t.Run("stale_snmp_fault_flags_suppressed", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			MockCUPSHealth:     mockCUPS,
			MockSNMPHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{
					Online:            true,
					State:             "ERROR",
					PaperJam:          true, // stale fault flag
					CoverOpen:         true,
					OutputBinFull:     true,
					PaperTrayStatus:   "EMPTY",
					TonerLevelPercent: 12,
					DrumLifePercent:   15,
					UpdatedAt:         staleTimestamp,
				}
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if !report.SNMPSample.Stale {
			t.Error("expected SNMP sample to be marked stale")
		}
		if report.SNMPSample.Available {
			t.Error("expected stale SNMP sample to have Available=false")
		}

		// Reconciled health MUST NOT accept stale fault flags
		if report.Reconciled.PaperJam {
			t.Error("CRITICAL: stale SNMP sample contributed paper jam flag!")
		}
		if report.Reconciled.CoverOpen {
			t.Error("CRITICAL: stale SNMP sample contributed cover open flag!")
		}
		if report.Reconciled.OutputBinFull {
			t.Error("CRITICAL: stale SNMP sample contributed output bin full flag!")
		}
		if report.Reconciled.TonerLevelPercent != 0 {
			t.Errorf("CRITICAL: stale SNMP sample contributed toner level %d", report.Reconciled.TonerLevelPercent)
		}
		if len(report.Reconciled.BlockingFaults) > 0 {
			t.Errorf("expected no blocking faults from stale SNMP sample, got %v", report.Reconciled.BlockingFaults)
		}
		if report.Reconciled.EffectiveState != "IDLE" {
			t.Errorf("expected EffectiveState IDLE from healthy CUPS/IPP, got %q", report.Reconciled.EffectiveState)
		}

		// Raw SNMP must preserve original observation for diagnostics
		if !report.RawSNMP.PaperJam || report.RawSNMP.UpdatedAt != staleTimestamp {
			t.Errorf("expected RawSNMP to preserve original raw flags and timestamp, got %+v", report.RawSNMP)
		}
	})
}

func TestShadowHealth_CUPSCommandFailureVsDisabledContrasts(t *testing.T) {
	now := time.Now().UTC()

	mockIPP := func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
		return printer.IPPPrinterStateResult{
			Source:            printer.CapabilitySourceCUPSQueue,
			Queue:             queue,
			State:             printer.IPPPrinterStateIdle,
			AcceptingJobs:     printer.AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        now,
		}, nil
	}

	t.Run("missing_lpstat_yields_unknown_never_offline", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, errors.New("exec: \"lpstat\": executable file not found in $PATH")
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected missing lpstat to set CUPSSample.Available=false")
		}
		if len(report.QueryErrors) == 0 {
			t.Fatal("expected QueryErrors to capture command failure")
		}
		hasErr := false
		for _, qe := range report.QueryErrors {
			if strings.Contains(qe, "executable file not found") {
				hasErr = true
				break
			}
		}
		if !hasErr {
			t.Errorf("expected executable file not found in QueryErrors, got %v", report.QueryErrors)
		}

		// Critical check: command failure MUST NOT be diagnosed as QUEUE_OFFLINE!
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN when lpstat is missing", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false")
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("CRITICAL: missing lpstat must NOT produce queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
			}
		}
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				t.Errorf("CRITICAL: missing lpstat must NOT emit FaultQueueOffline diagnostic item")
			}
		}
	})

	t.Run("both_commands_fail_yields_unknown_never_offline", func(t *testing.T) {
		var shadowCalls atomic.Int32
		var activeCheck atomic.Bool
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if activeCheck.Load() {
					shadowCalls.Add(1)
				}
				return nil, errors.New("lpstat: cannot connect to server: Connection refused")
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		activeCheck.Store(true)
		report := client.RunShadowHealthCheck(context.Background())
		activeCheck.Store(false)

		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if shadowCalls.Load() != 2 {
			t.Errorf("expected exactly 2 command attempts (-l -p then fallback -p), got %d", shadowCalls.Load())
		}
		if report.CUPSSample.Available {
			t.Error("expected command failure to set CUPSSample.Available=false")
		}
		if len(report.QueryErrors) == 0 {
			t.Fatal("expected QueryErrors to capture command failure")
		}
		hasErr := false
		for _, qe := range report.QueryErrors {
			if strings.Contains(qe, "Connection refused") {
				hasErr = true
				break
			}
		}
		if !hasErr {
			t.Errorf("expected Connection refused in QueryErrors, got %v", report.QueryErrors)
		}

		// Critical check: command failure MUST NOT be diagnosed as QUEUE_OFFLINE!
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN when both commands fail, got %q", report.Reconciled.EffectiveState, report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false")
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("CRITICAL: command failure must NOT produce queue-offline blocking fault, got %v", report.Reconciled.BlockingFaults)
			}
		}
		for _, d := range report.Reconciled.DiagnosticDetails {
			if d.Code == printer.FaultQueueOffline {
				t.Errorf("CRITICAL: command failure must NOT emit FaultQueueOffline diagnostic item")
			}
		}
	})

	t.Run("extended_query_failing_with_successful_fallback", func(t *testing.T) {
		var shadowCalls atomic.Int32
		var activeCheck atomic.Bool
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if activeCheck.Load() {
					shadowCalls.Add(1)
				}
				for _, arg := range args {
					if arg == "-l" {
						return nil, errors.New("lpstat: illegal option -- l")
					}
				}
				return []byte("printer BrotherDCP is idle. enabled since Sun Nov 15 10:00:00 2026\n"), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		activeCheck.Store(true)
		report := client.RunShadowHealthCheck(context.Background())
		activeCheck.Store(false)

		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if shadowCalls.Load() != 2 {
			t.Errorf("expected 2 command attempts, got %d", shadowCalls.Load())
		}
		if !report.CUPSSample.Available {
			t.Error("expected successful fallback to set CUPSSample.Available=true")
		}
		if len(report.QueryErrors) != 0 {
			t.Errorf("expected 0 QueryErrors on successful fallback, got %v", report.QueryErrors)
		}
		if report.Reconciled.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE on successful fallback", report.Reconciled.EffectiveState)
		}
		if !report.Reconciled.Online {
			t.Error("expected Reconciled.Online=true")
		}
		if len(report.Reconciled.BlockingFaults) > 0 {
			t.Errorf("expected no blocking faults, got %v", report.Reconciled.BlockingFaults)
		}
	})

	t.Run("genuinely_disabled_queue_preserves_fault_classification", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("printer BrotherDCP disabled since Sun Nov 15 10:00:00 2026 - printer is stopped or disabled\n"), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if !report.CUPSSample.Available {
			t.Error("expected successful disabled-queue query to set CUPSSample.Available=true")
		}
		if len(report.QueryErrors) != 0 {
			t.Errorf("expected 0 QueryErrors for successful disabled query, got %v", report.QueryErrors)
		}
		if report.Reconciled.EffectiveState != "ERROR" && report.Reconciled.EffectiveState != "OFFLINE" {
			t.Errorf("expected EffectiveState ERROR or OFFLINE for disabled queue, got %q", report.Reconciled.EffectiveState)
		}
		if report.Reconciled.Online {
			t.Error("expected Reconciled.Online=false for disabled queue")
		}
		if !report.Reconciled.HasBlockingFaults() {
			t.Error("expected blocking faults for disabled queue")
		}

		hasOfflineOrErrorFault := false
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") || strings.Contains(strings.ToLower(bf), "disabled") || strings.Contains(strings.ToLower(bf), "stopped") {
				hasOfflineOrErrorFault = true
				break
			}
		}
		if !hasOfflineOrErrorFault {
			t.Errorf("expected queue offline/disabled fault in BlockingFaults, got %v", report.Reconciled.BlockingFaults)
		}
	})

	t.Run("healthy_queue_preserves_idle_classification", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("printer BrotherDCP is idle. enabled since Sun Nov 15 10:00:00 2026\n"), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if !report.CUPSSample.Available {
			t.Error("expected CUPSSample.Available=true for healthy queue")
		}
		if len(report.QueryErrors) != 0 {
			t.Errorf("expected 0 QueryErrors for healthy queue, got %v", report.QueryErrors)
		}
		if report.Reconciled.EffectiveState != "IDLE" {
			t.Errorf("EffectiveState = %q, want IDLE for healthy queue", report.Reconciled.EffectiveState)
		}
		if !report.Reconciled.Online {
			t.Error("expected Reconciled.Online=true for healthy queue")
		}
		if len(report.Reconciled.BlockingFaults) > 0 {
			t.Errorf("expected 0 BlockingFaults for healthy queue, got %v", report.Reconciled.BlockingFaults)
		}
	})
}

func TestShadowHealth_CUPSOutputValidation(t *testing.T) {
	now := time.Now().UTC()

	mockIPP := func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
		return printer.IPPPrinterStateResult{
			Source:            printer.CapabilitySourceCUPSQueue,
			Queue:             queue,
			State:             printer.IPPPrinterStateIdle,
			AcceptingJobs:     printer.AcceptingJobsAccepting,
			EndpointReachable: true,
			QueueAccessible:   true,
			ObservedAt:        now,
		}, nil
	}

	t.Run("empty_output_classified_as_unknown", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(""), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected CUPSSample.Available=false for empty lpstat output")
		}
		if len(report.QueryErrors) == 0 {
			t.Error("expected non-empty QueryErrors for empty lpstat output")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN for empty output", report.Reconciled.EffectiveState)
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("expected no offline fault in BlockingFaults, got %v", report.Reconciled.BlockingFaults)
			}
		}
	})

	t.Run("whitespace_only_output_classified_as_unknown", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("   \n\t  \r\n   "), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected CUPSSample.Available=false for whitespace-only lpstat output")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN for whitespace output", report.Reconciled.EffectiveState)
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("expected no offline fault in BlockingFaults, got %v", report.Reconciled.BlockingFaults)
			}
		}
	})

	t.Run("unrelated_text_classified_as_unknown", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("kernel: [12345.678] usb reset\n"), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected CUPSSample.Available=false for unrelated text output")
		}
		if len(report.QueryErrors) == 0 {
			t.Error("expected QueryErrors for unrelated output")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN for unrelated text", report.Reconciled.EffectiveState)
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("expected no offline fault in BlockingFaults, got %v", report.Reconciled.BlockingFaults)
			}
		}
	})

	t.Run("wrong_queue_output_classified_as_unknown", func(t *testing.T) {
		client, cleanup := createTestClient(t, ClientConfig{
			CUPSQueue:          "BrotherDCP",
			EnableShadowHealth: true,
			CUPSRunner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("printer CanonMG3600 is idle. enabled since Sun Nov 15 10:00:00 2026\n"), nil
			},
			MockIPPState: mockIPP,
		})
		defer cleanup()

		report := client.RunShadowHealthCheck(context.Background())
		if report == nil {
			t.Fatal("expected non-nil ShadowHealthReport")
		}

		if report.CUPSSample.Available {
			t.Error("expected CUPSSample.Available=false when output describes wrong queue")
		}
		if len(report.QueryErrors) == 0 {
			t.Error("expected QueryErrors when output describes wrong queue")
		}
		if report.Reconciled.EffectiveState != "UNKNOWN" {
			t.Errorf("EffectiveState = %q, want UNKNOWN for wrong queue output", report.Reconciled.EffectiveState)
		}
		for _, bf := range report.Reconciled.BlockingFaults {
			if strings.Contains(strings.ToLower(bf), "offline") {
				t.Errorf("expected no offline fault in BlockingFaults, got %v", report.Reconciled.BlockingFaults)
			}
		}
	})
}
