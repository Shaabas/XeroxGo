# XeroxGo — agent operating contract

Read this file first, then [project state](docs/PROJECT_STATE.md), [tasks](docs/TASKS.md), [architecture](docs/ARCHITECTURE.md), and [decisions](docs/DECISIONS.md). Verify current branch, commit, worktree, and PR status before modifying anything. These documents describe a checkpoint, not a substitute for inspecting the code.

## Roles
- **Human engineering lead:** owns priorities, product decisions, architecture approval, production authorization.
- **ChatGPT:** architecture, planning, GitHub/Supabase investigation, code review, scoped remote repository changes, and project-state maintenance where appropriate.
- **Antigravity:** focused local implementation, local validation, commits, hardware-specific testing.
- These are defaults, not prohibitions: do the work directly when access and safety permit. Do not hand off tasks unnecessarily.

## Working rules
1. Use a task ID from `docs/TASKS.md`. If absent, define one before significant work.
2. One task, one accountable implementer, explicit file scope and acceptance criteria.
3. Preserve unrelated local changes. Never reset, stash, clean, force-push, or overwrite without first identifying and preserving the work.
4. Keep change sets small. Run targeted tests first; full verification before merge.
5. Explain architecture-level changes (what, why, impact, alternatives) to the human **before** implementation.
6. Require explicit human approval for pricing activation, payments, authentication, destructive database changes, production deploys, or merging to an auto-deploy branch.
7. Do not merge or deploy merely because CI is green. Inspect runtime prerequisites and rollback plan.
8. Avoid overlapping changes to the same files across agents. Use separate branches/worktrees for parallel work.
9. Never log or commit secrets, recovery tokens, customer documents, or credentials.
10. Maintain one canonical cloud PDF normalization/imposition and pricing path. Preserve billing invariants, transaction guarantees, and print recovery behavior.
11. A reported test pass requires evidence (command, result, environment or CI URL). Do not claim hardware validation based on emulation.
12. At each meaningful handoff update `docs/PROJECT_STATE.md` and `docs/TASKS.md`, including exact commit/PR, pending local-only changes, blockers, and next step.

## Change classes
- **A Routine:** limited UI, comments, small internal refactors — within approved task scope.
- **B Architectural:** interfaces, persistence, printing lifecycle/recovery, normalization — obtain design review first.
- **C Production critical:** payment, auth, tariff activation, destructive migrations, deployment — explicit human approval and rollback plan.

## Task handoff format
`Task ID | branch | commit SHA | files changed | tests (command/result) | risks | blockers | next action`.
Record unknowns explicitly; do not silently turn assumptions into facts.

## Important entry points
- `docs/README.md`: current vs historical document map.
- `docs/TESTING.md`: verification and release requirements.
- `docs/DEPLOYMENT.md`: deployment guardrails.
- `scripts/verify_gate.sh`: existing repository verification script.
- `deploy/supabase/migrations/`: versioned database changes.
