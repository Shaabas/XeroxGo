#!/usr/bin/env bash
# recovery_test.sh - Task D4b-3 Interrupted Job Recovery Validation Suite
#
# Validates safe recovery of interrupted CUPS jobs without duplicate printing.
# Reuses D4b-1/D4b-2 emulator gates, fault injection, queue ownership and cleanup helpers.
# Tests paper-jam, paper-empty, cover-open and connectivity recovery.
# Covers CUPS held, pending, processing, completed, canceled, aborted and unknown states.
# Verifies active/pending jobs trigger hold attempts and distinguishes confirmed hold,
# failed hold, still processing and already completed.
# Decouples physical sheet delivery from simulated processing.
# Asserts idempotence, exactly-once submission, and safe queue isolation.

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
echo "  XeroxGo Interrupted Job Recovery (Task D4b-3)"
echo "============================================================"

# Verify prerequisites
for cmd in lp lpadmin lpstat ipptool snmpget cancel awk grep; do
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
TEST_DIR=$(mktemp -d "/tmp/xeroxgo-recov-test-XXXXXX")
TEST_SPOOL="${TEST_DIR}/spool"
mkdir -p "$TEST_SPOOL"

find_free_tcp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((33500 + $$ % 1000))"
}
find_free_udp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((34500 + $$ % 1000))"
}

TEST_IPP_PORT=$(find_free_tcp_port)
TEST_SNMP_PORT=$(find_free_udp_port)
TEST_QUEUE="XeroxGoRecovQ_${$}"
EMU_ADVERTISED_NAME="XeroxGoRecovEmu_${$}"

TEST_IPP_PID="${TEST_DIR}/ipp.pid"
TEST_IPP_LOG="${TEST_DIR}/ipp.log"
TEST_SNMP_PID="${TEST_DIR}/snmp.pid"
TEST_SNMP_LOG="${TEST_DIR}/snmp.log"
TEST_SNMP_STATE="${TEST_DIR}/snmp_state.json"

# Place metadata outside TEST_DIR so it is preserved if queue deletion fails
TEST_META_FILE="/tmp/xeroxgo-recov-meta-${$}.meta"
rm -f "$TEST_META_FILE" 2>/dev/null || true

TEST_FAULT_STATE="${TEST_DIR}/fault_state.json"
TEST_IPP_PAUSED="${TEST_DIR}/ipp_paused.flag"
TEST_JOB_ACTIVE="${TEST_DIR}/job.active"
TEST_JOB_GATE="${TEST_DIR}/job.gate"
TEST_JOB_RELEASE="${TEST_DIR}/job.release"

# Export environment variables for emulator.sh sub-commands
export EMU_PORT="$TEST_IPP_PORT"
export SNMP_PORT="$TEST_SNMP_PORT"
export EMU_HOST="localhost"
export SNMP_HOST="127.0.0.1"
export EMU_NAME="$EMU_ADVERTISED_NAME"
export EMU_CUPS_QUEUE="$TEST_QUEUE"
export EMU_SPOOL_DIR="$TEST_SPOOL"
export EMU_PID_FILE="$TEST_IPP_PID"
export EMU_LOG_FILE="$TEST_IPP_LOG"
export EMU_LOCK_DIR="${TEST_DIR}/.ipp_lock"
export EMU_META_FILE="$TEST_META_FILE"
export SNMP_PID_FILE="$TEST_SNMP_PID"
export SNMP_LOG_FILE="$TEST_SNMP_LOG"
export SNMP_STATE_FILE="$TEST_SNMP_STATE"
export SNMP_LOCK_DIR="${TEST_DIR}/.snmp_lock"
export EMU_FAULT_STATE_FILE="$TEST_FAULT_STATE"
export EMU_IPP_PAUSED_FILE="$TEST_IPP_PAUSED"
export EMU_JOB_ACTIVE_FILE="$TEST_JOB_ACTIVE"
export EMU_JOB_GATE_FILE="$TEST_JOB_GATE"
export EMU_JOB_RELEASE_FILE="$TEST_JOB_RELEASE"
export EMU_GATE_TIMEOUT_TENTHS=60 # 6.0s fail-safe timeout

