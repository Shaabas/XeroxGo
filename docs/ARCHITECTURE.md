# XeroxGo — system architecture (concise map)

This is an orientation map, **not** a replacement for code-level contracts. Confirm paths against the current commit; older forensic audits may be outdated.

## Runtime boundaries
1. **Frontend** (`frontend/`): mobile customer portal and admin UI, document upload, print-option selection, preview/quote display. It does not authorize prices.
2. **Cloud backend** (`cloud-backend/`): Go HTTP APIs, canonical PDF normalization/imposition, versioned pricing, order/PIN creation, payment orchestration, dispatch and recovery coordination.
3. **Persistence** (`cloud-backend/internal/store/`, `deploy/supabase/`): PostgreSQL/Supabase for deployed system; SQLite support for local development. Durable orders, sessions, payments and tariff revisions.
4. **Kiosk daemon** (`kiosk-daemon/`): Raspberry Pi UI, PIN handling, cloud job download, local spooler state, CUPS/IPP print submission, printer health and recovery.
5. **Hardware**: CUPS queue and physical network printer; SNMP/IPP instrumentation when supported.

## Canonical flow
`Browser upload → Cloud PDF normalizer and authoritative quote → accepted quote/revision → order+PIN → kiosk claim/payment → durable dispatch → Pi spooler/CUPS → completion/reconciliation`.

## Non-negotiable invariants
- Frontend may show instant estimates using the server-published rates, but may not authorize or persist billed amounts. Only canonical cloud PDF normalization and pricing determine billable sides and accepted order prices; do not re-impose pages independently on Pi.
- Order creation validates the currently active tariff revision within the DB transaction.
- Enforce immutable accepted order price; no retroactive repricing.
- Preserve checksum / job provenance and idempotent printing/recovery behavior.
- Hardware status telemetry is advisory evidence; do not infer a completed print solely from an HTTP acknowledgment.
- Multiple documents may have different options; existing single-CUPS-job merge behavior requires an explicit fidelity audit (XG-004).

## Authoritative detail
- Current code: `cloud-backend/cmd/server/main.go`, `cloud-backend/cmd/server/pricing.go`, `cloud-backend/internal/pricing/calculator.go`, `cloud-backend/internal/store/`, `kiosk-daemon/internal/spooler/`.
- Contract models: `pkg/contracts/`.
- Tests: `docs/TESTING.md` and actual `*_test.go` / frontend tests.
- Historical detailed documents temporarily retained under `docs/architecture/`, `docs/workflows/`, `docs/testing/` pending consolidation review.
