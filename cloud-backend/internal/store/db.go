package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

type Order struct {
	OrderID      string
	Command      contracts.PrintJobCommand
	Status       contracts.JobStatus
	PricePaise   int64
	ColorMode    string // "BW" or "COLOR"
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
    job_id TEXT NOT NULL UNIQUE,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    pin_expires_at_ns BIGINT NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);

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
    job_id TEXT PRIMARY KEY
        REFERENCES orders(job_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS recovery_records (
    recovery_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES orders(job_id) ON DELETE CASCADE,
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
    order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
    job_id TEXT NOT NULL,
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
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);
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
			"CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);",
			"CREATE TABLE IF NOT EXISTS conversions (id TEXT PRIMARY KEY, token TEXT NOT NULL, original_filename TEXT NOT NULL, file_size BIGINT NOT NULL, status TEXT NOT NULL, error_message TEXT NOT NULL DEFAULT '', input_path TEXT NOT NULL, output_path TEXT NOT NULL DEFAULT '', created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, expires_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_status_created ON conversions(status, created_at_ns);",
			"CREATE INDEX IF NOT EXISTS idx_conversions_expires ON conversions(expires_at_ns);",
			"CREATE TABLE IF NOT EXISTS recovery_records (recovery_id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES orders(job_id) ON DELETE CASCADE, kiosk_id TEXT NOT NULL, sequence_id INTEGER NOT NULL, confirmed_sheets_printed INTEGER NOT NULL, remaining_pages INTEGER NOT NULL, remaining_sheets INTEGER NOT NULL, download_url TEXT NOT NULL, sha256_checksum TEXT NOT NULL, status TEXT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, UNIQUE(job_id, sequence_id));",
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
			"CREATE TABLE IF NOT EXISTS dispatch_intents (order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE, job_id TEXT NOT NULL, kiosk_id TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL, dispatched_at_ns BIGINT NOT NULL DEFAULT 0, acknowledged_at_ns BIGINT NOT NULL DEFAULT 0, retry_count INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');",
			"ALTER TABLE dispatch_intents ADD COLUMN IF NOT EXISTS acknowledged_at_ns BIGINT NOT NULL DEFAULT 0;",
			"CREATE INDEX IF NOT EXISTS idx_dispatch_intents_kiosk_status ON dispatch_intents (kiosk_id, status);",
			"CREATE TABLE IF NOT EXISTS payment_attempts (attempt_id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE, kiosk_id TEXT NOT NULL, session_id TEXT NOT NULL REFERENCES kiosk_sessions(session_id) ON DELETE CASCADE, provider TEXT NOT NULL, provider_order_id TEXT NOT NULL DEFAULT '', provider_payment_id TEXT NOT NULL DEFAULT '', amount_paise BIGINT NOT NULL, currency TEXT NOT NULL DEFAULT 'INR', status TEXT NOT NULL, qr_payload TEXT NOT NULL DEFAULT '', qr_image_url TEXT NOT NULL DEFAULT '', idempotency_key TEXT NOT NULL DEFAULT '', expires_at_ns BIGINT NOT NULL, created_at_ns BIGINT NOT NULL, updated_at_ns BIGINT NOT NULL);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);",
			"CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);",
		}
		for _, mig := range pgMigrations {
			if _, err := db.Exec(mig); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("open store: postgres migration: %w", err)
			}
		}

		return &Store{db: db, dialect: "postgres"}, nil
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
    job_id TEXT NOT NULL UNIQUE,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    status TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL,
    pin_expires_at_ns BIGINT NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);

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
    job_id TEXT PRIMARY KEY
        REFERENCES orders(job_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS recovery_records (
    recovery_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES orders(job_id) ON DELETE CASCADE,
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

	return &Store{db: db, dialect: "sqlite"}, nil
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
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_kiosk_pin ON orders (kiosk_id, display_code, status);")

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
    job_id TEXT NOT NULL REFERENCES orders(job_id) ON DELETE CASCADE,
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
    order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
    job_id TEXT NOT NULL,
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
    expires_at_ns BIGINT NOT NULL,
    created_at_ns BIGINT NOT NULL,
    updated_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts (order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_session ON payment_attempts (session_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_provider_order ON payment_attempts (provider, provider_order_id);
CREATE INDEX IF NOT EXISTS idx_payment_attempts_kiosk_status ON payment_attempts (kiosk_id, status);
`)

	colsDispatch := make(map[string]bool)
	rowsDisp, err := db.Query("PRAGMA table_info(dispatch_intents);")
	if err == nil {
		for rowsDisp.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt *string
			if err := rowsDisp.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				colsDispatch[name] = true
			}
		}
		_ = rowsDisp.Close()
	}
	if !colsDispatch["acknowledged_at_ns"] {
		_, _ = db.Exec("ALTER TABLE dispatch_intents ADD COLUMN acknowledged_at_ns INTEGER NOT NULL DEFAULT 0;")
	}
	_, _ = db.Exec("UPDATE dispatch_intents SET status = 'PENDING' WHERE status = 'SENT' AND (acknowledged_at_ns = 0 OR acknowledged_at_ns IS NULL);")

	return nil
}

// Close closes the underlying SQLite database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CreateOrder persists an order and its associated print configuration atomically.
func (s *Store) CreateOrder(ctx context.Context, order Order) error {
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
	if strings.TrimSpace(order.Command.JobID) == "" {
		return fmt.Errorf("create order: command.job_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.KioskID) == "" {
		return fmt.Errorf("create order: command.kiosk_id cannot be empty")
	}
	if strings.TrimSpace(order.Command.DisplayCode) == "" {
		return fmt.Errorf("create order: command.display_code cannot be empty")
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
	if !isValidJobStatus(order.Status) {
		return fmt.Errorf("create order: invalid job status %q", order.Status)
	}
	if order.PricePaise < 0 {
		return fmt.Errorf("create order: price_paise must be >= 0, got %d", order.PricePaise)
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
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, created_at_ns, updated_at_ns, pin_expires_at_ns, claim_token)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`),
		order.OrderID,
		order.Command.JobID,
		order.Command.KioskID,
		order.Command.DisplayCode,
		string(order.Status),
		order.PricePaise,
		createdNS,
		updatedNS,
		pinExpiresNS,
		order.ClaimToken,
	)
	if err != nil {
		return fmt.Errorf("create order: insert into orders: %w", err)
	}

	colorMode := strings.ToUpper(strings.TrimSpace(order.ColorMode))
	if colorMode == "" {
		colorMode = "BW"
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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create order: commit tx: %w", err)
	}

	return nil
}

// GetOrder retrieves an order and its associated print configuration.
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
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise, o.created_at_ns, o.updated_at_ns,
    p.download_url, p.sha256_checksum, p.total_pages, p.total_sheets, p.copies, p.duplex, p.start_from_page,
    COALESCE(p.color_mode, 'BW'), o.pin_expires_at_ns, COALESCE(o.claim_token, '')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.order_id = ?;
`

	row := s.db.QueryRowContext(ctx, s.rebind(query), orderID)

	var (
		resOrderID    string
		jobID         string
		kioskID       string
		displayCode   string
		statusStr     string
		pricePaise    int64
		createdNS     int64
		updatedNS     int64
		downloadURL   string
		sha256Check   string
		totalPages    int
		totalSheets   int
		copies        int
		duplexStr     string
		startFromPage int
		colorMode     string
		pinExpiresNS  int64
		claimToken    string
	)

	err := row.Scan(
		&resOrderID,
		&jobID,
		&kioskID,
		&displayCode,
		&statusStr,
		&pricePaise,
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
			KioskID:        kioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Check,
			TotalPages:     totalPages,
			TotalSheets:    totalSheets,
			Copies:         copies,
			Duplex:         contracts.DuplexMode(duplexStr),
			StartFromPage:  startFromPage,
		},
		Status:       contracts.JobStatus(statusStr),
		PricePaise:   pricePaise,
		ColorMode:    colorMode,
		CreatedAt:    time.Unix(0, createdNS).UTC(),
		UpdatedAt:    time.Unix(0, updatedNS).UTC(),
		PINExpiresAt: time.Unix(0, pinExpiresNS).UTC(),
		ClaimToken:   claimToken,
	}

	return result, nil
}

// GetOrderByJobID retrieves an order and its print configuration by jobID.
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

	query := `
SELECT
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise, o.created_at_ns, o.updated_at_ns,
    p.download_url, p.sha256_checksum, p.total_pages, p.total_sheets, p.copies, p.duplex, p.start_from_page,
    COALESCE(p.color_mode, 'BW'), o.pin_expires_at_ns, COALESCE(o.claim_token, '')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.job_id = ?;
`

	row := s.db.QueryRowContext(ctx, s.rebind(query), jobID)

	var (
		resOrderID    string
		resJobID      string
		kioskID       string
		displayCode   string
		statusStr     string
		pricePaise    int64
		createdNS     int64
		updatedNS     int64
		downloadURL   string
		sha256Check   string
		totalPages    int
		totalSheets   int
		copies        int
		duplexStr     string
		startFromPage int
		colorMode     string
		pinExpiresNS  int64
		claimToken    string
	)

	err := row.Scan(
		&resOrderID,
		&resJobID,
		&kioskID,
		&displayCode,
		&statusStr,
		&pricePaise,
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
		return nil, fmt.Errorf("get order by job id: scan: %w", err)
	}

	result := &Order{
		OrderID: resOrderID,
		Command: contracts.PrintJobCommand{
			JobID:          resJobID,
			KioskID:        kioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Check,
			TotalPages:     totalPages,
			TotalSheets:    totalSheets,
			Copies:         copies,
			Duplex:         contracts.DuplexMode(duplexStr),
			StartFromPage:  startFromPage,
		},
		Status:       contracts.JobStatus(statusStr),
		PricePaise:   pricePaise,
		ColorMode:    colorMode,
		CreatedAt:    time.Unix(0, createdNS).UTC(),
		UpdatedAt:    time.Unix(0, updatedNS).UTC(),
		PINExpiresAt: time.Unix(0, pinExpiresNS).UTC(),
		ClaimToken:   claimToken,
	}

	return result, nil
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
SELECT
    o.order_id, o.job_id, o.kiosk_id, o.display_code, o.status, o.price_paise, o.created_at_ns, o.updated_at_ns,
    p.download_url, p.sha256_checksum, p.total_pages, p.total_sheets, p.copies, p.duplex, p.start_from_page,
    COALESCE(p.color_mode, 'BW'), o.pin_expires_at_ns, COALESCE(o.claim_token, '')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.kiosk_id = ? AND o.display_code = ?
ORDER BY o.created_at_ns DESC
LIMIT 1;
`

	row := s.db.QueryRowContext(ctx, s.rebind(query), kioskID, pin)

	var (
		resOrderID    string
		jobID         string
		resKioskID    string
		displayCode   string
		statusStr     string
		pricePaise    int64
		createdNS     int64
		updatedNS     int64
		downloadURL   string
		sha256Check   string
		totalPages    int
		totalSheets   int
		copies        int
		duplexStr     string
		startFromPage int
		colorMode     string
		pinExpiresNS  int64
		claimToken    string
	)

	err := row.Scan(
		&resOrderID,
		&jobID,
		&resKioskID,
		&displayCode,
		&statusStr,
		&pricePaise,
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
		return nil, fmt.Errorf("get order by pin: scan: %w", err)
	}

	result := &Order{
		OrderID: resOrderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        resKioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Check,
			TotalPages:     totalPages,
			TotalSheets:    totalSheets,
			Copies:         copies,
			Duplex:         contracts.DuplexMode(duplexStr),
			StartFromPage:  startFromPage,
		},
		Status:       contracts.JobStatus(statusStr),
		PricePaise:   pricePaise,
		ColorMode:    colorMode,
		CreatedAt:    time.Unix(0, createdNS).UTC(),
		UpdatedAt:    time.Unix(0, updatedNS).UTC(),
		PINExpiresAt: time.Unix(0, pinExpiresNS).UTC(),
		ClaimToken:   claimToken,
	}

	return result, nil
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