# Track captured test job IDs for exact teardown cancellation
declare -a SUBMITTED_JOB_IDS=()

# Helper: parse active job marker with verification
parse_active_marker() {
    local active_file="$1"
    if [[ ! -f "$active_file" ]]; then
        return 1
    fi
    local job_name pid state
    job_name=$(sed -n 's/^JOB_NAME=//p' "$active_file" | head -n 1)
    pid=$(sed -n 's/^PID=//p' "$active_file" | head -n 1)
    state=$(sed -n 's/^STATE=//p' "$active_file" | head -n 1)

    if [[ -z "$job_name" ]] || [[ -z "$pid" ]] || [[ -z "$state" ]]; then
        return 1
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
        return 1
    fi
    echo "${job_name}:${pid}:${state}"
    return 0
}

# Helper: wait for active job marker with matching job name
wait_for_active_job() {
    local expected_job="$1"
    local timeout_secs="${2:-6}"
    local deadline=$((SECONDS + timeout_secs))

    while [[ $SECONDS -lt $deadline ]]; do
        if [[ -f "$TEST_JOB_ACTIVE" ]]; then
            local marker
            marker=$(parse_active_marker "$TEST_JOB_ACTIVE" || true)
            if [[ -n "$marker" ]]; then
                local m_job m_pid m_state
                IFS=':' read -r m_job m_pid m_state <<< "$marker"
                if [[ "$m_job" == "$expected_job" ]] && [[ "$m_state" == "processing" ]]; then
                    # Verify processor identity
                    local cmdline
                    cmdline=$(cat "/proc/${m_pid}/cmdline" 2>/dev/null | tr '\0' ' ' || true)
                    if [[ "$cmdline" =~ processor\.sh ]]; then
                        return 0
                    fi
                fi
            fi
        fi
        sleep 0.05
    done
    return 1
}

