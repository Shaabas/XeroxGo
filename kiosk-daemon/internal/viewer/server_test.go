package viewer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/kiosk-daemon/internal/audio"
)

func TestViewerServer_LifecycleAndEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	pngPath := filepath.Join(tmpDir, "screen.png")

	// Write mock PNG
	testPNGBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}
	if err := os.WriteFile(pngPath, testPNGBytes, 0644); err != nil {
		t.Fatalf("failed to write test png: %v", err)
	}

	cfg := Config{
		Addr:        "127.0.0.1:0",
		PreviewPNG:  pngPath,
		SoundPlayer: audio.NewPlayer(true),
		KioskID:     "kiosk-test",
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)

	baseURL := "http://" + server.Addr()

	// 1. Test GET / (HTML index)
	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "XeroxGo Kiosk Live Monitor") {
		t.Errorf("GET / missing expected title in HTML")
	}

	// 2. Test GET /screen.png
	respPNG, err := http.Get(baseURL + "/screen.png")
	if err != nil {
		t.Fatalf("GET /screen.png failed: %v", err)
	}
	defer respPNG.Body.Close()

	if respPNG.StatusCode != http.StatusOK {
		t.Errorf("GET /screen.png returned %d, want %d", respPNG.StatusCode, http.StatusOK)
	}
	if ct := respPNG.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %s, want image/png", ct)
	}
	if cc := respPNG.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %s, want no-store", cc)
	}
	pngData, _ := io.ReadAll(respPNG.Body)
	if len(pngData) != len(testPNGBytes) {
		t.Errorf("pngData len = %d, want %d", len(pngData), len(testPNGBytes))
	}

	// 3. Test GET /chime.wav
	respWAV, err := http.Get(baseURL + "/chime.wav")
	if err != nil {
		t.Fatalf("GET /chime.wav failed: %v", err)
	}
	defer respWAV.Body.Close()

	if respWAV.StatusCode != http.StatusOK {
		t.Errorf("GET /chime.wav returned %d, want %d", respWAV.StatusCode, http.StatusOK)
	}
	if ct := respWAV.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %s, want audio/wav", ct)
	}

	// 4. Test GET /status
	respStatus, err := http.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	defer respStatus.Body.Close()

	var st map[string]interface{}
	if err := json.NewDecoder(respStatus.Body).Decode(&st); err != nil {
		t.Fatalf("failed to decode /status JSON: %v", err)
	}
	if st["kiosk_id"] != "kiosk-test" {
		t.Errorf("kiosk_id = %v, want kiosk-test", st["kiosk_id"])
	}
	if st["preview_ready"] != true {
		t.Errorf("preview_ready = %v, want true", st["preview_ready"])
	}

	// 4b. Test NotifyJobCompleted via /status
	server.NotifyJobCompleted("job-chime-999")
	respStatus2, err := http.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("GET /status after NotifyJobCompleted failed: %v", err)
	}
	defer respStatus2.Body.Close()
	var st2 map[string]interface{}
	if err := json.NewDecoder(respStatus2.Body).Decode(&st2); err != nil {
		t.Fatalf("failed to decode /status JSON: %v", err)
	}
	if st2["last_completed_job_id"] != "job-chime-999" {
		t.Errorf("last_completed_job_id = %v, want job-chime-999", st2["last_completed_job_id"])
	}
	if completedAt, ok := st2["last_completed_at"].(float64); !ok || completedAt <= 0 {
		t.Errorf("last_completed_at = %v, want positive timestamp", st2["last_completed_at"])
	}

	// 5. Test 404 for arbitrary path
	resp404, err := http.Get(baseURL + "/other_file.txt")
	if err != nil {
		t.Fatalf("GET /other_file.txt failed: %v", err)
	}
	defer resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for arbitrary path, got %d", resp404.StatusCode)
	}
}

func TestViewerServer_MissingPreviewFileFallback(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistentPath := filepath.Join(tmpDir, "missing.png")

	server, err := NewServer(Config{
		Addr:       "127.0.0.1:0",
		PreviewPNG: nonExistentPath,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Start(ctx)

	baseURL := "http://" + server.Addr()

	resp, err := http.Get(baseURL + "/screen.png")
	if err != nil {
		t.Fatalf("GET /screen.png failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /screen.png with missing file returned %d, want 200 transparent fallback", resp.StatusCode)
	}
	if statusHeader := resp.Header.Get("X-XeroxGo-Status"); statusHeader != "waiting_for_frame" {
		t.Errorf("X-XeroxGo-Status = %s, want waiting_for_frame", statusHeader)
	}
}
