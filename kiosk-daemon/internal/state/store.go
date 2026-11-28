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
	Command              contracts.PrintJobCommand
	Status               contracts.JobStatus
	PagesPrinted         int
	SheetsPrinted        int
	ErrorReason          string
	CUPSJobID            string
	SubmittedToPrinter   bool
	BaselineCounterSet   bool
	BaselineCounterValue int64
	BaselineCounterIndex string
	BaselineCounterUnit  int64
	BaselineObservedAt   time.Time
	OutcomeVersion       int64
	OutcomeSyncedVersion int64
	OutcomeTimestamp     time.Time
	UpdatedAt            time.Time
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
    baseline_counter_set INTEGER NOT NULL DEFAULT 0,
    baseline_counter_value INTEGER NOT NULL DEFAULT 0,
    baseline_counter_index TEXT NOT NULL DEFAULT '',
    baseline_counter_unit INTEGER NOT NULL DEFAULT 0,
    baseline_observed_at_ns INTEGER NOT NULL DEFAULT 0,
    outcome_version INTEGER NOT NULL DEFAULT 0,
    outcome_synced_version INTEGER NOT NULL DEFAULT 0,
    outcome_timestamp_ns INTEGER NOT NULL DEFAULT 0,
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

	ts := event.Timestamp.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	updatedAtNS := ts.UnixNano()

	isOutcome := event.Status == contracts.JobStatusCompleted ||
		event.Status == contracts.JobStatusPausedError ||
		event.Status == contracts.JobStatusFailedRefundable

	var query string
	var args []any
	if isOutcome {
		query = `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    outcome_version = outcome_version + 1,
    outcome_timestamp_ns = ?,
    updated_at_ns = ?
WHERE job_id = ?
  AND NOT (status = 'COMPLETED' AND ? <> 'COMPLETED')
  AND NOT (status = 'FAILED_REFUNDABLE' AND ? <> 'FAILED_REFUNDABLE');`
		args = []any{
			string(event.Status),
			event.PagesPrinted,
			event.SheetsPrinted,
			event.ErrorReason,
			updatedAtNS,
			updatedAtNS,
			event.JobID,
			string(event.Status),
			string(event.Status),
		}
	} else {
		query = `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    updated_at_ns = ?
WHERE job_id = ?
  AND status NOT IN ('COMPLETED', 'FAILED_REFUNDABLE');`
		args = []any{
			string(event.Status),
			event.PagesPrinted,
			event.SheetsPrinted,
			event.ErrorReason,
			updatedAtNS,
			event.JobID,
		}
	}

	res, err := s.db.ExecContext(ctx, query, args...)
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

	if _, err := recordSheetProgressTx(ctx, tx, event, false); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit sheet progress transaction: %w", err)
	}

	return nil
}

