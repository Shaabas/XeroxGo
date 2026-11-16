# XeroxGo Interrupted Job Recovery Validation (Task D4b-3)

This document details the architecture, test scenarios, verified invariants, discovered behavioral nuances, and limitations for Task D4b-3: **Interrupted Job Recovery Validation** in `Shaabas/XeroxGo`.

---

## 1. Executive Summary & Goals

Task D4b-3 validates the safe recovery of interrupted CUPS jobs by the XeroxGo `kiosk-daemon` spooler runtime without duplicate printing, progress inflation, or unauthorized release of held print jobs.

The test suite exercises real CUPS jobs running against the localized D1–D3 IPP/SNMP emulator, real SQLite state persistence (`state.Store`), and the production `reconcileInterruptedCUPSJob`, `reconcileSheetsAndPages`, and startup reconciliation mechanisms in `kiosk-daemon/internal/spooler/client.go`.

Key verified guarantees:
1. **Never Duplicate Dispatch**: Interrupted job recovery strictly never submits a new CUPS job (`lp`), never dispatches duplicate print commands, and never re-queues unverified jobs.
2. **Never Automatically Release Held Jobs**: Jobs held in CUPS (`pending-held`) due to hardware faults or connectivity interruptions are **never** automatically resumed (`lp -H resume` / IPP Resume-Job) by reconciliation.
3. **Active/Pending Job Hold & State Inspection**: When an interrupted job is discovered in an active or queued state (`processing` or `pending`), reconciliation immediately triggers a verified hold attempt (`HoldAndVerifyCUPSJob`) and accurately distinguishes resulting states:
   - `ConfirmedHeld`: Successfully moved to `pending-held`.
   - `StillProcessing`: Remains active despite hold request (unmonitored physical output risk flagged).
   - `AlreadyCompleted`: Completed before hold could execute.
   - `HoldFailed`: Hold request rejected or failed.
4. **Decoupled Physical Proof from CUPS State**: CUPS job state transitions to `completed` are **never** trusted as physical delivery proof. Completed CUPS jobs without hardware odometer telemetry verification remain in `PAUSED_ERROR` requiring manual reconciliation.
5. **Preservation of Verified Physical Progress**: Reconciliation strictly preserves verified odometer progress (`SheetsPrinted`), clamps progress to `[0, TotalSheets]`, and rejects unverified CUPS IPP sheets-completed counters.
6. **Ambiguous Job Safety**: Ambiguous states (`canceled`, `aborted`, `unknown`, or failed CUPS queries) safely preserve `PAUSED_ERROR` with explicit operator reconciliation requirements.
7. **Idempotence**: Repeated reconciliation passes are strictly idempotent: no duplicate dispatches, no state changes, and no progress counter inflation.
8. **Startup Reconciliation**: On daemon boot/restart, incomplete jobs in `PAUSED_ERROR` have their CUPS jobs inspected and held, preserving `PAUSED_ERROR` with operator reconciliation notices.
9. **Zero Production Changes**: All validation was achieved with test suites (`recovery_test.sh` and `interrupted_recovery_integration_test.go`) without altering production printing, recovery, payment, database, or backend contracts.

---

## 2. Architecture & Recovery Mechanisms

### 2.1 Spooler Reconciliation Workflow

The recovery lifecycle in `kiosk-daemon/internal/spooler/client.go` operates as follows:

```
+----------------------------------------------------------------------------------+
|                   Trigger: Spooler Health Recovery / Startup                      |
+----------------------------------------------------------------------------------+
                                          |
                                          v
                      [Query SQLite for Incomplete Job]
                                          |
                                          v
                      Is Job in PAUSED_ERROR with CUPSJobID?
                       /                               \
                     No                                 Yes
                     /                                   \
                 [Ignore]                        [QueryCUPSJobInfo]
                                                         |
         +-----------------------------------------------+-----------------------------------------------+
         |                               |                               |                               |
         v                               v                               v                               v
[State: Pending/Processing]      [State: Completed]             [State: Held]               [State: Canceled/Aborted/Unknown]
         |                               |                               |                               |
         v                               v                               v                               v
[HoldAndVerifyCUPSJob]           [Verify Physical Proof]       [Preserve Held Job]          [Flag Unresolved/Ambiguous]
         |                               |                               |                               |
         v                               v                               v                               v
Distinguish Outcome:             Does Odometer == Total?       No action on CUPS;           Preserve PAUSED_ERROR;
- ConfirmedHeld:                 No -> Keep PAUSED_ERROR       Preserve PAUSED_ERROR;       Log operator reconciliation
  Preserve PAUSED_ERROR                (manual reconciliation) Update progress if verified   notice
- StillProcessing:
  Flag unmonitored risk
- AlreadyCompleted:
  Flag physical uncertainty
```

