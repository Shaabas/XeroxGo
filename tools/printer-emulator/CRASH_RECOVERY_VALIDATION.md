# XeroxGo Deterministic Crash Recovery Validation (Task D4c-2)

This document details the architecture, test scenarios, empirical validation results, verified safety invariants, and documented production limitations for Task D4c-2: **Deterministic Crash Recovery Tests** in `Shaabas/XeroxGo`.

---

## 1. Executive Summary & Audit Scope

Task D4c-2 reproduces the crash windows documented in [CRASH_RECOVERY_AUDIT.md](file:///home/shabas/Projects/XeroxGo/tools/printer-emulator/CRASH_RECOVERY_AUDIT.md) against real CUPS driverless queues, SQLite WAL persistence, and the localized D1–D3 IPP/SNMP emulator.

Deterministic crash injection is achieved using isolated child subprocesses terminated by child-scoped `SIGKILL` at exact lifecycle points. The parent test runner, system `cupsd`, and physical host printers remain entirely unmolested.

The validation suite empirically confirms:
1. **Safe Retry for Definitely Unsubmitted Jobs (CW-2):** A crash following `SaveJob` leaves `isDefinitelyNotSubmitted == true`. Startup recovery safely re-enqueues the job and prints it to completion with exactly-once CUPS submission.
2. **Fail-Closed Pre-Submission Crashes (CW-4):** A crash following `MarkJobSubmitting` prior to `lp` dispatch leaves `submitted_to_printer = 1` and `cups_job_id = ""`. Startup recovery fails closed into `PAUSED_ERROR`, dispatching zero CUPS commands.
3. **In-Flight `lp` Execution Crashes (CW-5):** A crash occurring while `lp` is actively in flight is independently classified via CUPS queue queries. If CUPS accepted the job before the crash, it becomes an unlinked job; if CUPS did not accept it, it is unsubmitted. In both cases, startup recovery marks `PAUSED_ERROR` with zero duplicate submissions.
4. **Confirmed Defect 1 — Unlinked Active CUPS Spooling (CW-6):** When CUPS accepts a job (`request id is <queue>-<id>`) but the process is killed before `RecordJobSubmission` commits `cups_job_id` to SQLite, restart recovery marks `PAUSED_ERROR` in SQLite. However, because `cups_job_id` is empty, **XeroxGo issues no hold requests and no CUPS queries**. The accepted CUPS job continues spooling, filtering, and printing unmonitored in the background to completion (empirically confirmed with exact `processing` $\to$ `completed` CUPS state evidence).
5. **Mid-Print Crash with Live SNMP Verification & Resumed Monitoring (CW-7):** When a crash occurs mid-print with a persisted CUPS ID and partial live SNMP-verified odometer progress (exercising pre-crash emulator SNMP counter increment and polling), startup recovery resumes active CUPS and SNMP telemetry monitoring to reach `COMPLETED` with exact `TotalSheets` verified.
6. **Preservation of Held Jobs on Mid-Print Faults (CW-7):** Mid-print hardware faults (e.g. paper jam) combined with daemon interruption preserve the CUPS `pending-held` state and prevent unauthorized release.
7. **Retained vs. Purged CUPS History Durability (CW-8):** Interrupted completion with retained CUPS history actively asserts that the CUPS job reached `completed` before startup recovery executes, successfully self-healing to `COMPLETED`. Purged CUPS history fails closed to `PAUSED_ERROR` rather than synthesizing an unverified completion.
8. **Confirmed Defect 2 — Single-Row Limit in Incomplete Job Recovery:** `GetIncompleteJob()` executes `ORDER BY updated_at_ns DESC LIMIT 1`. In a multi-crash or multi-job scenario with two or more incomplete jobs, restart recovery reconciles only the newest job, completely omitting older incomplete jobs.
9. **Title Collision Safety:** Existing recovery does not adopt active CUPS jobs solely by title match when `cups_job_id` is empty, preserving `PAUSED_ERROR`.
10. **Zero Production Code Changes:** All validation is accomplished via integration tests and shell scripts without altering production runtime, database schema, or backend contracts.

---

## 2. Test Matrix & Empirical Validation Results

### 2.1 Go Integration Suite (`kiosk-daemon/internal/spooler/crash_recovery_integration_test.go`)

All 12 deterministic integration scenarios pass with 100% success rate:

| Test Identifier | Crash Window / Focus | Injected Condition & Methodology | Expected & Observed Outcome | Evidence Classification | Status |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `TestCrashRecovery_CW2_SaveJobCrash_SafeRetry` | CW-2 (Unsubmitted) | Child killed by `SIGKILL` immediately after `SaveJob` | Startup recovery detects `isDefinitelyNotSubmitted==true`; re-enqueues job; exactly 1 CUPS submission; status `COMPLETED`. | **Proven Result** (Real child SIGKILL, real CUPS queue, real recovery) | **PASS** (7.94s) |
| `TestCrashRecovery_CW4_PreSubmissionCrash_FailsSafe` | CW-4 (Pre-Submission) | Child killed by `SIGKILL` immediately after `MarkJobSubmitting` | Startup recovery detects ambiguous state (`submitted=1, cups_id=""`); sets `PAUSED_ERROR`; zero `lp` submissions. | **Proven Result** (Real child SIGKILL, zero submissions verified) | **PASS** (6.10s) |
| `TestCrashRecovery_CW5_InFlightLP_CrashDuringSubmission` | CW-5 (In-Flight `lp`) | Child killed by `SIGKILL` while `lp` process is actively in flight | Independent CUPS query establishes acceptance; startup recovery sets `PAUSED_ERROR`; zero duplicate submissions. | **Proven Result** (Synchronized in-flight child SIGKILL, independent CUPS query) | **PASS** (6.89s) |
| `TestCrashRecovery_CW6_LPAccepted_UnlinkedActiveJob_DocumentsUnmonitoredSpooling` | CW-6 (Post-Acceptance) | Child killed after `lp` returns ID, before `RecordJobSubmission` | **Confirmed Defect 1:** SQLite `PAUSED_ERROR`; CUPS job remains active in queue unmonitored (`processing` $\to$ `completed`). | **Proven Result** (Exact CUPS job-state evidence, captured ID, zero hold issued) | **PASS** (7.14s) |
| `TestCrashRecovery_CW7_MidPrint_ResumesMonitoring` | CW-7 (Mid-Print Resumption) | Child killed mid-print after live SNMP-verified sheet 1 | Startup resumes monitoring on existing CUPS ID; 0 duplicate submissions; completes to `COMPLETED` (2/2 sheets). | **Proven Result** (Live SNMP odometer advance, exact `COMPLETED` and `TotalSheets` verified) | **PASS** (7.12s) |
| `TestCrashRecovery_CW7_MidPrint_HardwareFault_PreservesHeldJob` | CW-7 (Mid-Print Fault) | Hardware jam injected mid-print, child killed | Startup recovery preserves CUPS `pending-held` state; status `PAUSED_ERROR`; job never automatically resumed. | **Proven Result** (Real emulator fault, hold verified, no auto-release) | **PASS** (8.93s) |
| `TestCrashRecovery_CW8_CompletionPersistence_RetainedCUPSHistory` | CW-8 (Retained History) | Child killed before SQLite completion update; CUPS completed | Actively asserts CUPS job reached `completed` prior to recovery; verifies physical proof; marks `COMPLETED`. | **Proven Result** (Pre-recovery CUPS `completed` assertion, verified self-healing) | **PASS** (7.90s) |
| `TestCrashRecovery_CW8_CompletionPersistence_PurgedCUPSHistory` | CW-8 (Purged History) | SQLite pre-seeded with purged/non-existent CUPS ID | CUPS returns `StateUnknown`; startup recovery fails closed to `PAUSED_ERROR`; does not synthesize completion. | **Simulated Condition** (Directly seeded SQLite progress with non-existent CUPS ID) | **PASS** (6.09s) |
| `TestCrashRecovery_ReplayedCommands_IdempotentAndConflicting` | Cloud Replay Boundary | Identical & conflicting command replay | Identical command returns current state idempotently; conflicting command rejected with error. | **Proven Result** (Real command handler execution) | **PASS** (6.11s) |
| `TestCrashRecovery_MultipleIncompleteJobs_DocumentsSingleRowLimit` | Recovery Query Limit | Two incomplete jobs present in SQLite (`PAUSED_ERROR` & `PRINTING`) | **Confirmed Defect 2:** `GetIncompleteJob()` recovers only Job 2 (newest); older Job 1 omitted from recovery. | **Proven Result** (Directly seeded SQLite progress, real `GetIncompleteJob` execution) | **PASS** (7.99s) |
| `TestCrashRecovery_TitleCollision_DoesNotAssumeOwnership` | Title Ownership Boundary | Pre-existing CUPS job exists with matching title | Kiosk daemon does not adopt job; remains `PAUSED_ERROR` with empty `cups_job_id`. Title alone does not prove ownership. | **Proven Result** (Real CUPS job pre-seeded, real recovery non-adoption) | **PASS** (7.93s) |
| `TestCrashRecovery_TeardownPreservesMetadataOnFailedCleanup` | Queue Safety | Teardown encountered with simulated unverified queue | Queue ownership metadata file is strictly preserved; cleanup fails closed safely. | **Proven Result** (Failure injection on teardown helper) | **PASS** (1.76s) |

### 2.2 Shell Consistency Suite (`tools/printer-emulator/crash_recovery_test.sh`)

All 9 shell scenarios execute cleanly against real CUPS driverless queues and the localized emulator:

| Scenario | Focus | Methodology & Check | Status | Evidence Classification |
| :--- | :--- | :--- | :--- | :--- |
| **Scenario 1** | CW-2 Unsubmitted State | Schema consistency query confirms `isDefinitelyNotSubmitted==true` (`SPOOLING`, `submitted=0`, `cups_id=''`). | **PASS** | Consistency Check (Shell-only state check) |
| **Scenario 2** | CW-4 Pre-Submission Crash | Schema consistency query confirms ambiguous submission condition (`submitted=1`, `cups_id=''`). Zero simulated SQL updates. | **PASS** | Consistency Check (Shell-only state check) |
| **Scenario 3a** | CW-5 In-Flight `lp` Crash | Independent CUPS query establishes queue state prior to in-flight submission. | **PASS** | Consistency Check (Independent CUPS CLI query) |
| **Scenario 3b** | CW-6 Unlinked Active CUPS Job | Real `lp` submission with gate; exact CUPS state verified active (`processing`), then unmonitored completion (`completed`). | **PASS** | **Proven Result** (Real CUPS job & state evidence) |
| **Scenario 4** | Multiple Incomplete SQLite Jobs | Direct execution of store `LIMIT 1` query confirms omission of older row. | **PASS** | **Proven Result** (Actual SQLite engine query) |
| **Scenario 5** | Stale & Duplicate CUPS Titles | Real CUPS job submitted with title; stored row has empty `cups_job_id`. Title alone does not adopt. | **PASS** | Consistency Check (Real CUPS job + schema check) |
| **Scenario 6** | Purged CUPS History | CUPS query confirms non-existent ID produces 0 scheduler records (`StateUnknown`). | **PASS** | Consistency Check (Real CUPS scheduler query) |
| **Scenario 7** | Safe Teardown Metadata | `verify-queue` ownership check and metadata preservation verified. | **PASS** | **Proven Result** (Emulator CLI verification) |

---

## 3. Empirical Defect Confirmations

### 3.1 Confirmed Defect 1: Unlinked Active CUPS Spooling (CW-5 / CW-6)

**Mechanism:**  
In `kiosk-daemon/internal/printer/cups.go:132-152` and `kiosk-daemon/internal/spooler/client.go:343-356`:
1. `lp` is executed via `PrintPDFWithCUPSHooksCustom`. CUPS accepts the spool file and returns `request id is <queue>-<id>`.
2. A crash (`SIGKILL` or sudden power loss) terminates the process before `RecordJobSubmission(jobID, cupsJobID)` commits to SQLite WAL.
3. In SQLite, the job record has:
   - `submitted_to_printer = 1`
   - `cups_job_id = ""`
   - `status = "PRINTING"`

**Empirical Behavior on Restart:**  
During startup recovery (`kiosk-daemon/internal/spooler/client.go:638-644`), the recovery routine inspects the incomplete job:
```go
// Line 638 of kiosk-daemon/internal/spooler/client.go:
if job.CUPSJobID == "" {
    c.store.UpdateJobStatus(job.JobID, contracts.JobStatusPausedError,
        "interrupted during printer submission; physical output uncertain, manual reconciliation required")
    return
}
```
Because `job.CUPSJobID == ""`, the function updates SQLite to `PAUSED_ERROR` and immediately returns.

**Exact CUPS Job-State Evidence:**  
In `TestCrashRecovery_CW6_LPAccepted_UnlinkedActiveJob_DocumentsUnmonitoredSpooling` and Scenario 3b:
- **Pre-restart CUPS state:** `processing` (reasons: `[job-printing]`).
- **Post-restart CUPS state:** `processing` (XeroxGo issued zero hold requests to CUPS because `cups_job_id` was empty).
- **Post-release CUPS state:** `completed` (reasons: `[job-completed-successfully]`).
- **Defect Impact:** SQLite records the job as paused with 0 sheets verified, while the physical printer actually ejects printed media. The accepted CUPS job proceeds completely unmonitored by XeroxGo.

---

### 3.2 Confirmed Defect 2: Single-Row Limit in Incomplete Job Recovery

**Mechanism:**  
In `kiosk-daemon/internal/state/store.go:524-533`:
```go
func (s *Store) GetIncompleteJob() (*contracts.PrintJobCommand, error) {
    query := `
        SELECT ...
        FROM jobs
        WHERE status IN ('SPOOLING', 'PRINTING', 'PAUSED_ERROR')
        ORDER BY updated_at_ns DESC, job_id ASC
        LIMIT 1
    `
    ...
}
```

**Empirical Behavior on Restart:**  
When two or more incomplete jobs exist in the SQLite database:
- Example: Job 1 is in `PAUSED_ERROR` (from an earlier interrupted print or hardware fault). Job 2 is in `PRINTING` (interrupted during a subsequent crash).
- On daemon startup, `reconcileIncompleteJobOnStartup` invokes `GetIncompleteJob()`.
- Due to `ORDER BY updated_at_ns DESC LIMIT 1`, `GetIncompleteJob()` returns **only Job 2**.
- Job 1 is completely ignored during startup reconciliation and remains stranded in SQLite.

---

## 4. Key Invariants Empirically Verified

1. **Zero Duplicate Printing (Strictly Verified):**  
   All tests utilize `verifyExactlyOnceCUPSSubmissions`, which inspects:
   - Scheduler history via `lpstat -W all <queue>` to detect ANY additional/unexpected CUPS job IDs (not merely occurrences of the original ID).
   - Independent emulator spool directory artifacts (`1-*.pdf`, `2-*.pdf`).
   - `CUPSRunner` execution instrumentation counting `lp -d` print submission dispatches.
   In all crash windows tested (CW-2, CW-4, CW-5, CW-6, CW-7, CW-8), zero duplicate print submissions were dispatched.
2. **Authoritative Physical Proof Required:**  
   In CW-8 retained-history tests, XeroxGo actively asserted that the CUPS job reached `completed` prior to recovery and required verified hardware odometer progress matching `TotalSheets` before marking `COMPLETED`.
3. **Fail-Closed on Unknown / Purged History:**  
   When CUPS history was purged (`StateUnknown`), XeroxGo refused to assume success, safely demoting the job to `PAUSED_ERROR` with an explicit manual reconciliation notice.
4. **Rejection of Title-Based Adoption:**  
   Pre-existing CUPS jobs with matching titles were not automatically adopted when `cups_job_id` was empty. XeroxGo maintained `PAUSED_ERROR`, preventing accidental adoption of unrelated jobs sharing identical display codes or names.
5. **Idempotent Replays:**  
   Cloud commands replayed with identical parameters for an existing job returned the current job status without altering spooler state or dispatching new CUPS jobs.

---

## 5. Distinction: Proven Results, Simulated Conditions & Remaining Uncertainty

To ensure complete scientific integrity, the test methodology explicitly distinguishes three categories:

### 5.1 Proven Results
- **Isolated Subprocess `SIGKILL`:** Real Linux kernel process termination delivered strictly to `cmd.Process.Pid`. The parent test runner and system `cupsd` are never signaled.
- **Real CUPS Scheduler & Driverless Queues:** Real `lp`, `lpadmin`, `lpstat`, and IPP driverless queues communicating with the localized emulator on loopback ports.
- **Real SQLite WAL Persistence:** Real database files on disk under WAL mode (`PRAGMA synchronous=FULL`).
- **Real Startup Recovery Execution:** All Go integration tests instantiate `NewClient` and execute `client.Run()`, exercising the real production startup recovery logic.
- **Exact CUPS Job-State Evidence:** Proven transitions (`processing` $\to$ `completed`) verified through authoritative IPP/CUPS attributes.
- **Live SNMP Odometer Advancement:** In CW-7, the emulator's SNMP counter was advanced before the crash, polled via live UDP SNMP packets, and persisted via `RecordSheetProgress`.

### 5.2 Simulated Conditions
- **Directly Seeded SQLite Progress:** In CW-8 purged history (`TestCrashRecovery_CW8_CompletionPersistence_PurgedCUPSHistory`) and multi-incomplete job recovery (`TestCrashRecovery_MultipleIncompleteJobs_DocumentsSingleRowLimit`), SQLite rows were seeded directly to construct the exact multi-row and non-existent CUPS ID states.
- **Shell Consistency Checks:** Scenarios 1, 2, 3a, 5, and 6 in `crash_recovery_test.sh` execute schema, state, and CLI consistency checks. Simulated SQL updates were completely removed to prevent conflating SQL queries with runtime recovery execution.
- **Purged History Simulation:** Purged scheduler history is simulated using a non-existent CUPS job ID (`queue-99999`) against which CUPS returns `StateUnknown`.

### 5.3 Remaining Uncertainty & Engineering Limitations
- **In-Flight `lp` Kernel Timing Jitter (CW-5):** In a real unexpected power cut or `SIGKILL` during `lp` execution, whether CUPS accepted the spool file depends on exact microsecond scheduling between the `lp` client and `cupsd` socket buffer. The test harness proves that whether CUPS accepted the job (unlinked) or rejected/missed it (unsubmitted), XeroxGo startup recovery safely fails closed to `PAUSED_ERROR` with zero duplicate submissions.
- **Purged CUPS History Telemetry Loss:** When CUPS history has been purged (`MaxJobTime 0` or daemon restart with empty spool), XeroxGo cannot determine whether the job aborted in the spooler or finished printing. The system safely fails closed to `PAUSED_ERROR`, but requires operator manual reconciliation.
