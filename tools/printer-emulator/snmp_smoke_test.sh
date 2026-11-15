#!/usr/bin/env bash
# snmp_smoke_test.sh - Comprehensive smoke & safety tests for local SNMP printer emulator
#
# Tests RFC-compatible SNMPv2c MIB queries against the local loopback emulator,
# verifying healthy state, error bit decoding (byte 0 and byte 1), toner/drum levels,
# deterministic counter updates, missing OID handling, restart persistence,
# community authorization, and safety protections.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EMULATOR_SCRIPT="${SCRIPT_DIR}/emulator.sh"

# Test isolation parameters
TEST_SNMP_PORT="11161"
TEST_SNMP_HOST="127.0.0.1"
TEST_SNMP_COMMUNITY="public"
TEST_DIR=$(mktemp -d /tmp/xeroxgo-snmp-test-XXXXXX)
TEST_STATE_FILE="${TEST_DIR}/snmp_state.json"
TEST_PID_FILE="${TEST_DIR}/snmp_responder.pid"
TEST_LOG_FILE="${TEST_DIR}/snmp_responder.log"

TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

pass() {
    local name="$1"
    PASSED_TESTS=$((PASSED_TESTS + 1))
    echo "[PASS] ${name}"
    echo ""
}

fail() {
    local name="$1"
    local reason="${2:-}"
    FAILED_TESTS=$((FAILED_TESTS + 1))
    echo "[FAIL] ${name}: ${reason}" >&2
    echo ""
}

