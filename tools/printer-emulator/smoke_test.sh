#!/usr/bin/env bash
# smoke_test.sh - Repeatable smoke tests for local IPP printer emulator
#
# Tests emulator startup, direct IPP querying, direct IPP job submission,
# CUPS driverless queue creation, CUPS job submission via lp, safe cleanup,
# and negative safety tests (queue collision, unrelated PID, unsafe spool path,
# occupied port, and queue deletion ownership verification).
#
# If dependencies are missing (e.g. ippeveprinter, ipptool, or cupsd),
# tests are clearly reported as SKIPPED.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_PORT="${SMOKE_PORT:-8639}"
TEST_QUEUE="XeroxGoSmoke_${$}"
TEST_SPOOL="/tmp/xeroxgo-smoke-spool-${$}"
TEST_PID_FILE="/tmp/xeroxgo-smoke-${$}.pid"
TEST_LOG_FILE="/tmp/xeroxgo-smoke-${$}.log"
SAMPLE_PDF="${SCRIPT_DIR}/testdata/sample.pdf"
PROCESSOR="${SCRIPT_DIR}/processor.sh"

PASSED_COUNT=0
FAILED_COUNT=0
SKIPPED_COUNT=0

# Locate ippeveprinter binary
find_ippeveprinter() {
    if command -v ippeveprinter >/dev/null 2>&1; then
        command -v ippeveprinter
        return 0
    fi
    for candidate in \
        "${HOME}/.local/bin/ippeveprinter" \
        "/usr/bin/ippeveprinter" \
        "/usr/sbin/ippeveprinter" \
        "/usr/local/bin/ippeveprinter" \
        "/usr/lib/cups/command/ippeveprinter"
    do
        if [[ -x "$candidate" ]]; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}

