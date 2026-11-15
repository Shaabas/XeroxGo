# Mid-Print Hardware Fault Detection & CUPS Job Hold Validation (D4b-1)

## Overview

This document records the design, implementation, and verification results for Task D4b-1: detecting hardware faults during active CUPS print jobs, synchronizing mid-print fault injection with confirmed job execution, asserting CUPS job holds (`pending-held`), independently verifying post-fault job state, and decoupling physical sheet delivery from simulated processing.

All additions are confined to `tools/printer-emulator/` and test files (`kiosk-daemon/internal/printer/midprint_fault_integration_test.go`). Zero changes were made to production code (`cups.go`, `client.go`, SQLite schemas, recovery flows, payment logic, or backend contracts).

---

## Architecture & Synchronization Mechanisms

### 1. Active Job Synchronization (Zero Sleep Assumptions)

Physical hardware faults (paper jam, paper tray empty, cover open) occur while the printer is actively servicing a job. In unit tests or fast-executing emulators, print jobs often process in a few milliseconds, making it impossible to reliably inject a fault during active printing using arbitrary wall-clock sleeps (`sleep 1`).

To guarantee deterministic, non-flaky mid-print fault injection without modifying any production code:
- **`EMU_JOB_ACTIVE_FILE`**:
  `processor.sh` immediately writes job metadata (`JOB_NAME`, `PID`, `STATE=processing`, `TIMESTAMP`) upon job start and removes it upon job termination. Tests wait for this file and confirm that the PID is active before triggering any fault injection.
- **Deterministic Job Execution Gate (`EMU_JOB_GATE_FILE` & `EMU_JOB_RELEASE_FILE`)**:
  When `EMU_JOB_GATE_FILE` exists, `processor.sh` enters a controlled polling loop (up to `EMU_GATE_TIMEOUT_TENTHS`, default 6.0 seconds fail-safe timeout). This ensures the job remains in the `processing` state long enough for the test harness to inject faults and verify telemetry.
  Once the fault is injected or verified, creating `EMU_JOB_RELEASE_FILE` allows `processor.sh` to resume. If the harness exits unexpectedly, the 6.0-second timeout releases the gate automatically, ensuring jobs never hang indefinitely or block subsequent tests.

### 2. Live IPP Reason Updates During Active Jobs

- `ippeveprinter` is a single-threaded print server that handles one print job at a time. Submitting an out-of-band `ipptool Print-Job` probe (`sync_probe.test`) while a real CUPS job is actively printing returns HTTP 500 / `server-error-busy (Currently printing another job.)`.
- To support mid-print fault injection dynamically:
  - When `processor.sh` is active, `emulator.sh fault set <scenario>` writes `EMU_FAULT_STATE_FILE` directly and skips the conflicting `sync_probe.test`.
  - `processor.sh` reads `EMU_FAULT_STATE_FILE` in its polling loop and emits IPP filter protocol messages on stderr:
    ```bash
    STATE: -media-jam-error -media-empty-error -door-open-error ...
    STATE: +media-jam-error
    ```
  - This immediately updates `ippeveprinter`'s live printer state reasons in real time while servicing the active job.

### 3. SNMP Hardware Telemetry Synchronization

- `snmp_responder` continues serving RFC 3805 / RFC 1213 printer MIB telemetry on localhost UDP.
- `emulator.sh fault set` updates `EMU_FAULT_STATE_FILE` and triggers `snmp_responder` state synchronization (`tools/printer-emulator/snmp_responder -sync-state`).
- XeroxGo's `MonitorCUPSJobWithTelemetryCustom` actively polls SNMP via `PollHealthConfig`, immediately detecting hardware status flags:
  - `paper-jam`: `hrPrinterDetectedErrorState = 04 00 (jammed)` -> `telem.Flags.Jammed = true`
  - `paper-empty`: `hrPrinterDetectedErrorState = 00 01 (noPaper)` -> `telem.Flags.NoPaper = true`
  - `cover-open`: `hrPrinterDetectedErrorState = 08 00 (doorOpen)` -> `telem.Flags.DoorOpen = true`

---

## Hardware Fault Interruption & CUPS Job Hold Protocol

### 1. Job Interruption Errors

When a hardware fault is detected via telemetry while CUPS reports the job in `processing` or `pending`, `MonitorCUPSJobWithTelemetryCustom` attempts to hold the job in CUPS via `HoldAndVerifyCUPSJob` (`lp -i <cupsJobID> -H hold`) and immediately halts monitoring with a designated hardware interruption error:
- `paper-jam`: `paper feed jam detected in printer: please clear the jammed paper before continuing`
- `paper-empty`: `printer paper tray is empty: please load paper to continue printing`
- `cover-open`: `printer cover is open: please close the printer cover to continue`

### 2. Independent Post-Fault Verification

