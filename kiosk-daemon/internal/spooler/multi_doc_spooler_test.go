package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

type mockSoundPlayer struct {
	mu        sync.Mutex
	completed int
}

func (m *mockSoundPlayer) PlayCompletion() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed++
}

func (m *mockSoundPlayer) CompletionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completed
}

func (m *mockSoundPlayer) PlayKeyPress() {}

func (m *mockSoundPlayer) WAVBytes() []byte { return nil }

// TestMultiDoc_OutOfOrderArrivalAndSequencing verifies that when documents for an order arrive
// out-of-order over the wire (doc 2, doc 0, doc 1), none are queued until all 3 are durably persisted,
// and once all documents arrive, they are queued strictly in DocumentIndex order (0, 1, 2).
func TestMultiDoc_OutOfOrderArrivalAndSequencing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "out_of_order.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var printedJobs []string
	var printMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printMu.Lock()
		printedJobs = append(printedJobs, cmd.JobID)
		printMu.Unlock()
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-multi-1",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_out_of_order_123"
	makeCmd := func(idx int, jobID string) contracts.PrintJobCommand {
		return contracts.PrintJobCommand{
			JobID:          jobID,
			OrderID:        orderID,
			DocumentIndex:  idx,
			TotalDocuments: 3,
			KioskID:        "kiosk-multi-1",
			DisplayCode:    "8888",
			DownloadURL:    fileServer.URL + "/" + jobID + ".pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
	}

	cmd2 := makeCmd(2, "job_doc_2")
	cmd0 := makeCmd(0, "job_doc_0")
	cmd1 := makeCmd(1, "job_doc_1")

	// 1. Deliver Document 2 first
	client.handleIncomingJob(cmd2)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs in queue after doc 2 arrival, got %d", len(client.jobQueue))
	}

	// 2. Deliver Document 0 second
	client.handleIncomingJob(cmd0)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs in queue after doc 0 arrival, got %d", len(client.jobQueue))
	}

	// 3. Deliver Document 1 third (completes the order)
	client.handleIncomingJob(cmd1)

	// All 3 should now be queued
	if len(client.jobQueue) != 3 {
		t.Fatalf("expected 3 jobs in queue after all docs arrived, got %d", len(client.jobQueue))
	}

	// Verify the queue order is strictly 0, 1, 2
	first := <-client.jobQueue
	second := <-client.jobQueue
	third := <-client.jobQueue

	if first.DocumentIndex != 0 || first.JobID != "job_doc_0" {
		t.Fatalf("expected first queued job to be doc 0, got idx=%d job=%s", first.DocumentIndex, first.JobID)
	}
	if second.DocumentIndex != 1 || second.JobID != "job_doc_1" {
		t.Fatalf("expected second queued job to be doc 1, got idx=%d job=%s", second.DocumentIndex, second.JobID)
	}
	if third.DocumentIndex != 2 || third.JobID != "job_doc_2" {
		t.Fatalf("expected third queued job to be doc 2, got idx=%d job=%s", third.DocumentIndex, third.JobID)
	}
}

// TestMultiDoc_SiblingConsistencyValidation verifies that incoming jobs with conflicting
// metadata within the same order are rejected.
func TestMultiDoc_SiblingConsistencyValidation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "consistency.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:9090/ws",
		KioskID:       "kiosk-multi-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		CUPSQueue:     "MockQueue",
		ScreenSink:    func(*image.RGBA) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	validChecksum := strings.Repeat("a", 64)
	baseCmd := contracts.PrintJobCommand{
		JobID:          "job_sib_0",
		OrderID:        "ord_sibling_test",
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-multi-test",
		DisplayCode:    "5555",
		DownloadURL:    "http://127.0.0.1:9090/doc0.pdf",
		SHA256Checksum: validChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Persist doc 0
	client.handleIncomingJob(baseCmd)
	stored, err := store.GetJob(context.Background(), "job_sib_0")
	if err != nil || stored == nil {
		t.Fatalf("expected doc 0 to be stored, got err: %v", err)
	}

	// 1. Conflicting TotalDocuments (expected 2, got 3)
	badTotalDocs := baseCmd
	badTotalDocs.JobID = "job_sib_bad_total"
	badTotalDocs.DocumentIndex = 1
	badTotalDocs.TotalDocuments = 3
	client.handleIncomingJob(badTotalDocs)
	if j, _ := store.GetJob(context.Background(), "job_sib_bad_total"); j != nil {
		t.Fatalf("expected job with conflicting TotalDocuments to be rejected")
	}

	// 2. Conflicting DisplayCode
	badDisplayCode := baseCmd
	badDisplayCode.JobID = "job_sib_bad_disp"
	badDisplayCode.DocumentIndex = 1
	badDisplayCode.DisplayCode = "9999"
	client.handleIncomingJob(badDisplayCode)
	if j, _ := store.GetJob(context.Background(), "job_sib_bad_disp"); j != nil {
		t.Fatalf("expected job with conflicting DisplayCode to be rejected")
	}

	// 3. Conflicting JobID for already assigned DocumentIndex 0
	badIndexCollision := baseCmd
	badIndexCollision.JobID = "job_sib_collision"
	badIndexCollision.DocumentIndex = 0
	client.handleIncomingJob(badIndexCollision)
	if j, _ := store.GetJob(context.Background(), "job_sib_collision"); j != nil {
		t.Fatalf("expected job with duplicate DocumentIndex but different JobID to be rejected")
	}
}

