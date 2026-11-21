package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
	"xeroxgo/pkg/contracts"
)

var (
	ErrPINCollision        = errors.New("pin collision: PIN is currently reserved for an active order on this kiosk")
	ErrOrderAlreadyClaimed = errors.New("order is already claimed by an active transaction session")
	ErrOrderNotFound       = errors.New("order not found")
	ErrOrderExpired        = errors.New("order has expired")
	ErrOrderNotPending     = errors.New("order is not in pending payment state")
	ErrKioskMismatch       = errors.New("order does not belong to the requested kiosk")
	ErrSessionNotFound     = errors.New("kiosk session not found")
	ErrSessionNotActive    = errors.New("kiosk session is not active")
	ErrUnauthorized        = errors.New("unauthorized: missing or invalid recovery credential")
	ErrInsufficientPaper   = errors.New("insufficient paper in kiosk tray")
)

type queryable interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Store struct {
	db      *sql.DB
	dialect string
}

// DB returns the underlying *sql.DB handle.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) rebind(query string) string {
	if s == nil || s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	paramIdx := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			b.WriteString(fmt.Sprintf("$%d", paramIdx))
			paramIdx++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

type OrderDocument struct {
	JobID            string               `json:"job_id"`
	OrderID          string               `json:"order_id"`
	DocumentIndex    int                  `json:"document_index"`
	OriginalFilename string               `json:"original_filename"`
	DownloadURL      string               `json:"download_url"`
	SHA256Checksum   string               `json:"sha256_checksum"`
	TotalPages       int                  `json:"total_pages"`
	TotalSheets      int                  `json:"total_sheets"`
	Copies           int                  `json:"copies"`
	Duplex           contracts.DuplexMode `json:"duplex"`
	ColorMode        string               `json:"color_mode"` // "BW" or "COLOR"
	PageRange        string               `json:"page_range"`
	Orientation      string               `json:"orientation"`
	PagesPerSheet    int                  `json:"pages_per_sheet"`
	PricePaise       int64                `json:"price_paise"`
	Status           contracts.JobStatus  `json:"status"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

// Command converts an OrderDocument to a contracts.PrintJobCommand.
func (d OrderDocument) Command(kioskID, displayCode string, totalDocs int) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:            d.JobID,
		OrderID:          d.OrderID,
		DocumentIndex:    d.DocumentIndex,
		TotalDocuments:   totalDocs,
		OriginalFilename: d.OriginalFilename,
		ColorMode:        d.ColorMode,
		KioskID:          kioskID,
		DisplayCode:      displayCode,
		DownloadURL:      d.DownloadURL,
		SHA256Checksum:   d.SHA256Checksum,
		TotalPages:       d.TotalPages,
		TotalSheets:      d.TotalSheets,
		Copies:           d.Copies,
		Duplex:           d.Duplex,
		StartFromPage:    1,
	}
}

type Order struct {
	OrderID      string
	Command      contracts.PrintJobCommand
	Status       contracts.JobStatus
	PricePaise   int64
	ColorMode    string // "BW" or "COLOR"
	TotalPages   int
	TotalSheets  int
	Documents    []OrderDocument
	CreatedAt    time.Time
	UpdatedAt    time.Time
	PINExpiresAt time.Time
	ClaimToken   string
}

type RecoveryRecord struct {
	RecoveryID             string
	JobID                  string
	KioskID                string
	SequenceID             int
	ConfirmedSheetsPrinted int
	RemainingPages         int
	RemainingSheets        int
	DownloadURL            string
	SHA256Checksum         string
	Status                 string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type KioskSessionStatus string

const (
	KioskSessionPaymentPending KioskSessionStatus = "PAYMENT_PENDING"
	KioskSessionCompleted      KioskSessionStatus = "COMPLETED"
	KioskSessionCanceled       KioskSessionStatus = "CANCELED"
	KioskSessionExpired        KioskSessionStatus = "EXPIRED"
)

type KioskSession struct {
	SessionID  string             `json:"session_id"`
	KioskID    string             `json:"kiosk_id"`
	OrderID    string             `json:"order_id"`
	Status     KioskSessionStatus `json:"status"`
	PricePaise int64              `json:"price_paise"`
	ExpiresAt  time.Time          `json:"expires_at"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// Open opens a database at dbPath. If dbPath starts with "postgres://" or "postgresql://",
// it connects to a PostgreSQL instance (e.g. Supabase). Otherwise it connects to SQLite,
// verifies durability pragmas, initializes schema tables, and returns a Store.
func Open(dbPath string) (*Store, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("open store: dbPath cannot be empty")
	}

	isPostgres := strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")
	if isPostgres {
		config, err := pgx.ParseConfig(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open store: postgres parse config: %w", err)
		}
		// For connection poolers like Supabase transaction mode (PgBouncer/Supavisor),
		// prepared statements must not be cached to avoid SQLSTATE 42P05.
		config.DefaultQueryExecMode = pgx.QueryExecModeExec
		db := stdlib.OpenDB(*config)
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres ping failed: %w", err)
		}

		schema := `
CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    total_pages INTEGER NOT NULL DEFAULT 0,
    total_sheets INTEGER NOT NULL DEFAULT 0,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    pin_expires_at_ns BIGINT NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);
CREATE INDEX IF NOT EXISTS idx_orders_job_id ON orders (job_id);

CREATE TABLE IF NOT EXISTS order_documents (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    document_index INTEGER NOT NULL CHECK (document_index >= 0),
    original_filename TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    color_mode TEXT NOT NULL DEFAULT 'BW',
    page_range TEXT NOT NULL DEFAULT '',
    orientation TEXT NOT NULL DEFAULT 'AUTO',
    pages_per_sheet INTEGER NOT NULL DEFAULT 1,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(order_id, document_index)
);
CREATE INDEX IF NOT EXISTS idx_order_documents_order ON order_documents (order_id, document_index);

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY
        REFERENCES orders(order_id) ON DELETE CASCADE,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    color_mode TEXT NOT NULL DEFAULT 'BW'
);

CREATE TABLE IF NOT EXISTS pricing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    price_per_sheet_paise BIGINT NOT NULL
        CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150
        CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750
        CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS recovery_records (
    recovery_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    kiosk_id TEXT NOT NULL,
    sequence_id INTEGER NOT NULL,
    confirmed_sheets_printed INTEGER NOT NULL,
    remaining_pages INTEGER NOT NULL,
    remaining_sheets INTEGER NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(job_id, sequence_id)
);
CREATE INDEX IF NOT EXISTS idx_recovery_job_seq ON recovery_records(job_id, sequence_id);

CREATE TABLE IF NOT EXISTS ads (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    ad_type TEXT NOT NULL DEFAULT 'gradient',
    image_url TEXT NOT NULL DEFAULT '',
    gradient_c0 TEXT NOT NULL DEFAULT '#1E3A8A',
    gradient_c1 TEXT NOT NULL DEFAULT '#2563EB',
    gradient_c2 TEXT NOT NULL DEFAULT '#4F46E5',
    link_url TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    username TEXT NOT NULL REFERENCES admin_users(username) ON DELETE CASCADE,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);

CREATE TABLE IF NOT EXISTS pin_reservations (
    kiosk_id TEXT NOT NULL,
    pin TEXT NOT NULL,
    order_id TEXT NOT NULL,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    PRIMARY KEY (kiosk_id, pin)
);
CREATE INDEX IF NOT EXISTS idx_pin_reservations_order ON pin_reservations (order_id);
CREATE INDEX IF NOT EXISTS idx_pin_reservations_expiry ON pin_reservations (kiosk_id, expires_at_ns);

CREATE TABLE IF NOT EXISTS pin_rate_limits (
    kiosk_id TEXT PRIMARY KEY,
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    first_failure_ns BIGINT NOT NULL DEFAULT 0,
    last_failure_ns BIGINT NOT NULL DEFAULT 0,
    blocked_until_ns BIGINT NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kiosk_sessions (
    session_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_order_status ON kiosk_sessions (order_id, status);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_kiosk ON kiosk_sessions (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_expiry ON kiosk_sessions (expires_at_ns);

CREATE TABLE IF NOT EXISTS payments (
    payment_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    status TEXT NOT NULL,
    provider TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payments_order ON payments (order_id);

CREATE TABLE IF NOT EXISTS dispatch_intents (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    dispatched_at_ns BIGINT NOT NULL DEFAULT 0,
    acknowledged_at_ns BIGINT NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_kiosk_status ON dispatch_intents (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_order ON dispatch_intents (order_id);

CREATE TABLE IF NOT EXISTS payment_attempts (
    attempt_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES kiosk_sessions(session_id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_order_id TEXT NOT NULL DEFAULT '',
    provider_payment_id TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    currency TEXT NOT NULL DEFAULT 'INR',
    status TEXT NOT NULL,
    qr_payload TEXT NOT NULL DEFAULT '',
    qr_image_url TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    demo_token TEXT NOT NULL DEFAULT '',
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_demo_token ON payment_attempts (demo_token);
`
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres schema init: %w", err)
		}

		pgMigrations := []string{
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_duplex_price_paise BIGINT NOT NULL DEFAULT 750;",
			"ALTER TABLE pricing ADD COLUMN IF NOT EXISTS color_enabled INTEGER NOT NULL DEFAULT 0;",
			"ALTER TABLE print_configs ADD COLUMN IF NOT EXISTS color_mode TEXT NOT NULL DEFAULT 'BW';",
			"ALTER TABLE orders ADD COLUMN IF NOT EXISTS pin_expires_at_ns BIGINT NOT NULL DEFAULT 0;",
			"ALTER TABLE orders ADD COLUMN IF NOT EXISTS total_pages INTEGER NOT NULL DEFAULT 0;",
			"ALTER TABLE orders ADD COLUMN IF NOT EXISTS total_sheets INTEGER NOT NULL DEFAULT 0;",
			"CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);",
			"CREATE INDEX IF NOT EXISTS idx_orders_job_id ON orders (job_id);",
			"CREATE TABLE IF NOT EXISTS order_documents (job_id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, document_index INTEGER NOT NULL CHECK (document_index >= 0), original_filename TEXT NOT NULL, download_url TEXT NOT NULL, sha256_checksum TEXT NOT NULL, total_pages INTEGER NOT NULL CHECK (total_pages > 0), total_sheets INTEGER NOT NULL CHECK (total_sheets > 0), copies INTEGER NOT NULL CHECK (copies > 0), duplex TEXT NOT NULL, color_mode TEXT NOT NULL DEFAULT 'BW', page_range TEXT NOT NULL DEFAULT '', orientation TEXT NOT NULL DEFAULT 'AUTO', pages_per_sheet INTEGER NOT NULL DEFAULT 1, price_paise BIGINT NOT NULL CHECK (price_paise >= 0), status TEXT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, UNIQUE(order_id, document_index));",
			"CREATE INDEX IF NOT EXISTS idx_order_documents_order ON order_documents (order_id, document_index);",
			"CREATE TABLE IF NOT EXISTS conversions (id TEXT PRIMARY KEY, token TEXT NOT NULL, original_filename TEXT NOT NULL, file_size BIGINT NOT NULL, status TEXT NOT NULL, error_message TEXT NOT NULL DEFAULT '', input_path TEXT NOT NULL, output_path TEXT NOT NULL DEFAULT '', created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, expires_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);",
			"CREATE TABLE IF NOT EXISTS recovery_records (recovery_id TEXT PRIMARY KEY, job_id TEXT NOT NULL, kiosk_id TEXT NOT NULL, sequence_id INTEGER NOT NULL, confirmed_sheets_printed INTEGER NOT NULL, remaining_pages INTEGER NOT NULL, remaining_sheets INTEGER NOT NULL, download_url TEXT NOT NULL, sha256_checksum TEXT NOT NULL, status TEXT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, UNIQUE(job_id, sequence_id));",
			"CREATE INDEX IF NOT EXISTS idx_recovery_job_seq ON recovery_records(job_id, sequence_id);",
			"CREATE TABLE IF NOT EXISTS pin_reservations (kiosk_id TEXT NOT NULL, pin TEXT NOT NULL, order_id TEXT NOT NULL, expires_at_ns BIGINT NOT NULL, created_at_ns BIGINT NOT NULL, PRIMARY KEY (kiosk_id, pin));",
			"CREATE INDEX IF NOT EXISTS idx_pin_reservations_order ON pin_reservations (order_id);",
			"CREATE INDEX IF NOT EXISTS idx_pin_reservations_expiry ON pin_reservations (kiosk_id, expires_at_ns);",
			"CREATE TABLE IF NOT EXISTS pin_rate_limits (kiosk_id TEXT PRIMARY KEY, failed_attempts INTEGER NOT NULL DEFAULT 0, first_failure_ns BIGINT NOT NULL DEFAULT 0, last_failure_ns BIGINT NOT NULL DEFAULT 0, blocked_until_ns BIGINT NOT NULL DEFAULT 0, updated_at_ns BIGINT NOT NULL DEFAULT 0);",
			"CREATE TABLE IF NOT EXISTS kiosk_sessions (session_id TEXT PRIMARY KEY, kiosk_id TEXT NOT NULL, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, status TEXT NOT NULL, price_paise BIGINT NOT NULL, expires_at_ns BIGINT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_order_status ON kiosk_sessions (order_id, status);",
			"CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_kiosk ON kiosk_sessions (kiosk_id, status);",
			"CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_expiry ON kiosk_sessions (expires_at_ns);",
			"CREATE TABLE IF NOT EXISTS payments (payment_id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, kiosk_id TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', amount_paise BIGINT NOT NULL, status TEXT NOT NULL, provider TEXT NOT NULL, created_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_payments_order ON payments (order_id);",
			"ALTER TABLE orders ADD COLUMN IF NOT EXISTS claim_token TEXT NOT NULL DEFAULT '';",
			"CREATE TABLE IF NOT EXISTS dispatch_intents (job_id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, kiosk_id TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, dispatched_at_ns BIGINT NOT NULL DEFAULT 0, acknowledged_at_ns BIGINT NOT NULL DEFAULT 0, retry_count INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');",
			"ALTER TABLE dispatch_intents ADD COLUMN IF NOT EXISTS acknowledged_at_ns BIGINT NOT NULL DEFAULT 0;",
			"CREATE INDEX IF NOT EXISTS idx_dispatch_intents_kiosk_status ON dispatch_intents (kiosk_id, status);",
			"CREATE INDEX IF NOT EXISTS idx_dispatch_intents_order ON dispatch_intents (order_id);",
			"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema WHERE tc.table_name = 'dispatch_intents' AND tc.constraint_type = 'PRIMARY KEY' AND kcu.column_name = 'job_id') THEN ALTER TABLE dispatch_intents DROP CONSTRAINT IF EXISTS dispatch_intents_pkey; ALTER TABLE dispatch_intents ADD PRIMARY KEY (job_id); END IF; END $$;",
			"ALTER TABLE job_progress DROP CONSTRAINT IF EXISTS job_progress_job_id_fkey;",
			"ALTER TABLE recovery_records DROP CONSTRAINT IF EXISTS recovery_records_job_id_fkey;",
			"CREATE TABLE IF NOT EXISTS payment_attempts (attempt_id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, kiosk_id TEXT NOT NULL, session_id TEXT NOT NULL REFERENCES kiosk_sessions(session_id) ON DELETE CASCADE, provider TEXT NOT NULL, provider_order_id TEXT NOT NULL DEFAULT '', provider_payment_id TEXT NOT NULL DEFAULT '', amount_paise BIGINT NOT NULL, currency TEXT NOT NULL DEFAULT 'INR', status TEXT NOT NULL, qr_payload TEXT NOT NULL DEFAULT '', qr_image_url TEXT NOT NULL DEFAULT '', idempotency_key TEXT NOT NULL DEFAULT '', demo_token TEXT NOT NULL DEFAULT '', expires_at_ns BIGINT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL);",
			"ALTER TABLE payment_attempts ADD COLUMN IF NOT EXISTS demo_token TEXT NOT NULL DEFAULT '';",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_demo_token ON payment_attempts (demo_token);",
		}

		// Preflight check: detect existing duplicate active sessions before migrating.
		// Fail closed on any database query error or if duplicate active sessions exist.
		var dupActiveSessions int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(1) FROM (
    SELECT order_id FROM kiosk_sessions 
    WHERE status = 'PAYMENT_PENDING' 
    GROUP BY order_id 
    HAVING COUNT(1) > 1
) dups;
`).Scan(&dupActiveSessions); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres migration preflight query failed: %w", err)
		}
		if dupActiveSessions > 0 {
			_ = db.Close()
			return nil, fmt.Errorf("open store: postgres migration preflight failed: found %d orders with duplicate active PAYMENT_PENDING sessions; manual operator resolution required", dupActiveSessions)
		}

		for _, mig := range pgMigrations {
			if _, err := db.Exec(mig); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("open store: postgres migration: %w", err)
			}
		}

		store := &Store{db: db, dialect: "postgres"}
		if err := store.ensureFulfillmentSchema(context.Background()); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: fulfillment schema: %w", err)
		}
		return store, nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	db.SetMaxOpenConns(1)

	// Configure SQLite durability settings
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open store: configure %s: %w", pragma, err)
		}
	}

	// Verify durability settings
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected journal_mode=wal, got %q", journalMode)
	}

	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&synchronous); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read synchronous: %w", err)
	}
	if synchronous != 2 {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected synchronous=2, got %d", synchronous)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: read foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("open store: expected foreign_keys=1, got %d", foreignKeys)
	}

	// Create required schema tables
	schema := `
CREATE TABLE IF NOT EXISTS orders (
    order_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    total_pages INTEGER NOT NULL DEFAULT 0,
    total_sheets INTEGER NOT NULL DEFAULT 0,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    pin_expires_at_ns BIGINT NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);
CREATE INDEX IF NOT EXISTS idx_orders_job_id ON orders (job_id);

CREATE TABLE IF NOT EXISTS order_documents (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    document_index INTEGER NOT NULL CHECK (document_index >= 0),
    original_filename TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    color_mode TEXT NOT NULL DEFAULT 'BW',
    page_range TEXT NOT NULL DEFAULT '',
    orientation TEXT NOT NULL DEFAULT 'AUTO',
    pages_per_sheet INTEGER NOT NULL DEFAULT 1,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(order_id, document_index)
);
CREATE INDEX IF NOT EXISTS idx_order_documents_order ON order_documents (order_id, document_index);

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY
        REFERENCES orders(order_id) ON DELETE CASCADE,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    color_mode TEXT NOT NULL DEFAULT 'BW'
);

CREATE TABLE IF NOT EXISTS pricing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    price_per_sheet_paise BIGINT NOT NULL
        CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200
        CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150
        CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000
        CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750
        CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS recovery_records (
    recovery_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    kiosk_id TEXT NOT NULL,
    sequence_id INTEGER NOT NULL,
    confirmed_sheets_printed INTEGER NOT NULL,
    remaining_pages INTEGER NOT NULL,
    remaining_sheets INTEGER NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(job_id, sequence_id)
);
CREATE INDEX IF NOT EXISTS idx_recovery_job_seq ON recovery_records(job_id, sequence_id);

