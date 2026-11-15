# XeroxGo Active-Job Connectivity Failure Validation (Task D4b-2)

This document details the design, execution, observed outcomes, invariants, and limitations for Task D4b-2: **Active-Job Connectivity Failure Validation** in `Shaabas/XeroxGo`.

---

## 1. Executive Summary & Goals

Task D4b-2 validates the resilience of XeroxGo's print submission, polling monitor, and telemetry reconciler against real IPP and SNMP network connectivity failures occurring mid-job.

The test suite exercises real CUPS jobs running against the localized D1–D3 IPP and SNMP emulator, confirming:
1. **Reachability Separation**: Decouples the reachability of the local CUPS scheduler (`cupsd`), the IPP network endpoint (`ipp://localhost:port/ipp/print`), and the SNMP UDP telemetry endpoint (`127.0.0.1:port`).
2. **Continued Monitoring During SNMP Loss**: Telemetry loss alone does not trigger false hardware error states (no spurious paper jams, cover open, or out-of-paper interruptions) and does not abort CUPS polling.
3. **Decoupled Physical Delivery**: Physical sheet progress remains strictly tied to verified hardware odometer telemetry; SNMP dropouts never produce synthetic page progress.
4. **Job Preservation Under Outage**: When a bounded timeout expires during an active outage, context-aware job preservation prevents CUPS from executing unmonitored job cancellation (`cancel <jobID>`), holding the job safely in CUPS for subsequent recovery.
5. **Connectivity Restoration Without Resubmission**: Once network connectivity is restored, the original CUPS job completes cleanly without duplicate job dispatches.
6. **Failed Restoration Safety**: If restoration fails or the endpoint remains unavailable, the job remains in an unresolved state and is never falsely marked completed or ghost-printed.

---

## 2. Architecture & Harness Design

### 2.1 Subsystem Reachability Separation

| Subsystem | Transport / Socket | Reachability Probe | Behavior During Outage |
| :--- | :--- | :--- | :--- |
| **Local CUPS Scheduler** | UNIX domain socket (`/var/run/cups/cups.sock`) | `lpstat -r`, `lpstat -p <queue>` | **ONLINE**: Continues servicing client queries and queue inspections locally. |
| **IPP Endpoint** | TCP (`localhost:<port>`) | `ipptool -T 1 ... get-printer-attributes.test` | **OFFLINE**: Paused via `SIGSTOP`; TCP handshakes hang / time out. |
| **SNMP Endpoint** | UDP (`127.0.0.1:<port>`) | `PollHardwareTelemetry` / `snmpget` | **OFFLINE**: UDP requests dropped; telemetry poller returns `Online: false`. |

### 2.2 Active-Job Synchronization & Execution Gate

To ensure fault injection targets only a confirmed in-flight print job, the harness enforces:
1. **Atomic Active-Job Marker**: `tools/printer-emulator/processor.sh` atomically writes `EMU_JOB_ACTIVE_FILE` via temporary file rename (`rename(2)`), reporting `JOB_NAME`, `IPP_JOB_ID`, `PID`, and `STATE=processing`.
2. **Process Identity Verification**: Validates that the active PID is alive and its `/proc/<pid>/cmdline` or `/proc/<pid>/comm` contains `processor.sh`, rejecting live PID reuse.
3. **CUPS Processing State Requirement**: `QueryCUPSJobInfo` must report `CUPSJobStateProcessing` before fault injection is permitted.
4. **Execution Gate**: `processor.sh` waits on `EMU_JOB_GATE_FILE` until `EMU_JOB_RELEASE_FILE` is touched (with a bounded safety timeout).

### 2.3 Safe Outage Injection

Outages are injected purely through emulator process and responder control (`emulator.sh fault set offline --target <snmp|ipp|both>`); system networking, iptables, and loopback interfaces are never modified.

---

## 3. Test Scenarios & Observed Outcomes

### Scenario 1: SNMP-Only Outage During Active Printing
- **Injection**: `fault set offline --target snmp`.
- **Reachability**: SNMP drops packets (`Online == false`); IPP and CUPS scheduler remain fully reachable.
- **Observed Behavior**:
  - `MonitorCUPSJobWithTelemetryCustom` continues polling CUPS (`OutcomeContinuedMonitoring`).
  - Telemetry poller reports `Online: false`, safely bypassing hardware flag evaluations.
  - No false paper jam, cover open, or paper empty errors are raised.
  - Hardware odometer tracking is suspended; `onPage` callback is not invoked with synthetic counts.
- **Restoration & Completion**:
  - SNMP connectivity is restored via `fault set clear --target snmp`.
  - Processor gate is released.
  - Monitor observes transition to `CUPSJobStateCompleted` and returns `nil`.
  - Exactly-once submission verified: 1 CUPS job, 1 spool output file, 0 duplicate dispatches.

### Scenario 2: IPP-Only Outage During Active Printing
- **Injection**: `fault set offline --target ipp` (freezes IPP emulator process via `SIGSTOP`).
- **Reachability**: IPP TCP socket is unresponsive; SNMP and CUPS scheduler remain reachable.
- **Observed Behavior**:
  - Monitor runs with bounded context and `ContextWithPreserveCUPSJob`.
  - Monitor times out cleanly (`context.DeadlineExceeded`, `OutcomeTimeout`).
  - Job preservation flag suppresses CUPS cancel, placing the job on hold (`pending-held`, `OutcomeAmbiguousJobState`).
  - Original CUPS job ID remains securely recorded in SQLite state store.