// TestMultiDoc_DeduplicationWithoutReprint verifies that duplicate deliveries of already admitted,
// submitted, or completed jobs are acknowledged without re-queuing or double printing.
func TestMultiDoc_DeduplicationWithoutReprint(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "dedup.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:9090/ws",
		KioskID:       "kiosk-multi-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		CUPSQueue:     "MockQueue",
		ScreenSink:    func(*image.RGBA) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	validChecksum := strings.Repeat("b", 64)
	cmd := contracts.PrintJobCommand{
		JobID:          "job_dedup_01",
		OrderID:        "ord_dedup_test",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-multi-test",
		DisplayCode:    "1234",
		DownloadURL:    "http://127.0.0.1:9090/doc.pdf",
		SHA256Checksum: validChecksum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Initial delivery
	client.handleIncomingJob(cmd)
	if len(client.jobQueue) != 1 {
		t.Fatalf("expected 1 job queued, got %d", len(client.jobQueue))
	}
	<-client.jobQueue // drain

	// Redundant delivery while stored
	client.handleIncomingJob(cmd)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs queued on redundant delivery, got %d", len(client.jobQueue))
	}

	// Conflicting duplicate delivery (different copies) -> rejected
	conflictingCmd := cmd
	conflictingCmd.Copies = 5
	client.handleIncomingJob(conflictingCmd)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs queued on conflicting duplicate delivery, got %d", len(client.jobQueue))
	}
}

// TestMultiDoc_SingleCopyReplicationViaCUPS verifies that copies are passed to CUPS exactly once
// via cmd.Copies and not duplicated prior to CUPS submission.
func TestMultiDoc_SingleCopyReplicationViaCUPS(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "copies.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var receivedCmd contracts.PrintJobCommand
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		receivedCmd = cmd
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-copies-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job_copies_01",
		OrderID:        "ord_copies_test",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-copies-test",
		DisplayCode:    "7777",
		DownloadURL:    fileServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     2,
		TotalSheets:    6, // 2 pages * 3 copies = 6 sheets
		Copies:         3,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	client.processJob(ctx, cmd)

	if receivedCmd.Copies != 3 {
		t.Fatalf("expected CUPS mock to receive Copies=3, got %d", receivedCmd.Copies)
	}
}

// TestMultiDoc_HardwareFaultPausesOrder verifies that when document 0 experiences a hardware fault,
// the order is paused and document 1 is NOT submitted to CUPS.
func TestMultiDoc_HardwareFaultPausesOrder(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "fault_pause.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var cupsSubmitted []string
	var cupsMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsMu.Lock()
		cupsSubmitted = append(cupsSubmitted, cmd.JobID)
		cupsMu.Unlock()
		if cmd.DocumentIndex == 0 {
			return errors.New("paper feed jam detected in printer")
		}
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-fault-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_fault_123"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_fault_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-fault-test",
		DisplayCode:    "3333",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_fault_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-fault-test",
		DisplayCode:    "3333",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Deliver both documents so order is admitted
	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Process doc 0 -> should encounter jam and enter PAUSED_ERROR
	doc0 := <-client.jobQueue
	client.processJob(ctx, doc0)

	stored0, err := store.GetJob(ctx, "job_fault_0")
	if err != nil || stored0.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected doc 0 to be PAUSED_ERROR, got status=%s err=%v", stored0.Status, err)
	}

	// Now try to process doc 1 -> must NOT submit to CUPS due to sibling fault
	doc1 := <-client.jobQueue
	client.processJob(ctx, doc1)

	cupsMu.Lock()
	defer cupsMu.Unlock()
	if len(cupsSubmitted) != 1 || cupsSubmitted[0] != "job_fault_0" {
		t.Fatalf("expected only doc 0 to have been submitted to CUPS, got %v", cupsSubmitted)
	}

	stored1, err := store.GetJob(ctx, "job_fault_1")
	if err != nil || stored1.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected doc 1 to be PAUSED_ERROR without submitting, got status=%s", stored1.Status)
	}
}

