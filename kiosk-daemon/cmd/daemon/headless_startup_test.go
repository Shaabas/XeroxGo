package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHeadlessKioskDaemon_StartupAndViewer(t *testing.T) {
	// 1. Build binary for testing
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "xeroxgo-kiosk")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build kiosk daemon for test: %v\nOutput: %s", err, string(out))
	}

	// 2. Pick a free port for the viewer
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on free port: %v", err)
	}
	viewerAddr := ln.Addr().String()
	_ = ln.Close()

	previewPNG := filepath.Join(tmpDir, "screen.png")
	stateDB := filepath.Join(tmpDir, "state.db")
	downloadDir := filepath.Join(tmpDir, "jobs")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath,
		"--mock-printer",
		"--enable-keypad",
		"--preview-png="+previewPNG,
		"--viewer-addr="+viewerAddr,
		"--cloud-url=ws://127.0.0.1:49999/ws", // dummy offline cloud
		"--kiosk-id=kiosk-test-headless",
		"--kiosk-secret=test_secret",
		"--state-db="+stateDB,
		"--download-dir="+downloadDir,
		"--simulated-stock=75",
		"--enable-sound=false",
	)

	// Capture output for diagnosis if needed
	var outputBuf strings.Builder
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start headless kiosk process: %v", err)
	}

	daemonPID := cmd.Process.Pid
	t.Logf("Started headless kiosk daemon (PID %d) on viewer %s", daemonPID, viewerAddr)

	// Ensure process cleanup on test exit
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// 3. Wait for viewer server to respond
	viewerURL := "http://" + viewerAddr
	var viewerReady bool
	deadline := time.Now().Add(4 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		resp, err := client.Get(viewerURL + "/status")
		if err == nil && resp.StatusCode == http.StatusOK {
			var st map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&st)
			_ = resp.Body.Close()
			if st["kiosk_id"] == "kiosk-test-headless" && st["preview_ready"] == true {
				viewerReady = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !viewerReady {
		t.Fatalf("viewer server failed to become ready within deadline.\nDaemon logs:\n%s", outputBuf.String())
	}

	// 4. Verify preview PNG was rendered to file
	if _, err := os.Stat(previewPNG); err != nil {
		t.Fatalf("expected preview PNG to exist at %s: %v", previewPNG, err)
	}

	// 5. Verify GET / returns monitor HTML
	resp, err := client.Get(viewerURL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	htmlBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(htmlBody), "XeroxGo Kiosk Live Monitor") {
		t.Errorf("GET / missing monitor title")
	}

	// 6. Verify GET /screen.png returns the rendered frame
	respPNG, err := client.Get(viewerURL + "/screen.png")
	if err != nil {
		t.Fatalf("GET /screen.png failed: %v", err)
	}
	defer respPNG.Body.Close()
	if respPNG.StatusCode != http.StatusOK {
		t.Errorf("GET /screen.png status = %d, want 200", respPNG.StatusCode)
	}
	if ct := respPNG.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("GET /screen.png content-type = %s, want image/png", ct)
	}

	// 7. Verify clean graceful shutdown via SIGTERM
	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case waitErr := <-waitDone:
		if waitErr != nil && !strings.Contains(waitErr.Error(), "signal: terminated") {
			t.Logf("Process exited with: %v (expected on SIGTERM)", waitErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("kiosk daemon did not shut down cleanly within 3s after SIGTERM")
	}

	logs := outputBuf.String()
	if !strings.Contains(logs, "XeroxGo kiosk daemon shut down cleanly") {
		t.Errorf("expected clean shutdown log message, got:\n%s", logs)
	}
}
