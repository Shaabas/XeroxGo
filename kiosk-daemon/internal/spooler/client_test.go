package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

// createThreePageFixture writes a valid PostScript file with three distinct pages
// and generates a PDF using Ghostscript under dir.
func createThreePageFixture(t *testing.T, dir string) (string, []byte, string) {
	t.Helper()

	psPath := filepath.Join(dir, "fixture_3pages.ps")
	pdfPath := filepath.Join(dir, "fixture_3pages.pdf")

	psContent := `%!PS-Adobe-3.0
%%Pages: 3
%%Page: 1 1
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 1) show
showpage

%%Page: 2 2
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 2) show
showpage

%%Page: 3 3
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 3) show
showpage
`
	if err := os.WriteFile(psPath, []byte(psContent), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Ghostscript failed to generate PDF fixture: %s: %v", strings.TrimSpace(string(out)), err)
	}

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatalf("failed to read generated PDF: %v", err)
	}
	hash := sha256.Sum256(data)
	return pdfPath, data, hex.EncodeToString(hash[:])
}

func noopSink(*image.RGBA) error {
	return nil
}

// Test A — TestNewClientRejectsInvalidConfig
func TestNewClientRejectsInvalidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_config.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open state store: %v", err)
	}
	defer store.Close()

	validCfg := ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk/001",
		ScreenSink:    noopSink,
	}

	testCases := []struct {
		name      string
		modifyCfg func(c *ClientConfig)
	}{
		{"empty cloud url", func(c *ClientConfig) { c.CloudURL = "" }},
		{"invalid cloud url", func(c *ClientConfig) { c.CloudURL = "http://[::1]:namedport" }},
		{"invalid scheme", func(c *ClientConfig) { c.CloudURL = "http://127.0.0.1:8080/ws" }},
		{"missing host", func(c *ClientConfig) { c.CloudURL = "ws:///ws" }},
		{"empty kiosk id", func(c *ClientConfig) { c.KioskID = "" }},
		{"nil state store", func(c *ClientConfig) { c.StateStore = nil }},
		{"empty printer address", func(c *ClientConfig) { c.PrinterAddr = "" }},
		{"empty download dir", func(c *ClientConfig) { c.DownloadDir = "" }},
		{"empty idle qr", func(c *ClientConfig) { c.IdleQRContent = "" }},
		{"nil screen sink", func(c *ClientConfig) { c.ScreenSink = nil }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg
			tc.modifyCfg(&cfg)
			client, err := NewClient(cfg)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tc.name)
			}
			if client != nil {
				t.Fatalf("expected nil client on error for case %q", tc.name)
			}
		})
	}

	// Valid config succeeds
	c, err := NewClient(validCfg)
	if err != nil {
		t.Fatalf("expected success for valid config, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if !strings.Contains(c.normalizedURL, "kiosk_id=kiosk-001") {
		t.Fatalf("expected normalized URL to contain kiosk_id, got %s", c.normalizedURL)
	}
	if c.cfg.HTTPClient == nil {
		t.Fatal("expected default HTTP client to be initialized")
	}
}

// Test B — TestDownloadPDFVerifiesSHA256
func TestDownloadPDFVerifiesSHA256(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_b.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	goodSHA := hex.EncodeToString(hasher[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	jobID := "job-sha256-test"
	h := sha256.Sum256([]byte(jobID))
	finalName := hex.EncodeToString(h[:]) + ".pdf"
	finalPath := filepath.Join(tmpDir, finalName)

	cmdGood := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-001",
		DownloadURL:    server.URL + "/doc.pdf",
		SHA256Checksum: goodSHA,
	}

	// 1. Download with correct checksum
	if err := client.downloadAndVerify(context.Background(), cmdGood, finalPath); err != nil {
		t.Fatalf("downloadAndVerify failed with correct checksum: %v", err)
	}

	storedBytes, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("failed to read stored PDF: %v", err)
	}
	if !bytes.Equal(storedBytes, pdfContent) {
		t.Fatal("stored PDF content does not match served payload")
	}

	_ = os.Remove(finalPath)

	// 2. Download with incorrect checksum
	cmdBad := cmdGood
	cmdBad.SHA256Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	err = client.downloadAndVerify(context.Background(), cmdBad, finalPath)
	if err == nil {
		t.Fatal("expected error with bad checksum, got nil")
	}

	if _, err := os.Stat(finalPath); err == nil {
		t.Fatal("final PDF should not exist after checksum mismatch")
	}

	// Verify no temporary files remain
	files, _ := os.ReadDir(tmpDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "job_download_") {
			t.Fatalf("found leftover temporary file: %s", f.Name())
		}
	}
}

// Test C — TestDownloadPDFRejectsNon200AndOversizedResponses
func TestDownloadPDFRejectsNon200AndOversizedResponses(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_c.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Non-200 responses
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/404" {
			http.NotFound(w, r)
		} else {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}))
	defer statusServer.Close()

	targetPath := filepath.Join(tmpDir, "should_not_exist.pdf")
	cmd404 := contracts.PrintJobCommand{
		JobID:          "j-404",
		DownloadURL:    statusServer.URL + "/404",
		SHA256Checksum: strings.Repeat("a", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmd404, targetPath); err == nil {
		t.Fatal("expected error on 404, got nil")
	}

	cmd500 := contracts.PrintJobCommand{
		JobID:          "j-500",
		DownloadURL:    statusServer.URL + "/500",
		SHA256Checksum: strings.Repeat("a", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmd500, targetPath); err == nil {
		t.Fatal("expected error on 500, got nil")
	}

	// 2. Declared Content-Length exceeding 250 MiB limit
	declaredOversizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "300000000") // 300 MB > 250 MiB
		w.WriteHeader(http.StatusOK)
	}))
	defer declaredOversizedServer.Close()

	cmdOversized := contracts.PrintJobCommand{
		JobID:          "j-oversized",
		DownloadURL:    declaredOversizedServer.URL,
		SHA256Checksum: strings.Repeat("b", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmdOversized, targetPath); err == nil {
		t.Fatal("expected error on declared Content-Length > 250 MiB, got nil")
	}

	// 3. Streamed payload exceeding limit without Content-Length
	streamOversizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 128)))
	}))
	defer streamOversizedServer.Close()

	client.maxDownloadBytes = 50 // small test limit
	cmdStreamOversized := contracts.PrintJobCommand{
		JobID:          "j-stream-oversized",
		DownloadURL:    streamOversizedServer.URL,
		SHA256Checksum: strings.Repeat("c", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmdStreamOversized, targetPath); err == nil {
		t.Fatal("expected error when streaming exceeds limit, got nil")
	}

	// Verify no temporary files remain
	files, _ := os.ReadDir(tmpDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "job_download_") {
			t.Fatalf("leftover temporary file after error: %s", f.Name())
		}
	}
}

// Test D — TestClientProcessesJobEndToEnd
func TestClientProcessesJobEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	// 1. PDF Server
	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	// 2. Mock TCP Printer
	var printerReceived bytes.Buffer
	printerDone := make(chan struct{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for mock printer: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read incoming stream until client finishes CloseWrite()
		_, _ = io.Copy(&printerReceived, conn)

		// Respond with PJL status events: 1 page and job end
		_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		close(printerDone)
	}()

	// 3. Local WebSocket Hub Server
	type receivedProgressMsg struct {
		evt contracts.JobProgressEvent
	}
	var progressMu sync.Mutex
	var receivedProgress []contracts.JobProgressEvent
	jobDispatched := make(chan struct{})

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-e2e-001",
		KioskID:        "kiosk-e2e",
		DisplayCode:    "8844",
		DownloadURL:    pdfServer.URL + "/job.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Send the job command
		_ = wsjson.Write(r.Context(), c, wireMessage{
			Type: "job",
			Job:  &jobCmd,
		})
		close(jobDispatched)

		// Read progress events
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
			if msg.Type == "progress" && msg.Progress != nil {
				progressMu.Lock()
				receivedProgress = append(receivedProgress, *msg.Progress)
				progressMu.Unlock()
			}
		}
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	// 4. Recording Screen Sink
	var screenMu sync.Mutex
	var renderedScreens []*image.RGBA
	sink := func(img *image.RGBA) error {
		screenMu.Lock()
		defer screenMu.Unlock()
		renderedScreens = append(renderedScreens, img)
		return nil
	}

	// 5. Build Client
	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-e2e",
		StateStore:    store,
		PrinterAddr:   ln.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-e2e",
		ScreenSink:    sink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond
	client.heartbeatInterval = 50 * time.Millisecond
	client.healthPoller = func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			TonerLevelPercent: 90,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- client.Run(ctx)
	}()

	// Wait for printer to complete
	select {
	case <-printerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for mock printer to complete")
	}

	// Wait a moment for completion to settle in store and WS
	deadline := time.Now().Add(3 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil {
			// If incomplete job is nil, verify local DB is readable
			if _, sErr := store.GetEstimatedSheets(ctx); sErr == nil {
				completed = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client shutdown")
	}

	// Assertions
	if !completed {
		t.Fatal("expected job to reach COMPLETED status")
	}

	// 1. PDF payload transmitted unchanged
	if !bytes.Contains(printerReceived.Bytes(), pdfContent) {
		t.Fatal("printer stream did not contain the original PDF payload")
	}

	// 2. UI Transitions Produced
	client.renderHistoryMu.Lock()
	history := client.renderHistory
	client.renderHistoryMu.Unlock()

	var hasIdle, hasDownload, hasPrint, hasCompleted bool
	for _, s := range history {
		switch s.Mode {
		case ui_ipc.ModeIdle:
			hasIdle = true
		case ui_ipc.ModeDownloading:
			hasDownload = true
		case ui_ipc.ModePrinting:
			hasPrint = true
		case ui_ipc.ModeCompleted:
			hasCompleted = true
		}
	}
	if !hasIdle || !hasDownload || !hasPrint || !hasCompleted {
		t.Fatalf("missing required UI transitions: idle=%v, download=%v, print=%v, completed=%v",
			hasIdle, hasDownload, hasPrint, hasCompleted)
	}

	// 3. WS Hub received progress events including COMPLETED
	progressMu.Lock()
	defer progressMu.Unlock()
	var hubSawCompleted bool
	for _, p := range receivedProgress {
		if p.Status == contracts.JobStatusCompleted && p.PagesPrinted == 1 && p.SheetsPrinted == 1 {
			hubSawCompleted = true
		}
	}
	if !hubSawCompleted {
		t.Fatalf("cloud WS hub did not receive final COMPLETED progress event: %+v", receivedProgress)
	}

	// 5. Verify local PDF payload was deleted immediately after printing (ETSI EN 303 645 / GDPR compliance)
	deletedPath := jobPDFPath(tmpDir, jobCmd.JobID)
	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Fatalf("expected printed document payload to be deleted from disk, but file still exists at %s", deletedPath)
	}
}

// Test E — TestClientPersistsStateWhenCloudDisconnects
func TestClientPersistsStateWhenCloudDisconnects(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_e.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	printerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("printer listener failed: %v", err)
	}
	defer printerLn.Close()

	printerDone := make(chan struct{})
	go func() {
		conn, err := printerLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		close(printerDone)
	}()

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-disconnect-001",
		KioskID:        "kiosk-e",
		DisplayCode:    "1122",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Dispatch job and immediately close WebSocket!
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})
		_ = c.Close(websocket.StatusGoingAway, "server shutting down")
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-e",
		StateStore:    store,
		PrinterAddr:   printerLn.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-e",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 50 * time.Millisecond
	client.maxBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	select {
	case <-printerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for physical print to finish")
	}

	// Verify local state store reached COMPLETED
	deadline := time.Now().Add(2 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil {
			completed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !completed {
		t.Fatal("job should have completed locally despite cloud disconnection")
	}
}

// Test F — TestClientReconnectsWithBackoff
func TestClientReconnectsWithBackoff(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_f.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var allowConnect atomic.Bool
	var connected atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowConnect.Load() {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connected.Store(true)

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-f",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-f",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 10 * time.Millisecond
	client.maxBackoff = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(ctx)
	}()

	// Wait 50ms while server is unavailable
	time.Sleep(50 * time.Millisecond)
	if connected.Load() {
		t.Fatal("client should not have connected while server is unavailable")
	}

	// Now make server available
	allowConnect.Store(true)

	// Wait for successful reconnection
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if connected.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !connected.Load() {
		t.Fatal("client failed to reconnect after server became available")
	}

	// Prompt shutdown
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for client shutdown after cancel")
	}
}

