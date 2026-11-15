#!/usr/bin/env bash
# fault_smoke_test.sh - Smoke and validation suite for Coordinated Fault Injection (Task D3)
#
# Tests all 8 scenarios, independent IPP/SNMP targets, disagreement cases,
# offline/recovery transitions, deterministic odometer behavior, restart persistence,
# and safe cleanup using real ipptool and snmpget queries against localhost.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EMULATOR="${SCRIPT_DIR}/emulator.sh"
FAULT_CLI="${SCRIPT_DIR}/fault.sh"
SAMPLE_PDF="${SCRIPT_DIR}/testdata/sample.pdf"

# Create dedicated isolated test directory
TEST_DIR=$(mktemp -d "/tmp/xeroxgo-fault-test-XXXXXX")
TEST_SPOOL="${TEST_DIR}/spool"
mkdir -p "$TEST_SPOOL"

# Use high, unprivileged dynamic ports
TEST_IPP_PORT=18731
TEST_SNMP_PORT=18161
TEST_COMMUNITY="public"

TEST_IPP_PID="${TEST_DIR}/emulator.pid"
TEST_IPP_LOG="${TEST_DIR}/emulator.log"
TEST_SNMP_PID="${TEST_DIR}/snmp_responder.pid"
TEST_SNMP_LOG="${TEST_DIR}/snmp_responder.log"
TEST_SNMP_STATE="${TEST_DIR}/snmp_state.json"
TEST_FAULT_STATE="${TEST_DIR}/fault_state.json"
TEST_IPP_PAUSED="${TEST_DIR}/ipp_paused.flag"

export EMU_HOST="localhost"
export EMU_PORT="$TEST_IPP_PORT"
export EMU_NAME="XeroxGoFaultEmu"
export EMU_SPOOL_DIR="$TEST_SPOOL"
export EMU_PID_FILE="$TEST_IPP_PID"
export EMU_LOG_FILE="$TEST_IPP_LOG"
export EMU_FAULT_STATE_FILE="$TEST_FAULT_STATE"
export EMU_IPP_PAUSED_FILE="$TEST_IPP_PAUSED"
export EMU_SPEED="0"

export SNMP_HOST="127.0.0.1"
export SNMP_PORT="$TEST_SNMP_PORT"
export SNMP_COMMUNITY="$TEST_COMMUNITY"
export SNMP_PID_FILE="$TEST_SNMP_PID"
export SNMP_LOG_FILE="$TEST_SNMP_LOG"
export SNMP_STATE_FILE="$TEST_SNMP_STATE"

PASSED_COUNT=0
FAILED_COUNT=0

