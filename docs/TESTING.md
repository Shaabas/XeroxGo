# XeroxGo — verification policy

> **Verification note — 2026-11-17:** The PR #9 head and CI details cited below are historical. More recent merged pricing/admin work includes PRs #11–#14. Main commit `c3baa56` passed verification and Vercel deployment in [run 44108533141](https://github.com/Shaabas/XeroxGo/actions/runs/44108533141). Live Render backend deployment and checkout latency were not independently timed; see [PROJECT_STATE.md](PROJECT_STATE.md).


## Existing gate
The current repository uses `./scripts/verify_gate.sh`. At the verified 2026-11-17 PR #9 head `764e91a`, GitHub CI gates 1–6 passed in [run 44060583198](https://github.com/Shaabas/XeroxGo/actions/runs/44060583198). Run again for subsequent commits.

The gate covers frozen contracts, prohibited CGO SQLite driver, `go vet`, required target builds, Go race-enabled tests, and frontend conversion tests.

## Verification ladder
1. **Targeted:** run tests for modified code; report exact command and output.
2. **Subsystem:** run associated integration/recovery tests, with `-race` where applicable.
3. **Repository gate:** run `./scripts/verify_gate.sh` and confirm GitHub Actions on the exact PR head.
4. **Release:** compare required env/migrations with live deployment, test real service connectivity and rollback plan.
5. **Hardware:** use Pi/printer for hardware-specific acceptance; emulator tests are not sufficient.

## Change-specific checks
- **Pricing:** four rates, fractional paise values, odd duplex side, copies, N-up, quote/revision mismatch, immutable stored order price.
- **Order/payment:** duplicate webhook/payment, PIN collisions, atomic transitions, no paid order loss.
- **Printing/recovery:** crash before/after submission, CUPS uncertain status, repeated dispatch, idempotence, page counters and paper depletion.
- **Frontend:** mobile viewport, visual regressions, multi-file settings, quote refresh and errors.

No agent may report “all tests passed” unless the tested commit, environment, and evidence are recorded. A green gate does not certify production readiness or physical printer behavior.
