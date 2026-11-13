-- Migration: 20261113_active_kiosk_sessions_unique.sql
-- Goal: Prevent duplicate active (PAYMENT_PENDING) kiosk sessions per order.
-- Safety Invariant: INV-CONCUR-01 (Single Active Session Per Order)
--
-- IMPORTANT OPERATOR NOTICE:
-- DO NOT APPLY THIS MIGRATION AUTOMATICALLY TO PRODUCTION SUPABASE / POSTGRESQL.
-- Follow the phased preflight procedure below.
--
-- TRANSACTION BLOCK WARNING:
-- In PostgreSQL, `CREATE INDEX CONCURRENTLY` CANNOT be executed inside an explicit
-- transaction block (BEGIN ... COMMIT). Ensure your client (psql, Supabase SQL Editor,
-- migration runner) executes Phase 2 in AUTOCOMMIT mode as a standalone statement.

--------------------------------------------------------------------------------
-- Phase 1: Preflight Detection of Conflicting Duplicate Records
-- Run this check first. If it returns ANY rows, ABORT the migration immediately.
-- DO NOT automatically delete or update customer session records.
-- Operator must investigate each conflict, determine which session was paid/active,
-- and transition stale duplicates to 'EXPIRED' or 'CANCELED' manually.
--------------------------------------------------------------------------------
SELECT 
    order_id, 
    COUNT(1) AS active_session_count,
    array_agg(session_id) AS conflicting_session_ids,
    min(created_at_ns) AS earliest_created_at_ns,
    max(created_at_ns) AS latest_created_at_ns
FROM kiosk_sessions
WHERE status = 'PAYMENT_PENDING'
GROUP BY order_id
HAVING COUNT(1) > 1;

--------------------------------------------------------------------------------
-- Phase 2: Apply Database-Level Partial Unique Index
-- ONLY run if Phase 1 returns zero (0) rows.
-- MUST be executed OUTSIDE of any transaction block (autocommit mode).
-- CONCURRENTLY avoids table-level exclusive locks on kiosk_sessions during build.
--------------------------------------------------------------------------------
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_kiosk_sessions_active_order 
ON kiosk_sessions (order_id) 
WHERE status = 'PAYMENT_PENDING';

--------------------------------------------------------------------------------
-- Phase 3: Post-Migration Validity Verification
-- Verify the index is both recognized and marked VALID (indisvalid = true).
-- If indisvalid = false, the build failed and left an INVALID index that must be recovered.
--------------------------------------------------------------------------------
SELECT 
    c.relname AS index_name,
    pg_size_pretty(pg_relation_size(c.oid)) AS index_size,
    i.indisvalid AS is_valid,
    i.indisunique AS is_unique
FROM pg_class c
JOIN pg_index i ON c.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = 'idx_kiosk_sessions_active_order';

--------------------------------------------------------------------------------
-- Recovery Procedure for Failed Concurrent Migration
-- If Phase 2 is cancelled, deadlocks, or fails due to concurrent insert collisions,
-- PostgreSQL leaves the index in an INVALID state (indisvalid = false).
-- An invalid index incurs write overhead but cannot be used for queries or constraints.
-- Safe Recovery Steps:
-- 1. Drop the invalid index concurrently:
--      DROP INDEX CONCURRENTLY IF EXISTS idx_kiosk_sessions_active_order;
-- 2. Re-run Phase 1 preflight detection to inspect and resolve any conflicting active sessions.
-- 3. Re-run Phase 2 standalone statement in autocommit mode.
--------------------------------------------------------------------------------

--------------------------------------------------------------------------------
-- Rollback Plan
-- If rollback is required in production:
--------------------------------------------------------------------------------
-- DROP INDEX CONCURRENTLY IF EXISTS idx_kiosk_sessions_active_order;