CREATE TABLE IF NOT EXISTS ads (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    ad_type TEXT NOT NULL DEFAULT 'gradient',
    image_url TEXT NOT NULL DEFAULT '',
    gradient_c0 TEXT NOT NULL DEFAULT '#1E3A8A',
    gradient_c1 TEXT NOT NULL DEFAULT '#2563EB',
    gradient_c2 TEXT NOT NULL DEFAULT '#4F46E5',
    link_url TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 1,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    username TEXT NOT NULL REFERENCES admin_users(username) ON DELETE CASCADE,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: create schema: %w", err)
	}

	if err := migrateSQLiteSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: sqlite migration: %w", err)
	}

	store := &Store{db: db, dialect: "sqlite"}
	if err := store.ensureFulfillmentSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: fulfillment schema: %w", err)
	}
	return store, nil
}

func migrateSQLiteSchema(db *sql.DB) error {
	colsPricing := make(map[string]bool)
	rows, err := db.Query("PRAGMA table_info(pricing);")
	if err == nil {
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsPricing[name] = true
			}
		}
		_ = rows.Close()
	}
	if !colsPricing["bw_price_per_sheet_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_price_per_sheet_paise INTEGER NOT NULL DEFAULT 200;")
	}
	if !colsPricing["color_price_per_sheet_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_price_per_sheet_paise INTEGER NOT NULL DEFAULT 1000;")
	}
	if !colsPricing["bw_simplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_simplex_price_paise INTEGER NOT NULL DEFAULT 200;")
	}
	if !colsPricing["bw_duplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN bw_duplex_price_paise INTEGER NOT NULL DEFAULT 150;")
	}
	if !colsPricing["color_simplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_simplex_price_paise INTEGER NOT NULL DEFAULT 1000;")
	}
	if !colsPricing["color_duplex_price_paise"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_duplex_price_paise INTEGER NOT NULL DEFAULT 750;")
	}
	if !colsPricing["color_enabled"] {
		_, _ = db.Exec("ALTER TABLE pricing ADD COLUMN color_enabled INTEGER NOT NULL DEFAULT 0;")
	}

	colsConfigs := make(map[string]bool)
	rowsCfg, err := db.Query("PRAGMA table_info(print_configs);")
	if err == nil {
		for rowsCfg.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rowsCfg.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsConfigs[name] = true
			}
		}
		_ = rowsCfg.Close()
	}
	if !colsConfigs["color_mode"] {
		_, _ = db.Exec("ALTER TABLE print_configs ADD COLUMN color_mode TEXT NOT NULL DEFAULT 'BW';")
	}

	colsOrders := make(map[string]bool)
	rowsOrd, err := db.Query("PRAGMA table_info(orders);")
	if err == nil {
		for rowsOrd.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rowsOrd.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsOrders[name] = true
			}
		}
		_ = rowsOrd.Close()
	}
	if !colsOrders["pin_expires_at_ns"] {
		_, _ = db.Exec("ALTER TABLE orders ADD COLUMN pin_expires_at_ns INTEGER NOT NULL DEFAULT 0;")
	}
	if !colsOrders["claim_token"] {
		_, _ = db.Exec("ALTER TABLE orders ADD COLUMN claim_token TEXT NOT NULL DEFAULT '';")
	}
	if !colsOrders["total_pages"] {
		_, _ = db.Exec("ALTER TABLE orders ADD COLUMN total_pages INTEGER NOT NULL DEFAULT 0;")
	}
	if !colsOrders["total_sheets"] {
		_, _ = db.Exec("ALTER TABLE orders ADD COLUMN total_sheets INTEGER NOT NULL DEFAULT 0;")
	}
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_job_id ON orders (job_id);")

	_, _ = db.Exec(`
CREATE TABLE IF NOT EXISTS order_documents (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    document_index INTEGER NOT NULL CHECK (document_index >= 0),
    original_filename TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    color_mode TEXT NOT NULL DEFAULT 'BW',
    page_range TEXT NOT NULL DEFAULT '',
    orientation TEXT NOT NULL DEFAULT 'AUTO',
    pages_per_sheet INTEGER NOT NULL DEFAULT 1,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(order_id, document_index)
);
CREATE INDEX IF NOT EXISTS idx_order_documents_order ON order_documents (order_id, document_index);
`)

	// Preflight check: detect existing duplicate active sessions before migrating SQLite.
	// Must fail closed on any database query error or if duplicate active sessions exist.
	var tableCount int
	if err := db.QueryRow("SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='kiosk_sessions';").Scan(&tableCount); err != nil {
		return fmt.Errorf("open store: sqlite check kiosk_sessions table: %w", err)
	}
	if tableCount > 0 {
		var dupActiveSessions int
		if err := db.QueryRow(`
SELECT COUNT(1) FROM (
    SELECT order_id FROM kiosk_sessions 
    WHERE status = 'PAYMENT_PENDING' 
    GROUP BY order_id 
    HAVING COUNT(1) > 1
) dups;
`).Scan(&dupActiveSessions); err != nil {
			return fmt.Errorf("open store: sqlite migration preflight failed: %w", err)
		}
		if dupActiveSessions > 0 {
			return fmt.Errorf("open store: sqlite migration preflight failed: found %d orders with duplicate active PAYMENT_PENDING sessions; manual operator resolution required", dupActiveSessions)
		}
	}

	_, _ = db.Exec(`
CREATE TABLE IF NOT EXISTS conversions (
    id TEXT PRIMARY KEY,
    token TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL DEFAULT '',
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    expires_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);
CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);

CREATE TABLE IF NOT EXISTS recovery_records (
    recovery_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    kiosk_id TEXT NOT NULL,
    sequence_id INTEGER NOT NULL,
    confirmed_sheets_printed INTEGER NOT NULL,
    remaining_pages INTEGER NOT NULL,
    remaining_sheets INTEGER NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    UNIQUE(job_id, sequence_id)
);
CREATE INDEX IF NOT EXISTS idx_recovery_job_seq ON recovery_records(job_id, sequence_id);

CREATE TABLE IF NOT EXISTS pin_reservations (
    kiosk_id TEXT NOT NULL,
    pin TEXT NOT NULL,
    order_id TEXT NOT NULL,
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    PRIMARY KEY (kiosk_id, pin)
);
CREATE INDEX IF NOT EXISTS idx_pin_reservations_order ON pin_reservations (order_id);
CREATE INDEX IF NOT EXISTS idx_pin_reservations_expiry ON pin_reservations (kiosk_id, expires_at_ns);

CREATE TABLE IF NOT EXISTS pin_rate_limits (
    kiosk_id TEXT PRIMARY KEY,
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    first_failure_ns BIGINT NOT NULL DEFAULT 0,
    last_failure_ns BIGINT NOT NULL DEFAULT 0,
    blocked_until_ns BIGINT NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kiosk_sessions (
    session_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_order_status ON kiosk_sessions (order_id, status);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_kiosk ON kiosk_sessions (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_kiosk_sessions_expiry ON kiosk_sessions (expires_at_ns);

CREATE TABLE IF NOT EXISTS payments (
    payment_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    status TEXT NOT NULL,
    provider TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payments_order ON payments (order_id);

CREATE TABLE IF NOT EXISTS dispatch_intents (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    dispatched_at_ns BIGINT NOT NULL DEFAULT 0,
    acknowledged_at_ns BIGINT NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_kiosk_status ON dispatch_intents (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_order ON dispatch_intents (order_id);

CREATE TABLE IF NOT EXISTS payment_attempts (
    attempt_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES kiosk_sessions(session_id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_order_id TEXT NOT NULL DEFAULT '',
    provider_payment_id TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    currency TEXT NOT NULL DEFAULT 'INR',
    status TEXT NOT NULL,
    qr_payload TEXT NOT NULL DEFAULT '',
    qr_image_url TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    demo_token TEXT NOT NULL DEFAULT '',
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_demo_token ON payment_attempts (demo_token);
`)

	colsDispatch := make(map[string]bool)
	jobIdIsPK := false
	rowsDisp, err := db.Query("PRAGMA table_info(dispatch_intents);")
	if err != nil {
		return fmt.Errorf("open store: sqlite inspect dispatch_intents: %w", err)
	}
	for rowsDisp.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rowsDisp.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rowsDisp.Close()
			return fmt.Errorf("open store: sqlite scan dispatch_intents column info: %w", err)
		}
		colsDispatch[name] = true
		if name == "job_id" && pk == 1 {
			jobIdIsPK = true
		}
	}
	if err := rowsDisp.Err(); err != nil {
		_ = rowsDisp.Close()
		return fmt.Errorf("open store: sqlite iterate dispatch_intents column info: %w", err)
	}
	_ = rowsDisp.Close()

	if len(colsDispatch) > 0 && !jobIdIsPK {
		var dupJobs int
		if err := db.QueryRow("SELECT COUNT(1) FROM (SELECT job_id FROM dispatch_intents GROUP BY job_id HAVING COUNT(1) > 1);").Scan(&dupJobs); err != nil {
			return fmt.Errorf("open store: sqlite preflight dispatch_intents duplicate check: %w", err)
		}
		if dupJobs > 0 {
			return fmt.Errorf("open store: sqlite migration failed: found %d duplicate job_id records in dispatch_intents; manual resolution required", dupJobs)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("open store: sqlite begin dispatch_intents migration tx: %w", err)
		}
		defer tx.Rollback()

		migrationSQL := `
CREATE TABLE dispatch_intents_new (
    job_id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    dispatched_at_ns BIGINT NOT NULL DEFAULT 0,
    acknowledged_at_ns BIGINT NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
INSERT INTO dispatch_intents_new (job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns, dispatched_at_ns, acknowledged_at_ns, retry_count, last_error)
SELECT job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns, dispatched_at_ns, acknowledged_at_ns, retry_count, last_error
FROM dispatch_intents;
DROP TABLE dispatch_intents;
ALTER TABLE dispatch_intents_new RENAME TO dispatch_intents;
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_kiosk_status ON dispatch_intents (kiosk_id, status);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_order ON dispatch_intents (order_id);
`
		if _, err := tx.Exec(migrationSQL); err != nil {
			return fmt.Errorf("open store: sqlite execute dispatch_intents migration: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("open store: sqlite commit dispatch_intents migration: %w", err)
		}
		colsDispatch["acknowledged_at_ns"] = true
	}

	if len(colsDispatch) > 0 && !colsDispatch["acknowledged_at_ns"] {
		if _, err := db.Exec("ALTER TABLE dispatch_intents ADD COLUMN acknowledged_at_ns INTEGER NOT NULL DEFAULT 0;"); err != nil {
			return fmt.Errorf("open store: sqlite add acknowledged_at_ns column: %w", err)
		}
	}
	if _, err := db.Exec("UPDATE dispatch_intents SET status = 'PENDING' WHERE status = 'SENT' AND (acknowledged_at_ns = 0 OR acknowledged_at_ns IS NULL);"); err != nil {
		return fmt.Errorf("open store: sqlite reset unacknowledged dispatch intents: %w", err)
	}

	colsPayAttempts := make(map[string]bool)
	rowsPay, err := db.Query("PRAGMA table_info(payment_attempts);")
	if err != nil {
		return fmt.Errorf("open store: sqlite inspect payment_attempts: %w", err)
	}
	for rowsPay.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rowsPay.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rowsPay.Close()
			return fmt.Errorf("open store: sqlite scan payment_attempts column info: %w", err)
		}
		colsPayAttempts[name] = true
	}
	if err := rowsPay.Err(); err != nil {
		_ = rowsPay.Close()
		return fmt.Errorf("open store: sqlite iterate payment_attempts column info: %w", err)
	}
	_ = rowsPay.Close()

	if len(colsPayAttempts) > 0 && !colsPayAttempts["demo_token"] {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("open store: sqlite begin payment_attempts migration: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("ALTER TABLE payment_attempts ADD COLUMN demo_token TEXT NOT NULL DEFAULT '';"); err != nil {
			return fmt.Errorf("open store: sqlite add demo_token column: %w", err)
		}
		if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_payment_attempts_demo_token ON payment_attempts(demo_token);"); err != nil {
			return fmt.Errorf("open store: sqlite create idx_payment_attempts_demo_token: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("open store: sqlite commit payment_attempts migration: %w", err)
		}
	}

	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_kiosk_sessions_active_order ON kiosk_sessions (order_id) WHERE status = 'PAYMENT_PENDING';"); err != nil {
		return fmt.Errorf("open store: sqlite create idx_kiosk_sessions_active_order: %w", err)
	}

	return nil
}

// Close closes the underlying SQLite database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CreateOrder persists an order and its associated print configuration and child documents atomically.
// CreateOrder persists an order without a stock check. Internal/test callers can
// use this; the public mobile finalization path must use CreateOrderWithPaperCheck.
func (s *Store) CreateOrder(ctx context.Context, order Order) error {
	return s.createOrder(ctx, order, -1)
}

// CreateOrderWithPaperCheck atomically reserves the full order's sheets before
// publishing a PIN. availableStock is the latest kiosk-reported physical stock.
func (s *Store) CreateOrderWithPaperCheck(ctx context.Context, order Order, availableStock int) error {
	if availableStock < 0 {
		return fmt.Errorf("create order: available stock must be nonnegative")
	}
	return s.createOrder(ctx, order, availableStock)
}

func (s *Store) createOrder(ctx context.Context, order Order, availableStock int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create order: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create order: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("create order: %w", err)
	}

	// Validation
	if strings.TrimSpace(order.OrderID) == "" {
		return fmt.Errorf("create order: order_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.KioskID) == "" {
		return fmt.Errorf("create order: command.kiosk_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.DisplayCode) == "" {
		return fmt.Errorf("create order: command.display_code cannot be empty")
	}
	if !isValidJobStatus(order.Status) {
		return fmt.Errorf("create order: invalid job status %q", order.Status)
	}
	if order.PricePaise < 0 {
		return fmt.Errorf("create order: price_paise must be >= 0, got %d", order.PricePaise)
	}

	colorMode := strings.ToUpper(strings.TrimSpace(order.ColorMode))
	if colorMode == "" {
		colorMode = "BW"
	}

	// If no explicit documents provided, synthesize single document from order.Command
	if len(order.Documents) == 0 {
		if strings.TrimSpace(order.Command.JobID) == "" {
			return fmt.Errorf("create order: command.job_id cannot be empty")
		}
		if order.Command.TotalPages < 1 {
			return fmt.Errorf("create order: total_pages must be >= 1, got %d", order.Command.TotalPages)
		}
		if order.Command.TotalSheets < 1 {
			return fmt.Errorf("create order: total_sheets must be >= 1, got %d", order.Command.TotalSheets)
		}
		if order.Command.Copies < 1 {
			return fmt.Errorf("create order: copies must be >= 1, got %d", order.Command.Copies)
		}
		if order.Command.StartFromPage < 1 {
			return fmt.Errorf("create order: start_from_page must be >= 1, got %d", order.Command.StartFromPage)
		}
		if order.Command.StartFromPage > order.Command.TotalPages {
			return fmt.Errorf("create order: start_from_page (%d) exceeds total_pages (%d)", order.Command.StartFromPage, order.Command.TotalPages)
		}
		if !isValidDuplex(order.Command.Duplex) {
			return fmt.Errorf("create order: invalid duplex mode %q", order.Command.Duplex)
		}
		if order.TotalPages > 0 && order.TotalPages != order.Command.TotalPages {
			return fmt.Errorf("create order: total_pages mismatch: order has %d, command has %d", order.TotalPages, order.Command.TotalPages)
		}
		if order.TotalSheets > 0 && order.TotalSheets != order.Command.TotalSheets {
			return fmt.Errorf("create order: total_sheets mismatch: order has %d, command has %d", order.TotalSheets, order.Command.TotalSheets)
		}
		order.TotalPages = order.Command.TotalPages
		order.TotalSheets = order.Command.TotalSheets

		filename := order.Command.OriginalFilename
		if filename == "" {
			filename = "Document.pdf"
		}
		cmdColor := order.Command.ColorMode
		if cmdColor == "" {
			cmdColor = colorMode
		}
		order.Documents = []OrderDocument{
			{
				JobID:            order.Command.JobID,
				OrderID:          order.OrderID,
				DocumentIndex:    0,
				OriginalFilename: filename,
				DownloadURL:      order.Command.DownloadURL,
				SHA256Checksum:   order.Command.SHA256Checksum,
				TotalPages:       order.Command.TotalPages,
				TotalSheets:      order.Command.TotalSheets,
				Copies:           order.Command.Copies,
				Duplex:           order.Command.Duplex,
				ColorMode:        cmdColor,
				PricePaise:       order.PricePaise,
				Status:           order.Status,
			},
		}
	} else {
		// Validate multi-document list
		calcPages := 0
		calcSheets := 0
		var calcPrice int64
		for i, doc := range order.Documents {
			if strings.TrimSpace(doc.JobID) == "" {
				return fmt.Errorf("create order: document [%d] job_id cannot be empty", i)
			}
			if doc.DocumentIndex != i {
				return fmt.Errorf("create order: document [%d] index mismatch: got %d, want %d", i, doc.DocumentIndex, i)
			}
			if doc.TotalPages < 1 {
				return fmt.Errorf("create order: document [%d] total_pages must be >= 1, got %d", i, doc.TotalPages)
			}
			if doc.TotalSheets < 1 {
				return fmt.Errorf("create order: document [%d] total_sheets must be >= 1, got %d", i, doc.TotalSheets)
			}
			if doc.Copies < 1 {
				return fmt.Errorf("create order: document [%d] copies must be >= 1, got %d", i, doc.Copies)
			}
			if !isValidDuplex(doc.Duplex) {
				return fmt.Errorf("create order: document [%d] invalid duplex mode %q", i, doc.Duplex)
			}
			if doc.PricePaise < 0 {
				return fmt.Errorf("create order: document [%d] price_paise must be >= 0, got %d", i, doc.PricePaise)
			}
			calcPages += doc.TotalPages
			calcSheets += doc.TotalSheets
			calcPrice += doc.PricePaise
		}

		if order.TotalPages != calcPages {
			return fmt.Errorf("create order: total_pages mismatch: order has %d, sum of child documents is %d", order.TotalPages, calcPages)
		}
		if order.TotalSheets != calcSheets {
			return fmt.Errorf("create order: total_sheets mismatch: order has %d, sum of child documents is %d", order.TotalSheets, calcSheets)
		}
		if order.PricePaise != calcPrice {
			return fmt.Errorf("create order: price_paise mismatch: order has %d, sum of child documents is %d", order.PricePaise, calcPrice)
		}

		if strings.TrimSpace(order.Command.JobID) == "" {
			order.Command.JobID = order.Documents[0].JobID
		}
		order.Command.TotalPages = order.Documents[0].TotalPages
		order.Command.TotalSheets = order.Documents[0].TotalSheets
		order.Command.Copies = order.Documents[0].Copies
		if order.Command.StartFromPage < 1 {
			order.Command.StartFromPage = 1
		}
		if order.Command.Duplex == "" {
			order.Command.Duplex = order.Documents[0].Duplex
		}
		if order.Command.DownloadURL == "" {
			order.Command.DownloadURL = order.Documents[0].DownloadURL
		}
		if order.Command.SHA256Checksum == "" {
			order.Command.SHA256Checksum = order.Documents[0].SHA256Checksum
		}
	}

	if order.TotalPages <= 0 {
		order.TotalPages = order.Command.TotalPages
	}
	if order.TotalSheets <= 0 {
		order.TotalSheets = order.Command.TotalSheets
	}

	// Timestamp handling
	createdAt := order.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}

	updatedAt := order.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	} else {
		updatedAt = updatedAt.UTC()
	}

	pinExpiresAt := order.PINExpiresAt
	if pinExpiresAt.IsZero() {
		pinExpiresAt = createdAt.Add(15 * time.Minute)
	} else {
		pinExpiresAt = pinExpiresAt.UTC()
	}

	createdNS := createdAt.UnixNano()
	updatedNS := updatedAt.UnixNano()
	pinExpiresNS := pinExpiresAt.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create order: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize competing mobile reservations and kiosk session claims per kiosk.
	// The stock check and order/PIN insert share this transaction.
	if availableStock >= 0 {
		if s.dialect == "postgres" {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(22615, hashtext($1));", order.Command.KioskID); err != nil {
				return fmt.Errorf("create order: acquire kiosk paper lock: %w", err)
			}
		}
		committed, err := s.getCommittedPaperSheetsTx(ctx, tx, order.Command.KioskID, "", createdNS)
		if err != nil {
			return fmt.Errorf("create order: calculate paper commitments: %w", err)
		}
		if availableStock-committed < order.TotalSheets {
			return ErrInsufficientPaper
		}
	}

	// 1. Prune expired PIN reservations on this kiosk atomically
	_, err = tx.ExecContext(ctx, s.rebind(`
DELETE FROM pin_reservations
WHERE kiosk_id = ? AND expires_at_ns <= ?;
`), order.Command.KioskID, createdNS)
	if err != nil {
		return fmt.Errorf("create order: prune expired pin reservations: %w", err)
	}

	// 2. Reserve PIN atomically (PRIMARY KEY (kiosk_id, pin) enforces uniqueness)
	if order.Status != contracts.JobStatusCompleted && order.Status != contracts.JobStatusFailedRefundable {
		_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO pin_reservations (kiosk_id, pin, order_id, expires_at_ns, created_at_ns)
VALUES (?, ?, ?, ?, ?);
`), order.Command.KioskID, order.Command.DisplayCode, order.OrderID, pinExpiresNS, createdNS)
		if err != nil {
			errStr := strings.ToLower(err.Error())
			if strings.Contains(errStr, "unique") || strings.Contains(errStr, "primary key") || strings.Contains(errStr, "23505") {
				return fmt.Errorf("%w: pin %s is already active on kiosk %s", ErrPINCollision, order.Command.DisplayCode, order.Command.KioskID)
			}
			return fmt.Errorf("create order: reserve pin: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, total_pages, total_sheets, created_at_ns, updated_at_ns, pin_expires_at_ns, claim_token)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`),
		order.OrderID,
		order.Command.JobID,
		order.Command.KioskID,
		order.Command.DisplayCode,
		string(order.Status),
		order.PricePaise,
		order.TotalPages,
		order.TotalSheets,
		createdNS,
		updatedNS,
		pinExpiresNS,
		order.ClaimToken,
	)
	if err != nil {
		return fmt.Errorf("create order: insert into orders: %w", err)
	}

	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO print_configs (order_id, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, color_mode)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