// TestMultiDoc_OrderLevelPaperCheck verifies that before document 0 begins printing,
// the client verifies that the paper tray has enough sheets for the ENTIRE order.
func TestMultiDoc_OrderLevelPaperCheck(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "paper_check.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// Set paper tray supply to 3 sheets
	_ = store.ResetPaperTray(ctx, 3)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var cupsCalled atomic.Bool
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsCalled.Store(true)
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-paper-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_paper_shortage"
	// Order needs: doc 0 (2 sheets) + doc 1 (2 sheets) = 4 sheets. Tray only has 3 sheets.
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_paper_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-paper-test",
		DisplayCode:    "4444",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_paper_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-paper-test",
		DisplayCode:    "4444",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	doc0 := <-client.jobQueue
	client.processJob(ctx, doc0)

	if cupsCalled.Load() {
		t.Fatalf("CUPS must NOT be called when order paper supply is insufficient")
	}

	stored0, err := store.GetJob(ctx, "job_paper_0")
	if err != nil || stored0.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected doc 0 to be PAUSED_ERROR due to insufficient paper, got %v", stored0)
	}
	if !strings.Contains(stored0.ErrorReason, "paper tray supply insufficient") {
		t.Fatalf("expected reason to contain 'paper tray supply insufficient', got: %s", stored0.ErrorReason)
	}
}

// TestMultiDoc_StartupRecoveryUnsubmitted verifies that on daemon restart, an unprinted multi-document
// order is recovered from SQLite and executed in document index order 0, 1.
func TestMultiDoc_StartupRecoveryUnsubmitted(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "recovery_unprinted.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var recoveredOrder []string
	var recMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		recMu.Lock()
		recoveredOrder = append(recoveredOrder, cmd.JobID)
		recMu.Unlock()
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	orderID := "ord_recov_001"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_recov_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-recov-test",
		DisplayCode:    "1111",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_recov_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-recov-test",
		DisplayCode:    "1111",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Persist both as unsubmitted SPOOLING jobs
	if err := store.SaveJob(ctx, cmd0, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("failed to save cmd0: %v", err)
	}
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("failed to save cmd1: %v", err)
	}

	// Start a fresh client
	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-recov-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
		HTTPClient:    fileServer.Client(),
		ScreenSink:    func(*image.RGBA) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for both jobs to execute in order
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recMu.Lock()
		count := len(recoveredOrder)
		recMu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	runCancel()

	recMu.Lock()
	defer recMu.Unlock()
	if len(recoveredOrder) != 2 {
		t.Fatalf("expected 2 jobs recovered and printed, got %v", recoveredOrder)
	}
	if recoveredOrder[0] != "job_recov_0" || recoveredOrder[1] != "job_recov_1" {
		t.Fatalf("expected execution order [job_recov_0, job_recov_1], got %v", recoveredOrder)
	}
}

