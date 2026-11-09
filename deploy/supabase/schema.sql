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
    updated_at_ns BIGINT NOT NULL
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
