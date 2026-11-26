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

const DefaultTrayCapacity = 150

type Store struct {
	db           *sql.DB
	trayCapacity int
}

// TrayCapacity returns the configured physical tray capacity for this store.
func (s *Store) TrayCapacity() int {
	if s == nil || s.trayCapacity <= 0 {
		return DefaultTrayCapacity
	}
	return s.trayCapacity
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
	return OpenWithCapacity(dbPath, DefaultTrayCapacity)
}

// OpenWithCapacity opens the SQLite state store with a validated positive tray capacity.
func OpenWithCapacity(dbPath string, trayCapacity int) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("database path cannot be empty")
	}
	if trayCapacity <= 0 {
		return nil, fmt.Errorf("tray capacity must be positive, got %d", trayCapacity)
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
        CHECK (estimated_sheets >= 0),
    is_calibrated INTEGER NOT NULL DEFAULT 0
);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to initialize database schema: %w", err)
	}

	// Inspect existing columns before migrating
	jobCols := make(map[string]bool)
	rowsJobs, err := db.QueryContext(ctx, "PRAGMA table_info(jobs);")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("inspect jobs table schema: %w", err)
	}
	for rowsJobs.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rowsJobs.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rowsJobs.Close()
			cleanup()
			return nil, fmt.Errorf("scan jobs column info: %w", err)
		}
		jobCols[name] = true
	}
	if err := rowsJobs.Err(); err != nil {
		_ = rowsJobs.Close()
		cleanup()
		return nil, fmt.Errorf("iterate jobs column info: %w", err)
	}
	_ = rowsJobs.Close()

	printerCols := make(map[string]bool)
	rowsPrinter, err := db.QueryContext(ctx, "PRAGMA table_info(printer_state);")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("inspect printer_state table schema: %w", err)
	}
	for rowsPrinter.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rowsPrinter.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rowsPrinter.Close()
			cleanup()
			return nil, fmt.Errorf("scan printer_state column info: %w", err)
		}
		printerCols[name] = true
	}
	if err := rowsPrinter.Err(); err != nil {
		_ = rowsPrinter.Close()
		cleanup()
		return nil, fmt.Errorf("iterate printer_state column info: %w", err)
	}
	_ = rowsPrinter.Close()

	// Execute migrations atomically in a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	if !jobCols["cups_job_id"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN cups_job_id TEXT NOT NULL DEFAULT '';"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add cups_job_id: %w", err)
		}
	}
	if !jobCols["submitted_to_printer"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN submitted_to_printer INTEGER NOT NULL DEFAULT 0;"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add submitted_to_printer: %w", err)
		}
	}
	if !jobCols["order_id"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN order_id TEXT NOT NULL DEFAULT '';"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add order_id: %w", err)
		}
	}
	if !jobCols["document_index"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN document_index INTEGER NOT NULL DEFAULT 0;"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add document_index: %w", err)
		}
	}
	if !jobCols["total_documents"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN total_documents INTEGER NOT NULL DEFAULT 1;"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add total_documents: %w", err)
		}
	}
	if !jobCols["original_filename"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN original_filename TEXT NOT NULL DEFAULT '';"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add original_filename: %w", err)
		}
	}
	if !jobCols["color_mode"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN color_mode TEXT NOT NULL DEFAULT 'BW';"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate jobs add color_mode: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_jobs_order_doc ON jobs (order_id, document_index);"); err != nil {
		cleanup()
		return nil, fmt.Errorf("create index idx_jobs_order_doc: %w", err)
	}

	if !printerCols["is_calibrated"] {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE printer_state ADD COLUMN is_calibrated INTEGER NOT NULL DEFAULT 0;"); err != nil {
			cleanup()
			return nil, fmt.Errorf("migrate printer_state add is_calibrated: %w", err)
		}
	}

	var stateCount int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(1) FROM printer_state WHERE id = 1;").Scan(&stateCount); err != nil {
		cleanup()
		return nil, fmt.Errorf("check printer_state record: %w", err)
	}
	if stateCount == 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO printer_state (id, estimated_sheets, is_calibrated) VALUES (1, 0, 0);"); err != nil {
			cleanup()
			return nil, fmt.Errorf("initialize default printer state: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		cleanup()
		return nil, fmt.Errorf("commit migration tx: %w", err)
	}

	return &Store{db: db, trayCapacity: trayCapacity}, nil
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

	orderID := strings.TrimSpace(cmd.OrderID)
	if orderID == "" {
		orderID = cmd.JobID
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
		orderID,
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

	if strings.TrimSpace(job.Command.OrderID) == "" {
		job.Command.OrderID = job.Command.JobID
	}

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
WHERE order_id = ? OR (order_id = '' AND job_id = ?)
ORDER BY document_index ASC;`

	rows, err := s.db.QueryContext(ctx, query, orderID, orderID)
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
// Invariant: requires exactly the authoritative TotalDocuments count, all COMPLETED.
// Rejects missing, duplicate, out-of-range, or inconsistent child indices.
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

	query := `
SELECT document_index, total_documents, status
FROM jobs
WHERE order_id = ?;`

	rows, err := s.db.QueryContext(ctx, query, orderID)
	if err != nil {
		return false, fmt.Errorf("check order completion for %s: %w", orderID, err)
	}
	defer rows.Close()

	type jobInfo struct {
		index     int
		totalDocs int
		status    string
	}
	var jobList []jobInfo
	for rows.Next() {
		var j jobInfo
		if err := rows.Scan(&j.index, &j.totalDocs, &j.status); err != nil {
			return false, fmt.Errorf("check order completion for %s: scan: %w", orderID, err)
		}
		jobList = append(jobList, j)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("check order completion for %s: rows: %w", orderID, err)
	}

	if len(jobList) == 0 {
		return false, nil
	}

	authTotal := jobList[0].totalDocs
	if authTotal < 1 {
		return false, fmt.Errorf("order %s has invalid authoritative total_documents: %d", orderID, authTotal)
	}

	seenIndices := make(map[int]bool)
	allCompleted := true

	for _, j := range jobList {
		if j.totalDocs != authTotal {
			return false, fmt.Errorf("order %s has inconsistent total_documents: expected %d, got %d", orderID, authTotal, j.totalDocs)
		}
		if j.index < 0 || j.index >= authTotal {
			return false, fmt.Errorf("order %s has out-of-range document_index: %d (total %d)", orderID, j.index, authTotal)
		}
		if seenIndices[j.index] {
			return false, fmt.Errorf("order %s has duplicate document_index: %d", orderID, j.index)
		}
		seenIndices[j.index] = true

		if j.status != string(contracts.JobStatusCompleted) {
			allCompleted = false
		}
	}

	if len(jobList) < authTotal {
		return false, nil
	}
	if len(jobList) > authTotal {
		return false, fmt.Errorf("order %s has more jobs (%d) than total_documents (%d)", orderID, len(jobList), authTotal)
	}

	for i := 0; i < authTotal; i++ {
		if !seenIndices[i] {
			return false, fmt.Errorf("order %s has missing document_index %d", orderID, i)
		}
	}

	return allCompleted, nil
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
	cap := s.TrayCapacity()
	if sheets < 0 || sheets > cap {
		return 0, fmt.Errorf("stored estimated_sheets %d outside configured capacity bounds [0, %d]", sheets, cap)
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
	cap := s.TrayCapacity()
	if est < 0 || est > cap {
		return 0, false, fmt.Errorf("stored estimated_sheets %d outside configured capacity bounds [0, %d]", est, cap)
	}

	return est, calib == 1, nil
}

// ResetPaperTray delegates directly to CalibratePaperTray to ensure all inventory updates
// pass through durable calibration safety validation and reject active or unresolved physical jobs.
func (s *Store) ResetPaperTray(
	ctx context.Context,
	sheets int,
) error {
	return s.CalibratePaperTray(ctx, sheets)
}

// CalibratePaperTray atomically calibrates paper tray inventory to sheets within [0, TrayCapacity],
// rejecting calibration while any job is actively spooling/printing or has unresolved physical uncertainty.
func (s *Store) CalibratePaperTray(
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
	cap := s.TrayCapacity()
	if sheets < 0 || sheets > cap {
		return fmt.Errorf("sheets must be between 0 and %d inclusive, got %d", cap, sheets)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin calibration tx: %w", err)
	}
	defer tx.Rollback()

	var blockingCount int
	err = tx.QueryRowContext(ctx, `
SELECT COUNT(1) FROM jobs
WHERE status IN (?, ?)
   OR (status NOT IN (?, ?) AND (submitted_to_printer = 1 OR cups_job_id != '' OR sheets_printed > 0 OR status = ?));`,
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusFailedRefundable,
		contracts.JobStatusPausedError,
	).Scan(&blockingCount)
	if err != nil {
		return fmt.Errorf("check active or uncertain jobs before calibration: %w", err)
	}
	if blockingCount > 0 {
		return fmt.Errorf("cannot calibrate paper tray while %d active or unresolved physical job(s) exist", blockingCount)
	}

	res, err := tx.ExecContext(ctx, `UPDATE printer_state SET estimated_sheets = ?, is_calibrated = 1 WHERE id = 1;`, sheets)
	if err != nil {
		return fmt.Errorf("failed to calibrate paper tray: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows during calibration: %w", err)
	}
	if rows == 0 {
		return errors.New("printer_state record not found")
	}

	return tx.Commit()
}