cleanup_test_env() {
    echo ""
    echo "--- Cleaning up fault test environment ---"
    # Ensure abnormal test termination does not leave IPP process suspended by SIGSTOP
    # Never signal a PID without verifying the expected emulator process identity
    if [[ -f "$TEST_IPP_PID" ]]; then
        local pid
        pid=$(cat "$TEST_IPP_PID" 2>/dev/null || true)
        if [[ -n "$pid" && "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
            if [[ -f "/proc/${pid}/comm" ]] && grep -q "ippeveprinter" "/proc/${pid}/comm" 2>/dev/null; then
                kill -CONT "$pid" 2>/dev/null || true
            fi
        fi
    fi

    # Reuse D1/D2 validated stop and cleanup mechanisms
    "$EMULATOR" cleanup >/dev/null 2>&1 || {
        "$EMULATOR" stop >/dev/null 2>&1 || true
        "$EMULATOR" stop-snmp >/dev/null 2>&1 || true
    }
    rm -rf "$TEST_DIR" 2>/dev/null || true
}
trap cleanup_test_env EXIT

assert_test() {
    local name="$1"
    local condition="$2"
    if eval "$condition"; then
        echo "[PASS] ${name}"
        PASSED_COUNT=$((PASSED_COUNT + 1))
    else
        echo "[FAIL] ${name}" >&2
        FAILED_COUNT=$((FAILED_COUNT + 1))
    fi
}

echo "============================================================"
echo "  XeroxGo Coordinated Fault Injection — Smoke Test Suite"
echo "============================================================"
echo "Test temporary dir:  ${TEST_DIR}"
echo "Test IPP port:       ${TEST_IPP_PORT}"
echo "Test SNMP port:      ${TEST_SNMP_PORT}"
echo ""

# Verify prerequisites
command -v ipptool >/dev/null 2>&1 || { echo "ERROR: ipptool not found" >&2; exit 1; }
command -v snmpget >/dev/null 2>&1 || { echo "ERROR: snmpget not found" >&2; exit 1; }

# Helper to query IPP attributes
query_ipp_reasons() {
    ipptool -tv "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test 2>/dev/null | grep "printer-state-reasons" | sed -E 's/.*=[[:space:]]*//' | tr -d ' ' || echo "ERROR"
}

query_ipp_state() {
    ipptool -tv "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test 2>/dev/null | awk '/printer-state \(enum\)/ {print $NF}' || echo "ERROR"
}

# Helper to query SNMP MIBs
query_snmp_status() {
    snmpget -v2c -c "$TEST_COMMUNITY" -r 1 -t 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.1.1 2>/dev/null || echo "TIMEOUT"
}

query_snmp_error() {
    snmpget -v2c -c "$TEST_COMMUNITY" -r 1 -t 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.25.3.5.1.2.1 2>/dev/null || echo "TIMEOUT"
}

query_snmp_counter() {
    snmpget -v2c -c "$TEST_COMMUNITY" -r 1 -t 1 "${SNMP_HOST}:${SNMP_PORT}" 1.3.6.1.2.1.43.10.2.1.4.1.1 2>/dev/null || echo "TIMEOUT"
}

# -------------------------------------------------------------
echo "--- Test 1: Start IPP and SNMP emulators and verify initial healthy agreement ---"
"$EMULATOR" start
"$EMULATOR" start-snmp

assert_test "Test 1: Initial IPP and SNMP startup" '[[ -f "$TEST_IPP_PID" ]] && [[ -f "$TEST_SNMP_PID" ]]'

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP report HEALTHY" && t1_agree=true || t1_agree=false
assert_test "Test 1: Initial coordinated status reports HEALTHY agreement" '$t1_agree'

# -------------------------------------------------------------
echo ""
echo "--- Test 2: Coordinated paper-jam scenario ---"
"$FAULT_CLI" set paper-jam
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_err=$(query_snmp_error)
snmp_st=$(query_snmp_status)

echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP status: ${snmp_st}"
echo "  Live SNMP error:  ${snmp_err}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP report PAPER-JAM" && t2_agree=true || t2_agree=false

assert_test "Test 2: IPP reports media-jam-report" '[[ "$ipp_reasons" =~ media-jam ]]'
assert_test "Test 2: SNMP reports error byte 0 bit 5 (jammed 0x04)" '[[ "$snmp_err" =~ "04 00" || "$snmp_err" =~ "jam" ]]'
assert_test "Test 2: Coordinated status reports PAPER-JAM agreement" '$t2_agree'

# -------------------------------------------------------------
echo ""
echo "--- Test 3: Coordinated cover-open scenario ---"
"$FAULT_CLI" set cover-open
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_err=$(query_snmp_error)

echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP error:  ${snmp_err}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP report COVER-OPEN" && t3_agree=true || t3_agree=false

assert_test "Test 3: IPP reports cover-open-report" '[[ "$ipp_reasons" =~ cover-open ]]'
assert_test "Test 3: SNMP reports doorOpen / cover error (0x08)" '[[ "$snmp_err" =~ "08 00" || "$snmp_err" =~ "doorOpen" ]]'
assert_test "Test 3: Coordinated status reports COVER-OPEN agreement" '$t3_agree'

# -------------------------------------------------------------
echo ""
echo "--- Test 4: Coordinated paper-empty scenario ---"
"$FAULT_CLI" set paper-empty
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_err=$(query_snmp_error)

echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP error:  ${snmp_err}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP report PAPER-EMPTY" && t4_agree=true || t4_agree=false

assert_test "Test 4: IPP reports media-empty-report" '[[ "$ipp_reasons" =~ media-empty ]]'
assert_test "Test 4: SNMP reports noPaper error (0x40)" '[[ "$snmp_err" =~ "40 00" || "$snmp_err" =~ "noPaper" ]]'
assert_test "Test 4: Coordinated status reports PAPER-EMPTY agreement" '$t4_agree'

# -------------------------------------------------------------
echo ""
echo "--- Test 5: Coordinated printing scenario ---"
"$FAULT_CLI" set printing
sleep 0.2

snmp_st=$(query_snmp_status)
echo "  Live SNMP status: ${snmp_st}"

assert_test "Test 5: SNMP reports printing(4) status" '[[ "$snmp_st" =~ printing\(4\) || "$snmp_st" =~ "4" ]]'

# -------------------------------------------------------------
echo ""
echo "--- Test 6: Coordinated stopped scenario ---"
"$FAULT_CLI" set stopped
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_st=$(query_snmp_status)
echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP status: ${snmp_st}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[IPP-STOPPED\]" && t6_stop=true || t6_stop=false
echo "$STATUS_OUT" | grep -q "Limitation note: Standalone ippeveprinter" && t6_note=true || t6_note=false

assert_test "Test 6: IPP reports paused reason" '[[ "$ipp_reasons" =~ paused ]]'
assert_test "Test 6: Standalone limitation note documented truthfully" '$t6_note'

# -------------------------------------------------------------
echo ""
echo "--- Test 7: Coordinated offline and recovery scenario ---"
"$FAULT_CLI" set offline
sleep 0.2

# IPP socket should not respond (timeout after 1 second)
ipp_unresp=false
if ! timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
    ipp_unresp=true
fi

# SNMP query should timeout (UDP packets dropped)
snmp_unresp=false
snmp_res=$(query_snmp_status)
if [[ "$snmp_res" =~ "TIMEOUT" || "$snmp_res" =~ "No Response" ]]; then
    snmp_unresp=true
fi

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP are OFFLINE" && t7_agree=true || t7_agree=false

assert_test "Test 7: IPP TCP socket unresponsive during offline (SIGSTOP)" '$ipp_unresp'
assert_test "Test 7: SNMP UDP requests dropped during offline" '$snmp_unresp'
assert_test "Test 7: Coordinated status reports OFFLINE agreement" '$t7_agree'

echo "Restoring from offline via recovery scenario..."
"$FAULT_CLI" set recovery
sleep 0.5

ipp_rec_reasons=$(query_ipp_reasons)
snmp_rec_st=$(query_snmp_status)
snmp_rec_err=$(query_snmp_error)

echo "  Restored IPP reasons: ${ipp_rec_reasons}"
echo "  Restored SNMP status:  ${snmp_rec_st}"
echo "  Restored SNMP error:   ${snmp_rec_err}"

assert_test "Test 7: IPP restored to responsive idle with none reasons" '[[ "$ipp_rec_reasons" == "none" ]]'
assert_test "Test 7: SNMP restored to idle(3) with no errors" '[[ "$snmp_rec_st" =~ idle\(3\) ]] && [[ "$snmp_rec_err" =~ "00 00" ]]'

# -------------------------------------------------------------
echo ""
echo "--- Test 8: Independent target fault injection (disagreement simulation) ---"
echo "Injecting IPP-only paper-jam..."
"$FAULT_CLI" set --target ipp paper-jam
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_err=$(query_snmp_error)
echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP error:  ${snmp_err}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[CONTRADICTION\] IPP reports paper-jam" && t8_ipp_contra=true || t8_ipp_contra=false

assert_test "Test 8: IPP-only fault produces media-jam-report in IPP" '[[ "$ipp_reasons" =~ media-jam ]]'
assert_test "Test 8: SNMP remains healthy with 00 00 error" '[[ "$snmp_err" =~ "00 00" ]]'
assert_test "Test 8: Coordinated status reports IPP CONTRADICTION" '$t8_ipp_contra'

echo "Injecting SNMP-only paper-jam..."
"$FAULT_CLI" set --ipp healthy --snmp paper-jam
sleep 0.2

ipp_reasons=$(query_ipp_reasons)
snmp_err=$(query_snmp_error)
echo "  Live IPP reasons: ${ipp_reasons}"
echo "  Live SNMP error:  ${snmp_err}"

STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[CONTRADICTION\] SNMP reports paper-jam" && t8_snmp_contra=true || t8_snmp_contra=false

assert_test "Test 8: SNMP-only fault produces error in SNMP" '[[ "$snmp_err" =~ "04 00" || "$snmp_err" =~ "jam" ]]'
assert_test "Test 8: IPP remains healthy with none reasons" '[[ "$ipp_reasons" == "none" ]]'
assert_test "Test 8: Coordinated status reports SNMP CONTRADICTION" '$t8_snmp_contra'

# -------------------------------------------------------------
echo ""
echo "--- Test 9: Deterministic odometer and consumable control ---"
"$FAULT_CLI" set healthy --counter 9876 --toner 25 --drum 60
sleep 0.2

cnt_out=$(query_snmp_counter)
echo "  Odometer count: ${cnt_out}"
assert_test "Test 9: Odometer set deterministically to 9876" '[[ "$cnt_out" =~ "9876" ]]'

# Send a sync probe / job
"$FAULT_CLI" set healthy
sleep 0.2
cnt_out_after=$(query_snmp_counter)
echo "  Odometer count after sync job: ${cnt_out_after}"
assert_test "Test 9: Odometer did NOT auto-increment from sync probe/CUPS" '[[ "$cnt_out_after" =~ "9876" ]]'

# -------------------------------------------------------------
echo ""
echo "--- Test 10: Restart persistence across daemon stops ---"
"$FAULT_CLI" set cover-open
sleep 0.2

echo "Stopping IPP and SNMP daemons..."
"$EMULATOR" stop
"$EMULATOR" stop-snmp

echo "Restarting IPP and SNMP daemons..."
"$EMULATOR" start
"$EMULATOR" start-snmp
sleep 0.5

ipp_restored_reasons=$(query_ipp_reasons)
snmp_restored_err=$(query_snmp_error)

echo "  Restored IPP reasons: ${ipp_restored_reasons}"
echo "  Restored SNMP error:  ${snmp_restored_err}"

assert_test "Test 10: IPP restored cover-open-report on restart" '[[ "$ipp_restored_reasons" =~ cover-open ]]'
assert_test "Test 10: SNMP restored doorOpen error on restart" '[[ "$snmp_restored_err" =~ "08 00" || "$snmp_restored_err" =~ "doorOpen" ]]'

# -------------------------------------------------------------
echo ""
echo "--- Test 11: fault clear command restores healthy state ---"
"$FAULT_CLI" clear
sleep 0.2

ipp_clear_reasons=$(query_ipp_reasons)
snmp_clear_err=$(query_snmp_error)
STATUS_OUT=$("$FAULT_CLI" status)
echo "$STATUS_OUT" | grep -q "Status: \[AGREEMENT\] Both IPP and SNMP report HEALTHY" && t11_clear=true || t11_clear=false

assert_test "Test 11: IPP reasons cleared to none" '[[ "$ipp_clear_reasons" == "none" ]]'
assert_test "Test 11: SNMP errors cleared to 00 00" '[[ "$snmp_clear_err" =~ "00 00" ]]'
assert_test "Test 11: Coordinated status reports HEALTHY agreement" '$t11_clear'

# -------------------------------------------------------------
echo ""
echo "--- Test 12: Regression — Failed probe detection when emulator is stopped ---"
"$EMULATOR" stop >/dev/null 2>&1 || true

probe_fail_rc=0
"$FAULT_CLI" set paper-jam >/dev/null 2>&1 || probe_fail_rc=$?

assert_test "Test 12: fault set returns non-zero when probe fails" '[[ $probe_fail_rc -ne 0 ]]'

# Restart emulators for next tests
"$EMULATOR" start >/dev/null 2>&1
"$FAULT_CLI" clear >/dev/null 2>&1

# -------------------------------------------------------------
echo ""
echo "--- Test 13: Regression — Failed recovery detection when emulator is stopped ---"
"$EMULATOR" stop >/dev/null 2>&1 || true

recovery_fail_rc=0
"$FAULT_CLI" set recovery >/dev/null 2>&1 || recovery_fail_rc=$?

assert_test "Test 13: fault set recovery returns non-zero when endpoint is unavailable" '[[ $recovery_fail_rc -ne 0 ]]'

# Restart emulators for next tests
"$EMULATOR" start >/dev/null 2>&1
"$FAULT_CLI" clear >/dev/null 2>&1

# -------------------------------------------------------------
echo ""
echo "--- Test 14: Regression — Unrelated PID protection ---"
"$EMULATOR" stop >/dev/null 2>&1 || true
"$EMULATOR" stop-snmp >/dev/null 2>&1 || true

sleep 120 &
DUMMY_PID=$!

echo "$DUMMY_PID" > "$TEST_IPP_PID"
ipp_stop_rc=0
"$EMULATOR" stop >/dev/null 2>&1 || ipp_stop_rc=$?

assert_test "Test 14: emulator stop refuses to signal unrelated PID" '[[ $ipp_stop_rc -ne 0 ]]'
assert_test "Test 14: unrelated process remains alive after stop attempt" 'kill -0 "$DUMMY_PID" 2>/dev/null'

echo "$DUMMY_PID" > "$TEST_SNMP_PID"
snmp_stop_rc=0
"$EMULATOR" stop-snmp >/dev/null 2>&1 || snmp_stop_rc=$?

assert_test "Test 14: stop-snmp refuses to signal unrelated PID" '[[ $snmp_stop_rc -ne 0 ]]'
assert_test "Test 14: unrelated process remains alive after stop-snmp attempt" 'kill -0 "$DUMMY_PID" 2>/dev/null'

kill "$DUMMY_PID" 2>/dev/null || true
wait "$DUMMY_PID" 2>/dev/null || true
rm -f "$TEST_IPP_PID" "$TEST_SNMP_PID"

# Restart for normal state
"$EMULATOR" start >/dev/null 2>&1
"$EMULATOR" start-snmp >/dev/null 2>&1

# -------------------------------------------------------------
echo ""
echo "--- Test 15: Regression — Refusal to modify unowned CUPS queues ---"
alien_meta="${TEST_DIR}/cups_queue.meta"
export EMU_META_FILE="$alien_meta"
export EMU_CUPS_QUEUE="AlienQueue_Test"

# Case 1: No metadata file exists
unowned_modify_refused=false
if ! "$EMULATOR" verify-queue "AlienQueue_Test" >/dev/null 2>&1; then
    unowned_modify_refused=true
fi
assert_test "Test 15: Refuses queue operations when metadata is missing" '$unowned_modify_refused'

# Case 2: Metadata file exists but points to different queue name or URI
cat > "$alien_meta" << EOF
QUEUE_NAME=AlienQueue_Test
DEVICE_URI=ipp://alien-host:631/printers/AlienQueue_Test
EOF

mismatch_modify_refused=false
if ! "$EMULATOR" verify-queue "AlienQueue_Test" >/dev/null 2>&1; then
    mismatch_modify_refused=true
fi
assert_test "Test 15: Refuses queue operations when URI/queue does not match expected emulator URI" '$mismatch_modify_refused'

# Case 3: Refuse to remove or modify baseline system queue without ownership metadata
system_queue_refused=false
if ! "$EMULATOR" remove-cups "DCPT310" >/dev/null 2>&1; then
    system_queue_refused=true
fi
assert_test "Test 15: Refuses queue operations on unowned system CUPS queues" '$system_queue_refused'

rm -f "$alien_meta"
export EMU_CUPS_QUEUE="XeroxGoFaultEmu"

# -------------------------------------------------------------
echo ""
echo "--- Test 16: Safe cleanup of all emulator resources ---"
"$EMULATOR" cleanup

assert_test "Test 16: IPP PID file deleted" '[[ ! -f "$TEST_IPP_PID" ]]'
assert_test "Test 16: SNMP PID file deleted" '[[ ! -f "$TEST_SNMP_PID" ]]'
assert_test "Test 16: Fault state file deleted" '[[ ! -f "$TEST_FAULT_STATE" ]]'

echo ""
echo "============================================================"
echo "  Fault Smoke Test Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "============================================================"

if [[ "$FAILED_COUNT" -gt 0 ]]; then
    exit 1
fi
exit 0
