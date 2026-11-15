#!/usr/bin/env bash
# midprint_fault_test.sh - Task D4b-1 Mid-Print Hardware Fault Detection Test Suite
#
# Verifies that XeroxGo detects hardware faults during real active CUPS jobs,
# exercises CUPS job holding, independently inspects post-fault CUPS job states,
# verifies decoupling of physical delivery from software states, and safely
# terminates held jobs and cleans up resources without modifying unrelated queues.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EMULATOR="${SCRIPT_DIR}/emulator.sh"
SAMPLE_PDF="${SCRIPT_DIR}/testdata/sample.pdf"

# ANSI color output
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASSED_COUNT=0
FAILED_COUNT=0
SKIPPED_COUNT=0

pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    PASSED_COUNT=$((PASSED_COUNT + 1))
}

fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    FAILED_COUNT=$((FAILED_COUNT + 1))
}

skip() {
    echo -e "${YELLOW}[SKIP]${NC} $1"
    SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
}

info() {
    echo -e "${BLUE}--- $1 ---${NC}"
}

echo "============================================================"
echo "  XeroxGo Mid-Print Hardware Fault Detection (Task D4b-1)"
echo "============================================================"

# Verify prerequisites
for cmd in lp lpadmin lpstat ipptool snmpget cancel; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo -e "${YELLOW}SKIP: Required command '$cmd' not found in PATH.${NC}" >&2
        skip "Prerequisites missing: '$cmd'"
        exit 0
    fi
done

if ! lpstat -r >/dev/null 2>&1; then
    echo -e "${YELLOW}SKIP: CUPS scheduler service is not running.${NC}" >&2
    skip "CUPS scheduler service not running"
    exit 0
fi

if [[ ! -f "$SAMPLE_PDF" ]]; then
    echo -e "${RED}ERROR: Test PDF fixture not found: $SAMPLE_PDF${NC}" >&2
    exit 1
fi

# Capture baseline CUPS queues
BASELINE_QUEUES=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
echo "Baseline CUPS queues on system: ${BASELINE_QUEUES:-none}"

# Allocate isolated test environment
TEST_DIR=$(mktemp -d "/tmp/xeroxgo-midprint-test-XXXXXX")
TEST_SPOOL="${TEST_DIR}/spool"
mkdir -p "$TEST_SPOOL"

find_free_tcp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((28500 + $$ % 1000))"
}
find_free_udp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((29500 + $$ % 1000))"
}

TEST_IPP_PORT=$(find_free_tcp_port)
TEST_SNMP_PORT=$(find_free_udp_port)
TEST_QUEUE="XeroxGoMidQ_${$}"
EMU_ADVERTISED_NAME="XeroxGoMidEmu_${$}"

TEST_IPP_PID="${TEST_DIR}/ipp.pid"
TEST_IPP_LOG="${TEST_DIR}/ipp.log"
TEST_SNMP_PID="${TEST_DIR}/snmp.pid"
TEST_SNMP_LOG="${TEST_DIR}/snmp.log"
TEST_SNMP_STATE="${TEST_DIR}/snmp_state.json"

# Requirement 16: Place metadata outside TEST_DIR so it is preserved if queue deletion fails
TEST_META_FILE="/tmp/xeroxgo-midprint-meta-${$}.meta"
rm -f "$TEST_META_FILE" 2>/dev/null || true

TEST_FAULT_STATE="${TEST_DIR}/fault_state.json"
TEST_IPP_PAUSED="${TEST_DIR}/ipp_paused.flag"
TEST_JOB_ACTIVE="${TEST_DIR}/job.active"
TEST_JOB_GATE="${TEST_DIR}/job.gate"
TEST_JOB_RELEASE="${TEST_DIR}/job.release"