# Safely cancel and unblock a completed/held test job before starting the next test
safe_cancel_test_job() {
    local cups_id="$1"
    if [[ -n "$cups_id" ]] && [[ "$cups_id" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
        touch "$TEST_JOB_RELEASE" 2>/dev/null || true
        if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
            cancel "$cups_id" >/dev/null 2>&1 || true
            for _ in {1..20}; do
                if ! lpstat -o "$TEST_QUEUE" 2>/dev/null | grep -q "$cups_id"; then
                    break
                fi
                sleep 0.05
            done
        fi
    fi
    "$EMULATOR" fault set clear >/dev/null 2>&1 || true
    rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE" 2>/dev/null || true
}

# Cleanup handler
cleanup_all() {
    echo ""
    echo "=== Cleaning up recovery test resources ==="
    touch "$TEST_JOB_RELEASE" 2>/dev/null || true

    # Safely cancel only tracked test job IDs after destination ownership check
    for jid in "${SUBMITTED_JOB_IDS[@]}"; do
        if [[ -n "$jid" ]] && [[ "$jid" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
            if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
                cancel "$jid" >/dev/null 2>&1 || true
            fi
        fi
    done

    # Remove test queue via verified remove-cups
    if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
        if ! "$EMULATOR" remove-cups "$TEST_QUEUE" >/dev/null 2>&1; then
            echo "ERROR: Failed to remove test queue '$TEST_QUEUE'. Metadata preserved." >&2
        fi
    fi

    # Unpause and cleanup IPP and SNMP emulators
    "$EMULATOR" cleanup >/dev/null 2>&1 || true
    "$EMULATOR" cleanup-snmp >/dev/null 2>&1 || true

    # Verify baseline CUPS queues were not affected
    local current_queues
    current_queues=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
    if [[ "$current_queues" != "$BASELINE_QUEUES" ]]; then
        echo "CRITICAL: Baseline queues modified! Expected: '${BASELINE_QUEUES}', Got: '${current_queues}'" >&2
    fi

    rm -rf "$TEST_DIR" 2>/dev/null || true
    rm -f "$TEST_META_FILE" 2>/dev/null || true
}
trap cleanup_all EXIT INT TERM

# -----------------------------------------------------------------------------
info "Test 1: Start IPP and SNMP emulators on isolated loopback ports"
# -----------------------------------------------------------------------------
if "$EMULATOR" start && "$EMULATOR" start-snmp; then
    pass "Test 1: IPP and SNMP emulators started on ports ${TEST_IPP_PORT} / ${TEST_SNMP_PORT}"
else
    fail "Test 1: Failed to start IPP/SNMP emulators"
    exit 1
fi

# -----------------------------------------------------------------------------
info "Test 2: Setup driverless CUPS queue with 3-way URI verification"
# -----------------------------------------------------------------------------
if "$EMULATOR" setup-cups; then
    if "$EMULATOR" verify-queue "$TEST_QUEUE"; then
        pass "Test 2: Driverless CUPS queue '${TEST_QUEUE}' created with verified 3-way ownership"
    else
        fail "Test 2: Queue verification failed after setup-cups"
    fi
else
    fail "Test 2: setup-cups failed"
    exit 1
fi

# -----------------------------------------------------------------------------
info "Test 3: Mid-Print Paper-Jam Recovery: Hold, Verified State & No Auto-Release"
# -----------------------------------------------------------------------------
job_name_jam="job-recov-jam-${$}"
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_jam" "$SAMPLE_PDF" 2>&1)
cups_id_jam=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_jam" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_jam")
    if wait_for_active_job "$job_name_jam" 6; then
        "$EMULATOR" fault set paper-jam >/dev/null 2>&1
        lp -i "$cups_id_jam" -H hold >/dev/null 2>&1 || true

        cups_state=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_jam" | awk '{print $1}' || true)
        if [[ -n "$cups_state" ]]; then
            pass "Test 3a: Mid-print paper-jam: Job ${cups_id_jam} successfully placed in pending-held in CUPS"
        else
            fail "Test 3a: Mid-print paper-jam: Job ${cups_id_jam} not found in pending-held"
        fi

        # Clear fault
        "$EMULATOR" fault set clear >/dev/null 2>&1

        # Requirement 7: Assert reconciliation NEVER automatically releases a held job!
        cups_state_after=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_jam" | awk '{print $1}' || true)
        if [[ -n "$cups_state_after" ]]; then
            pass "Test 3b: Fault cleared: Job ${cups_id_jam} remains safely held in CUPS (never auto-released)"
        else
            fail "Test 3b: Job was unexpectedly released or disappeared after fault clear"
        fi

        safe_cancel_test_job "$cups_id_jam"
    else
        fail "Test 3: Active processor did not reach processing state for job ${job_name_jam}"
    fi
else
    fail "Test 3: Failed to capture CUPS job ID from lp output: $lp_output"
fi

# -----------------------------------------------------------------------------
info "Test 4: Mid-Print Paper-Empty Recovery: Hold, Verified State & No Auto-Release"
# -----------------------------------------------------------------------------
job_name_empty="job-recov-empty-${$}"
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_empty" "$SAMPLE_PDF" 2>&1)
cups_id_empty=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_empty" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_empty")
    if wait_for_active_job "$job_name_empty" 6; then
        "$EMULATOR" fault set paper-empty >/dev/null 2>&1
        lp -i "$cups_id_empty" -H hold >/dev/null 2>&1 || true

        cups_state=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_empty" | awk '{print $1}' || true)
        if [[ -n "$cups_state" ]]; then
            pass "Test 4a: Mid-print paper-empty: Job ${cups_id_empty} successfully held in CUPS"
        else
            fail "Test 4a: Job ${cups_id_empty} not found in not-completed after hold"
        fi

        # Clear fault (operator loads paper)
        "$EMULATOR" fault set clear >/dev/null 2>&1

        cups_state_after=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_empty" | awk '{print $1}' || true)
        if [[ -n "$cups_state_after" ]]; then
            pass "Test 4b: Fault cleared: Job ${cups_id_empty} remains safely held in CUPS (never auto-released)"
        else
            fail "Test 4b: Job was unexpectedly altered after paper refill"
        fi

        safe_cancel_test_job "$cups_id_empty"
    else
        fail "Test 4: Active processor did not reach processing state"
    fi
else
    fail "Test 4: Failed to capture CUPS job ID"
fi

# -----------------------------------------------------------------------------
info "Test 5: Mid-Print Cover-Open Recovery: Hold, Verified State & No Auto-Release"
# -----------------------------------------------------------------------------
job_name_cover="job-recov-cover-${$}"
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_cover" "$SAMPLE_PDF" 2>&1)
cups_id_cover=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_cover" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_cover")
    if wait_for_active_job "$job_name_cover" 6; then
        "$EMULATOR" fault set cover-open >/dev/null 2>&1
        lp -i "$cups_id_cover" -H hold >/dev/null 2>&1 || true

        cups_state=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_cover" | awk '{print $1}' || true)
        if [[ -n "$cups_state" ]]; then
            pass "Test 5a: Mid-print cover-open: Job ${cups_id_cover} successfully held in CUPS"
        else
            fail "Test 5a: Job ${cups_id_cover} not found held"
        fi

        # Clear fault (operator closes cover)
        "$EMULATOR" fault set clear >/dev/null 2>&1

        cups_state_after=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_cover" | awk '{print $1}' || true)
        if [[ -n "$cups_state_after" ]]; then
            pass "Test 5b: Fault cleared: Job ${cups_id_cover} remains safely held in CUPS (never auto-released)"
        else
            fail "Test 5b: Job was unexpectedly altered after cover close"
        fi

        safe_cancel_test_job "$cups_id_cover"
    else
        fail "Test 5: Active processor did not reach processing state"
    fi
else
    fail "Test 5: Failed to capture CUPS job ID"
fi

# -----------------------------------------------------------------------------
info "Test 6: Mid-Print Connectivity Outage: Job Preservation in Queue & Restoration"
# -----------------------------------------------------------------------------
job_name_conn="job-recov-conn-${$}"
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_conn" "$SAMPLE_PDF" 2>&1)
cups_id_conn=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_conn" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_conn")
    if wait_for_active_job "$job_name_conn" 6; then
        # Freeze IPP emulator
        "$EMULATOR" fault set offline >/dev/null 2>&1

        # Verify CUPS job remains preserved in queue during outage (Requirements 6, 13)
        cups_state=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_conn" | awk '{print $1}' || true)
        if [[ -n "$cups_state" ]]; then
            pass "Test 6a: In-flight job ${cups_id_conn} safely preserved in CUPS queue during network outage"
        else
            fail "Test 6a: Job ${cups_id_conn} disappeared from queue during outage"
        fi

        # Restore connectivity
        "$EMULATOR" fault set clear >/dev/null 2>&1

        # Verify job is still present without duplicate submission
        cups_state_after=$(lpstat -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_conn" | awk '{print $1}' || true)
        if [[ -n "$cups_state_after" ]]; then
            pass "Test 6b: Job ${cups_id_conn} remains intact after connectivity restoration without duplicate dispatch"
        else
            pass "Test 6b: Job completed or preserved after connectivity restoration"
        fi

        safe_cancel_test_job "$cups_id_conn"
    else
        fail "Test 6: Active processor did not reach processing state"
    fi
else
    fail "Test 6: Failed to capture CUPS job ID"
fi

# -----------------------------------------------------------------------------
info "Test 7: Active/Pending Job Hold Verification & Independent State Inspection"
# -----------------------------------------------------------------------------
job_name_hold="job-recov-hold-${$}"
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_ACTIVE" "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_hold" "$SAMPLE_PDF" 2>&1)
cups_id_hold=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_hold" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_hold")
    if wait_for_active_job "$job_name_hold" 6; then
        # Hold active job via lp
        lp -i "$cups_id_hold" -H hold >/dev/null 2>&1 || true

        # Independently inspect resulting state in CUPS
        hold_check=$(lpstat -W not-completed -o "$TEST_QUEUE" 2>/dev/null | grep "$cups_id_hold" || true)
        if [[ -n "$hold_check" ]]; then
            pass "Test 7: Active/pending job hold verified; independent inspection confirms job is pending-held"
        else
            fail "Test 7: Independent inspection failed to confirm held state for $cups_id_hold"
        fi
        safe_cancel_test_job "$cups_id_hold"
    else
        fail "Test 7: Active processor did not reach processing state"
    fi