`),
		order.OrderID,
		order.Command.DownloadURL,
		order.Command.SHA256Checksum,
		order.Command.TotalPages,
		order.Command.TotalSheets,
		order.Command.Copies,
		string(order.Command.Duplex),
		order.Command.StartFromPage,
		colorMode,
	)
	if err != nil {
		return fmt.Errorf("create order: insert into print_configs: %w", err)
	}

	// Insert all child document jobs into order_documents
	for _, doc := range order.Documents {
		docCreatedAt := doc.CreatedAt
		if docCreatedAt.IsZero() {
			docCreatedAt = createdAt
		}
		docUpdatedAt := doc.UpdatedAt
		if docUpdatedAt.IsZero() {
			docUpdatedAt = updatedAt
		}
		docColor := doc.ColorMode
		if docColor == "" {
			docColor = colorMode
		}
		docStatus := doc.Status
		if docStatus == "" {
			docStatus = order.Status
		}
		orient := doc.Orientation
		if orient == "" {
			orient = "AUTO"
		}
		pps := doc.PagesPerSheet
		if pps <= 0 {
			pps = 1
		}

		_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO order_documents (
    job_id, order_id, document_index, original_filename, download_url,
    sha256_checksum, total_pages, total_sheets, copies, duplex,
    color_mode, page_range, orientation, pages_per_sheet, price_paise,
    status, created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`),
			doc.JobID,
			order.OrderID,
			doc.DocumentIndex,
			doc.OriginalFilename,
			doc.DownloadURL,
			doc.SHA256Checksum,
			doc.TotalPages,
			doc.TotalSheets,
			doc.Copies,
			string(doc.Duplex),
			docColor,
			doc.PageRange,
			orient,
			pps,
			doc.PricePaise,
			string(docStatus),
			docCreatedAt.UnixNano(),
			docUpdatedAt.UnixNano(),
		)
		if err != nil {
			return fmt.Errorf("create order: insert into order_documents [%d]: %w", doc.DocumentIndex, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create order: commit tx: %w", err)
	}

	return nil
}

// GetOrder retrieves an order and its associated print configuration and child documents.
func (s *Store) GetOrder(ctx context.Context, orderID string) (*Order, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("get order: orderID cannot be empty")
	}

	query := `
SELECT
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise,
    COALESCE(o.total_pages, 0), COALESCE(o.total_sheets, 0),
    o.created_at_ns, o.updated_at_ns,
    p.download_url, p.sha256_checksum, p.total_pages, p.total_sheets, p.copies, p.duplex, p.start_from_page,
    COALESCE(p.color_mode, 'BW'), o.pin_expires_at_ns, COALESCE(o.claim_token, '')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.order_id = ?;
`

	row := s.db.QueryRowContext(ctx, s.rebind(query), orderID)

	var (
		resOrderID     string
		jobID          string
		kioskID        string
		displayCode    string
		statusStr      string
		pricePaise     int64
		ordTotalPages  int
		ordTotalSheets int
		createdNS      int64
		updatedNS      int64
		downloadURL    string
		sha256Check    string
		totalPages     int
		totalSheets    int
		copies         int
		duplexStr      string
		startFromPage  int
		colorMode      string
		pinExpiresNS   int64
		claimToken     string
	)

	err := row.Scan(
		&resOrderID,
		&jobID,
		&kioskID,
		&displayCode,
		&statusStr,
		&pricePaise,
		&ordTotalPages,
		&ordTotalSheets,
		&createdNS,
		&updatedNS,
		&downloadURL,
		&sha256Check,
		&totalPages,
		&totalSheets,
		&copies,
		&duplexStr,
		&startFromPage,
		&colorMode,
		&pinExpiresNS,
		&claimToken,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order: scan: %w", err)
	}

	result := &Order{
		OrderID: resOrderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			OrderID:        resOrderID,
			DocumentIndex:  0,
			TotalDocuments: 1,
			KioskID:        kioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Check,
			TotalPages:     totalPages,
			TotalSheets:    totalSheets,
			Copies:         copies,
			Duplex:         contracts.DuplexMode(duplexStr),
			StartFromPage:  startFromPage,
			ColorMode:      colorMode,
		},
		Status:       contracts.JobStatus(statusStr),
		PricePaise:   pricePaise,
		ColorMode:    colorMode,
		TotalPages:   ordTotalPages,
		TotalSheets:  ordTotalSheets,
		CreatedAt:    time.Unix(0, createdNS).UTC(),
		UpdatedAt:    time.Unix(0, updatedNS).UTC(),
		PINExpiresAt: time.Unix(0, pinExpiresNS).UTC(),
		ClaimToken:   claimToken,
	}

	// Fetch child documents from order_documents
	docs, err := s.GetOrderDocuments(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("get order: child documents for %s: %w", orderID, err)
	}
	result.Documents = docs
	if len(docs) > 0 {
		result.Command.JobID = docs[0].JobID
		result.Command.OrderID = resOrderID
		result.Command.DocumentIndex = 0
		result.Command.TotalDocuments = len(docs)
		result.Command.OriginalFilename = docs[0].OriginalFilename
		result.Command.DownloadURL = docs[0].DownloadURL
		result.Command.SHA256Checksum = docs[0].SHA256Checksum
		result.Command.TotalPages = docs[0].TotalPages
		result.Command.TotalSheets = docs[0].TotalSheets
		result.Command.Copies = docs[0].Copies
		result.Command.Duplex = docs[0].Duplex
		result.Command.ColorMode = docs[0].ColorMode
	}

	return result, nil
}

// GetOrderByJobID retrieves an order and its print configuration by jobID.
// It checks order_documents first, then falls back to orders.
func (s *Store) GetOrderByJobID(ctx context.Context, jobID string) (*Order, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order by job id: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order by job id: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get order by job id: %w", err)
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("get order by job id: jobID cannot be empty")
	}

	targetJobID := jobID
	if idx := strings.LastIndex(jobID, "-rec"); idx != -1 {
		pJobID := jobID[:idx]
		if pJobID != "" {
			targetJobID = pJobID
		}
	}

	var orderID string
	queryDoc := `SELECT order_id FROM order_documents WHERE job_id = ?;`
	err := s.db.QueryRowContext(ctx, s.rebind(queryDoc), targetJobID).Scan(&orderID)
	if err == nil && orderID != "" {
		return s.GetOrder(ctx, orderID)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get order by job id: query order_documents: %w", err)
	}

	return nil, nil
}

// GetOrderDocuments retrieves all child documents for an order, ordered by document_index.
func (s *Store) GetOrderDocuments(ctx context.Context, orderID string) ([]OrderDocument, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order documents: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order documents: context is nil")
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, fmt.Errorf("get order documents: orderID cannot be empty")
	}

	query := `
SELECT
    job_id, order_id, document_index, original_filename, download_url, sha256_checksum,
    total_pages, total_sheets, copies, duplex, color_mode, page_range, orientation,
    pages_per_sheet, price_paise, status, created_at_ns, updated_at_ns
FROM order_documents
WHERE order_id = ?
ORDER BY document_index ASC;
`
	rows, err := s.db.QueryContext(ctx, s.rebind(query), orderID)
	if err != nil {
		return nil, fmt.Errorf("get order documents: %w", err)
	}
	defer rows.Close()

	var docs []OrderDocument
	for rows.Next() {
		var (
			dJobID, dOrdID, dFilename, dURL, dCheck, dDuplex, dColor, dRange, dOrient, dStatus string
			dIdx, dPages, dSheets, dCopies, dPPS                                                int
			dPrice, dCreatedNS, dUpdatedNS                                                      int64
		)
		if err := rows.Scan(
			&dJobID, &dOrdID, &dIdx, &dFilename, &dURL, &dCheck,
			&dPages, &dSheets, &dCopies, &dDuplex, &dColor, &dRange, &dOrient,
			&dPPS, &dPrice, &dStatus, &dCreatedNS, &dUpdatedNS,
		); err != nil {
			return nil, fmt.Errorf("get order documents: scan: %w", err)
		}
		docs = append(docs, OrderDocument{
			JobID:            dJobID,
			OrderID:          dOrdID,
			DocumentIndex:    dIdx,
			OriginalFilename: dFilename,
			DownloadURL:      dURL,
			SHA256Checksum:   dCheck,
			TotalPages:       dPages,
			TotalSheets:      dSheets,
			Copies:           dCopies,
			Duplex:           contracts.DuplexMode(dDuplex),
			ColorMode:        dColor,
			PageRange:        dRange,
			Orientation:      dOrient,
			PagesPerSheet:    dPPS,
			PricePaise:       dPrice,
			Status:           contracts.JobStatus(dStatus),
			CreatedAt:        time.Unix(0, dCreatedNS).UTC(),
			UpdatedAt:        time.Unix(0, dUpdatedNS).UTC(),
		})
	}
	return docs, rows.Err()
}

// GetOrderDocument retrieves a single child document by its jobID.
func (s *Store) GetOrderDocument(ctx context.Context, jobID string) (*OrderDocument, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order document: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order document: context is nil")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("get order document: jobID cannot be empty")
	}

	query := `
SELECT
    job_id, order_id, document_index, original_filename, download_url, sha256_checksum,
    total_pages, total_sheets, copies, duplex, color_mode, page_range, orientation,
    pages_per_sheet, price_paise, status, created_at_ns, updated_at_ns
FROM order_documents
WHERE job_id = ?;
`
	var (
		dJobID, dOrdID, dFilename, dURL, dCheck, dDuplex, dColor, dRange, dOrient, dStatus string
		dIdx, dPages, dSheets, dCopies, dPPS                                                int
		dPrice, dCreatedNS, dUpdatedNS                                                      int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), jobID).Scan(
		&dJobID, &dOrdID, &dIdx, &dFilename, &dURL, &dCheck,
		&dPages, &dSheets, &dCopies, &dDuplex, &dColor, &dRange, &dOrient,
		&dPPS, &dPrice, &dStatus, &dCreatedNS, &dUpdatedNS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order document: %w", err)
	}

	return &OrderDocument{
		JobID:            dJobID,
		OrderID:          dOrdID,
		DocumentIndex:    dIdx,
		OriginalFilename: dFilename,
		DownloadURL:      dURL,
		SHA256Checksum:   dCheck,
		TotalPages:       dPages,
		TotalSheets:      dSheets,
		Copies:           dCopies,
		Duplex:           contracts.DuplexMode(dDuplex),
		ColorMode:        dColor,
		PageRange:        dRange,
		Orientation:      dOrient,
		PagesPerSheet:    dPPS,
		PricePaise:       dPrice,
		Status:           contracts.JobStatus(dStatus),
		CreatedAt:        time.Unix(0, dCreatedNS).UTC(),
		UpdatedAt:        time.Unix(0, dUpdatedNS).UTC(),
	}, nil
}

// GetOrderByPIN retrieves the latest order for a given kioskID and numeric PIN.
func (s *Store) GetOrderByPIN(ctx context.Context, kioskID, pin string) (*Order, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get order by pin: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get order by pin: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get order by pin: %w", err)
	}
	kioskID = strings.TrimSpace(kioskID)
	pin = strings.TrimSpace(pin)
	if kioskID == "" || pin == "" {
		return nil, nil
	}

	query := `
SELECT order_id
FROM orders
WHERE kiosk_id = ? AND display_code = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	var orderID string
	err := s.db.QueryRowContext(ctx, s.rebind(query), kioskID, pin).Scan(&orderID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order by pin: scan: %w", err)
	}

	return s.GetOrder(ctx, orderID)
}

// IsPINInUse checks whether the given PIN is currently reserved and unexpired on the specified kiosk.
func (s *Store) IsPINInUse(ctx context.Context, kioskID, pin string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("is pin in use: store is closed or nil")
	}
	if ctx == nil {
		return false, fmt.Errorf("is pin in use: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("is pin in use: %w", err)
	}
	kioskID = strings.TrimSpace(kioskID)
	pin = strings.TrimSpace(pin)
	if kioskID == "" || pin == "" {
		return false, nil
	}

	nowNS := now.UTC().UnixNano()
	query := `
SELECT COUNT(1)
FROM pin_reservations
WHERE kiosk_id = ?
  AND pin = ?
  AND expires_at_ns > ?;
`
	var count int
	err := s.db.QueryRowContext(ctx, s.rebind(query), kioskID, pin, nowNS).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("is pin in use: query: %w", err)
	}
	return count > 0, nil
}

// CheckRateLimit checks whether the kiosk is currently blocked due to repeated failed PIN attempts.
// It is durable across server restarts and shared across all backend replicas.
func (s *Store) CheckRateLimit(ctx context.Context, kioskID string, now time.Time) (bool, time.Duration, error) {
	if s == nil || s.db == nil {
		return true, 0, nil
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return true, 0, nil
	}

	query := `
SELECT failed_attempts, first_failure_ns, blocked_until_ns
FROM pin_rate_limits
WHERE kiosk_id = ?;
`
	var (
		failedAttempts int
		firstFailNS    int64
		blockedUntilNS int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), kioskID).Scan(&failedAttempts, &firstFailNS, &blockedUntilNS)
	if err == sql.ErrNoRows {
		return true, 0, nil
	}
	if err != nil {
		return true, 0, fmt.Errorf("check rate limit: %w", err)
	}

	nowNS := now.UTC().UnixNano()
	if blockedUntilNS > nowNS {
		remaining := time.Duration(blockedUntilNS - nowNS)
		return false, remaining, nil
	}

	return true, 0, nil
}

// RecordRateLimitFailure records a failed PIN lookup attempt.
// If the failure count reaches maxFailures within window, the kiosk is blocked for blockDuration.
func (s *Store) RecordRateLimitFailure(ctx context.Context, kioskID string, now time.Time, maxFailures int, window, blockDuration time.Duration) (bool, time.Duration, error) {
	if s == nil || s.db == nil {
		return false, 0, nil
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return false, 0, nil
	}

	nowNS := now.UTC().UnixNano()
	windowNS := window.Nanoseconds()
	blockNS := blockDuration.Nanoseconds()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("record rate limit failure: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
SELECT failed_attempts, first_failure_ns, blocked_until_ns
FROM pin_rate_limits
WHERE kiosk_id = ?;
`
	var (
		failedAttempts int
		firstFailNS    int64
		blockedUntilNS int64
	)
	err = tx.QueryRowContext(ctx, s.rebind(query), kioskID).Scan(&failedAttempts, &firstFailNS, &blockedUntilNS)
	if err == sql.ErrNoRows {
		failedAttempts = 1
		firstFailNS = nowNS
		blockedUntilNS = 0
		if failedAttempts >= maxFailures {
			blockedUntilNS = nowNS + blockNS
		}
		_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO pin_rate_limits (kiosk_id, failed_attempts, first_failure_ns, last_failure_ns, blocked_until_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?, ?);
`), kioskID, failedAttempts, firstFailNS, nowNS, blockedUntilNS, nowNS)
		if err != nil {
			return false, 0, fmt.Errorf("record rate limit failure: insert: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, 0, fmt.Errorf("record rate limit failure: commit: %w", err)
		}
		if blockedUntilNS > nowNS {
			return true, time.Duration(blockedUntilNS - nowNS), nil
		}
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("record rate limit failure: query: %w", err)
	}

	// If currently blocked, retain block
	if blockedUntilNS > nowNS {
		_ = tx.Commit()
		return true, time.Duration(blockedUntilNS - nowNS), nil
	}

	// If window expired, reset failure window
	if nowNS-firstFailNS > windowNS {
		failedAttempts = 1
		firstFailNS = nowNS
		blockedUntilNS = 0
	} else {
		failedAttempts++
	}

	if failedAttempts >= maxFailures {
		blockedUntilNS = nowNS + blockNS
	}

	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE pin_rate_limits
SET failed_attempts = ?, first_failure_ns = ?, last_failure_ns = ?, blocked_until_ns = ?, updated_at_ns = ?
WHERE kiosk_id = ?;
`), failedAttempts, firstFailNS, nowNS, blockedUntilNS, nowNS, kioskID)
	if err != nil {
		return false, 0, fmt.Errorf("record rate limit failure: update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("record rate limit failure: commit: %w", err)
	}

	if blockedUntilNS > nowNS {
		return true, time.Duration(blockedUntilNS - nowNS), nil
	}
	return false, 0, nil
}

