package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"io"
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
			CloudURL:       "ws://127.0.0.1:8080/ws",
			KioskID:        "kiosk-j",
			StateStore:     store,
			PrinterAddr:    printerLn.Addr().String(),
			DownloadDir:    tmpDir,
			IdleQRContent:  "https://example.com/kiosk-j",
			ScreenSink:     noopSink,
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

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
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

		client.handleIncomingJob(cmd)

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
		receivedProgress := make(chan wireMessage, 1)
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send job command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

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
		case ack := <-receivedAck:
			if ack.Progress == nil || ack.Progress.JobID != cmd.JobID {
				t.Fatalf("unexpected job_ack payload: %+v", ack)
			}
			if ack.Progress.Status != contracts.JobStatusSpooling {
				t.Fatalf("expected job_ack status SPOOLING, got %s", ack.Progress.Status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for job_ack")
		}

		select {
		case prog := <-receivedProgress:
			if prog.Progress == nil || prog.Progress.JobID != cmd.JobID || prog.Progress.Status != contracts.JobStatusSpooling {
				t.Fatalf("unexpected progress payload: %+v", prog)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for SPOOLING progress")
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
		var ackCount atomic.Int32
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			// Send first command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			<-sendDup
			// Send duplicate command
			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &cmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "job_ack" {
					ackCount.Add(1)
				}
			}
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
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()

			_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &conflictingCmd})

			for {
				var msg wireMessage
				if err := wsjson.Read(r.Context(), c, &msg); err != nil {
					break
				}
				if msg.Type == "job_ack" {
					gotAck.Store(true)
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

		// Wait briefly with polling for potential messages
		time.Sleep(200 * time.Millisecond)

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

	// Scenario G: Queue admission failure
	t.Run("ScenarioG_QueueAdmissionFailure", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "kiosk_g.db")
		store, err := state.Open(dbPath)
		if err != nil {
			t.Fatalf("failed to open store: %v", err)
		}
		defer store.Close()

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
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

		client.handleIncomingJob(cmd)

		// Assert: reservation released
		client.activeMu.Lock()
		reserved := client.activeJobs[cmd.JobID]
		client.activeMu.Unlock()
		if reserved {
			t.Errorf("expected in-memory reservation for %s to be released on queue full", cmd.JobID)
		}

		// Assert: durable record preserved in store for recovery
		ctx := context.Background()
		saved, err := store.GetJob(ctx, cmd.JobID)
		if err != nil || saved == nil {
			t.Fatalf("expected saved record in store for recovery, got: %v, err: %v", saved, err)
		}
		if saved.Status != contracts.JobStatusSpooling {
			t.Errorf("expected saved status SPOOLING, got %s", saved.Status)
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
}
