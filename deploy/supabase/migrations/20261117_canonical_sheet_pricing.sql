-- XeroxGo canonical sheet tariff migration.
-- Safe to run against the VERIFIED development database after schema inspection.
-- This migration is additive: it does not modify orders, payments, or legacy tables.
-- Legacy table retirement is a separate operation requiring remote dependency checks.
BEGIN;

CREATE TABLE IF NOT EXISTS sheet_tariff_versions (
    revision BIGINT PRIMARY KEY CHECK (revision > 0),
    bw_simplex_paise BIGINT NOT NULL CHECK (bw_simplex_paise BETWEEN 1 AND 1000000),
    bw_duplex_sheet_paise BIGINT NOT NULL CHECK (bw_duplex_sheet_paise BETWEEN 1 AND 1000000),
    color_simplex_paise BIGINT NOT NULL CHECK (color_simplex_paise BETWEEN 1 AND 1000000),
    color_duplex_sheet_paise BIGINT NOT NULL CHECK (color_duplex_sheet_paise BETWEEN 1 AND 1000000),
    color_enabled INTEGER NOT NULL CHECK (color_enabled IN (0, 1)),
    changed_by TEXT NOT NULL CHECK (length(changed_by) > 0),
    created_at_ns BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS sheet_tariff_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    active_revision BIGINT NOT NULL DEFAULT 0 CHECK (active_revision >= 0)
);

INSERT INTO sheet_tariff_state(id, active_revision)
VALUES (1, 0)
ON CONFLICT (id) DO NOTHING;

COMMIT;

-- Deliberately NO DROP TABLE: only the verified operator-approved
-- database cleanup may remove pricing and pricing_tiers.
