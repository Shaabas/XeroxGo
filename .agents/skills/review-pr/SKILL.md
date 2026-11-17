---
name: review-pr
description: Review a XeroxGo pull request for correctness, scope, safety, deployment implications, and actionable regressions before human merge approval.
---

# Review a XeroxGo PR

1. Fetch current base/head SHA and CI run; ensure reviewed code is the current head.
2. Compare changed files against task contract and `AGENTS.md`; identify unrelated additions and lost behavior.
3. Prioritize payment/order integrity, tariff consistency, data access/RLS, print submission idempotence, crash recovery, immutable contract models and UI regressions.
4. Cite exact file/line plus a reproducible scenario for any defect. Separate blocking from follow-up issues.
5. Check migration ordering and release dependencies; do not assume SQL migration files are already applied remotely.
6. Verify tests and CI at the exact head; note staging and hardware gaps.
7. Output: findings (severity, evidence, recommended fix), test gaps, merge readiness, deployment prerequisites.
8. Do not merge or mark production-ready without explicit human approval.
