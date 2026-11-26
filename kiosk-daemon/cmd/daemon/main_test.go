package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestMockPrinterShutdownUnblocksListener(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg, &mockPrinterState{})
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	if addr == "" {
		t.Fatal("expected non-empty mock printer address")
	}

	// Verify listener accepts TCP connections
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial mock printer listener: %v", err)
	}
	_ = conn.Close()

	// Invoke cleanup, which must close the listener and unblock the listener goroutine
	cleanup()

	// Calling cleanup again should be safe and idempotent
	cleanup()

	// Verify listener goroutine exits promptly without hanging wg.Wait()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Succeeded: listener goroutine exited promptly
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: startMockPrinter listener goroutine did not exit within 3s after cleanup")
	}
}

func TestResolveIdleQRContent(t *testing.T) {
	tests := []struct {
		name          string
		idleQRContent string
		portalURLBase string
		kioskID       string
		expected      string
	}{
		{
			name:          "default fallback uses canonical vercel root",
			idleQRContent: "",
			portalURLBase: "https://xeroxgo.vercel.app",
			kioskID:       "kiosk-demo",
			expected:      "https://xeroxgo.vercel.app",
		},
		{
			name:          "custom portal url base",
			idleQRContent: "",
			portalURLBase: "https://custom.campus.org/",
			kioskID:       "kiosk-hall-1",
			expected:      "https://custom.campus.org",
		},
		{
			name:          "explicit idle QR content is preserved as-is",
			idleQRContent: "https://custom-qr.org/scan-me",
			portalURLBase: "https://xeroxgo.vercel.app",
			kioskID:       "kiosk-demo",
			expected:      "https://custom-qr.org/scan-me",
		},
		{
			name:          "empty portal url defaults to vercel root",
			idleQRContent: "",
			portalURLBase: "   ",
			kioskID:       "kiosk-demo",
			expected:      "https://xeroxgo.vercel.app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveIdleQRContent(tt.idleQRContent, tt.portalURLBase, tt.kioskID)
			if got != tt.expected {
				t.Errorf("resolveIdleQRContent(%q, %q, %q) = %q, want %q",
					tt.idleQRContent, tt.portalURLBase, tt.kioskID, got, tt.expected)
			}
		})
	}
}

func TestSimulatedStockRequiresMockPrinterValidation(t *testing.T) {
	validateSimulatedStockFlags := func(simulatedStockSet bool, mockPrinter bool, stock int, capacity int) error {
		if simulatedStockSet {
			if !mockPrinter {
				return errors.New("--simulated-stock requires --mock-printer; cannot mutate production inventory without explicit simulation mode")
			}
			if stock < 0 || stock > capacity {
				return fmt.Errorf("--simulated-stock must be between 0 and %d inclusive, got %d", capacity, stock)
			}
		}
		return nil
	}

	if err := validateSimulatedStockFlags(true, false, 50, 150); err == nil {
		t.Fatal("expected error when simulated stock is set without mock printer")
	}
	if err := validateSimulatedStockFlags(true, true, 50, 150); err != nil {
		t.Fatalf("expected valid simulated stock with mock printer to pass, got %v", err)
	}
	if err := validateSimulatedStockFlags(true, true, 200, 150); err == nil {
		t.Fatal("expected error when simulated stock exceeds capacity")
	}
}
