# Mid-Print Hardware Fault Detection & CUPS Job Hold Validation (D4b-1)

## Overview

This document records the design, implementation, and verification results for Task D4b-1: detecting hardware faults during active CUPS print jobs, synchronizing mid-print fault injection with confirmed job execution, asserting CUPS job holds (`pending-held`), independently verifying post-fault job state, and decoupling physical sheet delivery from simulated processing.

All additions are confined to `tools/printer-emulator/` and test files (`kiosk-daemon/internal/printer/midprint_fault_integration_test.go`, `tools/printer-emulator/midprint_fault_test.sh`). Zero changes were made to production code (`cups.go`, `client.go`, SQLite schemas, recovery flows, payment logic, or backend contracts).

---

## Architecture & Synchronization Mechanisms

### 1. Authoritative Job-Specific Marker & Active Synchronization (Requirements 2–10)

Physical hardware faults (paper jam, paper tray empty, cover open) occur while the printer is actively servicing a job. In unit tests or fast-executing emulators, print jobs often process in a few milliseconds, making it impossible to reliably inject a fault during active printing using arbitrary wall-clock sleeps (`sleep 1`).

To guarantee deterministic, non-flaky mid-print fault injection without modifying any production code:
- **Atomic Marker Writing (Requirements 2, 3, 4)**:
  `processor.sh` writes active-job markers atomically using a temporary file in the same directory (`${marker_dir}/.job_active_tmp_${$}_${RANDOM}`) and renames it (`mv -f`) into `EMU_JOB_ACTIVE_FILE`.
  Atomic writing applies to both state transitions:
  1. `processing` transition upon startup.
  2. `completed` transition upon job exit or signal trap (`EXIT INT TERM`).
  Marker writes remain strictly test-only: if `EMU_JOB_ACTIVE_FILE` is unset or empty, or if internal sync jobs (`XeroxGoFaultSync`) run, no marker is created, preventing any interference with production jobs.
  ```ini
  JOB_NAME=<job_name>
  IPP_JOB_ID=<ipp_job_id>
  USER_NAME=<user_name>
  PID=<processor_pid>
  STATE=processing
  TIMESTAMP=<nanoseconds>
  ```
- **Authoritative Process & Job Validation (`validateActiveJob`) (Requirements 5–8)**:
  Before any fault is injected, the test harness authoritatively validates:
  1. **Job Identity Match**: `JOB_NAME` matches the exact submitted test job ID (`cmd.JobID`). Mismatched job IDs are rejected.
  2. **Active State Verification**: `STATE` is strictly `processing`. Completed markers (`STATE=completed`) and incomplete markers are rejected.
  3. **PID Process Verification**: `PID` is verified alive (`kill -0`) AND confirmed to belong to the expected processor process (`/proc/<pid>/cmdline` or `/proc/<pid>/comm` contains `processor.sh`). Unrelated live processes and reused PIDs are rejected.
  4. **IPP Job Identity Validation**: Where reliably available (`IPP_JOB_ID != "" && != "unknown"`), the marker's IPP job ID is validated as a positive integer.
  5. **Strict CUPS Processing Requirement**: The corresponding CUPS job state queried via `QueryCUPSJobInfo` must be strictly `processing`. If the CUPS job is in `pending`, `completed`, `canceled`, or `aborted`, injection is refused.
- **`emulator.sh` Processor Validation (Requirement 9)**:
  In `cmd_fault_set`, `active_in_processor` verifies that `EMU_JOB_ACTIVE_FILE` has `STATE=processing`, that `PID` is alive, and that `/proc/${PID}/cmdline` references `processor.sh`, rejecting stale markers and unrelated shell processes.
- **Deterministic Job Execution Gate (`EMU_JOB_GATE_FILE` & `EMU_JOB_RELEASE_FILE`) (Requirement 11)**:
  When `EMU_JOB_GATE_FILE` exists, `processor.sh` enters a controlled polling loop (bounded by `EMU_GATE_TIMEOUT_TENTHS`, default 6.0 seconds fail-safe timeout). This ensures the job remains in the `processing` state long enough for the test harness to inject faults and verify telemetry.
  Creating `EMU_JOB_RELEASE_FILE` signals `processor.sh` to resume. The 6.0-second timeout ensures jobs never hang indefinitely or block subsequent tests if a test crashes.

### 2. Live IPP Reason Updates During Active Jobs

