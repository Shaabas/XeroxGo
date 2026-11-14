package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

// TestHealthScreen_JamDuringPrinting_OverridesModePrintingToError verifies that
// a confirmed paper jam during active printing immediately overrides ModePrinting
// with ModeError and pauses the job in the state store.
func TestHealthScreen_JamDuringPrinting_OverridesModePrintingToError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	hSum := sha256.Sum256([]byte("job-jam-override"))
	finalName := hex.EncodeToString(hSum[:]) + ".pdf"
	if err := os.WriteFile(filepath.Join(tmpDir, finalName), pdfContent, 0644); err != nil {
		t.Fatalf("failed to pre-seed PDF file: %v", err)
	}

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var jamReported atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if jamReported.Load() {
			return contracts.PrinterHealth{
				Online:          false,
				State:           "ERROR",
				PaperJam:        true,
				ErrorMessage:    "paper jam in fuser feed",
				EstimatedSheets: sheets,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	printStarted := make(chan struct{})
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		close(printStarted)
		if onPage != nil {
			onPage(1)
		}
		// Simulate active printing stall while waiting for context cancel or fault
		<-ctx.Done()
		return ctx.Err()
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-jam-override",
			KioskID:        "kiosk-jam",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	cfg := ClientConfig{
		CloudURL:                 "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:                  "kiosk-jam",
		StateStore:               store,
		CUPSQueue:                "HPTank",
		DownloadDir:              tmpDir,
		IdleQRContent:            "https://kiosk.test/jam",
		HealthPollInterval:       30 * time.Millisecond,
		ErrorDisplayDuration:     200 * time.Millisecond,
		CompletedDisplayDuration: 100 * time.Millisecond,
		ScreenSink:               func(*image.RGBA) error { return nil },
		MockHealth:               healthMock,
		MockCUPSPrint:            mockCUPS,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for printing to begin
	select {
	case <-printStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for print job to start")
	}

	// Verify ModePrinting was reached
	deadline := time.Now().Add(2 * time.Second)
	printingSeen := false
	for time.Now().Before(deadline) {
		for _, st := range client.RenderHistory() {
			if st.Mode == ui_ipc.ModePrinting {
				printingSeen = true
				break
			}
		}
		if printingSeen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !printingSeen {
		t.Fatal("expected screen to show ModePrinting")
	}

	// Induce hardware paper jam
	jamReported.Store(true)

	// Verify ModeError immediately overrides ModePrinting
	errorSeen := false
	var lastError ui_ipc.ScreenState
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		history := client.RenderHistory()
		for _, st := range history {
			if st.Mode == ui_ipc.ModeError {
				errorSeen = true
				lastError = st
			}
		}
		if errorSeen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !errorSeen {
		t.Fatal("expected screen to transition to ModeError on paper jam")
	}
	if !strings.Contains(lastError.Message, "paper feed issue") {
		t.Errorf("expected error message to contain 'paper feed issue', got %q", lastError.Message)
	}

	// Verify job is recorded as PAUSED_ERROR in state store
	deadline = time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		savedJob, err := store.GetJob(ctx, "job-jam-override")
		if err == nil && savedJob != nil && savedJob.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !paused {
		savedJob, _ := store.GetJob(ctx, "job-jam-override")
		status := "nil"
		if savedJob != nil {
			status = string(savedJob.Status)
		}
		t.Fatalf("job status = %s, want PAUSED_ERROR", status)
	}
}

// TestHealthScreen_FaultPersistence_DoesNotResetToReadyToPrint verifies that
// while a confirmed blocking fault persists, the screen stays on ModeError and
// does NOT automatically reset to ModeIdle ("Ready to print").
func TestHealthScreen_FaultPersistence_DoesNotResetToReadyToPrint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:             "ws://127.0.0.1:8080/ws",
		KioskID:              "kiosk-persist",
		StateStore:           store,
		CUPSQueue:            "HPTank",
		DownloadDir:          tmpDir,
		IdleQRContent:        "https://kiosk.test/idle",
		HealthPollInterval:   20 * time.Millisecond,
		ErrorDisplayDuration: 40 * time.Millisecond, // very short to test timer expiry
		ScreenSink:           func(*image.RGBA) error { return nil },
		MockHealth:           healthMock,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Manually simulate a hardware fault error trigger
	cmd := contracts.PrintJobCommand{JobID: "job-persist", KioskID: "kiosk-persist", TotalPages: 2}
	client.setHardwareFault(true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer.")
	client.handleJobError(ctx, cmd, 1, 1, "paper feed jam detected in printer")

	// Wait past the ErrorDisplayDuration (40ms)
	time.Sleep(120 * time.Millisecond)

	// Verify screen did NOT reset to ModeIdle
	client.currentModeMu.Lock()
	currentMode := client.currentMode
	client.currentModeMu.Unlock()

	if currentMode != ui_ipc.ModeError {
		t.Fatalf("currentMode = %s, want ModeError (should not reset while fault persists)", currentMode)
	}

	// History check: last rendered state must NOT be ModeIdle
	history := client.RenderHistory()
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.Mode == ui_ipc.ModeIdle {
			t.Fatal("unexpected ModeIdle in history while hardware fault persists")
		}
	}
}

// TestHealthScreen_StaleProgressUpdates_CannotOverwriteErrorScreen verifies that
// late/stale printing progress callbacks cannot overwrite an active ModeError screen
// back to ModePrinting.
func TestHealthScreen_StaleProgressUpdates_CannotOverwriteErrorScreen(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cfg := ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-stale",
		StateStore:    store,
		CUPSQueue:     "HPTank",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/idle",
		ScreenSink:    func(*image.RGBA) error { return nil },
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Set hardware fault active and render ModeError
	client.setHardwareFault(true, "paper jam", "A paper feed issue occurred with the printer.")
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: "A paper feed issue occurred with the printer.",
		Detail:  "Order Ref: job-stale • Completed: 1 of 5 pages",
	})

	historyLenBefore := len(client.RenderHistory())

	// Attempt a stale progress callback to render ModePrinting
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModePrinting,
		CurrentPage:     2,
		TotalPages:      5,
		ProgressPercent: 40,
		Message:         "Printing document...",
		Detail:          "Job ID: job-stale",
	})

	historyAfter := client.RenderHistory()
	if len(historyAfter) != historyLenBefore {
		t.Fatalf("expected render history to remain unchanged, grew from %d to %d", historyLenBefore, len(historyAfter))
	}

	client.currentModeMu.Lock()
	mode := client.currentMode
	client.currentModeMu.Unlock()

	if mode != ui_ipc.ModeError {
		t.Fatalf("expected currentMode to remain ModeError, got %s", mode)
	}
}

