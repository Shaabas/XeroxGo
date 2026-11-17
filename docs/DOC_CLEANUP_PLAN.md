# Documentation consolidation plan (non-destructive)

Created with XG-001. The first workspace phase establishes a concise canonical entry point, **not** deletion of historical reports. This protects unreproduced tests, architectural contracts and investigation evidence while PR #9 is pending.

| Existing file | Initial disposition | Check before retiring |
| --- | --- | --- |
| `architecture/overview.md` | Consolidate | Current runtime topology, deployment, recovery |
| `architecture/module-boundaries.md` | Consolidate | Module responsibilities, forbidden dependencies |
| `architecture/module-contracts.md` | Preserve detail until reviewed | Public APIs, ownership invariants |
| `architecture/minimum-core.md` | Consolidate | Canonical vs legacy paths, valid decisions |
| `architecture/orders-sessions-audit.md` | Historical | Open findings and order state invariants |
| `cleanup/branch-integration-plan.md` | Historical | Earlier merge concerns; current branch state overrides |
| `cleanup/conversion-analysis.md` | Historical | Outstanding conversion/test issues |
| `cleanup/database-release-plan.md` | Preserve until release audit | Rollback and migration requirements |
| `cleanup/module-inventory.md` | Historical | Open refactoring tasks |
| `cleanup/orders-sessions-endpoints.md` | Historical | Legacy API consumers and risks |
| `reference/file-inventory.md` | Likely retire | Any unique ownership/coverage info |
| `testing/core-acceptance-matrix.md` | Preserve until test mapping | Unique safety and acceptance checks |
| `testing/document-pricing-baseline.md` | Preserve until test mapping | Pricing and PDF boundary cases |
| `testing/orders-sessions-concurrency.md` | Preserve until test mapping | Durable transaction/recovery invariants |
| `workflows/minimum-print-path.md` | Consolidate | Exact payment → CUPS → recovery workflow |

## Cleanup rules
- Do not delete based on age/size alone.
- Read full source before replacing or removing; retain unique unresolved requirements as tasks or tests.
- Replace machine-local `file:///home/...` links with relative repository paths.
- Work from a verified, consolidated branch after PR #9 release approval; create a separate docs-only PR for deletions.
- Keep stable documentation short; prefer source code/tests as truth for detailed implementation.
- Never refer to old branch audit as live status.

**Status:** inventory and strategy complete, irreversible cleanup deferred.
