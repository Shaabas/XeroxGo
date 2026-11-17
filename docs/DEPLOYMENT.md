# XeroxGo — deployment and database safety

**Verified GitHub snapshot:** 2026-11-17. Latest main at review: `c3baa56e4955f7715371fae991811287b040a610` (PR #14). [GitHub Actions 44108533141](https://github.com/Shaabas/XeroxGo/actions/runs/44108533141) reports verification and **Vercel frontend deployment successful**. This does not establish the Render backend's live commit, environment, or end-to-end order readiness.

## Pre-merge checklist
1. Verify current PR head, target, changed-file scope, required checks and approval.
2. Inspect actual Vercel/Render deployment triggers; main-branch merges can trigger releases.
3. Confirm relevant Supabase migrations/configuration are already compatible. Never blindly reapply live migrations.
4. Verify server tariff revision, admin/public pricing routes and accepted-quote contract when changing pricing.
5. Confirm payment mode without exposing secrets or inadvertently initiating real payments.
6. Preserve an identified rollback build and run post-deploy smoke checks for the affected system.
7. Distinguish `GitHub CI passed`, `frontend deploy succeeded`, `backend deploy verified`, and `end-to-end smoke tested`.

## Pricing and database state
- The canonical tariff tables `sheet_tariff_versions` and `sheet_tariff_state` were previously created and secured in connected Supabase.
- Revision 1 was previously activated with BW simplex/duplex 300/500 paise, color simplex/duplex 800/1000 paise, color disabled. **Do not assume these are still the live rates; admin can update them.**
- Existing order prices are recorded; quote/order revision mismatches must be rejected.
- Older table RLS/grants require a separate analysis. Avoid broad security migrations without dependency inspection.

## Changes and operational ownership
- ChatGPT: GitHub, architecture, reviews, approved remote edits.
- Antigravity: focused implementation and local tests.
- Human: approves architecture, payments, database mutation and deployment-sensitive changes.
- Render mode/deployed SHA and Raspberry Pi hardware readiness require independent verification.

## Rollback
Document previous successful frontend/backend builds, database migration compatibility, and current order state before a production-affecting change. A Git revert is not proof of a safe database or print-session rollback.

## Release record
`date | PR + merge SHA | environment | migration/tariff revision | CI | frontend deploy | backend deploy | smoke results | approval | rollback target`.