// TestMultiDoc_OrderCompletionSemantics verifies that intermediate documents display intermediate
// completion and do NOT trigger completion chime or idle reset, while the final document triggers
// completion chime, ModeCompleted screen, and idle reset.
func TestMultiDoc_OrderCompletionSemantics(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "order_completion.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	sound := &mockSoundPlayer{}

	client, err := NewClient(ClientConfig{
		CloudURL:                 strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:                  "kiosk-comp-test",
		StateStore:               store,
		DownloadDir:              tmpDir,
		IdleQRContent:            "https://example.com/qr",
		ScreenSink:               func(*image.RGBA) error { return nil },
		HTTPClient:               fileServer.Client(),
		CUPSQueue:                "MockQueue",
		MockCUPSPrint:            mockCUPS,
		SoundPlayer:              sound,
		CompletedDisplayDuration: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_comp_semantics"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_comp_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-comp-test",
		DisplayCode:    "2222",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_comp_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-comp-test",
		DisplayCode:    "2222",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Process doc 0
	doc0 := <-client.jobQueue
	client.processJob(ctx, doc0)

	// After doc 0 finishes, sound should NOT have played, and mode should not be ModeCompleted
	if sound.CompletionCount() != 0 {
		t.Fatalf("sound played after intermediate doc 0 completion: %d", sound.CompletionCount())
	}
	client.currentModeMu.Lock()
	mode0 := client.currentMode
	client.currentModeMu.Unlock()
	if mode0 == ui_ipc.ModeCompleted {
		t.Fatalf("expected mode != ModeCompleted after doc 0, got %s", mode0)
	}

	// Process doc 1
	doc1 := <-client.jobQueue
	client.processJob(ctx, doc1)

	// After doc 1 finishes, all 2 docs are completed -> sound must play exactly once
	if sound.CompletionCount() != 1 {
		t.Fatalf("expected sound played exactly once for order completion, got %d", sound.CompletionCount())
	}
	client.currentModeMu.Lock()
	mode1 := client.currentMode
	client.currentModeMu.Unlock()
	if mode1 != ui_ipc.ModeCompleted {
		t.Fatalf("expected mode == ModeCompleted after doc 1, got %s", mode1)
	}
}

func TestMultiDoc_DuplicateDispatchAfterRestart(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "dup_restart.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	orderID := "ord_restart_dup_001"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_dup_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-dup-restart",
		DisplayCode:    "1111",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_dup_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-dup-restart",
		DisplayCode:    "1111",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Doc 0 was completed before daemon stopped/restarted
	if err := store.SaveJob(ctx, cmd0, contracts.JobStatusCompleted); err != nil {
		t.Fatalf("SaveJob doc 0 failed: %v", err)
	}
	_ = store.RecordSheetProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd0.JobID,
		KioskID:       cmd0.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	})

	// Doc 1 was saved as Spooling before daemon stopped/restarted (unsubmitted)
	if err := store.SaveJob(ctx, cmd1, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob doc 1 failed: %v", err)
	}

	var printedJobs []string
	var printMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printMu.Lock()
		printedJobs = append(printedJobs, cmd.JobID)
		printMu.Unlock()
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-dup-restart",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Start daemon (runs startupRecovery)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = client.Run(runCtx)
	}()

	// Simulate cloud re-delivering duplicate commands for both Doc 0 and Doc 1
	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Wait for processing and order completion to settle
	deadline := time.Now().Add(5 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		if c, err := store.IsOrderCompleted(ctx, orderID); err == nil && c {
			completed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !completed {
		t.Fatalf("expected order %s to be completed", orderID)
	}

	printMu.Lock()
	defer printMu.Unlock()

	// Verify doc 0 was NEVER re-printed, and doc 1 was printed exactly once
	if len(printedJobs) != 1 {
		t.Fatalf("expected exactly 1 job printed, got %d: %v", len(printedJobs), printedJobs)
	}
	if printedJobs[0] != "job_dup_1" {
		t.Fatalf("expected job_dup_1 printed, got %s", printedJobs[0])
	}
}

func TestMultiDoc_DuplicateDispatchOnReconnection(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "dup_reconnect.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	var printedJobs []string
	var printMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printMu.Lock()
		printedJobs = append(printedJobs, cmd.JobID)
		printMu.Unlock()
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-reconnect-test",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_reconnect_001"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_recon_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-reconnect-test",
		DisplayCode:    "3333",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_recon_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-reconnect-test",
		DisplayCode:    "3333",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = client.Run(runCtx)
	}()

	// 1st delivery
	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Reconnection: duplicate delivery arrives while processing
	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Allow jobs to finish processing and order to complete
	deadline := time.Now().Add(5 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		if c, err := store.IsOrderCompleted(ctx, orderID); err == nil && c {
			completed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !completed {
		t.Fatalf("expected order %s to be completed", orderID)
	}

	printMu.Lock()
	defer printMu.Unlock()

	// Assert exactly 2 prints executed: doc 0 and doc 1
	if len(printedJobs) != 2 {
		t.Fatalf("expected exactly 2 jobs printed across reconnection, got %d: %v", len(printedJobs), printedJobs)
	}
	if printedJobs[0] != "job_recon_0" || printedJobs[1] != "job_recon_1" {
		t.Fatalf("unexpected print sequence: %v", printedJobs)
	}
}

func TestMultiDoc_PartialCompletionAndRepeatedAdmission(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "repeated_admission.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-repeat-admit",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_repeat_admit"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_repeat_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-repeat-admit",
		DisplayCode:    "4444",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_repeat_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-repeat-admit",
		DisplayCode:    "4444",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	client.handleIncomingJob(cmd0)
	client.handleIncomingJob(cmd1)

	// Dequeue doc 0 and process it to completion
	d0 := <-client.jobQueue
	client.processJob(ctx, d0)

	// Clean up doc 0 from active jobs as jobWorker would
	client.activeMu.Lock()
	delete(client.activeJobs, d0.JobID)
	client.activeMu.Unlock()

	// Repeatedly call checkAndAdmitOrder
	client.checkAndAdmitOrder(orderID, 2)
	client.checkAndAdmitOrder(orderID, 2)
	client.checkAndAdmitOrder(orderID, 2)

	// Assert: jobQueue contains ONLY doc 1 (queued originally), doc 0 was NOT requeued!
	if len(client.jobQueue) != 1 {
		t.Fatalf("expected exactly 1 job in queue (doc 1), got %d", len(client.jobQueue))
	}
	d1 := <-client.jobQueue
	if d1.JobID != "job_repeat_1" {
		t.Fatalf("expected job_repeat_1, got %s", d1.JobID)
	}

	// Process doc 1 to completion
	client.processJob(ctx, d1)
	client.activeMu.Lock()
	delete(client.activeJobs, d1.JobID)
	client.activeMu.Unlock()

	// Calling checkAndAdmitOrder after all docs complete must not queue anything
	client.checkAndAdmitOrder(orderID, 2)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected empty jobQueue after all docs complete, got %d", len(client.jobQueue))
	}
}

func TestMultiDoc_PreSubmissionSafety_UncertainSubmission(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "uncertain_sub.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCalled := false
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		mockCalled = true
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-uncertain",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_uncertain_sub"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_uncert_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-uncertain",
		DisplayCode:    "5555",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_uncert_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-uncertain",
		DisplayCode:    "5555",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = store.SaveJob(ctx, cmd0, contracts.JobStatusSpooling)
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusSpooling)

	// Simulate uncertain state: SubmittedToPrinter is set, but no CUPS job ID
	_ = store.MarkJobSubmitting(ctx, cmd0.JobID)

	// Attempt to process doc 0
	client.processJob(ctx, cmd0)

	// MockCUPSPrint must NOT have been called
	if mockCalled {
		t.Fatalf("MockCUPSPrint was invoked for uncertain job %s; must be rejected!", cmd0.JobID)
	}

	// Stored state must be paused in PAUSED_ERROR requiring manual reconciliation
	job0, _ := store.GetJob(ctx, cmd0.JobID)
	if job0.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job 0 status PAUSED_ERROR, got %s", job0.Status)
	}
	if !strings.Contains(job0.ErrorReason, "manual reconciliation required") {
		t.Fatalf("expected ErrorReason to contain 'manual reconciliation required', got %q", job0.ErrorReason)
	}

	// Pre-submission check on doc 1 must also halt because sibling doc 0 is PAUSED_ERROR
	client.processJob(ctx, cmd1)
	if mockCalled {
		t.Fatalf("MockCUPSPrint was invoked for doc 1 despite sibling doc 0 error!")
	}
	job1, _ := store.GetJob(ctx, cmd1.JobID)
	if job1.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job 1 status PAUSED_ERROR due to sibling error, got %s", job1.Status)
	}
}