// Test G — TestClientHeartbeat
func TestClientHeartbeat(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_g.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var gotHeartbeat, gotHealth atomic.Bool
	healthVal := atomic.Int32{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
			if msg.Type == "heartbeat" {
				gotHeartbeat.Store(true)
			}
			if msg.Type == "health" && msg.Health != nil {
				gotHealth.Store(true)
				healthVal.Store(int32(msg.Health.TonerLevelPercent))
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-g",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-g",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.heartbeatInterval = 20 * time.Millisecond
	client.healthPoller = func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			TonerLevelPercent: 77,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotHeartbeat.Load() && gotHealth.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !gotHeartbeat.Load() {
		t.Fatal("expected client to send heartbeat message")
	}
	if !gotHealth.Load() {
		t.Fatal("expected client to send health message")
	}
	if healthVal.Load() != 77 {
		t.Fatalf("expected toner level 77, got %d", healthVal.Load())
	}
}

func TestClientRefill_OverWebSocket(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_refill.db")
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	var gotInitialUnknown atomic.Bool
	var gotRefilledHealth atomic.Bool
	var refilledSheets atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
			if msg.Type == "health" && msg.Health != nil {
				if msg.Health.PaperTrayStatus == "UNKNOWN" {
					gotInitialUnknown.Store(true)
					// Trigger operator refill of 215 sheets
					_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 215})
				} else if msg.Health.PaperTrayStatus == "OK" && msg.Health.EstimatedSheets == 215 {
					gotRefilledHealth.Store(true)
					refilledSheets.Store(int32(msg.Health.EstimatedSheets))
				}
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-refill-ws",
		StateStore:    st,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-refill",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.heartbeatInterval = 20 * time.Millisecond
	client.healthPoller = func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			State:           "IDLE",
			PaperTrayStatus: "OK",
			EstimatedSheets: sheets,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotInitialUnknown.Load() && gotRefilledHealth.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !gotInitialUnknown.Load() {
		t.Fatal("expected uncalibrated client to report PaperTrayStatus UNKNOWN initially")
	}
	if !gotRefilledHealth.Load() {
		t.Fatal("expected client to report PaperTrayStatus OK with 215 sheets after refill")
	}
	if refilledSheets.Load() != 215 {
		t.Fatalf("expected refilled sheets 215, got %d", refilledSheets.Load())
	}

	// Verify stateStore persisted the calibrated state
	storedSheets, calib, err := st.GetPaperSupply(ctx)
	if err != nil {
		t.Fatalf("failed to query paper supply: %v", err)
	}
	if !calib {
		t.Errorf("expected stateStore calibrated=true")
	}
	if storedSheets != 215 {
		t.Errorf("expected stateStore sheets=215, got %d", storedSheets)
	}
}

// Test H — TestClientRejectsWrongKioskJob
func TestClientRejectsWrongKioskJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_h.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var downloadAttempted atomic.Bool
	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadAttempted.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer pdfServer.Close()

	jobWrong := contracts.PrintJobCommand{
		JobID:          "job-wrong-kiosk",
		KioskID:        "kiosk-wrong",
		DisplayCode:    "9999",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobWrong})
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-right",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-h",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	time.Sleep(150 * time.Millisecond)

	if downloadAttempted.Load() {
		t.Fatal("download should not be attempted for mismatched kiosk job")
	}

	inc, _ := store.GetIncompleteJob(ctx)
	if inc != nil {
		t.Fatal("no job should be stored for mismatched kiosk")
	}
}

// Test I — TestClientDuplicateJobDoesNotPrintTwice
func TestClientDuplicateJobDoesNotPrintTwice(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_i.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	printerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("printer listener failed: %v", err)
	}
	defer printerLn.Close()

	var printConnections atomic.Int32
	holdPrint := make(chan struct{})
	printerDone := make(chan struct{})

	go func() {
		for {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			printConnections.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
				<-holdPrint
				_, _ = c.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
				close(printerDone)
			}(conn)
		}
	}()

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-dup-001",
		KioskID:        "kiosk-i",
		DisplayCode:    "5555",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	sendDup := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Send initial job
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})

		// Send duplicate when triggered
		<-sendDup
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-i",
		StateStore:    store,
		PrinterAddr:   printerLn.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-i",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Wait until print connection is made
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if printConnections.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if printConnections.Load() != 1 {
		t.Fatalf("expected 1 print connection, got %d", printConnections.Load())
	}

	// Send duplicate job while first print is still active
	close(sendDup)
	time.Sleep(100 * time.Millisecond)

	// Ensure no second print connection was created
	if printConnections.Load() != 1 {
		t.Fatalf("expected exactly 1 print connection, got %d", printConnections.Load())
	}

	// Release printer and complete
	close(holdPrint)
	select {
	case <-printerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for print to complete")
	}
}

// Test J — TestClientStartupRecovery
func TestClientStartupRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_j.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	_, pdfBytes, pdfSum := createThreePageFixture(t, tmpDir)

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfServer.Close()

	// Case 1: Incomplete job with PagesPrinted = 0
	t.Run("recovery with 0 pages printed", func(t *testing.T) {
		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		go func() {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
			_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		}()

		job0 := contracts.PrintJobCommand{
			JobID:          "job-rec-0",
			KioskID:        "kiosk-j",
			DisplayCode:    "0000",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		// Save incomplete job in store before starting client
		if err := store.SaveJob(context.Background(), job0, contracts.JobStatusPausedError); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		completedCh := make(chan string, 1)
		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-j",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/kiosk-j",
			ScreenSink:    noopSink,
			OnJobCompleted: func(jobID string) {
				completedCh <- jobID
			},
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		select {
		case completedID := <-completedCh:
			if completedID != job0.JobID {
				t.Fatalf("expected completed job %s, got %s", job0.JobID, completedID)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("incomplete job with 0 pages was not recovered to COMPLETED within 10s")
		}

		// Verify the persisted job status is actually COMPLETED, not merely absent from the incomplete-job query
		savedJob, err := store.GetJob(context.Background(), job0.JobID)
		if err != nil {
			t.Fatalf("failed to query recovered job: %v", err)
		}
		if savedJob == nil {
			t.Fatalf("expected job %s to be persisted in store, found nil", job0.JobID)
		}
		if savedJob.Status != contracts.JobStatusCompleted {
			t.Fatalf("expected recovered job status to be %s, got %s", contracts.JobStatusCompleted, savedJob.Status)
		}
		if savedJob.SheetsPrinted != job0.TotalSheets {
			t.Fatalf("expected %d sheets printed, got %d", job0.TotalSheets, savedJob.SheetsPrinted)
		}
		if savedJob.PagesPrinted != job0.TotalPages {
			t.Fatalf("expected %d pages printed, got %d", job0.TotalPages, savedJob.PagesPrinted)
		}

		inc, err := store.GetIncompleteJob(context.Background())
		if err != nil {
			t.Fatalf("GetIncompleteJob failed: %v", err)
		}
		if inc != nil {
			t.Fatalf("expected no incomplete job, found %+v", inc)
		}
	})

	// Case 2: Incomplete job with PagesPrinted = 1: zero Pi-side slicing, pause safely in PAUSED_ERROR
	t.Run("recovery with partial print pauses safely without Pi slicing", func(t *testing.T) {
		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		var receivedBytes bytes.Buffer
		go func() {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.Copy(&receivedBytes, conn)
		}()

		jobSlice := contracts.PrintJobCommand{
			JobID:          "job-rec-partial",
			KioskID:        "kiosk-j",
			DisplayCode:    "3333",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), jobSlice, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Update progress to 1 page and 1 sheet printed
		evt := contracts.JobProgressEvent{
			JobID:         jobSlice.JobID,
			KioskID:       jobSlice.KioskID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  1,
			SheetsPrinted: 1,
			Timestamp:     time.Now().UTC(),
		}
		if err := store.UpdateProgress(context.Background(), evt); err != nil {
			t.Fatalf("UpdateProgress failed: %v", err)
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-j",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/kiosk-j",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		deadline := time.Now().Add(5 * time.Second)
		paused := false
		for time.Now().Before(deadline) {
			inc, err := store.GetIncompleteJob(ctx)
			if err == nil && inc != nil && inc.Status == contracts.JobStatusPausedError {
				if strings.Contains(inc.ErrorReason, "reconciliation required") {
					paused = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}

		if !paused {
			t.Fatal("partially printed job was not paused in PAUSED_ERROR with reconciliation required")
		}

		// Verify job record in store is preserved in PAUSED_ERROR
		savedPartial, err := store.GetJob(context.Background(), jobSlice.JobID)
		if err != nil {
			t.Fatalf("failed to query partially printed job: %v", err)
		}
		if savedPartial == nil {
			t.Fatalf("expected job %s to be persisted in store, found nil", jobSlice.JobID)
		}
		if savedPartial.Status != contracts.JobStatusPausedError {
			t.Fatalf("expected job status %s, got %s", contracts.JobStatusPausedError, savedPartial.Status)
		}
		if !strings.Contains(savedPartial.ErrorReason, "reconciliation required") {
			t.Fatalf("expected error reason to require reconciliation, got %q", savedPartial.ErrorReason)
		}

		// Verify zero bytes were submitted to the printer (no Pi slicing or duplicate reprint)
		if receivedBytes.Len() > 0 {
			t.Fatalf("expected 0 bytes sent to printer, got %d bytes", receivedBytes.Len())
		}
	})
}

// Test K — TestClientCancellationStopsOwnedGoroutines
func TestClientCancellationStopsOwnedGoroutines(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_k.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-k",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-k",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Run(ctx)
	}()

	// Let it run for a short duration
	time.Sleep(50 * time.Millisecond)

	// Cancel context
	cancel()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client.Run did not terminate promptly after context cancellation")
	}
}

func TestClientProcessesCUPSJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cups.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	var cupsPrinted atomic.Bool
	var cupsQueueUsed string
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsQueueUsed = queue
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		cupsPrinted.Store(true)
		return nil
	}

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-cups-001",
		KioskID:        "kiosk-cups",
		DisplayCode:    "5566",
		DownloadURL:    pdfServer.URL + "/cups.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var progressEvents []contracts.JobProgressEvent
	var eventsMu sync.Mutex

	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		// Push print job
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			if msg.Type == "progress" && msg.Progress != nil {
				eventsMu.Lock()
				progressEvents = append(progressEvents, *msg.Progress)
				eventsMu.Unlock()
			}
		}
	}))
	defer cloudServer.Close()

	cloudWSURL := "ws" + strings.TrimPrefix(cloudServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      cloudWSURL,
		KioskID:       "kiosk-cups",
		StateStore:    store,
		CUPSQueue:     "DCPT310",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-cups",
		ScreenSink:    noopSink,
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient with CUPS queue failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(4 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		if cupsPrinted.Load() {
			inc, err := store.GetIncompleteJob(ctx)
			if err == nil && inc == nil {
				completed = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !cupsPrinted.Load() {
		t.Fatal("expected CUPS print job to be executed")
	}
	if cupsQueueUsed != "DCPT310" {
		t.Fatalf("expected CUPS queue DCPT310, got %q", cupsQueueUsed)
	}
	if !completed {
		t.Fatal("expected job to reach COMPLETED status in store")
	}
}

func TestClientResetsToIdleAfterCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var cupsPrinted atomic.Bool
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsPrinted.Store(true)
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		// Send job command
		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-idle-reset",
			KioskID:        "kiosk-idle",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		// Read progress events until canceled
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:                 wsURL,
		KioskID:                  "kiosk-idle",
		StateStore:               store,
		CUPSQueue:                "DCPT310",
		DownloadDir:              tmpDir,
		IdleQRContent:            "https://kiosk.test/idle",
		CompletedDisplayDuration: 50 * time.Millisecond,
		ScreenSink:               func(*image.RGBA) error { return nil },
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

	// Wait for idle screen reset after completion
	deadline := time.Now().Add(5 * time.Second)
	resetSeen := false
	for time.Now().Before(deadline) {
		client.renderHistoryMu.Lock()
		history := make([]ui_ipc.ScreenState, len(client.renderHistory))
		copy(history, client.renderHistory)
		client.renderHistoryMu.Unlock()

		if len(history) >= 4 {
			last := history[len(history)-1]
			if last.Mode == ui_ipc.ModeIdle && last.QRContent == "https://kiosk.test/idle" {
				resetSeen = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !resetSeen {
		t.Fatal("expected kiosk screen to reset to ModeIdle after completion duration")
	}
}

func TestSpooler_SNMPTargetSelectionPolicy(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test state db: %v", err)
	}
	defer store.Close()

	baseCfg := ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-target-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/idle",
		ScreenSink:    noopSink,
		PrinterAddr:   "192.168.99.2:9100", // Default PJL address
	}

	t.Run("CUPS XeroxGoSim localhost device URI yields no automatic SNMP target", func(t *testing.T) {
		cfg := baseCfg
		cfg.CUPSQueue = "XeroxGoSim"
		cfg.CUPSRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" && len(args) == 2 && args[0] == "-v" && args[1] == "XeroxGoSim" {
				return []byte("device for XeroxGoSim: ipp://localhost:8631/ipp/print\n"), nil
			}
			return nil, errors.New("unexpected command")
		}

		c, err := NewClient(cfg)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		snmp := c.getSNMPConfig()
		if snmp != nil {
			t.Errorf("expected getSNMPConfig() to return nil for loopback discovery, got %+v", snmp)
		}
	})

	t.Run("CUPS HPTank yields discovered host 10.42.0.210", func(t *testing.T) {
		cfg := baseCfg
		cfg.CUPSQueue = "HPTank"
		cfg.CUPSRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lpstat" && len(args) == 2 && args[0] == "-v" && args[1] == "HPTank" {
				return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
			}
			return nil, errors.New("unexpected command")
		}

		c, err := NewClient(cfg)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		snmp := c.getSNMPConfig()
		if snmp == nil {
			t.Fatal("expected non-nil SNMPConfig for HPTank discovery")
		}
		if snmp.Target != "10.42.0.210" {
			t.Errorf("expected SNMP target 10.42.0.210, got %s", snmp.Target)
		}
	})

	t.Run("explicit SNMP target overrides CUPS discovery", func(t *testing.T) {
		cfg := baseCfg
		cfg.CUPSQueue = "HPTank"
		cfg.SNMPTarget = "127.0.0.1" // Explicit 127.0.0.1 must be honored
		cfg.CUPSRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("device for HPTank: ipp://10.42.0.210:631/ipp/print\n"), nil
		}

		c, err := NewClient(cfg)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		snmp := c.getSNMPConfig()
		if snmp == nil {
			t.Fatal("expected non-nil SNMPConfig for explicit SNMP target")
		}
		if snmp.Target != "127.0.0.1" {
			t.Errorf("expected SNMP target 127.0.0.1, got %s", snmp.Target)
		}
	})

	t.Run("failed CUPS discovery does not use 192.168.99.2", func(t *testing.T) {
		cfg := baseCfg
		cfg.CUPSQueue = "BrokenQueue"
		cfg.PrinterAddr = "192.168.99.2:9100"
		cfg.CUPSRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("lpstat: queue not found")
		}

		c, err := NewClient(cfg)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		snmp := c.getSNMPConfig()
		if snmp != nil {
			t.Errorf("expected getSNMPConfig() = nil when CUPS discovery fails, got %+v (must not use 192.168.99.2)", snmp)
		}
	})

	t.Run("raw TCP/PJL mode preserves current target behavior", func(t *testing.T) {
		// Non-loopback printer address
		cfg1 := baseCfg
		cfg1.CUPSQueue = ""
		cfg1.PrinterAddr = "192.168.1.50:9100"

		c1, err := NewClient(cfg1)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		snmp1 := c1.getSNMPConfig()
		if snmp1 == nil {
			t.Fatal("expected non-nil SNMPConfig for raw TCP 192.168.1.50")
		}
		if snmp1.Target != "192.168.1.50" {
			t.Errorf("expected SNMP target 192.168.1.50, got %s", snmp1.Target)
		}

		// Loopback printer address
		cfg2 := baseCfg
		cfg2.CUPSQueue = ""
		cfg2.PrinterAddr = "127.0.0.1:9100"

		c2, err := NewClient(cfg2)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		snmp2 := c2.getSNMPConfig()
		if snmp2 != nil {
			t.Errorf("expected getSNMPConfig() = nil for loopback raw TCP address, got %+v", snmp2)
		}
	})
}

