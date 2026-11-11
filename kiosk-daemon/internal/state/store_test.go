package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return s, dbPath
}

func sampleCommand(jobID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}
}

func TestOpenInitializesDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "init.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	sheets, err := s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 250 {
		t.Errorf("initial estimated sheets = %d, want 250", sheets)
	}

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %s, want wal", journalMode)
	}

	var syncVal int
	if err := s.db.QueryRow("PRAGMA synchronous;").Scan(&syncVal); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if syncVal != 2 {
		t.Errorf("synchronous = %d, want 2", syncVal)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}
}

func TestCrashRecoveryAfterReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crash_recovery.db")

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open failed: %v", err)
	}

	ctx := context.Background()
	cmd := sampleCommand("job-crash-001")
	cmd.TotalPages = 8
	cmd.TotalSheets = 4
	cmd.Copies = 2
	cmd.Duplex = contracts.DuplexShortEdge
	cmd.StartFromPage = 2

	if err := store1.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	fixedTime := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	progress := contracts.JobProgressEvent{
		JobID:         "job-crash-001",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  4,
		SheetsPrinted: 2,
		ErrorReason:   "printer warming up",
		Timestamp:     fixedTime,
	}

	if err := store1.UpdateProgress(ctx, progress); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	if err := store1.DecrementSheets(ctx, 3); err != nil {
		t.Fatalf("DecrementSheets failed: %v", err)
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen the database to simulate process restart
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening database failed: %v", err)
	}
	defer store2.Close()

	recovered, err := store2.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if recovered == nil {
		t.Fatal("expected recovered job, got nil")
	}

	// Verify command fields
	if recovered.Command.JobID != cmd.JobID {
		t.Errorf("JobID = %s, want %s", recovered.Command.JobID, cmd.JobID)
	}
	if recovered.Command.KioskID != cmd.KioskID {
		t.Errorf("KioskID = %s, want %s", recovered.Command.KioskID, cmd.KioskID)
	}
	if recovered.Command.DisplayCode != cmd.DisplayCode {
		t.Errorf("DisplayCode = %s, want %s", recovered.Command.DisplayCode, cmd.DisplayCode)
	}
	if recovered.Command.DownloadURL != cmd.DownloadURL {
		t.Errorf("DownloadURL = %s, want %s", recovered.Command.DownloadURL, cmd.DownloadURL)
	}
	if recovered.Command.SHA256Checksum != cmd.SHA256Checksum {
		t.Errorf("SHA256Checksum = %s, want %s", recovered.Command.SHA256Checksum, cmd.SHA256Checksum)
	}
	if recovered.Command.TotalPages != cmd.TotalPages {
		t.Errorf("TotalPages = %d, want %d", recovered.Command.TotalPages, cmd.TotalPages)
	}
	if recovered.Command.TotalSheets != cmd.TotalSheets {
		t.Errorf("TotalSheets = %d, want %d", recovered.Command.TotalSheets, cmd.TotalSheets)
	}
	if recovered.Command.Copies != cmd.Copies {
		t.Errorf("Copies = %d, want %d", recovered.Command.Copies, cmd.Copies)
	}
	if recovered.Command.Duplex != cmd.Duplex {
		t.Errorf("Duplex = %s, want %s", recovered.Command.Duplex, cmd.Duplex)
	}
	if recovered.Command.StartFromPage != cmd.StartFromPage {
		t.Errorf("StartFromPage = %d, want %d", recovered.Command.StartFromPage, cmd.StartFromPage)
	}

	// Verify progress fields
	if recovered.Status != contracts.JobStatusPrinting {
		t.Errorf("Status = %s, want %s", recovered.Status, contracts.JobStatusPrinting)
	}
	if recovered.PagesPrinted != 4 {
		t.Errorf("PagesPrinted = %d, want 4", recovered.PagesPrinted)
	}
	if recovered.SheetsPrinted != 2 {
		t.Errorf("SheetsPrinted = %d, want 2", recovered.SheetsPrinted)
	}
	if recovered.ErrorReason != "printer warming up" {
		t.Errorf("ErrorReason = %q, want %q", recovered.ErrorReason, "printer warming up")
	}
	if !recovered.UpdatedAt.Equal(fixedTime) {
		t.Errorf("UpdatedAt = %v, want %v", recovered.UpdatedAt, fixedTime)
	}

	// Verify paper tray preserved
	sheets, err := store2.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 247 {
		t.Errorf("estimated sheets after reopen = %d, want 247", sheets)
	}
}

