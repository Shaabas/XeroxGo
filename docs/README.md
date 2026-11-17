# XeroxGo — project documentation

**Start here:** [agent contract](../AGENTS.md) · [current checkpoint](PROJECT_STATE.md) · [task queue](TASKS.md) · [architecture](ARCHITECTURE.md) · [decisions](DECISIONS.md) · [verification](TESTING.md) · [deployment](DEPLOYMENT.md) · [release readiness](RELEASE_READINESS.md).

These seven focused documents are the **working navigation layer** for ChatGPT, Antigravity, and human maintainers. Exact repository/CI state must always be verified; the checkpoint is dated.

## Canonical working docs
- `PROJECT_STATE.md`: current branch, PR, blockers and next action.
- `TASKS.md`: tasks with owners, scope and dependencies.
- `ARCHITECTURE.md`: short runtime map and non-negotiable boundaries.
- `DECISIONS.md`: approved versus pending decisions.
- `TESTING.md`: evidence required for change acceptance.
- `DEPLOYMENT.md`: production and migration guardrails.

## Historical evidence — retained pending deliberate cleanup
The existing detailed reports are **not current status by default**. They may contain obsolete paths, line references, or outdated conclusions. Before deleting, classify whether each contains important unreproduced behavior/contracts.

- `architecture/overview.md`, `architecture/module-boundaries.md`, `architecture/module-contracts.md`, `architecture/minimum-core.md`: detailed architecture sources.
- `architecture/orders-sessions-audit.md`: historical orders/sessions audit.
- `cleanup/branch-integration-plan.md`, `cleanup/conversion-analysis.md`, `cleanup/database-release-plan.md`, `cleanup/module-inventory.md`, `cleanup/orders-sessions-endpoints.md`: historical assessments and plans.
- `reference/file-inventory.md`: potentially outdated tracked-file inventory.
- `testing/core-acceptance-matrix.md`, `testing/document-pricing-baseline.md`, `testing/orders-sessions-concurrency.md`: historical test matrices, preserve unique cases in actual tests before retirement.
- `workflows/minimum-print-path.md`: detailed workflow trace.

**Cleanup process:** review document for unique invariants and open tasks → migrate essentials into working docs or tracked tests → check references → remove obsolete report in a separate reviewed docs-only PR. Git history retains past reports.

## Starting a new ChatGPT conversation
> Continue XeroxGo from https://github.com/Shaabas/XeroxGo. Read AGENTS.md and docs/{PROJECT_STATE,TASKS,ARCHITECTURE,DECISIONS}.md. Verify current GitHub branch, commits, CI and PRs. Resume the highest-priority unblocked task under the dual-agent workflow; do not change production or architecture without approval.