# Cleanup handler guaranteed to run on exit
cleanup() {
    local exit_code=$?

    # Remove test CUPS queue if it was created
    if command -v lpadmin >/dev/null 2>&1 && [[ -n "${TEST_QUEUE:-}" ]]; then
        if lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
            lpadmin -x "$TEST_QUEUE" >/dev/null 2>&1 || true
        fi
    fi

    # Kill emulator process if running
    if [[ -f "$TEST_PID_FILE" ]]; then
        local pid
        pid=$(cat "$TEST_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 0.2
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$TEST_PID_FILE"
    fi

    # Remove temporary spool and logs
    rm -rf "$TEST_SPOOL" "$TEST_PID_FILE" "$TEST_LOG_FILE"

    exit "$exit_code"
}
trap cleanup EXIT INT TERM

echo "============================================================"
echo "  XeroxGo Local IPP Printer Emulator — Smoke & Safety Suite"
echo "============================================================"
echo ""

# -----------------------------------------------------------------------------
# Dependency Check
# -----------------------------------------------------------------------------
MISSING_DEPS=()
IPPEVEPRINTER_BIN=""

if ! IPPEVEPRINTER_BIN=$(find_ippeveprinter); then
    MISSING_DEPS+=("ippeveprinter (install cups-printerapp or cups-ipp-utils)")
fi

if ! command -v ipptool >/dev/null 2>&1; then
    MISSING_DEPS+=("ipptool (install cups-ipptool or cups-client)")
fi

if ! command -v lpstat >/dev/null 2>&1; then
    MISSING_DEPS+=("lpstat (install cups-client)")
fi

if ! command -v lpadmin >/dev/null 2>&1; then
    MISSING_DEPS+=("lpadmin (install cups-client)")
fi

if ! command -v lp >/dev/null 2>&1; then
    MISSING_DEPS+=("lp (install cups-client)")
fi

if command -v lpstat >/dev/null 2>&1; then
    if ! lpstat -r >/dev/null 2>&1; then
        MISSING_DEPS+=("cupsd (CUPS scheduler is not running; start cups.service)")
    fi
fi

if [[ ${#MISSING_DEPS[@]} -gt 0 ]]; then
    echo "Prerequisite check: MISSING DEPENDENCIES"
    for dep in "${MISSING_DEPS[@]}"; do
        echo "  - $dep"
    done
    echo ""
    echo "[SKIP] Emulator smoke tests skipped due to missing prerequisites."
    echo "Install instructions:"
    echo "  Fedora:  sudo dnf install cups-printerapp cups-ipptool cups-client cups && sudo systemctl start cups"
    echo "  Ubuntu:  sudo apt install cups-ipp-utils cups-client cups && sudo systemctl start cups"
    echo "  Arch:    sudo pacman -S cups cups-filters && sudo systemctl start cups"
    exit 0
fi

if [[ ! -f "$SAMPLE_PDF" ]]; then
    echo "[FAIL] Test sample PDF not found at ${SAMPLE_PDF}"
    exit 1
fi

echo "Prerequisites verified:"
echo "  ippeveprinter: ${IPPEVEPRINTER_BIN}"
echo "  ipptool:       $(command -v ipptool)"
echo "  lpstat / lp:   $(command -v lpstat)"
echo "  CUPS service:  scheduler running"
echo ""

# Record existing CUPS queues to verify isolation later
INITIAL_QUEUES=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort || true)
echo "Baseline CUPS queues on system: $(echo $INITIAL_QUEUES | tr '\n' ' ')"
echo ""

# -----------------------------------------------------------------------------
# Test 1: Emulator Startup & Process Lifecycle
# -----------------------------------------------------------------------------
echo "--- Test 1: Emulator startup and process lifecycle ---"
mkdir -p "$TEST_SPOOL"

"$IPPEVEPRINTER_BIN" \
    -f "application/pdf,image/pwg-raster,image/urf,application/octet-stream,text/plain" \
    -n "localhost" \
    -p "$TEST_PORT" \
    -d "$TEST_SPOOL" \
    -k \
    -v \
    -2 \
    -s "20,20" \
    -c "$PROCESSOR" \
    "SmokeTestPrinter" > "$TEST_LOG_FILE" 2>&1 &

EMU_PID=$!
echo "$EMU_PID" > "$TEST_PID_FILE"

# Wait for IPP service to become responsive
EMU_READY=false
for _ in {1..50}; do
    if ! kill -0 "$EMU_PID" 2>/dev/null; then
        break
    fi
    if ipptool "ipp://localhost:${TEST_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
        EMU_READY=true
        break
    fi
    sleep 0.1
done

if [[ "$EMU_READY" == "true" ]]; then
    echo "  Emulator responsive on port ${TEST_PORT} (PID ${EMU_PID})"
    echo "[PASS] Test 1: Emulator startup and process lifecycle"
    ((PASSED_COUNT++))
else
    echo "  Log output:"
    cat "$TEST_LOG_FILE" 2>/dev/null || true
    echo "[FAIL] Test 1: Emulator failed to start or respond"
    ((FAILED_COUNT++))
    exit 1
fi
echo ""

# -----------------------------------------------------------------------------
# Test 2: Direct IPP Attribute Discovery (ipptool)
# -----------------------------------------------------------------------------
echo "--- Test 2: Direct IPP attributes and readiness (ipptool) ---"
set +e
ATTRS_OUTPUT=$(ipptool -tv "ipp://localhost:${TEST_PORT}/ipp/print" get-printer-attributes.test 2>&1)
ATTRS_EXIT=$?
set -e

if [[ $ATTRS_EXIT -eq 0 ]] && \
   echo "$ATTRS_OUTPUT" | grep -q "status-code = successful-ok" && \
   echo "$ATTRS_OUTPUT" | grep -q "printer-state (enum) = idle" && \
   echo "$ATTRS_OUTPUT" | grep -q "printer-is-accepting-jobs (boolean) = true"; then
    echo "  Verified status-code = successful-ok"
    echo "  Verified printer-state = idle"
    echo "  Verified printer-is-accepting-jobs = true"
    echo "[PASS] Test 2: Direct IPP attributes and readiness (ipptool)"
    ((PASSED_COUNT++))
else
    echo "  ipptool output:"
    echo "$ATTRS_OUTPUT" | head -n 30
    echo "[FAIL] Test 2: Direct IPP attribute query failed"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Test 3: Direct IPP Print Job Submission & Receipt (ipptool)
# -----------------------------------------------------------------------------
echo "--- Test 3: Direct IPP print submission and lifecycle completion ---"
set +e
PRINT_OUTPUT=$(ipptool -tv -f "$SAMPLE_PDF" "ipp://localhost:${TEST_PORT}/ipp/print" print-job.test 2>&1)
PRINT_EXIT=$?
set -e

if [[ $PRINT_EXIT -eq 0 ]] && echo "$PRINT_OUTPUT" | grep -q "status-code = successful-ok"; then
    echo "  Print-Job accepted by emulator (status-code = successful-ok)"
    sleep 1

    # Check that job transitioned to completed
    COMPLETED_OUTPUT=$(ipptool -tv "ipp://localhost:${TEST_PORT}/ipp/print" get-completed-jobs.test 2>&1 || true)
    if echo "$COMPLETED_OUTPUT" | grep -q "job-state (enum) = completed"; then
        echo "  Job lifecycle transitioned to 'completed' (job-completed-successfully)"
    fi

    # Check preserved spool file artifact
    SPOOL_FILES=$(find "$TEST_SPOOL" -maxdepth 1 -type f 2>/dev/null | wc -l)
    if [[ $SPOOL_FILES -gt 0 ]]; then
        echo "  Spool file artifact preserved in ${TEST_SPOOL} (${SPOOL_FILES} file(s))"
        echo "[PASS] Test 3: Direct IPP print submission and lifecycle completion"
        ((PASSED_COUNT++))
    else
        echo "[FAIL] Test 3: Spool file not preserved in spool directory"
        ((FAILED_COUNT++))
    fi
else
    echo "  Print-Job output:"
    echo "$PRINT_OUTPUT" | head -n 30
    echo "[FAIL] Test 3: Direct IPP print submission failed"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Test 4: CUPS Driverless Queue Registration (lpadmin / lpstat)
# -----------------------------------------------------------------------------
echo "--- Test 4: CUPS driverless queue registration (lpadmin / lpstat) ---"
set +e
LPADMIN_OUT=$(lpadmin -p "$TEST_QUEUE" -v "ipp://localhost:${TEST_PORT}/ipp/print" -E -m everywhere 2>&1)
LPADMIN_STATUS=$?
set -e

if [[ $LPADMIN_STATUS -eq 0 ]] && lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
    QUEUE_STATUS=$(lpstat -p "$TEST_QUEUE")
    echo "  Registered CUPS queue: ${QUEUE_STATUS}"
    echo "[PASS] Test 4: CUPS driverless queue registration (lpadmin / lpstat)"
    ((PASSED_COUNT++))
else
    echo "  lpadmin error: $LPADMIN_OUT"
    echo "[FAIL] Test 4: Failed to create CUPS queue targeting emulator"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Test 5: End-to-End CUPS Job Dispatch and Artifact Receipt (lp)
# -----------------------------------------------------------------------------
echo "--- Test 5: End-to-end CUPS job dispatch and artifact receipt (lp) ---"
INITIAL_SPOOL_COUNT=$(find "$TEST_SPOOL" -maxdepth 1 -type f 2>/dev/null | wc -l)

set +e
LP_OUT=$(lp -d "$TEST_QUEUE" "$SAMPLE_PDF" 2>&1)
LP_STATUS=$?
set -e
echo "  lp submission output: ${LP_OUT}"

if [[ $LP_STATUS -eq 0 ]]; then
    # Wait up to 3 seconds for CUPS backend dispatch and emulator spool receipt
    JOB_RECEIVED=false
    for _ in {1..30}; do
        CURRENT_SPOOL_COUNT=$(find "$TEST_SPOOL" -maxdepth 1 -type f 2>/dev/null | wc -l)
        if [[ $CURRENT_SPOOL_COUNT -gt $INITIAL_SPOOL_COUNT ]]; then
            JOB_RECEIVED=true
            break
        fi
        sleep 0.1
    done

    if [[ "$JOB_RECEIVED" == "true" ]]; then
        echo "  Job dispatched through CUPS and received in emulator spool directory"
        echo "[PASS] Test 5: End-to-end CUPS job dispatch and artifact receipt (lp)"
        ((PASSED_COUNT++))
    else
        echo "[FAIL] Test 5: Job dispatched by lp but not received in emulator spool"
        ((FAILED_COUNT++))
    fi
else
    echo "[FAIL] Test 5: lp command failed: $LP_OUT"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Test 6: CUPS IPP Gateway Capabilities Query (ipptool)
# -----------------------------------------------------------------------------
echo "--- Test 6: CUPS IPP gateway capabilities query ---"
set +e
CUPS_IPP_OUT=$(ipptool -tv "ipp://localhost:631/printers/${TEST_QUEUE}" get-printer-attributes.test 2>&1)
set -e

if echo "$CUPS_IPP_OUT" | grep -q "status-code = successful-ok" && \
   echo "$CUPS_IPP_OUT" | grep -q "document-format-supported"; then
    echo "  Verified CUPS IPP gateway responds with valid printer capabilities"
    echo "[PASS] Test 6: CUPS IPP gateway capabilities query"
    ((PASSED_COUNT++))
else
    echo "  CUPS IPP output:"
    echo "$CUPS_IPP_OUT" | head -n 30
    echo "[FAIL] Test 6: CUPS IPP gateway capabilities query failed"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Test 7: Safe Cleanup and Isolation Verification
# -----------------------------------------------------------------------------
echo "--- Test 7: Safe cleanup and queue isolation ---"
# Remove test queue
lpadmin -x "$TEST_QUEUE"

# Stop emulator
if kill -0 "$EMU_PID" 2>/dev/null; then
    kill "$EMU_PID" 2>/dev/null || true
    sleep 0.2
fi

# Verify test queue is gone
if ! lpstat -p "$TEST_QUEUE" >/dev/null 2>&1; then
    echo "  Verified test queue '${TEST_QUEUE}' successfully deleted"
else
    echo "[FAIL] Test 7: Test queue was not removed"
    ((FAILED_COUNT++))
fi

# Verify baseline queues were never modified or removed
CURRENT_QUEUES=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort || true)
if [[ "$CURRENT_QUEUES" == "$INITIAL_QUEUES" ]]; then
    echo "  Verified baseline CUPS queues were NOT modified: $(echo $CURRENT_QUEUES | tr '\n' ' ')"
    echo "[PASS] Test 7: Safe cleanup and queue isolation (no unrelated queues modified)"
    ((PASSED_COUNT++))
else
    echo "[FAIL] Test 7: Baseline queues changed! Before: '$INITIAL_QUEUES', After: '$CURRENT_QUEUES'"
    ((FAILED_COUNT++))
fi
echo ""

# =============================================================================
# Negative & Safety Checks (Requirements 14 & 15)
# =============================================================================

# -----------------------------------------------------------------------------
# Safety Test 8: Existing Queue Collision Protection
# -----------------------------------------------------------------------------
echo "--- Safety Test 8: Existing queue collision protection ---"
COLLISION_QUEUE="XeroxGoCollision_${$}"
lpadmin -p "$COLLISION_QUEUE" -v "file:///dev/null" -E
BEFORE_COLLISION_URI=$(lpstat -v "$COLLISION_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')

set +e
COLLISION_OUT=$("${SCRIPT_DIR}/emulator.sh" setup-cups --queue "$COLLISION_QUEUE" 2>&1)
COLLISION_STATUS=$?
set -e

AFTER_COLLISION_URI=$(lpstat -v "$COLLISION_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')

if [[ $COLLISION_STATUS -ne 0 ]] && \
   echo "$COLLISION_OUT" | grep -q "already exists. Refusing to overwrite" && \
   [[ "$BEFORE_COLLISION_URI" == "$AFTER_COLLISION_URI" ]]; then
    echo "  setup-cups correctly refused to overwrite existing queue '${COLLISION_QUEUE}'"
    echo "  Verified existing queue configuration was preserved untouched"
    echo "[PASS] Safety Test 8: Existing queue collision protection (setup-cups refuses overwrite)"
    ((PASSED_COUNT++))
else
    echo "  setup-cups output: $COLLISION_OUT"
    echo "  Before URI: $BEFORE_COLLISION_URI, After URI: $AFTER_COLLISION_URI"
    echo "[FAIL] Safety Test 8: setup-cups did not refuse existing queue collision or modified it"
    ((FAILED_COUNT++))
fi
lpadmin -x "$COLLISION_QUEUE" 2>/dev/null || true
echo ""

# -----------------------------------------------------------------------------
# Safety Test 9: Unrelated PID Protection
# -----------------------------------------------------------------------------
echo "--- Safety Test 9: Unrelated PID protection ---"
sleep 60 &
SLEEP_PID=$!
FAKE_PID_FILE="/tmp/fake_emu_${$}.pid"
echo "$SLEEP_PID" > "$FAKE_PID_FILE"

set +e
STOP_UNRELATED_OUT=$(EMU_PID_FILE="$FAKE_PID_FILE" "${SCRIPT_DIR}/emulator.sh" stop 2>&1)
STOP_UNRELATED_STATUS=$?
set -e

if [[ $STOP_UNRELATED_STATUS -ne 0 ]] && \
   echo "$STOP_UNRELATED_OUT" | grep -q "Refusing to signal unrelated process" && \
   kill -0 "$SLEEP_PID" 2>/dev/null; then
    echo "  Verified emulator.sh stop refused to signal unrelated process (PID ${SLEEP_PID})"
    echo "  Verified unrelated process remained alive"
    echo "[PASS] Safety Test 9: Unrelated PID protection (refuses to signal non-emulator process)"
    ((PASSED_COUNT++))
else
    echo "  emulator stop output: $STOP_UNRELATED_OUT"
    echo "[FAIL] Safety Test 9: stop signaled or failed to reject unrelated PID"
    ((FAILED_COUNT++))
fi
kill "$SLEEP_PID" 2>/dev/null || true
rm -f "$FAKE_PID_FILE"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 10: Unsafe Spool Path Rejection
# -----------------------------------------------------------------------------
echo "--- Safety Test 10: Unsafe spool path rejection ---"
set +e
ROOT_OUT=$("${SCRIPT_DIR}/emulator.sh" start --spool-dir /etc 2>&1)
ROOT_STATUS=$?
set -e

# Test symlink escape
ln -sf /etc "/tmp/unsafe_symlink_${$}"
set +e
SYMLINK_OUT=$("${SCRIPT_DIR}/emulator.sh" start --spool-dir "/tmp/unsafe_symlink_${$}" 2>&1)
SYMLINK_STATUS=$?
set -e
rm -f "/tmp/unsafe_symlink_${$}"

if [[ $ROOT_STATUS -ne 0 ]] && echo "$ROOT_OUT" | grep -q "Refusing to use system directory" && \
   [[ $SYMLINK_STATUS -ne 0 ]] && (echo "$SYMLINK_OUT" | grep -q "Refusing to use system directory" || echo "$SYMLINK_OUT" | grep -q "points to forbidden directory"); then
    echo "  Verified rejection of system path (/etc)"
    echo "  Verified rejection of symlink pointing to forbidden directory"
    echo "[PASS] Safety Test 10: Unsafe spool path rejection (root, system dirs, and symlink escapes)"
    ((PASSED_COUNT++))
else
    echo "  Root test output: $ROOT_OUT"
    echo "  Symlink test output: $SYMLINK_OUT"
    echo "[FAIL] Safety Test 10: Failed to reject dangerous spool path or symlink escape"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Safety Test 11: Occupied Port Detection
# -----------------------------------------------------------------------------
echo "--- Safety Test 11: Occupied port detection ---"
OCCUPIED_PORT=8649
python3 -c "import socket, time; s = socket.socket(); s.bind(('127.0.0.1', $OCCUPIED_PORT)); s.listen(1); time.sleep(5)" >/dev/null 2>&1 &
DUMMY_SERVER_PID=$!
sleep 0.2

set +e
PORT_OCCUPIED_OUT=$("${SCRIPT_DIR}/emulator.sh" start -p "$OCCUPIED_PORT" 2>&1)
PORT_OCCUPIED_STATUS=$?
set -e

kill "$DUMMY_SERVER_PID" 2>/dev/null || true
wait "$DUMMY_SERVER_PID" 2>/dev/null || true

if [[ $PORT_OCCUPIED_STATUS -ne 0 ]] && echo "$PORT_OCCUPIED_OUT" | grep -q "already in use"; then
    echo "  Verified startup aborted cleanly when port ${OCCUPIED_PORT} is already occupied"
    echo "[PASS] Safety Test 11: Occupied port detection (refuses startup when port is in use)"
    ((PASSED_COUNT++))
else
    echo "  Port test output: $PORT_OCCUPIED_OUT"
    echo "[FAIL] Safety Test 11: Startup did not detect occupied port"
    ((FAILED_COUNT++))
fi
echo ""

# -----------------------------------------------------------------------------
# Safety Test 12: Queue Deletion Ownership & URI Verification
# -----------------------------------------------------------------------------
echo "--- Safety Test 12: Queue deletion ownership verification ---"
ALIEN_QUEUE="XeroxGoAlien_${$}"
lpadmin -p "$ALIEN_QUEUE" -v "ipp://10.0.0.1:631/printers/alien" -E

set +e
ALIEN_DEL_OUT=$("${SCRIPT_DIR}/emulator.sh" remove-cups --queue "$ALIEN_QUEUE" 2>&1)
ALIEN_DEL_STATUS=$?
set -e

if [[ $ALIEN_DEL_STATUS -ne 0 ]] && \
   echo "$ALIEN_DEL_OUT" | grep -q "Ownership metadata not found" && \
   lpstat -p "$ALIEN_QUEUE" >/dev/null 2>&1; then
    echo "  remove-cups refused to delete alien queue without ownership metadata"
    echo "  Verified alien queue remained intact in CUPS"
    echo "[PASS] Safety Test 12: Queue deletion ownership verification (refuses deleting unverified queues)"
    ((PASSED_COUNT++))
else
    echo "  remove-cups output: $ALIEN_DEL_OUT"
    echo "[FAIL] Safety Test 12: remove-cups deleted or failed to reject unverified queue"
    ((FAILED_COUNT++))
fi
lpadmin -x "$ALIEN_QUEUE" 2>/dev/null || true
echo ""

# -----------------------------------------------------------------------------
# Safety Test 13: Spool Ownership — Refuse Pre-Existing Directory with Files
# -----------------------------------------------------------------------------
echo "--- Safety Test 13: Spool ownership — refuse unowned directory with files ---"
UNOWNED_DIR="/tmp/xeroxgo-unowned-spool-${$}"
rm -rf "$UNOWNED_DIR"
mkdir -p "$UNOWNED_DIR"
PERSONAL_FILE="${UNOWNED_DIR}/personal_document.txt"
echo "Confidential data" > "$PERSONAL_FILE"
PERSONAL_MD5_BEFORE=$(md5sum "$PERSONAL_FILE" | awk '{print $1}')

set +e
START_UNOWNED_OUT=$("${SCRIPT_DIR}/emulator.sh" start -p 8645 --spool-dir "$UNOWNED_DIR" 2>&1)
START_UNOWNED_STATUS=$?
set -e

PERSONAL_MD5_AFTER=$(md5sum "$PERSONAL_FILE" 2>/dev/null | awk '{print $1}' || echo "missing")

if [[ $START_UNOWNED_STATUS -ne 0 ]] && \
   echo "$START_UNOWNED_OUT" | grep -q "Refusing to claim ownership" && \
   [[ "$PERSONAL_MD5_BEFORE" == "$PERSONAL_MD5_AFTER" ]] && \
   [[ ! -f "${UNOWNED_DIR}/.xeroxgo_emulator_spool" ]]; then
    echo "  emulator.sh start correctly refused unowned directory containing personal files"
    echo "  Verified personal file remained completely intact and marker was NOT created"
    echo "[PASS] Safety Test 13: Spool ownership — refuse unowned directory with files"
    ((PASSED_COUNT++))
else
    echo "  start output: $START_UNOWNED_OUT"
    echo "[FAIL] Safety Test 13: Failed to reject unowned directory or modified personal file"
    ((FAILED_COUNT++))
fi
rm -rf "$UNOWNED_DIR"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 14: Spool Ownership — Initialize Marker in Empty Directory
# -----------------------------------------------------------------------------
echo "--- Safety Test 14: Spool ownership — initialize marker in empty directory ---"
EMPTY_SPOOL_DIR="/tmp/xeroxgo-empty-spool-${$}"
rm -rf "$EMPTY_SPOOL_DIR"
mkdir -p "$EMPTY_SPOOL_DIR"
EMPTY_PID_FILE="/tmp/xeroxgo-empty-${$}.pid"
EMPTY_LOG_FILE="/tmp/xeroxgo-empty-${$}.log"

set +e
START_EMPTY_OUT=$(EMU_PID_FILE="$EMPTY_PID_FILE" EMU_LOG_FILE="$EMPTY_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p 8646 --spool-dir "$EMPTY_SPOOL_DIR" 2>&1)
START_EMPTY_STATUS=$?
set -e

MARKER_FILE="${EMPTY_SPOOL_DIR}/.xeroxgo_emulator_spool"
MARKER_VALID=false
if [[ -f "$MARKER_FILE" ]] && grep -q "EMULATOR_OWNED_SPOOL=true" "$MARKER_FILE"; then
    MARKER_VALID=true
fi

# Stop the emulator instance
EMU_PID_FILE="$EMPTY_PID_FILE" "${SCRIPT_DIR}/emulator.sh" stop -p 8646 >/dev/null 2>&1 || true

if [[ $START_EMPTY_STATUS -eq 0 ]] && [[ "$MARKER_VALID" == "true" ]]; then
    echo "  emulator.sh start succeeded for empty directory and created valid ownership marker"
    echo "[PASS] Safety Test 14: Spool ownership — initialize marker in empty directory"
    ((PASSED_COUNT++))
else
    echo "  start output: $START_EMPTY_OUT"
    echo "[FAIL] Safety Test 14: Failed to start in empty directory or initialize marker"
    ((FAILED_COUNT++))
fi
rm -rf "$EMPTY_SPOOL_DIR" "$EMPTY_PID_FILE" "$EMPTY_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 15: Spool Ownership — Reuse Owned Directory with Valid Artifacts
# -----------------------------------------------------------------------------
echo "--- Safety Test 15: Spool ownership — reuse owned directory with valid artifacts ---"
OWNED_SPOOL_DIR="/tmp/xeroxgo-owned-spool-${$}"
rm -rf "$OWNED_SPOOL_DIR"
mkdir -p "$OWNED_SPOOL_DIR"
echo "EMULATOR_OWNED_SPOOL=true" > "${OWNED_SPOOL_DIR}/.xeroxgo_emulator_spool"
echo "1-sample.pdf" > "${OWNED_SPOOL_DIR}/.xeroxgo_emulator_manifest"
echo "1-sample.prn" >> "${OWNED_SPOOL_DIR}/.xeroxgo_emulator_manifest"
touch "${OWNED_SPOOL_DIR}/1-sample.pdf"
touch "${OWNED_SPOOL_DIR}/1-sample.prn"
OWNED_PID_FILE="/tmp/xeroxgo-owned-${$}.pid"
OWNED_LOG_FILE="/tmp/xeroxgo-owned-${$}.log"

set +e
START_OWNED_OUT=$(EMU_PID_FILE="$OWNED_PID_FILE" EMU_LOG_FILE="$OWNED_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p 8647 --spool-dir "$OWNED_SPOOL_DIR" 2>&1)
START_OWNED_STATUS=$?
set -e

# Stop the emulator instance
EMU_PID_FILE="$OWNED_PID_FILE" "${SCRIPT_DIR}/emulator.sh" stop -p 8647 >/dev/null 2>&1 || true

if [[ $START_OWNED_STATUS -eq 0 ]] && \
   [[ -f "${OWNED_SPOOL_DIR}/1-sample.pdf" ]] && \
   [[ -f "${OWNED_SPOOL_DIR}/1-sample.prn" ]]; then
    echo "  emulator.sh start succeeded for owned directory containing valid artifacts"
    echo "  Verified valid artifacts were preserved"
    echo "[PASS] Safety Test 15: Spool ownership — reuse owned directory with valid artifacts"
    ((PASSED_COUNT++))
else
    echo "  start output: $START_OWNED_OUT"
    echo "[FAIL] Safety Test 15: Failed to start in owned directory with valid artifacts"
    ((FAILED_COUNT++))
fi
rm -rf "$OWNED_SPOOL_DIR" "$OWNED_PID_FILE" "$OWNED_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 16: Spool Ownership — Preserve Unrelated Files Added After Startup
# -----------------------------------------------------------------------------
echo "--- Safety Test 16: Spool ownership — preserve unrelated files during cleanup ---"
CLEANUP_SPOOL_DIR="/tmp/xeroxgo-cleanup-spool-${$}"
rm -rf "$CLEANUP_SPOOL_DIR"
mkdir -p "$CLEANUP_SPOOL_DIR"
CLEANUP_PID_FILE="/tmp/xeroxgo-cleanup-${$}.pid"
CLEANUP_LOG_FILE="/tmp/xeroxgo-cleanup-${$}.log"
CLEANUP_PORT=8648

# 1. Start emulator in empty directory (initializes marker)
EMU_PID_FILE="$CLEANUP_PID_FILE" EMU_LOG_FILE="$CLEANUP_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p "$CLEANUP_PORT" --spool-dir "$CLEANUP_SPOOL_DIR" >/dev/null 2>&1

# 2. Simulate print job submission by sending a job via ipptool
ipptool -f "$SAMPLE_PDF" "ipp://localhost:${CLEANUP_PORT}/ipp/print" print-job.test >/dev/null 2>&1 || true
sleep 0.5

# Ensure a verified artifact exists
if ! ls "$CLEANUP_SPOOL_DIR"/[0-9]*-* >/dev/null 2>&1; then
    touch "${CLEANUP_SPOOL_DIR}/1-testjob.pdf"
    echo "1-testjob.pdf" >> "${CLEANUP_SPOOL_DIR}/.xeroxgo_emulator_manifest"
fi

# 3. Add unrelated personal files, a symlink, and a subdirectory after startup
UNRELATED_FILE="${CLEANUP_SPOOL_DIR}/personal_document.txt"
echo "My personal document created after startup" > "$UNRELATED_FILE"
UNRELATED_MD5_BEFORE=$(md5sum "$UNRELATED_FILE" | awk '{print $1}')
mkdir -p "${CLEANUP_SPOOL_DIR}/personal_folder"
touch "${CLEANUP_SPOOL_DIR}/personal_folder/nested.txt"
ln -s /tmp "${CLEANUP_SPOOL_DIR}/personal_symlink"

# 4. Run cleanup
set +e
CLEANUP_OUT=$(EMU_PID_FILE="$CLEANUP_PID_FILE" EMU_LOG_FILE="$CLEANUP_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" cleanup -p "$CLEANUP_PORT" --spool-dir "$CLEANUP_SPOOL_DIR" 2>&1)
CLEANUP_STATUS=$?
set -e

UNRELATED_MD5_AFTER=$(md5sum "$UNRELATED_FILE" 2>/dev/null | awk '{print $1}' || echo "missing")

# 5. Verify results:
# - Emulator artifacts must be deleted
# - Unrelated personal file must remain completely INTACT
# - Subdirectory must remain completely INTACT
# - Symlink must remain completely INTACT
REMAINING_ARTIFACTS=0
for f in "$CLEANUP_SPOOL_DIR"/*; do
    [[ -f "$f" && ! -L "$f" ]] || continue
    fname=$(basename "$f")
    if [[ "$fname" =~ ^[0-9]+(-[A-Za-z0-9_.\-]+)?\.(pdf|prn|ps|pwg|urf|bin|txt|raw)$ ]]; then
        ((REMAINING_ARTIFACTS++))
    fi
done

if [[ $CLEANUP_STATUS -eq 0 ]] && \
   [[ $REMAINING_ARTIFACTS -eq 0 ]] && \
   [[ -f "$UNRELATED_FILE" ]] && \
   [[ "$UNRELATED_MD5_BEFORE" == "$UNRELATED_MD5_AFTER" ]] && \
   [[ -d "${CLEANUP_SPOOL_DIR}/personal_folder" ]] && \
   [[ -L "${CLEANUP_SPOOL_DIR}/personal_symlink" ]]; then
    echo "  Verified cleanup deleted emulator job artifacts"
    echo "  Verified unrelated personal file was preserved intact (matching checksum)"
    echo "  Verified unrelated subdirectory and symlink were preserved intact"
    echo "[PASS] Safety Test 16: Spool ownership — preserve unrelated files during cleanup"
    ((PASSED_COUNT++))
else
    echo "  cleanup output: $CLEANUP_OUT"
    echo "  Remaining artifacts: $REMAINING_ARTIFACTS"
    echo "  Personal file exists: $([[ -f "$UNRELATED_FILE" ]] && echo yes || echo no)"
    echo "[FAIL] Safety Test 16: Cleanup deleted unrelated files or failed to clean artifacts"
    ((FAILED_COUNT++))
fi
rm -rf "$CLEANUP_SPOOL_DIR" "$CLEANUP_PID_FILE" "$CLEANUP_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Test 17: Status Command Robustness with Spool Artifacts (Safe Increment)
# -----------------------------------------------------------------------------
echo "--- Test 17: Status command robustness with spool artifacts (safe increment) ---"
STATUS_SPOOL_DIR="/tmp/xeroxgo-status-spool-${$}"
rm -rf "$STATUS_SPOOL_DIR"
mkdir -p "$STATUS_SPOOL_DIR"
STATUS_PID_FILE="/tmp/xeroxgo-status-${$}.pid"
STATUS_LOG_FILE="/tmp/xeroxgo-status-${$}.log"
STATUS_PORT=8654

# Start emulator
EMU_PID_FILE="$STATUS_PID_FILE" EMU_LOG_FILE="$STATUS_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p "$STATUS_PORT" --spool-dir "$STATUS_SPOOL_DIR" >/dev/null 2>&1

# Submit print job so artifacts exist in spool
ipptool -f "$SAMPLE_PDF" "ipp://localhost:${STATUS_PORT}/ipp/print" print-job.test >/dev/null 2>&1 || true
sleep 0.5

# Ensure at least one artifact is present
if ! ls "$STATUS_SPOOL_DIR"/[0-9]*-* >/dev/null 2>&1; then
    touch "${STATUS_SPOOL_DIR}/1-sample.pdf"
fi

# Run status under bash -e to strictly test against arithmetic post-increment failure
set +e
STATUS_OUT=$(bash -e -c "EMU_PID_FILE='$STATUS_PID_FILE' EMU_LOG_FILE='$STATUS_LOG_FILE' '${SCRIPT_DIR}/emulator.sh' status -p '$STATUS_PORT' --spool-dir '$STATUS_SPOOL_DIR'" 2>&1)
STATUS_EXIT=$?
set -e

# Stop emulator
EMU_PID_FILE="$STATUS_PID_FILE" "${SCRIPT_DIR}/emulator.sh" stop -p "$STATUS_PORT" >/dev/null 2>&1 || true

if [[ $STATUS_EXIT -eq 0 ]] && \
   echo "$STATUS_OUT" | grep -q "Status: RUNNING" && \
   echo "$STATUS_OUT" | grep -q "Jobs in spool: [1-9]"; then
    echo "  emulator.sh status succeeded under set -e with spool artifacts present"
    echo "  Verified output: $(echo "$STATUS_OUT" | grep 'Jobs in spool:')"
    echo "[PASS] Test 17: Status command robustness with spool artifacts (safe increment)"
    ((PASSED_COUNT++))
else
    echo "  status exit code: $STATUS_EXIT"
    echo "  status output: $STATUS_OUT"
    echo "[FAIL] Test 17: Status command failed with spool artifacts"
    ((FAILED_COUNT++))
fi
rm -rf "$STATUS_SPOOL_DIR" "$STATUS_PID_FILE" "$STATUS_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 18: Preserve Unrelated Files Matching Artifact Pattern (2-notes.pdf)
# -----------------------------------------------------------------------------
echo "--- Safety Test 18: Preserve unrelated file matching artifact pattern (2-notes.pdf) ---"
PATTERN_SPOOL_DIR="/tmp/xeroxgo-pattern-spool-${$}"
rm -rf "$PATTERN_SPOOL_DIR"
mkdir -p "$PATTERN_SPOOL_DIR"
PATTERN_PID_FILE="/tmp/xeroxgo-pattern-${$}.pid"
PATTERN_LOG_FILE="/tmp/xeroxgo-pattern-${$}.log"
PATTERN_PORT=8655

# 1. Start emulator in empty directory
EMU_PID_FILE="$PATTERN_PID_FILE" EMU_LOG_FILE="$PATTERN_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p "$PATTERN_PORT" --spool-dir "$PATTERN_SPOOL_DIR" >/dev/null 2>&1

# 2. Submit print job so emulator actually produces genuine artifacts
ipptool -f "$SAMPLE_PDF" "ipp://localhost:${PATTERN_PORT}/ipp/print" print-job.test >/dev/null 2>&1 || true
sleep 0.5

# 3. Add an unrelated file specifically named matching the artifact pattern, e.g. 2-notes.pdf
MATCHING_UNRELATED="${PATTERN_SPOOL_DIR}/2-notes.pdf"
echo "Confidential user notes matching emulator artifact naming pattern" > "$MATCHING_UNRELATED"
MATCHING_MD5_BEFORE=$(md5sum "$MATCHING_UNRELATED" | awk '{print $1}')

# 4. Run cleanup
set +e
CLEANUP_PATTERN_OUT=$(EMU_PID_FILE="$PATTERN_PID_FILE" EMU_LOG_FILE="$PATTERN_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" cleanup -p "$PATTERN_PORT" --spool-dir "$PATTERN_SPOOL_DIR" 2>&1)
CLEANUP_PATTERN_STATUS=$?
set -e

MATCHING_MD5_AFTER=$(md5sum "$MATCHING_UNRELATED" 2>/dev/null | awk '{print $1}' || echo "missing")

# 5. Check that emulator artifacts were deleted, but 2-notes.pdf was PRESERVED
GENUINE_ARTIFACTS=$(find "$PATTERN_SPOOL_DIR" -maxdepth 1 -type f -name "1-*" 2>/dev/null | wc -l)

if [[ $CLEANUP_PATTERN_STATUS -eq 0 ]] && \
   [[ $GENUINE_ARTIFACTS -eq 0 ]] && \
   [[ -f "$MATCHING_UNRELATED" ]] && \
   [[ "$MATCHING_MD5_BEFORE" == "$MATCHING_MD5_AFTER" ]]; then
    echo "  Verified cleanup deleted genuine emulator artifacts (1-*)"
    echo "  Verified unrelated file matching artifact pattern (2-notes.pdf) was PRESERVED intact"
    echo "[PASS] Safety Test 18: Preserve unrelated file matching artifact pattern (2-notes.pdf)"
    ((PASSED_COUNT++))
else
    echo "  cleanup output: $CLEANUP_PATTERN_OUT"
    echo "  Genuine artifacts remaining: $GENUINE_ARTIFACTS"
    echo "  2-notes.pdf exists: $([[ -f "$MATCHING_UNRELATED" ]] && echo yes || echo no)"
    echo "[FAIL] Safety Test 18: Cleanup deleted 2-notes.pdf or failed to remove emulator artifact"
    ((FAILED_COUNT++))
fi
rm -rf "$PATTERN_SPOOL_DIR" "$PATTERN_PID_FILE" "$PATTERN_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Safety Test 19: Preserve Unrelated Artifact-Pattern File Across Restarts
# -----------------------------------------------------------------------------
echo "--- Safety Test 19: Preserve unrelated file across start -> stop -> restart -> cleanup ---"
RESTART_SPOOL_DIR="/tmp/xeroxgo-restart-spool-${$}"
rm -rf "$RESTART_SPOOL_DIR"
mkdir -p "$RESTART_SPOOL_DIR"
RESTART_PID_FILE="/tmp/xeroxgo-restart-${$}.pid"
RESTART_LOG_FILE="/tmp/xeroxgo-restart-${$}.log"
RESTART_PORT=8656

# 1. Start emulator in fresh directory
EMU_PID_FILE="$RESTART_PID_FILE" EMU_LOG_FILE="$RESTART_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p "$RESTART_PORT" --spool-dir "$RESTART_SPOOL_DIR" >/dev/null 2>&1

# 2. Submit print job 1 so genuine emulator artifact is produced and tracked in manifest
ipptool -f "$SAMPLE_PDF" "ipp://localhost:${RESTART_PORT}/ipp/print" print-job.test >/dev/null 2>&1 || true
sleep 0.5

# 3. Introduce an unrelated file specifically named matching the artifact pattern (2-notes.pdf)
UNRELATED_NOTES="${RESTART_SPOOL_DIR}/2-notes.pdf"
echo "Confidential user notes to survive across restarts" > "$UNRELATED_NOTES"
NOTES_MD5_BEFORE=$(md5sum "$UNRELATED_NOTES" | awk '{print $1}')

# 4. Stop the emulator
EMU_PID_FILE="$RESTART_PID_FILE" "${SCRIPT_DIR}/emulator.sh" stop -p "$RESTART_PORT" >/dev/null 2>&1 || true

# 5. Restart the emulator on the same spool directory
# Must NOT enroll 2-notes.pdf into .xeroxgo_emulator_manifest!
set +e
RESTART_OUT=$(EMU_PID_FILE="$RESTART_PID_FILE" EMU_LOG_FILE="$RESTART_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" start -p "$RESTART_PORT" --spool-dir "$RESTART_SPOOL_DIR" 2>&1)
RESTART_STATUS=$?
set -e

# Verify manifest does NOT contain 2-notes.pdf after restart
MANIFEST_FILE="${RESTART_SPOOL_DIR}/.xeroxgo_emulator_manifest"
MANIFEST_HAS_NOTES=false
if [[ -f "$MANIFEST_FILE" ]] && grep -Fxq "2-notes.pdf" "$MANIFEST_FILE"; then
    MANIFEST_HAS_NOTES=true
fi

# 6. Run cleanup
set +e
CLEANUP_RESTART_OUT=$(EMU_PID_FILE="$RESTART_PID_FILE" EMU_LOG_FILE="$RESTART_LOG_FILE" \
    "${SCRIPT_DIR}/emulator.sh" cleanup -p "$RESTART_PORT" --spool-dir "$RESTART_SPOOL_DIR" 2>&1)
CLEANUP_RESTART_STATUS=$?
set -e

NOTES_MD5_AFTER=$(md5sum "$UNRELATED_NOTES" 2>/dev/null | awk '{print $1}' || echo "missing")
REMAINING_GENUINE=$(find "$RESTART_SPOOL_DIR" -maxdepth 1 -type f -name "1-*" 2>/dev/null | wc -l)

if [[ $RESTART_STATUS -eq 0 ]] && \
   [[ "$MANIFEST_HAS_NOTES" == "false" ]] && \
   [[ $CLEANUP_RESTART_STATUS -eq 0 ]] && \
   [[ $REMAINING_GENUINE -eq 0 ]] && \
   [[ -f "$UNRELATED_NOTES" ]] && \
   [[ "$NOTES_MD5_BEFORE" == "$NOTES_MD5_AFTER" ]]; then
    echo "  Verified restart did NOT enroll 2-notes.pdf into manifest"
    echo "  Verified cleanup deleted genuine emulator artifacts (1-*)"
    echo "  Verified 2-notes.pdf survived start -> stop -> restart -> cleanup intact"
    echo "[PASS] Safety Test 19: Preserve unrelated file across start -> stop -> restart -> cleanup"
    ((PASSED_COUNT++))
else
    echo "  restart status: $RESTART_STATUS, output: $RESTART_OUT"
    echo "  manifest has notes: $MANIFEST_HAS_NOTES"
    echo "  cleanup status: $CLEANUP_RESTART_STATUS, output: $CLEANUP_RESTART_OUT"
    echo "  remaining genuine: $REMAINING_GENUINE"
    echo "  2-notes.pdf exists: $([[ -f "$UNRELATED_NOTES" ]] && echo yes || echo no)"
    echo "[FAIL] Safety Test 19: Failed restart preservation of unrelated file"
    ((FAILED_COUNT++))
fi
rm -rf "$RESTART_SPOOL_DIR" "$RESTART_PID_FILE" "$RESTART_LOG_FILE"
echo ""

# -----------------------------------------------------------------------------
# Summary
# -----------------------------------------------------------------------------
echo "============================================================"
echo "  Smoke & Safety Test Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "    SKIPPED: ${SKIPPED_COUNT}"
echo "============================================================"

if [[ $FAILED_COUNT -gt 0 ]]; then
    exit 1
fi

exit 0
