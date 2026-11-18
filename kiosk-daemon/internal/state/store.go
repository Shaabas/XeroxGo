package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"xeroxgo/pkg/contracts"
)

type Store struct {
	db *sql.DB
}

type StoredJob struct {
	Command            contracts.PrintJobCommand
	Status             contracts.JobStatus
	PagesPrinted       int
	SheetsPrinted      int
	ErrorReason        string
	CUPSJobID          string
	SubmittedToPrinter bool
	UpdatedAt          time.Time
}

func isValidStatus(status contracts.JobStatus) bool {
	switch status {
	case contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusPausedError,
		contracts.JobStatusFailedRefundable:
		return true
	default:
		return false
	}
}

func isValidDuplex(duplex contracts.DuplexMode) bool {
	switch duplex {
	case contracts.DuplexSimplex,
		contracts.DuplexLongEdge,
		contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
}

func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("database path cannot be empty")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database at %s: %w", dbPath, err)
	}

	// Single connection so connection-specific SQLite settings remain predictable
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cleanup := func() {
		_ = db.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Apply and verify PRAGMA journal_mode=WAL;
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL;").Scan(&journalMode); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to execute PRAGMA journal_mode=WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		cleanup()
		return nil, fmt.Errorf("unexpected journal_mode: got %s, want wal", journalMode)
	}

	// Apply and verify PRAGMA synchronous=FULL;
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL;"); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to set PRAGMA synchronous=FULL: %w", err)
	}
	var syncVal int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous;").Scan(&syncVal); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to read PRAGMA synchronous: %w", err)
	}
	if syncVal != 2 {
		cleanup()
		return nil, fmt.Errorf("unexpected synchronous value: got %d, want 2 (FULL)", syncVal)
	}

	// Create tables
	schema := `
CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL DEFAULT '',
    document_index INTEGER NOT NULL DEFAULT 0,
    total_documents INTEGER NOT NULL DEFAULT 1,
    original_filename TEXT NOT NULL DEFAULT '',
    color_mode TEXT NOT NULL DEFAULT 'BW',
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL DEFAULT 0 CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL DEFAULT 0 CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    cups_job_id TEXT NOT NULL DEFAULT '',
    submitted_to_printer INTEGER NOT NULL DEFAULT 0,
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS printer_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    estimated_sheets INTEGER NOT NULL
        CHECK (estimated_sheets >= 0 AND estimated_sheets <= 250),
    is_calibrated INTEGER NOT NULL DEFAULT 0
);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to initialize database schema: %w", err)
	}

	// Schema migrations for existing databases
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN cups_job_id TEXT NOT NULL DEFAULT '';")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN submitted_to_printer INTEGER NOT NULL DEFAULT 0;")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN order_id TEXT NOT NULL DEFAULT '';")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN document_index INTEGER NOT NULL DEFAULT 0;")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN total_documents INTEGER NOT NULL DEFAULT 1;")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN original_filename TEXT NOT NULL DEFAULT '';")
	_, _ = db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN color_mode TEXT NOT NULL DEFAULT 'BW';")
	_, _ = db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_jobs_order_doc ON jobs (order_id, document_index);")
	_, _ = db.ExecContext(ctx, "ALTER TABLE printer_state ADD COLUMN is_calibrated INTEGER NOT NULL DEFAULT 0;")

	initQuery := `
INSERT OR IGNORE INTO printer_state (id, estimated_sheets, is_calibrated)
VALUES (1, 250, 0);
`
	if _, err := db.ExecContext(ctx, initQuery); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to initialize default printer state: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SaveJob(
	ctx context.Context,
	cmd contracts.PrintJobCommand,
	status contracts.JobStatus,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.JobID == "" {
		return errors.New("job_id cannot be empty")
	}
	if cmd.KioskID == "" {
		return errors.New("kiosk_id cannot be empty")
	}
	if cmd.DocumentIndex < 0 {
		return fmt.Errorf("document_index must be >= 0, got %d", cmd.DocumentIndex)
	}
	if cmd.TotalPages < 1 {
		return fmt.Errorf("total_pages must be >= 1, got %d", cmd.TotalPages)
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("total_sheets must be >= 1, got %d", cmd.TotalSheets)
	}
	if cmd.Copies < 1 {
		return fmt.Errorf("copies must be >= 1, got %d", cmd.Copies)
	}
	if cmd.StartFromPage < 1 {
		return fmt.Errorf("start_from_page must be >= 1, got %d", cmd.StartFromPage)
	}
	if cmd.StartFromPage > cmd.TotalPages {
		return fmt.Errorf("start_from_page (%d) cannot exceed total_pages (%d)", cmd.StartFromPage, cmd.TotalPages)
	}
	if !isValidDuplex(cmd.Duplex) {
		return fmt.Errorf("invalid duplex mode: %q", cmd.Duplex)
	}
	if !isValidStatus(status) {
		return fmt.Errorf("invalid job status: %q", status)
	}

	totalDocs := cmd.TotalDocuments
	if totalDocs < 1 {
		totalDocs = 1
	}

	colorMode := strings.ToUpper(strings.TrimSpace(cmd.ColorMode))
	if colorMode == "" {
		colorMode = "BW"
	}

	nowNS := time.Now().UTC().UnixNano()

	query := `