// SetPricePerSheetPaise stores or replaces the singleton per-sheet pricing.
func (s *Store) SetPricePerSheetPaise(ctx context.Context, pricePaise int64) error {
	cfg, _ := s.GetPricing(ctx)
	colorPrice := cfg.ColorPricePerSheetPaise
	if colorPrice <= 0 {
		colorPrice = 1000
	}
	return s.SetPricing(ctx, pricePaise, colorPrice)
}

// GetPricePerSheetPaise retrieves the configured per-sheet pricing.
func (s *Store) GetPricePerSheetPaise(ctx context.Context) (int64, error) {
	cfg, err := s.GetPricing(ctx)
	if err != nil {
		return 0, err
	}
	return cfg.BWPricePerSheetPaise, nil
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

	// 1. Check order exists and retrieve kiosk_id, total_pages, total_sheets
	var (
		orderKioskID string
		totalPages   int
		totalSheets  int
	)
	queryOrder := `
SELECT o.kiosk_id, p.total_pages, p.total_sheets
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.job_id = ?;
`
	err = tx.QueryRowContext(ctx, s.rebind(queryOrder), event.JobID).Scan(&orderKioskID, &totalPages, &totalSheets)
	if err == sql.ErrNoRows {
		return fmt.Errorf("update job progress: job %q not found in orders", event.JobID)
	}
	if err != nil {
		return fmt.Errorf("update job progress: query order: %w", err)
	}

	// 2. Verify kiosk ID matches
	if orderKioskID != event.KioskID {
		return fmt.Errorf("update job progress: kiosk ID mismatch: event has %q, order belongs to %q", event.KioskID, orderKioskID)
	}

	// 3. Verify progress bounds
	if event.PagesPrinted > totalPages {
		return fmt.Errorf("update job progress: pages_printed (%d) exceeds order total_pages (%d)", event.PagesPrinted, totalPages)
	}
	if event.SheetsPrinted > totalSheets {
		return fmt.Errorf("update job progress: sheets_printed (%d) exceeds order total_sheets (%d)", event.SheetsPrinted, totalSheets)
	}

	// 4. Check existing progress timestamp to reject older/stale events
	var existingTimestampNS int64
	err = tx.QueryRowContext(ctx, s.rebind("SELECT timestamp_ns FROM job_progress WHERE job_id = ?;"), event.JobID).Scan(&existingTimestampNS)
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
		event.JobID,
		event.KioskID,
		string(event.Status),
		event.PagesPrinted,
		event.SheetsPrinted,
		event.ErrorReason,
		eventNS,
	)
	if err != nil {
		return fmt.Errorf("update job progress: upsert progress: %w", err)
	}

	// 6. Update associated order's status and updated_at_ns
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE job_id = ?;
`), string(event.Status), eventNS, event.JobID)
	if err != nil {
		return fmt.Errorf("update job progress: update order status: %w", err)
	}

	// Release PIN reservation upon terminal status
	if event.Status == contracts.JobStatusCompleted || event.Status == contracts.JobStatusFailedRefundable {
		_, _ = tx.ExecContext(ctx, s.rebind("DELETE FROM pin_reservations WHERE order_id = (SELECT order_id FROM orders WHERE job_id = ?);"), event.JobID)
	}

	// Synchronize dispatch intent status with authoritative progress
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
	var (
		resJobID      string
		kioskID       string
		statusStr     string
		pagesPrinted  int
		sheetsPrinted int
		errorReason   string
		timestampNS   int64
	)

	err := s.db.QueryRowContext(ctx, s.rebind(query), jobID).Scan(
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
// - completedThreshold applies to orders in terminal states (COMPLETED, PAUSED_ERROR, FAILED_REFUNDABLE).
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
SELECT order_id FROM orders
WHERE (status IN ('COMPLETED', 'PAUSED_ERROR', 'FAILED_REFUNDABLE') AND updated_at_ns <= ?)
   OR (status = 'PENDING_PAYMENT' AND created_at_ns <= ?);
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
// on the given kiosk by active unexpired unpaid sessions and active/in-flight paid orders,
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
    SELECT pc.total_sheets AS committed_sheets
    FROM orders o
    JOIN print_configs pc ON o.order_id = pc.order_id
    JOIN kiosk_sessions ks ON o.order_id = ks.order_id
    WHERE o.kiosk_id = ?
      AND o.status = 'PENDING_PAYMENT'
      AND ks.status = 'PAYMENT_PENDING'
      AND ks.expires_at_ns > ?
      AND (? = '' OR o.order_id != ?)
    GROUP BY o.order_id, pc.total_sheets

    UNION ALL

    SELECT 
        CASE 
            WHEN (pc.total_sheets - COALESCE(jp.sheets_printed, 0)) > 0 
            THEN (pc.total_sheets - COALESCE(jp.sheets_printed, 0))
            ELSE 0 
        END AS committed_sheets
    FROM orders o
    JOIN print_configs pc ON o.order_id = pc.order_id
    LEFT JOIN job_progress jp ON o.job_id = jp.job_id
    WHERE o.kiosk_id = ?
      AND o.status IN ('PAID_WAITING', 'DISPATCHED', 'SPOOLING', 'PRINTING', 'PAUSED_ERROR')
      AND (? = '' OR o.order_id != ?)
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

	// 1. Verify order existence, kiosk ownership, status, and expiry
	var (
		orderKioskID   string
		statusStr      string
		pricePaise     int64
		pinExpiresAtNS int64
	)
	queryOrder := s.rebind(`
