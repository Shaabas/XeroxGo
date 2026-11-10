package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func TestClientCloudRecovery_Success(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cloud_rec.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	var cupsPrinted atomic.Bool
	var printedCmd contracts.PrintJobCommand
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printedCmd = cmd
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		cupsPrinted.Store(true)
		return nil
	}

	// Mock cloud HTTP server
	var cloudServer *httptest.Server
	cloudServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/recover"):
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["output_confirmed"] != true {
				http.Error(w, `{"error":"output uncertain"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"recovery_id": "rec_123",
				"sequence_id": 1,
				"job_id":      "job_cont_01",
				"command": contracts.PrintJobCommand{
					JobID:          "job_cont_01",
					KioskID:        "kiosk-test",
					DisplayCode:    "9999",
					DownloadURL:    cloudServer.URL + "/cont.pdf",
					SHA256Checksum: pdfChecksum,
					TotalPages:     1,
					TotalSheets:    1,
					Copies:         1,
					Duplex:         contracts.DuplexSimplex,
					StartFromPage:  1,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case strings.HasSuffix(path, "/cont.pdf"):
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdfContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloudServer.Close()

	wsURL := strings.Replace(cloudServer.URL, "http://", "ws://", 1) + "/ws"

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-test",
		KioskSecret:         "secret",
		StateStore:          store,
		CUPSQueue:           "test-cups",
		MockCUPSPrint:       mockCUPS,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		HTTPClient:          cloudServer.Client(),
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Request cloud recovery
	cmd, err := client.RequestCloudRecovery(ctx, "job_orig", 1, true, "jam cleared")
	if err != nil {
		t.Fatalf("RequestCloudRecovery failed: %v", err)
	}
	if cmd == nil || cmd.JobID != "job_cont_01" {
		t.Fatalf("unexpected command returned: %+v", cmd)
	}

	// Wait for continuation job to be completed by worker
	deadline := time.Now().Add(5 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil && cupsPrinted.Load() {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !completed {
		t.Fatal("continuation job was not completed")
	}

	// Verify the command executed had StartFromPage=1 (zero Pi slicing)
	if printedCmd.StartFromPage != 1 {
		t.Fatalf("expected printed command to have StartFromPage=1, got %d", printedCmd.StartFromPage)
	}
}

func TestClientCloudRecovery_UncertainOutputBlocked(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cloud_rec_unc.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["output_confirmed"] != true {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"physical output is uncertain; explicit reconciliation required"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloudServer.Close()

	wsURL := strings.Replace(cloudServer.URL, "http://", "ws://", 1) + "/ws"

	client, err := NewClient(ClientConfig{
		CloudURL:            wsURL,
		KioskID:             "kiosk-test",
		StateStore:          store,
		CUPSQueue:           "test-cups",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		HTTPClient:          cloudServer.Client(),
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Call with outputConfirmed=false (Requirement 10)
	_, err = client.RequestCloudRecovery(context.Background(), "job_test", 1, false, "ambiguous paper jam")
	if err == nil {
		t.Fatal("expected RequestCloudRecovery to fail when outputConfirmed is false")
	}
	if !strings.Contains(err.Error(), "physical output is uncertain") {
		t.Fatalf("expected error message to cite uncertain physical output, got: %v", err)
	}
}

func TestClientCloudRecovery_StartupInterruptionMarkedPausedError(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cloud_rec_startup.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Simulate a job that was in the middle of PRINTING when daemon crashed/restarted
	job := contracts.PrintJobCommand{
		JobID:          "job_crash_01",
		KioskID:        "kiosk-test",
		DisplayCode:    "4444",
		DownloadURL:    "http://example.com/doc.pdf",
		SHA256Checksum: "dummy",
		TotalPages:     6,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), job, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	evt := contracts.JobProgressEvent{
		JobID:         job.JobID,
		KioskID:       job.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1, // mid-job telemetry
		Timestamp:     time.Now().UTC(),
	}
	if err := store.UpdateProgress(context.Background(), evt); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-test",
		StateStore:          store,
		CUPSQueue:           "test-cups",
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Wait for startup recovery check to transition job to PAUSED_ERROR
	deadline := time.Now().Add(3 * time.Second)
	var inc *state.StoredJob
	for time.Now().Before(deadline) {
		inc, err = store.GetIncompleteJob(ctx)
		if err == nil && inc != nil && inc.Status == contracts.JobStatusPausedError {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if inc == nil {
		t.Fatal("expected job to remain in state store as incomplete")
	}
	if inc.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR on uncertain restart, got %s", inc.Status)
	}
	if !strings.Contains(inc.ErrorReason, "daemon restart; physical output uncertain") {
		t.Fatalf("expected error reason to cite restart uncertainty, got %s", inc.ErrorReason)
	}
}

func TestClientCloudRecovery_ZeroSheetsInterruptionCanRetry(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cloud_rec_zero.db")
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
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	var cupsPrinted atomic.Bool
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		cupsPrinted.Store(true)
		return nil
	}

	// Job interrupted at SPOOLING before any sheet printed
	job := contracts.PrintJobCommand{
		JobID:          "job_zero_01",
		KioskID:        "kiosk-test",
		DisplayCode:    "1111",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := store.SaveJob(context.Background(), job, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:            "ws://127.0.0.1:8080/ws",
		KioskID:             "kiosk-test",
		StateStore:          store,
		CUPSQueue:           "test-cups",
		MockCUPSPrint:       mockCUPS,
		DownloadDir:         tmpDir,
		IdleQRContent:       "https://example.com/qr",
		ScreenSink:          noopSink,
		EnableCloudRecovery: true,
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
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil && cupsPrinted.Load() {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !completed {
		t.Fatal("zero-sheet interrupted job was not recovered to COMPLETED")
	}
}
