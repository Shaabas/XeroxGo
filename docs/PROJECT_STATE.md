> **2026-11-18 multi-document fidelity checkpoint:** Draft PR #17 now includes an immutable, server-owned per-document print-plan validator, read-only same-kiosk READY variant lookup, opt-in read-only `/api/documents/batch/quote`, isolated browser helper, and a proposed private `cloud_print_tasks` schema plus fail-closed transitions for CUPS ambiguity. Separate simplex/duplex and B&W/color intent are preserved. **No multi-file order/PIN creation or physical Pi dispatch is wired.** Existing production upload/checkout/payment/spooler unchanged; no Supabase migration applied. Next gate: atomically persist order+all tasks, implement a versioned Pi task protocol and test multi-job crash/print recovery before enabling. Latest CI run must pass.

> **2026-11-18 background-worker milestone:** PR #17 adds opt-in, DB-backed canonical PDF preparation independent of the user's HTTP connection. Upload+settings+tariff are persisted in a private queue; a Render polling worker claims jobs atomically, processes via the existing normalizer, recovers expired leases after restarts, retries at most three times and reports a capability-scoped status. `XEROXGO_CLOUD_ASYNC_PREP_ENABLED` defaults OFF and requires the upload flag; new code is not wired into the production cart. All bucket/schema changes remain migration files only. Latest CI verification required. Cross-Render restart/live Storage tests, trusted rate limiting, multi-file printer-safe handling, UI state integration and Pi end-to-end remain blockers.

> **2026-11-18 Storage safeguards checkpoint:** Draft PR #17 now includes repaired SQLite preparation lease scans, durable per-kiosk hourly upload reservation quotas (20 requests/100 MiB), and an independently disabled-by-default private Supabase Storage cleanup path. Cleanup only handles expired database-known unreferenced objects, never anything attached to orders or with an active preparation lease; authenticated deletion is retried on failure. No live migrations or production changes. Latest GitHub CI must be checked before any merge. Trusted edge abuse controls, durable independent preparation worker, multi-document print fidelity, orphan reconciliation and end-to-end Supabase/Pi testing remain open.

> **2026-11-18 latest V2 recovery milestone:** Added opt-in canonical-preparation lease expiration/reclaim and crash-safe stale publisher identity, bounded the old worker below its lease, and documented the confirmed multi-file merged-CUPS settings mismatch risk. No live migration or customer cart switch. PR #17 remains a draft and requires current CI verification and security review.

> **V2 checkout implementation checkpoint — 2026-11-18:** PR #17 is still a DRAFT on `feat/cloud-first-documents-v2`. Added feature-gated, single-prepared-document reference-only checkout, atomic PIN+order+variant+idempotency persistence, and private Storage-backed signed PDF serving for those orders. The browser helper is isolated and the real cart is NOT switched. Canonical multi-file merge, durable preparation retry, cleanup, Storage setup, quotas and full Pi validation are still BLOCKERS. No production migrations have been applied; no live services touched. CI must pass on the latest head. Current `main` remains at `0b401816` as last verified.

> **Checkpoint addendum — 2026-11-18**: Verified remote `main` at `0b40181669129b506a8804c2bb595d88cdc2c18e`; PR #16 is merged. Inventory: 34 non-main remote branches, all 0 commits ahead of `main`; no open PRs before XG-CLOUD-DOCS-V2 work. Local Antigravity worktree not inspected. New branch `feat/cloud-first-documents-v2` implements ONLY the disabled-by-default private upload foundation, not customer checkout cutover. Storage migration **not applied**. Preserve existing portal/Render/Pi paths. See `docs/architecture/cloud-first-documents-v2.md` and `docs/TASKS.md`.

# XeroxGo — current project checkpoint

**Verified:** 2026-11-17 using the connected GitHub repository. This is a GitHub snapshot, not proof of the Render backend's deployed image or the local Antigravity worktree.

## Repository and integration
- Repository: https://github.com/Shaabas/XeroxGo
- Stable remote branch: `main`, verified at `fa148fee54c91bdfac3496d4c55b466cb6be81f6` (PR #10 workspace merge). Verify again before acting.
- Merged: [PR #9](https://github.com/Shaabas/XeroxGo/pull/9) central pricing, [PR #11](https://github.com/Shaabas/XeroxGo/pull/11) Vercel workflow correction, [PR #12](https://github.com/Shaabas/XeroxGo/pull/12) instant price estimates, [PR #13](https://github.com/Shaabas/XeroxGo/pull/13) pricing refresh and parity tests, [PR #14](https://github.com/Shaabas/XeroxGo/pull/14) admin price calculator.
- The latest main verification and Vercel deployment workflow [44108533141](https://github.com/Shaabas/XeroxGo/actions/runs/44108533141) completed successfully. A GitHub Actions deploy success is **not** independent proof of a Render backend rollout or live smoke tests.
- [PR #10](https://github.com/Shaabas/XeroxGo/pull/10) dual-agent workspace has merged to `main` at `fa148fee`. Documentation consolidation is tracked separately in [PR #15](https://github.com/Shaabas/XeroxGo/pull/15); do not treat this pending cleanup as merged until GitHub confirms.
- Branch inventory on 2026-11-17: all other 31 non-main remote branches are ancestors of `main` (0 ahead); they need no new merges. Local Antigravity changes were not accessible or inspected.

## Active architecture
- Frontend: `frontend/index.html`, `frontend/js/portal.js` and admin UI, deployed to Vercel.
- Go cloud: `cloud-backend/cmd/server/`; canonical PDF normalization, pricing and order/PIN creation.
- Printer host: Raspberry Pi with CUPS/IP printing; device emulator and recovery work live under the same repo.
- Pricing: admin sets four integer-paise rates through `/api/admin/pricing`; `/api/pricing` publishes the active revision. Instant JavaScript calculation is display-only, Go enforces billing, no artificial duplex padding billed.
- Previously approved initial tariff: BW 300/500 paise; color 800/1000 paise, initially disabled. **Current live rates and deployment must be rechecked before any release decision.**
- Supabase pricing migrations were previously applied; do not reapply or edit live tariffs without a fresh database verification and approval.
- Payment provider integration has been deferred by the user. Do not infer actual payment configuration from documentation.

## Current priority
**XG-CHECKOUT-FAST-1** (analysis complete, implementation not started): reduce PIN-generation wait by eliminating redundant synchronous `fetchDocumentQuote()` calls inside `submitOrder()`, while retaining final server PDF normalization, price acceptance, tariff revision protection and order/PIN durability. Add real upload progress and an indeterminate server-processing state, not fake percentages. Benchmark before/after and add regression tests. Architectural scope must be reviewed before changes.

## Open risks / verification needed
- Checkout currently re-uploads/re-normalizes each PDF serially for final quotes, then uploads/re-normalizes again in `/api/k/{id}/upload` before generating PIN. No production latency measurements yet.
- Mixed per-document simplex/duplex settings are aggregated into a single cloud/Pi print job; verify physical print fidelity before changing execution.
- Review Supabase legacy-table RLS/grants separately, with a dependency assessment.
- Verify Render active commit, live API health, mock-vs-real payment configuration and safe rollback before production-affecting backend changes.
- Document changes on this branch must not silently authorize payment, tariff, printer, or deployment changes.

## Agent handoff
Read `AGENTS.md`, `docs/TASKS.md`, `docs/ARCHITECTURE.md`, and `docs/DECISIONS.md`. Verify current GitHub SHA and local worktree before starting. ChatGPT leads architecture/planning/review; Antigravity performs narrowly scoped local implementation/testing.