- `ippeveprinter` is a single-threaded print server that handles one print job at a time. Submitting an out-of-band `ipptool Print-Job` probe (`sync_probe.test`) while a real CUPS job is actively printing returns HTTP 500 / `server-error-busy (Currently printing another job.)`.
- When `processor.sh` is active, `emulator.sh fault set <scenario>` detects the active processor via authoritative marker inspection and writes `EMU_FAULT_STATE_FILE` directly, skipping the conflicting `sync_probe.test`.
- `processor.sh` reads `EMU_FAULT_STATE_FILE` in its polling loop and emits IPP filter protocol messages on stderr:
  ```bash
  STATE: -media-jam-error -media-empty-error -door-open-error ...
  STATE: +media-jam-error
  ```
  This immediately updates `ippeveprinter`'s live printer state reasons in real time while servicing the active job.

### 3. SNMP Hardware Telemetry Synchronization

- `snmp_responder` continues serving RFC 3805 / RFC 1213 printer MIB telemetry on localhost UDP.
- `emulator.sh fault set` updates `EMU_FAULT_STATE_FILE` and triggers `snmp_responder` state synchronization (`tools/printer-emulator/snmp_responder -sync-state`).
- XeroxGo's `MonitorCUPSJobWithTelemetryCustom` actively polls SNMP via `PollHealthConfig`, immediately detecting hardware status flags:
  - `paper-jam`: `hrPrinterDetectedErrorState = 04 00 (jammed)` -> `telem.Flags.Jammed = true`
  - `paper-empty`: `hrPrinterDetectedErrorState = 40 00 / 00 01 (noPaper)` -> `telem.Flags.NoPaper = true`
  - `cover-open`: `hrPrinterDetectedErrorState = 08 00 (doorOpen)` -> `telem.Flags.DoorOpen = true`

---

## Hardware Fault Interruption & CUPS Job Hold Protocol

### 1. Separation of Production Hold Attempt from Post-Fault Verification

When a hardware fault is detected via telemetry while CUPS reports the job in `processing`, `MonitorCUPSJobWithTelemetryCustom` attempts to hold the job in CUPS via `HoldAndVerifyCUPSJob` (`lp -i <cupsJobID> -H hold`) and halts monitoring with a hardware interruption error:
- `paper-jam`: `paper feed jam detected in printer: please clear the jammed paper before continuing`
- `paper-empty`: `printer paper tray is empty: please load paper to continue printing`
- `cover-open`: `printer cover is open: please close the printer cover to continue`

**Post-Fault Verification Separation**:
- Post-fault verification queries the real CUPS job via `QueryCUPSJobInfo` (or `ipptool get-job-attributes.test`) **without issuing another hold command**.
- Explicitly classifies and reports the real CUPS job outcome:
  - `confirmed-held`: CUPS state is `pending-held` with reasons including `job-hold-until-specified`.
  - `still-processing`: Job remains in `processing` or `pending` (hold attempted but not yet held).
  - `already-completed`: Job completed before hold took effect (`lp` refused hold with exit status 1).
  - `hold-failed`: CUPS returned error or aborted/canceled state.
  - `unknown`: State unparseable or ambiguous.
- Never claims `confirmed-held` when only an attempt occurred.

### 2. Decoupling Physical Sheet Delivery

Physical paper ejection is decoupled from simulated software processing:
- The `onPage` callback of `MonitorCUPSJobWithTelemetryCustom` tracks physical sheet delivery increments.
- In simulated hardware faults, `onPage` calls remain strictly `0`.
- The simulated SNMP odometer (`prtMarkerLifeCount` at OID `1.3.6.1.4.1.2699.1.2.1.2.1.13.1.1`) is queried before and after fault injection, verifying the count remains unchanged at 1000 sheets. No synthetic page advance is treated as physical sheet delivery.

### 3. Cleanup Safety & Queue Isolation

- **No Unverified Queue-Wide Cancel**: Removed all `cancel <queue>` invocations from test teardown.
- **Destination Verification Before Cancel**: Held jobs are canceled individually by their exact captured CUPS job ID (`<queue>-<jobNum>`), and only after validating that the destination queue belongs to the emulator (3-way URI check).
- **Metadata Preservation on Failure**: If `remove-cups` fails during cleanup, ownership metadata is strictly preserved outside `t.TempDir()` (in `/tmp/`) for audit and recovery, and the teardown reports a test failure.

---

## Observed Behaviors & Emulation Limitations

1. **`ippeveprinter` Single-Job Filter Concurrency**:
   - `ippeveprinter` processes one job filter process at a time. Out-of-band `Print-Job` probes sent during active printing return HTTP 500 / `server-error-busy (Currently printing another job.)`.
   - Dynamic IPP attribute synchronization during active jobs is achieved by routing filter state protocol strings (`STATE: +<reason>-error`) through the active job's stderr directly into the parent `ippeveprinter` process.