// TestHealthScreen_SNMPTimeout_CUPSReasonsPreserved verifies that when SNMP is
// unreachable or times out, CUPS health reasons (such as media-jam-error) are
// accurately preserved and elevated by the combined health poller.
func TestHealthScreen_SNMPTimeout_CUPSReasonsPreserved(t *testing.T) {
	cupsRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// CUPS reports idle with media-jam-error reason
		out := "printer HPTank is idle.  enabled since Thu Nov 13 10:00:00 2026\n\tPrinter state reasons: media-jam-error"
		return []byte(out), nil
	}

	// SNMP target on unused port to simulate timeout / unreachable
	snmpCfg := printer.SNMPConfig{
		Target:  "127.0.0.1",
		Port:    54321,
		Timeout: 50 * time.Millisecond,
		Retries: 0,
	}

	health := printer.PollCombinedHealth(context.Background(), cupsRunner, "HPTank", snmpCfg, 100)
	if health.State != "ERROR" {
		t.Fatalf("health.State = %s, want ERROR (CUPS reason must be preserved when SNMP is unreachable)", health.State)
	}
	if !health.PaperJam {
		t.Error("expected PaperJam = true from CUPS media-jam-error")
	}
	if !strings.Contains(strings.ToLower(health.ErrorMessage), "jam") {
		t.Errorf("expected ErrorMessage containing 'jam', got %q", health.ErrorMessage)
	}
}

// TestHealthScreen_Recovery_RefreshesScreenWithoutReprintingOrCompleting verifies that
// when the printer hardware recovers from a fault:
// 1. The screen refreshes to ModeIdle ("Ready to print").
// 2. The interrupted job is NOT marked completed and NOT automatically reprinted.
func TestHealthScreen_Recovery_RefreshesScreenWithoutReprintingOrCompleting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	// Interrupted job in store
	interruptedCmd := contracts.PrintJobCommand{
		JobID:          "job-interrupted-01",
		KioskID:        "kiosk-rec",
		TotalPages:     5,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
	}
	_ = store.SaveJob(ctx, interruptedCmd, contracts.JobStatusPausedError)
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job-interrupted-01",
		KioskID:       "kiosk-rec",
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:             "ws://127.0.0.1:8080/ws",
		KioskID:              "kiosk-rec",
		StateStore:           store,
		CUPSQueue:            "HPTank",
		DownloadDir:          tmpDir,
		IdleQRContent:        "https://kiosk.test/ready",
		HealthPollInterval:   20 * time.Millisecond,
		ErrorDisplayDuration: 10 * time.Second,
		ScreenSink:           func(*image.RGBA) error { return nil },
		MockHealth:           healthMock,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Initial fault state
	client.setHardwareFault(true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer.")
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: "A paper feed issue occurred with the printer.",
		Detail:  "Order Ref: job-interrupted-01 • Completed: 2 of 5 pages",
	})

	// 2. Printer hardware recovers
	isRecovered.Store(true)

	// Step 2A: Verify a single transient healthy reading does NOT clear the fault (Point 6)
	h1 := client.CheckPrinterHealth(ctx)
	if h1.State != "IDLE" {
		t.Fatalf("expected healthy IDLE state on recovery, got %s", h1.State)
	}
	if !client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to remain active after single transient healthy reading")
	}

	// Step 2B: Supply remaining consecutive healthy readings to satisfy reliable recovery evidence (Point 7)
	client.CheckPrinterHealth(ctx)
	client.CheckPrinterHealth(ctx)

	if client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to be cleared after reliable recovery evidence (3 consecutive healthy readings)")
	}

	// 3. Verify screen refreshed to ModeIdle ("Ready to print")
	client.currentModeMu.Lock()
	currentMode := client.currentMode
	client.currentModeMu.Unlock()

	if currentMode != ui_ipc.ModeIdle {
		t.Fatalf("expected currentMode to be ModeIdle after recovery, got %s", currentMode)
	}

	history := client.RenderHistory()
	lastState := history[len(history)-1]
	if lastState.Mode != ui_ipc.ModeIdle || lastState.Message != "Ready to print" {
		t.Fatalf("last rendered state = %v, want ModeIdle with 'Ready to print'", lastState)
	}

	// 4. Verify interrupted job was NOT marked COMPLETED and NOT reprinted
	savedJob, err := store.GetJob(ctx, "job-interrupted-01")
	if err != nil || savedJob == nil {
		t.Fatalf("failed to retrieve job from store: %v", err)
	}
	if savedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("job status was modified to %s, want PAUSED_ERROR", savedJob.Status)
	}
	if savedJob.PagesPrinted != 2 || savedJob.SheetsPrinted != 2 {
		t.Fatalf("sheets/pages printed was modified: got %d pages / %d sheets", savedJob.PagesPrinted, savedJob.SheetsPrinted)
	}

	// Verify no job was enqueued for automatic reprint
	select {
	case cmd := <-client.jobQueue:
		t.Fatalf("unexpected job enqueued for reprint: %v", cmd)
	default:
		// Queue correctly empty
	}
}

// TestHealthScreen_OtherHardwareFaults verifies cover open, paper empty, and output bin full messages.
func TestHealthScreen_OtherHardwareFaults(t *testing.T) {
	cmd := contracts.PrintJobCommand{JobID: "job-hw", TotalPages: 4}

	t.Run("cover_open", func(t *testing.T) {
		msg, _ := formatCustomerError(cmd, 2, "printer cover is open")
		if !strings.Contains(msg, "cover was opened") {
			t.Errorf("msg = %q, want 'cover was opened'", msg)
		}
	})

	t.Run("paper_empty", func(t *testing.T) {
		msg, _ := formatCustomerError(cmd, 1, "printer paper tray is empty")
		if !strings.Contains(msg, "run out of paper") {
			t.Errorf("msg = %q, want 'run out of paper'", msg)
		}
	})

	t.Run("output_bin_full", func(t *testing.T) {
		msg, _ := formatCustomerError(cmd, 3, "printer output bin full")
		if !strings.Contains(msg, "output bin is full") {
			t.Errorf("msg = %q, want 'output bin is full'", msg)
		}
	})

	t.Run("printer_offline", func(t *testing.T) {
		msg, _ := formatCustomerError(cmd, 0, "printer is offline or unreachable")
		if !strings.Contains(msg, "offline or unreachable") {
			t.Errorf("msg = %q, want 'offline or unreachable'", msg)
		}
	})
}