// RecordRateLimitSuccess resets failed attempt tracking for a kiosk upon successful PIN entry.
func (s *Store) RecordRateLimitSuccess(ctx context.Context, kioskID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.rebind(`
DELETE FROM pin_rate_limits WHERE kiosk_id = ?;
`), kioskID)
	if err != nil {
		return fmt.Errorf("record rate limit success: %w", err)
	}
	return nil
}

// ResetRateLimits clears all rate limit tracking records.
func (s *Store) ResetRateLimits(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.rebind(`
DELETE FROM pin_rate_limits;
`))
	return err
}

// CreateRecoveryRecord persists an immutable recovery attempt record.
func (s *Store) CreateRecoveryRecord(ctx context.Context, rec RecoveryRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create recovery record: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create recovery record: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("create recovery record: %w", err)
	}
	if strings.TrimSpace(rec.RecoveryID) == "" {
		return fmt.Errorf("create recovery record: recovery_id cannot be empty")
	}
	if strings.TrimSpace(rec.JobID) == "" {
		return fmt.Errorf("create recovery record: job_id cannot be empty")
	}
	if strings.TrimSpace(rec.KioskID) == "" {
		return fmt.Errorf("create recovery record: kiosk_id cannot be empty")
	}
	if rec.SequenceID < 1 {
		return fmt.Errorf("create recovery record: sequence_id must be >= 1, got %d", rec.SequenceID)
	}
	if rec.ConfirmedSheetsPrinted < 0 {
		return fmt.Errorf("create recovery record: confirmed_sheets_printed must be >= 0, got %d", rec.ConfirmedSheetsPrinted)
	}
	if rec.RemainingPages < 1 {
		return fmt.Errorf("create recovery record: remaining_pages must be >= 1, got %d", rec.RemainingPages)
	}
	if rec.RemainingSheets < 1 {
		return fmt.Errorf("create recovery record: remaining_sheets must be >= 1, got %d", rec.RemainingSheets)
	}
	if strings.TrimSpace(rec.DownloadURL) == "" {
		return fmt.Errorf("create recovery record: download_url cannot be empty")
	}
	if strings.TrimSpace(rec.SHA256Checksum) == "" {
		return fmt.Errorf("create recovery record: sha256_checksum cannot be empty")
	}

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := rec.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	query := `
INSERT INTO recovery_records (
    recovery_id, job_id, kiosk_id, sequence_id, confirmed_sheets_printed,
    remaining_pages, remaining_sheets, download_url, sha256_checksum, status,
    created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`
	_, err := s.db.ExecContext(ctx, s.rebind(query),
		rec.RecoveryID, rec.JobID, rec.KioskID, rec.SequenceID, rec.ConfirmedSheetsPrinted,
		rec.RemainingPages, rec.RemainingSheets, rec.DownloadURL, rec.SHA256Checksum, rec.Status,
		createdAt.UnixNano(), updatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("create recovery record: %w", err)
	}
	return nil
}

// GetRecoveryRecord retrieves a recovery record by recoveryID.
func (s *Store) GetRecoveryRecord(ctx context.Context, recoveryID string) (*RecoveryRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get recovery record: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get recovery record: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get recovery record: %w", err)
	}
	if strings.TrimSpace(recoveryID) == "" {
		return nil, fmt.Errorf("get recovery record: recoveryID cannot be empty")
	}

	query := `
SELECT
    recovery_id, job_id, kiosk_id, sequence_id, confirmed_sheets_printed,
    remaining_pages, remaining_sheets, download_url, sha256_checksum, status,
    created_at_ns, updated_at_ns
FROM recovery_records
WHERE recovery_id = ?;
`
	row := s.db.QueryRowContext(ctx, s.rebind(query), recoveryID)

	var (
		recID       string
		jobID       string
		kioskID     string
		seqID       int
		confSheets  int
		remPages    int
		remSheets   int
		downloadURL string
		sha256Check string
		status      string
		createdNS   int64
		updatedNS   int64
	)
	err := row.Scan(
		&recID, &jobID, &kioskID, &seqID, &confSheets,
		&remPages, &remSheets, &downloadURL, &sha256Check, &status,
		&createdNS, &updatedNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recovery record: scan: %w", err)
	}

	return &RecoveryRecord{
		RecoveryID:             recID,
		JobID:                  jobID,
		KioskID:                kioskID,
		SequenceID:             seqID,
		ConfirmedSheetsPrinted: confSheets,
		RemainingPages:         remPages,
		RemainingSheets:        remSheets,
		DownloadURL:            downloadURL,
		SHA256Checksum:         sha256Check,
		Status:                 status,
		CreatedAt:              time.Unix(0, createdNS).UTC(),
		UpdatedAt:              time.Unix(0, updatedNS).UTC(),
	}, nil
}

// GetLatestRecoverySequence returns the maximum sequence_id for the given jobID (0 if none exist).
func (s *Store) GetLatestRecoverySequence(ctx context.Context, jobID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("get latest recovery sequence: store is closed or nil")
	}
	if ctx == nil {
		return 0, fmt.Errorf("get latest recovery sequence: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("get latest recovery sequence: %w", err)
	}
	if strings.TrimSpace(jobID) == "" {
		return 0, fmt.Errorf("get latest recovery sequence: jobID cannot be empty")
	}

	query := `SELECT COALESCE(MAX(sequence_id), 0) FROM recovery_records WHERE job_id = ?;`
	row := s.db.QueryRowContext(ctx, s.rebind(query), jobID)
	var maxSeq int
	if err := row.Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("get latest recovery sequence: scan: %w", err)
	}
	return maxSeq, nil
}

// UpdateOrderStatus updates an existing order's status and updated_at_ns.
func (s *Store) UpdateOrderStatus(ctx context.Context, orderID string, status contracts.JobStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update order status: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update order status: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if strings.TrimSpace(orderID) == "" {
		return fmt.Errorf("update order status: orderID cannot be empty")
	}
	if !isValidJobStatus(status) {
		return fmt.Errorf("update order status: invalid job status %q", status)
	}

	updatedNS := time.Now().UTC().UnixNano()

	res, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ?;
`), string(status), updatedNS, orderID)
	if err != nil {
		return fmt.Errorf("update order status: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update order status: check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update order status: order %q not found", orderID)
	}

	// Release PIN reservation upon terminal status so the PIN can be safely reused
	if status == contracts.JobStatusCompleted || status == contracts.JobStatusFailedRefundable {
		_, _ = s.db.ExecContext(ctx, s.rebind("DELETE FROM pin_reservations WHERE order_id = ?;"), orderID)
	}

	return nil
}

// RecordWebhook atomically attempts to record a webhook ID.
// Returns (true, nil) if inserted for the first time, (false, nil) if already existed.
func (s *Store) RecordWebhook(ctx context.Context, webhookID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record webhook: store is closed or nil")
	}
	if ctx == nil {
		return false, fmt.Errorf("record webhook: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("record webhook: %w", err)
	}
	if strings.TrimSpace(webhookID) == "" {
		return false, fmt.Errorf("record webhook: webhookID cannot be empty or whitespace-only")
	}

	receivedNS := time.Now().UTC().UnixNano()

	query := `
INSERT INTO payment_webhooks (webhook_id, received_at_ns)
VALUES (?, ?)
ON CONFLICT(webhook_id) DO NOTHING;
`
	res, err := s.db.ExecContext(ctx, s.rebind(query), webhookID, receivedNS)
	if err != nil {
		return false, fmt.Errorf("record webhook: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record webhook: check rows affected: %w", err)
	}

	return rows == 1, nil
}

// UpdateJobProgress validates and atomically persists the latest progress event for a job.
func (s *Store) UpdateJobProgress(ctx context.Context, event contracts.JobProgressEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update job progress: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update job progress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update job progress: %w", err)
	}

	// Validation
	if strings.TrimSpace(event.JobID) == "" {
		return fmt.Errorf("update job progress: event.job_id cannot be empty")
	}
	if strings.TrimSpace(event.KioskID) == "" {
		return fmt.Errorf("update job progress: event.kiosk_id cannot be empty")
	}
	if !isValidJobStatus(event.Status) {
		return fmt.Errorf("update job progress: invalid event status %q", event.Status)
	}
	if event.PagesPrinted < 0 {
		return fmt.Errorf("update job progress: pages_printed must be >= 0, got %d", event.PagesPrinted)
	}
	if event.SheetsPrinted < 0 {
		return fmt.Errorf("update job progress: sheets_printed must be >= 0, got %d", event.SheetsPrinted)
	}
	if event.Timestamp.IsZero() {
		return fmt.Errorf("update job progress: timestamp cannot be zero")
	}

	eventNS := event.Timestamp.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update job progress: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Resolve the canonical child document, including active recovery jobs.
	var (
		orderID            string
		orderKioskID       string
		totalPages         int
		totalSheets        int
		isDocJob           bool
		isRecovery         bool
		parentJobID        string
		recoveryID         string
		recRemainingPages  int
		recRemainingSheets int
		recConfirmedSheets int
	)

	// Check order_documents first
	queryDoc := `
SELECT d.order_id, o.kiosk_id, d.total_pages, d.total_sheets
FROM order_documents d
JOIN orders o ON d.order_id = o.order_id
WHERE d.job_id = ?;
`
	err = tx.QueryRowContext(ctx, s.rebind(queryDoc), event.JobID).Scan(&orderID, &orderKioskID, &totalPages, &totalSheets)
	if err == nil {
		isDocJob = true
	} else if errors.Is(err, sql.ErrNoRows) {
		// Only recovery jobs associated with a canonical child document exist.
		idx := strings.LastIndex(event.JobID, "-rec")
		if idx < 1 {
			return fmt.Errorf("update job progress: unknown child document %q", event.JobID)
		}
		parentJob := event.JobID[:idx]
		seqID, parseErr := strconv.Atoi(event.JobID[idx+4:])
		if parseErr != nil {
			return fmt.Errorf("update job progress: invalid recovery id %q", event.JobID)
		}
		err = tx.QueryRowContext(ctx, s.rebind(`
SELECT r.recovery_id, r.job_id, r.kiosk_id, r.confirmed_sheets_printed,
       r.remaining_pages, r.remaining_sheets, d.total_pages, d.total_sheets, d.order_id
FROM recovery_records r
JOIN order_documents d ON d.job_id = r.job_id
WHERE r.job_id = ? AND r.sequence_id = ?;`), parentJob, seqID).Scan(
			&recoveryID, &parentJobID, &orderKioskID, &recConfirmedSheets,
			&recRemainingPages, &recRemainingSheets, &totalPages, &totalSheets, &orderID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("update job progress: unknown recovery job %q", event.JobID)
		}
		if err != nil {
			return fmt.Errorf("update job progress: lookup recovery document: %w", err)
		}
		isRecovery = true
		isDocJob = true
	} else {
		return fmt.Errorf("update job progress: lookup child document: %w", err)
	}

	// 2. Verify kiosk ID matches
	if orderKioskID != event.KioskID {
		return fmt.Errorf("update job progress: kiosk ID mismatch: event has %q, order belongs to %q", event.KioskID, orderKioskID)
	}

	// Once all child jobs are verified stopped, ignore delayed printer
	// progress that could resurrect the order or mark it completed while a
	// monetary refund is in flight.
	var refundStopped int
	if err := tx.QueryRowContext(ctx, s.rebind(
		"SELECT COUNT(*) FROM payment_refund_requests WHERE order_id = ? AND reason = 'FULFILLMENT_FAILED' AND (safe_stop_verified = 1 OR status = 'PROCESSED')"),
		orderID).Scan(&refundStopped); err != nil {
		return fmt.Errorf("update job progress: query refund safety: %w", err)
	}
	if refundStopped > 0 { return nil }

	// 3. Verify progress bounds
	if isRecovery {
		if event.PagesPrinted > recRemainingPages {
			return fmt.Errorf("update job progress: pages_printed (%d) exceeds recovery remaining_pages (%d)", event.PagesPrinted, recRemainingPages)
		}
		if event.SheetsPrinted > recRemainingSheets {
			return fmt.Errorf("update job progress: sheets_printed (%d) exceeds recovery remaining_sheets (%d)", event.SheetsPrinted, recRemainingSheets)
		}
	} else {
		if event.PagesPrinted > totalPages {
			return fmt.Errorf("update job progress: pages_printed (%d) exceeds total_pages (%d)", event.PagesPrinted, totalPages)
		}
		if event.SheetsPrinted > totalSheets {
			return fmt.Errorf("update job progress: sheets_printed (%d) exceeds total_sheets (%d)", event.SheetsPrinted, totalSheets)
		}
	}

	targetJobID := event.JobID
	pagesPrinted := event.PagesPrinted
	sheetsPrinted := event.SheetsPrinted

	if isRecovery {
		targetJobID = parentJobID
		sheetsPrinted = recConfirmedSheets + event.SheetsPrinted
		if sheetsPrinted > totalSheets {
			sheetsPrinted = totalSheets
		}
		pagesPrinted = (totalPages - recRemainingPages) + event.PagesPrinted
		if pagesPrinted > totalPages {
			pagesPrinted = totalPages
		}
	}

	// 4. Check existing progress timestamp to reject older/stale events
	var existingTimestampNS int64
	err = tx.QueryRowContext(ctx, s.rebind("SELECT timestamp_ns FROM job_progress WHERE job_id = ?;"), targetJobID).Scan(&existingTimestampNS)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("update job progress: query existing progress: %w", err)
	}
	if err == nil && eventNS < existingTimestampNS {
		return fmt.Errorf("update job progress: rejected stale event timestamp (%d < %d)", eventNS, existingTimestampNS)
	}

	// 5. Upsert latest progress row
	upsertProgress := `
INSERT INTO job_progress (job_id, kiosk_id, status, pages_printed, sheets_printed, error_reason, timestamp_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET
    kiosk_id = excluded.kiosk_id,
    status = excluded.status,
    pages_printed = excluded.pages_printed,
    sheets_printed = excluded.sheets_printed,
    error_reason = excluded.error_reason,
    timestamp_ns = excluded.timestamp_ns;
`
	_, err = tx.ExecContext(ctx, s.rebind(upsertProgress),
		targetJobID,
		event.KioskID,
		string(event.Status),
		pagesPrinted,
		sheetsPrinted,
		event.ErrorReason,
		eventNS,
	)
	if err != nil {
		return fmt.Errorf("update job progress: upsert progress: %w", err)
	}

	if isRecovery {
		recStatus := string(event.Status)
		if event.Status == contracts.JobStatusCompleted {
			recStatus = "COMPLETED"
		}
		_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE recovery_records
SET status = ?, updated_at_ns = ?
WHERE recovery_id = ?;
`), recStatus, eventNS, recoveryID)
	}

	// 6. Update document / order status
	if isDocJob {
		_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE order_documents
SET status = ?, updated_at_ns = ?
WHERE job_id = ?;
`), string(event.Status), eventNS, targetJobID)
		if err != nil {
			return fmt.Errorf("update job progress: update order_documents status: %w", err)
		}

		// Check multi-document completion invariant:
		// Order completion requires exactly N persisted jobs (N > 0), all marked COMPLETED. Zero jobs must never count as completed.
		var (
			totalDocs     int
			completedDocs int
			failedDocs    int
			pausedDocs    int
			printingDocs  int
			spoolingDocs  int
		)
		queryCounts := `
SELECT
    COUNT(1),
    COALESCE(SUM(CASE WHEN status = 'COMPLETED' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'FAILED_REFUNDABLE' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'PAUSED_ERROR' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'PRINTING' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'SPOOLING' THEN 1 ELSE 0 END), 0)
FROM order_documents
WHERE order_id = ?;
`
		err = tx.QueryRowContext(ctx, s.rebind(queryCounts), orderID).Scan(
			&totalDocs, &completedDocs, &failedDocs, &pausedDocs, &printingDocs, &spoolingDocs,
		)
		if err != nil {
			return fmt.Errorf("update job progress: query order document counts: %w", err)
		}

		if totalDocs > 0 && completedDocs == totalDocs {
			_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = 'COMPLETED', updated_at_ns = ?
WHERE order_id = ?;
`), eventNS, orderID)
			if err != nil {
				return fmt.Errorf("update job progress: update order completed: %w", err)
			}
			_, _ = tx.ExecContext(ctx, s.rebind("DELETE FROM pin_reservations WHERE order_id = ?;"), orderID)
		} else if failedDocs > 0 {
			_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = 'FAILED_REFUNDABLE', updated_at_ns = ?
WHERE order_id = ?;
`), eventNS, orderID)
		} else if pausedDocs > 0 {
			// Any paused child freezes the entire physical order. No later
			// progress from a different child may make it printable again.
			_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders SET status = 'PAUSED_ERROR', updated_at_ns = ?
WHERE order_id = ? AND status NOT IN ('FAILED_REFUNDABLE', 'COMPLETED');
`), eventNS, orderID)
			if err != nil { return fmt.Errorf("update job progress: pause order: %w", err) }
		} else if printingDocs > 0 {
			_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = 'PRINTING', updated_at_ns = ?
WHERE order_id = ? AND status NOT IN ('COMPLETED','FAILED_REFUNDABLE','PAUSED_ERROR');
`), eventNS, orderID)
		} else if spoolingDocs > 0 {
			_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = 'SPOOLING', updated_at_ns = ?
WHERE order_id = ? AND status NOT IN ('PRINTING','COMPLETED','FAILED_REFUNDABLE','PAUSED_ERROR');
`), eventNS, orderID)
		}
	}

	// 7. Synchronize dispatch intent status with authoritative progress
	if event.Status == contracts.JobStatusCompleted {
		_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET status = 'COMPLETED', updated_at_ns = ?
WHERE job_id = ?;
`), eventNS, event.JobID)
	} else if event.Status == contracts.JobStatusSpooling || event.Status == contracts.JobStatusPrinting {
		_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET status = 'ACKNOWLEDGED', acknowledged_at_ns = ?, updated_at_ns = ?
WHERE job_id = ? AND status IN ('PENDING', 'SENT');
`), eventNS, eventNS, event.JobID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update job progress: commit tx: %w", err)
	}

	return nil
}

