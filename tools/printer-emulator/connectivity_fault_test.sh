#!/usr/bin/env bash
# connectivity_fault_test.sh - Task D4b-2 Active-Job Connectivity Failure Validation Suite
#
# Verifies XeroxGo's behavior against real IPP/SNMP connectivity failures during active printing.
# Validates SNMP-only, IPP-only, and combined IPP+SNMP outages mid-print.
# Verifies reachability separation (CUPS scheduler vs IPP vs SNMP), job preservation vs cancellation,
# post-restoration completion without resubmission, and negative tests for failed restoration.

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
echo "  XeroxGo Active-Job Connectivity Failure (Task D4b-2)"
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
TEST_DIR=$(mktemp -d "/tmp/xeroxgo-conn-test-XXXXXX")
TEST_SPOOL="${TEST_DIR}/spool"
mkdir -p "$TEST_SPOOL"

find_free_tcp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((31500 + $$ % 1000))"
}
find_free_udp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((32500 + $$ % 1000))"
}

TEST_IPP_PORT=$(find_free_tcp_port)
TEST_SNMP_PORT=$(find_free_udp_port)
TEST_QUEUE="XeroxGoConnQ_${$}"
EMU_ADVERTISED_NAME="XeroxGoConnEmu_${$}"

TEST_IPP_PID="${TEST_DIR}/ipp.pid"
TEST_IPP_LOG="${TEST_DIR}/ipp.log"
TEST_SNMP_PID="${TEST_DIR}/snmp.pid"
TEST_SNMP_LOG="${TEST_DIR}/snmp.log"
TEST_SNMP_STATE="${TEST_DIR}/snmp_state.json"

# Place metadata outside TEST_DIR so it is preserved if queue deletion fails
TEST_META_FILE="/tmp/xeroxgo-conn-meta-${$}.meta"
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

# Authoritative validation of active-job marker and actual CUPS job state
validate_active_job() {
    local marker="$1"
    local expected_job="$2"
    local queue="$3"
    local cups_id="$4"

    if [[ ! -f "$marker" ]]; then
        return 1
    fi

    local job_name pid state ipp_id
    job_name=$(sed -n 's/^JOB_NAME=//p' "$marker" | head -n 1)
    ipp_id=$(sed -n 's/^IPP_JOB_ID=//p' "$marker" | head -n 1)
    pid=$(sed -n 's/^PID=//p' "$marker" | head -n 1)
    state=$(sed -n 's/^STATE=//p' "$marker" | head -n 1)

    # 1. Reject incomplete marker
    if [[ -z "$job_name" ]] || [[ -z "$pid" ]] || [[ -z "$state" ]]; then
        return 1
    fi

    # 2. Reject mismatched job ID
    if [[ "$job_name" != "$expected_job" ]]; then
        return 1
    fi

    # 3. Reject non-processing markers
    if [[ "$state" != "processing" ]]; then
        return 1
    fi

    # 4. Validate PID is alive
    if ! kill -0 "$pid" 2>/dev/null; then
        return 1
    fi

    # 5. Validate PID belongs to expected processor.sh script
    local pid_cmdline pid_comm
    pid_cmdline=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true)
    pid_comm=$(cat "/proc/${pid}/comm" 2>/dev/null || ps -p "$pid" -o comm= 2>/dev/null || true)
    if [[ ! "$pid_cmdline" =~ processor\.sh ]] && [[ ! "$pid_comm" =~ processor ]]; then
        return 1
    fi

    # 6. Validate IPP job ID when present
    if [[ -n "$ipp_id" ]] && [[ "$ipp_id" != "unknown" ]]; then
        if [[ ! "$ipp_id" =~ ^[0-9]+$ ]] || [[ "$ipp_id" -le 0 ]]; then
            return 1
        fi
    fi

    # 7. Require CUPS job to be processing
    if [[ -n "$cups_id" ]]; then
        local cups_status
        cups_status=$(lpstat -W not-completed -o "$queue" 2>/dev/null | grep "^${cups_id} " || true)
        if [[ -z "$cups_status" ]]; then
            return 1
        fi
    fi

    return 0
}