func TestJobPDFCleanup(t *testing.T) {
	// Test A — Successful completion
	t.Run("successful completion deletes intended PDF and preserves others", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "state.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()

		ctx := context.Background()
		cmd := contracts.PrintJobCommand{
			JobID:         "job-clean-A",
			KioskID:       "kiosk-cleanup",
			TotalPages:    2,
			TotalSheets:   2,
			Copies:        1,
			StartFromPage: 1,
			Duplex:        contracts.DuplexSimplex,
		}
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		intendedPDF := jobPDFPath(tmpDir, cmd.JobID)
		if err := os.WriteFile(intendedPDF, []byte("%PDF-1.4 intended"), 0644); err != nil {
			t.Fatalf("failed to write intended PDF: %v", err)
		}

		otherCmd := contracts.PrintJobCommand{
			JobID:         "job-clean-other",
			KioskID:       "kiosk-cleanup",
			TotalPages:    1,
			TotalSheets:   1,
			Copies:        1,
			StartFromPage: 1,
			Duplex:        contracts.DuplexSimplex,
		}
		otherPDF := jobPDFPath(tmpDir, otherCmd.JobID)
		if err := os.WriteFile(otherPDF, []byte("%PDF-1.4 other"), 0644); err != nil {
			t.Fatalf("failed to write other PDF: %v", err)
		}

		var completedCalled bool
		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-cleanup",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-cleanup",
			ScreenSink:    noopSink,
			OnJobCompleted: func(jobID string) {
				if jobID == cmd.JobID {
					completedCalled = true
				}
			},
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.completeJob(ctx, cmd)

		// Assert that the job becomes COMPLETED
		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved == nil || saved.Status != contracts.JobStatusCompleted {
			t.Fatalf("expected job status COMPLETED, got %+v", saved)
		}

		// Assert that the PDF is removed from disk
		if _, err := os.Stat(intendedPDF); !os.IsNotExist(err) {
			t.Errorf("expected intended PDF %s to be deleted, but it exists", intendedPDF)
		}

		// Assert that only the intended job file was deleted (other job PDF remains)
		if _, err := os.Stat(otherPDF); err != nil {
			t.Errorf("expected other PDF %s to remain on disk, got err: %v", otherPDF, err)
		}

		if !completedCalled {
			t.Error("expected OnJobCompleted callback to be invoked")
		}
	})

	// Test B — Persistence failure
	t.Run("persistence failure preserves PDF and does not invoke callback", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "state.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()

		ctx := context.Background()
		cmd := contracts.PrintJobCommand{
			JobID:         "job-clean-B",
			KioskID:       "kiosk-cleanup",
			TotalPages:    2,
			TotalSheets:   2,
			Copies:        1,
			StartFromPage: 1,
			Duplex:        contracts.DuplexSimplex,
		}
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Arrange for persistence failure by exhausting paper inventory to 0 while job requires 2 sheets
		if err := store.ResetPaperTray(ctx, 0); err != nil {
			t.Fatalf("ResetPaperTray failed: %v", err)
		}

		pdfPath := jobPDFPath(tmpDir, cmd.JobID)
		if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 fail-test"), 0644); err != nil {
			t.Fatalf("failed to write PDF: %v", err)
		}

		var callbackInvoked bool
		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-cleanup",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-cleanup",
			ScreenSink:    noopSink,
			OnJobCompleted: func(jobID string) {
				callbackInvoked = true
			},
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.completeJob(ctx, cmd)

		// Assert the PDF still exists
		if _, err := os.Stat(pdfPath); err != nil {
			t.Errorf("expected PDF %s to still exist after persistence failure, got err: %v", pdfPath, err)
		}

		// Assert the completion callback is not invoked
		if callbackInvoked {
			t.Error("expected completion callback NOT to be invoked on persistence failure")
		}

		// Assert job was not marked completed (transitions to PAUSED_ERROR via handleJobError)
		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved != nil && saved.Status == contracts.JobStatusCompleted {
			t.Errorf("job must not be marked COMPLETED when persistence fails")
		}
	})

	// Test C — Missing PDF
	t.Run("missing PDF does not prevent successful completion", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "state.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()

		ctx := context.Background()
		cmd := contracts.PrintJobCommand{
			JobID:         "job-clean-C",
			KioskID:       "kiosk-cleanup",
			TotalPages:    1,
			TotalSheets:   1,
			Copies:        1,
			StartFromPage: 1,
			Duplex:        contracts.DuplexSimplex,
		}
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Ensure PDF does NOT exist on disk
		pdfPath := jobPDFPath(tmpDir, cmd.JobID)
		_ = os.Remove(pdfPath)

		var callbackInvoked bool
		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-cleanup",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-cleanup",
			ScreenSink:    noopSink,
			OnJobCompleted: func(jobID string) {
				callbackInvoked = true
			},
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.completeJob(ctx, cmd)

		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved == nil || saved.Status != contracts.JobStatusCompleted {
			t.Fatalf("expected job status COMPLETED despite missing PDF, got %+v", saved)
		}

		if !callbackInvoked {
			t.Error("expected completion callback to be invoked even if PDF was already absent")
		}
	})

	// Test D — Path consistency
	t.Run("path consistency, uniqueness, and path traversal resistance", func(t *testing.T) {
		downloadDir := "/var/kiosk/downloads"

		// 1. Verify canonical hash formulation matches download & cleanup
		jobID := "order-12345"
		expectedHash := sha256.Sum256([]byte(jobID))
		expectedPath := filepath.Join(downloadDir, hex.EncodeToString(expectedHash[:])+".pdf")
		if got := jobPDFPath(downloadDir, jobID); got != expectedPath {
			t.Errorf("jobPDFPath() = %q, want %q", got, expectedPath)
		}

		// 2. Distinct job IDs produce distinct paths
		path1 := jobPDFPath(downloadDir, "job-alpha")
		path2 := jobPDFPath(downloadDir, "job-beta")
		if path1 == path2 {
			t.Errorf("distinct job IDs produced identical path: %s", path1)
		}

		// 3. Path traversal resistance
		traversalIDs := []string{
			"../../etc/passwd",
			"../../../tmp/evil",
			"..",
			"/absolute/path/override",
			"sub/dir/job",
			"foo\x00bar",
		}
		for _, evilID := range traversalIDs {
			p := jobPDFPath(downloadDir, evilID)
			if filepath.Dir(p) != filepath.Clean(downloadDir) {
				t.Errorf("job ID %q escaped download directory: %s", evilID, p)
			}
			base := filepath.Base(p)
			if !strings.HasSuffix(base, ".pdf") || len(base) != 64+4 {
				t.Errorf("job ID %q generated unexpected base filename: %s", evilID, base)
			}
		}
	})

	// Test E — Interrupted / paused jobs retain their files
	t.Run("interrupted and paused jobs retain downloaded PDF files", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "state.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()

		ctx := context.Background()
		cmd := contracts.PrintJobCommand{
			JobID:         "job-clean-E-interrupted",
			KioskID:       "kiosk-cleanup",
			TotalPages:    4,
			TotalSheets:   2,
			Copies:        1,
			StartFromPage: 1,
			Duplex:        contracts.DuplexSimplex,
		}
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		pdfPath := jobPDFPath(tmpDir, cmd.JobID)
		if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 recovery-data"), 0644); err != nil {
			t.Fatalf("failed to write PDF: %v", err)
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-cleanup",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-cleanup",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		// Trigger job error (e.g. hardware fault interruption)
		client.handleJobError(ctx, cmd, 1, 1, "paper feed jam detected in printer")

		// Verify job is PAUSED_ERROR
		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved == nil || saved.Status != contracts.JobStatusPausedError {
			t.Fatalf("expected job status PAUSED_ERROR, got %+v", saved)
		}

		// Verify PDF file was NOT deleted
		if _, err := os.Stat(pdfPath); err != nil {
			t.Fatalf("interrupted job PDF must remain on disk for recovery, but got err: %v", err)
		}
	})
}