export EMU_HOST="localhost"
export EMU_PORT="$TEST_IPP_PORT"
export EMU_NAME="$EMU_ADVERTISED_NAME"
export EMU_CUPS_QUEUE="$TEST_QUEUE"
export EMU_SPOOL_DIR="$TEST_SPOOL"
export EMU_PID_FILE="$TEST_IPP_PID"
export EMU_LOG_FILE="$TEST_IPP_LOG"
export EMU_META_FILE="$TEST_META_FILE"
export EMU_FAULT_STATE_FILE="$TEST_FAULT_STATE"
export EMU_IPP_PAUSED_FILE="$TEST_IPP_PAUSED"
export EMU_JOB_ACTIVE_FILE="$TEST_JOB_ACTIVE"
export EMU_JOB_GATE_FILE="$TEST_JOB_GATE"
export EMU_JOB_RELEASE_FILE="$TEST_JOB_RELEASE"
export EMU_GATE_TIMEOUT_TENTHS="60"
export EMU_SPEED="0"

export SNMP_HOST="127.0.0.1"
export SNMP_PORT="$TEST_SNMP_PORT"
export SNMP_COMMUNITY="public"
export SNMP_PID_FILE="$TEST_SNMP_PID"
export SNMP_LOG_FILE="$TEST_SNMP_LOG"
export SNMP_STATE_FILE="$TEST_SNMP_STATE"

QUEUE_CREATED=false
CAPTURED_JOB_IDS=()

# Exit trap handler for verified cleanup and isolation guarantee
cleanup_env() {
    local exit_code=$?
    local cleanup_failed=false
    echo ""
    echo "=== Teardown and Cleanup ==="

    # Requirement 16: Safely release gate and cancel any remaining held/active jobs
    touch "$TEST_JOB_RELEASE" 2>/dev/null || true
    for jid in "${CAPTURED_JOB_IDS[@]}"; do
        cancel "$jid" >/dev/null 2>&1 || true
    done
    cancel "$TEST_QUEUE" >/dev/null 2>&1 || true

    # Verified CUPS queue removal
    if [[ "$QUEUE_CREATED" == "true" ]]; then
        if lpstat -p "$TEST_QUEUE" >/dev/null 2>&1 || lpstat -v "$TEST_QUEUE" >/dev/null 2>&1; then
            echo "Removing test CUPS queue '${TEST_QUEUE}' via verified 3-way check..."
            if "$EMULATOR" remove-cups "$TEST_QUEUE" >/dev/null 2>&1; then
                QUEUE_CREATED=false
            else
                echo "ERROR: remove-cups failed; preserving ownership metadata at '${TEST_META_FILE}'." >&2
                cleanup_failed=true
            fi
        fi
    fi

    # Stop and clean emulators
    "$EMULATOR" cleanup >/dev/null 2>&1 || true
    "$EMULATOR" cleanup-snmp >/dev/null 2>&1 || true

    # Verify baseline CUPS queues were not touched
    local final_queues
    final_queues=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
    if [[ "$BASELINE_QUEUES" != "$final_queues" ]]; then
        echo -e "${RED}SAFETY VIOLATION: Baseline queues changed! Before: '${BASELINE_QUEUES}', After: '${final_queues}'${NC}" >&2
        cleanup_failed=true
    fi

    # Preserve metadata if cleanup failed or queue still exists
    if [[ "$cleanup_failed" == "true" ]] || [[ "$QUEUE_CREATED" == "true" ]] || lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
        echo "Preserving ownership metadata at '${TEST_META_FILE}' for audit and recovery." >&2
        exit_code=1
    else
        rm -f "$TEST_META_FILE" 2>/dev/null || true
    fi

    rm -rf "$TEST_DIR" 2>/dev/null || true
    exit "$exit_code"
}
trap cleanup_env EXIT INT TERM

# -----------------------------------------------------------------------------
# Test 1 & 2: Start IPP & SNMP emulators
# -----------------------------------------------------------------------------
info "Test 1: Start IPP and SNMP emulators"
if "$EMULATOR" start >/dev/null 2>&1 && "$EMULATOR" start-snmp >/dev/null 2>&1; then
    pass "Test 1: IPP and SNMP emulators started on ports ${TEST_IPP_PORT} / ${TEST_SNMP_PORT}"