Following the hardware fault return, the integration test independently queries CUPS via `QueryCUPSJobInfo`:
- Evaluates `job-state` and `job-state-reasons`.
- Distinguishes the hold outcome:
  - `ConfirmedHeld`: CUPS state transitioned to `pending-held` (state enum 4) and reasons contain `job-hold-until-specified`.
  - `StillProcessing`: CUPS state remained in `processing` (hold command was issued but job not yet held).
  - `AlreadyCompleted`: Job completed before hold could take effect (CUPS returned exit code 1: `Job #... is finished and cannot be altered`).
- Validates that the original CUPS job ID (`<queue>-<jobNum>`) persisted in the local SQLite state store matches the CUPS queue records.
- Confirms exactly one print job was dispatched (no duplicate submissions created).

### 3. Decoupling Physical Sheet Delivery

Physical paper ejection must be decoupled from simulated software processing:
- The `onPage` callback of `MonitorCUPSJobWithTelemetryCustom` tracks physical sheet delivery increments.
- In simulated hardware faults, `onPage` calls remain strictly `0`.
- The simulated SNMP odometer (`prtMarkerLifeCount` at OID `1.3.6.1.4.1.2699.1.2.1.2.1.13.1.1`) is queried before and after fault injection, verifying the count remains unchanged (e.g. 1000 sheets). No synthetic page advance is treated as proof of physical sheet completion.

---

## Observed Behaviors & Emulation Limitations

1. **`ippeveprinter` Concurrency Boundary**:
   - `ippeveprinter` only executes one job filter process at a time. Any external probe requesting a print operation during active printing fails with `server-error-busy`.
   - Dynamic IPP attribute synchronization during active jobs is successfully achieved by routing filter state protocol strings (`STATE: +<reason>-error`) through the active job's stderr directly into the parent `ippeveprinter` process.
2. **CUPS Hold Semantics on Active vs Completed Jobs**:
   - For active jobs, CUPS supports holding the job (`lp -i <id> -H hold`), placing it into `pending-held`.
   - Once a job finishes processing in CUPS, `lp -H hold` returns exit status 1 with: `lp: Job #... is finished and cannot be altered.`
   - `HoldAndVerifyCUPSJob` correctly inspects CUPS error output and independently verifies CUPS job state to differentiate between `ConfirmedHeld`, `StillProcessing`, and `AlreadyCompleted`.
3. **Queue Cleanup and Teardown Safety**:
   - All integration tests use dynamic, isolated queue names (`XeroxGoMidQ_<PID>_<RAND>`) and unique mDNS emulator names.
   - 3-way URI verification is enforced prior to modifying or removing any CUPS queue.
   - Active held jobs are cancelled (`cancel <jobID>`) and job gate files released prior to queue removal.
   - If queue deletion fails, ownership metadata is strictly preserved and teardown fails the test rather than masking the error.

---

## Test Suites & Validation Results

### 1. Go Integration Test Suite (`kiosk-daemon/internal/printer`)
Command: `go test -v -race -run TestMidPrintHardwareFault ./kiosk-daemon/internal/printer -timeout 120s`
- `TestMidPrintHardwareFault_PaperJam`: **PASS** (Job confirmed active, paper-jam injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact).
- `TestMidPrintHardwareFault_PaperEmpty`: **PASS** (Job confirmed active, paper-empty injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact).
- `TestMidPrintHardwareFault_CoverOpen`: **PASS** (Job confirmed active, cover-open injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact).
- `TestMidPrintHardwareFault_DistinguishCompletedFromHeld`: **PASS** (Job allowed to finish normally, hold attempted on finished job, correctly identified as `AlreadyCompleted = true` and `ConfirmedHeld = false`).

### 2. Shell Mid-Print Fault Suite (`tools/printer-emulator/midprint_fault_test.sh`)
- Test 1: Start IPP & SNMP emulators on ephemeral ports: **PASS**
- Test 2: Setup driverless CUPS queue with 3-way URI check: **PASS**
- Test 3: Mid-Print Paper-Jam detection, fault synchronization, CUPS hold, decoupling: **PASS**
- Test 4: Mid-Print Cover-Open detection and CUPS hold: **PASS**
- Test 5: Mid-Print Paper-Empty detection and CUPS hold: **PASS**
- Test 6: Distinguish completed job from held job: **PASS**
- Test 7: Traceability and single dispatch verification in CUPS: **PASS**
- Test 8: Verified CUPS queue cleanup and teardown safety: **PASS**

### 3. Regression Test Suites
- `tools/printer-emulator/smoke_test.sh`: **PASS** (19/19 tests passing)
- `tools/printer-emulator/snmp_smoke_test.sh`: **PASS** (16/16 tests passing)
- `tools/printer-emulator/fault_smoke_test.sh`: **PASS** (69/69 tests passing)
- `tools/printer-emulator/lifecycle_test.sh`: **PASS** (14/14 tests passing)
- Monorepo Verification Gate (`scripts/verify_gate.sh`): **PASS**
