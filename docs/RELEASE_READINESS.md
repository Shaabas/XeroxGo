# PR #9 — release readiness assessment

**Assessment date:** 2026-11-17. **Decision: TARIFF ACTIVATED; TESTING ROLLOUT NOT YET AUTHORIZED TO MERGE.** This is a read-only release assessment, not deployment approval.

## Verified source state
- Pricing PR: [#9](https://github.com/Shaabas/XeroxGo/pull/9), head `764e91a45a100674c195444cfa33de040b0ee385`, open and not merged.
- CI: [GitHub Actions 44060583198](https://github.com/Shaabas/XeroxGo/actions/runs/44060583198), Verification Gates 1–6 **success**.
- Workspace docs: [#10](https://github.com/Shaabas/XeroxGo/pull/10), draft; based on PR #9 branch.
- Confirmed code routes: `GET /api/pricing` and `GET/PUT /api/admin/pricing`. Frontend preview: `POST /api/documents/preview`. PR #9 description corrected.
- Supabase project recorded two canonical pricing migrations: `20261117082548` and `20261117085500`. Canonical tariff tables have RLS on and public client privileges revoked.
- Supabase tariff state **updated and verified after approval**: `active_revision = 1`, version count 1; BW simplex 300, BW duplex sheet 500, color simplex 800, color duplex sheet 1000 paise; color disabled. This does not prove the currently deployed backend is running the new pricing code.

## Confirmed testing configuration (user dashboard inspection)
- Render service is linked to `main`, auto-deploys **After CI Checks Pass**, Dockerfile `./Dockerfile`.
- `XEROXGO_DEV_MODE=1` currently; development mode forces `demoMode=true`.
- `DATABASE_URL`, admin password, download/webhook secrets are present (masked); 3 Vercel secrets reported configured. Do not infer values or sufficiency.
- **Important:** Demo mode does NOT force the mock payment provider. The server independently chooses Razorpay if `PAYMENT_PROVIDER=razorpay`, or if `PAYMENT_PROVIDER` is unset and both Razorpay credentials exist. For testing require **`PAYMENT_PROVIDER=mock`** and verify startup log "Initialized Mock payment provider". Do not expose real Razorpay key values.
- User explicitly chose development/testing deployment with NO real payments, not production launch. User further confirmed Razorpay is not configured and requested **payment setup be deferred**. Do not spend the active workstream on payment integration.

## Testing deployment order (pending approvals)
1. **Payments deferred by user.** Razorpay is not configured; do not work on payment-provider setup during this task. Before any test checkout that could initiate a payment, verify it cannot trigger real charges; keep that verification as a narrow pre-test safety check, not an integration project.
2. Verify the database referenced by Render is the intended testing Supabase database (without revealing the connection string). Otherwise do not activate tariffs in the wrong environment.
3. **Completed after separate human approval:** revision 1 activated in connected Supabase and independently verified with 300/500/800/1000 paise and color disabled. No migration or real payment provider change.
4. Verify code deployment prerequisites and choose a rollback mechanism. Because `main` auto-deploys after CI, use a maintenance window or temporarily disable auto-deploy through approved dashboard action if necessary.
5. Activate the first tariff revision using the authenticated admin API in a verified compatible backend or a separately reviewed transactional method; do not use unreviewed SQL.
6. After explicit release authorization, merge PR #9; observe Render deploy at exact SHA and smoke test /healthz, /api/pricing, quote, stale quote, demo checkout, and PIN/job flow. Avoid actual physical prints unless specifically approved.
7. If regressions occur, stop new test orders and execute a verified rollback; watch for schema and order-format compatibility.
8. When stable, integrate PR #10 and refresh all state/commit references.

## Deployment configuration and unknowns
- `render.yaml` checked into branch specifies Docker service `xeroxgo-cloud` with `XEROXGO_DEV_MODE=1` and `DEMO_MODE=true`. **Actual Render dashboard values and deployed commit were not verified.** These defaults should not be considered a production-safe configuration without an explicit decision.
- `frontend/vercel.json` routes API and payment requests to `https://xeroxgo-cloud.onrender.com`. Verify current Vercel deployment alias and gateway authentication config.
- Render was previously reported to auto-deploy from `main`; verify its live deploy settings before merging.
- Public health/pricing URLs were not verifiable through the available browser tool; no health result is claimed.
- 16 pre-existing public tables remain flagged by Supabase advisor for missing RLS. Requires separate grants/policy review; not fixed by the pricing migration.

## Release sequence requiring human approval
1. Verify live Render/Vercel environment settings, current deploy SHA, auto-deploy behavior, required secrets and payment mode **without revealing secret values**.
2. Initial canonical rates approved and revision 1 activated in the connected database; confirm the rollout's backend targets this database and perform application smoke tests after deployment.
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

## Final pre-merge verification — 2026-11-17
- PR #9 verified open, mergeable, non-draft, head `764e91a45a100674c195444cfa33de040b0ee385`; 85 commits ahead of `main`, 0 behind. GitHub Actions gates 1–6 succeeded at that head; frontend Vercel deployment check was skipped.
- PR #10 is still draft, open, and based on the pricing branch; do not merge it prematurely.
- User confirmed Render uses `main`, after-CI auto-deploy, Dockerfile `./Dockerfile`, development mode `1`, correct connected DB, and no Razorpay configuration. Payment integration remains deferred; no payment setup was changed.
- Connected Supabase revision 1 has been activated and independently read back at BW 300/500, color 800/1000 paise and color disabled.
- Render rollback documentation: service **Deploys** page → identify previous successful deploy → **Rollback** → confirm. Dashboard rollback disables auto-deploy; rollback restores the previous build but does **not** roll back external database changes. See https://render.com/docs/rollbacks .
- **Outstanding:** user must verify in Render Deploys page that a previous successful deployment is still available for rollback and capture its commit SHA; live deployed SHA and backend health remain unconfirmed. Web request for `https://xeroxgo-cloud.onrender.com/healthz` could not be evaluated by available web tool. Do not infer availability.
- Next approval gate: after previous successful deploy SHA is identified, authorize PR #9 merge and monitor Render until live. The pricing schema changes are additive; beware runtime compatibility when rolling back.
