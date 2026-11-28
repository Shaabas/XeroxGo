package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/pkg/contracts"
)

func TestSpooler_NativeIPPAdmission_PinAndDeferredAndPreSubmitGate(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "adm.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore failed: %v", err)
	}
	defer st.Close()
	if err := st.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfBytes := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfBytes)
	pdfSum := hex.EncodeToString(sum[:])
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()

	var adm atomic.Int32 // 0=NOT_ACCEPTING, 1=UNKNOWN, 2=ACCEPTING
	mockHealth := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		h := testHealthyMockHealth(ctx, sheets)
		switch adm.Load() {
		case 0:
			h.NativeIPPAdmission = contracts.AcceptingJobsNotAccepting
		case 1:
			h.NativeIPPAdmission = contracts.AcceptingJobsUnknown
		default:
			h.NativeIPPAdmission = contracts.AcceptingJobsAccepting
		}
		return h
	}

	var cupsCalls atomic.Int32
	client, err := NewClient(ClientConfig{
		CloudURL:               "ws://127.0.0.1:1/ws",
		KioskID:                "kiosk-adm-gate",
		StateStore:             st,
		DownloadDir:            tmpDir,
		IdleQRContent:          "https://kiosk.test/adm",
		HealthPollInterval:     20 * time.Millisecond,
		ConfiguredTrayCapacity: 250,
		ScreenSink:             func(*image.RGBA) error { return nil },
		CUPSQueue:              "MockQueue",
		MockCUPSPrint: func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(int)) error {
			cupsCalls.Add(1)
			if onPage != nil {
				onPage(cmd.TotalSheets)
			}
			return nil
		},
		MockHealth: mockHealth,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. PIN readiness is unavailable on IDLE+NOT_ACCEPTING and UNKNOWN, available on ACCEPTING
	adm.Store(0)
	client.CheckPrinterHealth(ctx)
	if ready, _, _ := client.effectiveCustomerServiceReadiness(ctx); ready {
		t.Fatal("expected PIN readiness false for IDLE+NOT_ACCEPTING")
	}
	adm.Store(1)
	client.CheckPrinterHealth(ctx)
	if ready, _, _ := client.effectiveCustomerServiceReadiness(ctx); ready {
		t.Fatal("expected PIN readiness false for UNKNOWN")
	}
	adm.Store(2)
	client.CheckPrinterHealth(ctx)
	if ready, reason, _ := client.effectiveCustomerServiceReadiness(ctx); !ready {
		t.Fatalf("expected PIN readiness true for IDLE+ACCEPTING, got %q", reason)
	}

	// 2. Pre-submission gate in processJob: flip to NOT_ACCEPTING before processJob runs -> FAILED_REFUNDABLE, zero CUPS calls, calibration preserved
	adm.Store(0)
	client.CheckPrinterHealth(ctx)
	cmdFail := contracts.PrintJobCommand{
		JobID:          "job-presub-notacc",
		OrderID:        "ord-presub-notacc",
		TotalDocuments: 1,
		KioskID:        "kiosk-adm-gate",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.SaveJob(ctx, cmdFail, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob cmdFail failed: %v", err)
	}
	client.processJob(ctx, cmdFail)

	savedFail, _ := st.GetJob(ctx, cmdFail.JobID)
	if savedFail == nil || savedFail.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected FAILED_REFUNDABLE on pre-submission NOT_ACCEPTING, got %+v", savedFail)
	}
	if savedFail.SubmittedToPrinter {
		t.Fatal("expected SubmittedToPrinter=false")
	}
	if cupsCalls.Load() != 0 {
		t.Fatalf("expected 0 CUPS calls, got %d", cupsCalls.Load())
	}
	if err := st.MarkOutcomeSynced(ctx, cmdFail.JobID, savedFail.OutcomeVersion); err != nil {
		t.Fatalf("MarkOutcomeSynced failed: %v", err)
	}
	sheets, cal, _ := st.GetPaperSupply(ctx)
	if !cal || sheets != 100 {
		t.Fatalf("expected calibrated=true sheets=100 preserved, got cal=%v sheets=%d", cal, sheets)
	}

	// 3. Deferred job admission: untouched DISPATCHED job deferred during NOT_ACCEPTING, admitted when ACCEPTING returns
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()
	cmdDef := contracts.PrintJobCommand{
		JobID:          "job-def-notacc",
		OrderID:        "ord-def-notacc",
		TotalDocuments: 1,
		KioskID:        "kiosk-adm-gate",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.SaveJob(ctx, cmdDef, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob cmdDef failed: %v", err)
	}
	client.checkAndAdmitOrder(cmdDef.OrderID, cmdDef.TotalDocuments)

	mid, _ := st.GetJob(ctx, cmdDef.JobID)
	if mid == nil || mid.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected deferred job to remain DISPATCHED, got %+v", mid)
	}
	if cupsCalls.Load() != 0 {
		t.Fatalf("expected 0 CUPS calls while deferred, got %d", cupsCalls.Load())
	}

	adm.Store(2)
	client.CheckPrinterHealth(ctx) // drains deferredJobs when ACCEPTING returns
	deadline := time.Now().Add(3 * time.Second)
	var doneStatus contracts.JobStatus
	for time.Now().Before(deadline) {
		if j, _ := st.GetJob(ctx, cmdDef.JobID); j != nil {
			doneStatus = j.Status
			if doneStatus == contracts.JobStatusCompleted {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if doneStatus != contracts.JobStatusCompleted {
		t.Fatalf("expected deferred job to complete once ACCEPTING returned, got %q", doneStatus)
	}
	if cupsCalls.Load() != 1 {
		t.Fatalf("expected 1 CUPS call after drain, got %d", cupsCalls.Load())
	}
}

// Canonical CUPS+IPP+SNMP regression: native IPP transitioning to NOT_ACCEPTING during an active print
// must NOT interrupt the running CUPS job and must allow canonical completion + odometer accounting.
func TestSpoolerCanonical_ActivePrint_NativeIPPNotAccepting_DoesNotInterrupt(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "active_notacc.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore failed: %v", err)
	}
	defer st.Close()
	if err := st.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfBytes := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfBytes)
	pdfSum := hex.EncodeToString(sum[:])
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()

	var phase atomic.Int32 // 0=IDLE+ACCEPTING, 1=PRINTING+NOT_ACCEPTING, 2=COMPLETED
	var holdCalled atomic.Bool
	var cancelCalled atomic.Bool

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for i, a := range args {
				if a == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					holdCalled.Store(true)
					return []byte(""), nil
				}
			}
			phase.Store(1)
			go func() {
				time.Sleep(250 * time.Millisecond)
				phase.Store(2)
			}()
			return []byte("request id is HPTank-991 (1 file(s))"), nil
		}
		if name == "cancel" {
			cancelCalled.Store(true)
			return []byte(""), nil
		}
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) {
					if args[i+1] == "completed" {
						if phase.Load() == 2 {
							return []byte("HPTank-991 user 1024 Sat Nov 28 20:00:00 2026\n\tjob-completed-successfully"), nil
						}
						return []byte(""), nil
					}
					if args[i+1] == "not-completed" {
						if phase.Load() == 1 {
							return []byte("HPTank-991 user 1024 Sat Nov 28 20:00:00 2026"), nil
						}
						return []byte(""), nil
					}
				}
			}
			if phase.Load() == 1 {
				return []byte("printer HPTank is printing HPTank-991. enabled"), nil
			}
			return []byte("printer HPTank is idle. enabled"), nil
		}
		if name == "ipptool" {
			if phase.Load() == 2 {
				return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 2\n"), nil
			}
			return []byte("job-state (enum) = processing\njob-state-reasons (keyword) = job-printing\njob-media-sheets-completed (integer) = 1\n"), nil
		}
		return []byte(""), nil
	}

	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		life := int64(1000)
		state := "IDLE"
		if phase.Load() == 1 {
			life = 1001
			state = "PRINTING"
		} else if phase.Load() == 2 {
			life = 1002
			state = "IDLE"
		}
		return printer.HardwareTelemetry{
			Online:         true,
			HasLife:        true,
			LifeCount:      life,
			CounterIndex:   "1.1",
			CounterUnit:    8,
			HasCounterMeta: true,
			HasFlags:       true,
			StatusState:    state,
			ObservedAt:     time.Now().UTC(),
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:               "ws://127.0.0.1:1/ws",
		KioskID:                "kiosk-active-notacc",
		StateStore:             st,
		CUPSQueue:              "HPTank",
		DownloadDir:            tmpDir,
		IdleQRContent:          "https://kiosk.test/active",
		HealthPollInterval:     15 * time.Millisecond,
		ConfiguredTrayCapacity: 250,
		ScreenSink:             func(*image.RGBA) error { return nil },
		CUPSRunner:             runner,
		SNMPPoller:             snmpPoller,
		CapabilityPolicy:       &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
		MockCUPSHealth: func(ctx context.Context, sheets int) contracts.PrinterHealth {
			stStr := "IDLE"
			if phase.Load() == 1 {
				stStr = "PRINTING"
			}
			return contracts.PrinterHealth{Online: true, State: stStr, PaperTrayStatus: "OK", EstimatedSheets: sheets, UpdatedAt: time.Now().UTC()}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			ippSt := printer.IPPPrinterStateIdle
			acc := printer.AcceptingJobsAccepting
			if phase.Load() == 1 {
				ippSt = printer.IPPPrinterStateProcessing
				acc = printer.AcceptingJobsNotAccepting
			}
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceNativeIPP,
				Queue:             queue,
				State:             ippSt,
				RawState:          string(ippSt),
				AcceptingJobs:     acc,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        time.Now().UTC(),
			}, nil
		},
		MockSNMPHealth: func(ctx context.Context, sheets int) contracts.PrinterHealth {
			h := testHealthySNMPHealth(ctx, sheets)
			if phase.Load() == 1 {
				h.State = "PRINTING"
			}
			return h
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h := client.CurrentHealthSnapshot(ctx); h.EvaluatedHealth == contracts.EvaluatedHealthHealthy && h.NativeIPPAdmission == contracts.AcceptingJobsAccepting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-active-notacc-01",
		OrderID:        "ord-active-notacc-01",
		TotalDocuments: 1,
		KioskID:        "kiosk-active-notacc",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusDispatched)
	client.processJob(ctx, cmd)

	if holdCalled.Load() || cancelCalled.Load() {
		t.Fatalf("expected active CUPS job NOT to be held or cancelled on native IPP NOT_ACCEPTING (hold=%v cancel=%v)", holdCalled.Load(), cancelCalled.Load())
	}
	saved, err := st.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil || saved.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected COMPLETED, got %+v err=%v", saved, err)
	}
	sheets, cal, _ := st.GetPaperSupply(ctx)
	if !cal || sheets != 98 {
		t.Fatalf("expected calibrated=true sheets=98, got cal=%v sheets=%d", cal, sheets)
	}
	_ = strings.TrimSpace("")
}
