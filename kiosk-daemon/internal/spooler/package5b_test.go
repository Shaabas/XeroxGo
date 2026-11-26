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
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func TestPackage5B_RefillValidationAndConfirmationOverWS(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "refill_5b.db")
	st, err := state.OpenWithCapacity(dbPath, 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.ResetPaperTray(ctx, 40); err != nil {
		t.Fatalf("ResetPaperTray(40) failed: %v", err)
	}

	var mu sync.Mutex
	acks := make(map[string]wireMessage)
	doneCh := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		neg := -1
		over := 151
		zero := 0
		exact := 150
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", RequestID: "req-missing"})
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", RequestID: "req-neg", Sheets: &neg})
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", RequestID: "req-over", Sheets: &over})
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", RequestID: "req-zero", Sheets: &zero})
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", RequestID: "req-exact", Sheets: &exact})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				return
			}
			if msg.Type == "refill_ack" {
				mu.Lock()
				acks[msg.RequestID] = msg
				count := len(acks)
				mu.Unlock()
				if count == 5 {
					close(doneCh)
					return
				}
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, err := NewClient(ClientConfig{
		CloudURL:               wsURL,
		KioskID:                "kiosk-5b-refill",
		StateStore:             st,
		ConfiguredTrayCapacity: 150,
		PrinterAddr:            "127.0.0.1:9100",
		DownloadDir:            tmpDir,
		IdleQRContent:          "https://kiosk.test",
		ScreenSink:             func(*image.RGBA) error { return nil },
		MockHealth:             testHealthyMockHealth,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = client.Run(runCtx) }()

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for 5 refill_ack messages")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, bad := range []string{"req-missing", "req-neg", "req-over"} {
		if acks[bad].OK {
			t.Errorf("expected %s to be rejected, got ok=true", bad)
		}
	}
	for _, good := range []string{"req-zero", "req-exact"} {
		if !acks[good].OK || !acks[good].Calibrated {
			t.Errorf("expected %s to succeed, got %+v", good, acks[good])
		}
	}

	sheets, calib, err := st.GetPaperSupply(ctx)
	if err != nil || !calib || sheets != 150 {
		t.Fatalf("expected persisted calibrated inventory=150, got sheets=%d calib=%v err=%v", sheets, calib, err)
	}
}

func TestPackage5B_PiEnforcesCapabilitiesBeforeSubmission(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := openCalibratedTestStore(t, filepath.Join(tmpDir, "cap_5b.db"))
	if err != nil {
		t.Fatalf("openCalibratedTestStore failed: %v", err)
	}
	defer st.Close()

	pdfBytes := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()

	submitted := 0
	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-5b-cap",
		StateStore:     st,
		CUPSQueue:      "HPTank1020w",
		DownloadDir:    tmpDir,
		IdleQRContent:  "https://kiosk.test",
		ScreenSink:     func(*image.RGBA) error { return nil },
		MockCUPSHealth: testHealthyCUPSHealth,
		MockIPPState:   testHealthyIPPState,
		MockSNMPHealth: testHealthySNMPHealth,
		MockCapabilities: &printer.PrinterCapabilities{
			Source:            printer.CapabilitySourceCUPSQueue,
			A4Support:         printer.CapabilitySupported,
			SimplexSupport:    printer.CapabilitySupported,
			MonochromeSupport: printer.CapabilitySupported,
			PDFSupport:        printer.CapabilityUnsupported,
		},
		MockCUPSPrint: func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(int)) error {
			submitted++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx := context.Background()
	colorCmd := contracts.PrintJobCommand{
		JobID:          "job-5b-color",
		KioskID:        "kiosk-5b-cap",
		DisplayCode:    "1111",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		ColorMode:      "COLOR",
		StartFromPage:  1,
	}
	client.processJob(ctx, colorCmd)

	duplexCmd := colorCmd
	duplexCmd.JobID = "job-5b-duplex"
	duplexCmd.ColorMode = "BW"
	duplexCmd.Duplex = contracts.DuplexLongEdge
	client.processJob(ctx, duplexCmd)

	if submitted != 0 {
		t.Fatalf("expected 0 physical submissions for unsupported COLOR/DUPLEX jobs, got %d", submitted)
	}
}