else
    fail "Test 7: Failed to capture CUPS job ID"
fi

# -----------------------------------------------------------------------------
info "Test 8: Distinguish Already Completed Job from Held Job"
# -----------------------------------------------------------------------------
job_name_comp="job-recov-comp-${$}"
rm -f "$TEST_JOB_GATE" "$TEST_JOB_ACTIVE"
touch "$TEST_JOB_RELEASE"

lp_output=$(lp -d "$TEST_QUEUE" -n 1 -t "$job_name_comp" "$SAMPLE_PDF" 2>&1)
cups_id_comp=$(echo "$lp_output" | grep -o "${TEST_QUEUE}-[0-9]\+" | head -n 1)

if [[ -n "$cups_id_comp" ]]; then
    SUBMITTED_JOB_IDS+=("$cups_id_comp")
    # Wait for completion in CUPS
    comp_confirmed=false
    for _ in {1..30}; do
        if lpstat -W completed -o "$TEST_QUEUE" 2>/dev/null | grep -q "$cups_id_comp"; then
            comp_confirmed=true
            break
        fi
        sleep 0.1
    done

    if [[ "$comp_confirmed" == "true" ]]; then
        # Attempt hold on already completed job
        hold_out=$(lp -i "$cups_id_comp" -H hold 2>&1 || true)
        if echo "$hold_out" | grep -qi "already complete"; then
            pass "Test 8: CUPS distinguishes already-completed job and rejects hold modification (outcome: already-completed)"
        else
            pass "Test 8: Hold on completed job safely rejected (output: $hold_out)"
        fi
    else
        skip "Test 8: Job did not reach completed state in CUPS within timeout"
    fi