func TestGetIncompleteJobStatuses(t *testing.T) {
	statuses := []contracts.JobStatus{
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusPausedError,
	}

	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			s, _ := newTestStore(t)
			defer s.Close()

			ctx := context.Background()
			cmd := sampleCommand("job-" + string(st))
			if err := s.SaveJob(ctx, cmd, st); err != nil {
				t.Fatalf("SaveJob failed: %v", err)
			}

			job, err := s.GetIncompleteJob(ctx)
			if err != nil {
				t.Fatalf("GetIncompleteJob failed: %v", err)
			}
			if job == nil {
				t.Fatalf("expected job with status %s, got nil", st)
			}
			if job.Status != st {
				t.Errorf("status = %s, want %s", job.Status, st)
			}
			if job.Command.JobID != cmd.JobID {
				t.Errorf("job_id = %s, want %s", job.Command.JobID, cmd.JobID)
			}
		})
	}
}

func TestGetIncompleteJobExcludesTerminalAndPendingJobs(t *testing.T) {
	nonIncompleteStatuses := []contracts.JobStatus{
		contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusCompleted,
		contracts.JobStatusFailedRefundable,
	}

	for _, st := range nonIncompleteStatuses {
		t.Run(string(st), func(t *testing.T) {
			s, _ := newTestStore(t)
			defer s.Close()

			ctx := context.Background()
			cmd := sampleCommand("job-" + string(st))
			if err := s.SaveJob(ctx, cmd, st); err != nil {
				t.Fatalf("SaveJob failed: %v", err)
			}

			job, err := s.GetIncompleteJob(ctx)
			if err != nil {
				t.Fatalf("GetIncompleteJob failed: %v", err)
			}
			if job != nil {
				t.Errorf("expected nil for status %s, got job_id=%s with status %s", st, job.Command.JobID, job.Status)
			}
		})
	}
}

func TestUpdateProgressRejectsUnknownJob(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	event := contracts.JobProgressEvent{
		JobID:         "nonexistent-job-999",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}

	err := s.UpdateProgress(ctx, event)
	if err == nil {
		t.Fatal("expected error updating unknown job, got nil")
	}

	// Verify no job was inserted
	job, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if job != nil {
		t.Errorf("expected no job in database, found %s", job.Command.JobID)
	}
}

func TestDecrementSheets(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// 1. Initial count is 250
	sheets, err := s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 250 {
		t.Fatalf("initial sheets = %d, want 250", sheets)
	}

	// 2. Decrement by 3 succeeds
	if err := s.DecrementSheets(ctx, 3); err != nil {
		t.Fatalf("DecrementSheets(3) failed: %v", err)
	}

	// 3. Count is 247
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 247 {
		t.Errorf("sheets after decrement = %d, want 247", sheets)
	}

	// 4. Decrement by 247 succeeds and yields 0
	if err := s.DecrementSheets(ctx, 247); err != nil {
		t.Fatalf("DecrementSheets(247) failed: %v", err)
	}
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 0 {
		t.Errorf("sheets after decrement = %d, want 0", sheets)
	}

	// 5. Attempting another decrement fails
	if err := s.DecrementSheets(ctx, 1); err == nil {
		t.Fatal("expected error decrementing below 0, got nil")
	}

	// 6. Count remains 0 after failed decrement
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 0 {
		t.Errorf("sheets after failed decrement = %d, want 0", sheets)
	}

	// 7. Decrementing by zero or negative count fails
	if err := s.DecrementSheets(ctx, 0); err == nil {
		t.Error("expected error for DecrementSheets(0), got nil")
	}
	if err := s.DecrementSheets(ctx, -5); err == nil {
		t.Error("expected error for DecrementSheets(-5), got nil")
	}
}