// TestHealthScreen_SimultaneousProgressAndFault_RaceFree verifies that concurrent
// progress callbacks (onPage) and active health fault detection execute without data races,
// accurately record partial progress, and prevent late progress from overwriting PAUSED_ERROR.
func TestHealthScreen_SimultaneousProgressAndFault_RaceFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	hSum := sha256.Sum256([]byte("job-race-simul"))
	finalName := hex.EncodeToString(hSum[:]) + ".pdf"
	if err := os.WriteFile(filepath.Join(tmpDir, finalName), pdfContent, 0644); err != nil {
		t.Fatalf("failed to pre-seed PDF file: %v", err)
	}

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var jamReported atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if jamReported.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "ERROR",
				PaperJam:        true,
				ErrorMessage:    "paper feed jam detected in printer",
				EstimatedSheets: sheets,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	printStarted := make(chan struct{})
	var printOnce sync.Once
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printOnce.Do(func() { close(printStarted) })
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for i := 1; i <= cmd.TotalSheets; i++ {
				if onPage != nil {
					onPage(i)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()

		go func() {
			defer wg.Done()
			time.Sleep(7 * time.Millisecond)
			jamReported.Store(true)
		}()

		<-ctx.Done()
		wg.Wait()
		return ctx.Err()
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-race-simul",
			KioskID:        "kiosk-race",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	cfg := ClientConfig{
		CloudURL:           "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:            "kiosk-race",
		StateStore:         store,
		CUPSQueue:          "HPTank",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/race",
		HealthPollInterval: 5 * time.Millisecond,
		ScreenSink:         func(*image.RGBA) error { return nil },
		MockHealth:         healthMock,
		MockCUPSPrint:      mockCUPS,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	select {
	case <-printStarted:
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for print job to start")
	}

	deadline := time.Now().Add(5 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		savedJob, err := store.GetJob(ctx, "job-race-simul")
		if err == nil && savedJob != nil && savedJob.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected job to be in PAUSED_ERROR state")
	}

	client.currentModeMu.Lock()
	mode := client.currentMode
	client.currentModeMu.Unlock()

	if mode != ui_ipc.ModeError {
		t.Fatalf("expected currentMode to be ModeError, got %s", mode)
	}
}

// TestHealthScreen_DuplicateErrorHandling_ExactlyOnce verifies that if both the active
// health poller AND the print execution return a hardware interruption error,
// handleJobError executes exactly once and PAUSED_ERROR is preserved without double-transitions.
func TestHealthScreen_DuplicateErrorHandling_ExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	hSum := sha256.Sum256([]byte("job-dup-error"))
	finalName := hex.EncodeToString(hSum[:]) + ".pdf"
	if err := os.WriteFile(filepath.Join(tmpDir, finalName), pdfContent, 0644); err != nil {
		t.Fatalf("failed to pre-seed PDF file: %v", err)
	}

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper feed jam detected in printer",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		return errors.New("hardware interruption: paper feed jam detected in printer")
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-dup-error",
			KioskID:        "kiosk-dup",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	cfg := ClientConfig{
		CloudURL:           "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:            "kiosk-dup",
		StateStore:         store,
		CUPSQueue:          "HPTank",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/dup",
		HealthPollInterval: 10 * time.Millisecond,
		ScreenSink:         func(*image.RGBA) error { return nil },
		MockHealth:         healthMock,
		MockCUPSPrint:      mockCUPS,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		savedJob, err := store.GetJob(ctx, "job-dup-error")
		if err == nil && savedJob != nil && savedJob.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected job to be in PAUSED_ERROR state")
	}

	errorRenderCount := 0
	for _, st := range client.RenderHistory() {
		if st.Mode == ui_ipc.ModeError {
			errorRenderCount++
		}
	}
	if errorRenderCount != 1 {
		t.Fatalf("expected ModeError to be rendered exactly once, rendered %d times", errorRenderCount)
	}
}

// TestHealthScreen_JamDuringCUPS_PreservesCUPSJobWithoutCancel verifies that when a
// hardware jam interrupts a CUPS print job:
// 1. Local monitoring stops.
// 2. CUPS cancel command is NOT called, preserving the CUPS job in queue.
// 3. CUPS job ID is retained in SQLite state store.
// 4. Job status remains PAUSED_ERROR with uncertain physical output note for reconciliation.
func TestHealthScreen_JamDuringCUPS_PreservesCUPSJobWithoutCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	hSum := sha256.Sum256([]byte("job-preserve-cups"))
	finalName := hex.EncodeToString(hSum[:]) + ".pdf"
	if err := os.WriteFile(filepath.Join(tmpDir, finalName), pdfContent, 0644); err != nil {
		t.Fatalf("failed to pre-seed PDF file: %v", err)
	}

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var cancelInvoked atomic.Bool
	var holdInvoked atomic.Bool
	var jamReported atomic.Bool
	cupsJobID := "HPTank-777"
	printStarted := make(chan struct{})
	var printOnce sync.Once

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			if len(args) >= 4 && args[0] == "-i" && args[2] == "-H" && args[3] == "hold" {
				holdInvoked.Store(true)
				return []byte(""), nil
			}
			printOnce.Do(func() { close(printStarted) })
			return []byte("request id is " + cupsJobID + " (1 file(s))"), nil
		}
		if name == "cancel" {
			cancelInvoked.Store(true)
			return []byte(""), nil
		}
		if name == "lpstat" {
			if jamReported.Load() {
				return []byte("printer HPTank is idle. enabled since Thu Nov 13 12:00:00 2026\n\tPrinter state reasons: media-jam-error"), nil
			}
			return []byte("printer HPTank is idle. enabled since Thu Nov 13 12:00:00 2026"), nil
		}
		return []byte(""), nil
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-preserve-cups",
			KioskID:        "kiosk-pres",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		jamReported.Store(true)
	}()

	cfg := ClientConfig{
		CloudURL:           "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:            "kiosk-pres",
		StateStore:         store,
		CUPSQueue:          "HPTank",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/pres",
		HealthPollInterval: 15 * time.Millisecond,
		ScreenSink:         func(*image.RGBA) error { return nil },
		CUPSRunner:         customRunner,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	select {
	case <-printStarted:
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for print job to start")
	}

	deadline := time.Now().Add(5 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		savedJob, err := store.GetJob(ctx, "job-preserve-cups")
		if err == nil && savedJob != nil && savedJob.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected job to be in PAUSED_ERROR state")
	}

	// 1. Verify cancel was NOT invoked on CUPS
	if cancelInvoked.Load() {
		t.Fatal("CUPS cancel was invoked! Jammed job must be preserved in CUPS queue for manual reconciliation")
	}

	// 2. Verify hold WAS invoked on CUPS to prevent unmonitored printing
	if !holdInvoked.Load() {
		t.Fatal("CUPS hold was NOT invoked! Jammed job must be placed on hold in CUPS")
	}

	// 3. Verify CUPS request ID is preserved in SQLite store
	savedJob, err := store.GetJob(ctx, "job-preserve-cups")
	if err != nil || savedJob == nil {
		t.Fatalf("failed to retrieve job from store: %v", err)
	}
	if savedJob.CUPSJobID != cupsJobID {
		t.Fatalf("savedJob.CUPSJobID = %q, want %q", savedJob.CUPSJobID, cupsJobID)
	}
}

