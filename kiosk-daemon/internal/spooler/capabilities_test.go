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

func TestStartupCapabilityDiscovery_Success(t *testing.T) {
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
printer-name (nameWithoutLanguage) = BrotherQueue
document-format-supported (1setOf mimeMediaType) = application/pdf,application/postscript
media-supported (1setOf keyword) = iso_a4_210x297mm,na_letter_8.5x11in
sides-supported (1setOf keyword) = one-sided,two-sided-long-edge
print-color-mode-supported (1setOf keyword) = monochrome,color
`), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-test-caps",
		StateStore:    store,
		CUPSQueue:     "BrotherQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-test",
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

	// Wait until discovery finishes or timeout
	var caps printer.PrinterCapabilities
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		caps = client.Capabilities()
		if caps.PDFSupport == printer.CapabilitySupported {
			break
		}
	}

	if !ipptoolCalled.Load() {
		t.Fatal("expected ipptool to be invoked during startup capability discovery")
	}

	if caps.Source != printer.CapabilitySourceCUPSQueue {
		t.Errorf("expected source %q, got %q", printer.CapabilitySourceCUPSQueue, caps.Source)
	}
	if caps.Queue != "BrotherQueue" {
		t.Errorf("expected queue BrotherQueue, got %q", caps.Queue)
	}
	if caps.PDFSupport != printer.CapabilitySupported {
		t.Errorf("expected PDFSupport SUPPORTED, got %v", caps.PDFSupport)
	}
	if caps.A4Support != printer.CapabilitySupported {
		t.Errorf("expected A4Support SUPPORTED, got %v", caps.A4Support)
	}
	if caps.SimplexSupport != printer.CapabilitySupported {
		t.Errorf("expected SimplexSupport SUPPORTED, got %v", caps.SimplexSupport)
	}
	if caps.DuplexLongEdgeSupport != printer.CapabilitySupported {
		t.Errorf("expected DuplexLongEdgeSupport SUPPORTED, got %v", caps.DuplexLongEdgeSupport)
	}
	if caps.DuplexShortEdgeSupport != printer.CapabilityUnsupported {
		t.Errorf("expected DuplexShortEdgeSupport UNSUPPORTED, got %v", caps.DuplexShortEdgeSupport)
	}
	if caps.ColorSupport != printer.CapabilitySupported {
		t.Errorf("expected ColorSupport SUPPORTED, got %v", caps.ColorSupport)
	}
	if caps.MonochromeSupport != printer.CapabilitySupported {
		t.Errorf("expected MonochromeSupport SUPPORTED, got %v", caps.MonochromeSupport)
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

func TestStartupCapabilityDiscovery_ToleratesFailure(t *testing.T) {
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
		KioskID:       "kiosk-fail-caps",
		StateStore:    store,
		CUPSQueue:     "FailingQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-fail",
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

	// Wait for ipptool invocation to occur
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		if ipptoolCalled.Load() {
			break
		}
	}

	if !ipptoolCalled.Load() {
		t.Fatal("expected ipptool to be attempted")
	}

	// Daemon continues running despite discovery failure
	caps := client.Capabilities()
	if caps.PDFSupport != printer.CapabilityUnknown {
		t.Errorf("expected PDFSupport UNKNOWN after failure, got %v", caps.PDFSupport)
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

func TestStartupCapabilityDiscovery_ToleratesIPPErrorStatus(t *testing.T) {
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
			// Runner succeeds with exit 0, but response has IPP client-error status
			return []byte(`
status-code = client-error-not-found (The printer or class does not exist.)
printer-name (nameWithoutLanguage) = DisappearedQueue
document-format-supported (1setOf mimeMediaType) = application/pdf
`), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-ipperr-caps",
		StateStore:    store,
		CUPSQueue:     "DisappearedQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-ipperr",
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
	caps := client.Capabilities()
	if caps.PDFSupport != printer.CapabilityUnknown {
		t.Errorf("expected PDFSupport UNKNOWN after IPP error status, got %v", caps.PDFSupport)
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

func TestStartupCapabilityDiscovery_ToleratesTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := state.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to create state store: %v", err)
	}
	defer store.Close()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			// Simulate hung command that blocks until context is done
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-timeout-caps",
		StateStore:    store,
		CUPSQueue:     "HangingQueue",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-timeout",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	start := time.Now()
	// Call DiscoverStartupCapabilities directly to measure bounded timeout
	caps := client.DiscoverStartupCapabilities(context.Background())
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("startup capability discovery took %v, exceeding bounded timeout", elapsed)
	}
	if caps.PDFSupport != printer.CapabilityUnknown {
		t.Errorf("expected PDFSupport UNKNOWN after timeout, got %v", caps.PDFSupport)
	}
}

func TestStartupCapabilityDiscovery_RawPJLMode_NoIPPQuery(t *testing.T) {
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
		KioskID:       "kiosk-pjl-caps",
		StateStore:    store,
		PrinterAddr:   "192.168.1.100:9100",
		CUPSQueue:     "",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk-pjl",
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

	caps := client.Capabilities()
	if caps.Source != "" {
		t.Errorf("expected empty capabilities source in PJL mode, got %q", caps.Source)
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
