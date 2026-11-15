# Task D4a — Real Print Lifecycle Validation Report

## 1. Overview & Objective

This document records the validation of XeroxGo's real CUPS printing submission, job monitoring, durable recovery persistence, and hardware telemetry client against live local IPP and SNMP emulators (developed in D1–D3).

The validation exercises:
1. Driverless CUPS queue creation targeting the live `ippeveprinter` instance.
2. Real print job submission via `PrintPDFWithCUPSHooksCustom` using standard `lp`.
3. Extraction and durable persistence of the real CUPS Request ID (`<queue>-<jobNum>`).
4. Real job state transitions (`pending`, `processing`, and `completed`).
5. Authoritative job status queries via `QueryCUPSJobInfo` (IPP `Get-Job-Attributes` and `lpstat`).
6. Receipt and preservation of genuine print artifacts (`1-*.pdf`, `1-*.prn`) in the emulator spool directory.
7. Distinguishing single submissions producing multiple artifacts from multiple independent submissions.
8. Real SNMP telemetry polling via `PollHardwareTelemetry` against the live `snmp_responder` instance.
9. Decoupling of CUPS software completion from physical paper ejection.
10. Strict preservation of queue ownership metadata outside temporary directories when deletion fails.
11. Verification of queue ownership metadata, PID protection, and system queue isolation.

---

## 2. Architecture & Lifecycle Flow

```
+-----------------------------------------------------------------------------------------+
|                                    XeroxGo Kiosk                                        |
|                                                                                         |
|  [SQLite state.Store] <--- (1. OnSubmitting / MarkJobSubmitting)                        |
|           |                                                                             |
|           |                 (2. lp submission: -t uniqueJobID)                          |
|           v          ---------------------------------> [Local CUPS Daemon :631]        |
|  [cupsJobID captured]                                               |                   |
|           |                                                         | (Everywhere IPP)  |
|           v                                                         v                   |
|  (3. OnSubmitted)                                     [ippeveprinter :EMU_PORT]         |
|  RecordJobSubmission(cupsJobID)                                     |                   |
|                                                                     v (processor.sh)    |
|                                                        [EMU_SPOOL_DIR / 1-*.pdf, *.prn] |
|                                                                                         |
|  (4. MonitorCUPSJobWithTelemetryCustom)                                                 |
|     |---> QueryCUPSJobInfo(ipptool Get-Job-Attributes / lpstat)                             |
|     |---> PollHardwareTelemetry(SNMPv2c UDP :SNMP_PORT)                                 |
|                                                                                         |
|  (5. Physical Sheets Decoupling Assertion)                                              |
|     - CUPS job-completed != physical delivery                                          |
|     - Physical progress requires hardware odometer proof (SNMP prtMarkerLifeCount)      |
+-----------------------------------------------------------------------------------------+
```

---

## 3. Key Observations & Invariant Enforcements

### 3.1 Strengthened Exactly-Once Submission Invariants
- **Unique Test Job Identity**: Each test run generates a unique job identifier (`job-live-lifecycle-<pid>-<nanos>`) passed as `cmd.JobID` and CUPS job title (`lp -t`).
- **Hook Tracking**: `OnSubmitting` and `OnSubmitted` are each asserted to execute exactly once per accepted job.
- **Durable Persistence**: SQLite state store atomically records the submission (`submitted_to_printer = 1` and `cups_job_id = <capturedID>`), ensuring a single record per job.
- **Actual CUPS Job Records**:
  - The test queries `lpstat -W all <queue>` to inspect CUPS job records.
  - When CUPS job history retention (`PreserveJobHistory`) is active, the test asserts that exactly one job was dispatched to the queue.
  - **Limitation Reporting**: If CUPS job history is purged or disabled on a host, the test explicitly logs this limitation rather than fabricating verification, relying on atomic submission hooks, SQLite persistence, and emulator spool manifest sequence.
- **Distinguishing One Submission with Multiple Artifacts from Multiple Submissions**:
  - A single submission to `ippeveprinter` and `processor.sh` produces multiple artifact files (e.g. `1-<job>.pdf` and `1-<job>.prn`).
  - Both artifact files share the same sequence ID prefix (`1-`).
  - Multiple independent submissions would increment the sequence prefix (`1-`, `2-`, etc.).
  - The test inspects the spool directory, groups artifacts by sequence prefix, and asserts that exactly one sequence exists, verifying that multiple artifact files originate from a single submission.

### 3.2 Real CUPS Request ID Extraction & Durable Recording
- The submission command outputs standard CUPS text: `request id is <queue>-<jobNum> (1 file(s))`.
- The parser matches `^([a-zA-Z0-9_.-]+)$`.
- The post-submission hook atomically writes the CUPS request ID to SQLite (`jobs.cups_job_id`) before monitoring begins.

### 3.3 Real Job Transitions Observed
- **Pending / Processing**: CUPS backend dispatches the job; IPP attributes report `job-state = processing` and `job-state-reasons = job-printing`.
- **Completed**: In driverless emulation, rasterization takes ~100–300ms. The job transitions to `job-state = completed` with reasons `[job-completed-successfully]`.
- Post-completion verification is confirmed via `QueryCUPSJobInfo`.

### 3.4 Physical Delivery Decoupling (Strict Invariant)
- **Invariant**: Physical sheets delivered (`onPage` callback) must **never** be incremented solely because CUPS reports job completion.
- In `MonitorCUPSJobWithTelemetryCustom`, `onPage` is invoked **only** when hardware telemetry proves an odometer increment:
  $$\Delta \text{sheets} = \text{telem.LifeCount} - \text{initialLife} > 0$$