// recordSheetProgressTx applies validated, monotonic job progress and atomic paper stock
// decrement inside an existing transaction. When skipIfIdenticalOutcome is true and the
// persisted job already matches status, monotonic pages/sheets, and error_reason, it is a
// no-op and returns changed=false without bumping outcome_version.
func recordSheetProgressTx(
	ctx context.Context,
	tx *sql.Tx,
	event contracts.JobProgressEvent,
	skipIfIdenticalOutcome bool,
) (bool, error) {
	var currentPages, currentSheets, totalPages, totalSheets int
	var currentStatus, currentReason string
	queryJob := `
SELECT status, pages_printed, sheets_printed, total_pages, total_sheets, error_reason
FROM jobs
WHERE job_id = ?;`
	err := tx.QueryRowContext(ctx, queryJob, event.JobID).Scan(
		&currentStatus,
		&currentPages,
		&currentSheets,
		&totalPages,
		&totalSheets,
		&currentReason,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("job not found: %s", event.JobID)
		}
		return false, fmt.Errorf("failed to query job %s: %w", event.JobID, err)
	}

	if event.Status == contracts.JobStatusCompleted {
		if event.SheetsPrinted != totalSheets {
			return false, fmt.Errorf("inconsistent completion sheets_printed: got %d, want %d", event.SheetsPrinted, totalSheets)
		}
		if event.PagesPrinted != totalPages {
			return false, fmt.Errorf("inconsistent completion pages_printed: got %d, want %d", event.PagesPrinted, totalPages)
		}
	}
	if currentStatus == string(contracts.JobStatusCompleted) && event.Status != contracts.JobStatusCompleted {
		return false, fmt.Errorf("cannot regress completed job %s to status %s", event.JobID, event.Status)
	}
	if currentStatus == string(contracts.JobStatusFailedRefundable) && event.Status != contracts.JobStatusFailedRefundable {
		return false, fmt.Errorf("cannot transition terminal FAILED_REFUNDABLE job %s to status %s", event.JobID, event.Status)
	}

	if event.SheetsPrinted > totalSheets {
		return false, fmt.Errorf("sheets_printed (%d) exceeds total_sheets (%d)", event.SheetsPrinted, totalSheets)
	}
	if event.PagesPrinted > totalPages {
		return false, fmt.Errorf("pages_printed (%d) exceeds total_pages (%d)", event.PagesPrinted, totalPages)
	}
	targetSheets := event.SheetsPrinted
	// Monotonicity clamp: delayed observations must not regress sheets_printed
	if targetSheets < currentSheets {
		targetSheets = currentSheets
	}
	targetPages := event.PagesPrinted
	if targetPages < currentPages {
		targetPages = currentPages
	}

	if skipIfIdenticalOutcome &&
		currentStatus == string(event.Status) &&
		targetPages == currentPages &&
		targetSheets == currentSheets &&
		currentReason == event.ErrorReason {
		return false, nil
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
			return false, fmt.Errorf("failed to decrement sheets: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("failed to check affected rows during decrement: %w", err)
		}
		if rows == 0 {
			return false, errors.New("insufficient sheets in paper tray")
		}
	}

	nowNS := event.Timestamp.UTC().UnixNano()
	if event.Timestamp.IsZero() {
		nowNS = time.Now().UTC().UnixNano()
	}

	isOutcome := event.Status == contracts.JobStatusCompleted ||
		event.Status == contracts.JobStatusPausedError ||
		event.Status == contracts.JobStatusFailedRefundable

	if isOutcome {
		updateJob := `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    outcome_version = outcome_version + 1,
    outcome_timestamp_ns = ?,
    updated_at_ns = ?
WHERE job_id = ?;`
		_, err = tx.ExecContext(ctx, updateJob,
			string(event.Status),
			targetPages,
			targetSheets,
			event.ErrorReason,
			nowNS,
			nowNS,
			event.JobID,
		)
	} else {
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
	}
	if err != nil {
		return false, fmt.Errorf("failed to update job %s: %w", event.JobID, err)
	}
	return true, nil
}

func (s *Store) MarkJobSubmitting(
	ctx context.Context,
	jobID string,
) error {
	return s.MarkJobSubmittingWithBaseline(ctx, jobID, false, 0, "", 0, time.Time{})
}

// BeginJobSpooling atomically transitions an untouched DISPATCHED job to SPOOLING before
// capability validation, downloading, or preparation begins. It rejects missing records,
// non-DISPATCHED jobs, submitted/physically started jobs, and orders with durable failure barriers.
func (s *Store) BeginJobSpooling(ctx context.Context, jobID string) error {
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spooling tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var orderID, status, cupsJobID, errorReason string
	var submitted, pagesPrinted, sheetsPrinted, docIdx int
	err = tx.QueryRowContext(ctx, `
SELECT order_id, status, submitted_to_printer, cups_job_id, pages_printed, sheets_printed, error_reason, document_index
FROM jobs
WHERE job_id = ?;`, jobID).Scan(&orderID, &status, &submitted, &cupsJobID, &pagesPrinted, &sheetsPrinted, &errorReason, &docIdx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job not found: %s", jobID)
		}
		return fmt.Errorf("query job %s before spooling: %w", jobID, err)
	}
	if status != string(contracts.JobStatusDispatched) {
		return fmt.Errorf("cannot begin spooling job %s in status %s", jobID, status)
	}
	if submitted != 0 || strings.TrimSpace(cupsJobID) != "" || pagesPrinted > 0 || sheetsPrinted > 0 || strings.TrimSpace(errorReason) != "" {
		return fmt.Errorf("cannot begin spooling job %s: already started or errored", jobID)
	}

	resolvedOrderID := strings.TrimSpace(orderID)
	if resolvedOrderID == "" {
		resolvedOrderID = jobID
	}
	var blockingSiblings int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM jobs
WHERE order_id = ?
  AND (
    status IN (?, ?, ?, ?)
    OR TRIM(error_reason) != ''
    OR (document_index < ? AND status != ?)
  );`,
		resolvedOrderID,
		string(contracts.JobStatusPausedError),
		string(contracts.JobStatusFailedRefundable),
		string(contracts.JobStatusSpooling),
		string(contracts.JobStatusPrinting),
		docIdx,
		string(contracts.JobStatusCompleted),
	).Scan(&blockingSiblings); err != nil {
		return fmt.Errorf("query order failure barrier for %s: %w", resolvedOrderID, err)
	}
	if blockingSiblings > 0 {
		return fmt.Errorf("order %s has an active failure barrier or incomplete prior document", resolvedOrderID)
	}

	nowNS := time.Now().UTC().UnixNano()
	res, err := tx.ExecContext(ctx, `