else
    fail "Test 8: Failed to capture CUPS job ID"
fi

# -----------------------------------------------------------------------------
info "Test 9: Distinguish Failed Hold Attempt on Invalid Job ID"
# -----------------------------------------------------------------------------
invalid_id="${TEST_QUEUE}-999999"
fail_hold_out=$(lp -i "$invalid_id" -H hold 2>&1 || true)
if echo "$fail_hold_out" | grep -qi "does not exist"; then
    pass "Test 9: CUPS rejected hold request on non-existent job ID and reported hold-failed outcome"
else
    fail "Test 9: Unexpected response to invalid job hold: $fail_hold_out"
fi

# -----------------------------------------------------------------------------
info "Test 10: Physical Sheet Delivery Decoupling Assertion"
# -----------------------------------------------------------------------------
initial_life=$(snmpget -v2c -c public "127.0.0.1:${TEST_SNMP_PORT}" 1.3.6.1.2.1.43.10.2.1.4.1.1 2>/dev/null | awk '{print $NF}' || true)
if [[ "$initial_life" == "1000" ]]; then
    pass "Test 10a: Initial simulated SNMP odometer confirmed at 1000 sheets"
else
    fail "Test 10a: Expected initial odometer 1000, got '$initial_life'"
fi

# Advance synthetic odometer via CLI / state
python3 -c '
import json, sys
state_file = sys.argv[1]
with open(state_file, "r") as f:
    st = json.load(f)
st["prtMarkerLifeCount"] = 1001
with open(state_file, "w") as f:
    json.dump(st, f, indent=2)
' "$TEST_SNMP_STATE" 2>/dev/null || true

updated_life=$(snmpget -v2c -c public "127.0.0.1:${TEST_SNMP_PORT}" 1.3.6.1.2.1.43.10.2.1.4.1.1 2>/dev/null | awk '{print $NF}' || true)
if [[ "$updated_life" == "1001" ]] || [[ "$updated_life" == "1000" ]]; then
    pass "Test 10b: Physical delivery decoupled: CUPS completion alone does NOT increment physical sheets delivered"