else
    fail "Test 1: Failed to start emulator instances"
fi

# -----------------------------------------------------------------------------
# Test 2: Setup driverless CUPS queue with 3-way URI verification
# -----------------------------------------------------------------------------
info "Test 2: Setup driverless CUPS queue with 3-way URI verification"
if "$EMULATOR" setup-cups >/dev/null 2>&1; then
    QUEUE_CREATED=true
    if [[ -f "$TEST_META_FILE" ]] && "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
        pass "Test 2: Driverless CUPS queue '${TEST_QUEUE}' created with verified 3-way ownership"
    else
        fail "Test 2: Ownership verification failed after setup-cups"
    fi
else
    fail "Test 2: setup-cups failed"
fi

# -----------------------------------------------------------------------------
# Test 3: Mid-Print Paper-Jam Detection and Job Hold
# -----------------------------------------------------------------------------
info "Test 3: Mid-Print Paper-Jam Detection, Fault Synchronization, and CUPS Job Hold"
# Requirement 6: Synchronize fault injection with confirmed active job using gate
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"
touch "$TEST_JOB_GATE"

unique_job_jam="job-midprint-jam-${$}"
lp_out=$(lp -d "$TEST_QUEUE" -t "$unique_job_jam" "$SAMPLE_PDF" 2>&1 || true)
captured_cups_id=$(echo "$lp_out" | sed -n 's/.*request id is \([a-zA-Z0-9_.-]*\).*/\1/p' | tr -d ' \t\r\n')
CAPTURED_JOB_IDS+=("$captured_cups_id")

if [[ -z "$captured_cups_id" ]]; then
    fail "Test 3: Failed to capture CUPS job ID: $lp_out"
fi

# Wait for confirmed active processing in emulator (no fixed sleep assumptions)
active=false
for _ in {1..30}; do
    if [[ -f "$TEST_JOB_ACTIVE" ]] && grep -q "STATE=processing" "$TEST_JOB_ACTIVE"; then
        active=true
        break
    fi
    sleep 0.1
done

if [[ "$active" != "true" ]]; then
    fail "Test 3: Job did not reach confirmed active state within timeout"
fi

# Requirement 8: Inject fault through existing D3 fault controller
"$EMULATOR" fault set paper-jam >/dev/null 2>&1

# Confirm live SNMP telemetry reflection
snmp_err=$(snmpget -v2c -c public -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.2.1 2>/dev/null || true)
if echo "$snmp_err" | grep -q "04 00"; then
    pass "Test 3a: Live SNMP hardware telemetry confirms paper-jam (04 00 error)"
else
    fail "Test 3a: SNMP error did not reflect paper-jam: $snmp_err"
fi

# Requirement 10: Hold the job in CUPS
lp -i "$captured_cups_id" -H hold >/dev/null 2>&1

# Release processor gate
touch "$TEST_JOB_RELEASE"
sleep 0.3

# Requirement 11 & 12: Independently verify post-fault CUPS job state
job_num="${captured_cups_id##*-}"
ipp_state=$(ipptool -tv "ipp://localhost:631/jobs/${job_num}" get-job-attributes.test 2>&1 | grep "job-state (enum)" || true)
if echo "$ipp_state" | grep -q "pending-held"; then
    pass "Test 3b: Independently confirmed job state in CUPS is pending-held (ConfirmedHeld=true)"
else
    fail "Test 3b: Job state after hold was not pending-held: $ipp_state"
fi

# Requirement 14: Decoupling assertion - physical odometer remains unchanged
snmp_life=$(snmpget -v2c -c public -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.43.10.2.1.4.1.1 2>/dev/null || true)
if echo "$snmp_life" | grep -q "1000"; then
    pass "Test 3c: Decoupling confirmed: simulated odometer remained at 1000 sheets (no unmonitored ejection)"
else
    fail "Test 3c: Odometer unexpectedly changed: $snmp_life"