### 2.2 Sheet & Page Reconciliation (`reconcileSheetsAndPages`)

Physical sheet accounting strictly follows the invariant that hardware odometer telemetry is the sole authority for physical paper ejection:

```go
func reconcileSheetsAndPages(cmd contracts.PrintJobCommand, savedSheets, savedPages, cupsSheetsCompleted int) (int, int) {
    // CUPS IPP sheets counter is untrusted; baseline preserved from verified store
    sheets := savedSheets
    if sheets < 0 { sheets = 0 }
    if sheets > cmd.TotalSheets { sheets = cmd.TotalSheets }
    
    // Pages printed computed strictly from verified sheets
    var pages int
    if cmd.Duplex == contracts.DuplexSimplex {
        pages = (cmd.StartFromPage - 1) + sheets/cmd.Copies
    } else {
        pages = (cmd.StartFromPage - 1) + (sheets*2)/cmd.Copies
    }
    if pages > cmd.TotalPages { pages = cmd.TotalPages }
    return sheets, pages
}
```

---

## 3. Test Suites & Coverage Matrix

### 3.1 Shell Integration Suite (`tools/printer-emulator/recovery_test.sh`)

A dedicated bash test suite executing real CUPS commands (`lp`, `lpstat`, `cancel`), emulator lifecycle controls (`emulator.sh`), and D3 fault injection.

| Scenario | Invariant / Behavior Tested | Result |
| :--- | :--- | :--- |
| **Scenario 1** | Paper-jam interruption holds active job in CUPS (`pending-held`); fault clear preserves hold | **PASS** |
| **Scenario 2** | Paper-empty interruption holds active job in CUPS (`pending-held`); fault clear preserves hold | **PASS** |
| **Scenario 3** | Cover-open interruption holds active job in CUPS (`pending-held`); fault clear preserves hold | **PASS** |
| **Scenario 4** | Connectivity outage (`offline`) with hold preserves job in CUPS after network restoration | **PASS** |
| **Scenario 5** | Active/pending job recovery triggers hold attempt and reports verified held state | **PASS** |
| **Scenario 6** | Completed CUPS job without physical proof decoupled from completion; physical output uncertain | **PASS** |
| **Scenario 7** | Simulated odometer advancement: verified physical sheets decoupled from CUPS counters | **PASS** |
| **Scenario 8** | Canceled/aborted CUPS jobs detected and flagged as ambiguous requiring operator reconciliation | **PASS** |
| **Scenario 9** | Failed CUPS query / missing job fails safely without re-submitting or crashing | **PASS** |
| **Scenario 10** | Repeated recovery queries are idempotent: state remains stable, 0 duplicate dispatches | **PASS** |
| **Scenario 11** | Zero duplicate print jobs submitted across all recovery scenarios | **PASS** |
| **Scenario 12** | Host printer `DCPT310` remains completely untouched | **PASS** |
| **Scenario 13** | Failed CUPS queue removal strictly preserves ownership metadata | **PASS** |
| **Scenario 14** | Verified teardown cleanly deletes queue and metadata | **PASS** |

### 3.2 Go Spooler Integration Suite (`kiosk-daemon/internal/spooler/interrupted_recovery_integration_test.go`)

Full integration suite executing `spooler.Client`, `state.Store` (SQLite), and the real D1–D3 emulator under Go's race detector (`-race`).

