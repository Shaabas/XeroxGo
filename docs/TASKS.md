# XeroxGo — engineering task queue

Each task has one implementer, an explicit checkpoint, and test evidence. Status values: TODO / IN_PROGRESS / BLOCKED / REVIEW / DONE. Human approval is separate from CI.

| ID | Status | Owner | Scope / acceptance | Dependency |
| --- | --- | --- | --- | --- |
| XG-001 | REVIEW | ChatGPT | Prepare separate docs branch, shared agent contract, concise state/architecture/decisions/testing/deployment docs, historical doc classification; preserve source docs until audited | PR #9 branch remains unchanged |
| XG-002 | BLOCKED | ChatGPT | Verify PR #9 release readiness: current head/CI, true pricing API routes, Supabase active tariff and RLS, Render/Vercel auto-deploy, rollback and activation sequence; produce approval checklist, see RELEASE_READINESS.md; blocked on mock payment verification, test DB identity, approved tariff and rollout | Human/deployment access |
| XG-003 | BLOCKED | Human approval, then delegated implementer | Merge verified PR #9 into `main` only after activation/deployment readiness; confirm health after deploy | XG-002 + approval |
| XG-004 | TODO | ChatGPT (analysis) | Trace mixed multi-document printing from portal through cloud, Pi/CUPS, recovery; propose smallest safe change, no implementation before architecture approval | XG-003 ideally |
| XG-005 | TODO | Antigravity (after scoped task contract) | Implement approved XG-004 changes with tests on isolated branch/worktree | XG-004 + approval |
| XG-006 | TODO | ChatGPT | Audit missing RLS on 16 legacy public tables and grants, map trusted backend access, propose safe phased hardening | XG-002 context |
| XG-007 | TODO | Joint | Add targeted verification entry points and focused Antigravity skills once actual commands/platform support are confirmed | XG-001 |

## Task contract template
- **Goal:** one measurable result.
- **Owner / branch / allowed paths:** prevent collisions.
- **Inputs and constraints:** architecture and backward compatibility.
- **Acceptance criteria:** exact observable behavior and tests.
- **Not allowed:** architecture rewrites, prod actions, unrelated files.
- **Evidence:** commit, diff, test output, CI URL, blockers.
- **Handoff:** update `PROJECT_STATE.md` and this table.

Do not mark tasks DONE from agent narrative alone; verify changes and results.
