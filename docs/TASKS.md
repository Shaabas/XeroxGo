# XeroxGo — engineering task queue

Statuses: TODO / IN_PROGRESS / BLOCKED / REVIEW / DONE. A merged PR needs evidence; approval and deployment are separate.

| ID | Status | Owner | Goal / evidence | Next action |
| --- | --- | --- | --- | --- |
| XG-001 | DONE (GitHub scope) | ChatGPT | PR #10 workspace merged at `fa148fee`; docs index and agent contract created | Verify Antigravity local skill discovery separately under XG-007 |
| XG-002 | DONE (GitHub scope) | ChatGPT + human | Pricing PR #9 merged; initial tariff migration/activation previously verified | Verify actual live backend deployment/rates separately before assuming production readiness |
| XG-003 | DONE (GitHub scope) | ChatGPT + human | PRs #11, #12, #13, #14 merged; CI and frontend deploy job for main `c3baa56` passed | Live smoke tests still separate |
| XG-004 | TODO | ChatGPT | Audit multi-document mixed simplex/duplex through merged PDF and Pi CUPS | Analysis and human architecture review before implementation |
| XG-005 | BLOCKED | Antigravity | Implement an approved XG-004 design | Requires XG-004 decision |
| XG-006 | TODO | ChatGPT | Assess existing Supabase table RLS/grants without breaking backend | Analysis-only first |
| XG-007 | TODO | Joint | Verify Antigravity discovery of `.agents/skills` in actual local checkout | User/local check required |
| XG-CHECKOUT-FAST-1 | DONE (GitHub scope) | ChatGPT | PR #16 merged at 0b40181; removed synchronous checkout re-quotes and added real upload progress + Gate 6 tests | Benchmark live checkout separately; quote preparation remains expensive |
| XG-CLOUD-DOCS-V2 | IN_PROGRESS (foundation only) | ChatGPT | `feat/cloud-first-documents-v2`: private Storage migration file, signed upload/verification APIs, canonical immutable variant preparation and deduplication, isolated browser transport + tests; old checkout untouched | Validate Gate 1–6, inspect storage credentials/config/security, then implement durable preparation and reference-only checkout as separate reviewed slice |

## Historical task: XG-CHECKOUT-FAST-1
- **Architecture:** front-end reuses server-confirmed quote and tariff revision for the final order request; server still recalculates price against normalized output and rejects stale/mismatched submissions.
- **UX:** provide actual upload transfer progress and an indeterminate state for PDF processing and order creation. Never simulate a meaningful percentage without backend signals.
- **Do not:** add a new storage service, bypass backend validation, generate PIN before a durable order exists, or silently retry an upload that may have created an order.
- **Tests:** one/many files, different print settings, changing tariff, server mismatch, failed upload, canceled/disconnected network, duplicate-click, and eventual PIN response.
- **Measurements:** collect client upload/response and server conversion/normalization/database timing without logging document content or credentials.

## Task contract
`Task ID | owner | branch/worktree | allowed files | decision/constraints | acceptance criteria | test evidence | blockers | handoff`.

Historical task snapshots are archived in previous commits; this is the active queue and should be updated after each integration.

| XG-DOC-CLEANUP | REVIEW | ChatGPT | PR #15 removes obsolete audit snapshots, preserves five detailed contract/test/migration references, carries forward unique gaps | Verify CI and changed-file scope before merge |

## Legacy-audit findings retained as review tasks

The removed historical reports are **not current defect confirmations**. Their actionable findings are retained here pending verification against present code and tests:

| ID | Status | Investigation required |
| --- | --- | --- |
| XG-008 | TODO | Re-audit the kiosk-facing legacy route aliases (`/pin/lookup`, `/confirm`, `/cancel-session`, order-scoped payment initiation) and dev-only payment simulation. Check actual Pi consumers and regression tests before deleting APIs. |
| XG-009 | TODO | Verify TTL cleanup for abandoned `pin_reservations`, `kiosk_sessions`, and `payment_attempts`; design a conservative cleanup process only if records can accumulate unsafely. |
| XG-010 | TODO | Verify printer health behavior when SNMP is unavailable but CUPS reports online; ensure degraded telemetry is visible and jams are not falsely marked completed. |
| XG-011 | TODO | Review old recommendations on CUPS vs PJL against actual Brother HL-L5210DN, HP demo hardware and IPP capabilities. Do **not** remove an engine solely on old audit claims. |
| XG-012 | TODO | Verify dropped/in-memory deferred kiosk jobs are recoverable after restart/reconnect without duplicate prints; preserve durable dispatch semantics. |
| XG-013 | TODO | Recheck any PostgreSQL concurrent session/paper-stock claims against current DB schema, advisory/row locks and existing tests before changing indexes or migrations. |
| XG-014 | TODO | Add or verify frontend end-to-end acceptance for document selection, N-Up/duplex price display, file collation, admin rate changes, and PIN workflow. Existing `conversion_test.js` and arithmetic parity tests do **not** by themselves prove browser UX or physical output. |

These are lower-priority investigations than the approved planning work for XG-CHECKOUT-FAST-1. Hardware tests and database migration reviews require separate scope and approval.


## XG-CLOUD-DOCS-V2 implementation guardrails
- Current work is **foundation only**; no production migration, Storage bucket, credentials, browser cutover, or payment/printer changes authorized.
- Architecture details, acceptance criteria and rollback are in `docs/architecture/cloud-first-documents-v2.md`.
- Do NOT turn `XEROXGO_CLOUD_UPLOADS_ENABLED` on until server-side upload rate limits/quotas, Supabase CORS, preflight tests and object cleanup have been implemented.
- Pending implementation: secure prepared variants, reference-only checkout, order idempotency, Storage-backed printer downloads, end-to-end tests and rollout.

- Prepared-variant foundation now implemented: verified original re-download, canonical normalization, immutable private Storage output, DB CAS READY, exact-setting reuse and tests. **Not yet async/durable**; request crashes can leave PREPARING and no retry lease exists. Do not enable.

### XG-CLOUD-DOCS-V2 reference-only checkout slice (draft PR #17)
- Added feature-gated single-variant `POST /api/documents/checkout`: upload capability, variant ID, stable idempotency key, accepted total and revision only; no PDF bytes.
- Added transactionally bound READY-variant+tariff check, order/PIN/print config/immutable variant reference and idempotency ledger. Retries must reuse the original order; new idempotency keys cannot reorder the same upload variant.
- Added signed Render PDF download backing from private Storage with checksum verification before bytes reach the Pi. Legacy jobs remain disk-backed.
- Added isolated browser checkout request helper and Go/Node regression tests. **Cart integration and multi-file packing not yet implemented; not production ready.**
- Blockers for release: Gate 1–6 latest verification, live Storage integration, durable preparation worker retry/cleanup, upload rate limits and quotas, multi-file fidelity audit XG-004, secure lifecycle/retention and Pi recovery testing.

- XG-CLOUD-DOCS-V2 recovery slice: 10-minute DB preparation lease, 8-minute worker execution bound, atomic stale PREPARING/FAILED recovery using new variant IDs, stale-publisher protection and SQLite tests. Still synchronous request-driven preparation, NOT durable background queue. Multi-file printer fidelity audited in `docs/architecture/multi-document-fidelity-gate.md`; no print behavior change approved. No live Storage migration or deployment.