- In the software emulator (`ippeveprinter`), no physical paper passes through rollers. The SNMP odometer remains at baseline (1000).
- Consequently, during CUPS completion, `onPage` was called **0 times**, strictly confirming that software completion does not fabricate physical sheet delivery.

### 3.5 Synthetic Odometer Simulation vs Physical Ejection
- In the local test emulator, the SNMP odometer can be incremented synthetically via `snmp_responder set -counter <n>`.
- `PollHardwareTelemetry` accurately observes this update.
- However, tests and operators must treat this telemetry as **synthetic**, not as proof of physical hardware delivery.

### 3.6 Metadata Preservation on Deletion Failure (Audit Evidence Protection)
- **Problem**: When temporary files are stored in `t.TempDir()`, Go's test runner unconditionally deletes `t.TempDir()` upon test completion. If CUPS queue deletion fails during teardown, the ownership metadata is destroyed, leaving orphaned unmanaged queues without audit evidence.
- **Solution**:
  - Ownership metadata files are stored outside `t.TempDir()` (in `/tmp/xeroxgo_cups_meta_<queue>.meta`).
  - If `remove-cups` fails or is refused, the metadata file is strictly preserved on disk, and teardown logs an error with the preserved path.
  - When `remove-cups` succeeds, `emulator.sh` cleanly removes the metadata file.
  - In `lifecycle_test.sh`, the EXIT trap preserves metadata and reports a non-zero exit code if cleanup fails.

---

## 4. Test Suites and Execution Commands

### 4.1 Go Integration Suite
Executes real submission, SQLite persistence, CUPS monitoring, SNMP telemetry polling, synthetic odometer validation, and deletion failure metadata preservation:
```bash
go test -v -race -run "TestCUPSPrintLifecycle|TestCUPSJobQuery|TestCUPSQueueDeletion" ./kiosk-daemon/internal/printer
```
**Results**:
- `TestCUPSPrintLifecycle_RealEmulatorSubmissionAndTracking`: **PASS** (6.10s)
  - CUPS job history verified exactly 1 job dispatched.
  - Spool artifact sequence verified single submission produced 2 artifacts (`.pdf` and `.prn`).
  - Decoupling verified: 0 physical sheets marked delivered from CUPS completion.
- `TestCUPSPrintLifecycle_SyntheticOdometerAdvancement`: **PASS** (0.06s)
- `TestCUPSJobQuery_LiveTransitions`: **PASS** (5.69s)
- `TestCUPSQueueDeletion_FailurePreservesOwnershipMetadata`: **PASS** (4.61s)
  - Refused removal on URI mismatch; verified ownership metadata was preserved on disk.
  - Verified queue was preserved in CUPS until valid metadata restored.

### 4.2 Standalone Shell Validation Suite
Executable CLI test suite verifying end-to-end lifecycle operations:
```bash
bash tools/printer-emulator/lifecycle_test.sh
```
**Results**:
- Test 1: Start IPP Emulator and verify direct IPP attributes: **PASS**
- Test 2: Start SNMP Responder and verify RFC MIB baseline: **PASS**
- Test 3: Setup driverless CUPS queue with 3-way URI verification: **PASS**
- Test 4: Capture real CUPS request ID from lp output: **PASS**
- Test 5: Verify CUPS request ID format pattern: **PASS**
- Test 5b: Inspect actual CUPS job records: **PASS**
- Test 6: Observe job transitions and verify CUPS completion: **PASS**
- Test 7: Verify spool artifact receipt and distinguish single from multiple submissions: **PASS**
- Test 8: Query live SNMP telemetry after CUPS completion: **PASS**
- Test 9: Physical sheet delivery decoupling assertion (odometer unchanged): **PASS**
- Test 10: Synthetic odometer advancement and documentation: **PASS**
- Test 11: Verified CUPS queue removal and queue isolation: **PASS**
- Test 12: Regression — Failed CUPS queue deletion preserves ownership metadata: **PASS**
- Total: **14 PASSED, 0 FAILED, 0 SKIPPED**

---

## 5. Summary of Emulation Characteristics & Limitations

| Dimension | Real Hardware | Software Emulator (`ippeveprinter` + `snmp_responder`) | Validation Handling |
| :--- | :--- | :--- | :--- |
| **Submission** | Driverless IPP via CUPS | Driverless IPP via CUPS | Identical (`lp -d <queue>`) |
| **CUPS Request ID** | `<queue>-<jobNum>` | `<queue>-<jobNum>` | Identical (`cupsJobIDRegex`) |
| **Spool Output** | Internal memory/rasterizer | File written to `EMU_SPOOL_DIR` (`1-*.pdf`, `1-*.prn`) | Grouped by sequence to distinguish single submission from multiple |
| **Job Latency** | 5–30s (mechanical paper feed & print head) | ~100–300ms (instant file write) | Polled via `QueryCUPSJobInfo` |
| **Odometer Advance** | Physical mechanical paper counter | Static unless synthetically incremented via CLI | Decoupled; `onPage` requires odometer delta |
| **Hardware Faults** | Sensor-driven (paper jam switch, microswitch) | Injected via `fault-set` CLI and signal handling | Coordinated IPP + SNMP state matching |
| **CUPS Job History** | Depends on host `PreserveJobHistory` | Depends on host `PreserveJobHistory` | Truthfully documented if empty; not fabricated |