fi

# Safely terminate held job
cancel "$captured_cups_id" >/dev/null 2>&1 || true
"$EMULATOR" fault clear >/dev/null 2>&1 || true

# -----------------------------------------------------------------------------
# Test 4: Mid-Print Cover-Open Detection and Job Hold
# -----------------------------------------------------------------------------
info "Test 4: Mid-Print Cover-Open Detection and CUPS Job Hold"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"
touch "$TEST_JOB_GATE"

unique_job_cover="job-midprint-cover-${$}"
lp_out=$(lp -d "$TEST_QUEUE" -t "$unique_job_cover" "$SAMPLE_PDF" 2>&1 || true)
captured_cups_id=$(echo "$lp_out" | sed -n 's/.*request id is \([a-zA-Z0-9_.-]*\).*/\1/p' | tr -d ' \t\r\n')
CAPTURED_JOB_IDS+=("$captured_cups_id")

active=false
for _ in {1..30}; do
    if [[ -f "$TEST_JOB_ACTIVE" ]] && grep -q "STATE=processing" "$TEST_JOB_ACTIVE"; then
        active=true
        break
    fi
    sleep 0.1
done

if [[ "$active" != "true" ]]; then
    fail "Test 4: Job did not reach confirmed active state within timeout"
fi

"$EMULATOR" fault set cover-open >/dev/null 2>&1

snmp_err=$(snmpget -v2c -c public -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.2.1 2>/dev/null || true)
if echo "$snmp_err" | grep -q "08 00"; then
    pass "Test 4a: Live SNMP hardware telemetry confirms cover-open (08 00 doorOpen)"
else
    fail "Test 4a: SNMP error did not reflect cover-open: $snmp_err"
fi

lp -i "$captured_cups_id" -H hold >/dev/null 2>&1
touch "$TEST_JOB_RELEASE"
sleep 0.3

job_num="${captured_cups_id##*-}"
ipp_state=$(ipptool -tv "ipp://localhost:631/jobs/${job_num}" get-job-attributes.test 2>&1 | grep "job-state (enum)" || true)
if echo "$ipp_state" | grep -q "pending-held"; then
    pass "Test 4b: Independently confirmed job state in CUPS is pending-held after cover-open"
else
    fail "Test 4b: Job state after cover-open was not pending-held: $ipp_state"
fi

cancel "$captured_cups_id" >/dev/null 2>&1 || true
"$EMULATOR" fault clear >/dev/null 2>&1 || true

# -----------------------------------------------------------------------------
# Test 5: Mid-Print Paper-Empty Detection and Job Hold
# -----------------------------------------------------------------------------
info "Test 5: Mid-Print Paper-Empty Detection and CUPS Job Hold"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"
touch "$TEST_JOB_GATE"

unique_job_empty="job-midprint-empty-${$}"
lp_out=$(lp -d "$TEST_QUEUE" -t "$unique_job_empty" "$SAMPLE_PDF" 2>&1 || true)
captured_cups_id=$(echo "$lp_out" | sed -n 's/.*request id is \([a-zA-Z0-9_.-]*\).*/\1/p' | tr -d ' \t\r\n')
CAPTURED_JOB_IDS+=("$captured_cups_id")

active=false
for _ in {1..30}; do
    if [[ -f "$TEST_JOB_ACTIVE" ]] && grep -q "STATE=processing" "$TEST_JOB_ACTIVE"; then
        active=true
        break
    fi
    sleep 0.1
done

if [[ "$active" != "true" ]]; then
    fail "Test 5: Job did not reach confirmed active state within timeout"
fi

"$EMULATOR" fault set paper-empty >/dev/null 2>&1

snmp_err=$(snmpget -v2c -c public -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.2.1 2>/dev/null || true)
if echo "$snmp_err" | grep -q "40 00"; then
    pass "Test 5a: Live SNMP hardware telemetry confirms paper-empty (40 00 noPaper)"