INSERT INTO jobs (
    job_id, order_id, document_index, total_documents, original_filename, color_mode,
    kiosk_id, display_code, download_url, sha256_checksum,
    total_pages, total_sheets, copies, duplex, start_from_page,
    status, pages_printed, sheets_printed, error_reason,
    cups_job_id, submitted_to_printer, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', '', 0, ?);`

	_, err := s.db.ExecContext(ctx, query,
		cmd.JobID,
		cmd.OrderID,
		cmd.DocumentIndex,
		totalDocs,
		cmd.OriginalFilename,
		colorMode,
		cmd.KioskID,
		cmd.DisplayCode,
		cmd.DownloadURL,
		cmd.SHA256Checksum,
		cmd.TotalPages,
		cmd.TotalSheets,
		cmd.Copies,
		string(cmd.Duplex),
		cmd.StartFromPage,
		string(status),
		nowNS,
	)
	if err != nil {
		return fmt.Errorf("failed to save job %s: %w", cmd.JobID, err)
	}

	return nil
}

func (s *Store) UpdateProgress(
	ctx context.Context,
	event contracts.JobProgressEvent,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.JobID == "" {
		return errors.New("event.job_id cannot be empty")
	}
	if !isValidStatus(event.Status) {
		return fmt.Errorf("invalid status in progress event: %q", event.Status)
	}
	if event.PagesPrinted < 0 {
		return fmt.Errorf("pages_printed cannot be negative, got %d", event.PagesPrinted)
	}
	if event.SheetsPrinted < 0 {
		return fmt.Errorf("sheets_printed cannot be negative, got %d", event.SheetsPrinted)
	}

	updatedAtNS := event.Timestamp.UTC().UnixNano()

	query := `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    updated_at_ns = ?
WHERE job_id = ?;`

	res, err := s.db.ExecContext(ctx, query,
		string(event.Status),
		event.PagesPrinted,
		event.SheetsPrinted,
		event.ErrorReason,
		updatedAtNS,
		event.JobID,
	)
	if err != nil {
		return fmt.Errorf("failed to update progress for job %s: %w", event.JobID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows for job %s: %w", event.JobID, err)
	}
	if rows == 0 {
		return fmt.Errorf("job not found: %s", event.JobID)
	}

	return nil
}

// RecordSheetProgress atomically updates a job's progress in the jobs table
// and adjusts the paper stock in printer_state within a single transaction.
// It enforces monotonicity: duplicate callbacks with the same or lesser sheet count
// will not double-decrement paper inventory, while newly completed sheets decrement
// estimated_sheets atomically with a check for sufficient supply.
func (s *Store) RecordSheetProgress(
	ctx context.Context,
	event contracts.JobProgressEvent,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.JobID == "" {
		return errors.New("event.job_id cannot be empty")
	}
	if !isValidStatus(event.Status) {
		return fmt.Errorf("invalid status in progress event: %q", event.Status)
	}
	if event.PagesPrinted < 0 {
		return fmt.Errorf("pages_printed cannot be negative, got %d", event.PagesPrinted)
	}
	if event.SheetsPrinted < 0 {
		return fmt.Errorf("sheets_printed cannot be negative, got %d", event.SheetsPrinted)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var currentPages, currentSheets, totalPages, totalSheets int
	var currentStatus string
	queryJob := `
SELECT status, pages_printed, sheets_printed, total_pages, total_sheets
FROM jobs
WHERE job_id = ?;`
	err = tx.QueryRowContext(ctx, queryJob, event.JobID).Scan(
		&currentStatus,
		&currentPages,
		&currentSheets,
		&totalPages,
		&totalSheets,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job not found: %s", event.JobID)
		}
		return fmt.Errorf("failed to query job %s: %w", event.JobID, err)
	}

	targetSheets := event.SheetsPrinted
	if event.Status == contracts.JobStatusCompleted && targetSheets == 0 {
		targetSheets = totalSheets
	}
	if targetSheets > totalSheets {
		targetSheets = totalSheets
	}
	// Monotonicity clamp: never regress sheets_printed
	if targetSheets < currentSheets {
		targetSheets = currentSheets
	}

	deltaSheets := targetSheets - currentSheets
	if deltaSheets > 0 {
		decQuery := `
UPDATE printer_state
SET estimated_sheets = estimated_sheets - ?
WHERE id = 1
  AND estimated_sheets >= ?;`
		res, err := tx.ExecContext(ctx, decQuery, deltaSheets, deltaSheets)
		if err != nil {
			return fmt.Errorf("failed to decrement sheets: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to check affected rows during decrement: %w", err)
		}
		if rows == 0 {
			return errors.New("insufficient sheets in paper tray")
		}
	}

	targetPages := event.PagesPrinted
	if event.Status == contracts.JobStatusCompleted && targetPages == 0 {
		targetPages = totalPages
	}
	if targetPages > totalPages {
		targetPages = totalPages
	}
	if targetPages < currentPages {
		targetPages = currentPages
	}

	nowNS := event.Timestamp.UTC().UnixNano()
	if event.Timestamp.IsZero() {
		nowNS = time.Now().UTC().UnixNano()
	}

	updateJob := `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    updated_at_ns = ?
WHERE job_id = ?;`
	_, err = tx.ExecContext(ctx, updateJob,
		string(event.Status),
		targetPages,
		targetSheets,
		event.ErrorReason,
		nowNS,
		event.JobID,
	)
	if err != nil {
		return fmt.Errorf("failed to update job %s: %w", event.JobID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit sheet progress transaction: %w", err)
	}

	return nil
}

func (s *Store) MarkJobSubmitting(
	ctx context.Context,
	jobID string,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" {
		return errors.New("job_id cannot be empty")
	}

	nowNS := time.Now().UTC().UnixNano()
	query := `
UPDATE jobs
SET submitted_to_printer = 1,
    status = 'PRINTING',
    updated_at_ns = ?
WHERE job_id = ?;`

	res, err := s.db.ExecContext(ctx, query, nowNS, jobID)
	if err != nil {
		return fmt.Errorf("failed to mark job %s submitting: %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows for %s: %w", jobID, err)
	}
	if rows == 0 {
		return fmt.Errorf("job not found: %s", jobID)
	}
	return nil
}

func (s *Store) RecordJobSubmission(
	ctx context.Context,
	jobID string,
	cupsJobID string,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" {
		return errors.New("job_id cannot be empty")
	}

	nowNS := time.Now().UTC().UnixNano()
	query := `
UPDATE jobs
SET cups_job_id = ?,
    submitted_to_printer = 1,
    updated_at_ns = ?
WHERE job_id = ?;`

	res, err := s.db.ExecContext(ctx, query, cupsJobID, nowNS, jobID)
	if err != nil {
		return fmt.Errorf("failed to record job submission for %s: %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows for %s: %w", jobID, err)
	}
	if rows == 0 {
		return fmt.Errorf("job not found: %s", jobID)
	}
	return nil
}