// TestHealthScreen_UnstableRecovery_TransientAndSNMPTimeoutRejected verifies that:
// 1. A single transient healthy reading does NOT clear an active hardware fault.
// 2. An SNMP timeout or unverified telemetry reading does NOT clear an active hardware fault.
// 3. Sensor flapping resets the consecutive recovery streak.
// 4. Reliable consecutive healthy readings clear the fault and refresh to ModeIdle.
func TestHealthScreen_UnstableRecovery_TransientAndSNMPTimeoutRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	var stateMu sync.Mutex
	currentHealth := contracts.PrinterHealth{
		Online:       false,
		State:        "ERROR",
		PaperJam:     true,
		ErrorMessage: "paper feed jam detected in printer",
		UpdatedAt:    time.Now().UTC(),
	}

	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		stateMu.Lock()
		defer stateMu.Unlock()
		h := currentHealth
		h.EstimatedSheets = sheets
		return h
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-unstable",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/idle",
		HealthPollInterval:           20 * time.Millisecond,
		RequiredRecoveryHealthyCount: 3,
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.setHardwareFault(true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer.")
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: "A paper feed issue occurred with the printer.",
	})

	// Case A: Single transient healthy reading
	stateMu.Lock()
	currentHealth = contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: time.Now().UTC()}
	stateMu.Unlock()

	client.CheckPrinterHealth(ctx)
	if !client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to remain active after 1 transient healthy reading")
	}

	// Case B: SNMP timeout during active fault
	stateMu.Lock()
	currentHealth = contracts.PrinterHealth{Online: true, State: "IDLE", ErrorMessage: "snmp connection timeout", UpdatedAt: time.Now().UTC()}
	stateMu.Unlock()

	client.CheckPrinterHealth(ctx)
	if !client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to remain active after SNMP timeout")
	}

	// Case C: Sensor flapping (healthy -> jam -> healthy -> healthy)
	stateMu.Lock()
	currentHealth = contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: time.Now().UTC()}
	stateMu.Unlock()
	client.CheckPrinterHealth(ctx) // healthy 1

	stateMu.Lock()
	currentHealth = contracts.PrinterHealth{Online: true, State: "ERROR", PaperJam: true, ErrorMessage: "paper jam", UpdatedAt: time.Now().UTC()}
	stateMu.Unlock()
	client.CheckPrinterHealth(ctx) // jam: reset streak!

	if !client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to remain active on jam")
	}

	// Case D: Reliable recovery with 3 consecutive healthy readings
	stateMu.Lock()
	currentHealth = contracts.PrinterHealth{Online: true, State: "IDLE", PaperTrayStatus: "OK", UpdatedAt: time.Now().UTC()}
	stateMu.Unlock()

	client.CheckPrinterHealth(ctx) // healthy 1
	if !client.isHardwareFaultActive() {
		t.Fatal("expected fault active at healthy streak = 1")
	}
	client.CheckPrinterHealth(ctx) // healthy 2
	if !client.isHardwareFaultActive() {
		t.Fatal("expected fault active at healthy streak = 2")
	}
	client.CheckPrinterHealth(ctx) // healthy 3 -> threshold met!
	if client.isHardwareFaultActive() {
		t.Fatal("expected fault to be cleared after 3 consecutive healthy readings")
	}

	client.currentModeMu.Lock()
	mode := client.currentMode
	client.currentModeMu.Unlock()

	if mode != ui_ipc.ModeIdle {
		t.Fatalf("expected currentMode to be ModeIdle after reliable recovery, got %s", mode)
	}
}

// TestHealthScreen_BackgroundAndJobPolling_CoordinatedScreen verifies that background
// health checks and active job error handling do not conflict or clobber order details.
func TestHealthScreen_BackgroundAndJobPolling_CoordinatedScreen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	hSum := sha256.Sum256([]byte("ord_coord_456"))
	finalName := hex.EncodeToString(hSum[:]) + ".pdf"
	if err := os.WriteFile(filepath.Join(tmpDir, finalName), pdfContent, 0644); err != nil {
		t.Fatalf("failed to pre-seed PDF file: %v", err)
	}

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var jamReported atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if jamReported.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "ERROR",
				PaperJam:        true,
				ErrorMessage:    "paper feed jam detected in printer",
				EstimatedSheets: sheets,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		if onPage != nil {
			onPage(1)
		}
		jamReported.Store(true)
		<-ctx.Done()
		return ctx.Err()
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		jobCmd := contracts.PrintJobCommand{
			JobID:          "ord_coord_456",
			KioskID:        "kiosk-coord",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	cfg := ClientConfig{
		CloudURL:           "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:            "kiosk-coord",
		StateStore:         store,
		CUPSQueue:          "HPTank",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/coord",
		HealthPollInterval: 10 * time.Millisecond,
		ScreenSink:         func(*image.RGBA) error { return nil },
		MockHealth:         healthMock,
		MockCUPSPrint:      mockCUPS,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		savedJob, err := store.GetJob(ctx, "ord_coord_456")
		if err == nil && savedJob != nil && savedJob.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected job ord_coord_456 to be in PAUSED_ERROR state")
	}

	history := client.RenderHistory()
	var finalError ui_ipc.ScreenState
	found := false
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Mode == ui_ipc.ModeError {
			finalError = history[i]
			found = true
			break
		}
	}

	if !found {
		t.Fatal("expected ModeError screen in render history")
	}

	if !strings.Contains(finalError.Detail, "ord_coord_456") {
		t.Fatalf("expected error screen to retain Order Ref 'ord_coord_456', got detail: %q", finalError.Detail)
	}
}

// TestHealthScreen_IndependentCUPSRecovery_ReconcilesSheetsWithoutAutoCompleting verifies that:
// When the printer hardware recovers independently and CUPS has finished the job,
// the client reconciles the actual sheets printed into the state store without double-counting,
// but NEVER automatically marks the interrupted job COMPLETED.
func TestHealthScreen_IndependentCUPSRecovery_ReconcilesSheetsWithoutAutoCompleting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_recov_001",
		KioskID:        "kiosk-recov",
		TotalPages:     4,
		TotalSheets:    4,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("b", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-999")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			// CUPS reports job completed with 4 sheets
			if len(args) >= 3 && args[1] == "completed" {
				return []byte("HPTank-999 user 1024 Thu Nov 13 12:00:00 2026\n\tjob-completed-successfully\n\tjob-media-sheets-completed = 4"), nil
			}
			return []byte(""), nil
		}
		if name == "ipptool" {
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 4\n"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-recov",
		StateStore:       store,
		CUPSQueue:        "HPTank",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://kiosk.test/recov",
		ScreenSink:       func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)
	if !client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to be active")
	}

	// 2. Transition printer hardware to recovered
	isRecovered.Store(true)

	// Call CheckPrinterHealth 3 times to satisfy required recovery count
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	if client.isHardwareFaultActive() {
		t.Fatal("expected hardware fault to clear after 3 consecutive healthy readings")
	}

	// 3. Verify state store: job MUST REMAIN in PAUSED_ERROR (never completed), and sheets MUST REMAIN 1 (never promoted to unverified CUPS counter 4)
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to remain in PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 1 {
		t.Fatalf("expected SheetsPrinted to remain 1 (verified physical progress), got %d (promoted unverified CUPS counter)", saved.SheetsPrinted)
	}
	if saved.PagesPrinted != 1 {
		t.Fatalf("expected PagesPrinted to remain 1, got %d", saved.PagesPrinted)
	}
	if saved.CUPSJobID != "HPTank-999" {
		t.Fatalf("expected CUPSJobID=HPTank-999, got %s", saved.CUPSJobID)
	}
	if !strings.Contains(saved.ErrorReason, "physical output uncertain") {
		t.Fatalf("expected ErrorReason to mention physical output uncertain, got: %q", saved.ErrorReason)
	}
}