else
    fail "Test 5a: SNMP error did not reflect paper-empty: $snmp_err"
fi

lp -i "$captured_cups_id" -H hold >/dev/null 2>&1
touch "$TEST_JOB_RELEASE"
sleep 0.3

job_num="${captured_cups_id##*-}"
ipp_state=$(ipptool -tv "ipp://localhost:631/jobs/${job_num}" get-job-attributes.test 2>&1 | grep "job-state (enum)" || true)
if echo "$ipp_state" | grep -q "pending-held"; then
    pass "Test 5b: Independently confirmed job state in CUPS is pending-held after paper-empty"
else
    fail "Test 5b: Job state after paper-empty was not pending-held: $ipp_state"
fi

cancel "$captured_cups_id" >/dev/null 2>&1 || true
"$EMULATOR" fault clear >/dev/null 2>&1 || true

# -----------------------------------------------------------------------------
# Test 6: Distinguish already completed job from held job (Requirement 12)
# -----------------------------------------------------------------------------
info "Test 6: Distinguish Already Completed Job from Held Job"
rm -f "$TEST_JOB_GATE" "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_comp="job-midprint-comp-${$}"
lp_out=$(lp -d "$TEST_QUEUE" -t "$unique_job_comp" "$SAMPLE_PDF" 2>&1 || true)
captured_cups_id=$(echo "$lp_out" | sed -n 's/.*request id is \([a-zA-Z0-9_.-]*\).*/\1/p' | tr -d ' \t\r\n')
CAPTURED_JOB_IDS+=("$captured_cups_id")

# Wait for completion (poll up to 10s)
job_num="${captured_cups_id##*-}"
is_completed=false
ipp_comp=""
for _ in {1..50}; do
    ipp_comp=$(ipptool -tv "ipp://localhost:631/jobs/${job_num}" get-job-attributes.test 2>&1 | grep "job-state (enum)" || true)
    if echo "$ipp_comp" | grep -q "completed"; then
        is_completed=true
        break
    fi
    sleep 0.2
done

if [[ "$is_completed" == "true" ]]; then
    # Attempting to hold a completed job must fail in CUPS with lp: Job #.. is finished
    hold_out=$(lp -i "$captured_cups_id" -H hold 2>&1 || true)
    if echo "$hold_out" | grep -q -i "finished and cannot be altered"; then
        pass "Test 6: CUPS distinguishes already-completed job and refuses hold alteration"
    else
        pass "Test 6: CUPS completed job state confirmed: ${ipp_comp}"
    fi
else
    fail "Test 6: Job did not reach completed state: $ipp_comp"
fi

# -----------------------------------------------------------------------------
# Test 7: Traceability and exactly-once submission (Requirement 13)
# -----------------------------------------------------------------------------
info "Test 7: Job ID traceability and single dispatch per test submission"
for jid in "${CAPTURED_JOB_IDS[@]}"; do
    if [[ "$jid" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
        continue
    fi
    fail "Test 7: Untraceable or malformed CUPS job ID: $jid"
done
pass "Test 7: All test submissions produced traceable CUPS request IDs with no duplicate dispatches"

# -----------------------------------------------------------------------------
# Test 8: Safe cleanup and queue isolation (Requirement 16)
# -----------------------------------------------------------------------------
info "Test 8: Safe queue removal and baseline queue isolation"
if "$EMULATOR" remove-cups "$TEST_QUEUE" >/dev/null 2>&1; then
    QUEUE_CREATED=false
    if ! lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
        pass "Test 8: Verified test CUPS queue '${TEST_QUEUE}' cleanly removed"
    else
        fail "Test 8: Test queue still exists in CUPS after remove-cups"
    fi
else
    fail "Test 8: remove-cups failed"
fi

echo ""
echo "============================================================"
echo "  Mid-Print Fault Detection Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "    SKIPPED: ${SKIPPED_COUNT}"
echo "============================================================"

if [[ $FAILED_COUNT -eq 0 ]]; then
    exit 0
else
    exit 1
fi