// GetJobProgress retrieves the latest recorded progress event for a job.
func (s *Store) GetJobProgress(ctx context.Context, jobID string) (*contracts.JobProgressEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get job progress: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get job progress: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get job progress: %w", err)
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("get job progress: jobID cannot be empty")
	}

	query := `
SELECT job_id, kiosk_id, status, pages_printed, sheets_printed, error_reason, timestamp_ns
FROM job_progress
WHERE job_id = ?;
`
	targetJobID := jobID
	if idx := strings.LastIndex(jobID, "-rec"); idx != -1 {
		pJobID := jobID[:idx]
		if pJobID != "" {
			targetJobID = pJobID
		}
	}

	var (
		resJobID      string
		kioskID       string
		statusStr     string
		pagesPrinted  int
		sheetsPrinted int
		errorReason   string
		timestampNS   int64
	)

	err := s.db.QueryRowContext(ctx, s.rebind(query), targetJobID).Scan(
		&resJobID,
		&kioskID,
		&statusStr,
		&pagesPrinted,
		&sheetsPrinted,
		&errorReason,
		&timestampNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job progress: %w", err)
	}

	event := &contracts.JobProgressEvent{
		JobID:         resJobID,
		KioskID:       kioskID,
		Status:        contracts.JobStatus(statusStr),
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		ErrorReason:   errorReason,
		Timestamp:     time.Unix(0, timestampNS).UTC(),
	}

	return event, nil
}

// GetExpiredOrderIDs returns order IDs whose stored PDF artifacts have exceeded retention.
// - completedThreshold applies only to COMPLETED orders, or FAILED_REFUNDABLE orders with no captured payment or a PROCESSED refund. PAUSED_ERROR never qualifies.
// - pendingThreshold applies to unpaid orders still in PENDING_PAYMENT.
func (s *Store) GetExpiredOrderIDs(ctx context.Context, completedThreshold, pendingThreshold time.Time) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get expired orders: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get expired orders: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get expired orders: %w", err)
	}

	query := `
SELECT o.order_id FROM orders o
WHERE (
    o.updated_at_ns <= ?
    AND (
      (o.status = 'COMPLETED' AND NOT EXISTS (
          SELECT 1 FROM payment_refund_requests rr
          WHERE rr.order_id = o.order_id AND rr.status <> 'PROCESSED'
      ))
      OR (
        o.status = 'FAILED_REFUNDABLE'
        AND (
          NOT EXISTS (SELECT 1 FROM payments p WHERE p.order_id = o.order_id AND p.status IN ('captured', 'CAPTURED'))
          OR EXISTS (SELECT 1 FROM payment_refund_requests rr WHERE rr.order_id = o.order_id AND rr.reason = 'FULFILLMENT_FAILED' AND rr.status = 'PROCESSED')
        )
      )
    )
    AND NOT EXISTS (
      SELECT 1 FROM payment_refund_requests rr
      WHERE rr.order_id = o.order_id AND rr.status <> 'PROCESSED'
    )
  )
  OR (
    o.status = 'PENDING_PAYMENT' AND o.created_at_ns <= ?
    -- Underpayments are captured money despite an unpaid order state.
    -- Never lose a provider/refund obligation through unpaid-PIN cleanup.
    AND NOT EXISTS (
      SELECT 1 FROM payments p WHERE p.order_id = o.order_id
      AND LOWER(p.status) IN ('captured','paid')
    )
    AND NOT EXISTS (
      SELECT 1 FROM payment_refund_requests rr WHERE rr.order_id = o.order_id
      AND rr.status <> 'PROCESSED'
    )
  );
`
	rows, err := s.db.QueryContext(ctx, s.rebind(query), completedThreshold.UnixNano(), pendingThreshold.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("query expired order ids: %w", err)
	}
	defer rows.Close()

	var orderIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired order id: %w", err)
		}
		orderIDs = append(orderIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows expired order ids: %w", err)
	}
	return orderIDs, nil
}

func isValidJobStatus(s contracts.JobStatus) bool {
	switch s {
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

func isValidDuplex(d contracts.DuplexMode) bool {
	switch d {
	case contracts.DuplexSimplex,
		contracts.DuplexLongEdge,
		contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
}

func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b), nil
}

// GetCommittedPaperSheets returns the total number of physical paper sheets currently committed
// on the given kiosk by unexpired unpaid PIN orders and active/in-flight paid orders,
// optionally excluding a specific orderID (e.g. the order currently being checked).
func (s *Store) GetCommittedPaperSheets(ctx context.Context, kioskID, excludeOrderID string, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store not initialized")
	}
	return s.getCommittedPaperSheetsTx(ctx, s.db, kioskID, excludeOrderID, now.UTC().UnixNano())
}

func (s *Store) getCommittedPaperSheetsTx(ctx context.Context, q queryable, kioskID, excludeOrderID string, nowNS int64) (int, error) {
	kioskID = strings.TrimSpace(kioskID)
	excludeOrderID = strings.TrimSpace(excludeOrderID)
	if kioskID == "" {
		return 0, nil
	}

	query := s.rebind(`
SELECT COALESCE(SUM(c.committed_sheets), 0)
FROM (
    -- Every pending order with an unexpired PIN already owns its paper.
    -- Session creation does not reserve additional sheets.
    SELECT o.total_sheets AS committed_sheets
    FROM orders o
    WHERE o.kiosk_id = ?
      AND o.status = 'PENDING_PAYMENT'
      AND o.pin_expires_at_ns > ?
      AND (? = '' OR o.order_id != ?)

    UNION ALL

    -- For paid/in-flight orders, subtract progress across every child document.
    SELECT CASE WHEN paid.remaining_sheets > 0 THEN paid.remaining_sheets ELSE 0 END
    FROM (
        SELECT o.total_sheets - COALESCE(
            (SELECT SUM(jp.sheets_printed)
             FROM order_documents od
             JOIN job_progress jp ON jp.job_id = od.job_id
             WHERE od.order_id = o.order_id), 0
        ) AS remaining_sheets
        FROM orders o
        WHERE o.kiosk_id = ?
          AND o.status IN ('PAID_WAITING', 'DISPATCHED', 'SPOOLING', 'PRINTING', 'PAUSED_ERROR')
          AND (? = '' OR o.order_id != ?)
    ) paid
) c;
`)

	var totalCommitted int
	err := q.QueryRowContext(ctx, query,
		kioskID, nowNS, excludeOrderID, excludeOrderID,
		kioskID, excludeOrderID, excludeOrderID,
	).Scan(&totalCommitted)
	if err != nil {
		return 0, fmt.Errorf("get committed paper sheets: %w", err)
	}
	if totalCommitted < 0 {
		totalCommitted = 0
	}
	return totalCommitted, nil
}

// CreateOrGetKioskSession claims an order for a kiosk transaction session atomically without paper checks.
func (s *Store) CreateOrGetKioskSession(ctx context.Context, kioskID, orderID string, sessionExpiry time.Duration, now time.Time) (*KioskSession, error) {
	return s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, -1, sessionExpiry, now)
}

// CreateOrGetKioskSessionWithPaperCheck claims an order for a kiosk transaction session atomically,
// validating that the kiosk has sufficient available paper before creating the session.
// availableStock is the current estimated physical stock from the kiosk.
// If availableStock < 0, paper check is skipped (unconstrained).
func (s *Store) CreateOrGetKioskSessionWithPaperCheck(ctx context.Context, kioskID, orderID string, availableStock int, sessionExpiry time.Duration, now time.Time) (*KioskSession, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	kioskID = strings.TrimSpace(kioskID)
	orderID = strings.TrimSpace(orderID)
	if kioskID == "" || orderID == "" {
		return nil, errors.New("kioskID and orderID cannot be empty")
	}
	if sessionExpiry <= 0 {
		sessionExpiry = 5 * time.Minute
	}

	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create kiosk session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize paper commitment checks and session creation per kiosk in PostgreSQL
	if s.dialect == "postgres" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(22615, hashtext($1));", kioskID); err != nil {
			return nil, fmt.Errorf("create kiosk session: acquire kiosk advisory lock: %w", err)
		}
	}

	// 1. Verify order existence, kiosk ownership, status, and expiry
	var (
		orderKioskID   string
		statusStr      string
		pricePaise       int64
		pinExpiresAtNS   int64
		orderTotalSheets int
	)
	queryOrder := s.rebind(`
SELECT kiosk_id, status, price_paise, pin_expires_at_ns, total_sheets
FROM orders
WHERE order_id = ?;
`)
	err = tx.QueryRowContext(ctx, queryOrder, orderID).Scan(&orderKioskID, &statusStr, &pricePaise, &pinExpiresAtNS, &orderTotalSheets)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("create kiosk session: query order: %w", err)
	}

	if statusStr != string(contracts.JobStatusPendingPayment) && statusStr != string(contracts.JobStatusPaidWaiting) {
		return nil, ErrOrderNotPending
	}

	if pinExpiresAtNS > 0 && nowNS >= pinExpiresAtNS {
		return nil, ErrOrderExpired
	}

	// 2. Check for existing PAYMENT_PENDING sessions for this order
	queryActive := s.rebind(`
SELECT session_id, kiosk_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns
FROM kiosk_sessions
WHERE order_id = ? AND status = 'PAYMENT_PENDING';
`)
	rows, err := tx.QueryContext(ctx, queryActive, orderID)
	if err != nil {
		return nil, fmt.Errorf("create kiosk session: query active sessions: %w", err)
	}
	defer rows.Close()

	var existingActive *KioskSession
	var expiredSessionIDs []string

	for rows.Next() {
		var (
			sessID, sessKiosk, sessStatus        string
			sessPrice, expNS, createNS, updateNS int64
		)
		if err := rows.Scan(&sessID, &sessKiosk, &sessStatus, &sessPrice, &expNS, &createNS, &updateNS); err != nil {
			return nil, fmt.Errorf("create kiosk session: scan active session: %w", err)
		}
		if expNS <= nowNS {
			expiredSessionIDs = append(expiredSessionIDs, sessID)
		} else {
			existingActive = &KioskSession{
				SessionID:  sessID,
				KioskID:    sessKiosk,
				OrderID:    orderID,
				Status:     KioskSessionStatus(sessStatus),
				PricePaise: sessPrice,
				ExpiresAt:  time.Unix(0, expNS).UTC(),
				CreatedAt:  time.Unix(0, createNS).UTC(),
				UpdatedAt:  time.Unix(0, updateNS).UTC(),
			}
		}
	}
	_ = rows.Close()

	// Prune expired sessions
	for _, expID := range expiredSessionIDs {
		_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE kiosk_sessions SET status = 'EXPIRED', updated_at_ns = ? WHERE session_id = ?;`), nowNS, expID)
	}

	// If unexpired active session exists
	if existingActive != nil {
		if existingActive.KioskID == kioskID {
			// Idempotent: return active session
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("create kiosk session: commit tx: %w", err)
			}
			return existingActive, nil
		}
		// Claimed by another kiosk/session!
		return nil, ErrOrderAlreadyClaimed
	}

	if orderKioskID != kioskID {
		return nil, ErrKioskMismatch
	}

	// 3. If paper checking is enabled (availableStock >= 0), verify sufficient available sheets
	if availableStock >= 0 {
		// Orders own the authoritative total for all documents.
		if orderTotalSheets <= 0 {
			return nil, fmt.Errorf("create kiosk session: invalid order total_sheets %d", orderTotalSheets)
		}

		committedSheets, err := s.getCommittedPaperSheetsTx(ctx, tx, kioskID, orderID, nowNS)
		if err != nil {
			return nil, fmt.Errorf("create kiosk session: check paper commitments: %w", err)
		}
		effectiveAvailable := availableStock - committedSheets
		if effectiveAvailable < orderTotalSheets {
			return nil, ErrInsufficientPaper
		}
	}

	// 4. Create new session
	sessID, err := generateSessionID()
	if err != nil {
		return nil, fmt.Errorf("create kiosk session: generate session id: %w", err)
	}

	expiresAt := now.Add(sessionExpiry)
	if pinExpiresAtNS > 0 {
		pinExp := time.Unix(0, pinExpiresAtNS).UTC()
		if expiresAt.After(pinExp) {
			expiresAt = pinExp
		}
	}
	expiresAtNS := expiresAt.UnixNano()

	insertQuery := s.rebind(`
INSERT INTO kiosk_sessions (session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, 'PAYMENT_PENDING', ?, ?, ?, ?);
`)
	_, err = tx.ExecContext(ctx, insertQuery, sessID, kioskID, orderID, pricePaise, expiresAtNS, nowNS, nowNS)
	if err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "unique") || strings.Contains(errStr, "23505") {
			// Uniqueness conflict on idx_kiosk_sessions_active_order:
			// Another transaction created an active session for this order concurrently.
			_ = tx.Rollback()

			var (
				existingID, existingKiosk, existingStatus string
				existingPrice, expNS, createNS, updateNS   int64
			)
			queryExisting := s.rebind(`
SELECT session_id, kiosk_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns
FROM kiosk_sessions
WHERE order_id = ? AND status = 'PAYMENT_PENDING';
`)
			qErr := s.db.QueryRowContext(ctx, queryExisting, orderID).Scan(
				&existingID, &existingKiosk, &existingStatus, &existingPrice, &expNS, &createNS, &updateNS,
			)
			if qErr == nil {
				// Only return the session if it belongs to the same authorized kiosk and has not expired
				if existingKiosk == kioskID && expNS > nowNS {
					return &KioskSession{
						SessionID:  existingID,
						KioskID:    existingKiosk,
						OrderID:    orderID,
						Status:     KioskSessionStatus(existingStatus),
						PricePaise: existingPrice,
						ExpiresAt:  time.Unix(0, expNS).UTC(),
						CreatedAt:  time.Unix(0, createNS).UTC(),
						UpdatedAt:  time.Unix(0, updateNS).UTC(),
					}, nil
				}
				// Belongs to a different kiosk or was claimed by another customer
				return nil, ErrOrderAlreadyClaimed
			}
			return nil, ErrOrderAlreadyClaimed
		}
		return nil, fmt.Errorf("create kiosk session: insert session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create kiosk session: commit: %w", err)
	}

	return &KioskSession{
		SessionID:  sessID,
		KioskID:    kioskID,
		OrderID:    orderID,
		Status:     KioskSessionPaymentPending,
		PricePaise: pricePaise,
		ExpiresAt:  expiresAt,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}, nil
}

// GetKioskSession retrieves a session by its sessionID and marks it EXPIRED if past expiry.
func (s *Store) GetKioskSession(ctx context.Context, sessionID string, now time.Time) (*KioskSession, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	nowNS := now.UTC().UnixNano()
	query := s.rebind(`
SELECT session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns
FROM kiosk_sessions
WHERE session_id = ?;
`)
	var (
		sessID, sessKiosk, orderID, sessStatus string
		pricePaise, expNS, createNS, updateNS  int64
	)
	err := s.db.QueryRowContext(ctx, query, sessionID).Scan(&sessID, &sessKiosk, &orderID, &sessStatus, &pricePaise, &expNS, &createNS, &updateNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("get kiosk session: %w", err)
	}

	status := KioskSessionStatus(sessStatus)
	if status == KioskSessionPaymentPending && nowNS >= expNS {
		status = KioskSessionExpired
		_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE kiosk_sessions SET status = 'EXPIRED', updated_at_ns = ? WHERE session_id = ?;`), nowNS, sessionID)
	}

	return &KioskSession{
		SessionID:  sessID,
		KioskID:    sessKiosk,
		OrderID:    orderID,
		Status:     status,
		PricePaise: pricePaise,
		ExpiresAt:  time.Unix(0, expNS).UTC(),
		CreatedAt:  time.Unix(0, createNS).UTC(),
		UpdatedAt:  time.Unix(0, updateNS).UTC(),
	}, nil
}

// GetActiveKioskSession retrieves an unexpired PAYMENT_PENDING session for a given kiosk and order.
func (s *Store) GetActiveKioskSession(ctx context.Context, kioskID, orderID string, now time.Time) (*KioskSession, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	nowNS := now.UTC().UnixNano()
	query := s.rebind(`
SELECT session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns
FROM kiosk_sessions
WHERE kiosk_id = ? AND order_id = ? AND status = 'PAYMENT_PENDING' AND expires_at_ns > ?;
`)
	var (
		sessID, sessKiosk, ordID, sessStatus  string
		pricePaise, expNS, createNS, updateNS int64
	)
	err := s.db.QueryRowContext(ctx, query, kioskID, orderID, nowNS).Scan(&sessID, &sessKiosk, &ordID, &sessStatus, &pricePaise, &expNS, &createNS, &updateNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get active kiosk session: %w", err)
	}

	return &KioskSession{
		SessionID:  sessID,
		KioskID:    sessKiosk,
		OrderID:    ordID,
		Status:     KioskSessionStatus(sessStatus),
		PricePaise: pricePaise,
		ExpiresAt:  time.Unix(0, expNS).UTC(),
		CreatedAt:  time.Unix(0, createNS).UTC(),
		UpdatedAt:  time.Unix(0, updateNS).UTC(),
	}, nil
}

// CancelKioskSession cancels an active session for the given kiosk.
func (s *Store) CancelKioskSession(ctx context.Context, sessionID, kioskID string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	kioskID = strings.TrimSpace(kioskID)
	sessionID = strings.TrimSpace(sessionID)
	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cancel kiosk session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if s.dialect == "postgres" && kioskID != "" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(22615, hashtext($1));", kioskID); err != nil {
			return fmt.Errorf("cancel kiosk session: acquire kiosk advisory lock: %w", err)
		}
	}

	query := s.rebind(`
UPDATE kiosk_sessions
SET status = 'CANCELED', updated_at_ns = ?
WHERE session_id = ? AND kiosk_id = ? AND status = 'PAYMENT_PENDING';
`)
	res, err := tx.ExecContext(ctx, query, nowNS, sessionID, kioskID)
	if err != nil {
		return fmt.Errorf("cancel kiosk session: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil { return fmt.Errorf("cancel kiosk session: rows affected: %w", err) }
	if rows == 0 {
		var currentStatus string
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT status FROM kiosk_sessions WHERE session_id = ? AND kiosk_id = ?;`), sessionID, kioskID).Scan(&currentStatus)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSessionNotFound
			}
			return err
		}
		// After payment capture, the session is COMPLETED. A stale kiosk
		// cancellation/timer must not revoke durable paid dispatch intents.
		// A repeat cancel of an already-terminal session is idempotent.
		return tx.Commit()
	}

	// Only a successfully canceled PAYMENT_PENDING session may revoke its
	// still-unacknowledged dispatch intents. Capture completion and cancel
	// serialize on the session row; whichever wins determines authorization.
	// Cancel any pending or unacknowledged dispatch intents for this session
	_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET status = 'CANCELED', updated_at_ns = ?
WHERE session_id = ? AND status IN ('PENDING', 'SENT');
`), nowNS, sessionID)

	// Revert order status from DISPATCHED to PAID_WAITING if not already durably acknowledged
	_, _ = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = (SELECT order_id FROM kiosk_sessions WHERE session_id = ?)
  AND status = ?
  AND order_id NOT IN (SELECT order_id FROM dispatch_intents WHERE status = 'ACKNOWLEDGED');
`), string(contracts.JobStatusPaidWaiting), nowNS, sessionID, string(contracts.JobStatusDispatched))

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cancel kiosk session: commit tx: %w", err)
	}
	return nil
}

// CompleteKioskSession marks any active PAYMENT_PENDING session for an order as COMPLETED.
func (s *Store) CompleteKioskSession(ctx context.Context, orderID string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	orderID = strings.TrimSpace(orderID)
	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete kiosk session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if s.dialect == "postgres" {
		var kioskID string
		_ = tx.QueryRowContext(ctx, "SELECT kiosk_id FROM kiosk_sessions WHERE order_id = $1 AND status = 'PAYMENT_PENDING';", orderID).Scan(&kioskID)
		if kioskID != "" {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(22615, hashtext($1));", kioskID); err != nil {
				return fmt.Errorf("complete kiosk session: acquire advisory lock: %w", err)
			}
		}
	}

	query := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE order_id = ? AND status = 'PAYMENT_PENDING';
`)
	res, err := tx.ExecContext(ctx, query, nowNS, orderID)
	if err != nil {
		return fmt.Errorf("complete kiosk session: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrSessionNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete kiosk session: commit tx: %w", err)
	}
	return nil
}

