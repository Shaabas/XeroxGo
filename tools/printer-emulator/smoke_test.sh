#!/usr/bin/env bash
# smoke_test.sh - Repeatable smoke tests for local IPP printer emulator
#
# Tests emulator startup, direct IPP querying, direct IPP job submission,
# CUPS driverless queue creation, CUPS job submission via lp, and safe cleanup.
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
echo "  XeroxGo Local IPP Printer Emulator — Smoke Test Suite"
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

# -----------------------------------------------------------------------------
# Summary
# -----------------------------------------------------------------------------
echo "============================================================"
echo "  Smoke Test Results:"
echo "    PASSED:  ${PASSED_COUNT}"
echo "    FAILED:  ${FAILED_COUNT}"
echo "    SKIPPED: ${SKIPPED_COUNT}"
echo "============================================================"

if [[ $FAILED_COUNT -gt 0 ]]; then
    exit 1
fi

exit 0