UPDATE jobs
SET status = ?, updated_at_ns = ?
WHERE job_id = ? AND status = ?;`,
		string(contracts.JobStatusSpooling), nowNS, jobID, string(contracts.JobStatusDispatched))
	if err != nil {
		return fmt.Errorf("update job %s to SPOOLING: %w", jobID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("job %s could not transition from DISPATCHED to SPOOLING", jobID)
	}
	return tx.Commit()
}

// MarkJobSubmittingWithBaseline atomically marks a job as submitted_to_printer = 1
// and persists the original pre-submission hardware counter baseline before lp runs.
// It atomically rejects terminal/paused jobs and orders with a durable failure barrier.
func (s *Store) MarkJobSubmittingWithBaseline(
	ctx context.Context,
	jobID string,
	baselineSet bool,
	baselineValue int64,
	baselineIndex string,
	baselineUnit int64,
	baselineObservedAt time.Time,
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
	setInt := 0
	if baselineSet {
		setInt = 1
	}
	obsNS := int64(0)
	if !baselineObservedAt.IsZero() {
		obsNS = baselineObservedAt.UTC().UnixNano()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark submitting tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var orderID, status string
	var submitted int
	err = tx.QueryRowContext(ctx, `
SELECT order_id, status, submitted_to_printer
FROM jobs
WHERE job_id = ?;`, jobID).Scan(&orderID, &status, &submitted)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job not found: %s", jobID)
		}
		return fmt.Errorf("query job %s before submission: %w", jobID, err)
	}
	if submitted != 0 {
		return fmt.Errorf("job %s was already marked submitted to printer", jobID)
	}
	if status == string(contracts.JobStatusCompleted) ||
		status == string(contracts.JobStatusPausedError) ||
		status == string(contracts.JobStatusFailedRefundable) {
		return fmt.Errorf("cannot submit job %s in terminal/paused status %s", jobID, status)
	}

	resolvedOrderID := strings.TrimSpace(orderID)
	if resolvedOrderID == "" {
		resolvedOrderID = jobID
	}
	var blockingSiblings int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(1)
FROM jobs
WHERE (order_id = ? OR (order_id = '' AND job_id = ?))
  AND job_id != ?
  AND status IN ('PAUSED_ERROR', 'FAILED_REFUNDABLE');`,
		resolvedOrderID, resolvedOrderID, jobID,
	).Scan(&blockingSiblings); err != nil {
		return fmt.Errorf("check order failure barrier for %s: %w", jobID, err)
	}
	if blockingSiblings > 0 {
		return fmt.Errorf("cannot submit job %s: order %s has %d paused/failed sibling(s)", jobID, resolvedOrderID, blockingSiblings)
	}

	query := `
UPDATE jobs
SET submitted_to_printer = 1,
    status = 'PRINTING',
    baseline_counter_set = CASE WHEN baseline_counter_set = 1 THEN 1 ELSE ? END,
    baseline_counter_value = CASE WHEN baseline_counter_set = 1 THEN baseline_counter_value ELSE ? END,
    baseline_counter_index = CASE WHEN baseline_counter_set = 1 THEN baseline_counter_index ELSE ? END,
    baseline_counter_unit = CASE WHEN baseline_counter_set = 1 THEN baseline_counter_unit ELSE ? END,
    baseline_observed_at_ns = CASE WHEN baseline_counter_set = 1 THEN baseline_observed_at_ns ELSE ? END,
    updated_at_ns = ?
WHERE job_id = ?
  AND submitted_to_printer = 0
  AND status NOT IN ('COMPLETED', 'PAUSED_ERROR', 'FAILED_REFUNDABLE');`

	res, err := tx.ExecContext(ctx, query, setInt, baselineValue, baselineIndex, baselineUnit, obsNS, nowNS, jobID)
	if err != nil {
		return fmt.Errorf("failed to mark job %s submitting: %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows for %s: %w", jobID, err)
	}
	if rows == 0 {
		return fmt.Errorf("job %s is not eligible for submission", jobID)
	}
	return tx.Commit()
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
WHERE job_id = ?
  AND status NOT IN ('COMPLETED', 'FAILED_REFUNDABLE')
  AND NOT (status = 'PAUSED_ERROR' AND cups_job_id != '');`

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
	var baselineSetInt int
	var baselineObsNS int64
	var outcomeTSNS int64

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
		&baselineSetInt,
		&job.BaselineCounterValue,
		&job.BaselineCounterIndex,
		&job.BaselineCounterUnit,
		&baselineObsNS,
		&job.OutcomeVersion,
		&job.OutcomeSyncedVersion,
		&outcomeTSNS,
		&updatedAtNS,
	)
	if err != nil {
		return nil, err
	}

	job.Command.Duplex = contracts.DuplexMode(duplexStr)
	job.Status = contracts.JobStatus(statusStr)
	job.SubmittedToPrinter = submittedInt != 0
	job.BaselineCounterSet = baselineSetInt != 0
	if baselineObsNS > 0 {
		job.BaselineObservedAt = time.Unix(0, baselineObsNS).UTC()
	}
	if outcomeTSNS > 0 {
		job.OutcomeTimestamp = time.Unix(0, outcomeTSNS).UTC()
	}
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
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
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
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
FROM jobs
WHERE status IN ('DISPATCHED', 'SPOOLING', 'PRINTING', 'PAUSED_ERROR')
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
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
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
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
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

// FinalizeOrderFailure atomically persists a job's failure outcome (PAUSED_ERROR or FAILED_REFUNDABLE),
// stops remaining definitely-unsubmitted siblings in the same order while preserving any COMPLETED prefix,
// bumps durable outcome sync metadata (outcome_version, outcome_timestamp_ns), and atomically invalidates
// paper inventory calibration (is_calibrated = 0) when physical consumption is uncertain.
func (s *Store) FinalizeOrderFailure(
	ctx context.Context,
	orderID string,
	jobID string,
	event contracts.JobProgressEvent,
	invalidateInventory bool,
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
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, errors.New("job_id cannot be empty")
	}
	if event.Status != contracts.JobStatusPausedError && event.Status != contracts.JobStatusFailedRefundable {
		return nil, fmt.Errorf("FinalizeOrderFailure requires PAUSED_ERROR or FAILED_REFUNDABLE, got %q", event.Status)
	}
	if event.PagesPrinted < 0 || event.SheetsPrinted < 0 {
		return nil, fmt.Errorf("invalid negative progress pages=%d sheets=%d", event.PagesPrinted, event.SheetsPrinted)
	}

	ts := event.Timestamp.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	tsNS := ts.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin FinalizeOrderFailure tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var targetOrderID, currentStatus string
	err = tx.QueryRowContext(ctx, `
