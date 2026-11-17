# XeroxGo — current project checkpoint

**Snapshot:** 2026-11-17, verified via connected GitHub and Supabase tools. **Update this file after the next integration milestone.** Information may become stale; confirm live branch and CI before action.

## Current source of truth
- Repository: https://github.com/Shaabas/XeroxGo
- Stable branch: `main`.
- Feature branch: `feat/cloud-pricing-unification`, verified head `764e91a45a100674c195444cfa33de040b0ee385`.
- PR: [#9 — centralize cloud sheet pricing and tariff security](https://github.com/Shaabas/XeroxGo/pull/9), **open, ready for review, not merged** at snapshot.
- Feature branch was 85 commits ahead of `main`, 0 behind at last check.
- GitHub verification gates 1–6: **success** for head `764e91a` in [run 44060583198](https://github.com/Shaabas/XeroxGo/actions/runs/44060583198).
- Antigravity reported checkpoint `checkpoint/pre-consolidation` at `0b6716b`; verify its current existence and any uncommitted local work in the local repository.

## Supabase / pricing
- Connected Supabase project: XeroxGo (`ftqtagndujzquonpjmfx`).
- Migrations recorded remotely: `20261117082548 canonical_sheet_pricing`; `20261117085500 secure_canonical_sheet_pricing_rls`.
- Pricing tables `sheet_tariff_versions` and `sheet_tariff_state` exist; RLS enabled, `anon`/`authenticated` access revoked, service role retains access.
- **Verified live 2026-11-17:** `sheet_tariff_state.active_revision = 1`, exactly one tariff version, BW 300/500 paise (simplex side/duplex sheet), color 800/1000 paise and `color_enabled=0`. Revision 1 was activated in connected Supabase project after explicit human approval. This is a DB configuration change only; PR #9 still not deployed.
- 16 other public tables were flagged by Supabase advisor for missing RLS; separate security work is required.
- Do not assume the two pricing migrations still need applying; confirm remote migration history first.
- Canonical tariff revision 1 is now active and verified. Deployment still requires PR #9 merge approval and rollout/rollback verification.

## Immediate priorities
1. **XG-001**: Workspace foundation committed on PR #10, pending local Antigravity skill discovery and integration; old docs retained for later audit.
2. **XG-002**: Development/testing rollout chosen. Render uses `main` / After CI Checks Pass / `./Dockerfile`; `XEROXGO_DEV_MODE=1`. Razorpay is not configured per user; **payment setup deferred**. Prioritize test DB identity, approved tariff activation procedure, rollout order and rollback. See [release readiness](RELEASE_READINESS.md).
3. **XG-003**: Obtain explicit approval then merge PR #9 to `main` and verify deployment.
4. **XG-004**: Audit mixed per-document simplex/duplex execution and Pi/CUPS recovery before implementing changes.

## Open issues / cautions
- Render may auto-deploy merges to `main`; Git merge is a deployment-affecting action.
- PR #9 description mentioned `/api/tariffs/active`, while inspected frontend/backend code used `/api/pricing`; correct documentation after verifying current code.
- Merged multi-document PDFs carry one aggregate duplex job flag; per-document print setting fidelity is unverified. Architecture audit required.
- The active task is **PR #9 non-payment release readiness**. Razorpay/payment integration is explicitly deferred. PR #9 text corrected; PR #10 remains a draft with workspace files.

## Hand-off
Read `AGENTS.md` and `docs/TASKS.md`, verify GitHub branch/PR and local pending changes, then continue the highest priority unblocked task. Never infer that a checkpoint proves current runtime state.
