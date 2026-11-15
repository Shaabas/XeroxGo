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

FAULT_TEST_QUEUE=""
FAULT_TEST_QUEUE_META=""
FAULT_TEST_QUEUE_CREATED=false

COLLISION_FIXTURE_QUEUE=""
COLLISION_FIXTURE_CREATED=false
COLLISION_EXPECTED_URI="file:///dev/null"

# Helper for verified, safe removal of the collision fixture queue
cleanup_collision_fixture() {
    # Requirement 4, 5, 6: Only clean up if explicitly recorded as created by current test
    if [[ "${COLLISION_FIXTURE_CREATED:-false}" != "true" || -z "${COLLISION_FIXTURE_QUEUE:-}" ]]; then
        return 0
    fi

    if ! command -v lpadmin >/dev/null 2>&1; then
        return 0
    fi

    local target_queue="$COLLISION_FIXTURE_QUEUE"

    # Requirement 7: Verify its actual CUPS URI still matches the expected fixture URI before deleting
    local actual_uri
    actual_uri=$(lpstat -v "$target_queue" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')
    if [[ -z "$actual_uri" || "$actual_uri" != "${COLLISION_EXPECTED_URI:-}" ]]; then
        echo "ERROR: Fixture queue '${target_queue}' URI ('${actual_uri}') does not match expected URI ('${COLLISION_EXPECTED_URI:-}')." >&2
        echo "Ownership uncertain; preserving queue '${target_queue}' for safety." >&2
        return 1
    fi

    # Verify queue still exists before attempting removal
    if ! lpstat -p "$target_queue" >/dev/null 2>&1; then
        COLLISION_FIXTURE_CREATED=false
        return 0
    fi

    # Requirement 5 & 8: Never execute unconditional deletion; preserve queue and report failure if deletion fails
    if ! lpadmin -x "$target_queue" 2>/dev/null; then
        echo "ERROR: Failed to delete fixture queue '${target_queue}' via lpadmin -x." >&2
        echo "Preserving queue '${target_queue}' for safety." >&2
        return 1
    fi

    # Verify queue ceases to exist in CUPS
    if lpstat -p "$target_queue" >/dev/null 2>&1 || lpstat -v "$target_queue" >/dev/null 2>&1; then
        echo "ERROR: Fixture queue '${target_queue}' still exists in CUPS after deletion attempt." >&2
        echo "Preserving queue reference for safety." >&2
        return 1
    fi

    COLLISION_FIXTURE_CREATED=false
    return 0
}

cleanup_test_env() {
    echo ""
    echo "--- Cleaning up fault test environment ---"

    # Requirement 9: Ensure the EXIT trap handles interrupted tests without modifying unrelated queues
    if [[ "${COLLISION_FIXTURE_CREATED:-false}" == "true" && -n "${COLLISION_FIXTURE_QUEUE:-}" ]]; then
        cleanup_collision_fixture >/dev/null 2>&1 || true
    fi

    # Only delete test CUPS queue if it was explicitly recorded as created by this harness
    # Requirement 2: The EXIT trap must never execute unverified lpadmin -x.
    # Requirement 3: A generated queue name does not establish ownership.
    # Requirement 4: If setup-cups fails because a queue already exists, cleanup must preserve that queue.
    # Requirement 5: Delete test queues only through verified ownership checks (configured, metadata, and actual URIs).
    # Requirement 6: Record successful test queue creation explicitly (FAULT_TEST_QUEUE_CREATED).
    if [[ "${FAULT_TEST_QUEUE_CREATED:-false}" == "true" && -n "${FAULT_TEST_QUEUE:-}" ]] && command -v lpadmin >/dev/null 2>&1; then
        if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
            echo "Attempting verified removal of harness test queue '${FAULT_TEST_QUEUE}'..."
            if EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE" EMU_META_FILE="$FAULT_TEST_QUEUE_META" "$EMULATOR" remove-cups "$FAULT_TEST_QUEUE" >/dev/null 2>&1; then
                FAULT_TEST_QUEUE_CREATED=false
            else
                echo "WARNING: Verified removal of test queue '${FAULT_TEST_QUEUE}' failed or was refused. Preserving metadata '${FAULT_TEST_QUEUE_META}'." >&2
            fi
        else
            echo "WARNING: Test queue '${FAULT_TEST_QUEUE}' marked created but metadata is missing. Refusing unverified deletion." >&2
        fi
    fi

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

    # Requirement 7: Preserve ownership metadata if verified deletion fails
    if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
        find "$TEST_DIR" -mindepth 1 -not -path "$FAULT_TEST_QUEUE_META" -delete 2>/dev/null || true
    else
        rm -rf "$TEST_DIR" 2>/dev/null || true
    fi
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
echo "--- Test 15: Regression — Three-way URI verification and refusal to modify unowned queues ---"
alien_meta="${TEST_DIR}/cups_queue.meta"
export EMU_META_FILE="$alien_meta"
export EMU_CUPS_QUEUE="AlienQueue_Test"

# Case 1: No metadata file exists
unowned_modify_refused=false
if ! "$EMULATOR" verify-queue "AlienQueue_Test" >/dev/null 2>&1; then
    unowned_modify_refused=true
fi
assert_test "Test 15: Refuses queue operations when metadata is missing" '$unowned_modify_refused'

# Case 2: Metadata file exists but points to different queue name
cat > "$alien_meta" << EOF
QUEUE_NAME=DifferentQueue
DEVICE_URI=ipp://${EMU_HOST}:${EMU_PORT}/ipp/print
EOF
wrong_name_refused=false
if ! "$EMULATOR" verify-queue "AlienQueue_Test" >/dev/null 2>&1; then
    wrong_name_refused=true
fi
assert_test "Test 15: Refuses queue operations when metadata queue name does not match" '$wrong_name_refused'

# Case 3: Metadata file exists with correct queue name, but incorrect/alien URI
cat > "$alien_meta" << EOF
QUEUE_NAME=AlienQueue_Test
DEVICE_URI=ipp://alien-printer:631/printers/AlienQueue_Test
EOF

alien_uri_refused=false
if ! "$EMULATOR" verify-queue "AlienQueue_Test" >/dev/null 2>&1; then
    alien_uri_refused=true
fi
assert_test "Test 15: Refuses queue operations when metadata URI points to unrelated printer/endpoint" '$alien_uri_refused'

# Case 4: Refusal to delete queue when metadata URI does not match configured emulator URI
alien_del_refused=false
if ! "$EMULATOR" remove-cups "AlienQueue_Test" >/dev/null 2>&1; then
    alien_del_refused=true
fi
assert_test "Test 15: Refuses remove-cups when metadata URI does not match configured URI" '$alien_del_refused'
assert_test "Test 15: Metadata file preserved when remove-cups is refused" '[[ -f "$alien_meta" ]]'

# Case 5: Refuse to remove or modify baseline system queue without ownership metadata
system_queue_refused=false
if ! "$EMULATOR" remove-cups "DCPT310" >/dev/null 2>&1; then
    system_queue_refused=true
fi
assert_test "Test 15: Refuses queue operations on unowned system CUPS queues" '$system_queue_refused'

rm -f "$alien_meta"
export EMU_CUPS_QUEUE="XeroxGoFaultEmu"

# -------------------------------------------------------------
echo ""
echo "--- Test 16: Regression — Failed CUPS queue deletion preserves metadata ---"
if command -v lpadmin >/dev/null 2>&1 && lpstat -r >/dev/null 2>&1; then
    FAULT_TEST_QUEUE="XeroxGoFaultQ_${$}"
    FAULT_TEST_QUEUE_META="${TEST_DIR}/fault_queue.meta"
    FAULT_TEST_QUEUE_CREATED=false
    export EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE"
    export EMU_META_FILE="$FAULT_TEST_QUEUE_META"

    # Create temporary verified driverless CUPS queue targeting emulator
    if "$EMULATOR" setup-cups >/dev/null 2>&1; then
        # Requirement 6: Record successful test queue creation explicitly
        FAULT_TEST_QUEUE_CREATED=true
        assert_test "Test 16: Setup temporary driverless test queue succeeded" 'lpstat -p "$FAULT_TEST_QUEUE" >/dev/null 2>&1'
        assert_test "Test 16: Verification succeeds on matching configured, metadata, and actual URIs" '"$EMULATOR" verify-queue "$FAULT_TEST_QUEUE" >/dev/null 2>&1'

        # Simulate lpadmin -x failure via mock
        mock_bin_dir="${TEST_DIR}/mock_bin"
        mkdir -p "$mock_bin_dir"
        cat > "${mock_bin_dir}/lpadmin" << 'MOCK_EOF'
#!/usr/bin/env bash
echo "lpadmin: simulated deletion failure" >&2
exit 1
MOCK_EOF
        chmod +x "${mock_bin_dir}/lpadmin"

        simulated_del_failed=false
        PATH="${mock_bin_dir}:${PATH}" "$EMULATOR" remove-cups "$FAULT_TEST_QUEUE" >/dev/null 2>&1 || simulated_del_failed=true

        assert_test "Test 16: remove-cups propagates lpadmin -x failure (non-zero return code)" '$simulated_del_failed'
        assert_test "Test 16: Ownership metadata preserved when deletion fails" '[[ -f "$FAULT_TEST_QUEUE_META" ]]'
        assert_test "Test 16: Queue still exists in CUPS when deletion fails" 'lpstat -p "$FAULT_TEST_QUEUE" >/dev/null 2>&1'

        rm -rf "$mock_bin_dir"

        # Now execute clean removal with real lpadmin
        real_del_succeeded=false
        if "$EMULATOR" remove-cups "$FAULT_TEST_QUEUE" >/dev/null 2>&1; then
            real_del_succeeded=true
            FAULT_TEST_QUEUE_CREATED=false
        fi

        assert_test "Test 16: remove-cups succeeds with real lpadmin" '$real_del_succeeded'
        assert_test "Test 16: Metadata removed only after queue verified deleted from CUPS" '[[ ! -f "$FAULT_TEST_QUEUE_META" ]]'
        assert_test "Test 16: Queue verified deleted from CUPS" '! lpstat -p "$FAULT_TEST_QUEUE" >/dev/null 2>&1'
    fi

    # Reset environment variables
    export EMU_CUPS_QUEUE="XeroxGoFaultEmu"
    export EMU_META_FILE="${TEST_DIR}/cups_queue.meta"
fi

# -------------------------------------------------------------
echo ""
echo "--- Test 17: Regression — Queue-name collision followed by EXIT cleanup preserves existing queue ---"
if command -v lpadmin >/dev/null 2>&1 && lpstat -r >/dev/null 2>&1; then
    COLLISION_FIXTURE_QUEUE=""
    COLLISION_FIXTURE_CREATED=false
    COLLISION_EXPECTED_URI=""

    # Requirement 2 & 3: Before calling lpadmin -p, verify proposed fixture name does not already exist.
    # If occupied, choose another unique name or fail safely without modifying it.
    candidate_base="XeroxGoCollision_${$}"
    candidate_queue="${candidate_base}"
    attempt=1
    while lpstat -p "$candidate_queue" >/dev/null 2>&1 || lpstat -v "$candidate_queue" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        candidate_queue="${candidate_base}_${attempt}"
        if [[ $attempt -gt 10 ]]; then
            break
        fi
    done

    # Verify candidate is genuinely unused
    candidate_available=false
    if ! lpstat -p "$candidate_queue" >/dev/null 2>&1 && ! lpstat -v "$candidate_queue" >/dev/null 2>&1; then
        candidate_available=true
    fi
    assert_test "Test 17: Candidate fixture queue name verified unused before creation" '$candidate_available'

    if [[ "$candidate_available" == "true" ]]; then
        COLLISION_FIXTURE_QUEUE="$candidate_queue"

        # Create pre-existing fixture queue
        if lpadmin -p "$COLLISION_FIXTURE_QUEUE" -v "file:///dev/null" -E 2>/dev/null; then
            pre_uri=$(lpstat -v "$COLLISION_FIXTURE_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')
            if [[ -n "$pre_uri" && ( "$pre_uri" == "file:///dev/null" || "$pre_uri" == "///dev/null" ) ]] && lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1; then
                # Requirement 4: Record explicit successful creation of fixture queue and expected URI
                COLLISION_EXPECTED_URI="$pre_uri"
                COLLISION_FIXTURE_CREATED=true
            fi
        fi

        assert_test "Test 17: Pre-existing collision fixture queue recorded created" '[[ "$COLLISION_FIXTURE_CREATED" == "true" ]]'

        # Sub-case 1: setup-cups fails due to collision; EXIT trap must preserve the colliding queue
        collision_subshell_out=$(
            bash -c '
                set -euo pipefail
                COLLISION_Q="'"$COLLISION_FIXTURE_QUEUE"'"
                EMULATOR="'"$EMULATOR"'"
                TEST_DIR=$(mktemp -d /tmp/xeroxgo-coll-test-XXXXXX)

                FAULT_TEST_QUEUE="$COLLISION_Q"
                FAULT_TEST_QUEUE_META="${TEST_DIR}/collision_queue.meta"
                FAULT_TEST_QUEUE_CREATED=false

                export EMU_HOST="localhost"
                export EMU_PORT="'"$TEST_IPP_PORT"'"
                export EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE"
                export EMU_META_FILE="$FAULT_TEST_QUEUE_META"

                cleanup_subshell() {
                    if [[ "${FAULT_TEST_QUEUE_CREATED:-false}" == "true" && -n "${FAULT_TEST_QUEUE:-}" ]] && command -v lpadmin >/dev/null 2>&1; then
                        if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
                            EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE" EMU_META_FILE="$FAULT_TEST_QUEUE_META" "$EMULATOR" remove-cups "$FAULT_TEST_QUEUE" >/dev/null 2>&1 || true
                            if ! lpstat -p "$FAULT_TEST_QUEUE" >/dev/null 2>&1; then
                                FAULT_TEST_QUEUE_CREATED=false
                            fi
                        fi
                    fi
                    if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
                        find "$TEST_DIR" -mindepth 1 -not -path "$FAULT_TEST_QUEUE_META" -delete 2>/dev/null || true
                    else
                        rm -rf "$TEST_DIR" 2>/dev/null || true
                    fi
                }
                trap cleanup_subshell EXIT

                # setup-cups must fail because queue already exists
                if "$EMULATOR" setup-cups 2>&1; then
                    FAULT_TEST_QUEUE_CREATED=true
                    echo "UNEXPECTED_SETUP_SUCCESS"
                    exit 1
                fi

                echo "SETUP_FAILED_AS_EXPECTED"
                exit 0
            ' 2>&1
        )

        post_uri=$(lpstat -v "$COLLISION_FIXTURE_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')

        assert_test "Test 17: setup-cups refused existing queue collision" '[[ "$collision_subshell_out" =~ "SETUP_FAILED_AS_EXPECTED" ]]'
        assert_test "Test 17: Colliding CUPS queue preserved after subshell EXIT cleanup" 'lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1'
        assert_test "Test 17: Colliding CUPS queue URI unchanged after subshell EXIT cleanup" '[[ "$pre_uri" == "$post_uri" && -n "$post_uri" ]]'

        # Sub-case 2: Unverified queue deletion refusal during cleanup:
        # Even if FAULT_TEST_QUEUE_CREATED=true were mistakenly set with forged metadata, EXIT cleanup refuses unverified queue deletion
        unverified_trap_out=$(
            bash -c '
                set -euo pipefail
                COLLISION_Q="'"$COLLISION_FIXTURE_QUEUE"'"
                EMULATOR="'"$EMULATOR"'"
                TEST_DIR=$(mktemp -d /tmp/xeroxgo-unver-test-XXXXXX)

                FAULT_TEST_QUEUE="$COLLISION_Q"
                FAULT_TEST_QUEUE_META="${TEST_DIR}/unver_queue.meta"
                FAULT_TEST_QUEUE_CREATED=true

                export EMU_HOST="localhost"
                export EMU_PORT="'"$TEST_IPP_PORT"'"
                export EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE"
                export EMU_META_FILE="$FAULT_TEST_QUEUE_META"

                # Create metadata pointing to emulator URI, but actual CUPS queue points to file:///dev/null
                cat > "$FAULT_TEST_QUEUE_META" << EOF
QUEUE_NAME=${COLLISION_Q}
DEVICE_URI=ipp://localhost:'"$TEST_IPP_PORT"'/ipp/print
EOF

                cleanup_subshell() {
                    if [[ "${FAULT_TEST_QUEUE_CREATED:-false}" == "true" && -n "${FAULT_TEST_QUEUE:-}" ]] && command -v lpadmin >/dev/null 2>&1; then
                        if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
                            EMU_CUPS_QUEUE="$FAULT_TEST_QUEUE" EMU_META_FILE="$FAULT_TEST_QUEUE_META" "$EMULATOR" remove-cups "$FAULT_TEST_QUEUE" 2>&1 || true
                        fi
                    fi
                    if [[ -n "${FAULT_TEST_QUEUE_META:-}" && -f "$FAULT_TEST_QUEUE_META" ]]; then
                        find "$TEST_DIR" -mindepth 1 -not -path "$FAULT_TEST_QUEUE_META" -delete 2>/dev/null || true
                    else
                        rm -rf "$TEST_DIR" 2>/dev/null || true
                    fi
                }
                trap cleanup_subshell EXIT
                exit 0
            ' 2>&1
        )

        post_uri2=$(lpstat -v "$COLLISION_FIXTURE_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')

        assert_test "Test 17: EXIT cleanup refused unverified queue deletion" '[[ "$unverified_trap_out" =~ "Refusing deletion of unverified CUPS queue" ]]'
        assert_test "Test 17: Colliding CUPS queue preserved after unverified EXIT cleanup" 'lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1'
        assert_test "Test 17: Colliding CUPS queue URI still unchanged" '[[ "$pre_uri" == "$post_uri2" && -n "$post_uri2" ]]'

        # Sub-case 3: Requirement 7 & 8: Verify actual CUPS URI matches expected fixture URI before deleting; if mismatched, refuse and preserve
        mismatch_refused=false
        saved_expected_uri="$COLLISION_EXPECTED_URI"
        COLLISION_EXPECTED_URI="file:///unrelated/path"
        if ! cleanup_collision_fixture >/dev/null 2>&1; then
            mismatch_refused=true
        fi
        COLLISION_EXPECTED_URI="$saved_expected_uri"

        assert_test "Test 17: cleanup_collision_fixture refuses deletion on expected URI mismatch" '$mismatch_refused'
        assert_test "Test 17: Queue preserved when expected URI mismatch occurs" 'lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1'

        # Sub-case 4: Requirement 5: Never execute unconditional lpadmin -x for an unverified fixture
        # Test that cleanup_collision_fixture does nothing if COLLISION_FIXTURE_CREATED is false
        unverified_clean_noop=false
        saved_created="$COLLISION_FIXTURE_CREATED"
        COLLISION_FIXTURE_CREATED=false
        if cleanup_collision_fixture >/dev/null 2>&1; then
            if lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1; then
                unverified_clean_noop=true
            fi
        fi
        COLLISION_FIXTURE_CREATED="$saved_created"
        assert_test "Test 17: cleanup_collision_fixture refuses deletion when unverified (CREATED=false)" '$unverified_clean_noop'

        # Sub-case 5: Requirement 6 & 7: Clean up only the exact queue successfully created by the current test
        # after verifying its actual CUPS URI matches the expected fixture URI
        fixture_cleaned=false
        if cleanup_collision_fixture >/dev/null 2>&1; then
            fixture_cleaned=true
        fi
        assert_test "Test 17: Fixture cleanup succeeds on verified matching URI" '$fixture_cleaned'
        assert_test "Test 17: Fixture queue verified deleted after clean removal" '! lpstat -p "$COLLISION_FIXTURE_QUEUE" >/dev/null 2>&1'
        assert_test "Test 17: Fixture creation status reset to false" '[[ "$COLLISION_FIXTURE_CREATED" == "false" ]]'
    fi
fi

# -------------------------------------------------------------
echo ""
echo "--- Test 18: Safe cleanup of all emulator resources ---"
"$EMULATOR" cleanup

assert_test "Test 18: IPP PID file deleted" '[[ ! -f "$TEST_IPP_PID" ]]'
assert_test "Test 18: SNMP PID file deleted" '[[ ! -f "$TEST_SNMP_PID" ]]'
assert_test "Test 18: Fault state file deleted" '[[ ! -f "$TEST_FAULT_STATE" ]]'

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