func scanStoredJob(scan func(dest ...any) error) (*StoredJob, error) {
	var job StoredJob
	var duplexStr string
	var statusStr string
	var updatedAtNS int64
	var submittedInt int

	err := scan(
		&job.Command.JobID,
		&job.Command.OrderID,
		&job.Command.DocumentIndex,
		&job.Command.TotalDocuments,
		&job.Command.OriginalFilename,
		&job.Command.ColorMode,
		&job.Command.KioskID,
		&job.Command.DisplayCode,
		&job.Command.DownloadURL,
		&job.Command.SHA256Checksum,
		&job.Command.TotalPages,
		&job.Command.TotalSheets,
		&job.Command.Copies,
		&duplexStr,
		&job.Command.StartFromPage,
		&statusStr,
		&job.PagesPrinted,
		&job.SheetsPrinted,
		&job.ErrorReason,
		&job.CUPSJobID,
		&submittedInt,
		&updatedAtNS,
	)
	if err != nil {
		return nil, err
	}

	job.Command.Duplex = contracts.DuplexMode(duplexStr)
	job.Status = contracts.JobStatus(statusStr)
	job.SubmittedToPrinter = submittedInt != 0
	job.UpdatedAt = time.Unix(0, updatedAtNS).UTC()

	return &job, nil
}

func (s *Store) GetIncompleteJob(
	ctx context.Context,
) (*StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer, updated_at_ns
FROM jobs
WHERE status IN ('SPOOLING', 'PRINTING', 'PAUSED_ERROR')
ORDER BY updated_at_ns DESC, job_id ASC
LIMIT 1;`

	row := s.db.QueryRowContext(ctx, query)
	job, err := scanStoredJob(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query incomplete job: %w", err)
	}
	return job, nil
}

// GetIncompleteJobs returns all incomplete jobs ordered by order_id, document_index, and updated_at_ns.
func (s *Store) GetIncompleteJobs(
	ctx context.Context,
) ([]StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer, updated_at_ns
FROM jobs
WHERE status IN ('SPOOLING', 'PRINTING', 'PAUSED_ERROR')
ORDER BY order_id ASC, document_index ASC, updated_at_ns ASC;`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query incomplete jobs: %w", err)
	}
	defer rows.Close()

	var jobs []StoredJob
	for rows.Next() {
		job, err := scanStoredJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan incomplete job: %w", err)
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

func (s *Store) GetJob(
	ctx context.Context,
	jobID string,
) (*StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("job_id cannot be empty")
	}

	query := `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer, updated_at_ns
FROM jobs
WHERE job_id = ?;`

	row := s.db.QueryRowContext(ctx, query, jobID)
	job, err := scanStoredJob(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query job %s: %w", jobID, err)
	}
	return job, nil
}

