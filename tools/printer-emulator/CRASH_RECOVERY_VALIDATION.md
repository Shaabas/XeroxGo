# XeroxGo Deterministic Crash Recovery Validation (Task D4c-2)

This document presents the validation results, empirical defect characterizations, verified safety invariants, and test harness architecture for Task D4c-2: **Deterministic Crash Recovery Tests** in `Shaabas/XeroxGo`.

---

## 1. Executive Summary & Goals

Task D4c-2 reproduces and validates the crash windows documented in [CRASH_RECOVERY_AUDIT.md](file:///home/shabas/Projects/XeroxGo/tools/printer-emulator/CRASH_RECOVERY_AUDIT.md) against real CUPS, SQLite WAL persistence, and the localized D1–D3 IPP/SNMP emulator.

Deterministic crash injection is achieved using isolated child subprocesses that receive `SIGKILL` at precise lifecycle milestones. The parent test runner, system `cupsd`, and physical host printers remain entirely unmolested.

The validation suite empirically confirms:
1. **Safe Retry for Definitely Unsubmitted Jobs (CW-2):** A crash following `SaveJob` leaves `isDefinitelyNotSubmitted == true`. Startup recovery safely re-enqueues the job and prints it to completion with exactly-once CUPS submission.
2. **Fail-Closed Pre-Submission Crashes (CW-4):** A crash following `MarkJobSubmitting` prior to `lp` dispatch leaves `submitted_to_printer = 1` and `cups_job_id = ""`. Startup recovery fails closed into `PAUSED_ERROR`, dispatching zero CUPS commands.
3. **Confirmed Defect 1 — Unlinked Active CUPS Spooling (CW-5/CW-6):** When a crash occurs after `lp` returns a valid CUPS job ID but before `RecordJobSubmission` commits to SQLite, the job enters `PAUSED_ERROR` on restart. However, because `cups_job_id` is empty, **XeroxGo issues no hold requests and no CUPS queries**. The accepted CUPS job continues spooling and printing unmonitored in the background.
4. **Resumed Monitoring on Mid-Print Interruption (CW-7):** When a crash occurs mid-print with a persisted CUPS job ID and partial odometer progress, startup recovery resumes active CUPS and SNMP telemetry monitoring to complete the job cleanly.
5. **Preservation of Held Jobs on Mid-Print Faults (CW-7):** Mid-print hardware faults (e.g. paper jam) combined with daemon interruption preserve the CUPS `pending-held` state and prevent unauthorized release.
6. **Retained vs. Purged CUPS History Durability (CW-8):** Interrupted completion with retained CUPS history reconciles against scheduler proof. Purged CUPS history fails closed to `PAUSED_ERROR` rather than synthesizing an unverified completion.
7. **Replayed Command Safety:** Identical replayed cloud commands are handled idempotently; conflicting commands for an active job are strictly rejected.
8. **Confirmed Defect 2 — Single-Row Limit in Incomplete Job Recovery:** `GetIncompleteJob()` executes `ORDER BY updated_at_ns DESC LIMIT 1`. In a multi-crash or multi-job scenario with two or more incomplete jobs, restart recovery reconciles only the newest job, completely omitting older incomplete jobs.
9. **Title Collision Safety:** Existing recovery does not adopt active CUPS jobs solely by title match when `cups_job_id` is empty, preserving `PAUSED_ERROR`.
10. **Zero Production Code Changes:** All validation is accomplished via integration tests and shell scripts without altering production runtime, database schema, or backend contracts.

---

## 2. Test Matrix & Empirical Validation Results

### 2.1 Go Integration Suite (`kiosk-daemon/internal/spooler/crash_recovery_integration_test.go`)

All 11 deterministic integration scenarios pass with 100% success rate:

| Test Identifier | Crash Window / Scenario | Injected Condition | Expected & Observed Outcome | Status |
| :--- | :--- | :--- | :--- | :--- |
| `TestCrashRecovery_CW2_SaveJobCrash_SafeRetry` | CW-2 | Subprocess killed immediately after `SaveJob` | Restart re-enqueues job; exactly one CUPS submission; 1 sheet printed; status `COMPLETED`. | **PASS** (7.91s) |
| `TestCrashRecovery_CW4_PreSubmissionCrash_FailsSafe` | CW-4 | Subprocess killed immediately after `MarkJobSubmitting` | Restart detects ambiguous submission (`submitted=1, cups_id=""`); sets `PAUSED_ERROR`; 0 CUPS submissions. | **PASS** (6.10s) |
| `TestCrashRecovery_CW5_CW6_UnlinkedActiveJob_DocumentsUnmonitoredSpooling` | CW-5 / CW-6 | Subprocess killed after `lp` returns ID but before `RecordJobSubmission` | **Confirmed Defect 1:** Restart marks `PAUSED_ERROR` in SQLite, but CUPS job remains active in queue unmonitored; no hold attempted. | **PASS** (6.88s) |
| `TestCrashRecovery_CW7_MidPrint_ResumesMonitoring` | CW-7 | Subprocess killed mid-print with partial odometer progress | Restart resumes monitoring on existing CUPS ID; 0 duplicate submissions; finishes to `COMPLETED`. | **PASS** (25.49s) |
| `TestCrashRecovery_CW7_MidPrint_HardwareFault_PreservesHeldJob` | CW-7 | Hardware jam injected mid-print, subprocess killed | Restart preserves CUPS `pending-held` state; status `PAUSED_ERROR`; job never automatically resumed. | **PASS** (8.94s) |
| `TestCrashRecovery_CW8_CompletionPersistence_RetainedCUPSHistory` | CW-8 | Subprocess killed before SQLite completion update; CUPS retains job | Restart queries CUPS history (`completed`); verifies physical odometer; marks `COMPLETED`. | **PASS** (7.13s) |
| `TestCrashRecovery_CW8_CompletionPersistence_PurgedCUPSHistory` | CW-8 | Subprocess killed before completion update; CUPS history purged | Restart receives `StateUnknown` from CUPS; fails closed to `PAUSED_ERROR`; does not synthesize completion. | **PASS** (6.10s) |
| `TestCrashRecovery_ReplayedCommands_IdempotentAndConflicting` | Cloud Boundary | Identical command re-sent; conflicting command re-sent | Identical command returns current state idempotently; conflicting command rejected with error. | **PASS** (6.17s) |
| `TestCrashRecovery_MultipleIncompleteJobs_DocumentsSingleRowLimit` | Recovery Query | Two incomplete jobs present in SQLite (`PAUSED_ERROR` & `PRINTING`) | **Confirmed Defect 2:** `GetIncompleteJob()` recovers only Job B (newest); Job A omitted from startup recovery. | **PASS** (8.03s) |
| `TestCrashRecovery_TitleCollision_DoesNotAssumeOwnership` | CW-5 / CW-6 | Pre-existing CUPS job exists with matching title | Kiosk daemon does not adopt job; remains `PAUSED_ERROR` with empty `cups_job_id`. | **PASS** (7.92s) |
| `TestCrashRecovery_TeardownPreservesMetadataOnFailedCleanup` | Queue Safety | Teardown encountered with simulated unverified queue | Queue ownership metadata file is preserved; cleanup fails closed safely. | **PASS** (1.75s) |

### 2.2 Shell Validation Suite (`tools/printer-emulator/crash_recovery_test.sh`)

All 7 shell scenarios execute cleanly against real CUPS driverless queues and the localized emulator:

| Scenario | Description | Verified Result | Status |
| :--- | :--- | :--- | :--- |
| **Scenario 1** | CW-2 Unsubmitted Job Retry Verification | Confirmed `isDefinitelyNotSubmitted == true` (`SPOOLING`, `submitted=0`, `cups_id=""`). Safe for retry. | **PASS** |
| **Scenario 2** | CW-4 Pre-Submission Crash Fail-Closed | Confirmed ambiguous submission transitions to `PAUSED_ERROR`; zero CUPS submissions. | **PASS** |
| **Scenario 3** | CW-5/CW-6 Unlinked Active CUPS Spooling Defect | Confirmed SQLite `PAUSED_ERROR` while CUPS job remains active in queue unmonitored. | **PASS** |
| **Scenario 4** | Multiple Incomplete SQLite Jobs `LIMIT 1` Gap | Confirmed `LIMIT 1` returns newest job; older incomplete job omitted from recovery. | **PASS** |
| **Scenario 5** | Stale & Duplicate CUPS Job Titles | Confirmed matching-title CUPS job is not adopted; title alone does not prove ownership. | **PASS** |
| **Scenario 6** | Purged CUPS History Fails Closed | Confirmed purged CUPS job fails closed to `PAUSED_ERROR`; no synthesized completion. | **PASS** |
| **Scenario 7** | Safe Teardown Metadata Preservation | Confirmed ownership verification and safe cleanup preservation. | **PASS** |

---

## 3. Empirical Defect Confirmations

### 3.1 Confirmed Defect 1: Unlinked Active CUPS Spooling (CW-5 / CW-6)

**Mechanism:**  
In `kiosk-daemon/internal/printer/cups.go:132-152` and `kiosk-daemon/internal/spooler/client.go:343-356`:
1. `lp` is executed via `PrintPDFWithCUPSHooksCustom`. CUPS accepts the spool file and prints `request id is <queue>-<id>`.
2. A crash (`SIGKILL` or sudden power cut) occurs before `RecordJobSubmission(jobID, cupsJobID)` is committed to SQLite.
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

**Defect Impact:**  
- **No Hold Issued:** The spooler never calls `HoldAndVerifyCUPSJob` because it has no job ID.
- **Unmonitored Spooling:** The CUPS scheduler has already accepted the print job. The CUPS job proceeds through filtering, rasterization, and physical output completely unmonitored by XeroxGo.
- **Divergence:** SQLite records the job as paused with 0 sheets verified, while the physical printer actually ejects printed media.
- **Risk:** If an operator manually resumes the job or re-triggers submission without checking CUPS, duplicate printing occurs.

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
- Job 1 is completely ignored during startup reconciliation.

**Defect Impact:**  
- Any previously interrupted jobs remain permanently stranded in SQLite without reconciliation, telemetry inspection, or hold verification.
- In production, multiple crashes or queued error states cause all but the newest job to be neglected.

---

## 4. Key Invariants Empirically Verified

1. **Zero Duplicate Printing:**  
   In all crash windows tested (CW-2, CW-4, CW-7, CW-8), the kiosk daemon never issued a second `lp` command for an already-submitted job.
2. **Authoritative Physical Proof Required:**  
   A CUPS job status transition to `completed` is never sufficient to complete a job. In CW-8 tests where CUPS indicated completion, XeroxGo required verified hardware odometer progress matching `TotalSheets` before marking `COMPLETED`.
3. **Fail-Closed on Unknown / Purged History:**  
   When CUPS history was purged (job unknown to CUPS scheduler), XeroxGo refused to assume success, demoting the job to `PAUSED_ERROR` with an explicit manual reconciliation notice.
4. **Rejection of Title-Based Adoption:**  
   Pre-existing CUPS jobs with matching titles were not automatically adopted when `cups_job_id` was empty. XeroxGo maintained `PAUSED_ERROR`, preventing accidental adoption of unrelated jobs sharing identical display codes or names.
5. **Idempotent Replays:**  
   Cloud commands replayed with identical parameters for an existing job returned the current job status without altering spooler state or dispatching new CUPS jobs.

---

## 5. Subprocess Test Harness Safety Architecture

To guarantee strict safety during crash testing:
1. **Subprocess Isolation:** Crash scenarios are executed inside isolated child worker processes spawned via:
   ```go
   cmd := exec.Command(os.Args[0], "-test.run=TestCrashHelperProcess", "--")
   cmd.Env = append(os.Environ(), "GO_WANT_CRASH_HELPER_PROCESS=1", ...)
   ```
2. **Strict Signal Scoping:** The supervisor process issues `proc.Process.Signal(syscall.SIGKILL)` strictly to `cmd.Process.Pid`. The parent test runner and system `cupsd` are never signaled.
3. **Queue Ownership and Teardown:** Every test allocates a uniquely named CUPS queue (`XeroxGoCrashQ_<rand>`) backed by the driverless emulator on ephemeral high-range TCP/UDP ports. On exit, queues and state files are verified and cleaned up, preserving system baseline queues (`DCPT310`).