func TestPackage5B_UncalibratedInventoryPreventsPhysicalSubmission(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "uncalibrated.db")
	st, err := state.OpenWithCapacity(dbPath, 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity failed: %v", err)
	}
	defer st.Close()

	// Initial store starts with estimated_sheets=0, is_calibrated=0
	sheets, calib, err := st.GetPaperSupply(context.Background())
	if err != nil || calib || sheets != 0 {
		t.Fatalf("expected uncalibrated 0 sheets, got sheets=%d calib=%v err=%v", sheets, calib, err)
	}

	pdfBytes := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(sum[:])

	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfSrv.Close()

	submitted := 0
	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-5b-uncalib",
		StateStore:     st,
		CUPSQueue:      "HPTank1020w",
		DownloadDir:    tmpDir,
		IdleQRContent:  "https://kiosk.test",
		ScreenSink:     func(*image.RGBA) error { return nil },
		MockCUPSHealth: testHealthyCUPSHealth,
		MockIPPState:   testHealthyIPPState,
		MockSNMPHealth: testHealthySNMPHealth,
		MockCapabilities: &printer.PrinterCapabilities{
			Source:            printer.CapabilitySourceCUPSQueue,
			A4Support:         printer.CapabilitySupported,
			SimplexSupport:    printer.CapabilitySupported,
			MonochromeSupport: printer.CapabilitySupported,
			PDFSupport:        printer.CapabilityUnsupported,
		},
		MockCUPSPrint: func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(int)) error {
			submitted++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx := context.Background()
	cmd := contracts.PrintJobCommand{
		JobID:          "job-uncalibrated",
		KioskID:        "kiosk-5b-uncalib",
		DisplayCode:    "2222",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		ColorMode:      "BW",
		StartFromPage:  1,
	}
	client.processJob(ctx, cmd)

	if submitted != 0 {
		t.Fatalf("expected 0 physical submissions when inventory is uncalibrated, got %d", submitted)
	}
}

func TestPackage5B_SimulationCannotBypassProductionCalibration(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sim_bypass.db")
	st, err := state.OpenWithCapacity(dbPath, 150)
	if err != nil {
		t.Fatalf("OpenWithCapacity failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Initial calibration succeeds
	if err := st.CalibratePaperTray(ctx, 100); err != nil {
		t.Fatalf("CalibratePaperTray(100) failed: %v", err)
	}

	// Insert an active PRINTING job
	cmd := contracts.PrintJobCommand{
		JobID:         "job-active-print",
		OrderID:       "ord-active",
		KioskID:       "kiosk-sim-bypass",
		TotalPages:    1,
		TotalSheets:   1,
		Copies:        1,
		StartFromPage: 1,
		Duplex:        contracts.DuplexSimplex,
		ColorMode:     "BW",
	}
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	// Both CalibratePaperTray and ResetPaperTray MUST reject calibration while active job exists
	if err := st.CalibratePaperTray(ctx, 120); err == nil {
		t.Fatal("expected CalibratePaperTray to reject while PRINTING job is active")
	}
	if err := st.ResetPaperTray(ctx, 120); err == nil {
		t.Fatal("expected ResetPaperTray to reject while PRINTING job is active (cannot bypass durable calibration safety)")
	}

	// Move job to PAUSED_ERROR (physically unresolved uncertainty)
	if err := st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:  "job-active-print",
		Status: contracts.JobStatusPausedError,
	}); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	// Both CalibratePaperTray and ResetPaperTray MUST reject calibration while unresolved job exists
	if err := st.CalibratePaperTray(ctx, 130); err == nil {
		t.Fatal("expected CalibratePaperTray to reject while unresolved physical job exists")
	}
	if err := st.ResetPaperTray(ctx, 130); err == nil {
		t.Fatal("expected ResetPaperTray to reject while unresolved physical job exists")
	}
}
