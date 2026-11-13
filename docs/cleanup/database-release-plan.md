# XeroxGo Database Release Plan: Orders & Sessions Concurrency Hardening

**Document Status:** Release Ready  
**Date:** 2026-11-13  
**Target:** PostgreSQL (Supabase Production) & Embedded SQLite  
**Scope:** Partial Unique Constraint (`idx_kiosk_sessions_active_order`), Startup Decoupling, Fail-Closed Preflight Checks, and Test Database Isolation.

---

## 1. Executive Summary

During the Task 4.3 architectural hardening audit, a partial unique index (`idx_kiosk_sessions_active_order`) was introduced to enforce invariant **INV-CONCUR-01** (at most one active `PAYMENT_PENDING` session per order). Subsequent validation in Task INT-4A identified three critical database release blockers that have now been resolved:

1. **Automatic Startup Migration Hazard:** Cloud backend startup previously attempted to execute `CREATE UNIQUE INDEX IF NOT EXISTS` via `pgMigrations`. In production PostgreSQL, synchronous index creation holds exclusive table locks, bypasses the operator preflight protocol, and violates zero-downtime deployment rules.
2. **Fail-Open Preflight Queries:** Database errors during preflight duplicate detection were previously ignored (`_ =` in PostgreSQL and `if errPre == nil && dups > 0` in SQLite), allowing store initialization to proceed even if the preflight check failed to execute.
3. **Unsafe Test Environment Variable Fallback:** Concurrency integration tests fell back to generic production environment variables (`DATABASE_URL`, `SUPABASE_URI`), creating a risk of destructive test operations running against staging or production databases.

This document specifies the exact production deployment sequence, operator preflight checks, index validity assertions, recovery protocols, and development/testing configurations.

---

## 2. Verified Database Blockers & Resolutions

| Blocker ID | Description | Risk | Resolution |
| :--- | :--- | :--- | :--- |
| **BLK-DB-01** | `pgMigrations` automatically created `idx_kiosk_sessions_active_order` during application startup. | Synchronous table lock on `kiosk_sessions` in production; bypass of DBA migration protocol; deployment failure if duplicates exist. | **Decoupled:** Removed index creation from `pgMigrations`. PostgreSQL startup never modifies this constraint. Production deployment is owned exclusively by standalone migration `20261113_active_kiosk_sessions_unique.sql`. |
| **BLK-DB-02** | PostgreSQL & SQLite preflight duplicate checks silently swallowed query errors. | Initialization continued during database disconnects, table locks, or corrupted metadata without verifying duplicate absence (fail-open). | **Fail-Closed:** Preflight checks in both PostgreSQL (`Open`) and SQLite (`migrateSQLiteSchema`) explicitly evaluate query errors and immediately abort initialization if the preflight check fails. |
| **BLK-DB-03** | Fresh dev/test databases lacked a supported, documented method to obtain the constraint. | Tests and fresh instances could silently run without the partial unique constraint. | **Supported APIs & DDL:** `deploy/supabase/schema.sql` provides the constraint for fresh SQL boots; `Store.EnsureActiveSessionConstraint(ctx)` provides the supported, programmatic method for test harnesses. |
| **BLK-DB-04** | PostgreSQL integration tests fell back to `DATABASE_URL` / `SUPABASE_URI`. | Running `go test` in environments with production/staging credentials could mutate production schemas and inject test rows. | **Strict Test Scoping:** Integration tests strictly require `XEROXGO_TEST_POSTGRES_URL` (or `POSTGRES_TEST_URL`). Generic production variables are explicitly ignored. |

---

## 3. Production Deployment Procedure

