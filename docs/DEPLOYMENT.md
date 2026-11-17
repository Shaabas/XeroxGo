# XeroxGo — deployment and database safety

**Operational guardrail:** At last review Render automatically deploys from `main`. Therefore a GitHub merge may trigger a production release. Verify current settings before action.

## Pre-merge checklist
- Verify current PR head/target, all checks and approvals. Do not trust old reports.
- Identify deployment trigger (Render/Vercel), production environment variables, health checks and rollback target.
- Confirm Supabase migrations are present and compatible; migrations are not automatically safe to rerun.
- Validate active tariff: the canonical pricing API returns 503 until an admin activates revision 1. At last direct check, `active_revision=0`.
- Check the public API route in code (`/api/pricing` at the verified snapshot); correct stale PR/documentation references.
- Establish backup/rollback plan and smoke tests (pricing, preview, upload, payment/PIN, kiosk dispatch).
- Obtain explicit human approval for merging into an auto-deploy branch and for any tariff activation.

## Supabase snapshot at 2026-11-17
- Canonical tariff migrations and security migration recorded remotely.
- Two new pricing tables have RLS and client-access revocations; service role access retained.
- 16 older public tables still need grants/RLS review — do not fix blindly without backend dependency analysis.

## Release records
For each deployment record: date, commit, environment, migration versions, tariff revision, verification, rollback reference, operator approval, and outcome.

Do not commit credentials or copy real customer records into development tests.