| Test Name | Description & Invariants Verified | Result |
| :--- | :--- | :--- |
| `TestInterruptedRecovery_PaperJam_PreservesHeldJobAndPausedError` | Injects mid-print `paper-jam` via `processor.sh` gate. Verifies job is placed in `PAUSED_ERROR` and held in CUPS. Reconciles after fault clear: verifies job is NOT released, `PAUSED_ERROR` is preserved, 0 duplicates. | **PASS** |
| `TestInterruptedRecovery_PaperEmpty_PreservesHeldJobAndPausedError` | Injects mid-print `paper-empty`. Verifies CUPS job is held (`pending-held`), status is `PAUSED_ERROR`. Reconciles after fault clear: verifies hold preserved, no duplicate dispatch. | **PASS** |
| `TestInterruptedRecovery_CoverOpen_PreservesHeldJobAndPausedError` | Injects mid-print `cover-open`. Verifies CUPS hold, SQLite `PAUSED_ERROR`. Reconciles after cover closed: verifies hold preserved. | **PASS** |
| `TestInterruptedRecovery_ConnectivityOutage_PreservesHeldJobAndPausedError` | Injects mid-print `offline` outage (IPP+SNMP frozen). Holds job in CUPS. Reconciles after network restoration: verifies hold preserved, status remains `PAUSED_ERROR`. | **PASS** |
| `TestInterruptedRecovery_ActiveOrPendingJob_TriggersHoldAttempt` | Discovers an interrupted job still in `processing` in CUPS. Verifies reconciliation issues hold request and accurately confirms `pending-held` state with operator reconciliation notice. | **PASS** |
| `TestInterruptedRecovery_CompletedCUPSJob_WithoutPhysicalProof_PreservesPausedError` | Job completed in CUPS while emulator was gated, but physical odometer recorded 0 sheets. Reconciliation strictly refuses to declare completion, preserving `PAUSED_ERROR`. | **PASS** |
| `TestInterruptedRecovery_PartialPhysicalProgress_PreservesVerifiedOdometer` | Job completed in CUPS with 1 sheet, but odometer advanced by 2 sheets (e.g. 2 copies). Reconciliation preserves verified 2 sheets and strictly rejects untrusted CUPS counters. | **PASS** |
| `TestInterruptedRecovery_CanceledOrAbortedCUPSJob_PreservesPausedError` | Interrupted job canceled or aborted in CUPS. Reconciliation records ambiguous state and operator reconciliation requirement in SQLite. | **PASS** |
| `TestInterruptedRecovery_FailedQueryOrMissingJob_FailsSafely` | Interrupted job references non-existent CUPS job ID. Reconciliation fails safely without crashing or resubmitting print job. | **PASS** |
| `TestInterruptedRecovery_RepeatedReconciliation_Idempotence` | Executes 5 consecutive reconciliation cycles on an interrupted job. Verifies status, sheets, error reasons, and CUPS state remain identical across all cycles. | **PASS** |
| `TestInterruptedRecovery_StartupRecovery_IncompleteJob` | Daemon boots with incomplete job in SQLite. On startup discovery, client inspects CUPS, holds active job, and preserves `PAUSED_ERROR` with reconciliation required message. | **PASS** |
| `TestInterruptedRecovery_TeardownPreservesMetadataOnFailedCleanup` | Simulates failed CUPS queue removal. Verifies `.meta` file is strictly preserved for administrative audit. | **PASS** |

---

## 4. Discovered Behavioral Nuances & Production Observations

During integration validation against real CUPS and SQLite state persistence, several behavioral nuances were identified and documented:

1. **Transient Query Failure in Runtime Reconciliation vs Startup**:
   - In `client.go:reconcileInterruptedCUPSJob`: When `QueryCUPSJobInfo` fails (e.g., transient CUPS scheduler timeout or missing job ID), the function logs an error and returns immediately without modifying the SQLite state. This safely preserves `PAUSED_ERROR` in SQLite without resubmitting, though it does not append the transient query error to `ErrorReason`.
   - In `client.go:Run` (Startup recovery): Startup reconciliation explicitly appends `cups query failed: ...` to `ErrorReason` in SQLite. Both behaviors safely fail closed, preserving `PAUSED_ERROR`.
2. **CUPS Queue Stacking with Held Jobs**:
   - In CUPS, when a print job is held (`pending-held`) at the front of a driverless queue, CUPS does not dispatch subsequent jobs to the backend until the held job is canceled or resumed. Integration test teardowns must ensure test jobs are cleanly canceled so subsequent tests have clear queues.
3. **Decoupling of CUPS Completed from Physical Delivery**:
   - Real CUPS driverless backends mark jobs `completed` as soon as the IPP data stream is transmitted to the IPP printer, long before paper is mechanically fed or ejected. XeroxGo's design of decoupling physical sheet accounting from CUPS job status is essential to preventing billing for unprinted paper.

---

## 5. Non-Interference with Host System

All integration tests and shell scripts strictly conform to host safety invariants:
- **No host printer modification**: The physical host printer `DCPT310` was queried before, during, and after all test runs and remained completely idle and unaffected (`lpstat -p -d`).
- **Dedicated unique queues**: Every test queue used the format `XeroxGoRecovQ_<scenario>_<pid>_<nano>` with 3-way ownership verification (`EMU_PORT`, `EMU_CUPS_QUEUE`, and device URI).
- **Metadata preservation on teardown failure**: In the event of any verified queue removal failure, ownership metadata files are strictly preserved.
