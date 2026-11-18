# XG-006 — Supabase public-table RLS and grants audit (2026-11-18)

Status: **READ-ONLY FINDINGS — NO LIVE CHANGES MADE**. Project: XeroxGo. Do not execute the example remediation blindly; review application DB roles, data API usage, integrations, rollback and a staging rehearsal first.

## Evidence (connected Supabase, 2026-11-18)

- Project is ACTIVE_HEALTHY; PostgreSQL 17.
- Installed migration history lists only `20261117082548 canonical_sheet_pricing` and `20261117085500 secure_canonical_sheet_pricing_rls`. The cloud-documents V2 migration from PR #17 is not applied.
- Exactly 16 existing public tables have `rls_enabled = false`: `orders`, `print_configs`, `pricing`, `payment_webhooks`, `job_progress`, `ads`, `admin_users`, `admin_sessions`, `conversions`, `recovery_records`, `pin_reservations`, `pin_rate_limits`, `kiosk_sessions`, `payments`, `dispatch_intents`, `payment_attempts`.
- Read-only query against `information_schema.role_table_grants` confirmed that both `anon` and `authenticated` have `SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER` grants on **each of those 16 tables**. Coupled with disabled RLS, this is a serious Data API exposure where the public schema is exposed to those roles.
- The two tariff tables have RLS enabled and are separate from this finding.
- No customer rows or secrets were retrieved during this audit.

## Required risk assessment

1. Confirm Data API exposed schemas, actual anon-key exposure and any frontend or third-party usage of PostgREST before making permission changes. Treat current grants as a critical security concern even if normal application traffic uses the privileged Go backend.
2. Inventory every database connection role and code route; distinguish Render's trusted direct PostgreSQL connection from `anon`/`authenticated` Data API calls. Map required reads/writes per table.
3. Build an explicit restrictive policy/grant matrix. Prefer no Data API access for internal order, payment, PIN, admin and recovery tables unless verified clients genuinely require it.
4. Stage and test `REVOKE` and/or RLS enablement plus only the required policies. Test both untrusted Data API roles (must fail) and Render's legitimate operations (must pass). Record rollback SQL.
5. Execute production remediation only with explicit human production/security approval. Monitor live order, PIN, admin and payment paths after rollout.

Do **not** run a blanket `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` without route testing: this may block legitimate accesses. Do **not** automatically grant `anon`/`authenticated` broad bypass policies to compensate. A service-role key must never be placed in the browser.

Reference: https://supabase.com/docs/guides/database/postgres/row-level-security

## Separate cloud-document V2 release gate

The draft V2 migration defines RLS and revokes client grants for new document/upload/variant/order-reference/task tables. It has not been applied and does not remediate the 16 preexisting public tables. Keep the concerns independent when evaluating PR #17.
