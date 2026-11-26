package spooler

import (
	"context"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

func TestPackage5C_DisconnectWhileHealthy_AndRecoveryTransitions(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := state.OpenWithCapacity(filepath.Join(tmpDir, "p5c_spooler.db"), 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity failed: %v", err)
	}
	defer st.Close()

	var wsMu sync.Mutex
	allowWS := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			wsMu.Lock()
			ok := allowWS
			wsMu.Unlock()
			if !ok {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if c, err := websocket.Accept(w, r, nil); err == nil {
				defer c.CloseNow()
				for {
					if _, _, rErr := c.Read(r.Context()); rErr != nil {
						return
					}
				}
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:           "kiosk-5c-pi-rec",
		StateStore:        st,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       tmpDir,
		IdleQRContent:     "https://kiosk.test",
		ScreenSink:        func(*image.RGBA) error { return nil },
		InputSource:       simInput,
		MockHealth:        testHealthyMockHealth,
		HeartbeatInterval: 50 * time.Millisecond,
		MinBackoff:        20 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// 1. Starts blocked while uncalibrated -> healthy-to-healthy calibration recovers customer UI!
	time.Sleep(40 * time.Millisecond)
	if !client.PINController().IsBlocked() {
		t.Fatal("expected pinController blocked while inventory is uncalibrated")
	}

	// Calibrate while hardware is already healthy -> must automatically recover without fault cycle
	sheets := 100
	client.handleRefillMessage(ctx, nil, wireMessage{Type: "refill", RequestID: "rec-calib", Sheets: &sheets})
	if !waitCondition(3*time.Second, func() bool { return !client.PINController().IsBlocked() }) {
		t.Fatal("expected automatic recovery after calibration while hardware already healthy")
	}

	// 2. Disconnect while hardware remains healthy -> must block PIN controller and render ModeError
	wsMu.Lock()
	allowWS = false
	wsMu.Unlock()
	// Force reconnect drop
	client.connMu.Lock()
	if client.conn != nil {
		_ = client.conn.Close(websocket.StatusNormalClosure, "drop")
	}
	client.connMu.Unlock()

	if !waitCondition(3*time.Second, func() bool { return client.PINController().IsBlocked() }) {
		t.Fatal("expected pinController blocked on cloud disconnect even while hardware is healthy")
	}
	client.currentModeMu.Lock()
	modeOnDrop := client.currentMode
	client.currentModeMu.Unlock()
	if modeOnDrop != ui_ipc.ModeError {
		t.Fatalf("expected ModeError on cloud disconnect, got %s", modeOnDrop)
	}

	// 3. Reconnect while hardware is already healthy -> automatically recovers to ModeIdle
	wsMu.Lock()
	allowWS = true
	wsMu.Unlock()
	if !waitCondition(3*time.Second, func() bool {
		client.currentModeMu.Lock()
		m := client.currentMode
		client.currentModeMu.Unlock()
		return !client.PINController().IsBlocked() && m == ui_ipc.ModeIdle
	}) {
		t.Fatal("expected automatic recovery to ModeIdle upon cloud reconnection")
	}

	// 4. Occupied kiosk (activeJobs non-empty) rejects new PIN interaction and restores on completion
	client.activeMu.Lock()
	client.activeJobs["job-occupied"] = true
	client.activeMu.Unlock()
	client.syncCustomerServiceState(ctx)
	if !client.PINController().IsBlocked() {
		t.Fatal("expected pinController blocked while kiosk is occupied by active paid job")
	}

	client.activeMu.Lock()
	delete(client.activeJobs, "job-occupied")
	client.activeMu.Unlock()
	client.syncCustomerServiceState(ctx)
	if client.PINController().IsBlocked() {
		t.Fatal("expected pinController unblocked after active paid job completes")
	}

	// 5. Execution-safety recovery while hardware already healthy:
	// Insert PAUSED_ERROR job -> blocks; mark COMPLETED -> CheckPrinterHealth recovers automatically
	cmd := contracts.PrintJobCommand{
		JobID: "job-exec-rec", OrderID: "ord-exec-rec", KioskID: "kiosk-5c-pi-rec",
		TotalPages: 1, TotalSheets: 1, Copies: 1, StartFromPage: 1, Duplex: contracts.DuplexSimplex,
	}
	_ = st.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	_ = client.CheckPrinterHealth(ctx)
	if !client.PINController().IsBlocked() {
		t.Fatal("expected pinController blocked when execution-safety barrier is active")
	}
	_ = st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-exec-rec", KioskID: "kiosk-5c-pi-rec", Status: contracts.JobStatusCompleted,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: time.Now().UTC(),
	})
	_ = client.CheckPrinterHealth(ctx)
	if client.PINController().IsBlocked() {
		t.Fatal("expected automatic recovery after execution-safety resolution")
	}
}