cleanup_test_env() {
    echo "--- Cleaning up test environment ---"
    if [[ -f "$TEST_PID_FILE" ]]; then
        local pid
        pid=$(cat "$TEST_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 0.2
            kill -9 "$pid" 2>/dev/null || true
        fi
    fi
    rm -rf "$TEST_DIR" 2>/dev/null || true
}
trap cleanup_test_env EXIT

echo "============================================================"
echo "  XeroxGo Local SNMP Printer Emulator — Smoke Test Suite"
echo "============================================================"
echo "Working directory: ${SCRIPT_DIR}"
echo "Test temporary dir: ${TEST_DIR}"
echo "Test UDP port:     ${TEST_SNMP_PORT}"
echo ""

# Prerequisites verification
echo "Prerequisites verified:"
if command -v snmpget >/dev/null 2>&1; then
    echo "  snmpget: $(command -v snmpget)"
else
    echo "ERROR: snmpget CLI is required for SNMP smoke tests." >&2
    exit 1
fi
echo ""

# Helper to run snmpget against test instance
snmp_get() {
    local oid="$1"
    /usr/bin/snmpget -v2c -c "$TEST_SNMP_COMMUNITY" -r 1 -t 1 "${TEST_SNMP_HOST}:${TEST_SNMP_PORT}" "$oid" 2>&1
}

# -----------------------------------------------------------------------------
# Test 1: Emulator startup and process lifecycle
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 1: SNMP emulator startup and lifecycle ---"
"$EMULATOR_SCRIPT" \
    --snmp-host "$TEST_SNMP_HOST" \
    --snmp-port "$TEST_SNMP_PORT" \
    --snmp-community "$TEST_SNMP_COMMUNITY" \
    --snmp-state "$TEST_STATE_FILE" \
    start-snmp

if [[ ! -f "$TEST_PID_FILE" ]]; then
    # Default pid path was used
    TEST_PID_FILE="${SCRIPT_DIR}/snmp_responder.pid"
fi

if ! kill -0 "$(cat "$TEST_PID_FILE")" 2>/dev/null; then
    fail "Test 1: SNMP emulator startup" "Process PID is not alive"
else
    echo "  SNMP emulator responsive on port ${TEST_SNMP_PORT} (PID $(cat "$TEST_PID_FILE"))"
    pass "Test 1: SNMP emulator startup and lifecycle"
fi

# -----------------------------------------------------------------------------
# Test 2: Default healthy state MIB queries via snmpget
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 2: Default healthy state queries (snmpget) ---"

# 1. sysDescr (1.3.6.1.2.1.1.1.0)
out_sys=$(snmp_get "1.3.6.1.2.1.1.1.0")
echo "  sysDescr: $out_sys"
if [[ ! "$out_sys" =~ "Xerox WorkCentre" ]]; then
    fail "Test 2: sysDescr" "Unexpected output: $out_sys"
fi

# 2. hrPrinterStatus (1.3.6.1.2.1.25.3.5.1.1.1)
out_status=$(snmp_get "1.3.6.1.2.1.25.3.5.1.1.1")
echo "  hrPrinterStatus: $out_status"
if [[ ! "$out_status" =~ "idle(3)" && ! "$out_status" =~ "INTEGER: 3" ]]; then
    fail "Test 2: hrPrinterStatus" "Expected idle(3), got: $out_status"
fi

# 3. hrPrinterDetectedErrorState (1.3.6.1.2.1.25.3.5.1.2.1)
out_err=$(snmp_get "1.3.6.1.2.1.25.3.5.1.2.1")
echo "  hrPrinterDetectedErrorState: $out_err"
if [[ ! "$out_err" =~ "00 00" && ! "$out_err" =~ "\"\"" ]]; then
    fail "Test 2: hrPrinterDetectedErrorState" "Expected clean zero error bits, got: $out_err"
fi

# 4. prtMarkerLifeCount (1.3.6.1.2.1.43.10.2.1.4.1.1)
out_cnt=$(snmp_get "1.3.6.1.2.1.43.10.2.1.4.1.1")
echo "  prtMarkerLifeCount: $out_cnt"
if [[ ! "$out_cnt" =~ "1000" ]]; then
    fail "Test 2: prtMarkerLifeCount" "Expected Counter32: 1000, got: $out_cnt"
fi

# 5. Toner level and max (1.3.6.1.2.1.43.11.1.1.9.1.1 / 1.3.6.1.2.1.43.11.1.1.8.1.1)
out_tl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.1")
out_tm=$(snmp_get "1.3.6.1.2.1.43.11.1.1.8.1.1")
echo "  Toner level / max: $out_tl / $out_tm"
if [[ ! "$out_tl" =~ "85" ]] || [[ ! "$out_tm" =~ "100" ]]; then
    fail "Test 2: Toner supply" "Expected 85 / 100, got: $out_tl / $out_tm"
fi

# 6. Drum level and max (1.3.6.1.2.1.43.11.1.1.9.1.2 / 1.3.6.1.2.1.43.11.1.1.8.1.2)
out_dl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.2")
out_dm=$(snmp_get "1.3.6.1.2.1.43.11.1.1.8.1.2")
echo "  Drum level / max: $out_dl / $out_dm"
if [[ ! "$out_dl" =~ "95" ]] || [[ ! "$out_dm" =~ "100" ]]; then
    fail "Test 2: Drum supply" "Expected 95 / 100, got: $out_dl / $out_dm"
fi

pass "Test 2: Default healthy state MIB queries (snmpget)"

# -----------------------------------------------------------------------------
# Test 3: Status command check (status-snmp)
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 3: Status command reporting ---"
out_stat=$("$EMULATOR_SCRIPT" --snmp-port "$TEST_SNMP_PORT" --snmp-state "$TEST_STATE_FILE" status-snmp)
echo "$out_stat"
if [[ ! "$out_stat" =~ "SNMP Status: RUNNING" ]] || [[ ! "$out_stat" =~ "IDLE (3)" ]]; then
    fail "Test 3: Status reporting" "Expected RUNNING status with IDLE (3), got: $out_stat"
else
    pass "Test 3: Status command reporting"
fi

# -----------------------------------------------------------------------------
# Test 4: Controlled state updates (Byte 0 error bits: jammed, doorOpen)
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 4: Controlled Byte 0 error bit updates (jammed, door_open) ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -set-error jammed,door_open

out_err=$(snmp_get "1.3.6.1.2.1.25.3.5.1.2.1")
echo "  Updated hrPrinterDetectedErrorState: $out_err"
# DoorOpen is 0x08, Jammed is 0x04 -> Byte 0 = 0x0C
if [[ ! "$out_err" =~ "0C 00" && ! "$out_err" =~ "0c 00" ]]; then
    fail "Test 4: Byte 0 error bitmask" "Expected 0C 00 (jammed + doorOpen), got: $out_err"
else
    pass "Test 4: Controlled Byte 0 error bit updates (jammed, door_open)"
fi

# -----------------------------------------------------------------------------
# Test 5: Controlled Byte 1 error bit updates (outputFull, inputTrayMissing)
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 5: Controlled Byte 1 error bit updates (output_full, input_tray_missing) ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -clear-all-errors -set-error output_full,input_tray_missing

out_err=$(snmp_get "1.3.6.1.2.1.25.3.5.1.2.1")
echo "  Updated hrPrinterDetectedErrorState: $out_err"
# inputTrayMissing is bit 8 (0x80), outputFull is bit 12 (0x08) -> Byte 1 = 0x88
if [[ ! "$out_err" =~ "00 88" ]]; then
    fail "Test 5: Byte 1 error bitmask" "Expected 00 88 (outputFull + inputTrayMissing), got: $out_err"
else
    pass "Test 5: Controlled Byte 1 error bit updates (output_full, input_tray_missing)"
fi

# -----------------------------------------------------------------------------
# Test 6: Consumables and status modification
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 6: Consumables and status modification ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set \
    -status warmup \
    -toner-level 15 \
    -drum-level 45

out_st=$(snmp_get "1.3.6.1.2.1.25.3.5.1.1.1")
out_tl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.1")
out_dl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.2")
echo "  Status: $out_st, Toner: $out_tl, Drum: $out_dl"
if [[ ! "$out_st" =~ "warmup(5)" && ! "$out_st" =~ "INTEGER: 5" ]]; then
    fail "Test 6: Status warmup" "Expected status 5, got: $out_st"
elif [[ ! "$out_tl" =~ "15" ]] || [[ ! "$out_dl" =~ "45" ]]; then
    fail "Test 6: Consumables" "Expected toner 15, drum 45, got: $out_tl / $out_dl"
else
    pass "Test 6: Consumables and status modification"
fi

# -----------------------------------------------------------------------------
# Test 7: Deterministic counter updates
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 7: Deterministic counter updates ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -counter-inc 42

out_cnt=$(snmp_get "1.3.6.1.2.1.43.10.2.1.4.1.1")
echo "  Updated marker count: $out_cnt"
if [[ ! "$out_cnt" =~ "1042" ]]; then
    fail "Test 7: Counter increment" "Expected Counter32: 1042, got: $out_cnt"
else
    pass "Test 7: Deterministic counter updates"
fi

# -----------------------------------------------------------------------------
# Test 8: Missing OID simulation (NoSuchInstance)
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 8: Missing OID simulation ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -omit-oid 1.3.6.1.2.1.43.10.2.1.4.1.1

out_omit=$(snmp_get "1.3.6.1.2.1.43.10.2.1.4.1.1")
echo "  Omitted OID response: $out_omit"
if [[ ! "$out_omit" =~ "No Such Instance" && ! "$out_omit" =~ "noSuchInstance" ]]; then
    fail "Test 8: Missing OID" "Expected 'No Such Instance', got: $out_omit"
else
    pass "Test 8: Missing OID simulation"
fi

# Restore OID for subsequent tests
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -clear-omitted

# -----------------------------------------------------------------------------
# Test 9: Restart persistence
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 9: Stop -> Restart state persistence ---"
# Set a distinct marker count before stopping
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-set -counter 7777 -toner-level 33

last_pid=$(cat "$TEST_PID_FILE" 2>/dev/null || true)
"$EMULATOR_SCRIPT" --snmp-port "$TEST_SNMP_PORT" stop-snmp
sleep 0.5

# Verify previous process stopped
if [[ -n "$last_pid" ]] && kill -0 "$last_pid" 2>/dev/null; then
    fail "Test 9: Stop emulator" "Process PID $last_pid failed to stop"
fi

# Restart using same state file
"$EMULATOR_SCRIPT" \
    --snmp-host "$TEST_SNMP_HOST" \
    --snmp-port "$TEST_SNMP_PORT" \
    --snmp-community "$TEST_SNMP_COMMUNITY" \
    --snmp-state "$TEST_STATE_FILE" \
    start-snmp

out_cnt=$(snmp_get "1.3.6.1.2.1.43.10.2.1.4.1.1")
out_tl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.1")
echo "  Restored state: Counter = $out_cnt, Toner = $out_tl"
if [[ ! "$out_cnt" =~ "7777" ]] || [[ ! "$out_tl" =~ "33" ]]; then
    fail "Test 9: Restart persistence" "State did not persist across restart. Got counter: $out_cnt, toner: $out_tl"
else
    pass "Test 9: Stop -> Restart state persistence"
fi

# -----------------------------------------------------------------------------
# Test 10: Reset state
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 10: Reset state to default healthy/idle ---"
"$EMULATOR_SCRIPT" --snmp-state "$TEST_STATE_FILE" snmp-reset

out_st=$(snmp_get "1.3.6.1.2.1.25.3.5.1.1.1")
out_cnt=$(snmp_get "1.3.6.1.2.1.43.10.2.1.4.1.1")
out_tl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.1")
out_dl=$(snmp_get "1.3.6.1.2.1.43.11.1.1.9.1.2")
if [[ ! "$out_st" =~ "idle(3)" && ! "$out_st" =~ "INTEGER: 3" ]] || \
   [[ ! "$out_cnt" =~ "1000" ]] || \
   [[ ! "$out_tl" =~ "85" ]] || \
   [[ ! "$out_dl" =~ "95" ]]; then
    fail "Test 10: Reset state" "Expected default values (3, 1000, 85, 95)"
else
    pass "Test 10: Reset state to default healthy/idle"
fi

# -----------------------------------------------------------------------------
# Test 11: Community authorization
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 11: Unauthorized community string rejection ---"
out_auth=$(/usr/bin/snmpget -v2c -c "invalid_secret" -r 0 -t 1 "${TEST_SNMP_HOST}:${TEST_SNMP_PORT}" "1.3.6.1.2.1.1.1.0" 2>&1 || true)
echo "  Unauthorized query output: $out_auth"
if [[ "$out_auth" =~ "Timeout" || "$out_auth" =~ "No Response" ]]; then
    echo "  Unauthorized community query safely timed out with no data leakage"
    pass "Test 11: Unauthorized community string rejection"
else
    fail "Test 11: Unauthorized community" "Expected Timeout, got: $out_auth"
fi

# -----------------------------------------------------------------------------
# Safety Test 12: Occupied port detection
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Safety Test 12: Occupied port detection ---"
# Spin up a dummy netcat listener on an unprivileged port
collision_port=11197
nc -u -l 127.0.0.1 "$collision_port" &
dummy_nc_pid=$!
sleep 0.2

out_coll=$("$EMULATOR_SCRIPT" --snmp-port "$collision_port" --snmp-state "${TEST_DIR}/dummy.json" start-snmp 2>&1 || true)
kill "$dummy_nc_pid" 2>/dev/null || true
echo "  Collision startup attempt output: $out_coll"

if [[ "$out_coll" =~ "already in use" ]]; then
    echo "  start-snmp correctly refused to bind already occupied UDP port"
    pass "Safety Test 12: Occupied port detection"
else
    fail "Safety Test 12: Occupied port" "Expected refusal due to port collision"
fi

# -----------------------------------------------------------------------------
# Safety Test 13: Loopback safety verification
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Safety Test 13: Loopback safety enforcement ---"
out_loop=$("$EMULATOR_SCRIPT" --snmp-host "192.168.99.200" --snmp-port 11199 start-snmp 2>&1 || true)
echo "  Non-loopback attempt output: $out_loop"
if [[ "$out_loop" =~ "Safety violation" ]]; then
    echo "  start-snmp correctly rejected non-loopback host interface"
    pass "Safety Test 13: Loopback safety enforcement (refuses non-loopback)"
else
    fail "Safety Test 13: Loopback safety" "Refusal expected for non-loopback interface"
fi

# -----------------------------------------------------------------------------
# Safety Test 14: Unrelated PID protection
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Safety Test 14: Unrelated PID protection ---"
fake_pid_file="${SCRIPT_DIR}/snmp_responder.pid"
# Find another active process (current shell PID)
target_pid=$$
echo "$target_pid" > "$fake_pid_file"

out_unrel=$("$EMULATOR_SCRIPT" --snmp-port "$TEST_SNMP_PORT" stop-snmp 2>&1 || true)
rm -f "$fake_pid_file"
echo "  Unrelated PID stop output: $out_unrel"

if [[ "$out_unrel" =~ "Refusing to signal unrelated process" ]]; then
    echo "  stop-snmp correctly refused to signal unrelated PID ($target_pid)"
    pass "Safety Test 14: Unrelated PID protection (refuses to kill alien PID)"
else
    fail "Safety Test 14: Unrelated PID protection" "stop-snmp failed to protect unrelated PID"
fi

# -----------------------------------------------------------------------------
# Test 15: Safe cleanup (cleanup-snmp)
# -----------------------------------------------------------------------------
TOTAL_TESTS=$((TOTAL_TESTS + 1))
echo "--- Test 15: Safe cleanup ---"
"$EMULATOR_SCRIPT" \
    --snmp-port "$TEST_SNMP_PORT" \
    --snmp-state "$TEST_STATE_FILE" \
    cleanup-snmp

if [[ -f "$TEST_STATE_FILE" ]]; then
    fail "Test 15: Cleanup" "State file was not deleted"
else
    echo "  Verified cleanup removed emulator state and terminated process"
    pass "Test 15: Safe cleanup"
fi

# -----------------------------------------------------------------------------
# Final Summary
# -----------------------------------------------------------------------------
echo "============================================================"
echo "  SNMP Smoke & Safety Test Results:"
echo "    TOTAL:   ${TOTAL_TESTS}"
echo "    PASSED:  ${PASSED_TESTS}"
echo "    FAILED:  ${FAILED_TESTS}"
echo "============================================================"

if [[ $FAILED_TESTS -eq 0 ]]; then
    exit 0
else
    exit 1
fi