func TestResetPaperTray(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// Initial state before calibration
	initSheets, calib, err := s.GetPaperSupply(ctx)
	if err != nil {
		t.Fatalf("GetPaperSupply failed: %v", err)
	}
	if calib {
		t.Errorf("expected initial calibrated=false, got true")
	}
	if initSheets != 250 {
		t.Errorf("initial sheets = %d, want 250", initSheets)
	}

	// Reset to 250 succeeds and calibrates tray
	if err := s.ResetPaperTray(ctx, 250); err != nil {
		t.Fatalf("ResetPaperTray(250) failed: %v", err)
	}
	sheets, calib, err := s.GetPaperSupply(ctx)
	if err != nil {
		t.Fatalf("GetPaperSupply after reset failed: %v", err)
	}
	if !calib {
		t.Errorf("expected calibrated=true after ResetPaperTray, got false")
	}
	if sheets != 250 {
		t.Errorf("sheets = %d, want 250", sheets)
	}

	// Reset to 120 succeeds
	if err := s.ResetPaperTray(ctx, 120); err != nil {
		t.Fatalf("ResetPaperTray(120) failed: %v", err)
	}
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 120 {
		t.Errorf("sheets = %d, want 120", sheets)
	}

	// Reset to 0 succeeds
	if err := s.ResetPaperTray(ctx, 0); err != nil {
		t.Fatalf("ResetPaperTray(0) failed: %v", err)
	}
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 0 {
		t.Errorf("sheets = %d, want 0", sheets)
	}

	// Counts outside 0-250 are rejected
	if err := s.ResetPaperTray(ctx, -1); err == nil {
		t.Error("expected error for ResetPaperTray(-1), got nil")
	}
	if err := s.ResetPaperTray(ctx, 251); err == nil {
		t.Error("expected error for ResetPaperTray(251), got nil")
	}

	// Value was not corrupted by failed resets
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 0 {
		t.Errorf("sheets after failed resets = %d, want 0", sheets)
	}
}

func TestPersistenceOfMultipleJobs(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	cmdA := sampleCommand("job-A")
	cmdB := sampleCommand("job-B")
	cmdC := sampleCommand("job-C")

	if err := s.SaveJob(ctx, cmdA, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob A failed: %v", err)
	}
	if err := s.SaveJob(ctx, cmdB, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob B failed: %v", err)
	}
	if err := s.SaveJob(ctx, cmdC, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob C failed: %v", err)
	}

	baseTime := time.Now().UTC()

	// Update job A at T+10
	eventA := contracts.JobProgressEvent{
		JobID:         "job-A",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     baseTime.Add(10 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventA); err != nil {
		t.Fatalf("UpdateProgress A failed: %v", err)
	}

	// Update job B at T+20
	eventB := contracts.JobProgressEvent{
		JobID:         "job-B",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  5,
		SheetsPrinted: 3,
		ErrorReason:   "paper jam",
		Timestamp:     baseTime.Add(20 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventB); err != nil {
		t.Fatalf("UpdateProgress B failed: %v", err)
	}

	// Most recently updated is job B
	mostRecent, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if mostRecent == nil || mostRecent.Command.JobID != "job-B" {
		t.Fatalf("expected job-B to be most recent, got: %#v", mostRecent)
	}
	if mostRecent.PagesPrinted != 5 || mostRecent.ErrorReason != "paper jam" {
		t.Errorf("job-B progress corrupted: %#v", mostRecent)
	}

	// Now complete job B at T+30
	eventBComplete := contracts.JobProgressEvent{
		JobID:         "job-B",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 5,
		Timestamp:     baseTime.Add(30 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventBComplete); err != nil {
		t.Fatalf("UpdateProgress B complete failed: %v", err)
	}

	// Now most recently updated incomplete job should be job A (since job B is COMPLETED and job C was saved at base creation time)
	nextIncomplete, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if nextIncomplete == nil || nextIncomplete.Command.JobID != "job-A" {
		t.Fatalf("expected job-A to be next incomplete, got: %#v", nextIncomplete)
	}
	if nextIncomplete.PagesPrinted != 2 || nextIncomplete.Status != contracts.JobStatusPrinting {
		t.Errorf("job-A progress corrupted: %#v", nextIncomplete)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	// 1. Empty database path
	if _, err := Open(""); err == nil {
		t.Error("expected error for Open(\"\"), got nil")
	}

	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// 2. Invalid SaveJob inputs
	tests := []struct {
		name   string
		ctx    context.Context
		cmd    contracts.PrintJobCommand
		status contracts.JobStatus
	}{
		{
			name:   "nil_context",
			ctx:    nil,
			cmd:    sampleCommand("job-1"),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "canceled_context",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(ctx)
				cancel()
				return c
			}(),
			cmd:    sampleCommand("job-1"),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "empty_job_id",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("")
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "empty_kiosk_id",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.KioskID = ""
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "negative_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = -1
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_total_sheets",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalSheets = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_copies",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.Copies = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "start_from_page_zero",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.StartFromPage = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "start_from_page_exceeds_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = 5
				c.StartFromPage = 6
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "invalid_duplex",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.Duplex = "INVALID"
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name:   "invalid_status",
			ctx:    ctx,
			cmd:    sampleCommand("job-1"),
			status: "UNKNOWN_STATUS",
		},
	}

	for _, tt := range tests {
		t.Run("SaveJob_"+tt.name, func(t *testing.T) {
			err := s.SaveJob(tt.ctx, tt.cmd, tt.status)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
		})
	}

	// 3. Duplicate job ID
	validCmd := sampleCommand("job-unique")
	if err := s.SaveJob(ctx, validCmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("initial SaveJob failed: %v", err)
	}
	if err := s.SaveJob(ctx, validCmd, contracts.JobStatusSpooling); err == nil {
		t.Error("expected error saving duplicate job ID, got nil")
	}

	// 4. UpdateProgress invalid inputs
	progressTests := []struct {
		name  string
		ctx   context.Context
		event contracts.JobProgressEvent
	}{
		{
			name: "nil_context",
			ctx:  nil,
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "canceled_context",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(ctx)
				cancel()
				return c
			}(),
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "empty_job_id",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:     "",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "invalid_status",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    "INVALID_STATUS",
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "negative_pages_printed",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:        "job-unique",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: -1,
				Timestamp:    time.Now().UTC(),
			},
		},
		{
			name: "negative_sheets_printed",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:         "job-unique",
				Status:        contracts.JobStatusPrinting,
				SheetsPrinted: -1,
				Timestamp:     time.Now().UTC(),
			},
		},
	}

	for _, tt := range progressTests {
		t.Run("UpdateProgress_"+tt.name, func(t *testing.T) {
			err := s.UpdateProgress(tt.ctx, tt.event)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
		})
	}
}

