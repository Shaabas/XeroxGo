# XeroxGo — engineering task queue

Statuses: TODO / IN_PROGRESS / BLOCKED / REVIEW / DONE. A merged PR needs evidence; approval and deployment are separate.

| ID | Status | Owner | Goal / evidence | Next action |
| --- | --- | --- | --- | --- |
| XG-001 | REVIEW | ChatGPT | Integrate documentation-only dual-agent workspace in PR #10 | Retargeted to main; refresh docs, verify checks and merge only when ready |
| XG-002 | DONE (GitHub scope) | ChatGPT + human | Pricing PR #9 merged; initial tariff migration/activation previously verified | Verify actual live backend deployment/rates separately before assuming production readiness |
| XG-003 | DONE (GitHub scope) | ChatGPT + human | PRs #11, #12, #13, #14 merged; CI and frontend deploy job for main `c3baa56` passed | Live smoke tests still separate |
| XG-004 | TODO | ChatGPT | Audit multi-document mixed simplex/duplex through merged PDF and Pi CUPS | Analysis and human architecture review before implementation |
| XG-005 | BLOCKED | Antigravity | Implement an approved XG-004 design | Requires XG-004 decision |
| XG-006 | TODO | ChatGPT | Assess existing Supabase table RLS/grants without breaking backend | Analysis-only first |
| XG-007 | TODO | Joint | Verify Antigravity discovery of `.agents/skills` in actual local checkout | User/local check required |
| XG-CHECKOUT-FAST-1 | TODO | Antigravity (after scope approval) | Remove redundant checkout final re-quote passes; retain authoritative final normalization and tariff/price verification. Add honest upload progress and processing state, protect multi-file, stale pricing and retry failure cases | Baseline network/PDF timing, scoped design, tests and human review before merging |

## Next task: XG-CHECKOUT-FAST-1
- **Architecture:** front-end reuses server-confirmed quote and tariff revision for the final order request; server still recalculates price against normalized output and rejects stale/mismatched submissions.
- **UX:** provide actual upload transfer progress and an indeterminate state for PDF processing and order creation. Never simulate a meaningful percentage without backend signals.
- **Do not:** add a new storage service, bypass backend validation, generate PIN before a durable order exists, or silently retry an upload that may have created an order.
- **Tests:** one/many files, different print settings, changing tariff, server mismatch, failed upload, canceled/disconnected network, duplicate-click, and eventual PIN response.
- **Measurements:** collect client upload/response and server conversion/normalization/database timing without logging document content or credentials.

## Task contract
`Task ID | owner | branch/worktree | allowed files | decision/constraints | acceptance criteria | test evidence | blockers | handoff`.

Historical task snapshots are archived in previous commits; this is the active queue and should be updated after each integration.

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