The production migration script is located at:  
[`deploy/supabase/migrations/20261113_active_kiosk_sessions_unique.sql`](file:///home/shabas/Projects/XeroxGo/deploy/supabase/migrations/20261113_active_kiosk_sessions_unique.sql)

### Step 1: Preflight Duplicate Detection (Read-Only)
Before applying any schema changes, execute the preflight detection query in the Supabase SQL Editor or `psql`:

```sql
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
```

- **Pass Condition:** The query returns **0 rows**. Proceed directly to Step 3.
- **Fail Condition:** The query returns **1 or more rows**. **DO NOT APPLY THE MIGRATION.** Proceed to Step 2.

### Step 2: Conflict Resolution Protocol
> [!CAUTION]
> Never delete or modify customer session records blindly. Conflicting active sessions represent concurrent claims that must be reconciled individually.

1. For each reported `order_id`:
   - Inspect the associated order status in `orders`:
     ```sql
     SELECT order_id, kiosk_id, status, pin_expires_at_ns FROM orders WHERE order_id = '<ORDER_ID>';
     ```
   - Inspect associated payment attempts in `payment_attempts`:
     ```sql
     SELECT attempt_id, session_id, status, amount_paise, created_at_ns 
     FROM payment_attempts 
     WHERE order_id = '<ORDER_ID>';
     ```
2. If one session has a captured or pending payment, preserve that session. Transition stale duplicate sessions to `'EXPIRED'` or `'CANCELED'`:
   ```sql
   UPDATE kiosk_sessions 
   SET status = 'EXPIRED', updated_at_ns = extract(epoch from now())::bigint * 1000000000 
   WHERE session_id = '<STALE_SESSION_ID>' AND status = 'PAYMENT_PENDING';
   ```
3. Re-run Step 1. Ensure the preflight query returns 0 rows before proceeding.

### Step 3: Standalone Concurrent Migration Execution
> [!IMPORTANT]
> `CREATE INDEX CONCURRENTLY` **cannot** run inside an explicit transaction block (`BEGIN ... COMMIT`).
> In `psql`, ensure autocommit is enabled (default). In Supabase SQL Editor, run this statement individually.

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_kiosk_sessions_active_order 
ON kiosk_sessions (order_id) 
WHERE status = 'PAYMENT_PENDING';
```

- `CONCURRENTLY` builds the index without acquiring an exclusive table lock, allowing live customer claims and reads to continue uninterrupted.

### Step 4: Index Validity Verification
Immediately verify that the index was successfully built and marked valid by the PostgreSQL query planner:

```sql
SELECT 
    c.relname AS index_name,
    pg_size_pretty(pg_relation_size(c.oid)) AS index_size,
    i.indisvalid AS is_valid,
    i.indisunique AS is_unique
FROM pg_class c
JOIN pg_index i ON c.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = 'idx_kiosk_sessions_active_order';
```

- **Pass Condition:** `is_valid = true` and `is_unique = true`.
- **Fail Condition:** If `is_valid = false`, the concurrent build encountered a conflict or interruption. Follow the recovery procedure below.

### Step 5: Safe Recovery for Interrupted or Invalid Index Builds
If Step 4 reports `is_valid = false`:
1. Drop the invalid index concurrently:
   ```sql
   DROP INDEX CONCURRENTLY IF EXISTS idx_kiosk_sessions_active_order;
   ```
2. Re-run Step 1 to detect any new duplicate sessions created while the invalid index was present.
3. Re-run Step 3 and Step 4.

### Step 6: Application Deployment
Deploy the updated XeroxGo cloud backend binary.
- On startup, the application runs `initPostgresSchema` and verifies preflight safety.
- Startup will **not** attempt to recreate `idx_kiosk_sessions_active_order`, ensuring zero migration interference with running services.

---

## 4. Development and Testing Environments

### Fresh PostgreSQL / Supabase Databases
For newly provisioned Supabase or PostgreSQL databases, execute the complete baseline schema:
```bash
psql "$XEROXGO_TEST_POSTGRES_URL" -f deploy/supabase/schema.sql
```
`deploy/supabase/schema.sql` (line 147) includes `idx_kiosk_sessions_active_order` as part of the initial table creation.

### Programmatic Test Suites
For Go integration tests and CI pipelines, use the supported method on `*store.Store`:
```go
err := s.EnsureActiveSessionConstraint(ctx)
if err != nil {
    // Fails closed if preflight detects duplicates or on database errors
}
```

### Migration Test Isolation
To prevent non-deterministic test race conditions and eliminate destructive operations on shared schemas:
- Tests must never drop, modify, or recreate an application's real index on shared test schemas (`public`).
- Migration lifecycle tests (`Postgres_IndexDeployment_StartupVsExplicit`) dynamically create a disposable PostgreSQL schema (`test_deploy_iso_<timestamp>`) via `search_path`, validating startup behavior and constraint installation strictly on test-owned objects.
- Cleanup is registered via Go's `t.Cleanup` to guarantee `DROP SCHEMA IF EXISTS ... CASCADE` executes even on test assertion failures or timeouts.

### Environment Variable Requirements for Integration Tests
- **Designated Variable:** `XEROXGO_TEST_POSTGRES_URL` (or legacy alias `POSTGRES_TEST_URL`).
- **Ignored Variables:** `DATABASE_URL`, `SUPABASE_URI`, `SUPABASE_DB_URL`, `POSTGRES_URL`.
- If no test URL is provided, tests report `[UNVERIFIED]` and skip cleanly without error.

---

## 5. Verification Matrix & Rollback Plan

### Invariant Verification Status
| Invariant | Description | SQLite Status | PostgreSQL Status | Enforcement Mechanism |
| :--- | :--- | :--- | :--- | :--- |
| **INV-CONCUR-01** | Single active `PAYMENT_PENDING` session per order | **VERIFIED** | **VERIFIED** | Partial unique index `idx_kiosk_sessions_active_order` + safe uniqueness conflict fallback in `CreateOrGetKioskSessionWithPaperCheck`. |
| **INV-CONCUR-02** | Paper stock overcommitment serialization | **VERIFIED** | **VERIFIED** | Transaction-scoped advisory lock `pg_advisory_xact_lock(22615, hashtext(kiosk_id))`. |
| **INV-CONCUR-03** | Fail-closed preflight migration safety | **VERIFIED** | **VERIFIED** | Immediate error return on query failure or detected duplicate rows during initialization. |

### Emergency Rollback Plan
If the partial unique index causes unexpected production behavior:
```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_kiosk_sessions_active_order;
```
The application code gracefully falls back to transaction-level locking and status verification, maintaining backward compatibility.