// GetJobsByOrderID returns all jobs belonging to an order, ordered by document_index.
func (s *Store) GetJobsByOrderID(
	ctx context.Context,
	orderID string,
) ([]StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, errors.New("order_id cannot be empty")
	}

	query := `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer, updated_at_ns
FROM jobs
WHERE order_id = ?
ORDER BY document_index ASC;`

	rows, err := s.db.QueryContext(ctx, query, orderID)
	if err != nil {
		return nil, fmt.Errorf("failed to query jobs for order %s: %w", orderID, err)
	}
	defer rows.Close()

	var jobs []StoredJob
	for rows.Next() {
		job, err := scanStoredJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job for order %s: %w", orderID, err)
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// IsOrderCompleted checks whether all jobs belonging to the order are completed.
// Invariant: exactly N persisted jobs (N > 0) must all be marked COMPLETED.
// Zero jobs must never count as completed.
func (s *Store) IsOrderCompleted(
	ctx context.Context,
	orderID string,
) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("store is not open")
	}
	if ctx == nil {
		return false, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return false, errors.New("order_id cannot be empty")
	}

	var totalJobs, completedJobs int
	query := `
SELECT COUNT(1), COALESCE(SUM(CASE WHEN status = 'COMPLETED' THEN 1 ELSE 0 END), 0)
FROM jobs
WHERE order_id = ?;`

	err := s.db.QueryRowContext(ctx, query, orderID).Scan(&totalJobs, &completedJobs)
	if err != nil {
		return false, fmt.Errorf("failed to check order completion for %s: %w", orderID, err)
	}

	if totalJobs == 0 {
		return false, nil
	}

	return totalJobs == completedJobs, nil
}

func (s *Store) GetEstimatedSheets(
	ctx context.Context,
) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store is not open")
	}
	if ctx == nil {
		return 0, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var sheets int
	err := s.db.QueryRowContext(ctx, "SELECT estimated_sheets FROM printer_state WHERE id = 1;").Scan(&sheets)
	if err != nil {
		return 0, fmt.Errorf("failed to query estimated sheets: %w", err)
	}

	return sheets, nil
}

func (s *Store) DecrementSheets(
	ctx context.Context,
	sheets int,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sheets <= 0 {
		return fmt.Errorf("sheets to decrement must be > 0, got %d", sheets)
	}

	query := `
UPDATE printer_state
SET estimated_sheets = estimated_sheets - ?
WHERE id = 1
  AND estimated_sheets >= ?;`

	res, err := s.db.ExecContext(ctx, query, sheets, sheets)
	if err != nil {
		return fmt.Errorf("failed to decrement sheets: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows during decrement: %w", err)
	}
	if rows == 0 {
		return errors.New("insufficient sheets in paper tray")
	}

	return nil
}

// GetPaperSupply returns the estimated physical stock and whether the tray has been calibrated.
func (s *Store) GetPaperSupply(
	ctx context.Context,
) (sheets int, calibrated bool, err error) {
	if s == nil || s.db == nil {
		return 0, false, errors.New("store is not open")
	}
	if ctx == nil {
		return 0, false, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}

	var est int
	var calib int
	err = s.db.QueryRowContext(ctx, "SELECT estimated_sheets, is_calibrated FROM printer_state WHERE id = 1;").Scan(&est, &calib)
	if err != nil {
		return 0, false, fmt.Errorf("failed to query paper supply: %w", err)
	}

	return est, calib == 1, nil
}

func (s *Store) ResetPaperTray(
	ctx context.Context,
	sheets int,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sheets < 0 || sheets > 250 {
		return fmt.Errorf("sheets must be between 0 and 250 inclusive, got %d", sheets)
	}

	query := `UPDATE printer_state SET estimated_sheets = ?, is_calibrated = 1 WHERE id = 1;`
	res, err := s.db.ExecContext(ctx, query, sheets)
	if err != nil {
		return fmt.Errorf("failed to reset paper tray: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows during reset: %w", err)
	}
	if rows == 0 {
		return errors.New("printer_state record not found")
	}

	return nil
}