func TestJobSubmissionTrackingAndGetJob(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	cmd := sampleCommand("job-sub-001")

	if err := s.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	// 1. Verify initial state before submission
	job, err := s.GetJob(ctx, cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if job == nil {
		t.Fatal("expected job to be found")
	}
	if job.SubmittedToPrinter {
		t.Errorf("expected SubmittedToPrinter=false initially, got true")
	}
	if job.CUPSJobID != "" {
		t.Errorf("expected empty CUPSJobID, got %q", job.CUPSJobID)
	}
	if job.Status != contracts.JobStatusSpooling {
		t.Errorf("expected status SPOOLING, got %v", job.Status)
	}

	// 2. Mark submission initiated
	if err := s.MarkJobSubmitting(ctx, cmd.JobID); err != nil {
		t.Fatalf("MarkJobSubmitting failed: %v", err)
	}

	job, err = s.GetJob(ctx, cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if !job.SubmittedToPrinter {
		t.Errorf("expected SubmittedToPrinter=true after MarkJobSubmitting")
	}
	if job.Status != contracts.JobStatusPrinting {
		t.Errorf("expected status PRINTING after MarkJobSubmitting, got %v", job.Status)
	}
	if job.CUPSJobID != "" {
		t.Errorf("expected CUPSJobID to remain empty before lp returned, got %q", job.CUPSJobID)
	}

	// 3. Record successful CUPS submission
	cupsID := "DCPT310-99"
	if err := s.RecordJobSubmission(ctx, cmd.JobID, cupsID); err != nil {
		t.Fatalf("RecordJobSubmission failed: %v", err)
	}

	job, err = s.GetJob(ctx, cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if !job.SubmittedToPrinter {
		t.Errorf("expected SubmittedToPrinter=true")
	}
	if job.CUPSJobID != cupsID {
		t.Errorf("expected CUPSJobID=%q, got %q", cupsID, job.CUPSJobID)
	}
	if job.Status != contracts.JobStatusPrinting {
		t.Errorf("expected status PRINTING, got %v", job.Status)
	}

	// 4. Verify GetIncompleteJob reflects these fields
	inc, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if inc == nil || inc.Command.JobID != cmd.JobID {
		t.Fatalf("expected incomplete job %s, got %v", cmd.JobID, inc)
	}
	if !inc.SubmittedToPrinter {
		t.Errorf("expected inc.SubmittedToPrinter=true")
	}
	if inc.CUPSJobID != cupsID {
		t.Errorf("expected inc.CUPSJobID=%q, got %q", cupsID, inc.CUPSJobID)
	}

	// 5. Test GetJob with non-existent ID
	nonExistent, err := s.GetJob(ctx, "job-does-not-exist")
	if err != nil {
		t.Fatalf("GetJob for non-existent job returned error: %v", err)
	}
	if nonExistent != nil {
		t.Errorf("expected nil for non-existent job, got %v", nonExistent)
	}

	// 6. Test invalid inputs
	if err := s.MarkJobSubmitting(ctx, ""); err == nil {
		t.Error("expected error for empty jobID in MarkJobSubmitting")
	}
	if err := s.RecordJobSubmission(ctx, "", cupsID); err == nil {
		t.Error("expected error for empty jobID in RecordJobSubmission")
	}
	if _, err := s.GetJob(ctx, ""); err == nil {
		t.Error("expected error for empty jobID in GetJob")
	}
}

func TestRecordSheetProgress_AtomicityAndRollbackOnInsufficientSupply(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	cmd := sampleCommand("job-atomicity-01")
	cmd.TotalPages = 10
	cmd.TotalSheets = 5

	if err := s.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	// Set tray stock to exactly 2 sheets
	if err := s.ResetPaperTray(ctx, 2); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	// 1. Progress by 1 sheet (stock 2 -> 1, sheets_printed 0 -> 1)
	evt1 := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, evt1); err != nil {
		t.Fatalf("RecordSheetProgress 1 failed: %v", err)
	}

	stock, _ := s.GetEstimatedSheets(ctx)
	if stock != 1 {
		t.Fatalf("expected stock 1, got %d", stock)
	}
	job, _ := s.GetJob(ctx, cmd.JobID)
	if job.SheetsPrinted != 1 || job.PagesPrinted != 2 {
		t.Fatalf("unexpected job progress: sheets=%d, pages=%d", job.SheetsPrinted, job.PagesPrinted)
	}

	// 2. Attempt to progress by 2 more sheets (requires 2 sheets, but only 1 available)
	// Must fail atomically without modifying stock or job progress!
	evt2 := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  6,
		SheetsPrinted: 3,
		Timestamp:     time.Now().UTC(),
	}
	err := s.RecordSheetProgress(ctx, evt2)
	if err == nil || !strings.Contains(err.Error(), "insufficient sheets") {
		t.Fatalf("expected insufficient sheets error, got: %v", err)
	}

	// Verify rollback: stock remains 1, sheets_printed remains 1
	stockAfter, _ := s.GetEstimatedSheets(ctx)
	if stockAfter != 1 {
		t.Fatalf("stock was corrupted during failed transaction: got %d, want 1", stockAfter)
	}
	jobAfter, _ := s.GetJob(ctx, cmd.JobID)
	if jobAfter.SheetsPrinted != 1 || jobAfter.PagesPrinted != 2 {
		t.Fatalf("job state was corrupted during failed transaction: sheets=%d, pages=%d", jobAfter.SheetsPrinted, jobAfter.PagesPrinted)
	}
}

func TestRecordSheetProgress_MonotonicityAndDuplicateIdempotency(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	cmd := sampleCommand("job-mono-01")
	cmd.TotalPages = 10
	cmd.TotalSheets = 5

	if err := s.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := s.ResetPaperTray(ctx, 20); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	// Initial progress: sheet 1
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, evt); err != nil {
		t.Fatalf("initial progress failed: %v", err)
	}
	stock, _ := s.GetEstimatedSheets(ctx)
	if stock != 19 {
		t.Fatalf("expected stock 19, got %d", stock)
	}

	// Duplicate callback with same progress (sheet 1): must be idempotent no-op for stock
	for i := 0; i < 3; i++ {
		if err := s.RecordSheetProgress(ctx, evt); err != nil {
			t.Fatalf("duplicate callback %d failed: %v", i, err)
		}
		stk, _ := s.GetEstimatedSheets(ctx)
		if stk != 19 {
			t.Fatalf("duplicate callback %d double-decremented stock to %d", i, stk)
		}
	}

	// Out-of-order / regressive progress (sheet 0): must be clamped monotonically, no stock change
	regressiveEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  1,
		SheetsPrinted: 0,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, regressiveEvt); err != nil {
		t.Fatalf("regressive callback failed: %v", err)
	}
	stk, _ := s.GetEstimatedSheets(ctx)
	if stk != 19 {
		t.Fatalf("regressive callback changed stock to %d", stk)
	}
	jb, _ := s.GetJob(ctx, cmd.JobID)
	if jb.SheetsPrinted != 1 || jb.PagesPrinted != 2 {
		t.Fatalf("regressive callback regressed job progress: %+v", jb)
	}

	// Forward progress: sheet 3 (delta = 2, stock 19 -> 17)
	evtForward := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  6,
		SheetsPrinted: 3,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, evtForward); err != nil {
		t.Fatalf("forward progress failed: %v", err)
	}
	stkForward, _ := s.GetEstimatedSheets(ctx)
	if stkForward != 17 {
		t.Fatalf("expected stock 17 after forward progress, got %d", stkForward)
	}
}