// TestHealthScreen_IndependentCUPSResume_HoldsActiveJob verifies that:
// When the printer hardware recovers independently and CUPS has resumed an existing job (processing/pending),
// the client immediately places the CUPS job on hold to prevent unmonitored printing.
func TestHealthScreen_IndependentCUPSResume_HoldsActiveJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_resume_hold_002",
		KioskID:        "kiosk-hold",
		TotalPages:     4,
		TotalSheets:    4,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("c", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-888")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	var holdCalled atomic.Bool
	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" && len(args) >= 4 && args[0] == "-i" && args[2] == "-H" && args[3] == "hold" {
			if args[1] == "HPTank-888" {
				holdCalled.Store(true)
			}
			return []byte(""), nil
		}
		if name == "lpstat" {
			// CUPS reports job active in queue
			return []byte("HPTank-888 user 1024 Thu Nov 13 12:00:00 2026\nnow printing HPTank-888"), nil
		}
		if name == "ipptool" {
			return []byte("job-state (enum) = processing\njob-state-reasons (keyword) = none\njob-media-sheets-completed (integer) = 2\n"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:         "ws://127.0.0.1:8080/ws",
		KioskID:          "kiosk-hold",
		StateStore:       store,
		CUPSQueue:        "HPTank",
		DownloadDir:      tmpDir,
		IdleQRContent:    "https://kiosk.test/hold",
		ScreenSink:       func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Recover printer
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify hold was invoked on CUPS job
	if !holdCalled.Load() {
		t.Fatal("expected HoldCUPSJob to be called on active CUPS job upon independent printer recovery")
	}

	// 4. Verify job remains safely in PAUSED_ERROR with verified physical sheets (not promoted to CUPS counter)
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 1 {
		t.Fatalf("expected SheetsPrinted to remain 1 (verified physical progress), got %d (promoted unverified CUPS counter)", saved.SheetsPrinted)
	}
}

// TestHealthScreen_ConcurrentCheckThenRender_ErrorScreenNeverOverwritten proves that:
// Concurrent calls to renderScreen(ModePrinting) cannot race with hardware-fault transitions
// to overwrite ModeError.
func TestHealthScreen_ConcurrentCheckThenRender_ErrorScreenNeverOverwritten(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "state.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}

		var lastScreenState ui_ipc.ScreenState
		var screenStateMu sync.Mutex

		cfg := ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-race-render",
			StateStore:    store,
			CUPSQueue:     "HPTank",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk.test/race",
			ScreenSink: func(img *image.RGBA) error {
				return nil
			},
		}

		client, err := NewClient(cfg)
		if err != nil {
			store.Close()
			t.Fatalf("NewClient failed: %v", err)
		}

		client.cfg.ScreenSink = func(img *image.RGBA) error {
			screenStateMu.Lock()
			client.currentModeMu.Lock()
			mode := client.currentMode
			client.currentModeMu.Unlock()
			lastScreenState.Mode = mode
			screenStateMu.Unlock()
			return nil
		}

		// Initial idle render
		_ = client.renderScreen(ui_ipc.ScreenState{
			Mode:    ui_ipc.ModeIdle,
			Message: "Ready to print",
		})

		var wg sync.WaitGroup
		stopPrinting := make(chan struct{})

		// Start 5 concurrent goroutines calling renderScreen(ModePrinting)
		for g := 0; g < 5; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				page := 1
				for {
					select {
					case <-stopPrinting:
						return
					default:
						_ = client.renderScreen(ui_ipc.ScreenState{
							Mode:            ui_ipc.ModePrinting,
							CurrentPage:     page,
							TotalPages:      10,
							ProgressPercent: page * 10,
							Message:         "Printing document...",
							Detail:          fmt.Sprintf("Worker %d", id),
						})
						page = (page % 10) + 1
						time.Sleep(1 * time.Millisecond)
					}
				}
			}(g)
		}

		// Let printing frames render briefly
		time.Sleep(10 * time.Millisecond)

		// Fault transition: set hardware fault active and render ModeError
		client.setHardwareFault(true, "paper feed jam detected in printer", "A paper feed issue occurred with the printer.")
		_ = client.renderScreen(ui_ipc.ScreenState{
			Mode:    ui_ipc.ModeError,
			Message: "A paper feed issue occurred with the printer.",
			Detail:  "Order Ref: ord_race_123 • Completed: 3 of 10 pages",
		})

		// Stop printing goroutines
		close(stopPrinting)
		wg.Wait()

		// Verify final mode is ModeError
		client.currentModeMu.Lock()
		finalMode := client.currentMode
		client.currentModeMu.Unlock()

		if finalMode != ui_ipc.ModeError {
			store.Close()
			t.Fatalf("iteration %d: expected finalMode ModeError, got %s", iter, finalMode)
		}

		screenStateMu.Lock()
		sinkMode := lastScreenState.Mode
		screenStateMu.Unlock()

		if sinkMode != ui_ipc.ModeError {
			store.Close()
			t.Fatalf("iteration %d: expected ScreenSink final state ModeError, got %s", iter, sinkMode)
		}

		store.Close()
	}
}

// TestHealthScreen_ModeIdleSuppressedDuringActiveHardwareFault verifies that
// ModeIdle frames are suppressed from rendering while a hardware fault is active.
func TestHealthScreen_ModeIdleSuppressedDuringActiveHardwareFault(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cfg := ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-idle-suppress",
		StateStore:    store,
		CUPSQueue:     "HPTank",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/idle",
		ScreenSink:    func(*image.RGBA) error { return nil },
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Hardware fault active and ModeError rendered
	client.setHardwareFault(true, "printer cover is open", "The printer cover was opened.")
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: "The printer cover was opened.",
		Detail:  "A printer hardware fault requires attention.",
	})

	historyLen := len(client.RenderHistory())

	// 2. Attempt to render ModeIdle
	_ = client.renderScreen(ui_ipc.ScreenState{
		Mode:      ui_ipc.ModeIdle,
		QRContent: "https://kiosk.test/idle",
		Message:   "Ready to print",
	})

	// History must NOT have grown
	if len(client.RenderHistory()) != historyLen {
		t.Fatalf("expected render history length %d, got %d (ModeIdle was not suppressed)", historyLen, len(client.RenderHistory()))
	}

	// Mode must remain ModeError
	client.currentModeMu.Lock()
	mode := client.currentMode
	client.currentModeMu.Unlock()

	if mode != ui_ipc.ModeError {
		t.Fatalf("expected currentMode to remain ModeError, got %s", mode)
	}
}

