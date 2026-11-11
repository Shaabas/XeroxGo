package spooler

import (
	"context"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

type mockSoundTracker struct {
	playCount atomic.Int32
}

func (m *mockSoundTracker) PlayCompletion() {
	m.playCount.Add(1)
}

func (m *mockSoundTracker) WAVBytes() []byte {
	return []byte("RIFF-MOCK")
}

func TestSpooler_CompletionSoundTriggeredExactlyOnce(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/state.db"
	jobsDir := tmpDir + "/jobs"
	_ = os.MkdirAll(jobsDir, 0700)
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open state db: %v", err)
	}
	defer st.Close()

	soundTracker := &mockSoundTracker{}

	// Setup mock PDF download server
	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 mock content"))
	}))
	defer pdfServer.Close()

	cfg := ClientConfig{
		CloudURL:                 "ws://127.0.0.1:9999/ws", // Won't connect, testing processJob directly
		KioskID:                  "kiosk-sound-test",
		KioskSecret:              "secret",
		StateStore:               st,
		PrinterAddr:              "127.0.0.1:9100",
		DownloadDir:              tmpDir + "/jobs",
		IdleQRContent:            "https://xeroxgo.test/idle",
		ScreenSink:               func(img *image.RGBA) error { return nil },
		SoundPlayer:              soundTracker,
		CompletedDisplayDuration: 100 * time.Millisecond,
		MockCUPSPrint: func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
			if onPage != nil {
				onPage(cmd.TotalSheets)
			}
			return nil
		},
		CUPSQueue: "MockQueue",
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Reset paper tray
	ctx := context.Background()
	_ = st.ResetPaperTray(ctx, 100)

	// Compute SHA256 of mock PDF
	const expectedSHA = "3f9666887369214a92e99be147261d97ef1c579093143e502e6ee62229b35482"

	cmd := contracts.PrintJobCommand{
		JobID:          "job_sound_001",
		KioskID:        "kiosk-sound-test",
		DownloadURL:    pdfServer.URL + "/mock.pdf",
		SHA256Checksum: expectedSHA,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		TotalPages:     2,
		TotalSheets:    2,
		StartFromPage:  1,
	}

	// Run processJob
	client.processJob(ctx, cmd)

	// Verify PlayCompletion was called exactly once
	count := soundTracker.playCount.Load()
	if count != 1 {
		t.Fatalf("expected PlayCompletion called exactly 1 time, got %d", count)
	}

	// 2. Run a failed job (wrong sha256) and verify PlayCompletion is NOT called again
	failedCmd := contracts.PrintJobCommand{
		JobID:          "job_sound_fail",
		KioskID:        "kiosk-sound-test",
		DownloadURL:    pdfServer.URL + "/mock.pdf",
		SHA256Checksum: "0000000000000000000000000000000000000000000000000000000000000000",
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		TotalPages:     1,
		TotalSheets:    1,
		StartFromPage:  1,
	}

	client.processJob(ctx, failedCmd)

	countAfterFail := soundTracker.playCount.Load()
	if countAfterFail != 1 {
		t.Fatalf("expected PlayCompletion count to remain 1 after failed job, got %d", countAfterFail)
	}
}
