# PR #9 — release readiness assessment

**Assessment date:** 2026-11-17. **Decision: NOT YET READY TO DEPLOY.** This is a read-only release assessment, not deployment approval.

## Verified source state
- Pricing PR: [#9](https://github.com/Shaabas/XeroxGo/pull/9), head `764e91a45a100674c195444cfa33de040b0ee385`, open and not merged.
- CI: [GitHub Actions 44060583198](https://github.com/Shaabas/XeroxGo/actions/runs/44060583198), Verification Gates 1–6 **success**.
- Workspace docs: [#10](https://github.com/Shaabas/XeroxGo/pull/10), draft; based on PR #9 branch.
- Confirmed code routes: `GET /api/pricing` and `GET/PUT /api/admin/pricing`. Frontend preview: `POST /api/documents/preview`. PR #9 description corrected.
- Supabase project recorded two canonical pricing migrations: `20261117082548` and `20261117085500`. Canonical tariff tables have RLS on and public client privileges revoked.
- Supabase tariff state: **active_revision = 0**, **version count = 0**. The new backend will reject pricing-dependent requests until activation; admin GET provides *form defaults*, not active prices.

## Deployment configuration and unknowns
- `render.yaml` checked into branch specifies Docker service `xeroxgo-cloud` with `XEROXGO_DEV_MODE=1` and `DEMO_MODE=true`. **Actual Render dashboard values and deployed commit were not verified.** These defaults should not be considered a production-safe configuration without an explicit decision.
- `frontend/vercel.json` routes API and payment requests to `https://xeroxgo-cloud.onrender.com`. Verify current Vercel deployment alias and gateway authentication config.
- Render was previously reported to auto-deploy from `main`; verify its live deploy settings before merging.
- Public health/pricing URLs were not verifiable through the available browser tool; no health result is claimed.
- 16 pre-existing public tables remain flagged by Supabase advisor for missing RLS. Requires separate grants/policy review; not fixed by the pricing migration.

## Release sequence requiring human approval
1. Verify live Render/Vercel environment settings, current deploy SHA, auto-deploy behavior, required secrets and payment mode **without revealing secret values**.
2. Approve initial canonical rates and whether color is enabled. The code's template defaults are B&W 300/500 paise and color 800/1000 paise but **none are active**.
3. Establish a tested rollback path: Render previous deploy/commit; confirm legacy pricing interactions and that rollback remains compatible with additive schema and any newly accepted orders. Document interruption risks.
4. Decide safe tariff activation window relative to backend release. Prefer staging compatibility verification first. A first revision may be activated using the authenticated admin PUT on the new backend, or a separately reviewed database method with the same invariants. **Do not improvise production SQL or silently enable rates.**
5. Smoke test `/healthz`, `/api/pricing`, preview/quote, upload, stale quote rejection, admin auth, payment order/PIN and Pi dispatch in a suitable staging/test environment. No real payment or real printer job without authorization.
6. Only after evidence review and explicit approval merge PR #9 to `main`; watch auto-deploy and perform rollback if health/checkout fails.
7. Retarget draft PR #10 to `main`, refresh state docs to deployed SHA and re-run checks before its merge.

## Known separately tracked follow-ups
- Mixed duplex/simplex multi-document print options collapse to one job-wide duplex mode. Evaluate print fidelity and recovery (XG-004).
- Full older-table RLS/grants assessment (XG-006).
- Confirm local Antigravity discovery of `.agents/skills` in checked-out repo (XG-001 acceptance).

## Rollback policy
Rollback must be tied to **verified** previous deployed image/commit and a concrete operational test. Git revert alone does not prove transaction safety, tariff compatibility, or printer delivery recovery.

**Current approval state:** no production deploy or tariff activation authorized.