// TestHealthScreen_HoldCommandFailure_PreservesPausedErrorAndReportsRisk verifies that:
// When the lp hold command fails, the error is NOT silently ignored; the job strictly
// remains in PAUSED_ERROR with its CUPS job ID preserved and the unresolved risk reported.
func TestHealthScreen_HoldCommandFailure_PreservesPausedErrorAndReportsRisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_hold_fail_01",
		KioskID:        "kiosk-fail",
		TotalPages:     5,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("d", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-fail-77")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" && len(args) >= 4 && args[0] == "-i" && args[2] == "-H" && args[3] == "hold" {
			return []byte("lp: Operation not permitted"), errors.New("exit status 1")
		}
		if name == "lpstat" {
			return []byte("HPTank-fail-77 user 1024 Thu Nov 13 12:00:00 2026\nnow printing HPTank-fail-77"), nil
		}
		if name == "ipptool" {
			return []byte("job-state (enum) = processing\njob-state-reasons (keyword) = none\njob-media-sheets-completed (integer) = 2\n"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-fail",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/fail",
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Clear hardware fault
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify state store: job remains in PAUSED_ERROR, hold failure reported, CUPS ID preserved
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.CUPSJobID != "HPTank-fail-77" {
		t.Fatalf("expected CUPSJobID HPTank-fail-77, got %s", saved.CUPSJobID)
	}
	if !strings.Contains(saved.ErrorReason, "hold request failed") {
		t.Fatalf("expected ErrorReason to report hold failure, got: %q", saved.ErrorReason)
	}
	if !strings.Contains(saved.ErrorReason, "unmonitored physical output risk") {
		t.Fatalf("expected ErrorReason to report unmonitored physical output risk, got: %q", saved.ErrorReason)
	}
}

// TestHealthScreen_JobRemainingProcessingAfterHold_PreservesPausedError verifies that:
// When CUPS accepts the hold request but the authoritative state remains processing,
// the client reports the unresolved sheet ejection risk and preserves PAUSED_ERROR.
func TestHealthScreen_JobRemainingProcessingAfterHold_PreservesPausedError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_still_proc_02",
		KioskID:        "kiosk-proc",
		TotalPages:     6,
		TotalSheets:    6,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("e", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-proc-88")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" && len(args) >= 4 && args[0] == "-i" && args[2] == "-H" && args[3] == "hold" {
			// Hold accepted by CLI runner
			return []byte(""), nil
		}
		if name == "ipptool" {
			// But CUPS scheduler still reports processing (backend buffer still active)
			return []byte("job-state (enum) = processing\njob-state-reasons (keyword) = none\njob-media-sheets-completed (integer) = 3\n"), nil
		}
		if name == "lpstat" {
			return []byte("HPTank-proc-88 user 1024 Thu Nov 13 12:00:00 2026\nnow printing HPTank-proc-88"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-proc",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/proc",
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Clear hardware fault
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify state store: job remains in PAUSED_ERROR, sheets MUST REMAIN 2 (never promoted to unverified CUPS counter 3 or total 6), risk reported
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 2 {
		t.Fatalf("expected SheetsPrinted to remain 2 (verified physical progress), got %d (promoted unverified CUPS counter)", saved.SheetsPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "job remains processing in CUPS") {
		t.Fatalf("expected ErrorReason to mention job remains processing, got: %q", saved.ErrorReason)
	}
	if !strings.Contains(saved.ErrorReason, "unmonitored physical output risk") {
		t.Fatalf("expected ErrorReason to report unmonitored physical output risk, got: %q", saved.ErrorReason)
	}
}

// TestHealthScreen_UncertainHeldState_PreservesPausedError verifies that:
// When CUPS queries return unknown / ambiguous state after a hold attempt,
// the job is preserved in PAUSED_ERROR with operator reconciliation required.
func TestHealthScreen_UncertainHeldState_PreservesPausedError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_ambig_03",
		KioskID:        "kiosk-ambig",
		TotalPages:     5,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("f", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-ambig-99")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" && len(args) >= 4 && args[0] == "-i" && args[2] == "-H" && args[3] == "hold" {
			return []byte(""), nil
		}
		// Query fails / returns unknown
		return []byte(""), errors.New("CUPS communication failure")
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-ambig",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/ambig",
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Clear hardware fault
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify state store: job remains in PAUSED_ERROR with sheets not regressed
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 1 {
		t.Fatalf("expected SheetsPrinted=1, got %d", saved.SheetsPrinted)
	}
}

// TestHealthScreen_CompletedCUPSWithoutPhysicalProof_NeverSynthesizesTotalSheets verifies that:
// When CUPS reports completed but provides no numeric physical proof (or 0 sheets completed),
// the client NEVER synthesizes full sheet completion (TotalSheets) and preserves PAUSED_ERROR.
func TestHealthScreen_CompletedCUPSWithoutPhysicalProof_NeverSynthesizesTotalSheets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_no_proof_04",
		KioskID:        "kiosk-noproof",
		TotalPages:     10,
		TotalSheets:    10,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-noproof-10")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			// Completed without media-sheets-completed (or 0)
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\n"), nil
		}
		if name == "lpstat" {
			return []byte("HPTank-noproof-10 user 1024 Thu Nov 13 12:00:00 2026\n\tjob-completed-successfully"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: 100,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: 100,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-noproof",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/noproof",
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Clear hardware fault
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify state store: sheets MUST REMAIN 2 (never synthesized to 10!) and status MUST BE PAUSED_ERROR (never COMPLETED)
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 2 {
		t.Fatalf("CRITICAL SAFETY FAILURE: expected SheetsPrinted to remain 2 (verified physical), but got %d (synthesized full completion)", saved.SheetsPrinted)
	}
	if saved.PagesPrinted != 2 {
		t.Fatalf("expected PagesPrinted to remain 2, got %d", saved.PagesPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "without physical proof") {
		t.Fatalf("expected ErrorReason to mention without physical proof, got: %q", saved.ErrorReason)
	}
}

// TestHealthScreen_StartupRecovery_ReconcilesWithoutAutoCompletingOrSynthesizingSheets verifies that:
// During startup recovery of an in-flight or paused job where CUPS reports completed,
// the client never synthesizes full TotalSheets and never marks the job completed without physical proof.
func TestHealthScreen_StartupRecovery_ReconcilesWithoutAutoCompletingOrSynthesizingSheets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_startup_safety_05",
		KioskID:        "kiosk-startup",
		TotalPages:     8,
		TotalSheets:    8,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("1", 64),
	}
	// Seed job in PAUSED_ERROR status (interrupted by fault, daemon restarted)
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-startup-55")
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			// CUPS reports completed with 4 sheets completed (out of 8)
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 4\n"), nil
		}
		if name == "lpstat" {
			return []byte("HPTank-startup-55 user 1024 Thu Nov 13 12:00:00 2026\n\tjob-completed-successfully\n\tjob-media-sheets-completed = 4"), nil
		}
		return []byte(""), nil
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	cfg := ClientConfig{
		CloudURL:                     "ws" + strings.TrimPrefix(wsServer.URL, "http"),
		KioskID:                      "kiosk-startup",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/startup",
		HealthPollInterval:           10 * time.Millisecond,
		ScreenSink:                   func(*image.RGBA) error { return nil },
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for startup reconciliation to process incomplete job
	deadline := time.Now().Add(2 * time.Second)
	var saved *state.StoredJob
	for time.Now().Before(deadline) {
		saved, err = store.GetJob(ctx, cmd.JobID)
		if err == nil && saved != nil && strings.Contains(saved.ErrorReason, "without physical proof") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if saved == nil || saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to be preserved in PAUSED_ERROR during startup recovery, got status: %v", saved)
	}
	// Verified physical sheets must NOT be promoted to CUPS unverified counter (4)
	if saved.SheetsPrinted != 2 {
		t.Fatalf("expected SheetsPrinted to remain 2 (verified physical progress), got %d (promoted unverified CUPS counter)", saved.SheetsPrinted)
	}
	if saved.PagesPrinted != 2 {
		t.Fatalf("expected PagesPrinted to remain 2, got %d", saved.PagesPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "without physical proof") {
		t.Fatalf("expected ErrorReason to mention without physical proof, got: %q", saved.ErrorReason)
	}
	if !strings.Contains(saved.ErrorReason, "CUPS counter: 4 sheets") {
		t.Fatalf("expected ErrorReason to report CUPS counter separately, got: %q", saved.ErrorReason)
	}

	// Verify paper tray inventory was NOT decremented for unverified CUPS sheets
	stk, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("failed to query estimated sheets: %v", err)
	}
	if stk != 98 {
		t.Fatalf("expected estimated sheets to remain 98 (100 - 2 verified physical sheets), got %d", stk)
	}
}

// TestHealthScreen_UnverifiedCUPSCounters_CannotDecrementInventoryOrFabricatePhysicalSheets verifies that:
// During independent printer recovery, when CUPS reports completed with an unverified sheet counter,
// the client never promotes the counter into SheetsPrinted, never decrements paper inventory,
// and strictly preserves PAUSED_ERROR with verified physical progress.
func TestHealthScreen_UnverifiedCUPSCounters_CannotDecrementInventoryOrFabricatePhysicalSheets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "ord_inv_safety_07",
		KioskID:        "kiosk-inv",
		TotalPages:     10,
		TotalSheets:    10,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("7", 64),
	}
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-inv-77")
	// 3 sheets physically printed prior to interruption
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  3,
		SheetsPrinted: 3,
		ErrorReason:   "paper feed jam detected in printer",
		Timestamp:     time.Now().UTC(),
	})

	stkBefore, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stkBefore != 97 {
		t.Fatalf("expected inventory 97 before recovery, got %d", stkBefore)
	}

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			// CUPS reports completed with 10 sheets completed (unverified counter)
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 10\n"), nil
		}
		if name == "lpstat" {
			return []byte("HPTank-inv-77 user 1024 Thu Nov 13 12:00:00 2026\n\tjob-completed-successfully\n\tjob-media-sheets-completed = 10"), nil
		}
		return []byte(""), nil
	}

	var isRecovered atomic.Bool
	healthMock := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if isRecovered.Load() {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: sheets,
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperJam:        true,
			ErrorMessage:    "paper jam",
			EstimatedSheets: sheets,
			UpdatedAt:       time.Now().UTC(),
		}
	}

	cfg := ClientConfig{
		CloudURL:                     "ws://127.0.0.1:8080/ws",
		KioskID:                      "kiosk-inv",
		StateStore:                   store,
		CUPSQueue:                    "HPTank",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/inv",
		HealthPollInterval:           10 * time.Millisecond,
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   healthMock,
		CUPSRunner:                   customRunner,
		RequiredRecoveryHealthyCount: 3,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Prime hardware fault
	_ = client.CheckPrinterHealth(ctx)

	// 2. Clear hardware fault with 3 consecutive healthy readings
	isRecovered.Store(true)
	for i := 0; i < 3; i++ {
		_ = client.CheckPrinterHealth(ctx)
	}

	// 3. Verify state store: sheets MUST REMAIN 3 (never promoted to 10!) and status MUST BE PAUSED_ERROR
	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 3 {
		t.Fatalf("CRITICAL SAFETY FAILURE: expected SheetsPrinted to remain 3 (verified physical), but got %d (promoted unverified CUPS counter)", saved.SheetsPrinted)
	}
	if saved.PagesPrinted != 3 {
		t.Fatalf("expected PagesPrinted to remain 3, got %d", saved.PagesPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "without physical proof") {
		t.Fatalf("expected ErrorReason to mention without physical proof, got: %q", saved.ErrorReason)
	}
	if !strings.Contains(saved.ErrorReason, "CUPS counter: 10 sheets") {
		t.Fatalf("expected ErrorReason to mention CUPS counter: 10 sheets, got: %q", saved.ErrorReason)
	}

	// 4. CRITICAL INVENTORY CHECK: Paper inventory must NOT have decremented from 97 to 90!
	stkAfter, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stkAfter != 97 {
		t.Fatalf("CRITICAL INVENTORY LEAK: expected stock to remain 97, but got %d (decremented for unverified sheets!)", stkAfter)
	}
}

// TestCompletionAccounting_CUPSCompleted_PartialOdometerProgress_PreservesPausedErrorAndConservesInventory
// proves that when CUPS completes its queue processing but the hardware odometer only confirms
// partial progress (e.g. 2 of 4 sheets), the job is NOT marked COMPLETED; it is retained in
// PAUSED_ERROR with verified physical sheets (2), and remaining paper inventory is CONSERVED (no deduction).
func TestCompletionAccounting_CUPSCompleted_PartialOdometerProgress_PreservesPausedErrorAndConservesInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "partial_odo.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-partial-odo-01",
		KioskID:        "kiosk-odo",
		DisplayCode:    "1234",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     8,
		TotalSheets:    4,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			return []byte("request id is HPTank-part-01 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-part-01 user 1024 Thu Nov 14 12:00:00 2026\n\tjob-completed-successfully"), nil
				}
				if arg == "-p" {
					return []byte("printer HPTank is idle. enabled"), nil
				}
			}
		}
		return []byte(""), nil
	}

	// Hardware odometer only confirms 2 sheets (baseline 1000, final 1002)
	var odoCalls int32
	poller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 1000
		if c > 1 {
			life = 1002 // delta = 2 sheets (out of 4)
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	cfg := ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-odo",
		StateStore:          store,
		CUPSQueue:           "HPTank",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://kiosk.test/ready",
		ScreenSink:          func(*image.RGBA) error { return nil },
		CUPSRunner:          runner,
		SNMPPoller:          poller,
		EnableCloudRecovery: true,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.processJob(ctx, cmd)

	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to be in PAUSED_ERROR due to incomplete physical output, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 2 {
		t.Fatalf("expected SheetsPrinted to reflect verified physical progress (2), got %d", saved.SheetsPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "physical output incomplete") || !strings.Contains(saved.ErrorReason, "verified physical: 2/4") {
		t.Fatalf("expected ErrorReason to detail incomplete physical output (2/4), got %q", saved.ErrorReason)
	}

	// CRITICAL INVENTORY CONSERVATION: Stock must be 98 (100 - 2 verified sheets).
	// The 2 unverified sheets must NEVER be deducted!
	stk, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stk != 98 {
		t.Fatalf("CRITICAL INVENTORY LEAK: expected stock 98 (conserved for unverified sheets), got %d", stk)
	}

	// Screen must be ModeError
	history := client.RenderHistory()
	if len(history) == 0 {
		t.Fatal("expected screen frames rendered")
	}
	lastScreen := history[len(history)-1]
	if lastScreen.Mode != ui_ipc.ModeError {
		t.Fatalf("expected last screen ModeError, got %v", lastScreen.Mode)
	}
}

// TestCompletionAccounting_CUPSCompleted_MissingOdometer_PreservesPausedErrorAndConservesInventory
// proves that when CUPS completes its queue processing without an SNMP odometer, physical output
// cannot be verified; the job transitions to PAUSED_ERROR with 0 physical sheets, and paper inventory
// is completely CONSERVED (0 sheets deducted).
func TestCompletionAccounting_CUPSCompleted_MissingOdometer_PreservesPausedErrorAndConservesInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "missing_odo.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-missing-odo-01",
		KioskID:        "kiosk-noodo",
		DisplayCode:    "5678",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     6,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			return []byte("request id is HPTank-noodo-01 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-noodo-01 user 1024 Thu Nov 14 12:00:00 2026\n\tjob-completed-successfully"), nil
				}
				if arg == "-p" {
					return []byte("printer HPTank is idle. enabled"), nil
				}
			}
		}
		if name == "ipptool" {
			// IPP reports 3 sheets completed, but no odometer exists
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 3\n"), nil
		}
		return []byte(""), nil
	}

	// No SNMP poller configured: odometer is missing
	cfg := ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-noodo",
		StateStore:          store,
		CUPSQueue:           "HPTank",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://kiosk.test/ready",
		ScreenSink:          func(*image.RGBA) error { return nil },
		CUPSRunner:          runner,
		SNMPPoller:          nil, // MISSING ODONOMETER
		EnableCloudRecovery: true,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.processJob(ctx, cmd)

	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job to be in PAUSED_ERROR due to missing odometer proof, got %s", saved.Status)
	}
	if saved.SheetsPrinted != 0 {
		t.Fatalf("expected SheetsPrinted to remain 0 (no synthetic physical sheets), got %d", saved.SheetsPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "without physical sheet verification") || !strings.Contains(saved.ErrorReason, "verified physical: 0/3") {
		t.Fatalf("expected ErrorReason to detail missing physical sheet verification (0/3), got %q", saved.ErrorReason)
	}

	// CRITICAL INVENTORY CONSERVATION: Stock must remain untouched at 100!
	stk, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stk != 100 {
		t.Fatalf("CRITICAL INVENTORY LEAK: expected stock 100 (conserved), got %d", stk)
	}
}