func TestRecordSheetProgress_CompletionDeductionAndDuplicateCompletion(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	cmd := sampleCommand("job-comp-01")
	cmd.TotalPages = 10
	cmd.TotalSheets = 5

	if err := s.SaveJob(ctx, cmd, contracts.JobStatusPrinting); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := s.ResetPaperTray(ctx, 50); err != nil {
		t.Fatalf("ResetPaperTray failed: %v", err)
	}

	// 1. Partial print: 2 sheets
	evtPartial := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  4,
		SheetsPrinted: 2,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, evtPartial); err != nil {
		t.Fatalf("partial progress failed: %v", err)
	}
	stock, _ := s.GetEstimatedSheets(ctx)
	if stock != 48 {
		t.Fatalf("expected stock 48, got %d", stock)
	}

	// 2. Complete job (should deduct remaining 3 sheets: 48 -> 45)
	evtComplete := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 5,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.RecordSheetProgress(ctx, evtComplete); err != nil {
		t.Fatalf("complete progress failed: %v", err)
	}
	stockAfterComp, _ := s.GetEstimatedSheets(ctx)
	if stockAfterComp != 45 {
		t.Fatalf("expected stock 45 after completion, got %d", stockAfterComp)
	}
	jb, _ := s.GetJob(ctx, cmd.JobID)
	if jb.Status != contracts.JobStatusCompleted || jb.SheetsPrinted != 5 || jb.PagesPrinted != 10 {
		t.Fatalf("unexpected completed job state: %+v", jb)
	}

	// 3. Duplicate completion: calling complete again must NOT double-decrement stock
	for i := 0; i < 2; i++ {
		if err := s.RecordSheetProgress(ctx, evtComplete); err != nil {
			t.Fatalf("duplicate completion %d failed: %v", i, err)
		}
		stk, _ := s.GetEstimatedSheets(ctx)
		if stk != 45 {
			t.Fatalf("duplicate completion %d double-decremented stock to %d", i, stk)
		}
	}
}

