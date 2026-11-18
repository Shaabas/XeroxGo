package state

import (
	"context"
	"testing"

	"xeroxgo/pkg/contracts"
)

func TestMultiDocumentStatePersistence(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	cmd := contracts.PrintJobCommand{
		JobID:            "job-k-doc-1",
		OrderID:          "ord-k-100",
		DocumentIndex:    1,
		TotalDocuments:   3,
		OriginalFilename: "financial_report.pdf",
		ColorMode:        "COLOR",
		KioskID:          "kiosk-01",
		DisplayCode:      "1234",
		DownloadURL:      "https://example.invalid/fin.pdf",
		SHA256Checksum:   "sha256fin",
		TotalPages:       5,
		TotalSheets:      3,
		Copies:           2,
		Duplex:           contracts.DuplexLongEdge,
		StartFromPage:    1,
	}

	if err := s.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	retrieved, err := s.GetJob(ctx, "job-k-doc-1")
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if retrieved == nil {
		t.Fatalf("expected job, got nil")
	}

	if retrieved.Command.OrderID != "ord-k-100" {
		t.Errorf("expected OrderID ord-k-100, got %s", retrieved.Command.OrderID)
	}
	if retrieved.Command.DocumentIndex != 1 {
		t.Errorf("expected DocumentIndex 1, got %d", retrieved.Command.DocumentIndex)
	}
	if retrieved.Command.TotalDocuments != 3 {
		t.Errorf("expected TotalDocuments 3, got %d", retrieved.Command.TotalDocuments)
	}
	if retrieved.Command.OriginalFilename != "financial_report.pdf" {
		t.Errorf("expected OriginalFilename 'financial_report.pdf', got %s", retrieved.Command.OriginalFilename)
	}
	if retrieved.Command.ColorMode != "COLOR" {
		t.Errorf("expected ColorMode COLOR, got %s", retrieved.Command.ColorMode)
	}
	if retrieved.Command.Copies != 2 {
		t.Errorf("expected Copies 2, got %d", retrieved.Command.Copies)
	}
	if retrieved.Command.Duplex != contracts.DuplexLongEdge {
		t.Errorf("expected Duplex DUPLEX_LONG_EDGE, got %s", retrieved.Command.Duplex)
	}
}