SELECT kiosk_id, status, price_paise, pin_expires_at_ns
FROM orders
WHERE order_id = ?;
`)
	err = tx.QueryRowContext(ctx, queryOrder, orderID).Scan(&orderKioskID, &statusStr, &pricePaise, &pinExpiresAtNS)
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
		var totalSheets int
		querySheets := s.rebind(`SELECT total_sheets FROM print_configs WHERE order_id = ?;`)
		err = tx.QueryRowContext(ctx, querySheets, orderID).Scan(&totalSheets)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrOrderNotFound
			}
			return nil, fmt.Errorf("create kiosk session: query print config: %w", err)
		}
		if totalSheets <= 0 {
			totalSheets = 1
		}

		committedSheets, err := s.getCommittedPaperSheetsTx(ctx, tx, kioskID, orderID, nowNS)
		if err != nil {
			return nil, fmt.Errorf("create kiosk session: check paper commitments: %w", err)
		}
		effectiveAvailable := availableStock - committedSheets
		if effectiveAvailable < totalSheets {
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
	nowNS := now.UTC().UnixNano()
	query := s.rebind(`
UPDATE kiosk_sessions
SET status = 'CANCELED', updated_at_ns = ?
WHERE session_id = ? AND kiosk_id = ? AND status = 'PAYMENT_PENDING';
`)
	res, err := s.db.ExecContext(ctx, query, nowNS, sessionID, kioskID)
	if err != nil {
		return fmt.Errorf("cancel kiosk session: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		var currentStatus string
		err := s.db.QueryRowContext(ctx, s.rebind(`SELECT status FROM kiosk_sessions WHERE session_id = ? AND kiosk_id = ?;`), sessionID, kioskID).Scan(&currentStatus)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSessionNotFound
			}
			return err
		}
	}

	// Cancel any pending or unacknowledged dispatch intents for this session
	_, _ = s.db.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET status = 'CANCELED', updated_at_ns = ?
WHERE session_id = ? AND status IN ('PENDING', 'SENT');
`), nowNS, sessionID)

	// Revert order status from DISPATCHED to PAID_WAITING if not already durably acknowledged
	_, _ = s.db.ExecContext(ctx, s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = (SELECT order_id FROM kiosk_sessions WHERE session_id = ?)
  AND status = ?
  AND order_id NOT IN (SELECT order_id FROM dispatch_intents WHERE status = 'ACKNOWLEDGED');
`), string(contracts.JobStatusPaidWaiting), nowNS, sessionID, string(contracts.JobStatusDispatched))

	return nil
}

// CompleteKioskSession marks any active PAYMENT_PENDING session for an order as COMPLETED.
func (s *Store) CompleteKioskSession(ctx context.Context, orderID string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	nowNS := now.UTC().UnixNano()
	query := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE order_id = ? AND status = 'PAYMENT_PENDING';
`)
	res, err := s.db.ExecContext(ctx, query, nowNS, orderID)
	if err != nil {
		return fmt.Errorf("complete kiosk session: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrSessionNotFound
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
	ActiveSessionID  string
	PricePaise       int64
	KioskID          string
	Command          *contracts.PrintJobCommand
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
	if p.OrderID == "" || p.PaymentID == "" {
		return nil, errors.New("orderID and paymentID cannot be empty")
	}
	nowNS := now.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("capture payment: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Check if payment record already exists
	var existingPaymentStatus string
	checkPayQuery := s.rebind(`SELECT status FROM payments WHERE payment_id = ?;`)
	err = tx.QueryRowContext(ctx, checkPayQuery, p.PaymentID).Scan(&existingPaymentStatus)
	if err == nil {
		// Already recorded
		order, oErr := s.GetOrder(ctx, p.OrderID)
		if oErr != nil || order == nil {
			return &PaymentCaptureResult{AlreadyProcessed: true}, nil
		}
		return &PaymentCaptureResult{
			OrderStatus:      order.Status,
			AlreadyProcessed: true,
			DispatchEligible: false,
			PricePaise:       order.PricePaise,
			KioskID:          order.Command.KioskID,
			Command:          &order.Command,
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

	// 3. Verify amount against authoritative price
	if p.AmountPaise > 0 && p.AmountPaise != pricePaise {
		return nil, fmt.Errorf("payment amount mismatch: expected %d paise, got %d paise", pricePaise, p.AmountPaise)
	}

	currentStatus := contracts.JobStatus(statusStr)

	// 4. Check if order is already terminal or dispatched
	if currentStatus == contracts.JobStatusCompleted ||
		currentStatus == contracts.JobStatusPrinting ||
		currentStatus == contracts.JobStatusSpooling ||
		currentStatus == contracts.JobStatusDispatched {
		insertPay := s.rebind(`
INSERT INTO payments (payment_id, order_id, kiosk_id, session_id, amount_paise, status, provider, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
`)
		_, _ = tx.ExecContext(ctx, insertPay, p.PaymentID, p.OrderID, orderKioskID, p.SessionID, pricePaise, p.Status, p.Provider, nowNS)
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("capture payment: commit tx: %w", err)
		}
		order, _ := s.GetOrder(ctx, p.OrderID)
		var cmd *contracts.PrintJobCommand
		if order != nil {
			cmd = &order.Command
		}
		return &PaymentCaptureResult{
			OrderStatus:      currentStatus,
			DispatchEligible: false,
			AlreadyProcessed: true,
			PricePaise:       pricePaise,
			KioskID:          orderKioskID,
			Command:          cmd,
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
		rows, _ := res.RowsAffected()
		if rows == 0 {
			var newStatus string
			_ = tx.QueryRowContext(ctx, s.rebind(`SELECT status FROM orders WHERE order_id = ?;`), p.OrderID).Scan(&newStatus)
			currentStatus = contracts.JobStatus(newStatus)
		} else {
			currentStatus = contracts.JobStatusPaidWaiting
		}
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
			updateSession := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE session_id = ? AND status = 'PAYMENT_PENDING';
`)
			resSess, err := tx.ExecContext(ctx, updateSession, nowNS, sessID)
			if err == nil {
				rowsSess, _ := resSess.RowsAffected()
				if rowsSess > 0 {
					updateDispatched := s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ? AND status = ?;
`)
					resOrd, err := tx.ExecContext(ctx, updateDispatched, string(contracts.JobStatusDispatched), nowNS, p.OrderID, string(contracts.JobStatusPaidWaiting))
					if err == nil {
						rowsOrd, _ := resOrd.RowsAffected()
						if rowsOrd > 0 {
							dispatchEligible = true
							activeSessionID = sessID
							currentStatus = contracts.JobStatusDispatched

							// Record durable dispatch intent in PENDING status
							_, _ = tx.ExecContext(ctx, s.rebind(`DELETE FROM dispatch_intents WHERE order_id = ?;`), p.OrderID)
							insertIntent := s.rebind(`
INSERT INTO dispatch_intents (order_id, job_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, 'PENDING', ?, ?);
`)
							_, _ = tx.ExecContext(ctx, insertIntent, p.OrderID, orderJobID, orderKioskID, sessID, nowNS, nowNS)
						}
					}
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
	_, err = tx.ExecContext(ctx, insertPay, p.PaymentID, p.OrderID, orderKioskID, recordSessionID, pricePaise, p.Status, p.Provider, nowNS)
	if err != nil {
		return nil, fmt.Errorf("capture payment: insert payment record: %w", err)
	}

	// Update matching payment_attempts to PAID
	updateAtt := s.rebind(`
UPDATE payment_attempts
SET status = 'PAID', provider_payment_id = ?, updated_at_ns = ?
WHERE order_id = ? AND status = 'CREATED';
`)
	_, _ = tx.ExecContext(ctx, updateAtt, p.PaymentID, nowNS, p.OrderID)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("capture payment: commit tx: %w", err)
	}

	order, _ := s.GetOrder(ctx, p.OrderID)
	var cmd *contracts.PrintJobCommand
	if order != nil {
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

	// 3. Mark session COMPLETED
	updateSession := s.rebind(`
UPDATE kiosk_sessions
SET status = 'COMPLETED', updated_at_ns = ?
WHERE session_id = ? AND status = 'PAYMENT_PENDING';
`)
	resSess, err := tx.ExecContext(ctx, updateSession, nowNS, sessID)
	if err != nil {
		return nil, fmt.Errorf("release paid order: complete session: %w", err)
	}
	rowsSess, _ := resSess.RowsAffected()
	if rowsSess == 0 {
		return nil, ErrSessionNotFound
	}

	// 4. Mark order DISPATCHED
	updateOrder := s.rebind(`
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ? AND status = ?;
`)
	resOrd, err := tx.ExecContext(ctx, updateOrder, string(contracts.JobStatusDispatched), nowNS, orderID, string(contracts.JobStatusPaidWaiting))
	if err != nil {
		return nil, fmt.Errorf("release paid order: update order status: %w", err)
	}
	rowsOrd, _ := resOrd.RowsAffected()
	if rowsOrd == 0 {
		return nil, fmt.Errorf("order state conflict: could not transition to DISPATCHED")
	}

	// 5. Record durable dispatch intent in PENDING status
	_, _ = tx.ExecContext(ctx, s.rebind(`DELETE FROM dispatch_intents WHERE order_id = ?;`), orderID)
	insertIntent := s.rebind(`
INSERT INTO dispatch_intents (order_id, job_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, 'PENDING', ?, ?);
`)
	if _, err := tx.ExecContext(ctx, insertIntent, orderID, orderJobID, kioskID, sessID, nowNS, nowNS); err != nil {
		return nil, fmt.Errorf("release paid order: record dispatch intent: %w", err)
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

// MarkDispatchIntentSent marks the dispatch intent for an order or job as SENT.
func (s *Store) MarkDispatchIntentSent(ctx context.Context, orderOrJobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	orderOrJobID = strings.TrimSpace(orderOrJobID)
	if orderOrJobID == "" {
		return errors.New("orderOrJobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'SENT', dispatched_at_ns = ?, updated_at_ns = ?
WHERE (order_id = ? OR job_id = ?) AND status = 'PENDING';
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, nowNS, orderOrJobID, orderOrJobID)
	return err
}

// MarkDispatchIntentAcknowledged records that the kiosk daemon has durably accepted and persisted the job.
func (s *Store) MarkDispatchIntentAcknowledged(ctx context.Context, orderOrJobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	orderOrJobID = strings.TrimSpace(orderOrJobID)
	if orderOrJobID == "" {
		return errors.New("orderOrJobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'ACKNOWLEDGED', acknowledged_at_ns = ?, updated_at_ns = ?
WHERE (order_id = ? OR job_id = ?) AND status IN ('PENDING', 'SENT');
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, nowNS, orderOrJobID, orderOrJobID)
	return err
}

// MarkDispatchIntentCompleted records that physical printing has completed.
func (s *Store) MarkDispatchIntentCompleted(ctx context.Context, orderOrJobID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialized")
	}
	orderOrJobID = strings.TrimSpace(orderOrJobID)
	if orderOrJobID == "" {
		return errors.New("orderOrJobID cannot be empty")
	}
	nowNS := time.Now().UTC().UnixNano()
	query := s.rebind(`
UPDATE dispatch_intents
SET status = 'COMPLETED', updated_at_ns = ?
WHERE (order_id = ? OR job_id = ?) AND status != 'COMPLETED';
`)
	_, err := s.db.ExecContext(ctx, query, nowNS, orderOrJobID, orderOrJobID)
	return err
}

// GetDispatchIntentStatus returns the status and acknowledged_at_ns for an order's dispatch intent.
func (s *Store) GetDispatchIntentStatus(ctx context.Context, orderID string) (status string, acknowledgedAtNS int64, err error) {
	if s == nil || s.db == nil {
		return "", 0, errors.New("store not initialized")
	}
	query := s.rebind(`SELECT status, acknowledged_at_ns FROM dispatch_intents WHERE order_id = ?;`)
	err = s.db.QueryRowContext(ctx, query, orderID).Scan(&status, &acknowledgedAtNS)
	return status, acknowledgedAtNS, err
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
		_, _ = s.db.ExecContext(ctx, s.rebind(`UPDATE dispatch_intents SET retry_count = retry_count + 1, updated_at_ns = ? WHERE order_id = ?;`), nowNS, cand.orderID)

		ord, err := s.GetOrder(ctx, cand.orderID)
		if err == nil && ord != nil {
			cmds = append(cmds, ord.Command)
		}
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
    status, qr_payload, qr_image_url, idempotency_key,
    expires_at_ns, created_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`)
	_, err := s.db.ExecContext(ctx, query,
		att.AttemptID, att.OrderID, att.KioskID, att.SessionID, att.Provider,
		att.ProviderOrderID, att.ProviderPaymentID, att.AmountPaise, att.Currency,
		att.Status, att.QRPayload, att.QRImageURL, att.IdempotencyKey,
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
       status, qr_payload, qr_image_url, idempotency_key,
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
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey,
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
       status, qr_payload, qr_image_url, idempotency_key,
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
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey,
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
       status, qr_payload, qr_image_url, idempotency_key,
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
		&att.Status, &att.QRPayload, &att.QRImageURL, &att.IdempotencyKey,
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