// Test K — TestDurableJobAcceptanceAndAck
// Verifies Scenarios A through H from Task B2 specification.
func TestDurableJobAcceptanceAndAck(t *testing.T) {
	// Scenario A: SQLite persistence failure
	t.Run("ScenarioA_PersistenceFailure", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_a.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		// Add SQLite trigger to force SaveJob INSERT to fail
		rawDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open raw sqlite db: %v", err)
		}
		defer rawDB.Close()
		_, err = rawDB.Exec("CREATE TRIGGER fail_insert BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT, 'forced persistence failure'); END;")
		if err != nil {
			t.Fatalf("failed to create trigger: %v", err)
		}

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-a",
			KioskID:        "kiosk-a",
			DisplayCode:    "0001",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		var receivedMessages []wireMessage
		var msgMu sync.Mutex
		doneSync := make(chan struct{})

		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send the job that will fail persistence
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})
			// Deterministic synchronization probe: send refill immediately after.
			// Because handleConnection processes messages sequentially on a single goroutine,
			// receiving "health" deterministically proves handleIncomingJob finished.
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 200})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				msgMu.Lock()
				receivedMessages = append(receivedMessages, msg)
				msgMu.Unlock()
				if msg.Type == "health" {
					close(doneSync)
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-a",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-a",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case <-doneSync:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for deterministic sync health message")
		}

		// Assert that neither job_ack nor false SPOOLING progress was transmitted
		msgMu.Lock()
		for _, m := range receivedMessages {
			if m.Type == "job_ack" && m.Progress != nil && m.Progress.JobID == cmd.JobID {
				t.Errorf("job_ack must NOT be transmitted on persistence failure: %+v", m)
			}
			if m.Type == "progress" && m.Progress != nil && m.Progress.JobID == cmd.JobID {
				t.Errorf("progress must NOT be transmitted on persistence failure: %+v", m)
			}
		}
		msgMu.Unlock()

		// Assert: reservation released
		client.activeMu.Lock()
		reserved := client.activeJobs[cmd.JobID]
		client.activeMu.Unlock()
		if reserved {
			t.Errorf("expected in-memory reservation for %s to be released on persistence failure", cmd.JobID)
		}

		// Assert: not submitted to jobQueue
		if len(client.jobQueue) != 0 {
			t.Errorf("expected jobQueue to be empty, got %d items", len(client.jobQueue))
		}

		// Assert: not in store
		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil {
			t.Fatalf("GetJob error: %v", err)
		}
		if saved != nil {
			t.Errorf("expected job not to be saved in store, got %+v", saved)
		}
	})

	// Scenario B: Successful acceptance
	t.Run("ScenarioB_SuccessfulAcceptance", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_b.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-b",
			KioskID:        "kiosk-b",
			DisplayCode:    "0002",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		receivedAck := make(chan wireMessage, 1)
		receivedProgress := make(chan wireMessage, 5)
		doneSync := make(chan struct{})
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send job command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})
			// Deterministic synchronization: probe with refill message
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 200})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "progress" {
					select {
					case receivedProgress <- msg:
					default:
					}
				}
				if msg.Type == "job_ack" {
					// When job_ack is emitted, verify job was ALREADY persisted in store
					saved, err := store.GetJob(context.Background(), cmd.JobID)
					if err != nil || saved == nil {
						t.Errorf("expected job %s to be persisted in store upon job_ack, got saved=%v, err=%v", cmd.JobID, saved, err)
					} else if saved.Status != contracts.JobStatusSpooling {
						t.Errorf("expected persisted status SPOOLING, got %s", saved.Status)
					}
					receivedAck <- msg
				}
				if msg.Type == "health" {
					close(doneSync)
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-b",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-b",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		conn, _, err := websocket.Dial(ctx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(ctx, conn)
		}()

		select {
		case <-doneSync:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for sync health message")
		}

		select {
		case ack := <-receivedAck:
			if ack.Progress == nil || ack.Progress.JobID != cmd.JobID {
				t.Fatalf("unexpected job_ack payload: %+v", ack)
			}
			if ack.Progress.Status != contracts.JobStatusSpooling {
				t.Fatalf("expected job_ack status SPOOLING, got %s", ack.Progress.Status)
			}
		default:
			t.Fatal("expected job_ack message")
		}

		// Admission itself must emit ONLY job_ack, never synthetic progress
		select {
		case prog := <-receivedProgress:
			t.Fatalf("handleIncomingJob must not send progress event; expected only job_ack, got: %+v", prog)
		default:
		}

		// Verify queue admission
		select {
		case queued := <-client.jobQueue:
			if queued.JobID != cmd.JobID {
				t.Fatalf("expected queued job %s, got %s", cmd.JobID, queued.JobID)
			}
		default:
			t.Fatal("expected job to be present in jobQueue")
		}
	})

	// Scenario C: Duplicate before printing
	t.Run("ScenarioC_DuplicateBeforePrinting", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_c.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
		hasher := sha256.Sum256(pdfContent)
		pdfChecksum := hex.EncodeToString(hasher[:])

		pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(pdfContent)
		}))
		defer pdfServer.Close()

		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		var printConnections atomic.Int32
		holdPrint := make(chan struct{})
		printerDone := make(chan struct{})

		go func() {
			for {
				conn, err := printerLn.Accept()
				if err != nil {
					return
				}
				printConnections.Add(1)
				go func(c net.Conn) {
					defer c.Close()
					_, _ = io.Copy(io.Discard, c)
					<-holdPrint
					_, _ = c.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
					close(printerDone)
				}(conn)
			}
		}()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-c",
			KioskID:        "kiosk-c",
			DisplayCode:    "0003",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		sendDup := make(chan struct{})
		var dupSent atomic.Bool
		var ackCount atomic.Int32
		var dupSpoolingProgressCount atomic.Int32
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				for {
					var msg wireMessage
					if err := wsjson.Read(r.Context(), c, &msg); err != nil {
						break
					}
					if msg.Type == "job_ack" {
						ackCount.Add(1)
					}
					if msg.Type == "progress" && msg.Progress != nil && msg.Progress.JobID == cmd.JobID {
						if dupSent.Load() && msg.Progress.Status == contracts.JobStatusSpooling {
							dupSpoolingProgressCount.Add(1)
						}
					}
				}
			}()

			// Send first command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			<-sendDup
			dupSent.Store(true)
			// Send duplicate command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			<-readDone
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-c",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-c",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		// Wait for first print connection to be established
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if printConnections.Load() == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if printConnections.Load() != 1 {
			t.Fatalf("expected 1 print connection, got %d", printConnections.Load())
		}

		// Trigger duplicate delivery while print is actively streaming
		close(sendDup)

		// Wait for duplicate ack
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if ackCount.Load() >= 2 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ackCount.Load() < 2 {
			t.Fatalf("expected at least 2 job_ack messages (1 for original, 1 for duplicate), got %d", ackCount.Load())
		}

		// Verify duplicate delivery did not emit a redundant or stale SPOOLING progress event
		if dupSpoolingProgressCount.Load() != 0 {
			t.Fatalf("expected 0 SPOOLING progress events from duplicate delivery, got %d (duplicate must not emit stale progress)", dupSpoolingProgressCount.Load())
		}

		// Ensure printer connections count remains strictly 1
		if printConnections.Load() != 1 {
			t.Fatalf("expected strictly 1 printer connection, got %d", printConnections.Load())
		}

		// Release printer
		close(holdPrint)
		select {
		case <-printerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for print to finish")
		}
	})

	// Scenario D: Duplicate after printer submission
	t.Run("ScenarioD_DuplicateAfterPrinterSubmission", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_d.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-d",
			KioskID:        "kiosk-d",
			DisplayCode:    "0004",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		ctx := context.Background()
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}
		if err := store.RecordJobSubmission(ctx, cmd.JobID, "cups-job-456"); err != nil {
			t.Fatalf("RecordJobSubmission failed: %v", err)
		}
		if err := store.UpdateProgress(ctx, contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  2,
			SheetsPrinted: 1,
			Timestamp:     time.Now().UTC(),
		}); err != nil {
			t.Fatalf("UpdateProgress failed: %v", err)
		}

		receivedAck := make(chan wireMessage, 1)
		var gotProgress atomic.Bool
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send duplicate command for already submitted job
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "progress" && msg.Progress != nil && msg.Progress.JobID == cmd.JobID {
					gotProgress.Store(true)
				}
				if msg.Type == "job_ack" {
					receivedAck <- msg
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-d",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-d",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case ack := <-receivedAck:
			if ack.Progress == nil {
				t.Fatalf("expected progress in job_ack, got nil")
			}
			if ack.Progress.Status != contracts.JobStatusPrinting {
				t.Errorf("expected status PRINTING, got %s", ack.Progress.Status)
			}
			if ack.Progress.PagesPrinted != 2 || ack.Progress.SheetsPrinted != 1 {
				t.Errorf("expected 2 pages and 1 sheet printed, got %d pages and %d sheets",
					ack.Progress.PagesPrinted, ack.Progress.SheetsPrinted)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for job_ack")
		}

		if gotProgress.Load() {
			t.Error("duplicate command for already submitted job must not emit redundant progress event")
		}

		// Assert: not submitted to jobQueue
		if len(client.jobQueue) != 0 {
			t.Errorf("expected jobQueue to remain empty, got %d", len(client.jobQueue))
		}

		// Assert: store state is preserved
		stored, err := store.GetJob(ctx, cmd.JobID)
		if err != nil || stored == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if !stored.SubmittedToPrinter || stored.CUPSJobID != "cups-job-456" || stored.PagesPrinted != 2 || stored.SheetsPrinted != 1 {
			t.Errorf("stored job altered: %+v", stored)
		}
	})

	// Scenario E: Duplicate completed job
	t.Run("ScenarioE_DuplicateCompletedJob", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_e.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-e",
			KioskID:        "kiosk-e",
			DisplayCode:    "0005",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     6,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		ctx := context.Background()
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusCompleted); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}
		if err := store.UpdateProgress(ctx, contracts.JobProgressEvent{
			JobID:         cmd.JobID,
			Status:        contracts.JobStatusCompleted,
			PagesPrinted:  6,
			SheetsPrinted: 3,
			Timestamp:     time.Now().UTC(),
		}); err != nil {
			t.Fatalf("UpdateProgress failed: %v", err)
		}

		receivedAck := make(chan wireMessage, 1)
		var gotProgress atomic.Bool
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "progress" && msg.Progress != nil && msg.Progress.JobID == cmd.JobID {
					gotProgress.Store(true)
				}
				if msg.Type == "job_ack" {
					receivedAck <- msg
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-e",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-e",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case ack := <-receivedAck:
			if ack.Progress == nil {
				t.Fatalf("expected progress in job_ack, got nil")
			}
			if ack.Progress.Status != contracts.JobStatusCompleted {
				t.Errorf("expected status COMPLETED, got %s", ack.Progress.Status)
			}
			if ack.Progress.PagesPrinted != 6 || ack.Progress.SheetsPrinted != 3 {
				t.Errorf("expected 6 pages and 3 sheets, got %d pages and %d sheets",
					ack.Progress.PagesPrinted, ack.Progress.SheetsPrinted)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for job_ack")
		}

		if gotProgress.Load() {
			t.Error("duplicate command for already completed job must not emit redundant progress event")
		}

		// Queue must remain empty
		if len(client.jobQueue) != 0 {
			t.Errorf("expected empty jobQueue, got %d", len(client.jobQueue))
		}

		// Store status must not regress
		stored, err := store.GetJob(ctx, cmd.JobID)
		if err != nil || stored == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if stored.Status != contracts.JobStatusCompleted || stored.PagesPrinted != 6 || stored.SheetsPrinted != 3 {
			t.Errorf("stored job regressed: %+v", stored)
		}
	})

	// Scenario F: Conflicting duplicate payload
	t.Run("ScenarioF_ConflictingDuplicatePayload", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_f.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		originalCmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-f",
			KioskID:        "kiosk-f",
			DisplayCode:    "0006",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     5,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		}

		ctx := context.Background()
		if err := store.SaveJob(ctx, originalCmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		conflictingCmd := originalCmd
		conflictingCmd.TotalPages = 99                  // Conflict!
		conflictingCmd.Duplex = contracts.DuplexSimplex // Conflict!

		var gotAck atomic.Bool
		doneSync := make(chan struct{})
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &conflictingCmd})
			// Deterministic synchronization: probe with refill message
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 200})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "job_ack" {
					gotAck.Store(true)
				}
				if msg.Type == "health" {
					close(doneSync)
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-f",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-f",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case <-doneSync:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for sync health message")
		}

		if gotAck.Load() {
			t.Fatal("conflicting command must not be acknowledged")
		}

		if len(client.jobQueue) != 0 {
			t.Errorf("conflicting command must not be queued, got %d items", len(client.jobQueue))
		}

		stored, err := store.GetJob(ctx, originalCmd.JobID)
		if err != nil || stored == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if stored.Command.TotalPages != 5 || stored.Command.Duplex != contracts.DuplexLongEdge {
			t.Errorf("original stored job settings modified: TotalPages=%d, Duplex=%s",
				stored.Command.TotalPages, stored.Command.Duplex)
		}
	})

	// Scenario G: Queue saturation durable acceptance
	t.Run("ScenarioG_QueueSaturationDurableAcceptance", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_g.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-g",
			KioskID:        "kiosk-g",
			DisplayCode:    "0007",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		var receivedMessages []wireMessage
		var msgMu sync.Mutex
		doneSync := make(chan struct{})

		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send the job command when in-memory queue is already saturated
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				msgMu.Lock()
				receivedMessages = append(receivedMessages, msg)
				msgMu.Unlock()
				// Once committed to SQLite, client sends job_ack regardless of queue saturation
				if msg.Type == "job_ack" && msg.Progress != nil && msg.Progress.JobID == cmd.JobID {
					close(doneSync)
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-g",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-g",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		// Fill jobQueue to capacity
		for len(client.jobQueue) < cap(client.jobQueue) {
			client.jobQueue <- contracts.PrintJobCommand{JobID: "dummy-fill"}
		}

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case <-doneSync:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for job_ack under queue saturation")
		}

		// Assert: durable record preserved in store in SPOOLING status
		ctx := context.Background()
		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("expected saved record in store, got: %v, err: %v", saved, err)
		}
		if saved.Status != contracts.JobStatusSpooling {
			t.Errorf("expected saved status SPOOLING, got %s", saved.Status)
		}

		// Assert: admitted and buffered by ordered producer under queue saturation
		deadlineG := time.Now().Add(2 * time.Second)
		var pendingCount int
		var isProducerRunning bool
		for time.Now().Before(deadlineG) {
			client.queueMu.Lock()
			pendingCount = len(client.pendingQueue)
			isProducerRunning = client.producerRunning
			client.queueMu.Unlock()
			if pendingCount > 0 || isProducerRunning {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pendingCount == 0 && !isProducerRunning {
			t.Errorf("expected job to be pending or handled by ordered producer")
		}

		// Assert: GetIncompleteJob finds this job for startup recovery
		incomplete, err := store.GetIncompleteJob(ctx)
		if err != nil || incomplete == nil {
			t.Fatalf("expected GetIncompleteJob to find the job for recovery, got: %v, err: %v", incomplete, err)
		}
		if incomplete.Command.JobID != cmd.JobID {
			t.Errorf("expected incomplete job ID %s, got %s", cmd.JobID, incomplete.Command.JobID)
		}
	})

	// Scenario H: Reconnect / repeated delivery
	t.Run("ScenarioH_ReconnectRepeatedDelivery", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_h.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
		hasher := sha256.Sum256(pdfContent)
		pdfChecksum := hex.EncodeToString(hasher[:])

		pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(pdfContent)
		}))
		defer pdfServer.Close()

		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		var printConnections atomic.Int32
		go func() {
			for {
				conn, err := printerLn.Accept()
				if err != nil {
					return
				}
				printConnections.Add(1)
				go func(c net.Conn) {
					defer c.Close()
					_, _ = io.Copy(io.Discard, c)
					_, _ = c.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
				}(conn)
			}
		}()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-h",
			KioskID:        "kiosk-h",
			DisplayCode:    "0008",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		var connectionCount atomic.Int32
		reconnectedAck := make(chan wireMessage, 1)

		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connNum := connectionCount.Add(1)
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			if connNum == 1 {
				// First connection: send job, read until spooling job_ack, then close to simulate lost connection
				_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})
				for {
					var msg wireMessage
					if err := wsjson.Read(r.Context(), c, &msg); err != nil {
						break
					}
					if msg.Type == "job_ack" {
						_ = c.Close(websocket.StatusGoingAway, "lost connection")
						return
					}
				}
				return
			}

			// Reconnected session: re-send same job command (cloud retrying unacknowledged dispatch)
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "job_ack" {
					select {
					case reconnectedAck <- msg:
					default:
					}
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-h",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-h",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.minBackoff = 50 * time.Millisecond
		client.maxBackoff = 100 * time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		select {
		case ack := <-reconnectedAck:
			if ack.Progress == nil || ack.Progress.JobID != cmd.JobID {
				t.Fatalf("unexpected reconnected ack: %+v", ack)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for reconnected job_ack")
		}

		// Wait briefly for all operations to settle
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if printConnections.Load() == 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Assert: printer received strictly 1 physical print connection
		if printConnections.Load() != 1 {
			t.Fatalf("expected exactly 1 physical print connection, got %d", printConnections.Load())
		}
	})

	// Scenario I: Admission sends only job_ack even if job advances to PRINTING before acknowledgment is sent
	t.Run("ScenarioI_AdmissionSendsOnlyAckEvenIfAdvancedToPrinting", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_i.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-scenario-i",
			KioskID:        "kiosk-i",
			DisplayCode:    "0009",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		var receivedMessages []wireMessage
		var msgMu sync.Mutex
		doneSync := make(chan struct{})

		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send job command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})
			// Probe with refill to confirm handleIncomingJob is completely finished
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 200})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				msgMu.Lock()
				receivedMessages = append(receivedMessages, msg)
				msgMu.Unlock()
				if msg.Type == "health" {
					close(doneSync)
					return
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-i",
			StateStore:    store,
			PrinterAddr:   "127.0.0.1:9100",
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-i",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		// Simulate an asynchronous worker that instantly picks up the job from the queue
		// and advances it to PRINTING in the store before acknowledgment is sent.
		workerDone := make(chan struct{})
		go func() {
			defer close(workerDone)
			queued := <-client.jobQueue
			if queued.JobID == cmd.JobID {
				ctx := context.Background()
				_ = store.UpdateProgress(ctx, contracts.JobProgressEvent{
					JobID:         cmd.JobID,
					KioskID:       cmd.KioskID,
					Status:        contracts.JobStatusPrinting,
					PagesPrinted:  1,
					SheetsPrinted: 1,
					Timestamp:     time.Now().UTC(),
				})
			}
		}()

		connCtx, connCancel := context.WithCancel(context.Background())
		defer connCancel()

		conn, _, err := websocket.Dial(connCtx, client.normalizedURL, nil)
		if err != nil {
			t.Fatalf("websocket.Dial failed: %v", err)
		}
		defer conn.CloseNow()

		go func() {
			client.handleConnection(connCtx, conn)
		}()

		select {
		case <-doneSync:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for sync health message")
		}

		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for simulated worker")
		}

		// Inspect all received messages:
		// 1. Must receive exactly 1 job_ack
		// 2. Must NEVER receive a SPOOLING progress event from admission
		msgMu.Lock()
		defer msgMu.Unlock()

		var ackCount int
		var progressCount int

		for _, m := range receivedMessages {
			if m.Type == "job_ack" && m.Progress != nil && m.Progress.JobID == cmd.JobID {
				ackCount++
			}
			if m.Type == "progress" && m.Progress != nil && m.Progress.JobID == cmd.JobID {
				progressCount++
			}
		}

		if ackCount != 1 {
			t.Fatalf("expected exactly 1 job_ack, got %d", ackCount)
		}
		if progressCount != 0 {
			t.Fatalf("admission itself must not emit progress events; got %d progress events", progressCount)
		}

		// Verify job actually advanced to PRINTING in the store
		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved.Status != contracts.JobStatusPrinting {
			t.Fatalf("expected store status PRINTING, got %s", saved.Status)
		}
	})
}

