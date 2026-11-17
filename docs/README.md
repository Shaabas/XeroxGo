# XeroxGo — documentation

**Start here:** [agent operating rules](../AGENTS.md) · [current state](PROJECT_STATE.md) · [task queue](TASKS.md).

## Active reference documents
| File | Purpose |
| --- | --- |
| [PROJECT_STATE.md](PROJECT_STATE.md) | Verified GitHub checkpoint; review date before trusting |
| [TASKS.md](TASKS.md) | Active tasks, owners, unresolved investigations |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System boundaries and non-negotiable invariants |
| [DECISIONS.md](DECISIONS.md) | Approved and pending architecture decisions |
| [TESTING.md](TESTING.md) | Verification gates, current coverage and test gaps |
| [DEPLOYMENT.md](DEPLOYMENT.md) | Deployment, database and rollback precautions |

## Detailed references retained intentionally
These documents contain unique behavioral contracts, accepted test cases, or database release procedures that should not be reduced to a short status summary:

- [architecture/module-contracts.md](architecture/module-contracts.md) — module APIs, state ownership and safety invariants. **Historical implementation references; verify against source code.**
- [testing/core-acceptance-matrix.md](testing/core-acceptance-matrix.md) — concrete missing hardware/frontend/operational acceptance tests. **Status and paths are historical.**
- [testing/document-pricing-baseline.md](testing/document-pricing-baseline.md) — PDF layout/billing test cases and the preview/upload API naming discrepancies.
- [testing/orders-sessions-concurrency.md](testing/orders-sessions-concurrency.md) — PostgreSQL concurrency evidence and unique index/locking invariants.
- [cleanup/database-release-plan.md](cleanup/database-release-plan.md) — staged PostgreSQL index migration, preflight and rollback procedures. **Do not run its SQL without confirming live DB state.**

## Cleanup result
Obsolete branch comparison reports, old code inventories, completed conversion-cleanup narratives, redundant system overviews, stale workflow traces, and pre-release snapshots were removed in the documentation cleanup PR. Their complete content remains available in Git history. Key unresolved work was moved into [TASKS.md](TASKS.md) and [TESTING.md](TESTING.md).

**Rules:** source and tests override old documents; verify version and environment; never interpret historical audit findings as proof of current bugs. Do not delete the retained detailed specifications unless their unique checks have been migrated into tested code.

## Starting a new ChatGPT conversation
> Continue XeroxGo from https://github.com/Shaabas/XeroxGo. Read AGENTS.md and docs/{PROJECT_STATE,TASKS,ARCHITECTURE,DECISIONS}.md. Verify current GitHub commit, PRs, CI and local worktree. Resume the highest-priority unblocked task. Explain architectural changes before implementing them.