SELECT order_id, status
FROM jobs
WHERE job_id = ?;`, jobID).Scan(&targetOrderID, &currentStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("job not found: %s", jobID)
		}
		return nil, fmt.Errorf("query target job %s: %w", jobID, err)
	}

	trimmedOrderID := strings.TrimSpace(orderID)
	trimmedTargetOrderID := strings.TrimSpace(targetOrderID)
	if trimmedOrderID != "" && trimmedTargetOrderID != "" && trimmedOrderID != trimmedTargetOrderID {
		return nil, fmt.Errorf("order_id mismatch for job %s: got %q, want %q", jobID, trimmedOrderID, trimmedTargetOrderID)
	}

	resolvedOrderID := trimmedOrderID
	if resolvedOrderID == "" {
		resolvedOrderID = trimmedTargetOrderID
	}
	if resolvedOrderID == "" {
		resolvedOrderID = jobID
	}

	// Preserve COMPLETED and terminal FAILED_REFUNDABLE states without regression,
	// while enforcing RecordSheetProgress bounds, monotonicity, and paper accounting.
	if currentStatus != string(contracts.JobStatusCompleted) &&
		!(currentStatus == string(contracts.JobStatusFailedRefundable) && event.Status == contracts.JobStatusPausedError) {
		evtCopy := event
		evtCopy.JobID = jobID
		evtCopy.Timestamp = ts
		if _, err := recordSheetProgressTx(ctx, tx, evtCopy, true); err != nil {
			return nil, fmt.Errorf("finalize target job %s outcome: %w", jobID, err)
		}
	}

	// Load siblings in the same order and propagate stoppage without disturbing COMPLETED prefix
	// or unverified submitted jobs.
	rows, err := tx.QueryContext(ctx, `
SELECT job_id, status, submitted_to_printer, cups_job_id, sheets_printed, pages_printed
FROM jobs
WHERE (order_id = ? OR (order_id = '' AND job_id = ?))
  AND job_id != ?