func TestMultiDoc_PreSubmissionSafety_ExistingCUPSJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "existing_cups.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	lpCalled := false
	lpstatCalls := 0
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpCalled = true
			return []byte("request id is TestQueue-99 (1 file(s))\n"), nil
		}
		if name == "lpstat" {
			lpstatCalls++
			if len(args) > 1 && args[1] == "completed" {
				return []byte("TestQueue-99 username 1024 Tue Nov 3 20:30:00 2026\n\tAlerts: job-completed-successfully\n\tqueued for TestQueue\n"), nil
			}
			return []byte(""), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-existing-cups",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "TestQueue",
		CUPSRunner:    runner,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job_existing_cups",
		OrderID:        "ord_existing_cups",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-existing-cups",
		DisplayCode:    "6666",
		DownloadURL:    fileServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Save job and record existing CUPS job submission
	_ = store.SaveJob(ctx, cmd, contracts.JobStatusPrinting)
	_ = store.RecordJobSubmission(ctx, cmd.JobID, "TestQueue-99")

	// Call processJob on this existing CUPS job
	client.processJob(ctx, cmd)

	// lp must NOT have been called!
	if lpCalled {
		t.Fatalf("lp submission was called second time for existing CUPS job %s!", cmd.JobID)
	}
	// lpstat must have been called to resume monitoring
	if lpstatCalls == 0 {
		t.Fatalf("expected lpstat monitoring to be called, got 0 calls")
	}
}

func TestMultiDoc_ColorCapabilitiesEnforcement(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "color_caps.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCalled := false
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		mockCalled = true
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-color-caps",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Explicitly set printer capabilities: Monochrome ONLY (Color is Unsupported)
	client.capsMu.Lock()
	client.capabilities = printer.PrinterCapabilities{
		ColorSupport:      printer.CapabilityUnsupported,
		MonochromeSupport: printer.CapabilitySupported,
	}
	client.capsMu.Unlock()

	colorCmd := contracts.PrintJobCommand{
		JobID:          "job_color_unsupported",
		OrderID:        "ord_color_caps",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-color-caps",
		DisplayCode:    "7777",
		DownloadURL:    fileServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		ColorMode:      "COLOR",
		StartFromPage:  1,
	}

	_ = store.SaveJob(ctx, colorCmd, contracts.JobStatusSpooling)
	client.processJob(ctx, colorCmd)

	if mockCalled {
		t.Fatalf("MockCUPSPrint was invoked for unsupported COLOR job!")
	}
	stored, _ := store.GetJob(ctx, colorCmd.JobID)
	if stored.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR for unsupported color, got %s", stored.Status)
	}
	if !strings.Contains(stored.ErrorReason, "printer does not support requested COLOR printing mode") {
		t.Fatalf("unexpected error reason: %s", stored.ErrorReason)
	}

	// Now test Mono unsupported
	client.capsMu.Lock()
	client.capabilities = printer.PrinterCapabilities{
		ColorSupport:      printer.CapabilitySupported,
		MonochromeSupport: printer.CapabilityUnsupported,
	}
	client.capsMu.Unlock()

	monoCmd := contracts.PrintJobCommand{
		JobID:          "job_mono_unsupported",
		OrderID:        "ord_mono_caps",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-color-caps",
		DisplayCode:    "8888",
		DownloadURL:    fileServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		ColorMode:      "BW",
		StartFromPage:  1,
	}

	mockCalled = false
	_ = store.SaveJob(ctx, monoCmd, contracts.JobStatusSpooling)
	client.processJob(ctx, monoCmd)

	if mockCalled {
		t.Fatalf("MockCUPSPrint was invoked for unsupported BW job!")
	}
	storedMono, _ := store.GetJob(ctx, monoCmd.JobID)
	if storedMono.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status PAUSED_ERROR for unsupported BW, got %s", storedMono.Status)
	}
	if !strings.Contains(storedMono.ErrorReason, "printer does not support requested BW/MONOCHROME printing mode") {
		t.Fatalf("unexpected error reason: %s", storedMono.ErrorReason)
	}
}