// DetectDuplicateActiveSessions queries the database for any orders that currently have
// more than one active (PAYMENT_PENDING) session. It returns the offending order IDs.
// This is used for preflight migration validation and operational health monitoring.
func (s *Store) DetectDuplicateActiveSessions(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	query := s.rebind(`
SELECT order_id
FROM kiosk_sessions
WHERE status = 'PAYMENT_PENDING'
GROUP BY order_id
HAVING COUNT(1) > 1;
`)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("detect duplicate active sessions: %w", err)
	}
	defer rows.Close()

	var dups []string
	for rows.Next() {
		var orderID string
		if err := rows.Scan(&orderID); err != nil {
			return nil, fmt.Errorf("scan duplicate order id: %w", err)
		}
		dups = append(dups, orderID)
	}
	return dups, rows.Err()
}

// EnsureActiveSessionConstraint verifies preflight conditions and creates the
// idx_kiosk_sessions_active_order partial unique index.
// This is the supported mechanism for fresh development, CI, and disposable test databases.
// In production PostgreSQL, the explicit standalone migration script
// deploy/supabase/migrations/20261113_active_kiosk_sessions_unique.sql must be
// applied via CREATE INDEX CONCURRENTLY outside transaction blocks instead of application startup.
func (s *Store) EnsureActiveSessionConstraint(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	// Fail-closed preflight check:
	dups, err := s.DetectDuplicateActiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("ensure active session constraint preflight failed: %w", err)
	}
	if len(dups) > 0 {
		return fmt.Errorf("ensure active session constraint preflight failed: found %d orders with duplicate active PAYMENT_PENDING sessions; manual operator resolution required", len(dups))
	}
	stmt := "CREATE UNIQUE INDEX IF NOT EXISTS idx_kiosk_sessions_active_order ON kiosk_sessions (order_id) WHERE status = 'PAYMENT_PENDING';"
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("apply active session unique constraint: %w", err)
	}
	return nil
}

// PaymentRecord encapsulates trusted payment metadata.
type PaymentRecord struct {
	PaymentID   string
	OrderID     string
	KioskID     string
	SessionID   string
	AmountPaise int64
	Provider    string
	Status      string
}

// PaymentCaptureResult contains the atomic state transition outcome and dispatch eligibility.
type PaymentCaptureResult struct {
	OrderStatus      contracts.JobStatus
	DispatchEligible bool
	AlreadyProcessed bool
	ExcessCapture    bool
	ActiveSessionID  string
	PricePaise       int64
	KioskID          string
	Command          *contracts.PrintJobCommand
	Commands         []contracts.PrintJobCommand
}

// IsVerifiedCapturedStatus reports whether a payment status string represents
// verified successful capture (e.g. "captured" or "paid"). Merely "authorized",
// "pending", or "failed" payments are NOT sufficient for physical printing.
func IsVerifiedCapturedStatus(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "captured" || s == "paid"
}

// CapturePaymentAndCheckDispatch atomically records payment, transitions order status to PAID_WAITING,
// and determines whether an active unexpired kiosk session authorizes physical print dispatch.
// If an active session authorizes dispatch, the session is completed, order transitions to DISPATCHED,
// and DispatchEligible is true.
// If no valid active session exists (canceled, expired, or absent), order remains PAID_WAITING,
// payment is preserved, and DispatchEligible is false.
func (s *Store) CapturePaymentAndCheckDispatch(ctx context.Context, p PaymentRecord, now time.Time) (*PaymentCaptureResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	p.OrderID = strings.TrimSpace(p.OrderID)
	p.PaymentID = strings.TrimSpace(p.PaymentID)
	p.Provider = strings.TrimSpace(p.Provider)
	p.Status = strings.TrimSpace(p.Status)

	if p.OrderID == "" || p.PaymentID == "" {
		return nil, errors.New("orderID and paymentID cannot be empty")
	}
	if p.Provider == "" {
		return nil, errors.New("payment provider identity cannot be empty")
	}
	if p.AmountPaise <= 0 {
		return nil, fmt.Errorf("payment amount must be greater than zero, got %d", p.AmountPaise)
	}
	if !IsVerifiedCapturedStatus(p.Status) {
		return nil, fmt.Errorf("payment %s status %q is not verified captured: physical printing requires verified successful capture", p.PaymentID, p.Status)
	}
	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("capture payment: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize distinct captures per order. The funding decision, capture row,
	// and any surplus refund obligation must commit as one transaction.
	if s.dialect == "postgres" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(22616, hashtext($1));", p.OrderID); err != nil {
			return nil, fmt.Errorf("capture payment: acquire funding lock: %w", err)
		}
	}

	// 1. Check if payment record already exists
	var (
		existingOrderID  string
		existingAmount   int64
		existingStatus   string
		existingProvider string
	)
	checkPayQuery := s.rebind(`SELECT order_id, amount_paise, status, provider FROM payments WHERE payment_id = ?;`)
	err = tx.QueryRowContext(ctx, checkPayQuery, p.PaymentID).Scan(&existingOrderID, &existingAmount, &existingStatus, &existingProvider)
	if err == nil {
		// Already recorded: verify it belongs to the same order and matching payment details
		_ = tx.Rollback()
		if existingOrderID != p.OrderID {
			return nil, fmt.Errorf("payment %s already exists for conflicting order %s (cannot be reused for order %s)", p.PaymentID, existingOrderID, p.OrderID)
		}
		if existingAmount <= 0 || p.AmountPaise != existingAmount {
			return nil, fmt.Errorf("payment %s amount mismatch for order %s: existing %d paise vs requested %d paise", p.PaymentID, p.OrderID, existingAmount, p.AmountPaise)
		}
		if existingProvider == "" || p.Provider != existingProvider {
			return nil, fmt.Errorf("payment %s provider mismatch for order %s: existing %s vs requested %s", p.PaymentID, p.OrderID, existingProvider, p.Provider)
		}
		if !IsVerifiedCapturedStatus(existingStatus) {
			return nil, fmt.Errorf("payment %s existing status %q is not verified captured", p.PaymentID, existingStatus)
		}

		order, oErr := s.GetOrder(ctx, p.OrderID)
		if oErr != nil {
			return nil, fmt.Errorf("capture payment: duplicate payment fetch order %s: %w", p.OrderID, oErr)
		}
		if order == nil {
			return nil, fmt.Errorf("capture payment: duplicate payment order %s not found: %w", p.OrderID, ErrOrderNotFound)
		}
		var cmds []contracts.PrintJobCommand
		if len(order.Documents) > 0 {
			cmds = make([]contracts.PrintJobCommand, len(order.Documents))
			for i, doc := range order.Documents {
				cmds[i] = doc.Command(order.Command.KioskID, order.Command.DisplayCode, len(order.Documents))
			}
		} else {
			cmds = []contracts.PrintJobCommand{order.Command}
		}
		return &PaymentCaptureResult{
			OrderStatus:      order.Status,
			AlreadyProcessed: true,
			DispatchEligible: false,
			PricePaise:       order.PricePaise,
			KioskID:          order.Command.KioskID,
			Command:          &order.Command,
			Commands:         cmds,
		}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("capture payment: check existing payment: %w", err)
	}

	// 2. Query order
	var (
		orderKioskID string
		statusStr    string
		pricePaise   int64
		orderJobID   string
	)
	queryOrder := s.rebind(`
SELECT kiosk_id, status, price_paise, job_id
FROM orders
WHERE order_id = ?;
`)
	err = tx.QueryRowContext(ctx, queryOrder, p.OrderID).Scan(&orderKioskID, &statusStr, &pricePaise, &orderJobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("capture payment: query order: %w", err)
	}

	// 3. A genuine capture with the wrong amount is still real money.
	// It cannot fund printing, but must have a durable refund obligation.
	if p.AmountPaise != pricePaise {
		if err := s.ensureRefundableOrderSnapshot(ctx, tx, p.OrderID); err != nil { return nil, err }
		if err := s.recordExcessCaptureInTx(ctx, tx, p, orderKioskID, nowNS); err != nil {
			return nil, fmt.Errorf("capture payment: refund incorrect amount: %w", err)
		}
		if err := tx.Commit(); err != nil { return nil, fmt.Errorf("capture payment: commit incorrect amount: %w", err) }
		return &PaymentCaptureResult{
			OrderStatus: contracts.JobStatus(statusStr),
			AlreadyProcessed: true,
			ExcessCapture: true,
			KioskID: orderKioskID,
			PricePaise: pricePaise,
		}, nil
	}

	currentStatus := contracts.JobStatus(statusStr)

	// Precisely one captured payment may fund a print order. Historic captures
	// are preferred if this row has not been backfilled yet. New distinct
	// captures on dispatched/terminal orders are always surplus.
	var fundingPaymentID string
	err = tx.QueryRowContext(ctx, s.rebind(
		"SELECT payment_id FROM order_funding_payments WHERE order_id = ?"), p.OrderID).Scan(&fundingPaymentID)
	if errors.Is(err, sql.ErrNoRows) {
		if currentStatus == contracts.JobStatusPendingPayment || currentStatus == contracts.JobStatusPaidWaiting {
			var earlierID string
			err = tx.QueryRowContext(ctx, s.rebind(`
SELECT payment_id FROM payments WHERE order_id = ?
AND LOWER(status) IN ('captured','paid') AND amount_paise = ?
ORDER BY created_at_ns ASC, payment_id ASC LIMIT 1;`),
				p.OrderID, pricePaise).Scan(&earlierID)
			if errors.Is(err, sql.ErrNoRows) {
				earlierID = p.PaymentID
			} else if err != nil {
				return nil, fmt.Errorf("capture payment: find existing funding: %w", err)
			}
			_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO order_funding_payments(order_id,payment_id,created_at_ns)
VALUES(?,?,?) ON CONFLICT(order_id) DO NOTHING;`), p.OrderID, earlierID, nowNS)
			if err != nil { return nil, fmt.Errorf("capture payment: allocate funding: %w", err) }
			err = tx.QueryRowContext(ctx, s.rebind(
				"SELECT payment_id FROM order_funding_payments WHERE order_id = ?"), p.OrderID).Scan(&fundingPaymentID)
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("capture payment: determine funding: %w", err)
	}
	if fundingPaymentID != p.PaymentID {
		// Unlike webhook replay, this is different money. Never send a new job.
		// Persist the payment and its independently refundable obligation together.
		if err := s.ensureRefundableOrderSnapshot(ctx, tx, p.OrderID); err != nil { return nil, err }
		if err := s.recordExcessCaptureInTx(ctx, tx, p, orderKioskID, nowNS); err != nil {
			return nil, fmt.Errorf("capture payment: record surplus: %w", err)
		}
		if err := tx.Commit(); err != nil { return nil, fmt.Errorf("capture payment: commit surplus: %w", err) }
		return &PaymentCaptureResult{
			OrderStatus: currentStatus,
			DispatchEligible: false,
			AlreadyProcessed: true,
			ExcessCapture: true,
			PricePaise: pricePaise,
			KioskID: orderKioskID,
		}, nil
	}

	// 4. Check if order is already terminal or dispatched
	if currentStatus == contracts.JobStatusCompleted ||
		currentStatus == contracts.JobStatusPrinting ||
		currentStatus == contracts.JobStatusSpooling ||
		currentStatus == contracts.JobStatusDispatched {
		insertPay := s.rebind(`
INSERT INTO payments (payment_id, order_id, kiosk_id, session_id, amount_paise, status, provider, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
`)
		resPay, err := tx.ExecContext(ctx, insertPay, p.PaymentID, p.OrderID, orderKioskID, p.SessionID, pricePaise, p.Status, p.Provider, nowNS)
		if err != nil {
			_ = tx.Rollback()
			var (
				cOrderID  string
				cAmount   int64
				cStatus   string
				cProvider string
			)
			qCheck := s.rebind(`SELECT order_id, amount_paise, status, provider FROM payments WHERE payment_id = ?;`)
			if sErr := s.db.QueryRowContext(ctx, qCheck, p.PaymentID).Scan(&cOrderID, &cAmount, &cStatus, &cProvider); sErr == nil {
				if cOrderID == p.OrderID && cAmount == pricePaise && cProvider == p.Provider && IsVerifiedCapturedStatus(cStatus) {
					order, oErr := s.GetOrder(ctx, p.OrderID)
					if oErr != nil {
						return nil, fmt.Errorf("capture payment: conflict recovery fetch order %s: %w", p.OrderID, oErr)
					}
					if order == nil {
						return nil, fmt.Errorf("capture payment: conflict recovery order %s not found: %w", p.OrderID, ErrOrderNotFound)
					}
					var cmds []contracts.PrintJobCommand
					if len(order.Documents) > 0 {
						cmds = make([]contracts.PrintJobCommand, len(order.Documents))
						for i, doc := range order.Documents {
							cmds[i] = doc.Command(orderKioskID, order.Command.DisplayCode, len(order.Documents))
						}
					} else {
						cmds = []contracts.PrintJobCommand{order.Command}
					}
					return &PaymentCaptureResult{
						OrderStatus:      order.Status,
						DispatchEligible: false,
						AlreadyProcessed: true,
						PricePaise:       pricePaise,
						KioskID:          orderKioskID,
						Command:          &order.Command,
						Commands:         cmds,
					}, nil
				}
			}
			return nil, fmt.Errorf("capture payment: insert payment record (already dispatched): %w", err)
		}
		if _, err := resPay.RowsAffected(); err != nil {
			return nil, fmt.Errorf("capture payment: insert payment rows affected (already dispatched): %w", err)
		}

		// Insert into payment_webhooks within the same atomic transaction
		insertWebhook := s.rebind(`
INSERT INTO payment_webhooks (webhook_id, received_at_ns)
VALUES (?, ?)
ON CONFLICT(webhook_id) DO NOTHING;
`)
		if _, err := tx.ExecContext(ctx, insertWebhook, p.PaymentID, nowNS); err != nil {
			return nil, fmt.Errorf("capture payment: insert payment webhook (already dispatched): %w", err)
		}

		// Update matching payment_attempts to PAID
		updateAtt := s.rebind(`
UPDATE payment_attempts
SET status = 'PAID', provider_payment_id = ?, updated_at_ns = ?
WHERE order_id = ? AND status = 'CREATED';
`)
		resAtt, err := tx.ExecContext(ctx, updateAtt, p.PaymentID, nowNS, p.OrderID)
		if err != nil {
			return nil, fmt.Errorf("capture payment: update payment attempt (already dispatched): %w", err)
		}
		if _, err := resAtt.RowsAffected(); err != nil {
			return nil, fmt.Errorf("capture payment: update payment attempt rows affected (already dispatched): %w", err)
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("capture payment: commit tx: %w", err)
		}
		order, oErr := s.GetOrder(ctx, p.OrderID)
		if oErr != nil {
			return nil, fmt.Errorf("capture payment: fetch committed order %s: %w", p.OrderID, oErr)
		}
		if order == nil {
			return nil, fmt.Errorf("capture payment: committed order %s not found: %w", p.OrderID, ErrOrderNotFound)
		}
		var cmds []contracts.PrintJobCommand
		if len(order.Documents) > 0 {
			cmds = make([]contracts.PrintJobCommand, len(order.Documents))
			for i, doc := range order.Documents {
				cmds[i] = doc.Command(orderKioskID, order.Command.DisplayCode, len(order.Documents))
			}
		} else {
			cmds = []contracts.PrintJobCommand{order.Command}
		}
		return &PaymentCaptureResult{
			OrderStatus:      currentStatus,
			DispatchEligible: false,
			AlreadyProcessed: true,
			PricePaise:       pricePaise,
			KioskID:          orderKioskID,
			Command:          &order.Command,
			Commands:         cmds,
		}, nil
	}

	// 5. Transition order to PAID_WAITING atomically if currently PENDING_PAYMENT
	if currentStatus == contracts.JobStatusPendingPayment {
		updateOrder := s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ? AND status = ?;
`)
		res, err := tx.ExecContext(ctx, updateOrder, string(contracts.JobStatusPaidWaiting), nowNS, p.OrderID, string(contracts.JobStatusPendingPayment))
		if err != nil {
			return nil, fmt.Errorf("capture payment: update order status: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("capture payment: update order status rows affected: %w", err)
		}
		if rows == 0 {
			var newStatus string
			err = tx.QueryRowContext(ctx, s.rebind(`SELECT status FROM orders WHERE order_id = ?;`), p.OrderID).Scan(&newStatus)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, ErrOrderNotFound
				}
				return nil, fmt.Errorf("capture payment: query fallback order status: %w", err)
			}
			currentStatus = contracts.JobStatus(newStatus)
		} else {
			currentStatus = contracts.JobStatusPaidWaiting
		}
	}

	// Capture promotes a still-valid customer PIN into a paid fulfillment
	// entitlement. The five-minute unpaid deadline must not terminate a paid order.
	// A genuinely expired PIN is never resurrected (it may have been reused).
	if err := s.extendPaidPINInTx(ctx, tx, p.OrderID, nowNS); err != nil {
		return nil, fmt.Errorf("capture payment: extend paid PIN: %w", err)
	}

	// 6. Check for active unexpired session on orderKioskID
	querySession := s.rebind(`
SELECT session_id, kiosk_id, status, expires_at_ns
FROM kiosk_sessions
WHERE order_id = ? AND kiosk_id = ? AND status = 'PAYMENT_PENDING' AND expires_at_ns > ?;
`)
	var (
		sessID, sessKiosk, sessStatus string
		sessExpNS                     int64
	)
	sessionFound := false
	err = tx.QueryRowContext(ctx, querySession, p.OrderID, orderKioskID, nowNS).Scan(&sessID, &sessKiosk, &sessStatus, &sessExpNS)
	if err == nil {
		sessionFound = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("capture payment: query active session: %w", err)
	}

	dispatchEligible := false
	activeSessionID := ""

	// Physical presence check: only dispatch if valid active session exists and session matches
	if sessionFound {
		if p.SessionID == "" || p.SessionID != sessID {
			dispatchEligible = false
		} else {
			var docJobIDs []string
			rowsDocs, err := tx.QueryContext(ctx, s.rebind(`SELECT job_id FROM order_documents WHERE order_id = ? ORDER BY document_index ASC;`), p.OrderID)
			if err != nil {
				return nil, fmt.Errorf("capture payment: query order_documents: %w", err)
			}
			for rowsDocs.Next() {
				var jID string
				if sErr := rowsDocs.Scan(&jID); sErr != nil {
					_ = rowsDocs.Close()
					return nil, fmt.Errorf("capture payment: scan order_documents: %w", sErr)
				}
				docJobIDs = append(docJobIDs, jID)
			}
			if err := rowsDocs.Err(); err != nil {
				_ = rowsDocs.Close()
				return nil, fmt.Errorf("capture payment: iterate order_documents: %w", err)
			}
			_ = rowsDocs.Close()

			if len(docJobIDs) == 0 {
				docJobIDs = []string{orderJobID}
			}

			updateSession := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE session_id = ? AND status = 'PAYMENT_PENDING';
`)
			resSess, err := tx.ExecContext(ctx, updateSession, nowNS, sessID)
			if err != nil {
				return nil, fmt.Errorf("capture payment: complete session: %w", err)
			}
			rowsSess, err := resSess.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("capture payment: complete session rows affected: %w", err)
			}
			if rowsSess > 0 {
				updateDispatched := s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ? AND status = ?;
`)
				resOrd, err := tx.ExecContext(ctx, updateDispatched, string(contracts.JobStatusDispatched), nowNS, p.OrderID, string(contracts.JobStatusPaidWaiting))
				if err != nil {
					return nil, fmt.Errorf("capture payment: update order dispatched: %w", err)
				}
				rowsOrd, err := resOrd.RowsAffected()
				if err != nil {
					return nil, fmt.Errorf("capture payment: update order dispatched rows affected: %w", err)
				}
				if rowsOrd > 0 {
					// Check existing dispatch intent status before modifying
					var existingActiveIntents int
					queryIntent := s.rebind(`SELECT COUNT(1) FROM dispatch_intents WHERE order_id = ? AND status IN ('ACKNOWLEDGED', 'COMPLETED');`)
					errIntent := tx.QueryRowContext(ctx, queryIntent, p.OrderID).Scan(&existingActiveIntents)
					if errIntent != nil {
						return nil, fmt.Errorf("capture payment: check dispatch intent: %w", errIntent)
					}
					if existingActiveIntents > 0 {
						return nil, fmt.Errorf("order %s cannot create dispatch intent: existing intent is already active or completed", p.OrderID)
					}

					deleteIntent := s.rebind(`DELETE FROM dispatch_intents WHERE order_id = ? AND status NOT IN ('ACKNOWLEDGED', 'COMPLETED');`)
					resDel, err := tx.ExecContext(ctx, deleteIntent, p.OrderID)
					if err != nil {
						return nil, fmt.Errorf("capture payment: delete dispatch intent: %w", err)
					}
					if _, err := resDel.RowsAffected(); err != nil {
						return nil, fmt.Errorf("capture payment: delete dispatch intent rows affected: %w", err)
					}

					insertIntent := s.rebind(`
INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, 'PENDING', ?, ?);
`)
					for idx, jID := range docJobIDs {
						intentCreatedNS := nowNS + int64(idx)
						resIns, err := tx.ExecContext(ctx, insertIntent, jID, p.OrderID, orderKioskID, sessID, intentCreatedNS, intentCreatedNS)
						if err != nil {
							return nil, fmt.Errorf("capture payment: insert dispatch intent for job %s: %w", jID, err)
						}
						if _, err := resIns.RowsAffected(); err != nil {
							return nil, fmt.Errorf("capture payment: insert dispatch intent rows affected for job %s: %w", jID, err)
						}
					}

					dispatchEligible = true
					activeSessionID = sessID
					currentStatus = contracts.JobStatusDispatched
				}
			}
		}
	}

	// 7. Insert trusted payment record
	recordSessionID := activeSessionID
	if recordSessionID == "" {
		recordSessionID = p.SessionID
	}
	insertPay := s.rebind(`
INSERT INTO payments (payment_id, order_id, kiosk_id, session_id, amount_paise, status, provider, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
`)
	resPay, err := tx.ExecContext(ctx, insertPay, p.PaymentID, p.OrderID, orderKioskID, recordSessionID, pricePaise, p.Status, p.Provider, nowNS)
	if err != nil {
		_ = tx.Rollback()
		var (
			cOrderID  string
			cAmount   int64
			cStatus   string
			cProvider string
		)
		qCheck := s.rebind(`SELECT order_id, amount_paise, status, provider FROM payments WHERE payment_id = ?;`)
		if sErr := s.db.QueryRowContext(ctx, qCheck, p.PaymentID).Scan(&cOrderID, &cAmount, &cStatus, &cProvider); sErr == nil {
			if cOrderID == p.OrderID && cAmount == pricePaise && cProvider == p.Provider && IsVerifiedCapturedStatus(cStatus) {
				order, oErr := s.GetOrder(ctx, p.OrderID)
				if oErr != nil {
					return nil, fmt.Errorf("capture payment: conflict recovery fetch order %s: %w", p.OrderID, oErr)
				}
				if order == nil {
					return nil, fmt.Errorf("capture payment: conflict recovery order %s not found: %w", p.OrderID, ErrOrderNotFound)
				}
				var cmds []contracts.PrintJobCommand
				if len(order.Documents) > 0 {
					cmds = make([]contracts.PrintJobCommand, len(order.Documents))
					for i, doc := range order.Documents {
						cmds[i] = doc.Command(order.Command.KioskID, order.Command.DisplayCode, len(order.Documents))
					}
				} else {
					cmds = []contracts.PrintJobCommand{order.Command}
				}
				return &PaymentCaptureResult{
					OrderStatus:      order.Status,
					AlreadyProcessed: true,
					DispatchEligible: false,
					PricePaise:       order.PricePaise,
					KioskID:          order.Command.KioskID,
					Command:          &order.Command,
					Commands:         cmds,
				}, nil
			}
		}
		return nil, fmt.Errorf("capture payment: insert payment record: %w", err)
	}
	if _, err := resPay.RowsAffected(); err != nil {
		return nil, fmt.Errorf("capture payment: insert payment rows affected: %w", err)
	}

	// Insert into payment_webhooks within the same atomic transaction
	insertWebhook := s.rebind(`
INSERT INTO payment_webhooks (webhook_id, received_at_ns)
VALUES (?, ?)
ON CONFLICT(webhook_id) DO NOTHING;
`)
	if _, err := tx.ExecContext(ctx, insertWebhook, p.PaymentID, nowNS); err != nil {
		return nil, fmt.Errorf("capture payment: insert payment webhook: %w", err)
	}

	// Update matching payment_attempts to PAID
	updateAtt := s.rebind(`
UPDATE payment_attempts
SET status = 'PAID', provider_payment_id = ?, updated_at_ns = ?
WHERE order_id = ? AND status = 'CREATED';
`)
	resAtt, err := tx.ExecContext(ctx, updateAtt, p.PaymentID, nowNS, p.OrderID)
	if err != nil {
		return nil, fmt.Errorf("capture payment: update payment attempt: %w", err)
	}
	if _, err := resAtt.RowsAffected(); err != nil {
		return nil, fmt.Errorf("capture payment: update payment attempt rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("capture payment: commit tx: %w", err)
	}

	order, oErr := s.GetOrder(ctx, p.OrderID)
	if oErr != nil {
		return nil, fmt.Errorf("capture payment: fetch committed order %s: %w", p.OrderID, oErr)
	}
	if order == nil {
		return nil, fmt.Errorf("capture payment: committed order %s not found: %w", p.OrderID, ErrOrderNotFound)
	}
	var cmd *contracts.PrintJobCommand
	var cmds []contracts.PrintJobCommand
	if dispatchEligible {
		cmd = &order.Command
		if len(order.Documents) > 0 {
			cmds = make([]contracts.PrintJobCommand, len(order.Documents))
			for i, doc := range order.Documents {
				cmds[i] = doc.Command(orderKioskID, order.Command.DisplayCode, len(order.Documents))
			}
		} else {
			cmds = []contracts.PrintJobCommand{order.Command}
		}
	} else {
		cmd = &order.Command
	}

	return &PaymentCaptureResult{
		OrderStatus:      currentStatus,
		DispatchEligible: dispatchEligible,
		AlreadyProcessed: false,
		ActiveSessionID:  activeSessionID,
		PricePaise:       pricePaise,
		KioskID:          orderKioskID,
		Command:          cmd,
		Commands:         cmds,
	}, nil
}