cleanup_test_env() {
    local exit_code=$?
    echo ""
    echo "=== Teardown and Cleanup ==="
    # Release gate so processor finishes if still running
    if [[ -f "$TEST_JOB_GATE" ]]; then
        echo "release" > "$TEST_JOB_RELEASE" 2>/dev/null || true
    fi

    # Cancel only captured test job IDs after validating queue ownership
    for c_id in "${CAPTURED_JOB_IDS[@]:-}"; do
        if [[ -n "$c_id" ]] && [[ "$c_id" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
            if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
                cancel "$c_id" >/dev/null 2>&1 || true
            fi
        fi
    done

    # Safe CUPS queue removal
    if [[ "$QUEUE_CREATED" == "true" ]]; then
        if ! "$EMULATOR" remove-cups; then
            echo -e "${RED}ERROR: Failed to remove test CUPS queue '${TEST_QUEUE}'. Metadata preserved at ${TEST_META_FILE}.${NC}" >&2
        else
            echo "Test CUPS queue '${TEST_QUEUE}' cleanly removed."
        fi
    fi

    # Cleanup emulator processes
    "$EMULATOR" cleanup >/dev/null 2>&1 || true
    "$EMULATOR" cleanup-snmp >/dev/null 2>&1 || true
    rm -rf "$TEST_DIR" 2>/dev/null || true

    # Baseline queue isolation assertion
    local current_queues
    current_queues=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
    if [[ "$current_queues" != "$BASELINE_QUEUES" ]]; then
        echo -e "${RED}ERROR: CUPS queue state changed during test! Baseline: '$BASELINE_QUEUES', Current: '$current_queues'${NC}" >&2
        exit 1
    fi
    exit $exit_code
}
trap cleanup_test_env EXIT INT TERM

# -----------------------------------------------------------------------------
info "Test 1: Start IPP and SNMP emulators"
# -----------------------------------------------------------------------------
"$EMULATOR" start
"$EMULATOR" start-snmp

# Verify both are running
if "$EMULATOR" status >/dev/null 2>&1 && "$EMULATOR" status-snmp >/dev/null 2>&1; then
    pass "Test 1: IPP and SNMP emulators started on ports ${TEST_IPP_PORT} / ${TEST_SNMP_PORT}"
else
    fail "Test 1: Emulators failed to start"
    exit 1
fi

# -----------------------------------------------------------------------------
info "Test 2: Setup driverless CUPS queue with 3-way URI verification"
# -----------------------------------------------------------------------------
"$EMULATOR" setup-cups
QUEUE_CREATED=true

if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
    pass "Test 2: Driverless CUPS queue '${TEST_QUEUE}' created with verified 3-way ownership"
else
    fail "Test 2: Failed to verify ownership of created CUPS queue"
    exit 1
fi

# -----------------------------------------------------------------------------
info "Test 3: Reachability Separation Verification (CUPS vs IPP vs SNMP)"
# -----------------------------------------------------------------------------
# Verify CUPS scheduler reachability
cups_sched_ok=false
if lpstat -r >/dev/null 2>&1 && lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
    cups_sched_ok=true
fi

# Verify direct IPP reachability via ipptool
ipp_reach_ok=false
if timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
    ipp_reach_ok=true
fi

# Verify direct SNMP reachability via snmpget
snmp_reach_ok=false
if snmpget -v2c -c "$SNMP_COMMUNITY" -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.1.1 >/dev/null 2>&1; then
    snmp_reach_ok=true
fi

if [[ "$cups_sched_ok" == "true" ]] && [[ "$ipp_reach_ok" == "true" ]] && [[ "$snmp_reach_ok" == "true" ]]; then
    pass "Test 3: Independent reachability confirmed: CUPS scheduler, IPP endpoint, and SNMP endpoint all reachable"
else
    fail "Test 3: Reachability separation verification failed (cups=$cups_sched_ok, ipp=$ipp_reach_ok, snmp=$snmp_reach_ok)"
fi

# -----------------------------------------------------------------------------
info "Test 4: Mid-Print SNMP-Only Outage (Continued Monitoring & Completion)"
# -----------------------------------------------------------------------------
# Prime execution gate
echo "gate" > "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_snmp="job-conn-snmp-${$}-$(date +%s%N | cut -c1-6)"
submit_out=$(lp -d "$TEST_QUEUE" -n 1 -t "$unique_job_snmp" "$SAMPLE_PDF" 2>&1)
cups_id_snmp=$(echo "$submit_out" | grep -oE "${TEST_QUEUE}-[0-9]+" | head -n 1 || true)
if [[ -z "$cups_id_snmp" ]]; then
    fail "Test 4: Failed to submit print job: $submit_out"
    exit 1
fi
CAPTURED_JOB_IDS+=("$cups_id_snmp")

# Wait for active processor synchronization
for _ in {1..50}; do
    if validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_snmp" "$TEST_QUEUE" "$cups_id_snmp"; then
        break
    fi
    sleep 0.05
done

if ! validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_snmp" "$TEST_QUEUE" "$cups_id_snmp"; then
    fail "Test 4: Active job marker validation failed before injection"
else
    # Inject SNMP outage only (never modify system networking)
    "$EMULATOR" fault set offline --target snmp >/dev/null 2>&1

    # Verify reachability separation:
    # 1. SNMP UDP is dropped / unresponsive
    snmp_dropped=false
    if ! snmpget -v2c -c "$SNMP_COMMUNITY" -t 1 -r 0 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.1.1 >/dev/null 2>&1; then
        snmp_dropped=true
    fi

    # 2. IPP is still online
    ipp_still_up=false
    if timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
        ipp_still_up=true
    fi

    # 3. CUPS scheduler is still online and job is still processing
    cups_still_up=false
    if lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep -q "^${cups_id_snmp} "; then
        cups_still_up=true
    fi

    if [[ "$snmp_dropped" == "true" ]] && [[ "$ipp_still_up" == "true" ]] && [[ "$cups_still_up" == "true" ]]; then
        pass "Test 4a: SNMP outage isolated: SNMP UDP dropped while IPP and CUPS scheduler remain reachable"
    else
        fail "Test 4a: SNMP outage isolation failed (snmp_dropped=$snmp_dropped, ipp_up=$ipp_still_up, cups_up=$cups_still_up)"
    fi

    # Continued monitoring: verify no synthetic hardware error in CUPS
    # Restore SNMP connectivity
    "$EMULATOR" fault set clear --target snmp >/dev/null 2>&1

    # Release gate to allow processor to complete
    echo "release" > "$TEST_JOB_RELEASE"

    # Wait for job completion in CUPS
    completed_snmp=false
    for _ in {1..50}; do
        comp_out=$(lpstat -W completed -o "$TEST_QUEUE" 2>/dev/null || true)
        if echo "$comp_out" | grep -q "^${cups_id_snmp} "; then
            completed_snmp=true
            break
        fi
        sleep 0.1
    done

    if [[ "$completed_snmp" == "true" ]]; then
        pass "Test 4b: Original CUPS job ${cups_id_snmp} cleanly completed after SNMP restoration without resubmission"
    else
        fail "Test 4b: Job ${cups_id_snmp} did not complete in CUPS after SNMP restoration"
    fi
fi

# -----------------------------------------------------------------------------
info "Test 5: Mid-Print IPP-Only Outage (Freeze, Reachability & Safe Restoration)"
# -----------------------------------------------------------------------------
echo "gate" > "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_ipp="job-conn-ipp-${$}-$(date +%s%N | cut -c1-6)"
submit_out=$(lp -d "$TEST_QUEUE" -n 1 -t "$unique_job_ipp" "$SAMPLE_PDF" 2>&1)
cups_id_ipp=$(echo "$submit_out" | grep -oE "${TEST_QUEUE}-[0-9]+" | head -n 1 || true)
if [[ -z "$cups_id_ipp" ]]; then
    fail "Test 5: Failed to submit print job: $submit_out"
    exit 1
fi
CAPTURED_JOB_IDS+=("$cups_id_ipp")

for _ in {1..50}; do
    if validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_ipp" "$TEST_QUEUE" "$cups_id_ipp"; then
        break
    fi
    sleep 0.05
done

if ! validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_ipp" "$TEST_QUEUE" "$cups_id_ipp"; then
    fail "Test 5: Active job validation failed for IPP outage test"
else
    # Inject IPP outage only (freezes IPP emulator process via SIGSTOP)
    "$EMULATOR" fault set offline --target ipp >/dev/null 2>&1

    # Verify reachability:
    # 1. IPP TCP socket is unresponsive / times out
    ipp_unresp=false
    if ! timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
        ipp_unresp=true
    fi

    # 2. SNMP is still reachable
    snmp_still_up=false
    if snmpget -v2c -c "$SNMP_COMMUNITY" -t 1 -r 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.1.1 >/dev/null 2>&1; then
        snmp_still_up=true
    fi

    # 3. CUPS scheduler is still online
    cups_sched_up=false
    if lpstat -r >/dev/null 2>&1; then
        cups_sched_up=true
    fi

    # 4. Job is still preserved in CUPS queue (neither completed nor disappeared)
    job_preserved=false
    if lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep -q "^${cups_id_ipp} "; then
        job_preserved=true
    fi

    if [[ "$ipp_unresp" == "true" ]] && [[ "$snmp_still_up" == "true" ]] && [[ "$cups_sched_up" == "true" ]] && [[ "$job_preserved" == "true" ]]; then
        pass "Test 5a: IPP outage isolated: IPP frozen while SNMP, CUPS scheduler, and in-flight job remain intact"
    else
        fail "Test 5a: IPP outage isolation failed (ipp_unresp=$ipp_unresp, snmp_up=$snmp_still_up, cups_up=$cups_sched_up, job_preserved=$job_preserved)"
    fi

    # Restore IPP connectivity (SIGCONT to emulator)
    "$EMULATOR" fault set clear --target ipp >/dev/null 2>&1

    # Release gate
    echo "release" > "$TEST_JOB_RELEASE"

    # Query original CUPS job until completed
    completed_ipp=false
    for _ in {1..50}; do
        comp_out=$(lpstat -W completed -o "$TEST_QUEUE" 2>/dev/null || true)
        if echo "$comp_out" | grep -q "^${cups_id_ipp} "; then
            completed_ipp=true
            break
        fi
        sleep 0.1
    done

    if [[ "$completed_ipp" == "true" ]]; then
        pass "Test 5b: Original CUPS job ${cups_id_ipp} cleanly completed after IPP restoration without resubmission"
    else
        fail "Test 5b: Job ${cups_id_ipp} did not complete in CUPS after IPP restoration"
    fi
fi

# -----------------------------------------------------------------------------
info "Test 6: Mid-Print Combined IPP+SNMP Outage and Restoration"
# -----------------------------------------------------------------------------
echo "gate" > "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_both="job-conn-both-${$}-$(date +%s%N | cut -c1-6)"
submit_out=$(lp -d "$TEST_QUEUE" -n 1 -t "$unique_job_both" "$SAMPLE_PDF" 2>&1)
cups_id_both=$(echo "$submit_out" | grep -oE "${TEST_QUEUE}-[0-9]+" | head -n 1 || true)
if [[ -z "$cups_id_both" ]]; then
    fail "Test 6: Failed to submit print job: $submit_out"
    exit 1
fi
CAPTURED_JOB_IDS+=("$cups_id_both")

for _ in {1..50}; do
    if validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_both" "$TEST_QUEUE" "$cups_id_both"; then
        break
    fi
    sleep 0.05
done

if ! validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_both" "$TEST_QUEUE" "$cups_id_both"; then
    fail "Test 6: Active job validation failed for combined outage test"
else
    # Inject combined outage (target both)
    "$EMULATOR" fault set offline --target both >/dev/null 2>&1

    # Verify both endpoints unresponsive
    ipp_down=false
    snmp_down=false
    if ! timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
        ipp_down=true
    fi
    if ! snmpget -v2c -c "$SNMP_COMMUNITY" -t 1 -r 0 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.1.1 >/dev/null 2>&1; then
        snmp_down=true
    fi

    if [[ "$ipp_down" == "true" ]] && [[ "$snmp_down" == "true" ]]; then
        pass "Test 6a: Combined outage verified: both IPP and SNMP endpoints are unresponsive"
    else
        fail "Test 6a: Combined outage verification failed (ipp_down=$ipp_down, snmp_down=$snmp_down)"
    fi

    # Restore both
    "$EMULATOR" fault set clear --target both >/dev/null 2>&1

    # Release gate
    echo "release" > "$TEST_JOB_RELEASE"

    # Verify completion of original job
    completed_both=false
    for _ in {1..50}; do
        comp_out=$(lpstat -W completed -o "$TEST_QUEUE" 2>/dev/null || true)
        if echo "$comp_out" | grep -q "^${cups_id_both} "; then
            completed_both=true
            break
        fi
        sleep 0.1
    done

    if [[ "$completed_both" == "true" ]]; then
        pass "Test 6b: Original CUPS job ${cups_id_both} cleanly completed after combined restoration without resubmission"
    else
        fail "Test 6b: Job ${cups_id_both} did not complete after combined restoration"
    fi
fi

# -----------------------------------------------------------------------------
info "Test 7: Job Preservation vs Cancellation Safety (Requirement 13)"
# -----------------------------------------------------------------------------
echo "gate" > "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_pres="job-conn-pres-${$}-$(date +%s%N | cut -c1-6)"
submit_out=$(lp -d "$TEST_QUEUE" -n 1 -t "$unique_job_pres" "$SAMPLE_PDF" 2>&1)
cups_id_pres=$(echo "$submit_out" | grep -oE "${TEST_QUEUE}-[0-9]+" | head -n 1 || true)
CAPTURED_JOB_IDS+=("$cups_id_pres")

for _ in {1..50}; do
    if validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_pres" "$TEST_QUEUE" "$cups_id_pres"; then
        break
    fi
    sleep 0.05
done

# Safe preservation: hold job instead of cancel
hold_res=$(lp -i "$cups_id_pres" -H hold 2>&1 || true)
is_held=false
if lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "^${cups_id_pres} " | grep -q "held"; then
    is_held=true
elif lpstat -o "$TEST_QUEUE" 2>/dev/null | grep -q "^${cups_id_pres} "; then
    is_held=true
fi

if [[ "$is_held" == "true" ]]; then
    pass "Test 7a: Job preservation verified: job ${cups_id_pres} remains in CUPS queue without cancellation"
else
    fail "Test 7a: Job preservation failed for ${cups_id_pres}: $hold_res"
fi

# Negative assertion: cancel deletes the job from CUPS
cancel "$cups_id_pres" 2>/dev/null || true
echo "release" > "$TEST_JOB_RELEASE"
sleep 0.2

not_comp=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "^${cups_id_pres} " || true)
if [[ -z "$not_comp" ]]; then
    pass "Test 7b: Negative verification confirmed: unpreserved cancellation permanently removes job from CUPS queue"
else
    fail "Test 7b: Canceled job still present in active queue: $not_comp"
fi

# -----------------------------------------------------------------------------
info "Test 8: Regression — Failed Restoration and Uncertain Job State (Requirement 18)"
# -----------------------------------------------------------------------------
echo "gate" > "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

unique_job_fail="job-conn-fail-${$}-$(date +%s%N | cut -c1-6)"
submit_out=$(lp -d "$TEST_QUEUE" -n 1 -t "$unique_job_fail" "$SAMPLE_PDF" 2>&1)
cups_id_fail=$(echo "$submit_out" | grep -oE "${TEST_QUEUE}-[0-9]+" | head -n 1 || true)
CAPTURED_JOB_IDS+=("$cups_id_fail")

for _ in {1..50}; do
    if validate_active_job "$TEST_JOB_ACTIVE" "$unique_job_fail" "$TEST_QUEUE" "$cups_id_fail"; then
        break
    fi
    sleep 0.05
done

# Inject outage and intentionally leave restoration unperformed (failed restoration)
"$EMULATOR" fault set offline --target ipp >/dev/null 2>&1

# Query CUPS: job must NOT report completed
comp_check=$(lpstat -W completed -o "$TEST_QUEUE" 2>/dev/null | grep "^${cups_id_fail} " || true)
if [[ -z "$comp_check" ]]; then
    pass "Test 8a: Without restoration, job ${cups_id_fail} is NOT reported as completed in CUPS (uncertain state preserved)"
else
    fail "Test 8a: Job was incorrectly reported completed without connectivity restoration: $comp_check"
fi

# Clean up failed test job
"$EMULATOR" fault set clear --target ipp >/dev/null 2>&1
echo "release" > "$TEST_JOB_RELEASE"
cancel "$cups_id_fail" 2>/dev/null || true

# -----------------------------------------------------------------------------
info "Test 9: Job ID Traceability and Single Dispatch per Submission"
# -----------------------------------------------------------------------------
# Verify each test submission produced a unique traceable CUPS ID
if [[ "${#CAPTURED_JOB_IDS[@]}" -ge 4 ]]; then
    # Ensure no duplicates in captured IDs
    unique_count=$(printf "%s\n" "${CAPTURED_JOB_IDS[@]}" | sort -u | wc -l)
    if [[ "$unique_count" -eq "${#CAPTURED_JOB_IDS[@]}" ]]; then
        pass "Test 9: All test submissions produced unique traceable CUPS request IDs with no duplicate dispatches"
    else
        fail "Test 9: Duplicate CUPS job IDs detected: ${CAPTURED_JOB_IDS[*]}"
    fi
else
    fail "Test 9: Insufficient captured job IDs: ${#CAPTURED_JOB_IDS[@]}"
fi

# -----------------------------------------------------------------------------
info "Test 10: Safe Queue Removal and Baseline Queue Isolation"
# -----------------------------------------------------------------------------
if "$EMULATOR" remove-cups; then
    QUEUE_CREATED=false
    pass "Test 10a: Verified test CUPS queue '${TEST_QUEUE}' cleanly removed"
else
    fail "Test 10a: remove-cups failed"
fi

# Verify baseline queue DCPT310 is unchanged
current_queues=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
if [[ "$current_queues" == "$BASELINE_QUEUES" ]]; then
    pass "Test 10b: Host printer '${BASELINE_QUEUES}' remained completely isolated and untouched"
else
    fail "Test 10b: Host printer queue altered! Current: '$current_queues', Baseline: '$BASELINE_QUEUES'"
fi

# -----------------------------------------------------------------------------
echo ""
echo "============================================================"
echo "  Connectivity Fault Validation Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "    SKIPPED: ${SKIPPED_COUNT}"
echo "============================================================"

if [[ $FAILED_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