### V2 upload/storage guardrails (draft; 2026-11-18)
- **Done on branch:** Durable per-kiosk 20-upload/100-MiB-per-hour reservation quota, HTTP 429 exhaustion, transaction lock, targeted store tests. Kiosk identifiers are untrusted, so globally enforced edge/session abuse controls still block deployment.
- **Done on branch:** Opt-in 5-minute cleanup of expired, database-known unreferenced originals/variants, safe Storage DELETE path validation, retry after failed deletion, never clean in-flight/ordered files. Cleanup flag OFF by default; live Supabase untouched.
- **Still to do:** Trusted session quotas/edge rate limiting, durable independent preparation queue, cleanup of orphan Storage objects not represented in DB, terminal-order retention policy, all-format uploads, full printer-safe multi-file handling, cart integration, Supabase/Render/Pi staging tests and latest CI gate approval.

### Durable background PDF preparation slice (2026-11-18)
- **Implemented on draft branch:** Private `cloud_preparation_jobs` schema, canonical settings snapshot+tariff revision, deduplicated enqueue returning HTTP 202 and preparation ID, dedicated independent Render polling worker, atomic PostgreSQL/SQLite claim, 8-minute processing bound, 11-minute queue lease, 10-minute canonical variant lease, up to three automatic attempts and terminal crash reconciliation.
- **Implemented on draft branch:** Capability-scoped `/api/documents/preparations/status` reports QUEUED/PROCESSING/RECOVERING/READY/FAILED; only READY contains canonical quote and immutable variant ID.
- Both upload and `XEROXGO_CLOUD_ASYNC_PREP_ENABLED` must be enabled to activate background mode; flags default OFF. Legacy checkout and Pi are unchanged.
- **To verify:** CI Gate 1–6 for latest commit, PostgreSQL concurrent claims and forced process restart, real Supabase Storage, error recovery, browser reconnect and upload retention semantics. Queue migration not applied.
- **Still release blockers:** global quota/abuse controls, multi-file print fidelity, full browser cart integration, orphan Storage reconciliation, completed order retention and real Pi/printer regression.

### XG-004 multi-file fidelity foundation — 2026-11-18
- Implemented **read-only** `BuildCloudPrintPlan`, `LoadReadyCloudPrintPlan`, and opt-in `POST /api/documents/batch/quote`: reads READY same-kiosk, unexpired, verified prepared variants, keeps each document's actual duplex/color/physical sheet count, validates 1–20 document batch, checksum/path, tariff revision, duplicates and total overflow.
- Added private `cloud_print_tasks` migration schema as a ledger for future per-document durable Pi work and a fail-closed transition model. Uncertain CUPS dispatch must go to RECONCILE, and a next task must not proceed before prior task is physically confirmed. Tests cover invalid transitions, mixed color/duplex, invalid references and price calculations.
- **No multi-file checkout or Pi dispatch is enabled**; `checkout_supported=false` for multi-file quote. Future task: **single atomic checkout** for all variants+PIN+idempotency+task creation, versioned per-task Pi commands, CUPS per-task persistence and physical recovery, emulator and physical printer validation.
- No legacy upload, customer UI, payment or printer changes. All feature flags remain OFF and migration unapplied.
\n### XG-CLOUD-DOCS-V2 multi-file checkout boundary (2026-11-18)\n- **Done on draft branch (transaction gate):** `LoadReadyCloudPrintPlanTx` locks active tariff and validated READY upload/variant rows inside the caller's SQL transaction, in deterministic lock order; it preserves original document sequence and rejects duplicate uploads/variants, foreign kiosks, expired records and stale tariffs. SQLite regression tests added.\n- **Next:** Design and implement an isolated atomic 1–20 document checkout transaction that verifies every upload capability before entering the transaction, revalidates the authenticated upload IDs and all authoritative variant rows *inside* that transaction, persists one PIN/order plus all order_documents and cloud_print_tasks, and provides crash-safe idempotency. Do not reuse the old single-document request ledger without an explicit schema strategy.\n- **Still blocked:** Versioned per-task Pi contract, durable CUPS acknowledgement/reconciliation and real printer tests. Never wire multi-file tasks through the existing global duplex/color `PrintJobCommand`. No production migrations, dispatch, or checkout cutover without review.\n