// ReleasePaidOrder authorizes physical print dispatch for an already paid order
// by verifying that the kiosk has an active unexpired transaction session.
func (s *Store) ReleasePaidOrder(ctx context.Context, kioskID, orderID, sessionID string, now time.Time) (*Order, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	kioskID = strings.TrimSpace(kioskID)
	orderID = strings.TrimSpace(orderID)
	sessionID = strings.TrimSpace(sessionID)
	if kioskID == "" || orderID == "" || sessionID == "" {
		return nil, errors.New("kioskID, orderID, and active sessionID cannot be empty for paid order release")
	}
	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("release paid order: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Verify order exists, belongs to kiosk, and is in PAID_WAITING status
	var (
		orderKioskID string
		statusStr    string
		orderJobID   string
	)
	queryOrder := s.rebind(`SELECT kiosk_id, status, job_id FROM orders WHERE order_id = ?;`)
	err = tx.QueryRowContext(ctx, queryOrder, orderID).Scan(&orderKioskID, &statusStr, &orderJobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("release paid order: query order: %w", err)
	}

	if orderKioskID != kioskID {
		return nil, ErrKioskMismatch
	}

	if statusStr != string(contracts.JobStatusPaidWaiting) {
		return nil, fmt.Errorf("order cannot be released: current status is %s, expected %s", statusStr, contracts.JobStatusPaidWaiting)
	}

	// 2. Verify active unexpired session exists for this kiosk, order, and sessionID
	var sessID string
	querySess := s.rebind(`
SELECT session_id
FROM kiosk_sessions
WHERE session_id = ? AND order_id = ? AND kiosk_id = ? AND status = 'PAYMENT_PENDING' AND expires_at_ns > ?;
`)
	err = tx.QueryRowContext(ctx, querySess, sessionID, orderID, kioskID, nowNS).Scan(&sessID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("release paid order: verify session: %w", err)
	}

	// 3. Ensure we do not delete or overwrite an acknowledged or completed dispatch intent
	var existingActiveIntents int
	queryIntent := s.rebind(`SELECT COUNT(1) FROM dispatch_intents WHERE order_id = ? AND status IN ('ACKNOWLEDGED', 'COMPLETED');`)
	errIntent := tx.QueryRowContext(ctx, queryIntent, orderID).Scan(&existingActiveIntents)
	if errIntent != nil {
		return nil, fmt.Errorf("release paid order: check existing dispatch intent: %w", errIntent)
	}
	if existingActiveIntents > 0 {
		return nil, fmt.Errorf("order %s cannot be released: dispatch intent is already active or completed", orderID)
	}

	// 4. Mark session COMPLETED
	updateSession := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE session_id = ? AND status = 'PAYMENT_PENDING';
`)
	resSess, err := tx.ExecContext(ctx, updateSession, nowNS, sessID)
	if err != nil {
		return nil, fmt.Errorf("release paid order: complete session: %w", err)
	}
	rowsSess, err := resSess.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("release paid order: complete session rows affected: %w", err)
	}
	if rowsSess == 0 {
		return nil, ErrSessionNotFound
	}

	// 5. Mark order DISPATCHED
	updateOrder := s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ? AND status = ?;
`)
	resOrd, err := tx.ExecContext(ctx, updateOrder, string(contracts.JobStatusDispatched), nowNS, orderID, string(contracts.JobStatusPaidWaiting))
	if err != nil {
		return nil, fmt.Errorf("release paid order: update order status: %w", err)
	}
	rowsOrd, err := resOrd.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("release paid order: update order status rows affected: %w", err)
	}
	if rowsOrd == 0 {
		return nil, fmt.Errorf("order state conflict: could not transition to DISPATCHED")
	}

	// 6. Record durable dispatch intent in PENDING status for all child document jobs
	deleteIntent := s.rebind(`DELETE FROM dispatch_intents WHERE order_id = ? AND status NOT IN ('ACKNOWLEDGED', 'COMPLETED');`)
	resDel, err := tx.ExecContext(ctx, deleteIntent, orderID)
	if err != nil {
		return nil, fmt.Errorf("release paid order: delete dispatch intent: %w", err)
	}
	if _, err := resDel.RowsAffected(); err != nil {
		return nil, fmt.Errorf("release paid order: delete dispatch intent rows affected: %w", err)
	}

	var docJobIDs []string
	rowsDocs, err := tx.QueryContext(ctx, s.rebind(`SELECT job_id FROM order_documents WHERE order_id = ? ORDER BY document_index ASC;`), orderID)
	if err != nil {
		return nil, fmt.Errorf("release paid order: query order_documents: %w", err)
	}
	for rowsDocs.Next() {
		var jID string
		if sErr := rowsDocs.Scan(&jID); sErr != nil {
			_ = rowsDocs.Close()
			return nil, fmt.Errorf("release paid order: scan order_documents: %w", sErr)
		}
		if jID != "" {
			docJobIDs = append(docJobIDs, jID)
		}
	}
	if err := rowsDocs.Err(); err != nil {
		_ = rowsDocs.Close()
		return nil, fmt.Errorf("release paid order: iterate order_documents: %w", err)
	}
	_ = rowsDocs.Close()

	if len(docJobIDs) == 0 {
		docJobIDs = []string{orderJobID}
	}

	insertIntent := s.rebind(`
INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, 'PENDING', ?, ?);
`)
	for idx, jID := range docJobIDs {
		intentCreatedNS := nowNS + int64(idx)
		resIns, err := tx.ExecContext(ctx, insertIntent, jID, orderID, kioskID, sessID, intentCreatedNS, intentCreatedNS)
		if err != nil {
			return nil, fmt.Errorf("release paid order: record dispatch intent: %w", err)
		}
		if _, err := resIns.RowsAffected(); err != nil {
			return nil, fmt.Errorf("release paid order: record dispatch intent rows affected: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("release paid order: commit tx: %w", err)
	}

	return s.GetOrder(ctx, orderID)
}

// RenewOrderPIN generates a new PIN and 15-minute expiry for a paid-but-unprinted order.
// Requires verified recovery credential matching the order's claim token.
func (s *Store) RenewOrderPIN(ctx context.Context, orderID, newPIN, recoveryToken string, newExpiry time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	orderID = strings.TrimSpace(orderID)
	newPIN = strings.TrimSpace(newPIN)
	recoveryToken = strings.TrimSpace(recoveryToken)
	if orderID == "" || newPIN == "" {
		return errors.New("orderID and newPIN cannot be empty")
	}
	now := time.Now().UTC()
	nowNS := now.UnixNano()
	expNS := newExpiry.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("renew pin: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Verify order exists, is in PAID_WAITING status, and verify recovery credential
	var (
		kioskID    string
		statusStr  string
		claimToken string
	)
	queryOrder := s.rebind(`SELECT kiosk_id, status, COALESCE(claim_token, '') FROM orders WHERE order_id = ?;`)
	err = tx.QueryRowContext(ctx, queryOrder, orderID).Scan(&kioskID, &statusStr, &claimToken)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOrderNotFound
		}
		return fmt.Errorf("renew pin: query order: %w", err)
	}
	if statusStr != string(contracts.JobStatusPaidWaiting) {
		return fmt.Errorf("cannot renew PIN: order is in status %s (expected %s)", statusStr, contracts.JobStatusPaidWaiting)
	}
	if claimToken == "" || recoveryToken == "" || !hmac.Equal([]byte(recoveryToken), []byte(claimToken)) {
		return ErrUnauthorized
	}

	// 2. Check if newPIN is collision with active unexpired reservation on same kiosk
	var count int
	queryInUse := s.rebind(`
SELECT COUNT(1)
FROM pin_reservations
WHERE kiosk_id = ? AND pin = ? AND expires_at_ns > ? AND order_id != ?;
`)
	if err := tx.QueryRowContext(ctx, queryInUse, kioskID, newPIN, nowNS, orderID).Scan(&count); err != nil {
		return fmt.Errorf("renew pin: check pin collision: %w", err)
	}
	if count > 0 {
		return ErrPINCollision
	}

	// 3. Remove old reservation
	deleteOld := s.rebind(`DELETE FROM pin_reservations WHERE order_id = ?;`)
	_, _ = tx.ExecContext(ctx, deleteOld, orderID)

	// 4. Insert new reservation
	insertRes := s.rebind(`
INSERT INTO pin_reservations (kiosk_id, pin, order_id, expires_at_ns, created_at_ns)
VALUES (?, ?, ?, ?, ?);
`)
	if _, err := tx.ExecContext(ctx, insertRes, kioskID, newPIN, orderID, expNS, nowNS); err != nil {
		return fmt.Errorf("renew pin: insert reservation: %w", err)
	}

	// 5. Update orders table
	updateOrder := s.rebind(`
UPDATE orders
SET display_code = ?, pin_expires_at_ns = ?, updated_at_ns = ?
WHERE order_id = ?;
`)
	if _, err := tx.ExecContext(ctx, updateOrder, newPIN, expNS, nowNS, orderID); err != nil {
		return fmt.Errorf("renew pin: update order: %w", err)
	}

	return tx.Commit()
}

// MarkDispatchIntentSent marks the dispatch intent for a specific job as SENT.
func (s *Store) MarkDispatchIntentSent(ctx context.Context, jobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("jobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'SENT', dispatched_at_ns = ?, updated_at_ns = ?
WHERE job_id = ? AND status = 'PENDING';
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, nowNS, jobID)
	return err
}

// MarkDispatchIntentAcknowledged records that the kiosk daemon has durably accepted and persisted a specific job.
func (s *Store) MarkDispatchIntentAcknowledged(ctx context.Context, jobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("jobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'ACKNOWLEDGED', acknowledged_at_ns = ?, updated_at_ns = ?
WHERE job_id = ? AND status IN ('PENDING', 'SENT');
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, nowNS, jobID)
	return err
}

// MarkDispatchIntentCompleted records that physical printing has completed for a specific job.
func (s *Store) MarkDispatchIntentCompleted(ctx context.Context, jobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("jobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'COMPLETED', updated_at_ns = ?
WHERE job_id = ? AND status != 'COMPLETED';
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, jobID)
	return err
}

// GetJobDispatchIntentStatus returns the status and acknowledged_at_ns for a specific job's dispatch intent.
func (s *Store) GetJobDispatchIntentStatus(ctx context.Context, jobID string) (status string, acknowledgedAtNS int64, err error) {
	if s == nil || s.db == nil {
		return "", 0, errors.New("store not initialized")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return "", 0, errors.New("jobID cannot be empty")
	}
	query := s.rebind(`SELECT status, acknowledged_at_ns FROM dispatch_intents WHERE job_id = ?;`)
	err = s.db.QueryRowContext(ctx, query, jobID).Scan(&status, &acknowledgedAtNS)
	return status, acknowledgedAtNS, err
}