func testPDFServer(t *testing.T) (string, string) {
	t.Helper()
	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/job.pdf", pdfChecksum
}

// Test L — TestCUPSSubmissionPersistence
func TestCUPSSubmissionPersistence(t *testing.T) {
	makePDFServer := testPDFServer

	noopSink := func(img *image.RGBA) error { return nil }

	// A. Pre-submission SQLite failure: Force MarkJobSubmitting() to fail; assert runner is never invoked
	t.Run("ScenarioA_PreSubmissionSQLiteFailure", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_a.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-presub-fail",
			KioskID:        "kiosk-a",
			DisplayCode:    "1001",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Force MarkJobSubmitting to fail using SQLite trigger
		rawDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open raw sqlite db: %v", err)
		}
		defer rawDB.Close()
		_, err = rawDB.Exec("CREATE TRIGGER fail_mark_submitting BEFORE UPDATE OF status ON jobs WHEN NEW.status = 'PRINTING' BEGIN SELECT RAISE(ABORT, 'forced MarkJobSubmitting failure'); END;")
		if err != nil {
			t.Fatalf("failed to create trigger: %v", err)
		}

		var lpInvoked atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpInvoked.Store(true)
				return []byte("request id is DCPT310-101 (1 file(s))\n"), nil
			}
			return []byte(""), nil
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-a",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			CUPSRunner:    runner,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-a",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		if lpInvoked.Load() {
			t.Fatal("CUPS lp submission command must NOT be invoked when MarkJobSubmitting fails")
		}

		// Check stored record: submitted_to_printer must remain false and status must be PAUSED_ERROR
		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved.SubmittedToPrinter {
			t.Error("submitted_to_printer must NOT be set when MarkJobSubmitting failed")
		}
		if saved.Status != contracts.JobStatusPausedError {
			t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
		}
	})

	// B. Successful submission: Verify submission intent persisted before lp, and CUPS job ID recorded afterward
	t.Run("ScenarioB_SuccessfulSubmission", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_b.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-success-sub",
			KioskID:        "kiosk-b",
			DisplayCode:    "1002",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		var lpInvoked atomic.Bool
		var submittedBeforeLP atomic.Bool
		var pollCount atomic.Int32
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpInvoked.Store(true)
				j, err := store.GetJob(context.Background(), cmd.JobID)
				if err == nil && j != nil && j.SubmittedToPrinter && j.CUPSJobID == "" {
					submittedBeforeLP.Store(true)
				}
				return []byte("request id is DCPT310-202 (1 file(s))\n"), nil
			}
			if name == "ipptool" {
				p := pollCount.Add(1)
				if p == 1 {
					return []byte(`
job-id (integer) = 202
job-state (enum) = processing
job-media-sheets-completed (integer) = 1
`), nil
				}
				return []byte(`
job-id (integer) = 202
job-state (enum) = completed
job-media-sheets-completed (integer) = 1
`), nil
			}
			if name == "lpstat" {
				return []byte("DCPT310-202 user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully"), nil
			}
			return []byte(""), nil
		}

		var odoCalls atomic.Int32
		snmpPoller := func(ctx context.Context) printer.HardwareTelemetry {
			c := odoCalls.Add(1)
			var life int64 = 1000
			if c > 1 {
				life += 1
			}
			return printer.HardwareTelemetry{
				Online:    true,
				HasLife:   true,
				LifeCount: life,
			}
		}

		client, err := NewClient(ClientConfig{
			CloudURL:   "ws://127.0.0.1:8080/ws",
			KioskID:    "kiosk-b",
			StateStore: store,
			CUPSQueue:  "DCPT310",
			CUPSRunner: runner,
			SNMPPoller: snmpPoller,
			MockHealth: func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
				return contracts.PrinterHealth{Online: true}
			},
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-b",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		if !lpInvoked.Load() {
			t.Fatal("expected lp to be invoked")
		}
		if !submittedBeforeLP.Load() {
			t.Fatal("expected submitted_to_printer=1 to be durably persisted BEFORE lp is called")
		}

		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if !saved.SubmittedToPrinter {
			t.Error("expected submitted_to_printer to be true")
		}
		if saved.CUPSJobID != "DCPT310-202" {
			t.Errorf("expected CUPSJobID DCPT310-202, got %q", saved.CUPSJobID)
		}
		if saved.Status != contracts.JobStatusCompleted {
			t.Errorf("expected status COMPLETED, got %s", saved.Status)
		}
	})

	// C. Post-submission SQLite failure: Simulate successful lp execution followed by failure to persist the CUPS ID
	t.Run("ScenarioC_PostSubmissionSQLiteFailure", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_c.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-postsub-fail",
			KioskID:        "kiosk-c",
			DisplayCode:    "1003",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Fail RecordJobSubmission by adding trigger on UPDATE OF cups_job_id
		rawDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open raw sqlite db: %v", err)
		}
		defer rawDB.Close()
		_, err = rawDB.Exec("CREATE TRIGGER fail_record_cups_id BEFORE UPDATE OF cups_job_id ON jobs BEGIN SELECT RAISE(ABORT, 'forced RecordJobSubmission failure'); END;")
		if err != nil {
			t.Fatalf("failed to create trigger: %v", err)
		}

		var lpCalls atomic.Int32
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				// Check if hold request
				for i, arg := range args {
					if arg == "-H" && i+1 < len(args) && args[i+1] == "hold" {
						return []byte(""), nil
					}
				}
				lpCalls.Add(1)
				return []byte("request id is DCPT310-303 (1 file(s))"), nil
			}
			return []byte(""), nil
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-c",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			CUPSRunner:    runner,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-c",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		// Assert: No second submission occurs
		if lpCalls.Load() != 1 {
			t.Errorf("expected exactly 1 lp submission call, got %d", lpCalls.Load())
		}

		// Assert: The original CUPS ID remains available for reconciliation in memory
		cupsID := client.GetCUPSJobID(cmd.JobID)
		if cupsID != "DCPT310-303" {
			t.Errorf("expected client.GetCUPSJobID to return DCPT310-303, got %q", cupsID)
		}

		// Assert: Stored record retains submitted_to_printer=1 and is NOT marked COMPLETED
		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if !saved.SubmittedToPrinter {
			t.Error("submitted_to_printer must remain true to preserve ambiguous submission state")
		}
		if saved.Status == contracts.JobStatusCompleted {
			t.Error("job must NOT be marked COMPLETED when post-submission persistence failed")
		}
		if saved.Status != contracts.JobStatusPausedError {
			t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
		}
	})

	// D. CUPS submission command failure: Simulate an error from lp; verify conservative recovery state and no automatic retry
	t.Run("ScenarioD_CUPSSubmissionCommandFailure", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_d.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-lp-fail",
			KioskID:        "kiosk-d",
			DisplayCode:    "1004",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				return []byte("lp: unable to contact CUPS server"), errors.New("exit status 1")
			}
			return []byte(""), nil
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-d",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			CUPSRunner:    runner,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-d",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if !saved.SubmittedToPrinter {
			t.Error("submitted_to_printer must remain true even after lp error (conservative crash safety)")
		}
		if saved.Status != contracts.JobStatusPausedError {
			t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
		}

		// Verify no automatic retry during recovery
		inc, err := store.GetIncompleteJob(context.Background())
		if err != nil {
			t.Fatalf("GetIncompleteJob failed: %v", err)
		}
		if inc == nil {
			t.Fatal("expected incomplete job to be present in store")
		}
		isDefinitelyNotSubmitted := !inc.SubmittedToPrinter && inc.CUPSJobID == "" && inc.SheetsPrinted == 0 && inc.PagesPrinted == 0 && inc.ErrorReason == ""
		if isDefinitelyNotSubmitted {
			t.Fatal("job with failed lp must NOT be treated as definitely unsubmitted")
		}
	})

	// E. Missing or unparseable CUPS request ID: Verify potentially accepted job treated as ambiguous, not definitely unsubmitted
	t.Run("ScenarioE_MissingOrUnparseableCUPSRequestID", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_e.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-unparseable-id",
			KioskID:        "kiosk-e",
			DisplayCode:    "1005",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				return []byte("Job accepted successfully with nonstandard notice"), nil
			}
			return []byte(""), nil
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-e",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			CUPSRunner:    runner,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-e",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if !saved.SubmittedToPrinter {
			t.Error("submitted_to_printer must remain true when request ID was unparseable")
		}
		if saved.CUPSJobID != "" {
			t.Errorf("CUPSJobID must be empty, got %q", saved.CUPSJobID)
		}
		if saved.Status != contracts.JobStatusPausedError {
			t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
		}

		// Verify treated as ambiguous: cannot be automatically retried
		inc, err := store.GetIncompleteJob(context.Background())
		if err != nil || inc == nil {
			t.Fatalf("GetIncompleteJob failed: %v", err)
		}
		isDefinitelyNotSubmitted := !inc.SubmittedToPrinter && inc.CUPSJobID == "" && inc.SheetsPrinted == 0 && inc.PagesPrinted == 0 && inc.ErrorReason == ""
		if isDefinitelyNotSubmitted {
			t.Fatal("job with unparseable CUPS ID must NOT be treated as definitely unsubmitted")
		}
	})

	// F. Restart after submission-intent persistence: Persist submitted_to_printer=1 without CUPS ID, restart, assert no auto re-submission
	t.Run("ScenarioF_RestartAfterSubmissionIntentPersistence", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_f.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:          "job-crash-after-intent",
			KioskID:        "kiosk-f",
			DisplayCode:    "1006",
			DownloadURL:    "https://storage.example.com/job.pdf",
			SHA256Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		ctx := context.Background()
		if err := store.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}
		// Simulate crash right after MarkJobSubmitting
		if err := store.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
			t.Fatalf("MarkJobSubmitting failed: %v", err)
		}

		var lpCalled atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				lpCalled.Store(true)
			}
			return []byte(""), nil
		}

		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
			}
		}))
		defer wsServer.Close()

		wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
		client, err := NewClient(ClientConfig{
			CloudURL:      wsURL,
			KioskID:       "kiosk-f",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			CUPSRunner:    runner,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-f",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()

		go func() {
			_ = client.Run(runCtx)
		}()

		// Wait briefly with condition check for startup recovery to process the incomplete job
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			saved, err := store.GetJob(context.Background(), cmd.JobID)
			if err == nil && saved != nil && saved.Status == contracts.JobStatusPausedError {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if lpCalled.Load() {
			t.Fatal("startup recovery must NOT automatically invoke lp for potentially submitted job")
		}
		if len(client.jobQueue) != 0 {
			t.Fatalf("expected jobQueue to remain empty, got %d", len(client.jobQueue))
		}

		saved, err := store.GetJob(context.Background(), cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if saved.Status != contracts.JobStatusPausedError {
			t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
		}
		if !saved.SubmittedToPrinter {
			t.Error("submitted_to_printer must remain true")
		}
	})

	// G. Cancellation and existing monitoring: Ensure successful jobs retain behavior and no unmonitored printing path
	t.Run("ScenarioG_CancellationAndExistingMonitoring", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_g.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		cmd := contracts.PrintJobCommand{
			JobID:       "job-cancel-test",
			Copies:      1,
			TotalPages:  1,
			TotalSheets: 1,
			Duplex:      contracts.DuplexSimplex,
		}

		var cancelInvoked atomic.Bool
		runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "lp" {
				return []byte("request id is DCPT310-701 (1 file(s))"), nil
			}
			if name == "cancel" {
				cancelInvoked.Store(true)
				return []byte(""), nil
			}
			if name == "lpstat" {
				return []byte("DCPT310-701 user 1024 Tue Nov 3 14:00:00 2026\n\tprocessing"), nil
			}
			return []byte(""), nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel after short delay
		time.AfterFunc(30*time.Millisecond, cancel)

		err = printer.PrintPDFWithCUPSHooksCustom(ctx, runner, "DCPT310", cmd, "/tmp/dummy.pdf", nil, 10*time.Millisecond, nil)
		if err == nil {
			t.Fatal("expected error on context cancellation")
		}

		if !cancelInvoked.Load() {
			t.Fatal("expected CUPS cancel to be invoked on context cancellation without job preservation")
		}
	})

	// Step 5: Test MockCUPSPrint pre-submission persistence failure prevents mock print
	t.Run("MockCUPSPrint_PreSubmissionFailure_PreventsPrinting", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_mock.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		pdfURL, pdfChecksum := makePDFServer(t)

		cmd := contracts.PrintJobCommand{
			JobID:          "job-mock-fail",
			KioskID:        "kiosk-mock",
			DisplayCode:    "1007",
			DownloadURL:    pdfURL,
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling)

		rawDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open raw db: %v", err)
		}
		defer rawDB.Close()
		_, _ = rawDB.Exec("CREATE TRIGGER fail_mock_submitting BEFORE UPDATE OF status ON jobs WHEN NEW.status = 'PRINTING' BEGIN SELECT RAISE(ABORT, 'forced MarkJobSubmitting failure'); END;")

		var mockPrinted atomic.Bool
		mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
			mockPrinted.Store(true)
			return nil
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-mock",
			StateStore:    store,
			CUPSQueue:     "DCPT310",
			MockCUPSPrint: mockCUPS,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://kiosk-mock",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		client.processJob(context.Background(), cmd)

		if mockPrinted.Load() {
			t.Fatal("MockCUPSPrint must NOT be called when MarkJobSubmitting fails")
		}
	})
}