2. **CUPS Hold Semantics on Active vs Completed Jobs**:
   - For active jobs, CUPS supports holding the job (`lp -i <id> -H hold`), placing it into `pending-held`.
   - Once a job finishes processing in CUPS, `lp -H hold` returns exit status 1 with: `lp: Job #... is finished and cannot be altered.`
   - Handled truthfully as `already-completed`, never fabricated as held.
3. **Queue Cleanup and Teardown Safety**:
   - All integration tests use dynamic, isolated queue names (`XeroxGoMidQ_<PID>_<RAND>`) and unique mDNS emulator names.
   - 3-way URI verification is enforced prior to modifying or removing any CUPS queue.
   - Baseline queues (`DCPT310`) remain untouched across all runs.

---

## Test Suites & Validation Results

### 1. Go Integration Test Suite (`kiosk-daemon/internal/printer`)
Command: `go test -v -race -run "TestMidPrint" ./kiosk-daemon/internal/printer -timeout 120s`
- `TestMidPrintHardwareFault_PaperJam`: **PASS** (6.42s) (Job confirmed active, paper-jam injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact, post-fault query confirmed held without re-issuing hold).
- `TestMidPrintHardwareFault_PaperEmpty`: **PASS** (6.31s) (Job confirmed active, paper-empty injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact).
- `TestMidPrintHardwareFault_CoverOpen`: **PASS** (6.41s) (Job confirmed active, cover-open injected, SNMP detected, CUPS held in `pending-held`, 0 page progress, odometer intact).
- `TestMidPrintFault_RejectStaleAndMismatchedMarkers`: **PASS** (0.00s) (Comprehensive negative tests: missing marker, malformed/incomplete marker, missing PID, mismatched `JOB_NAME`, `STATE=completed`, dead PID, unrelated live PID reuse, invalid IPP job ID, pending CUPS job rejection, completed CUPS job rejection, verified processor PID + processing state acceptance).
- `TestMidPrintFault_FailedHoldAttempt`: **PASS** (0.01s) (Invalid job ID returns `HoldErr`, `ConfirmedHeld=false`, accurately reported as hold-failed).
- `TestMidPrintHardwareFault_DistinguishCompletedFromHeld`: **PASS** (5.72s) (Completed job reports `AlreadyCompleted=true`, `ConfirmedHeld=false`, CUPS refuses hold alteration).
- **Result: 6/6 tests PASS in 25.90s with race detector enabled.**

### 2. Shell Mid-Print Fault Suite (`tools/printer-emulator/midprint_fault_test.sh`)
Command: `bash tools/printer-emulator/midprint_fault_test.sh`
- Test 1: Start IPP & SNMP emulators on ephemeral ports: **PASS**
- Test 2: Setup driverless CUPS queue with 3-way URI check: **PASS**
- Test 3: Mid-Print Paper-Jam detection, fault synchronization, CUPS hold, decoupling: **PASS**
- Test 4: Mid-Print Cover-Open detection and CUPS hold: **PASS**
- Test 5: Mid-Print Paper-Empty detection and CUPS hold: **PASS**
- Test 6a: Validation correctly rejected mismatched job ID: **PASS**
- Test 6b: Validation correctly rejected completed marker: **PASS**
- Test 6c: Validation correctly rejected stale marker with dead PID: **PASS**
- Test 6d: Validation correctly rejected incomplete marker: **PASS**
- Test 6e: Validation correctly rejected unrelated live process PID: **PASS**
- Test 6f: Validation correctly rejected invalid IPP job identity: **PASS**
- Test 7: Failed hold attempt on non-existent job ID reported hold-failed outcome: **PASS**
- Test 8a: `validate_active_job` correctly rejected a completed CUPS job: **PASS**
- Test 8b: CUPS distinguishes already-completed job and refuses hold alteration: **PASS**
- Test 9: Traceability and single dispatch verification in CUPS: **PASS**
- Test 10: Verified CUPS queue cleanup and teardown safety: **PASS**
- **Result: 20/20 tests PASS.**

### 3. Regression Test Suites
- `tools/printer-emulator/smoke_test.sh`: **PASS** (19/19 tests passing)
- `tools/printer-emulator/snmp_smoke_test.sh`: **PASS** (16/16 tests passing)
- `tools/printer-emulator/fault_smoke_test.sh`: **PASS** (69/69 tests passing)
- `tools/printer-emulator/lifecycle_test.sh`: **PASS** (14/14 tests passing)
- Monorepo Verification Gate (`scripts/verify_gate.sh`): **PASS** (All 6 gates passing)