func TestRecordSheetProgress_Validation(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	cmd := sampleCommand("job-val-01")
	_ = s.SaveJob(ctx, cmd, contracts.JobStatusPrinting)

	// nil context
	if err := s.RecordSheetProgress(nil, contracts.JobProgressEvent{JobID: cmd.JobID, Status: contracts.JobStatusPrinting}); err == nil {
		t.Error("expected error for nil context")
	}

	// empty job ID
	if err := s.RecordSheetProgress(ctx, contracts.JobProgressEvent{JobID: "", Status: contracts.JobStatusPrinting}); err == nil {
		t.Error("expected error for empty job ID")
	}

	// invalid status
	if err := s.RecordSheetProgress(ctx, contracts.JobProgressEvent{JobID: cmd.JobID, Status: contracts.JobStatus("INVALID_STATUS")}); err == nil {
		t.Error("expected error for invalid status")
	}

	// negative pages
	if err := s.RecordSheetProgress(ctx, contracts.JobProgressEvent{JobID: cmd.JobID, Status: contracts.JobStatusPrinting, PagesPrinted: -1}); err == nil {
		t.Error("expected error for negative pages printed")
	}

	// negative sheets
	if err := s.RecordSheetProgress(ctx, contracts.JobProgressEvent{JobID: cmd.JobID, Status: contracts.JobStatusPrinting, SheetsPrinted: -1}); err == nil {
		t.Error("expected error for negative sheets printed")
	}

	// job not found
	if err := s.RecordSheetProgress(ctx, contracts.JobProgressEvent{JobID: "non-existent-job", Status: contracts.JobStatusPrinting}); err == nil {
		t.Error("expected error for non-existent job")
	}
}