// TestCompletionAccounting_RestartRecovery_WithoutPhysicalProof_PreservesPausedErrorAndConservesInventory
// proves that when the daemon restarts and finds a CUPS job completed, but the local state store
// only recorded partial progress prior to restart (e.g. 1 of 5 sheets), startup reconciliation
// NEVER promotes sheets or auto-completes; it preserves PAUSED_ERROR and conserves inventory.
func TestCompletionAccounting_RestartRecovery_WithoutPhysicalProof_PreservesPausedErrorAndConservesInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "rec_noproof.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-rec-noproof-01",
		KioskID:        "kiosk-rec",
		DisplayCode:    "9999",
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: strings.Repeat("f", 64),
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Seed job before daemon crash: in PRINTING, 1 sheet verified physically
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.RecordJobSubmission(ctx, cmd.JobID, "HPTank-rec-99"); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}
	if err := store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordSheetProgress failed: %v", err)
	}

	stkBefore, _ := store.GetEstimatedSheets(ctx)
	if stkBefore != 99 {
		t.Fatalf("expected stock 99 before restart, got %d", stkBefore)
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-rec-99 user 1024 Thu Nov 14 12:00:00 2026\n\tjob-completed-successfully"), nil
				}
				if arg == "-p" {
					return []byte("printer HPTank is idle. enabled"), nil
				}
			}
		}
		if name == "ipptool" {
			return []byte("job-state (enum) = completed\njob-state-reasons (keyword) = job-completed-successfully\njob-media-sheets-completed (integer) = 5\n"), nil
		}
		return []byte(""), nil
	}

	cfg := ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-rec",
		StateStore:          store,
		CUPSQueue:           "HPTank",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://kiosk.test/ready",
		ScreenSink:          func(*image.RGBA) error { return nil },
		CUPSRunner:          runner,
		EnableCloudRecovery: true,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		job, err := store.GetJob(ctx, cmd.JobID)
		if err == nil && job != nil && job.Status == contracts.JobStatusPausedError {
			paused = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if !paused {
		t.Fatal("expected startup recovery to preserve job in PAUSED_ERROR without physical proof")
	}

	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.SheetsPrinted != 1 {
		t.Fatalf("CRITICAL SAFETY FAILURE: expected SheetsPrinted to remain 1 (verified physical), got %d (promoted unverified sheets)", saved.SheetsPrinted)
	}
	if !strings.Contains(saved.ErrorReason, "without physical proof") {
		t.Fatalf("expected ErrorReason to mention without physical proof, got %q", saved.ErrorReason)
	}

	// CRITICAL INVENTORY CHECK: Paper inventory must stay at 99 (never deduct 4 unverified sheets)
	stkAfter, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stkAfter != 99 {
		t.Fatalf("CRITICAL INVENTORY LEAK: expected stock to remain 99, but got %d", stkAfter)
	}
}

