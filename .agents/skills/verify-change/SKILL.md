---
name: verify-change
description: Validate a XeroxGo code change using targeted tests, repository gates, and evidence-based reporting without modifying production.
---

# Verify a XeroxGo change

1. Confirm requested branch, exact commit SHA, relevant changed files, local worktree state and task contract.
2. Identify affected subsystems: frontend, cloud, persistence, pricing, payments, Pi spooler or printer recovery.
3. Run smallest relevant tests first; use the repository's existing commands and avoid inventing flags. For full pre-merge verification run `./scripts/verify_gate.sh`.
4. On failures distinguish code defects, flaky timing, missing test dependencies and environment-specific behavior. Do not weaken assertions to make CI green.
5. If edits are needed, separate them into a scoped implementation task and commit, then reverify the new SHA.
6. Record tests with commands, environment, pass/fail, CI URLs, and untested hardware/deployment requirements.
7. Produce a verdict: PASS / FAIL / INCOMPLETE, with precise blocking issues. Green CI alone does not approve production deployment.

No production database writes or deployment actions during verification.
