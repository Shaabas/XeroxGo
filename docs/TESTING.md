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

## Acceptance gaps carried forward from retired audit reports

The detailed [core acceptance matrix](testing/core-acceptance-matrix.md) remains the historical specification; its code paths, expected behavior and verification status may have changed.

- **Frontend:** browser-level test of actual PDF page count, page range, N-Up/duplex, copies, cart order, quote mismatch, admin rate updates, multi-file upload and 4-digit PIN display. Check against current cloud-generated output rather than old client-side merging assumptions.
- **Cloud ordering:** concurrent session claims and paper stock reservations in PostgreSQL; cancellation/reconnect must not resurrect printing. Verify existing `orders_sessions_safety_test.go` and integration coverage before declaring a gap.
- **Kiosk hardware:** raw `/dev/tty1` keypad/termios via a virtual PTY; framebuffer ioctl and color layout where a suitable device exists; CUPS-online/SNMP-offline degraded telemetry and physical jam recovery.
- **Operations:** validate deployment/service shell scripts with shellcheck and safe dry runs if supported.
- **Maintenance:** expired payment attempts, session and PIN reservation cleanup must be safe with in-flight work and covered by tests.
- **PDF billing:** preserve duplex odd-side billing, 1/2/4/6-up imposition, padding exclusion, copies and per-document order. Actual normalized PDF page counts may differ from simplified preflight estimates only by a detected error, never by silently changing the charged amount.
- **API response compatibility:** historical baseline found different meanings for `total_pages` in preview and single/multi-file upload, and `price_paise` vs `total_price_paise` across endpoints. Audit consumers before renaming fields; tests, not historical report, are the current source of truth.

Do not claim these gaps are still untested solely because a historical audit listed them. Each future task should search current source/tests first, then record fresh evidence.