// TestCompletionAccounting_CUPSCompleted_FullOdometerProof_CompletesAndDeductsInventoryAccurately
// proves that when the hardware odometer confirms that all sheets were physically ejected (4 of 4 sheets),
// the job transitions to COMPLETED and paper inventory is accurately decremented by 4 sheets.
func TestCompletionAccounting_CUPSCompleted_FullOdometerProof_CompletesAndDeductsInventoryAccurately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "full_odo.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.ResetPaperTray(ctx, 100); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	cmd := contracts.PrintJobCommand{
		JobID:          "job-full-odo-01",
		KioskID:        "kiosk-full",
		DisplayCode:    "4321",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     8,
		TotalSheets:    4,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			return []byte("request id is HPTank-full-01 (1 file(s))"), nil
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "completed" {
					return []byte("HPTank-full-01 user 1024 Thu Nov 14 12:00:00 2026\n\tjob-completed-successfully"), nil
				}
				if arg == "-p" {
					return []byte("printer HPTank is idle. enabled"), nil
				}
			}
		}
		return []byte(""), nil
	}

	// Hardware odometer confirms all 4 sheets (baseline 1000 -> 1004)
	var odoCalls int32
	poller := func(ctx context.Context) printer.HardwareTelemetry {
		c := atomic.AddInt32(&odoCalls, 1)
		var life int64 = 1000
		if c > 1 {
			life = 1004
		}
		return printer.HardwareTelemetry{
			Online:    true,
			HasLife:   true,
			LifeCount: life,
		}
	}

	cfg := ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-full",
		StateStore:          store,
		CUPSQueue:           "HPTank",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://kiosk.test/ready",
		ScreenSink:          func(*image.RGBA) error { return nil },
		CUPSRunner:          runner,
		SNMPPoller:          poller,
		EnableCloudRecovery: true,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.processJob(ctx, cmd)

	saved, err := store.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("failed to query job: %v", err)
	}
	if saved.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected job to be COMPLETED with full odometer proof, got %s with reason %q", saved.Status, saved.ErrorReason)
	}
	if saved.SheetsPrinted != 4 {
		t.Fatalf("expected SheetsPrinted = 4, got %d", saved.SheetsPrinted)
	}
	if saved.PagesPrinted != 8 {
		t.Fatalf("expected PagesPrinted = 8, got %d", saved.PagesPrinted)
	}

	// CRITICAL INVENTORY DEDUCTION: Stock must be accurately decremented: 100 - 4 = 96
	stk, err := store.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if stk != 96 {
		t.Fatalf("expected stock 96 after complete physical printing, got %d", stk)
	}
}
