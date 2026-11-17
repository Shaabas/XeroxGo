# XeroxGo — current project checkpoint

**Verified:** 2026-11-17 using the connected GitHub repository. This is a GitHub snapshot, not proof of the Render backend's deployed image or the local Antigravity worktree.

## Repository and integration
- Repository: https://github.com/Shaabas/XeroxGo
- Stable remote branch: `main`, checked at `c3baa56e4955f7715371fae991811287b040a610` (PR #14 merge).
- Merged: [PR #9](https://github.com/Shaabas/XeroxGo/pull/9) central pricing, [PR #11](https://github.com/Shaabas/XeroxGo/pull/11) Vercel workflow correction, [PR #12](https://github.com/Shaabas/XeroxGo/pull/12) instant price estimates, [PR #13](https://github.com/Shaabas/XeroxGo/pull/13) pricing refresh and parity tests, [PR #14](https://github.com/Shaabas/XeroxGo/pull/14) admin price calculator.
- The latest main verification and Vercel deployment workflow [44108533141](https://github.com/Shaabas/XeroxGo/actions/runs/44108533141) completed successfully. A GitHub Actions deploy success is **not** independent proof of a Render backend rollout or live smoke tests.
- [PR #10](https://github.com/Shaabas/XeroxGo/pull/10) is the outstanding docs/dual-agent workspace integration, now targeting `main`. Check its final merge state and CI after any edits.
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