func TestCUPSHoldOutcomeHandling(t *testing.T) {
	testCases := []struct {
		name              string
		cupsJobID         string
		holdErr           error
		queryState        string
		queryReasons      string
		queryErr          error
		expectedSubstrs   []string
		unexpectedSubstrs []string
	}{
		{
			name:         "ConfirmedHold",
			cupsJobID:    "DCPT310-hold-01",
			queryState:   "held",
			queryReasons: "job-held-by-user",
			expectedSubstrs: []string{
				"DCPT310-hold-01",
				"held (buffered sheets may continue printing)",
				"manual reconciliation required",
			},
			unexpectedSubstrs: []string{
				"possible unmonitored physical output",
			},
		},
		{
			name:         "FailedHold",
			cupsJobID:    "DCPT310-hold-02",
			holdErr:      errors.New("lp: server-error-service-unavailable"),
			queryState:   "processing",
			queryReasons: "none",
			expectedSubstrs: []string{
				"DCPT310-hold-02",
				"hold request failed",
				"possible unmonitored physical output",
				"manual reconciliation required",
			},
		},
		{
			name:         "StillProcessing",
			cupsJobID:    "DCPT310-hold-03",
			queryState:   "processing",
			queryReasons: "none",
			expectedSubstrs: []string{
				"DCPT310-hold-03",
				"remains processing after hold request",
				"possible unmonitored physical output",
				"manual reconciliation required",
			},
		},
		{
			name:         "AlreadyCompleted",
			cupsJobID:    "DCPT310-hold-04",
			queryState:   "completed",
			queryReasons: "job-completed-successfully",
			expectedSubstrs: []string{
				"DCPT310-hold-04",
				"was already completed",
				"possible unmonitored physical output",
				"manual reconciliation required",
			},
		},
		{
			name:      "UnknownState",
			cupsJobID: "DCPT310-hold-05",
			queryErr:  errors.New("ipptool: server unreachable"),
			expectedSubstrs: []string{
				"DCPT310-hold-05",
				"status is unknown",
				"possible unmonitored physical output",
				"manual reconciliation required",
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "kiosk.db")
			store, err := state.Open(dbPath)
			if err != nil {
				t.Fatalf("failed to open store: %v", err)
			}
			defer store.Close()

			pdfURL, pdfChecksum := testPDFServer(t)

			cmd := contracts.PrintJobCommand{
				JobID:          "job-" + tc.name,
				KioskID:        "kiosk-hold-test",
				DisplayCode:    "8888",
				DownloadURL:    pdfURL,
				SHA256Checksum: pdfChecksum,
				TotalPages:     1,
				TotalSheets:    1,
				Copies:         1,
				Duplex:         contracts.DuplexSimplex,
				StartFromPage:  1,
			}

			if err := store.SaveJob(context.Background(), cmd, contracts.JobStatusSpooling); err != nil {
				t.Fatalf("SaveJob failed: %v", err)
			}

			var holdInvoked atomic.Bool
			runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "lp" {
					for i, a := range args {
						if a == "-H" && i+1 < len(args) && args[i+1] == "hold" {
							holdInvoked.Store(true)
							if tc.holdErr != nil {
								return nil, tc.holdErr
							}
							return []byte(""), nil
						}
					}
					return []byte(fmt.Sprintf("request id is %s (1 file(s))\n", tc.cupsJobID)), nil
				}
				if name == "ipptool" {
					if !holdInvoked.Load() {
						// Trigger monitor failure so processJob enters printErr != nil
						return []byte("job-state (enum) = canceled\njob-state-reasons (keyword) = job-canceled-at-device\n"), nil
					}
					if tc.queryErr != nil {
						return nil, tc.queryErr
					}
					return []byte(fmt.Sprintf("job-state (enum) = %s\njob-state-reasons (keyword) = %s\n", tc.queryState, tc.queryReasons)), nil
				}
				if name == "lpstat" {
					if tc.queryErr != nil && holdInvoked.Load() {
						return nil, tc.queryErr
					}
					if holdInvoked.Load() && tc.queryState == "completed" {
						return []byte(fmt.Sprintf("%s user 1024 Tue Nov 3 14:00:00 2026\n\tjob-completed-successfully\n", tc.cupsJobID)), nil
					}
					return []byte(""), nil
				}
				return []byte(""), nil
			}

			client, err := NewClient(ClientConfig{
				CloudURL:      "ws://127.0.0.1:8080/ws",
				KioskID:       "kiosk-hold-test",
				StateStore:    store,
				CUPSQueue:     "DCPT310",
				CUPSRunner:    runner,
				DownloadDir:   tmpDir,
				IdleQRContent: "https://kiosk-hold-test",
				ScreenSink:    noopSink,
			})
			if err != nil {
				t.Fatalf("NewClient failed: %v", err)
			}

			client.processJob(context.Background(), cmd)

			if !holdInvoked.Load() {
				t.Fatal("expected HoldAndVerifyCUPSJob to invoke hold on CUPS runner")
			}

			saved, err := store.GetJob(context.Background(), cmd.JobID)
			if err != nil || saved == nil {
				t.Fatalf("GetJob failed: %v", err)
			}

			if saved.Status != contracts.JobStatusPausedError {
				t.Errorf("expected status PAUSED_ERROR, got %s", saved.Status)
			}
			if !saved.SubmittedToPrinter {
				t.Error("expected submitted_to_printer to be true for accepted CUPS job")
			}
			if saved.CUPSJobID != tc.cupsJobID {
				t.Errorf("expected CUPSJobID %s, got %s", tc.cupsJobID, saved.CUPSJobID)
			}

			for _, expected := range tc.expectedSubstrs {
				if !strings.Contains(saved.ErrorReason, expected) {
					t.Errorf("expected ErrorReason to contain %q, but got: %q", expected, saved.ErrorReason)
				}
			}

			for _, unexpected := range tc.unexpectedSubstrs {
				if strings.Contains(saved.ErrorReason, unexpected) {
					t.Errorf("expected ErrorReason NOT to contain %q, but got: %q", unexpected, saved.ErrorReason)
				}
			}
		})
	}
}

