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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
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
	var downloadHits atomic.Int32
	var adm atomic.Int32 // 0=NOT_ACCEPTING, 1=UNKNOWN, 2=ACCEPTING
	var flipDuringDownload atomic.Bool
	var client *Client
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadHits.Add(1)
		if flipDuringDownload.CompareAndSwap(true, false) && client != nil {
			adm.Store(0)
			client.refreshPrinterHealthTelemetry(context.Background())
		}
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()
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
	var printedOrder []string
	var printedMu sync.Mutex
	client, err = NewClient(ClientConfig{
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
			printedMu.Lock()
			printedOrder = append(printedOrder, cmd.JobID)
			printedMu.Unlock()
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

	// 2. Non-acceptance BEFORE processing begins: untouched DISPATCHED retains DISPATCHED, zero download, zero CUPS calls
	adm.Store(0)
	client.CheckPrinterHealth(ctx)
	cmdUntouched := contracts.PrintJobCommand{
		JobID:          "job-untouched-notacc",
		OrderID:        "ord-untouched-notacc",
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
	if err := st.SaveJob(ctx, cmdUntouched, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob cmdUntouched failed: %v", err)
	}
	client.processJob(ctx, cmdUntouched)
	if got, _ := st.GetJob(ctx, cmdUntouched.JobID); got == nil || got.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected untouched job to retain DISPATCHED, got %+v", got)
	}
	if downloadHits.Load() != 0 || cupsCalls.Load() != 0 {
		t.Fatalf("expected 0 downloads and 0 CUPS calls, got downloads=%d cups=%d", downloadHits.Load(), cupsCalls.Load())
	}
	// Clear deferredJobs before next sub-step
	client.deferredMu.Lock()
	client.deferredJobs = nil
	client.deferredMu.Unlock()

	// 3. Admission loss AFTER processing began (during preparation/download): exact FAILED_REFUNDABLE, zero CUPS calls, calibration preserved
	adm.Store(2)
	client.CheckPrinterHealth(ctx)
	flipDuringDownload.Store(true)
	cmdSpooling := contracts.PrintJobCommand{
		JobID:          "job-spooling-notacc",
		OrderID:        "ord-spooling-notacc",
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
	if err := st.SaveJob(ctx, cmdSpooling, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob cmdSpooling failed: %v", err)
	}
	client.processJob(ctx, cmdSpooling)

	savedFail, _ := st.GetJob(ctx, cmdSpooling.JobID)
	if savedFail == nil || savedFail.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected FAILED_REFUNDABLE on pre-submission NOT_ACCEPTING after SPOOLING, got %+v", savedFail)
	}
	if savedFail.SubmittedToPrinter {
		t.Fatal("expected SubmittedToPrinter=false")
	}
	if cupsCalls.Load() != 0 {
		t.Fatalf("expected 0 CUPS calls, got %d", cupsCalls.Load())
	}
	if err := st.MarkOutcomeSynced(ctx, cmdSpooling.JobID, savedFail.OutcomeVersion); err != nil {
		t.Fatalf("MarkOutcomeSynced failed: %v", err)
	}
	sheets, cal, _ := st.GetPaperSupply(ctx)
	if !cal || sheets != 100 {
		t.Fatalf("expected calibrated=true sheets=100 preserved, got cal=%v sheets=%d", cal, sheets)
	}

	// 4. Deferred job resumes EXACTLY ONCE when ACCEPTING returns, even when screen is ALREADY ModeIdle
	client.currentModeMu.Lock()
	client.currentMode = ui_ipc.ModeIdle
	client.currentModeMu.Unlock()

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()

	cmdDef := contracts.PrintJobCommand{
		JobID:          "job-def-idle-screen",
		OrderID:        "ord-def-idle-screen",
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
	// Confirm screen is already ModeIdle before health check drains deferredJobs
	client.currentModeMu.Lock()
	client.currentMode = ui_ipc.ModeIdle
	client.currentModeMu.Unlock()
	client.CheckPrinterHealth(ctx)

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
		t.Fatalf("expected deferred job to complete when ACCEPTING returned on already-idle screen, got %q", doneStatus)
	}
	// Trigger additional health checks to prove it resumes exactly once
	client.CheckPrinterHealth(ctx)
	time.Sleep(50 * time.Millisecond)
	if cupsCalls.Load() != 1 {
		t.Fatalf("expected deferred job to run exactly once (1 CUPS call), got %d", cupsCalls.Load())
	}
}

func TestSpooler_NativeIPPAdmission_StartupRecoveryAndMultiDocOrder(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "startup_multidoc.db")
	st, err := openCalibratedTestStore(t, dbPath)
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
	var downloadHits atomic.Int32
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadHits.Add(1)
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()

	// Seed untouched queued multi-doc order before startup recovery
	for i, jid := range []string{"job-boot-0", "job-boot-1"} {
		cmd := contracts.PrintJobCommand{
			JobID:          jid,
			OrderID:        "ord-boot",
			DocumentIndex:  i,
			TotalDocuments: 2,
			KioskID:        "kiosk-boot-adm",
			DownloadURL:    pdfSrv.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
			t.Fatalf("SaveJob %s failed: %v", jid, err)
		}
	}

	var adm atomic.Int32 // 0=NOT_ACCEPTING, 2=ACCEPTING
	var printed []string
	var mu sync.Mutex

	client, err := NewClient(ClientConfig{
		CloudURL:               "ws://127.0.0.1:1/ws",
		KioskID:                "kiosk-boot-adm",
		StateStore:             st,
		DownloadDir:            tmpDir,
		IdleQRContent:          "https://kiosk.test/boot",
		HealthPollInterval:     20 * time.Millisecond,
		ConfiguredTrayCapacity: 250,
		ScreenSink:             func(*image.RGBA) error { return nil },
		CUPSQueue:              "MockQueue",
		MockCUPSPrint: func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(int)) error {
			mu.Lock()
			printed = append(printed, cmd.JobID)
			mu.Unlock()
			if cmd.JobID == "job-mdoc-0" {
				// Flip to NOT_ACCEPTING while doc 0 is printing so docs 1 & 2 wait
				adm.Store(0)
				clientCheck := onPage
				_ = clientCheck
			}
			if onPage != nil {
				onPage(cmd.TotalSheets)
			}
			return nil
		},
		MockHealth: func(ctx context.Context, sheets int) contracts.PrinterHealth {
			h := testHealthyMockHealth(ctx, sheets)
			if adm.Load() == 0 {
				h.NativeIPPAdmission = contracts.AcceptingJobsNotAccepting
			} else {
				h.NativeIPPAdmission = contracts.AcceptingJobsAccepting
			}
			return h
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Start daemon while NOT_ACCEPTING: untouched queued jobs must remain DISPATCHED with 0 downloads
	adm.Store(0)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()
	time.Sleep(120 * time.Millisecond)

	for _, jid := range []string{"job-boot-0", "job-boot-1"} {
		j, _ := st.GetJob(ctx, jid)
		if j == nil || j.Status != contracts.JobStatusDispatched {
			t.Fatalf("expected %s to remain DISPATCHED during startup non-acceptance, got %+v", jid, j)
		}
	}
	if downloadHits.Load() != 0 {
		t.Fatalf("expected 0 downloads during startup non-acceptance, got %d", downloadHits.Load())
	}

	// Restore ACCEPTING -> startup deferred jobs complete in strict index order [0, 1]
	adm.Store(2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j1, _ := st.GetJob(ctx, "job-boot-1")
		if j1 != nil && j1.Status == contracts.JobStatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	gotBoot := append([]string(nil), printed...)
	mu.Unlock()
	if len(gotBoot) != 2 || gotBoot[0] != "job-boot-0" || gotBoot[1] != "job-boot-1" {
		t.Fatalf("expected [job-boot-0 job-boot-1] in order, got %v", gotBoot)
	}

	// Multi-document order: doc 0 prints while ACCEPTING, MockCUPSPrint flips adm->0, docs 1 & 2 must wait in DISPATCHED
	for i := 0; i < 3; i++ {
		cmd := contracts.PrintJobCommand{
			JobID:          "job-mdoc-" + string(rune('0'+i)),
			OrderID:        "ord-mdoc",
			DocumentIndex:  i,
			TotalDocuments: 3,
			KioskID:        "kiosk-boot-adm",
			DownloadURL:    pdfSrv.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
			t.Fatalf("SaveJob %s failed: %v", cmd.JobID, err)
		}
	}
	client.checkAndAdmitOrder("ord-mdoc", 3)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j0, _ := st.GetJob(ctx, "job-mdoc-0")
		if j0 != nil && j0.Status == contracts.JobStatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	for _, jid := range []string{"job-mdoc-1", "job-mdoc-2"} {
		j, _ := st.GetJob(ctx, jid)
		if j == nil || j.Status != contracts.JobStatusDispatched {
			t.Fatalf("expected subsequent doc %s to remain DISPATCHED while NOT_ACCEPTING, got %+v", jid, j)
		}
	}

	// Restore ACCEPTING -> docs 1 & 2 complete in strict order
	adm.Store(2)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j2, _ := st.GetJob(ctx, "job-mdoc-2")
		if j2 != nil && j2.Status == contracts.JobStatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	gotAll := append([]string(nil), printed...)
	mu.Unlock()
	wantAll := []string{"job-boot-0", "job-boot-1", "job-mdoc-0", "job-mdoc-1", "job-mdoc-2"}
	if len(gotAll) != len(wantAll) {
		t.Fatalf("expected %v, got %v", wantAll, gotAll)
	}
	for i := range wantAll {
		if gotAll[i] != wantAll[i] {
			t.Fatalf("order mismatch at %d: expected %v, got %v", i, wantAll, gotAll)
		}
	}
}

func TestSpoolerCanonical_RealCUPSPreSubmitFlip_AndExpiredAdmission(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "real_cups_flip.db"))
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

	var lpCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalls.Add(1)
			return []byte("request id is HPTank-001 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, a := range args {
				if a == "-W" && i+1 < len(args) && args[i+1] == "not-completed" {
					return []byte(""), nil
				}
			}
			return []byte("printer HPTank is idle. enabled"), nil
		}
		return []byte(""), nil
	}

	var adm atomic.Int32 // 2=ACCEPTING, 0=NOT_ACCEPTING
	adm.Store(2)

	var client *Client
	// Deterministic boundary: when SNMPPoller is called right before OnSubmittingWithBaseline inside PrintPDFWithCUPSHooksCustom,
	// flip native IPP to NOT_ACCEPTING and refresh telemetry so OnSubmittingWithBaseline sees NOT_ACCEPTING before lp runs.
	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		if adm.CompareAndSwap(2, 0) && client != nil {
			// Wait? We are inside processJob; refreshPrinterHealthTelemetry acquires acqMu/calibrationMu (calibrationMu already unlocked at step 7)
			client.refreshPrinterHealthTelemetry(ctx)
		}
		return printer.HardwareTelemetry{
			Online:         true,
			HasLife:        true,
			LifeCount:      1000,
			CounterIndex:   "1.1",
			CounterUnit:    8,
			HasCounterMeta: true,
			HasFlags:       true,
			StatusState:    "IDLE",
			ObservedAt:     time.Now().UTC(),
		}
	}

	client, err = NewClient(ClientConfig{
		CloudURL:               "ws://127.0.0.1:1/ws",
		KioskID:                "kiosk-cups-presub-flip",
		StateStore:             st,
		CUPSQueue:              "HPTank",
		DownloadDir:            tmpDir,
		IdleQRContent:          "https://kiosk.test/flip",
		HealthPollInterval:     15 * time.Millisecond,
		ConfiguredTrayCapacity: 250,
		ScreenSink:             func(*image.RGBA) error { return nil },
		CUPSRunner:             runner,
		SNMPPoller:             snmpPoller,
		CapabilityPolicy:       &printer.PrinterCapabilityPolicy{AllowColor: true, AllowDuplex: true, RequireA4: true, AllowMultiCopy: true},
		MockCUPSHealth: func(ctx context.Context, sheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", EstimatedSheets: sheets, UpdatedAt: time.Now().UTC()}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			acc := printer.AcceptingJobsAccepting
			if adm.Load() == 0 {
				acc = printer.AcceptingJobsNotAccepting
			}
			return printer.IPPPrinterStateResult{
				Source:            printer.CapabilitySourceNativeIPP,
				Queue:             queue,
				State:             printer.IPPPrinterStateIdle,
				RawState:          "idle",
				AcceptingJobs:     acc,
				EndpointReachable: true,
				QueueAccessible:   true,
				ObservedAt:        time.Now().UTC(),
			}, nil
		},
		MockSNMPHealth: testHealthySNMPHealth,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Acquire initial ACCEPTING health (reset adm to 2 first because SNMPPoller only flips on first call if we gate it)
	adm.Store(2)
	// First let's prime initial health without flipping: temporarily set adm=3 -> treated as ACCEPTING, then set adm=2 right before processJob
	adm.Store(3)
	client.CheckPrinterHealth(ctx)
	if h := client.CurrentHealthSnapshot(ctx); h.NativeIPPAdmission != contracts.AcceptingJobsAccepting {
		t.Fatalf("expected initial ACCEPTING, got %s", h.NativeIPPAdmission)
	}
	adm.Store(2) // next SNMPPoller call inside PrintPDFWithCUPSHooksCustom flips 2 -> 0

	cmd := contracts.PrintJobCommand{
		JobID:          "job-cups-presub-flip",
		OrderID:        "ord-cups-presub-flip",
		TotalDocuments: 1,
		KioskID:        "kiosk-cups-presub-flip",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusDispatched); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	client.processJob(ctx, cmd)

	if lpCalls.Load() != 0 {
		t.Fatalf("expected 0 lp calls when admission flipped at CUPS pre-submission boundary, got %d", lpCalls.Load())
	}
	saved, _ := st.GetJob(ctx, cmd.JobID)
	if saved == nil || saved.Status != contracts.JobStatusFailedRefundable {
		t.Fatalf("expected FAILED_REFUNDABLE, got %+v", saved)
	}
	if saved.SubmittedToPrinter || saved.CUPSJobID != "" {
		t.Fatalf("expected SubmittedToPrinter=false and empty CUPSJobID, got submitted=%v cups=%q", saved.SubmittedToPrinter, saved.CUPSJobID)
	}
	sheets, cal, _ := st.GetPaperSupply(ctx)
	if !cal || sheets != 100 {
		t.Fatalf("expected calibrated=true sheets=100 preserved, got cal=%v sheets=%d", cal, sheets)
	}

	// Expired native IPP observation in CurrentHealthSnapshot must become AcceptingJobsUnknown and UNVERIFIED, preserving original timestamps
	oldTS := time.Now().UTC().Add(-15 * time.Second)
	client.snapshotMu.Lock()
	client.latestSnap.HasSample = true
	client.latestSnap.Health = testHealthyMockHealth(ctx, 100)
	client.latestSnap.Health.NativeIPPAdmission = contracts.AcceptingJobsAccepting
	client.latestSnap.Health.UpdatedAt = oldTS
	client.latestSnap.Health.EvidenceFreshness = contracts.BuildUniformEvidenceFreshness(oldTS, DefaultTelemetryMaxAge)
	client.snapshotMu.Unlock()

	expiredSnap := client.CurrentHealthSnapshot(ctx)
	if expiredSnap.NativeIPPAdmission != contracts.AcceptingJobsUnknown {
		t.Fatalf("expected expired CurrentHealthSnapshot NativeIPPAdmission=UNKNOWN, got %s", expiredSnap.NativeIPPAdmission)
	}
	if expiredSnap.EvaluatedHealth != contracts.EvaluatedHealthUnverified {
		t.Fatalf("expected expired CurrentHealthSnapshot EvaluatedHealth=UNVERIFIED, got %s", expiredSnap.EvaluatedHealth)
	}
	if !expiredSnap.UpdatedAt.Equal(oldTS) {
		t.Fatalf("expected original UpdatedAt %v preserved, got %v", oldTS, expiredSnap.UpdatedAt)
	}
}

// Canonical CUPS+IPP+SNMP regression: native IPP transitioning to NOT_ACCEPTING during an active print
// must NOT interrupt the running or resumed CUPS job, must evaluate PRINTING+NOT_ACCEPTING as HEALTHY,
// and must complete with accurate odometer/inventory accounting.
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
	var observedNotAcceptingWhilePrinting atomic.Bool

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
						if phase.Load() == 2 || phase.Load() == 4 {
							return []byte("HPTank-991 user 1024 Sat Nov 28 20:00:00 2026\n\tjob-completed-successfully"), nil
						}
						return []byte(""), nil
					}
					if args[i+1] == "not-completed" {
						if phase.Load() == 1 || phase.Load() == 3 {
							return []byte("HPTank-991 user 1024 Sat Nov 28 20:00:00 2026"), nil
						}
						return []byte(""), nil
					}
				}
			}
			if phase.Load() == 1 || phase.Load() == 3 {
				return []byte("printer HPTank is printing HPTank-991. enabled"), nil
			}
			return []byte("printer HPTank is idle. enabled"), nil
		}
		if name == "ipptool" {
			if phase.Load() == 2 || phase.Load() == 4 {
				return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 2\n"), nil
			}
			return []byte("job-state (enum) = processing\njob-state-reasons (keyword) = job-printing\njob-media-sheets-completed (integer) = 1\n"), nil
		}
		return []byte(""), nil
	}

	snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
		life := int64(1000)
		state := "IDLE"
		switch phase.Load() {
		case 1:
			life = 1001
			state = "PRINTING"
		case 2:
			life = 1002
			state = "IDLE"
		case 3:
			life = 1003
			state = "PRINTING"
		case 4:
			life = 1004
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

	var client *Client
	client, err = NewClient(ClientConfig{
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
			if phase.Load() == 1 || phase.Load() == 3 {
				stStr = "PRINTING"
			}
			return contracts.PrinterHealth{Online: true, State: stStr, PaperTrayStatus: "OK", EstimatedSheets: sheets, UpdatedAt: time.Now().UTC()}
		},
		MockIPPState: func(ctx context.Context, queue string) (printer.IPPPrinterStateResult, error) {
			ippSt := printer.IPPPrinterStateIdle
			acc := printer.AcceptingJobsAccepting
			if phase.Load() == 1 || phase.Load() == 3 {
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
			if phase.Load() == 1 || phase.Load() == 3 {
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

	// Observer goroutine proving CurrentHealthSnapshot evaluates HEALTHY + PRINTING + NOT_ACCEPTING during active print
	obsStop := make(chan struct{})
	go func() {
		for {
			select {
			case <-obsStop:
				return
			default:
				h := client.CurrentHealthSnapshot(ctx)
				if h.State == "PRINTING" && h.EvaluatedHealth == contracts.EvaluatedHealthHealthy && h.NativeIPPAdmission == contracts.AcceptingJobsNotAccepting {
					observedNotAcceptingWhilePrinting.Store(true)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

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
	close(obsStop)

	if !observedNotAcceptingWhilePrinting.Load() {
		t.Fatal("expected HEALTHY + PRINTING + NOT_ACCEPTING snapshot to be observed during active printing")
	}
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

	// Resumed CUPS monitoring during healthy NOT_ACCEPTING must also complete without interruption
	phase.Store(3)
	go func() {
		time.Sleep(180 * time.Millisecond)
		phase.Store(4)
	}()
	cmdResumed := contracts.PrintJobCommand{
		JobID:          "job-resumed-notacc-02",
		OrderID:        "ord-resumed-notacc-02",
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
	_ = st.SaveJob(ctx, cmdResumed, contracts.JobStatusDispatched)
	_ = st.BeginJobSpooling(ctx, cmdResumed.JobID)
	_ = st.MarkJobSubmittingWithBaseline(ctx, cmdResumed.JobID, true, 1002, "1.1", 8, time.Now().UTC())
	_ = st.RecordJobSubmission(ctx, cmdResumed.JobID, "HPTank-991")
	resRec, _ := st.GetJob(ctx, cmdResumed.JobID)
	client.resumeCUPSMonitoring(ctx, resRec)

	if holdCalled.Load() || cancelCalled.Load() {
		t.Fatalf("expected resumed CUPS monitor NOT to hold or cancel on native IPP NOT_ACCEPTING (hold=%v cancel=%v)", holdCalled.Load(), cancelCalled.Load())
	}
	resAfter, _ := st.GetJob(ctx, cmdResumed.JobID)
	if resAfter == nil || resAfter.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected resumed job COMPLETED, got %+v", resAfter)
	}
	_ = strings.TrimSpace("")
}