func TestMultiDoc_FailClosedOnStoreErrorsAndInconsistency(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "fail_closed.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCalled := false
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		mockCalled = true
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-fail-closed",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	orderID := "ord_inconsistent_seq"
	// Create inconsistent order state: doc 1 is completed, but doc 0 is NOT completed
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job_inconsist_0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-fail-closed",
		DisplayCode:    "9999",
		DownloadURL:    fileServer.URL + "/doc0.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job_inconsist_1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-fail-closed",
		DisplayCode:    "9999",
		DownloadURL:    fileServer.URL + "/doc1.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = store.SaveJob(ctx, cmd0, contracts.JobStatusSpooling)
	_ = store.SaveJob(ctx, cmd1, contracts.JobStatusCompleted)

	// checkAndAdmitOrder must fail closed and refuse to queue because doc 1 is completed while doc 0 is not
	client.checkAndAdmitOrder(orderID, 2)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs queued for inconsistent order state, got %d", len(client.jobQueue))
	}

	// Pre-submission check in processJob must also reject doc 1 if somehow dispatched
	client.processJob(ctx, cmd1)
	if mockCalled {
		t.Fatalf("MockCUPSPrint was invoked for already-completed inconsistent doc 1!")
	}
}

// TestMultiDoc_QueueSaturation_FIFOOrder verifies that when jobQueue is saturated (constrained buffer capacity),
// the single ordered producer queues and feeds documents strictly in DocumentIndex order without race-induced reordering.
func TestMultiDoc_QueueSaturation_FIFOOrder(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "queue_sat.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-sat-1",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Create a constrained jobQueue channel of capacity 2 to test saturation deterministically
	client.jobQueue = make(chan contracts.PrintJobCommand, 2)

	orderID := "ord_sat_123"
	totalDocs := 5
	var cmds []contracts.PrintJobCommand
	for i := 0; i < totalDocs; i++ {
		cmd := contracts.PrintJobCommand{
			JobID:          fmt.Sprintf("job_sat_%d", i),
			OrderID:        orderID,
			DocumentIndex:  i,
			TotalDocuments: totalDocs,
			KioskID:        "kiosk-sat-1",
			DisplayCode:    "5555",
			DownloadURL:    fileServer.URL + fmt.Sprintf("/doc%d.pdf", i),
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = store.SaveJob(ctx, cmd, contracts.JobStatusSpooling)
		cmds = append(cmds, cmd)
	}

	// Admit the order. Since cap is 2 and there are 5 docs, the queue will saturate immediately.
	// The single ordered producer must handle the saturation without reordering.
	client.checkAndAdmitOrder(orderID, totalDocs)

	// Consume all 5 jobs from jobQueue in sequence
	var receivedIndices []int
	for i := 0; i < totalDocs; i++ {
		select {
		case j := <-client.jobQueue:
			receivedIndices = append(receivedIndices, j.DocumentIndex)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for job index %d; received so far: %v", i, receivedIndices)
		}
	}

	// Verify exact sequence 0, 1, 2, 3, 4
	for i := 0; i < totalDocs; i++ {
		if receivedIndices[i] != i {
			t.Fatalf("expected document index %d at position %d, got %d (full sequence: %v)",
				i, i, receivedIndices[i], receivedIndices)
		}
	}
}

// TestMultiDoc_DeferredOrder_Sequencing verifies that deferred orders resume strictly in FIFO document order
// once the kiosk becomes idle, without reordering or skipped documents.
func TestMultiDoc_DeferredOrder_Sequencing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "deferred_seq.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-def-1",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Mark kiosk busy with an active job
	client.activeMu.Lock()
	client.activeJobs["active_blocking_job"] = true
	client.activeMu.Unlock()

	// Order A (3 docs) arrives while busy
	orderA := "ord_deferred_A"
	for i := 0; i < 3; i++ {
		cmd := contracts.PrintJobCommand{
			JobID:          fmt.Sprintf("job_A_%d", i),
			OrderID:        orderA,
			DocumentIndex:  i,
			TotalDocuments: 3,
			KioskID:        "kiosk-def-1",
			DisplayCode:    "1111",
			DownloadURL:    fileServer.URL + fmt.Sprintf("/docA_%d.pdf", i),
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = store.SaveJob(ctx, cmd, contracts.JobStatusSpooling)
	}
	client.checkAndAdmitOrder(orderA, 3)

	// Since kiosk is busy, all 3 jobs must be in deferredJobs, nothing in jobQueue
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs in queue while busy, got %d", len(client.jobQueue))
	}
	client.deferredMu.Lock()
	defCount := len(client.deferredJobs)
	client.deferredMu.Unlock()
	if defCount != 3 {
		t.Fatalf("expected 3 deferred jobs, got %d", defCount)
	}

	// Now clear the active blocking job so kiosk becomes idle
	client.activeMu.Lock()
	delete(client.activeJobs, "active_blocking_job")
	client.activeMu.Unlock()

	// Drain deferred jobs
	client.drainDeferredJobsIfSafe(ctx)

	// Verify all 3 jobs of Order A are queued in strict DocumentIndex order
	for i := 0; i < 3; i++ {
		select {
		case j := <-client.jobQueue:
			if j.DocumentIndex != i || j.JobID != fmt.Sprintf("job_A_%d", i) {
				t.Fatalf("expected Order A doc %d, got idx=%d job=%s", i, j.DocumentIndex, j.JobID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for deferred job %d", i)
		}
	}
}

// TestMultiDoc_StartupRecovery_FIFOOrder verifies that startup recovery resumes unsubmitted jobs in strict
// DocumentIndex order even if the queue buffer is constrained, preserving prefix completions and sequence.
func TestMultiDoc_StartupRecovery_FIFOOrder(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "startup_seq.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_ = store.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	orderID := "ord_startup_order"
	totalDocs := 4
	for i := 0; i < totalDocs; i++ {
		cmd := contracts.PrintJobCommand{
			JobID:          fmt.Sprintf("job_startup_%d", i),
			OrderID:        orderID,
			DocumentIndex:  i,
			TotalDocuments: totalDocs,
			KioskID:        "kiosk-startup-1",
			DisplayCode:    "3333",
			DownloadURL:    fileServer.URL + fmt.Sprintf("/doc%d.pdf", i),
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		// Save doc 0 as COMPLETED, and docs 1, 2, 3 as unsubmitted SPOOLING
		status := contracts.JobStatusSpooling
		if i == 0 {
			status = contracts.JobStatusCompleted
		}
		_ = store.SaveJob(ctx, cmd, status)
	}

	var printedDocs []int
	var printMu sync.Mutex
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		printMu.Lock()
		printedDocs = append(printedDocs, cmd.DocumentIndex)
		printMu.Unlock()
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("printer-state=idle\nprinter-is-accepting-jobs=true\n"), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
		KioskID:       "kiosk-startup-1",
		StateStore:    store,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/qr",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    fileServer.Client(),
		CUPSQueue:     "MockQueue",
		CUPSRunner:    mockRunner,
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Use a constrained jobQueue buffer to verify startup recovery ordering under saturation
	client.jobQueue = make(chan contracts.PrintJobCommand, 1)

	// Trigger startup recovery via client.Run with a cancelable context
	runCtx, runCancel := context.WithCancel(ctx)
	go func() {
		_ = client.Run(runCtx)
	}()
	defer runCancel()

	// Wait for all 3 uncompleted documents (1, 2, 3) to be submitted via MockCUPSPrint in FIFO order
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		printMu.Lock()
		cnt := len(printedDocs)
		printMu.Unlock()
		if cnt >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	printMu.Lock()
	defer printMu.Unlock()
	if len(printedDocs) != 3 {
		t.Fatalf("expected 3 recovered documents to be printed, got %d (%v)", len(printedDocs), printedDocs)
	}
	if printedDocs[0] != 1 || printedDocs[1] != 2 || printedDocs[2] != 3 {
		t.Fatalf("expected FIFO execution order [1, 2, 3], got %v", printedDocs)
	}
}

// TestMultiDoc_StartupRecovery_InjectedSQLiteFailure verifies that when GetJobsByOrderID fails or returns empty,
// startup recovery fails closed: incomplete jobs are never substituted or queued, no automatic physical
// submissions occur, and jobs are transitioned to PAUSED_ERROR reporting the database inconsistency.
func TestMultiDoc_StartupRecovery_InjectedSQLiteFailure(t *testing.T) {
	ctx := context.Background()
	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer fileServer.Close()

	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("printer-state=idle\nprinter-is-accepting-jobs=true\n"), nil
	}

	// Case A: GetJobsByOrderID returns an injected SQLite query error
	t.Run("injected query error fails closed and never submits", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "sqlite_inj_a.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()
		_ = store.ResetPaperTray(ctx, 100)

		var physicalSubmissionCount int64
		mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
			atomic.AddInt64(&physicalSubmissionCount, 1)
			return nil
		}

		orderID := "ord_inj_err_1"
		cmd0 := contracts.PrintJobCommand{
			JobID:          "job_inj_0",
			OrderID:        orderID,
			DocumentIndex:  0,
			TotalDocuments: 2,
			KioskID:        "kiosk-inj-1",
			DisplayCode:    "7777",
			DownloadURL:    fileServer.URL + "/doc0.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		cmd1 := contracts.PrintJobCommand{
			JobID:          "job_inj_1",
			OrderID:        orderID,
			DocumentIndex:  1,
			TotalDocuments: 2,
			KioskID:        "kiosk-inj-1",
			DisplayCode:    "7777",
			DownloadURL:    fileServer.URL + "/doc1.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = store.SaveJob(ctx, cmd0, contracts.JobStatusSpooling)
		_ = store.SaveJob(ctx, cmd1, contracts.JobStatusSpooling)

		// Inject query failure into GetJobsByOrderID
		errInj := errors.New("injected SQLite disk read error")
		store.SetInjectedGetJobsByOrderIDErr(errInj)

		client, err := NewClient(ClientConfig{
			CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
			KioskID:       "kiosk-inj-1",
			StateStore:    store,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/qr",
			ScreenSink:    func(*image.RGBA) error { return nil },
			HTTPClient:    fileServer.Client(),
			CUPSQueue:     "MockQueue",
			CUPSRunner:    mockRunner,
			MockCUPSPrint: mockCUPS,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		runCtx, runCancel := context.WithCancel(ctx)
		go func() {
			_ = client.Run(runCtx)
		}()
		defer runCancel()

		// Poll until jobs are marked PAUSED_ERROR with database inconsistency
		deadline := time.Now().Add(5 * time.Second)
		var j0, j1 *state.StoredJob
		for time.Now().Before(deadline) {
			j0, _ = store.GetJob(ctx, "job_inj_0")
			j1, _ = store.GetJob(ctx, "job_inj_1")
			if j0 != nil && j0.Status == contracts.JobStatusPausedError &&
				j1 != nil && j1.Status == contracts.JobStatusPausedError {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		// 1. Must never substitute incomplete list or queue jobs
		if len(client.jobQueue) != 0 {
			t.Fatalf("expected 0 jobs queued on injected SQLite error, got %d", len(client.jobQueue))
		}

		// 2. Must never trigger automatic physical submission
		if atomic.LoadInt64(&physicalSubmissionCount) != 0 {
			t.Fatalf("injected SQLite error triggered physical submission!")
		}

		// 3. Jobs in store must be transitioned to PAUSED_ERROR reporting database inconsistency
		if j0 == nil || j0.Status != contracts.JobStatusPausedError || !strings.Contains(j0.ErrorReason, "database inconsistency") {
			t.Fatalf("expected job 0 in PAUSED_ERROR with database inconsistency, got status=%v reason=%q", j0.Status, j0.ErrorReason)
		}
		if j1 == nil || j1.Status != contracts.JobStatusPausedError || !strings.Contains(j1.ErrorReason, "database inconsistency") {
			t.Fatalf("expected job 1 in PAUSED_ERROR with database inconsistency, got status=%v reason=%q", j1.Status, j1.ErrorReason)
		}
	})

	// Case B: GetJobsByOrderID returns empty slice (unexpected empty result)
	t.Run("unexpected empty order jobs fails closed and never submits", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, err := state.Open(filepath.Join(tmpDir, "sqlite_inj_b.db"))
		if err != nil {
			t.Fatalf("state.Open failed: %v", err)
		}
		defer store.Close()
		_ = store.ResetPaperTray(ctx, 100)

		var physicalSubmissionCount int64
		mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
			atomic.AddInt64(&physicalSubmissionCount, 1)
			return nil
		}

		orderID := "ord_inj_empty_2"
		cmd := contracts.PrintJobCommand{
			JobID:          "job_inj_empty_0",
			OrderID:        orderID,
			DocumentIndex:  0,
			TotalDocuments: 1,
			KioskID:        "kiosk-inj-2",
			DisplayCode:    "7778",
			DownloadURL:    fileServer.URL + "/doc0.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = store.SaveJob(ctx, cmd, contracts.JobStatusSpooling)

		// Inject empty result into GetJobsByOrderID
		store.SetInjectedGetJobsByOrderIDEmpty(true)

		client, err := NewClient(ClientConfig{
			CloudURL:      strings.Replace(fileServer.URL, "http://", "ws://", 1) + "/ws",
			KioskID:       "kiosk-inj-2",
			StateStore:    store,
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/qr",
			ScreenSink:    func(*image.RGBA) error { return nil },
			HTTPClient:    fileServer.Client(),
			CUPSQueue:     "MockQueue",
			CUPSRunner:    mockRunner,
			MockCUPSPrint: mockCUPS,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		runCtx, runCancel := context.WithCancel(ctx)
		go func() {
			_ = client.Run(runCtx)
		}()
		defer runCancel()

		// Poll until job is marked PAUSED_ERROR with database inconsistency
		deadline := time.Now().Add(5 * time.Second)
		var j *state.StoredJob
		for time.Now().Before(deadline) {
			j, _ = store.GetJob(ctx, "job_inj_empty_0")
			if j != nil && j.Status == contracts.JobStatusPausedError {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		// 1. Must never substitute incomplete list or queue jobs
		if len(client.jobQueue) != 0 {
			t.Fatalf("expected 0 jobs queued on unexpected empty order jobs, got %d", len(client.jobQueue))
		}

		// 2. Must never trigger automatic physical submission
		if atomic.LoadInt64(&physicalSubmissionCount) != 0 {
			t.Fatalf("unexpected empty order jobs triggered physical submission!")
		}

		// 3. Job in store must be marked PAUSED_ERROR reporting database inconsistency
		if j == nil || j.Status != contracts.JobStatusPausedError || !strings.Contains(j.ErrorReason, "database inconsistency") {
			t.Fatalf("expected job in PAUSED_ERROR with database inconsistency, got status=%v reason=%q", j.Status, j.ErrorReason)
		}
	})
}
