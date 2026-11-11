-- XeroxGo Supabase PostgreSQL Schema
-- Canonical idempotent table definitions for orders, print configs, pricing tiers, ads, conversions, and job progress.

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

CREATE TABLE IF NOT EXISTS print_configs (
    order_id TEXT PRIMARY KEY REFERENCES orders(order_id) ON DELETE CASCADE,
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
    price_per_sheet_paise BIGINT NOT NULL CHECK (price_per_sheet_paise > 0),
    bw_price_per_sheet_paise BIGINT NOT NULL DEFAULT 200 CHECK (bw_price_per_sheet_paise > 0),
    color_price_per_sheet_paise BIGINT NOT NULL DEFAULT 1000 CHECK (color_price_per_sheet_paise > 0),
    bw_simplex_price_paise BIGINT NOT NULL DEFAULT 200 CHECK (bw_simplex_price_paise > 0),
    bw_duplex_price_paise BIGINT NOT NULL DEFAULT 150 CHECK (bw_duplex_price_paise > 0),
    color_simplex_price_paise BIGINT NOT NULL DEFAULT 1000 CHECK (color_simplex_price_paise > 0),
    color_duplex_price_paise BIGINT NOT NULL DEFAULT 750 CHECK (color_duplex_price_paise > 0),
    color_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS pricing_tiers (
    id TEXT PRIMARY KEY,
    min_pages INTEGER NOT NULL CHECK (min_pages > 0),
    max_pages INTEGER NOT NULL CHECK (max_pages >= min_pages),
    bw_discount_percent NUMERIC(5,2) NOT NULL DEFAULT 0.0 CHECK (bw_discount_percent >= 0 AND bw_discount_percent <= 100),
    color_discount_percent NUMERIC(5,2) NOT NULL DEFAULT 0.0 CHECK (color_discount_percent >= 0 AND color_discount_percent <= 100),
    updated_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_webhooks (
    webhook_id TEXT PRIMARY KEY,
    received_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS job_progress (
    job_id TEXT PRIMARY KEY REFERENCES orders(job_id) ON DELETE CASCADE,
    kiosk_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    timestamp_ns BIGINT NOT NULL
);

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