// OrderDispatchSummary aggregates dispatch intent states across an entire order.
type OrderDispatchSummary struct {
	TotalCount        int
	PendingCount      int
	SentCount         int
	AcknowledgedCount int
	CompletedCount    int
	CanceledCount     int
}

// GetOrderDispatchSummary returns aggregated dispatch intent counts for all child jobs under an order.
func (s *Store) GetOrderDispatchSummary(ctx context.Context, orderID string) (OrderDispatchSummary, error) {
	if s == nil || s.db == nil {
		return OrderDispatchSummary{}, errors.New("store not initialized")
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return OrderDispatchSummary{}, errors.New("orderID cannot be empty")
	}
	query := s.rebind(`
SELECT
    COUNT(1),
    COALESCE(SUM(CASE WHEN status = 'PENDING' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'SENT' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'ACKNOWLEDGED' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'COMPLETED' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN status = 'CANCELED' THEN 1 ELSE 0 END), 0)
FROM dispatch_intents
WHERE order_id = ?;
`)
	var sum OrderDispatchSummary
	err := s.db.QueryRowContext(ctx, query, orderID).Scan(
		&sum.TotalCount, &sum.PendingCount, &sum.SentCount,
		&sum.AcknowledgedCount, &sum.CompletedCount, &sum.CanceledCount,
	)
	if err != nil {
		return OrderDispatchSummary{}, fmt.Errorf("get order dispatch summary for %s: %w", orderID, err)
	}
	return sum, nil
}

// DispatchIntent represents a persisted job dispatch intent.
type DispatchIntent struct {
	JobID            string
	OrderID          string
	KioskID          string
	SessionID        string
	Status           string
	CreatedAtNS      int64
	UpdatedAtNS      int64
	DispatchedAtNS   int64
	AcknowledgedAtNS int64
	RetryCount       int
	LastError        string
}

// GetDispatchIntentsByOrder retrieves all dispatch intents for an order ordered by created_at_ns ASC.
func (s *Store) GetDispatchIntentsByOrder(ctx context.Context, orderID string) ([]DispatchIntent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, errors.New("orderID cannot be empty")
	}
	query := s.rebind(`
SELECT job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns, dispatched_at_ns, acknowledged_at_ns, retry_count, last_error
FROM dispatch_intents
WHERE order_id = ?
ORDER BY created_at_ns ASC;
`)
	rows, err := s.db.QueryContext(ctx, query, orderID)
	if err != nil {
		return nil, fmt.Errorf("get dispatch intents by order: %w", err)
	}
	defer rows.Close()

	var intents []DispatchIntent
	for rows.Next() {
		var di DispatchIntent
		if err := rows.Scan(
			&di.JobID, &di.OrderID, &di.KioskID, &di.SessionID, &di.Status,
			&di.CreatedAtNS, &di.UpdatedAtNS, &di.DispatchedAtNS, &di.AcknowledgedAtNS,
			&di.RetryCount, &di.LastError,
		); err != nil {
			return nil, fmt.Errorf("scan dispatch intent: %w", err)
		}
		intents = append(intents, di)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispatch intents: %w", err)
	}
	return intents, nil
}

// ResetUnacknowledgedDispatchIntents safely reverts unacknowledged SENT dispatch intents
// back to PENDING status so that network or daemon crashes do not permanently strand orders.
func (s *Store) ResetUnacknowledgedDispatchIntents(ctx context.Context, kioskID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return nil
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'PENDING', updated_at_ns = ?
WHERE kiosk_id = ? AND status = 'SENT' AND (acknowledged_at_ns = 0 OR acknowledged_at_ns IS NULL);
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, kioskID)
	return err
}

// GetPendingDispatchOrders returns all PrintJobCommands for a kiosk that need dispatch.
// It retrieves pending dispatch intents (status 'PENDING'),
// and verifies that the customer session has NOT been canceled or expired.
// If the session was canceled or expired, the intent is canceled and NOT dispatched.
func (s *Store) GetPendingDispatchOrders(ctx context.Context, kioskID string) ([]contracts.PrintJobCommand, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return nil, nil
	}
	nowNS := time.Now().UTC().UnixNano()

	query := s.rebind(`
SELECT order_id, job_id, session_id, status
FROM dispatch_intents
WHERE kiosk_id = ? AND status = 'PENDING'
ORDER BY created_at_ns ASC;
`)
	rows, err := s.db.QueryContext(ctx, query, kioskID)
	if err != nil {
		return nil, fmt.Errorf("get pending dispatch orders: %w", err)
	}
	defer rows.Close()

	type pendingRecord struct {
		orderID   string
		jobID     string
		sessionID string
		status    string
	}
	var candidates []pendingRecord
	for rows.Next() {
		var rec pendingRecord
		if err := rows.Scan(&rec.orderID, &rec.jobID, &rec.sessionID, &rec.status); err == nil {
			candidates = append(candidates, rec)
		}
	}
	_ = rows.Close()

	var cmds []contracts.PrintJobCommand
	for _, cand := range candidates {
		// Session validity verification (Task 8):
		// Cloud reconnects cannot dispatch a previously authorized job after its customer session was canceled or expired.
		if cand.sessionID != "" {
			var (
				sessStatus string
				sessExpNS  int64
			)
			qSess := s.rebind(`SELECT status, expires_at_ns FROM kiosk_sessions WHERE session_id = ?;`)
			err := s.db.QueryRowContext(ctx, qSess, cand.sessionID).Scan(&sessStatus, &sessExpNS)
			if err != nil || sessStatus == "CANCELED" || sessStatus == "EXPIRED" || (sessStatus == "PAYMENT_PENDING" && sessExpNS <= nowNS) {
				// Cancel intent and revert order to PAID_WAITING to avoid unattended ghost printing
				_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE dispatch_intents SET status = 'CANCELED', updated_at_ns = ? WHERE order_id = ?;`), nowNS, cand.orderID)
				_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE orders SET status = ?, updated_at_ns = ? WHERE order_id = ? AND status = ?;`),
					string(contracts.JobStatusPaidWaiting), nowNS, cand.orderID, string(contracts.JobStatusDispatched))
				continue
			}
		}

		// Valid authorized job: record retry increment
		_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE dispatch_intents SET retry_count = retry_count + 1, updated_at_ns = ? WHERE job_id = ?;`), nowNS, cand.jobID)

		ord, err := s.GetOrder(ctx, cand.orderID)
		if err != nil {
			return nil, fmt.Errorf("get pending dispatch orders: fetch order %s: %w", cand.orderID, err)
		}
		if ord == nil {
			return nil, fmt.Errorf("get pending dispatch orders: order %s not found", cand.orderID)
		}

		// A funded multi-document order remains dispatch-authorized while
		// earlier children are SPOOLING or PRINTING. Reconnect must deliver
		// unacknowledged later children without resending acknowledged ones.
		// Faults and completion never authorize additional delivery.
		if ord.Status != contracts.JobStatusDispatched &&
			ord.Status != contracts.JobStatusSpooling &&
			ord.Status != contracts.JobStatusPrinting {
			if ord.Status == contracts.JobStatusFailedRefundable || string(ord.Status) == "CANCELED" {
				_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE dispatch_intents SET status = 'CANCELED', updated_at_ns = ? WHERE job_id = ?;`), nowNS, cand.jobID)
			}
			continue
		}

		// Validate payment status explicitly: require verified captured payment record
		var (
			payStatus   string
			payAmount   int64
			payProvider string
		)
		qPay := s.rebind(`
SELECT p.status, p.amount_paise, p.provider
FROM payments p
LEFT JOIN order_funding_payments f ON f.order_id = p.order_id
WHERE p.order_id = ? AND LOWER(p.status) IN ('captured', 'paid')
AND (p.payment_id = f.payment_id OR
     (f.payment_id IS NULL AND p.amount_paise = ?))
ORDER BY
    CASE WHEN p.payment_id = f.payment_id THEN 0 ELSE 1 END,
    p.created_at_ns ASC, p.payment_id ASC
LIMIT 1;
`)
		// A later surplus capture can be for a different amount. Reconnect
		// must validate the same funding payment that authorized printing,
		// never whichever captured payment happened most recently.
		errPay := s.db.QueryRowContext(ctx, qPay, cand.orderID, ord.PricePaise).Scan(&payStatus, &payAmount, &payProvider)
		if errPay != nil {
			if errors.Is(errPay, sql.ErrNoRows) {
				// No verified captured payment exists for this order; do not dispatch!
				continue
			}
			return nil, fmt.Errorf("get pending dispatch orders: check payment for order %s: %w", cand.orderID, errPay)
		}
		if payAmount <= 0 || payAmount != ord.PricePaise {
			return nil, fmt.Errorf("get pending dispatch orders: payment amount mismatch for order %s: expected %d paise, got %d paise", cand.orderID, ord.PricePaise, payAmount)
		}
		if strings.TrimSpace(payProvider) == "" {
			return nil, fmt.Errorf("get pending dispatch orders: missing payment provider for order %s", cand.orderID)
		}

		// Retrieve child document job - never fall back to parent print command
		doc, errDoc := s.GetOrderDocument(ctx, cand.jobID)
		if errDoc != nil {
			return nil, fmt.Errorf("get pending dispatch orders: child document %s for order %s lookup failed: %w", cand.jobID, cand.orderID, errDoc)
		}
		if doc == nil {
			return nil, fmt.Errorf("get pending dispatch orders: child document %s for order %s not found in order_documents (corrupted dispatch intent)", cand.jobID, cand.orderID)
		}

		totalDocs := len(ord.Documents)
		if totalDocs == 0 {
			totalDocs = ord.Command.TotalDocuments
		}
		if totalDocs == 0 {
			totalDocs = 1
		}
		cmds = append(cmds, doc.Command(ord.Command.KioskID, ord.Command.DisplayCode, totalDocs))
	}
	return cmds, nil
}

// PaymentAttempt represents a persisted payment initiation attempt.
type PaymentAttempt struct {
	AttemptID         string
	OrderID           string
	KioskID           string
	SessionID         string
	Provider          string
	ProviderOrderID   string
	ProviderPaymentID string
	AmountPaise       int64
	Currency          string
	Status            string // "CREATED", "PAID", "EXPIRED", "FAILED"
	QRPayload         string
	QRImageURL        string
	IdempotencyKey    string
	DemoToken         string
	ExpiresAt         time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreatePaymentAttempt persists a new payment attempt bound to an active session.
func (s *Store) CreatePaymentAttempt(ctx context.Context, att PaymentAttempt) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	if ctx == nil {
		return errors.New("context is nil")
	}
	att.AttemptID = strings.TrimSpace(att.AttemptID)
	att.OrderID = strings.TrimSpace(att.OrderID)
	att.KioskID = strings.TrimSpace(att.KioskID)
	att.SessionID = strings.TrimSpace(att.SessionID)
	if att.AttemptID == "" || att.OrderID == "" || att.KioskID == "" || att.SessionID == "" {
		return errors.New("attempt_id, order_id, kiosk_id, and session_id are required")
	}
	if att.AmountPaise <= 0 {
		return fmt.Errorf("invalid amount_paise: %d", att.AmountPaise)
	}
	if att.Currency == "" {
		att.Currency = "INR"
	}
	if att.Status == "" {
		att.Status = "CREATED"
	}
	now := time.Now().UTC()
	createdNS := att.CreatedAt.UnixNano()
	if att.CreatedAt.IsZero() {
		createdNS = now.UnixNano()
	}
	updatedNS := att.UpdatedAt.UnixNano()
	if att.UpdatedAt.IsZero() {
		updatedNS = createdNS
	}
	expNS := att.ExpiresAt.UnixNano()

	query := s.rebind(`
INSERT INTO payment_attempts (
    attempt_id, order_id, kiosk_id, session_id, provider,
    provider_order_id, provider_payment_id, amount_paise, currency,
    status, qr_payload, qr_image_url, idempotency_key, demo_token,
    expires_at_ns, created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`)
	_, err := s.db.ExecContext(ctx, query,
		att.AttemptID, att.OrderID, att.KioskID, att.SessionID, att.Provider,
		att.ProviderOrderID, att.ProviderPaymentID, att.AmountPaise, att.Currency,
		att.Status, att.QRPayload, att.QRImageURL, att.IdempotencyKey, att.DemoToken,
		expNS, createdNS, updatedNS,
	)
	if err != nil {
		return fmt.Errorf("create payment attempt: %w", err)
	}
	return nil
}

// GetActivePaymentAttempt retrieves an existing unexpired payment attempt for the given session_id in CREATED status.
func (s *Store) GetActivePaymentAttempt(ctx context.Context, sessionID string, now time.Time) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	nowNS := now.UTC().UnixNano()

	query := s.rebind(`
SELECT attempt_id, order_id, kiosk_id, session_id, provider,
       provider_order_id, provider_payment_id, amount_paise, currency,
       status, qr_payload, qr_image_url, idempotency_key, demo_token,
       expires_at_ns, created_at_ns, updated_at_ns
FROM payment_attempts
WHERE session_id = ? AND status = 'CREATED' AND expires_at_ns > ?
ORDER BY created_at_ns DESC
LIMIT 1;
`)
	row := s.db.QueryRowContext(ctx, query, sessionID, nowNS)
	var att PaymentAttempt
	var expNS, cNS, uNS int64
	err := row.Scan(
		&att.AttemptID, &att.OrderID, &att.KioskID, &att.SessionID, &att.Provider,
		&att.ProviderOrderID, &att.ProviderPaymentID, &att.AmountPaise, &att.Currency,
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey, &att.DemoToken,
		&expNS, &cNS, &uNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active payment attempt: %w", err)
	}
	att.ExpiresAt = time.Unix(0, expNS).UTC()
	att.CreatedAt = time.Unix(0, cNS).UTC()
	att.UpdatedAt = time.Unix(0, uNS).UTC()
	return &att, nil
}

// GetPaymentAttemptByProviderOrder retrieves a payment attempt by provider and provider_order_id.
func (s *Store) GetPaymentAttemptByProviderOrder(ctx context.Context, provider, providerOrderID string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	provider = strings.TrimSpace(provider)
	providerOrderID = strings.TrimSpace(providerOrderID)
	if providerOrderID == "" {
		return nil, nil
	}

	query := s.rebind(`
SELECT attempt_id, order_id, kiosk_id, session_id, provider,
       provider_order_id, provider_payment_id, amount_paise, currency,
       status, qr_payload, qr_image_url, idempotency_key, demo_token,
       expires_at_ns, created_at_ns, updated_at_ns
FROM payment_attempts
WHERE (provider = ? OR ? = '') AND provider_order_id = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`)
	row := s.db.QueryRowContext(ctx, query, provider, provider, providerOrderID)
	var att PaymentAttempt
	var expNS, cNS, uNS int64
	err := row.Scan(
		&att.AttemptID, &att.OrderID, &att.KioskID, &att.SessionID, &att.Provider,
		&att.ProviderOrderID, &att.ProviderPaymentID, &att.AmountPaise, &att.Currency,
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey, &att.DemoToken,
		&expNS, &cNS, &uNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get payment attempt by provider order: %w", err)
	}
	att.ExpiresAt = time.Unix(0, expNS).UTC()
	att.CreatedAt = time.Unix(0, cNS).UTC()
	att.UpdatedAt = time.Unix(0, uNS).UTC()
	return &att, nil
}

// GetPaymentAttemptByOrderAndSession retrieves the latest payment attempt matching an order and session.
func (s *Store) GetPaymentAttemptByOrderAndSession(ctx context.Context, orderID, sessionID string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	orderID = strings.TrimSpace(orderID)
	sessionID = strings.TrimSpace(sessionID)
	if orderID == "" {
		return nil, nil
	}

	query := s.rebind(`
SELECT attempt_id, order_id, kiosk_id, session_id, provider,
       provider_order_id, provider_payment_id, amount_paise, currency,
       status, qr_payload, qr_image_url, idempotency_key, demo_token,
       expires_at_ns, created_at_ns, updated_at_ns
FROM payment_attempts
WHERE order_id = ? AND (session_id = ? OR ? = '')
ORDER BY created_at_ns DESC
LIMIT 1;
`)
	row := s.db.QueryRowContext(ctx, query, orderID, sessionID, sessionID)
	var att PaymentAttempt
	var expNS, cNS, uNS int64
	err := row.Scan(
		&att.AttemptID, &att.OrderID, &att.KioskID, &att.SessionID, &att.Provider,
		&att.ProviderOrderID, &att.ProviderPaymentID, &att.AmountPaise, &att.Currency,
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey, &att.DemoToken,
		&expNS, &cNS, &uNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get payment attempt by order: %w", err)
	}
	att.ExpiresAt = time.Unix(0, expNS).UTC()
	att.CreatedAt = time.Unix(0, cNS).UTC()
	att.UpdatedAt = time.Unix(0, uNS).UTC()
	return &att, nil
}

// GetPaymentAttemptByToken retrieves a payment attempt by its demo checkout token.
func (s *Store) GetPaymentAttemptByToken(ctx context.Context, demoToken string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialized")
	}
	demoToken = strings.TrimSpace(demoToken)
	if demoToken == "" {
		return nil, nil
	}

	query := s.rebind(`
SELECT attempt_id, order_id, kiosk_id, session_id, provider,
       provider_order_id, provider_payment_id, amount_paise, currency,
       status, qr_payload, qr_image_url, idempotency_key, demo_token,
       expires_at_ns, created_at_ns, updated_at_ns
FROM payment_attempts
WHERE demo_token = ? OR session_id = ? OR attempt_id = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`)
	row := s.db.QueryRowContext(ctx, query, demoToken, demoToken, demoToken)
	var att PaymentAttempt
	var expNS, cNS, uNS int64
	err := row.Scan(
		&att.AttemptID, &att.OrderID, &att.KioskID, &att.SessionID, &att.Provider,
		&att.ProviderOrderID, &att.ProviderPaymentID, &att.AmountPaise, &att.Currency,
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey, &att.DemoToken,
		&expNS, &cNS, &uNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get payment attempt by demo token: %w", err)
	}
	att.ExpiresAt = time.Unix(0, expNS).UTC()
	att.CreatedAt = time.Unix(0, cNS).UTC()
	att.UpdatedAt = time.Unix(0, uNS).UTC()
	return &att, nil
}

// MarkPaymentAttemptPaid transitions the attempt to PAID status upon verified capture.
func (s *Store) MarkPaymentAttemptPaid(ctx context.Context, attemptID, providerPaymentID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	attemptID = strings.TrimSpace(attemptID)
	if attemptID == "" {
		return nil
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE payment_attempts
SET status = 'PAID', provider_payment_id = ?, updated_at_ns = ?
WHERE attempt_id = ?;
`)
	_, err := s.db.ExecContext(ctx, query, providerPaymentID, nowNS, attemptID)
	return err
}