func TestGetJobsByOrderID(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	orderID := "ord-order-query"

	// Insert 3 documents out of index order
	cmd2 := contracts.PrintJobCommand{
		JobID:          "job-idx-2",
		OrderID:        orderID,
		DocumentIndex:  2,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/2.pdf",
		SHA256Checksum: "sha2",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job-idx-0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0.pdf",
		SHA256Checksum: "sha0",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job-idx-1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/1.pdf",
		SHA256Checksum: "sha1",
		TotalPages:     4,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	for _, cmd := range []contracts.PrintJobCommand{cmd2, cmd0, cmd1} {
		if err := s.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}
	}

	jobs, err := s.GetJobsByOrderID(ctx, orderID)
	if err != nil {
		t.Fatalf("GetJobsByOrderID failed: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}

	// Must be sorted strictly by document_index ASC (0, 1, 2)
	for i, j := range jobs {
		if j.Command.DocumentIndex != i {
			t.Errorf("job[%d].DocumentIndex = %d, want %d", i, j.Command.DocumentIndex, i)
		}
	}

	// Query with empty orderID must fail
	if _, err := s.GetJobsByOrderID(ctx, ""); err == nil {
		t.Errorf("expected error when passing empty orderID, got nil")
	}
}

func TestGetIncompleteJobs(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	// Job 1: Completed
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job-done-1",
		OrderID:        "ord-a",
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/done.pdf",
		SHA256Checksum: "sha1",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = s.SaveJob(ctx, cmd1, contracts.JobStatusCompleted)

	// Job 2: Spooling under ord-b doc 0
	cmd2 := contracts.PrintJobCommand{
		JobID:          "job-spool-2",
		OrderID:        "ord-b",
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/spool.pdf",
		SHA256Checksum: "sha2",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = s.SaveJob(ctx, cmd2, contracts.JobStatusSpooling)

	// Job 3: Printing under ord-b doc 1
	cmd3 := contracts.PrintJobCommand{
		JobID:          "job-print-3",
		OrderID:        "ord-b",
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: "sha3",
		TotalPages:     3,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}
	_ = s.SaveJob(ctx, cmd3, contracts.JobStatusPrinting)

	incomplete, err := s.GetIncompleteJobs(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJobs failed: %v", err)
	}
	if len(incomplete) != 2 {
		t.Fatalf("expected 2 incomplete jobs, got %d", len(incomplete))
	}

	if incomplete[0].Command.JobID != "job-spool-2" || incomplete[1].Command.JobID != "job-print-3" {
		t.Errorf("unexpected incomplete jobs ordering: %v", incomplete)
	}
}

func TestIsOrderCompleted(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	// 1. Zero jobs: invariant check -> must return false
	completed, err := s.IsOrderCompleted(ctx, "ord-nonexistent")
	if err != nil {
		t.Fatalf("IsOrderCompleted failed: %v", err)
	}
	if completed {
		t.Fatalf("INVARIANT VIOLATION: order with 0 jobs evaluated to completed!")
	}

	// 2. Partial completion
	orderID := "ord-completion-test"
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job-part-0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0.pdf",
		SHA256Checksum: "sha0",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job-part-1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/1.pdf",
		SHA256Checksum: "sha1",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = s.SaveJob(ctx, cmd0, contracts.JobStatusCompleted)
	_ = s.SaveJob(ctx, cmd1, contracts.JobStatusPrinting)

	completed, err = s.IsOrderCompleted(ctx, orderID)
	if err != nil {
		t.Fatalf("IsOrderCompleted failed: %v", err)
	}
	if completed {
		t.Fatalf("INVARIANT VIOLATION: order with 1 of 2 jobs completed evaluated to completed!")
	}

	// 3. Full completion: update job 1 to COMPLETED
	evt := contracts.JobProgressEvent{
		JobID:         "job-part-1",
		OrderID:       orderID,
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  1,
		SheetsPrinted: 1,
	}
	if err := s.UpdateProgress(ctx, evt); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	completed, err = s.IsOrderCompleted(ctx, orderID)
	if err != nil {
		t.Fatalf("IsOrderCompleted failed: %v", err)
	}
	if !completed {
		t.Errorf("expected order to be completed when all child jobs are COMPLETED")
	}
}

func TestIsOrderCompleted_MissingJobIndices(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	orderID := "ord-missing-indices"
	// Order expects 3 documents, but only index 0 and index 2 exist (index 1 is missing)
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job-miss-0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0.pdf",
		SHA256Checksum: "sha0",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd2 := contracts.PrintJobCommand{
		JobID:          "job-miss-2",
		OrderID:        orderID,
		DocumentIndex:  2,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/2.pdf",
		SHA256Checksum: "sha2",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = s.SaveJob(ctx, cmd0, contracts.JobStatusCompleted)
	_ = s.SaveJob(ctx, cmd2, contracts.JobStatusCompleted)

	completed, err := s.IsOrderCompleted(ctx, orderID)
	if err != nil {
		t.Fatalf("IsOrderCompleted returned unexpected error: %v", err)
	}
	if completed {
		t.Fatalf("INVARIANT VIOLATION: order with missing index 1 evaluated to completed!")
	}
}

func TestIsOrderCompleted_DuplicateJobIndices(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	orderID := "ord-dup-indices"
	// Two distinct jobs saved with duplicate document_index 0
	cmd0a := contracts.PrintJobCommand{
		JobID:          "job-dup-0a",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0a.pdf",
		SHA256Checksum: "sha0a",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd0b := contracts.PrintJobCommand{
		JobID:          "job-dup-0b",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0b.pdf",
		SHA256Checksum: "sha0b",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = s.SaveJob(ctx, cmd0a, contracts.JobStatusCompleted)
	_ = s.SaveJob(ctx, cmd0b, contracts.JobStatusCompleted)

	_, err := s.IsOrderCompleted(ctx, orderID)
	if err == nil {
		t.Fatalf("expected error for duplicate document_index, got nil")
	}
}

func TestIsOrderCompleted_InconsistentTotalDocuments(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	orderID := "ord-inconsistent-total"
	// Job 0 claims total=2, Job 1 claims total=3
	cmd0 := contracts.PrintJobCommand{
		JobID:          "job-incon-0",
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/0.pdf",
		SHA256Checksum: "sha0",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	cmd1 := contracts.PrintJobCommand{
		JobID:          "job-incon-1",
		OrderID:        orderID,
		DocumentIndex:  1,
		TotalDocuments: 3,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/1.pdf",
		SHA256Checksum: "sha1",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = s.SaveJob(ctx, cmd0, contracts.JobStatusCompleted)
	_ = s.SaveJob(ctx, cmd1, contracts.JobStatusCompleted)

	_, err := s.IsOrderCompleted(ctx, orderID)
	if err == nil {
		t.Fatalf("expected error for inconsistent total_documents across jobs, got nil")
	}
}

func TestIsOrderCompleted_OutOfRangeIndex(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()
	ctx := context.Background()

	orderID := "ord-out-of-range"
	// TotalDocuments is 2, but DocumentIndex is 2 (valid indices are 0 and 1)
	cmd := contracts.PrintJobCommand{
		JobID:          "job-out-range",
		OrderID:        orderID,
		DocumentIndex:  2,
		TotalDocuments: 2,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/2.pdf",
		SHA256Checksum: "sha2",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	_ = s.SaveJob(ctx, cmd, contracts.JobStatusCompleted)

	_, err := s.IsOrderCompleted(ctx, orderID)
	if err == nil {
		t.Fatalf("expected error for out-of-range document_index, got nil")
	}
}