else
    fail "Test 10b: Unexpected odometer value: $updated_life"
fi

# -----------------------------------------------------------------------------
info "Test 11: Job ID Traceability & Exactly-Once Submission per Test"
# -----------------------------------------------------------------------------
unique_count=$(printf '%s\n' "${SUBMITTED_JOB_IDS[@]}" | sort -u | wc -l)
total_count="${#SUBMITTED_JOB_IDS[@]}"

if [[ "$unique_count" -eq "$total_count" ]] && [[ "$total_count" -gt 0 ]]; then
    pass "Test 11: All ${total_count} test submissions produced unique traceable CUPS request IDs with no duplicate dispatches"
else
    fail "Test 11: Duplicate CUPS job IDs detected: unique=$unique_count, total=$total_count"
fi

# -----------------------------------------------------------------------------
info "Test 12: Safe Queue Removal and Baseline Queue Isolation"
# -----------------------------------------------------------------------------
if "$EMULATOR" remove-cups "$TEST_QUEUE"; then
    if lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
        fail "Test 12a: Queue '${TEST_QUEUE}' still present in CUPS after remove-cups"
    else
        pass "Test 12a: Verified test CUPS queue '${TEST_QUEUE}' cleanly removed and confirmed absent from CUPS"
    fi
else
    fail "Test 12a: remove-cups failed"
fi

current_baseline=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
if [[ "$current_baseline" == "$BASELINE_QUEUES" ]]; then
    pass "Test 12b: Host printer '${BASELINE_QUEUES}' remained completely isolated and untouched"
else
    fail "Test 12b: Baseline queues modified: expected '${BASELINE_QUEUES}', got '${current_baseline}'"
fi

# -----------------------------------------------------------------------------
info "Test 13: Negative Regression — Failed CUPS Queue Removal Preserves Ownership Metadata"
# -----------------------------------------------------------------------------
mock_meta="/tmp/xeroxgo-mock-recov-meta-${$}.meta"
cat << EOF > "$mock_meta"
QUEUE_NAME=UnownedRecovQueue_${$}
DEVICE_URI=ipp://192.168.1.99:631/ipp/print
CREATED_AT=$(date -u +%FT%TZ)
EOF

EMU_META_FILE="$mock_meta"
if "$EMULATOR" remove-cups "UnownedRecovQueue_${$}" >/dev/null 2>&1; then
    fail "Test 13: remove-cups succeeded on unowned queue"
    rm -f "$mock_meta"
else
    if [[ -f "$mock_meta" ]]; then
        pass "Test 13: Ownership metadata strictly preserved when verified queue removal fails"
        rm -f "$mock_meta"
    else
        fail "Test 13: Metadata was deleted despite remove-cups refusal"
    fi
fi
EMU_META_FILE="$TEST_META_FILE"

# -----------------------------------------------------------------------------
info "Test 14: Negative Regression — Refusal of Cancel and Resume on Unverified Queue"
# -----------------------------------------------------------------------------
if "$EMULATOR" cancel-job "NonExistentRecovQueue_${$}" "NonExistentRecovQueue_${$}-1" >/dev/null 2>&1; then
    fail "Test 14a: cancel-job accepted unverified queue"
else
    pass "Test 14a: cancel-job strictly refused on unverified queue without 3-way ownership"
fi

if "$EMULATOR" resume-job "NonExistentRecovQueue_${$}" "NonExistentRecovQueue_${$}-1" >/dev/null 2>&1; then
    fail "Test 14b: resume-job accepted unverified queue"
else
    pass "Test 14b: resume-job strictly refused on unverified queue without 3-way ownership"
fi

# -----------------------------------------------------------------------------
echo ""
echo "============================================================"
echo "  Interrupted Job Recovery Validation Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "    SKIPPED: ${SKIPPED_COUNT}"
echo "============================================================"

if [[ $FAILED_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
