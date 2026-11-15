package spooler

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
)

func TestStartupIPPPrinterState_Success(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	var stateQueried atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			stateQueried.Store(true)
			return []byte(`
status-code = successful-ok
printer-state (enum) = idle
printer-is-accepting-jobs (boolean) = true
printer-state-reasons (keyword) = none
`), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-state-test",
		StateStore:    store,
		CUPSQueue:     "BrotherQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-state",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	var st printer.IPPPrinterStateResult
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		st = client.IPPPrinterState()
		if st.State == printer.IPPPrinterStateIdle {
			break
		}
	}

	if !stateQueried.Load() {
		t.Fatal("expected ipptool to be queried during startup")
	}

	if st.Source != printer.CapabilitySourceCUPSQueue {
		t.Errorf("expected source %q, got %q", printer.CapabilitySourceCUPSQueue, st.Source)
	}
	if st.Queue != "BrotherQueue" {
		t.Errorf("expected queue BrotherQueue, got %q", st.Queue)
	}
	if st.State != printer.IPPPrinterStateIdle {
		t.Errorf("expected State idle, got %v", st.State)
	}
	if st.AcceptingJobs != printer.AcceptingJobsAccepting {
		t.Errorf("expected AcceptingJobs ACCEPTING, got %v", st.AcceptingJobs)
	}
	if !st.QueueAccessible {
		t.Errorf("expected QueueAccessible true")
	}
	if st.HasHardwareFault {
		t.Errorf("expected HasHardwareFault false")
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client.Run to terminate")
	}
}

func TestStartupIPPPrinterState_ToleratesFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	var ipptoolCalled atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			ipptoolCalled.Store(true)
			return nil, errors.New("exec: \"ipptool\": executable file not found in $PATH")
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-fail-state",
		StateStore:    store,
		CUPSQueue:     "FailingStateQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-fail-state",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		if ipptoolCalled.Load() {
			break
		}
	}

	if !ipptoolCalled.Load() {
		t.Fatal("expected ipptool to be attempted")
	}

	// Daemon continues running and does NOT adopt failed response as authoritative
	st := client.IPPPrinterState()
	if st.State != printer.IPPPrinterStateUnknown {
		t.Errorf("expected State UNKNOWN after failure, got %v", st.State)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client.Run to terminate")
	}
}

func TestStartupIPPPrinterState_ToleratesIPPErrorStatus(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	var ipptoolCalled atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			ipptoolCalled.Store(true)
			return []byte(`
status-code = client-error-not-found (The printer or class was not found.)
printer-state (enum) = unknown
`), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-ipperr-state",
		StateStore:    store,
		CUPSQueue:     "NotFoundStateQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-ipperr-state",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		if ipptoolCalled.Load() {
			break
		}
	}

	if !ipptoolCalled.Load() {
		t.Fatal("expected ipptool to be attempted")
	}

	st := client.IPPPrinterState()
	if st.State != printer.IPPPrinterStateUnknown {
		t.Errorf("expected State UNKNOWN after IPP error status, got %v", st.State)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client.Run to terminate")
	}
}

func TestStartupIPPPrinterState_ToleratesTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-timeout-state",
		StateStore:    store,
		CUPSQueue:     "HangingStateQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-timeout-state",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	start := time.Now()
	st := client.DiscoverStartupPrinterState(context.Background())
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("startup IPP state discovery took %v, exceeding bounded timeout", elapsed)
	}
	if st.State != printer.IPPPrinterStateUnknown {
		t.Errorf("expected State UNKNOWN after timeout, got %v", st.State)
	}
}

func TestStartupIPPPrinterState_RawPJLMode_NoIPPQuery(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	var ipptoolCalled atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			ipptoolCalled.Store(true)
			t.Errorf("ipptool must NOT be invoked in raw PJL-only mode")
			return []byte(""), errors.New("ipptool should not be invoked")
		}
		return []byte(""), nil
	}

	// PJL-only config: PrinterAddr specified, CUPSQueue empty
	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-pjl-state",
		StateStore:    store,
		PrinterAddr:   "192.168.1.100:9100",
		CUPSQueue:     "",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-pjl-state",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	if ipptoolCalled.Load() {
		t.Fatal("ipptool was invoked in raw PJL mode")
	}

	st := client.IPPPrinterState()
	if st.Source != "" {
		t.Errorf("expected empty IPP state source in PJL mode, got %q", st.Source)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client.Run to terminate")
	}
}
