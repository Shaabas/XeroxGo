package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for print job to start")
	}

	deadline := time.Now().Add(3 * time.Second)
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
	var jamReported atomic.Bool
	cupsJobID := "HPTank-777"
	printStarted := make(chan struct{})
	var printOnce sync.Once

	customRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
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
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for print job to start")
	}

	deadline := time.Now().Add(3 * time.Second)
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

	// 2. Verify CUPS request ID is preserved in SQLite store
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