- **Restoration & Completion**:
  - IPP connectivity restored via `fault set clear --target ipp` (`SIGCONT`).
  - Processor gate is released, and CUPS hold is resumed via `lp -i <id> -H resume`.
  - Original job completes in CUPS without resubmission.

### Scenario 3: Combined IPP + SNMP Outage During Active Printing
- **Injection**: `fault set offline --target both`.
- **Reachability**: Both IPP and SNMP endpoints are unresponsive; local CUPS scheduler remains online.
- **Observed Behavior**:
  - Monitor times out cleanly with job preservation.
  - Job remains preserved in CUPS queue (`pending-held`).
- **Restoration & Completion**:
  - Dual restoration (`fault set clear --target both`) restores both subsystems.
  - Processor gate released and CUPS hold resumed.
  - Original job completes cleanly with no duplicate submissions.

### Scenario 4: Failed Restoration & Uncertain Job State (Regression)
- **Injection**: IPP outage injected mid-print; context times out with job preservation.
- **Failure Simulation**: Emulator remains unrestored / offline.
- **Observed Behavior**:
  - CUPS job does NOT report completed (`info.State != CUPSJobStateCompleted`).
  - XeroxGo never reports physical output delivery or false job completion.
  - Original job ID remains preserved in SQLite store for crash recovery / manual reconciliation.

### Scenario 5: Context Cancellation Without Preservation (Negative Regression)
- **Injection**: Active job monitored with canceling context WITHOUT job preservation flag.
- **Observed Behavior**:
  - Context cancellation triggers `cancel <cupsJobID>`.
  - Job is permanently removed from active queue (`CUPSJobStateCanceled` / `CUPSJobStateUnknown`), proving that job preservation is mandatory to prevent accidental in-flight job deletion.

### Scenario 6: Reachability Separation Verification
- Direct probe test proving that:
  - SNMP outage drops SNMP UDP without impacting IPP TCP or CUPS scheduler.
  - IPP outage drops IPP TCP without impacting SNMP UDP or CUPS scheduler.

---

## 4. Test Matrix & Results

### 4.1 Go Integration Suite (`connectivity_fault_integration_test.go`)
Executed with `go test -v -race -timeout 180s ./kiosk-daemon/internal/printer -run 'TestConnectivityFault'`:

| Test Name | Duration | Result |
| :--- | :--- | :--- |
| `TestConnectivityFault_SNMPOnlyOutage_ContinuedMonitoringAndCompletion` | 13.13s | **PASS** |
| `TestConnectivityFault_IPPOnlyOutage_TimeoutPreservationAndRecovery` | 10.28s | **PASS** |
| `TestConnectivityFault_CombinedOutage_RestoreAndComplete` | 14.27s | **PASS** |
| `TestConnectivityFault_FailedRestoration_PreservesUncertainJob` | 7.50s | **PASS** |
| `TestConnectivityFault_CancellationWithoutPreservation_DeletesJob` | 6.74s | **PASS** |
| `TestConnectivityFault_ReachabilitySeparation` | 11.55s | **PASS** |

### 4.2 Shell Integration Suite (`connectivity_fault_test.sh`)
Executed with `./tools/printer-emulator/connectivity_fault_test.sh`:

| Test Case | Description | Result |
| :--- | :--- | :--- |
| **Test 1** | IPP and SNMP emulators started on allocated ports | **PASS** |
| **Test 2** | Driverless CUPS queue created with 3-way URI verification | **PASS** |
| **Test 3** | Independent reachability confirmed (CUPS vs IPP vs SNMP) | **PASS** |
| **Test 4a** | SNMP outage isolated: SNMP UDP dropped while IPP & CUPS up | **PASS** |
| **Test 4b** | Original CUPS job cleanly completed after SNMP restoration | **PASS** |
| **Test 5a** | IPP outage isolated: IPP frozen while SNMP & CUPS up | **PASS** |
| **Test 5b** | Original CUPS job cleanly completed after IPP restoration | **PASS** |
| **Test 6a** | Combined outage verified: both endpoints unresponsive | **PASS** |
| **Test 6b** | Original CUPS job completed after combined restoration | **PASS** |
| **Test 7a** | Job preservation verified: job remains in queue without cancellation | **PASS** |
| **Test 7b** | Negative verification: unpreserved cancellation removes job | **PASS** |
| **Test 8a** | Failed restoration: job is NOT reported completed | **PASS** |
| **Test 9** | Traceable CUPS request IDs with no duplicate dispatches | **PASS** |
| **Test 10a** | Verified test CUPS queue cleanly removed | **PASS** |
| **Test 10b** | Host printer `DCPT310` remained completely isolated | **PASS** |

**Summary**: 15 passed, 0 failed, 0 skipped.

---

## 5. Production Defect Observations & Architectural Limitations

1. **CUPS Hold Resumption Semantics**:
   - When a job is held during timeout preservation (`HoldAndVerifyCUPSJob`), CUPS marks the job `pending-held`. Even when the IPP endpoint is unpaused (`kill -CONT`), CUPS will not automatically dispatch the job to the backend filter until the hold is explicitly resumed (`lp -i <id> -H resume`).
   - This design protects customer funds by preventing spontaneous printing upon reconnect; resuming execution requires explicit recovery dispatch.

2. **Decoupling Communication Loss from Printer Faults**:
   - SNMP UDP packet dropouts must never be treated as physical paper jams or door openings. In `cups.go`, evaluating `telem.Online` before checking error flags prevents transient network jitter from aborting active prints.
   - Physical page ejection tracking requires confirmed hardware odometer advances (`telem.Online && telem.HasLife`); synthetic software counters are correctly ignored.