func TestFormatCUPSHoldOutcomeReasonDirect(t *testing.T) {
	baseReason := "printing failed: simulated failure"
	cID := "TEST-99"

	t.Run("ConfirmedHeld", func(t *testing.T) {
		res := printer.HoldResult{
			ConfirmedHeld: true,
			State:         printer.CUPSJobStateHeld,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, cID) {
			t.Errorf("expected cID %s in %q", cID, out)
		}
		if !strings.Contains(out, "held (buffered sheets may continue printing)") {
			t.Errorf("expected held warning in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
		if strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("unexpected unmonitored output warning for confirmed held in %q", out)
		}
	})

	t.Run("FailedHoldWithQueryErr", func(t *testing.T) {
		res := printer.HoldResult{
			HoldErr:   errors.New("hold failed"),
			QueryErr:  errors.New("query failed"),
			IsUnknown: true,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, cID) {
			t.Errorf("expected cID %s in %q", cID, out)
		}
		if !strings.Contains(out, "hold request failed") {
			t.Errorf("expected hold request failed in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})

	t.Run("FailedHoldWithKnownState", func(t *testing.T) {
		res := printer.HoldResult{
			HoldErr: errors.New("hold failed"),
			State:   printer.CUPSJobStateProcessing,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, "hold request failed") || !strings.Contains(out, "state is processing") {
			t.Errorf("expected hold request failed with state in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})

	t.Run("StillProcessing", func(t *testing.T) {
		res := printer.HoldResult{
			StillProcessing: true,
			State:           printer.CUPSJobStateProcessing,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, "remains processing after hold request") {
			t.Errorf("expected remains processing in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})

	t.Run("AlreadyCompleted", func(t *testing.T) {
		res := printer.HoldResult{
			AlreadyCompleted: true,
			State:            printer.CUPSJobStateCompleted,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, "was already completed") {
			t.Errorf("expected was already completed in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})

	t.Run("UnknownStateWithQueryErr", func(t *testing.T) {
		res := printer.HoldResult{
			QueryErr:  errors.New("network down"),
			IsUnknown: true,
			State:     printer.CUPSJobStateUnknown,
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, "status is unknown") || !strings.Contains(out, "network down") {
			t.Errorf("expected status is unknown with error in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})

	t.Run("UnknownStateWithoutQueryErr", func(t *testing.T) {
		res := printer.HoldResult{
			IsUnknown: true,
			State:     "unrecognized_custom_state",
		}
		out := formatCUPSHoldOutcomeReason(baseReason, cID, res)
		if !strings.Contains(out, "status is unknown") || !strings.Contains(out, "unrecognized_custom_state") {
			t.Errorf("expected status is unknown with state in %q", out)
		}
		if !strings.Contains(out, "possible unmonitored physical output") {
			t.Errorf("expected possible unmonitored physical output in %q", out)
		}
		if !strings.Contains(out, "manual reconciliation required") {
			t.Errorf("expected manual reconciliation required in %q", out)
		}
	})
}

// WS-1 Regression Tests

func TestClient_WebSocket_SecretSanitization(t *testing.T) {
	secret := "kiosk-super-secret-auth-key-987654"
	store, err := state.Open(filepath.Join(t.TempDir(), "test_secret.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token: "+r.URL.RawQuery, http.StatusUnauthorized)
	}))
	defer badServer.Close()
	badWSURL := "ws" + strings.TrimPrefix(badServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:      badWSURL,
		KioskID:       "kiosk-001",
		KioskSecret:   secret,
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   t.TempDir(),
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    noopSink,
		MinBackoff:    20 * time.Millisecond,
		MaxBackoff:    50 * time.Millisecond,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. SafeURL check: never include URL query parameters or authentication credentials
	if strings.Contains(client.SafeURL(), secret) {
		t.Fatalf("SafeURL leaked secret token: %s", client.SafeURL())
	}
	if strings.Contains(client.SafeURL(), "?") {
		t.Fatalf("SafeURL must never contain URL query parameters: %s", client.SafeURL())
	}

	// 2. sanitizeError checks
	cases := []struct {
		name     string
		inputErr error
	}{
		{"contains_normalized_url", fmt.Errorf("dial failed: %s: connection refused", client.normalizedURL)},
		{"contains_raw_secret", fmt.Errorf("authentication rejected for secret %s", secret)},
		{"contains_bearer_token", fmt.Errorf("Authorization header Bearer %s expired", secret)},
		{"contains_query_token", fmt.Errorf("failed GET /ws?kiosk_id=kiosk-001&token=%s: 401", secret)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sanitized := client.sanitizeError(tc.inputErr)
			if strings.Contains(sanitized, secret) {
				t.Fatalf("sanitized error leaked secret %q: %s", secret, sanitized)
			}
			if strings.Contains(sanitized, "?") {
				t.Fatalf("sanitized error must never contain URL query parameters: %s", sanitized)
			}
		})
	}

	// 3. Integration log output check during failed dial
	var logBuf safeBuffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origWriter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var logOutput string
	for time.Now().Before(deadline) {
		logOutput = logBuf.String()
		if strings.Contains(logOutput, "[SPOOLER] WebSocket connection to cloud failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-runErrCh

	if strings.Contains(logOutput, secret) {
		t.Fatalf("captured log output leaked secret %q:\n%s", secret, logOutput)
	}
	if strings.Contains(logOutput, "?") {
		t.Fatalf("captured log output must never contain URL query parameters:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "[SPOOLER] WebSocket connection to cloud failed") {
		t.Fatalf("expected failed connection log, got:\n%s", logOutput)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestClient_PingWatchdog_UnresponsivePeer_TriggersReconnect(t *testing.T) {
	var connCount atomic.Int32
	conn2Established := make(chan struct{})

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		count := connCount.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		if count == 1 {
			// First connection: accept WebSocket upgrade, but do NOT read.
			// In coder/websocket, if the peer does not read, incoming pings are never replied to with pong.
			// Hold the connection open so the client's watchdog must detect the timeout.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		} else {
			// Second connection: read normally so client connection is healthy
			select {
			case <-conn2Established:
			default:
				close(conn2Established)
			}
			_ = c.CloseRead(r.Context())
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_watchdog.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:          wsURL,
		KioskID:           "kiosk-watchdog-test",
		KioskSecret:       "watchdog-test-token",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       t.TempDir(),
		IdleQRContent:     "https://example.com/qr",
		ScreenSink:        noopSink,
		PingInterval:      30 * time.Millisecond,
		PingTimeout:       30 * time.Millisecond,
		HeartbeatInterval: 1 * time.Second,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErrCh := make(chan error, 1)
	go func() {
		startErrCh <- client.Run(ctx)
	}()

	select {
	case <-conn2Established:
		// Reconnection succeeded because watchdog terminated unresponsive connection 1
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for client to reconnect after unresponsive peer; total connections: %d", connCount.Load())
	}

	if connCount.Load() < 2 {
		t.Fatalf("expected at least 2 connections (initial + reconnect), got %d", connCount.Load())
	}

	cancel()
	select {
	case err := <-startErrCh:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on cancel: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("client.Run did not terminate promptly after context cancel")
	}
}

type failWriterConn struct {
	net.Conn
	failWrite atomic.Bool
}

func (c *failWriterConn) Write(b []byte) (int, error) {
	if c.failWrite.Load() {
		return 0, errors.New("simulated network write failure")
	}
	return c.Conn.Write(b)
}

type pingTrackingConn struct {
	net.Conn
	pingCount atomic.Int32
}

func (c *pingTrackingConn) Write(b []byte) (int, error) {
	if len(b) > 0 && (b[0]&0x0f) == 0x9 {
		c.pingCount.Add(1)
	}
	return c.Conn.Write(b)
}

func TestClient_Heartbeat_WriteError_TerminatesConnection(t *testing.T) {
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Server keeps the socket open so the client's reader does NOT detect EOF
		go func() {
			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					return
				}
			}
		}()
		<-r.Context().Done()
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_hb_err.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:          wsURL,
		KioskID:           "kiosk-hb-err-test",
		KioskSecret:       "hb-test-token",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       t.TempDir(),
		IdleQRContent:     "https://example.com/qr",
		ScreenSink:        noopSink,
		HeartbeatInterval: 20 * time.Millisecond,
		PingInterval:      10 * time.Second,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	var logBuf safeBuffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origWriter)

	// Dial connection 1 with failWriterConn
	var fw1 *failWriterConn
	dialer := &net.Dialer{}
	dialClient1 := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				rawConn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				fw1 = &failWriterConn{Conn: rawConn}
				return fw1, nil
			},
		},
	}

	conn1Ctx, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	conn1, _, err := websocket.Dial(conn1Ctx, client.normalizedURL, &websocket.DialOptions{
		HTTPClient: dialClient1,
	})
	if err != nil {
		t.Fatalf("Dial connection 1 failed: %v", err)
	}
	defer conn1.CloseNow()

	// Arm write failure on connection 1
	fw1.failWrite.Store(true)

	// Run handleConnection for connection 1
	conn1Done := make(chan struct{})
	go func() {
		client.handleConnection(conn1Ctx, conn1)
		close(conn1Done)
	}()

	// Verify connection 1 terminated via heartbeat write error handler
	select {
	case <-conn1Done:
		// Connection 1 terminated cleanly via write error handler
	case <-time.After(1 * time.Second):
		t.Fatal("connection 1 did not terminate after heartbeat write failure")
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "[SPOOLER] Heartbeat write failed") {
		t.Fatalf("expected '[SPOOLER] Heartbeat write failed' in log, got:\n%s", logOutput)
	}
}

func TestClient_PingWatchdog_CleanShutdown(t *testing.T) {
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.CloseRead(r.Context())
		<-r.Context().Done()
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_clean_shutdown.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:          wsURL,
		KioskID:           "kiosk-clean-shutdown-test",
		KioskSecret:       "clean-shutdown-token",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       t.TempDir(),
		IdleQRContent:     "https://example.com/qr",
		ScreenSink:        noopSink,
		PingInterval:      20 * time.Millisecond,
		PingTimeout:       20 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		MinBackoff:        10 * time.Millisecond,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	// Let ping watchdog and heartbeat run for several iterations
	time.Sleep(100 * time.Millisecond)

	// Cancel root context
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("client.Run returned non-nil error on clean shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("client.Run did not exit within 1 second on shutdown")
	}
}

func TestClient_PingWatchdog_NotBlockedBySendMu(t *testing.T) {
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.CloseRead(r.Context())
		<-r.Context().Done()
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_sendmu.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-sendmu-test",
		KioskSecret:   "token-test",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   t.TempDir(),
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    noopSink,
		PingInterval:  20 * time.Millisecond,
		PingTimeout:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	connCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ptc *pingTrackingConn
	dialer := &net.Dialer{}
	dialClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				rawConn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				ptc = &pingTrackingConn{Conn: rawConn}
				return ptc, nil
			},
		},
	}

	conn, _, err := websocket.Dial(connCtx, client.normalizedURL, &websocket.DialOptions{
		HTTPClient: dialClient,
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Start reader concurrently as required by coder/websocket Ping
	go func() {
		for {
			var msg wireMessage
			if err := wsjson.Read(connCtx, conn, &msg); err != nil {
				return
			}
		}
	}()

	// Simulate slow writer or health poller holding sendMu
	client.sendMu.Lock()
	defer client.sendMu.Unlock()

	// Exercise the ACTUAL pingWatchdog implementation while sendMu is locked
	watchdogCtx, watchdogCancel := context.WithCancel(connCtx)
	watchdogDone := make(chan struct{})
	go func() {
		client.pingWatchdog(watchdogCtx, cancel, conn)
		close(watchdogDone)
	}()

	// Let the watchdog execute multiple ping iterations while sendMu is held
	time.Sleep(70 * time.Millisecond)

	// If pingWatchdog were blocked by sendMu, canceling watchdogCtx wouldn't unblock it
	// because it would be stuck trying to acquire sendMu.
	watchdogCancel()

	select {
	case <-watchdogDone:
		// Successfully executed and exited without blocking on sendMu
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pingWatchdog hung or was blocked while sendMu was locked")
	}

	// Positive observable assertion that the watchdog actually executed pings
	if pings := ptc.pingCount.Load(); pings < 2 {
		t.Fatalf("expected ping watchdog to execute multiple pings while sendMu was locked, got %d", pings)
	}
}

func TestClient_SafeURL_RemovesEmbeddedUserInfoAndFragment(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "safe_url.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://account:secretpass@example.invalid/ws?token=hidden#fragmentsecret",
		KioskID:       "kiosk-safe-url",
		StateStore:    st,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   t.TempDir(),
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatal(err)
	}
	safe := client.SafeURL()
	if safe != "ws://example.invalid/ws" {
		t.Fatalf("unexpected sanitized URL: %q", safe)
	}
	got := client.sanitizeError(fmt.Errorf("dial rejected: wss://alice:secretpass@example.invalid/ws?token=hidden"))
	for _, secret := range []string{"alice", "secretpass", "hidden", "?"} {
		if strings.Contains(got, secret) {
			t.Fatalf("error still contains %q: %s", secret, got)
		}
	}
}

func TestClient_PingWatchdog_ReconnectWhileSendMuLocked(t *testing.T) {
	var connCount atomic.Int32
	conn1Established := make(chan struct{})
	conn2Established := make(chan struct{})

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		count := connCount.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		if count == 1 {
			// First connection: accept WebSocket upgrade, but do NOT read.
			// Incoming pings will not receive pongs, triggering the ping watchdog.
			select {
			case <-conn1Established:
			default:
				close(conn1Established)
			}
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		} else {
			// Second connection (reconnect): signal establishment and read normally.
			select {
			case <-conn2Established:
			default:
				close(conn2Established)
			}
			_ = c.CloseRead(r.Context())
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_watchdog_sendmu_reconnect.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:          wsURL,
		KioskID:           "kiosk-watchdog-sendmu-test",
		KioskSecret:       "watchdog-sendmu-token",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       t.TempDir(),
		IdleQRContent:     "https://example.com/qr",
		ScreenSink:        noopSink,
		PingInterval:      30 * time.Millisecond,
		PingTimeout:       30 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(ctx)
	}()

	select {
	case <-conn1Established:
	case <-time.After(1 * time.Second):
		t.Fatal("connection 1 was not established")
	}

	// Hold sendMu to simulate a stalled print job or slow writer
	client.sendMu.Lock()

	// Prove client reconnects while sendMu is STILL held
	select {
	case <-conn2Established:
		// Reconnection succeeded while sendMu was locked
	case <-time.After(1500 * time.Millisecond):
		client.sendMu.Unlock()
		t.Fatal("client failed to reconnect while sendMu was locked (connection teardown deadlock)")
	}

	// Release mutex safely
	client.sendMu.Unlock()

	// Prove clean shutdown
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on clean shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("client.Run did not terminate promptly after context cancel")
	}
}

func TestClient_IncompleteJobSync_ReconnectWhileSendMuLocked(t *testing.T) {
	var connCount atomic.Int32
	conn1Established := make(chan struct{})
	conn2Established := make(chan struct{})

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		count := connCount.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		if count == 1 {
			// First connection: accept WebSocket upgrade, but do NOT read.
			// Incoming pings will not receive pongs, triggering the ping watchdog.
			select {
			case <-conn1Established:
			default:
				close(conn1Established)
			}
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		} else {
			// Second connection (reconnect): signal establishment and read normally.
			select {
			case <-conn2Established:
			default:
				close(conn2Established)
			}
			_ = c.CloseRead(r.Context())
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
	}))
	defer wsServer.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "test_incomplete_sync_sendmu.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	// Persist a real PAUSED_ERROR job in state store
	pausedCmd := contracts.PrintJobCommand{
		JobID:          "paused-job-test-001",
		KioskID:        "kiosk-paused-sync-test",
		DownloadURL:    "https://example.com/test.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     3,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), pausedCmd, contracts.JobStatusPausedError); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:          wsURL,
		KioskID:           "kiosk-paused-sync-test",
		KioskSecret:       "paused-sync-token",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       t.TempDir(),
		IdleQRContent:     "https://example.com/qr",
		ScreenSink:        noopSink,
		PingInterval:      30 * time.Millisecond,
		PingTimeout:       30 * time.Millisecond,
		HeartbeatInterval: 1 * time.Second,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Hold sendMu before connection-startup synchronization worker attempts its send
	client.sendMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(ctx)
	}()

	// Wait for connection 1 to connect to the server
	select {
	case <-conn1Established:
	case <-time.After(1 * time.Second):
		client.sendMu.Unlock()
		t.Fatal("connection 1 was not established")
	}

	// Prove client reconnects while sendMu is STILL held
	select {
	case <-conn2Established:
		// Reconnection succeeded while sendMu was locked!
	case <-time.After(1500 * time.Millisecond):
		client.sendMu.Unlock()
		t.Fatal("client failed to reconnect while sendMu was locked (incomplete job sync deadlock)")
	}

	// Release mutex safely
	client.sendMu.Unlock()

	// Prove clean shutdown
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on clean shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("client.Run did not terminate promptly after context cancel")
	}
}