ORDER BY document_index ASC;`, resolvedOrderID, resolvedOrderID, jobID)
	if err != nil {
		return nil, fmt.Errorf("query sibling jobs for order %s: %w", resolvedOrderID, err)
	}

	type siblingRow struct {
		id        string
		status    string
		submitted bool
		cupsID    string
		sheets    int
		pages     int
	}
	var siblings []siblingRow
	for rows.Next() {
		var r siblingRow
		var subInt int
		if err := rows.Scan(&r.id, &r.status, &subInt, &r.cupsID, &r.sheets, &r.pages); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan sibling job for order %s: %w", resolvedOrderID, err)
		}
		r.submitted = subInt != 0
		siblings = append(siblings, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate sibling jobs for order %s: %w", resolvedOrderID, err)
	}
	_ = rows.Close()

	siblingReason := fmt.Sprintf("Order stopped due to earlier document failure (%s)", event.ErrorReason)
	if strings.TrimSpace(event.ErrorReason) == "" {
		siblingReason = "Order stopped due to earlier document failure"
	}

	for _, sib := range siblings {
		if sib.status == string(contracts.JobStatusCompleted) || sib.status == string(contracts.JobStatusFailedRefundable) {
			continue
		}
		definitelyUnsubmitted := !sib.submitted && strings.TrimSpace(sib.cupsID) == "" && sib.sheets == 0 && sib.pages == 0
		targetSibStatus := contracts.JobStatusPausedError
		if definitelyUnsubmitted && event.Status == contracts.JobStatusFailedRefundable {
			targetSibStatus = contracts.JobStatusFailedRefundable
		}
		if sib.status == string(targetSibStatus) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET status = ?,
    error_reason = CASE WHEN error_reason = '' THEN ? ELSE error_reason END,
    outcome_version = outcome_version + 1,
    outcome_timestamp_ns = ?,
    updated_at_ns = ?
WHERE job_id = ?;`,
			string(targetSibStatus),
			siblingReason,
			tsNS,
			tsNS,
			sib.id,
		); err != nil {
			return nil, fmt.Errorf("update sibling job %s outcome: %w", sib.id, err)
		}
	}

	if invalidateInventory {
		if _, err := tx.ExecContext(ctx, `UPDATE printer_state SET is_calibrated = 0 WHERE id = 1;`); err != nil {
			return nil, fmt.Errorf("invalidate paper calibration: %w", err)
		}
	}

	outRows, err := tx.QueryContext(ctx, `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
FROM jobs
WHERE order_id = ? OR (order_id = '' AND job_id = ?)
ORDER BY document_index ASC;`, resolvedOrderID, resolvedOrderID)
	if err != nil {
		return nil, fmt.Errorf("reload order %s jobs: %w", resolvedOrderID, err)
	}
	defer outRows.Close()

	var updated []StoredJob
	for outRows.Next() {
		job, err := scanStoredJob(outRows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan reloaded job for order %s: %w", resolvedOrderID, err)
		}
		updated = append(updated, *job)
	}
	if err := outRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reloaded jobs for order %s: %w", resolvedOrderID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit FinalizeOrderFailure: %w", err)
	}
	return updated, nil
}

// GetPendingOutcomeSyncJobs returns jobs whose latest durable outcome has not yet been
// acknowledged by the cloud backend.
func (s *Store) GetPendingOutcomeSyncJobs(ctx context.Context) ([]StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, order_id, document_index, total_documents, original_filename, color_mode,
       kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason,
       cups_job_id, submitted_to_printer,
       baseline_counter_set, baseline_counter_value, baseline_counter_index, baseline_counter_unit, baseline_observed_at_ns,
       outcome_version, outcome_synced_version, outcome_timestamp_ns, updated_at_ns
FROM jobs
WHERE outcome_version > outcome_synced_version
  AND status IN ('COMPLETED', 'PAUSED_ERROR', 'FAILED_REFUNDABLE')
ORDER BY order_id ASC, document_index ASC, updated_at_ns ASC;`)
	if err != nil {
		return nil, fmt.Errorf("query pending outcome sync jobs: %w", err)
	}
	defer rows.Close()

	var jobs []StoredJob
	for rows.Next() {
		job, err := scanStoredJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan pending outcome sync job: %w", err)
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// MarkOutcomeSynced durably records cloud acknowledgement for a specific outcome version.
func (s *Store) MarkOutcomeSynced(ctx context.Context, jobID string, version int64) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" || version <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET outcome_synced_version = ?
WHERE job_id = ?
  AND outcome_version >= ?
  AND outcome_synced_version < ?;`,
		version, jobID, version, version,
	)
	if err != nil {
		return fmt.Errorf("mark outcome synced for %s v%d: %w", jobID, version, err)
	}
	return nil
}
