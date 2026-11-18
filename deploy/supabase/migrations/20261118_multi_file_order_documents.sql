-- XeroxGo Migration: Multi-File Independent Print Jobs Foundation
-- Creates order_documents table, migrates dispatch_intents to job-level PK,
-- and updates foreign keys for job_progress and recovery_records.

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

-- Add total_pages and total_sheets to orders table if missing
ALTER TABLE orders ADD COLUMN IF NOT EXISTS total_pages INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS total_sheets INTEGER NOT NULL DEFAULT 0;

-- Migrate dispatch_intents primary key to job_id
ALTER TABLE dispatch_intents DROP CONSTRAINT IF EXISTS dispatch_intents_pkey;
ALTER TABLE dispatch_intents ADD PRIMARY KEY (job_id);
CREATE INDEX IF NOT EXISTS idx_dispatch_intents_order ON dispatch_intents (order_id);

-- Remove obsolete foreign key on job_progress pointing to orders(job_id)
ALTER TABLE job_progress DROP CONSTRAINT IF EXISTS job_progress_job_id_fkey;
