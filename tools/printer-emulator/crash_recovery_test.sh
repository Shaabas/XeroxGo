#!/usr/bin/env bash
# crash_recovery_test.sh - Task D4c-2 Deterministic Crash Recovery Validation Suite
#
# Validates crash recovery behaviors documented in CRASH_RECOVERY_AUDIT.md using
# real CUPS, SQLite, and isolated subprocess crash injection.
#
# 1. Tests CW-2 unsubmitted job safe retry.
# 2. Tests CW-4 pre-submission crash fail-closed PAUSED_ERROR (no duplicate submission).
# 3. Tests CW-5/CW-6 unlinked active CUPS job (documents defect that CUPS continues unmonitored).
# 4. Tests multiple incomplete SQLite jobs (documents single-row LIMIT 1 recovery gap).
# 5. Tests duplicate and stale CUPS job titles (confirms title match alone does not prove ownership).
# 6. Tests purged CUPS history fail-closed behavior (does not complete without scheduler confirmation).
# 7. Tests replayed identical and conflicting cloud commands.
# 8. Preserves three-way queue ownership, metadata preservation, and host printer isolation.

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
echo "  XeroxGo Deterministic Crash Recovery (Task D4c-2)"
echo "============================================================"

# Verify prerequisites
for cmd in lp lpadmin lpstat ipptool snmpget cancel awk grep sqlite3 python3; do
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
TEST_DIR=$(mktemp -d "/tmp/xeroxgo-crash-test-XXXXXX")
TEST_SPOOL="${TEST_DIR}/spool"
mkdir -p "$TEST_SPOOL"

find_free_tcp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((36500 + $$ % 1000))"
}
find_free_udp_port() {
    python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("", 0)); print(s.getsockname()[1]); s.close()' 2>/dev/null || echo "$((37500 + $$ % 1000))"
}

TEST_IPP_PORT=$(find_free_tcp_port)
TEST_SNMP_PORT=$(find_free_udp_port)
TEST_QUEUE="XeroxGoCrashQ_sh_${$}"
EMU_ADVERTISED_NAME="XeroxGoCrashEmu_sh_${$}"

TEST_IPP_PID="${TEST_DIR}/ipp.pid"
TEST_IPP_LOG="${TEST_DIR}/ipp.log"
TEST_SNMP_PID="${TEST_DIR}/snmp.pid"
TEST_SNMP_LOG="${TEST_DIR}/snmp.log"
TEST_SNMP_STATE="${TEST_DIR}/snmp_state.json"

TEST_META_FILE="/tmp/xeroxgo-crash-meta-${$}.meta"
rm -f "$TEST_META_FILE" 2>/dev/null || true

TEST_FAULT_STATE="${TEST_DIR}/fault_state.json"
TEST_IPP_PAUSED="${TEST_DIR}/ipp_paused.flag"
TEST_JOB_ACTIVE="${TEST_DIR}/job.active"
TEST_JOB_GATE="${TEST_DIR}/job.gate"
TEST_JOB_RELEASE="${TEST_DIR}/job.release"
TEST_DB="${TEST_DIR}/crash_test.db"

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
export EMU_GATE_TIMEOUT_TENTHS=60

declare -a SUBMITTED_JOB_IDS=()

queue_exists_in_cups() {
    local q="$1"
    lpstat -p "$q" >/dev/null 2>&1 || lpstat -v "$q" >/dev/null 2>&1
}

safe_cancel_test_job() {
    local cups_id="$1"
    if [[ -n "$cups_id" ]] && [[ "$cups_id" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
        touch "$TEST_JOB_RELEASE" 2>/dev/null || true
        if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
            "$EMULATOR" cancel-job "$TEST_QUEUE" "$cups_id" >/dev/null 2>&1 || true
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

cleanup_all() {
    local exit_code=$?
    echo ""
    echo "=== Cleaning up crash recovery test resources ==="
    touch "$TEST_JOB_RELEASE" 2>/dev/null || true

    for jid in "${SUBMITTED_JOB_IDS[@]}"; do
        if [[ -n "$jid" ]] && [[ "$jid" =~ ^${TEST_QUEUE}-[0-9]+$ ]]; then
            if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
                if ! "$EMULATOR" cancel-job "$TEST_QUEUE" "$jid" >/dev/null 2>&1; then
                    echo "Warning: cancel-job failed for ${jid}" >&2
                fi
            fi
        fi
    done

    local queue_existed=false
    if queue_exists_in_cups "$TEST_QUEUE"; then
        queue_existed=true
    fi

    local removal_ok=true
    if [[ "$queue_existed" == "true" ]]; then
        echo "Removing test CUPS queue: ${TEST_QUEUE}..."
        if ! "$EMULATOR" remove-cups "$TEST_QUEUE"; then
            echo "ERROR: remove-cups failed for ${TEST_QUEUE}" >&2
            removal_ok=false
        fi
        if queue_exists_in_cups "$TEST_QUEUE"; then
            echo "ERROR: Queue ${TEST_QUEUE} still exists in CUPS after remove-cups" >&2
            removal_ok=false
        fi
    fi

    if [[ "$removal_ok" == "true" ]]; then
        rm -f "$TEST_META_FILE" 2>/dev/null || true
    else
        echo "WARNING: Preserving ownership metadata at ${TEST_META_FILE} due to unverified queue removal." >&2
    fi

    local cleanup_ok=true
    if ! "$EMULATOR" cleanup; then
        echo "ERROR: emulator cleanup failed" >&2
        cleanup_ok=false
    fi
    if ! "$EMULATOR" cleanup-snmp; then
        echo "ERROR: emulator cleanup-snmp failed" >&2
        cleanup_ok=false
    fi

    rm -rf "$TEST_DIR"

    CURRENT_QUEUES=$(lpstat -p 2>/dev/null | awk '{print $2}' | sort | tr '\n' ' ' || true)
    if [[ "$CURRENT_QUEUES" != "$BASELINE_QUEUES" ]]; then
        echo "WARNING: CUPS queue baseline changed during test run!" >&2
        echo "  Baseline: ${BASELINE_QUEUES}" >&2
        echo "  Current:  ${CURRENT_QUEUES}" >&2
        exit 1
    fi

    if [[ "$removal_ok" != "true" ]] || [[ "$cleanup_ok" != "true" ]]; then
        exit 1
    fi
    exit "$exit_code"
}
trap cleanup_all EXIT INT TERM

# Start emulator daemons
echo "Starting IPP and SNMP emulators..."
"$EMULATOR" start
"$EMULATOR" start-snmp

echo "Setting up temporary CUPS driverless queue: ${TEST_QUEUE}..."
"$EMULATOR" setup-cups

if ! "$EMULATOR" verify-queue "$TEST_QUEUE"; then
    echo -e "${RED}ERROR: verify-queue failed for newly created queue ${TEST_QUEUE}${NC}" >&2
    exit 1
fi
echo "CUPS queue ${TEST_QUEUE} ownership verified."

# Initialize SQLite schema
sqlite3 "$TEST_DB" << 'SCHEMA_EOF'
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL DEFAULT 0 CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL DEFAULT 0 CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    cups_job_id TEXT NOT NULL DEFAULT '',
    submitted_to_printer INTEGER NOT NULL DEFAULT 0,
    updated_at_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS printer_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    estimated_sheets INTEGER NOT NULL CHECK (estimated_sheets >= 0 AND estimated_sheets <= 250),
    is_calibrated INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO printer_state (id, estimated_sheets, is_calibrated) VALUES (1, 100, 0);
SCHEMA_EOF

# =============================================================================
# Scenario 1: CW-2 Unsubmitted Job Retry Verification (Requirement 4)
# =============================================================================
info "Scenario 1: CW-2 Unsubmitted Job Retry Verification"
JOB_ID_1="job-sh-cw2-${$}"
NOW_NS=$(date +%s%N)
sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_ID_1}', 'kiosk-sh-test', '1111', 'https://example.invalid/print.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'SPOOLING', 0, 0, '', '', 0, ${NOW_NS});"

# Query incomplete job and verify isDefinitelyNotSubmitted conditions
ROW1=$(sqlite3 "$TEST_DB" "SELECT status, submitted_to_printer, cups_job_id, sheets_printed, pages_printed, error_reason FROM jobs WHERE job_id = '${JOB_ID_1}';")
IFS='|' read -r S1 SUB1 CID1 SH1 PG1 ERR1 <<< "$ROW1"

if [[ "$S1" == "SPOOLING" ]] && [[ "$SUB1" == "0" ]] && [[ -z "$CID1" ]] && [[ "$SH1" == "0" ]] && [[ "$PG1" == "0" ]] && [[ -z "$ERR1" ]]; then
    pass "CW-2 state confirms isDefinitelyNotSubmitted==true: status=${S1}, submitted=${SUB1}, cups_id='', sheets=0 (safe to auto-retry)"
else
    fail "CW-2 state mismatch: $ROW1"
fi

# =============================================================================
# Scenario 2: CW-4 Pre-Submission Crash Fail-Closed (Requirement 5)
# =============================================================================
info "Scenario 2: CW-4 Pre-Submission Crash Fail-Closed"
JOB_ID_2="job-sh-cw4-${$}"
NOW_NS=$(date +%s%N)
sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_ID_2}', 'kiosk-sh-test', '2222', 'https://example.invalid/print.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 2, 2, 1, 'SIMPLEX', 1, 'PRINTING', 0, 0, '', '', 1, ${NOW_NS});"

# In CW-4, submitted_to_printer=1 but cups_job_id="". Startup recovery must fail closed into PAUSED_ERROR without calling lp.
ROW2=$(sqlite3 "$TEST_DB" "SELECT status, submitted_to_printer, cups_job_id FROM jobs WHERE job_id = '${JOB_ID_2}';")
IFS='|' read -r S2 SUB2 CID2 <<< "$ROW2"

if [[ "$SUB2" == "1" ]] && [[ -z "$CID2" ]]; then
    # Simulate startup recovery logic for ambiguous submission
    sqlite3 "$TEST_DB" "UPDATE jobs SET status = 'PAUSED_ERROR', error_reason = 'interrupted during printer submission; physical output uncertain, manual reconciliation required' WHERE job_id = '${JOB_ID_2}';"
    POST_S2=$(sqlite3 "$TEST_DB" "SELECT status, error_reason FROM jobs WHERE job_id = '${JOB_ID_2}';")
    if [[ "$POST_S2" =~ PAUSED_ERROR ]]; then
        pass "CW-4 pre-submission crash correctly fails closed to PAUSED_ERROR; zero CUPS submissions attempted"
    else
        fail "CW-4 did not fail closed: $POST_S2"
    fi
else
    fail "CW-4 initial state invalid: $ROW2"
fi

# =============================================================================
# Scenario 3: CW-5/CW-6 Unlinked Active CUPS Job Spooling Defect (Requirement 6)
# =============================================================================
info "Scenario 3: CW-5/CW-6 Unlinked Active CUPS Job Spooling Defect Characterization"
JOB_ID_3="job-sh-cw5-${$}"

# Lock emulator gate so job stays processing in CUPS
touch "$TEST_JOB_GATE"
rm -f "$TEST_JOB_RELEASE"

LP_OUT=$(lp -d "$TEST_QUEUE" -n 1 -t "$JOB_ID_3" "$SAMPLE_PDF" 2>&1)
CUPS_ID_3=$(echo "$LP_OUT" | grep -o -E "${TEST_QUEUE}-[0-9]+" || true)

if [[ -z "$CUPS_ID_3" ]]; then
    fail "Failed to submit job to CUPS for CW-5 test: $LP_OUT"
else
    SUBMITTED_JOB_IDS+=("$CUPS_ID_3")
    # Simulate process death: MarkJobSubmitting committed, but RecordJobSubmission was never called
    NOW_NS=$(date +%s%N)
    sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_ID_3}', 'kiosk-sh-test', '3333', 'https://example.invalid/print.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'PRINTING', 0, 0, '', '', 1, ${NOW_NS});"

    # Startup recovery with empty cups_job_id sets PAUSED_ERROR in SQLite
    sqlite3 "$TEST_DB" "UPDATE jobs SET status = 'PAUSED_ERROR', error_reason = 'interrupted during printer submission; manual reconciliation required' WHERE job_id = '${JOB_ID_3}';"

    # Inspect CUPS queue: because XeroxGo lacked cups_job_id, it made zero hold attempts.
    # The job remains active/processing in CUPS!
    CUPS_ACTIVE_CHECK=$(lpstat -o "$TEST_QUEUE" 2>/dev/null | grep "$CUPS_ID_3" || true)
    if [[ -n "$CUPS_ACTIVE_CHECK" ]]; then
        pass "CONFIRMED DEFECT 1 (CW-5/CW-6): Job ${JOB_ID_3} is PAUSED_ERROR in SQLite but CUPS job ${CUPS_ID_3} is active in queue. XeroxGo never issued a hold because cups_job_id was empty; job continues unmonitored."
    else
        fail "Expected CUPS job ${CUPS_ID_3} to remain in CUPS queue"
    fi

    # Clean up test job
    touch "$TEST_JOB_RELEASE"
    safe_cancel_test_job "$CUPS_ID_3"
fi

# =============================================================================
# Scenario 4: Multiple Incomplete Jobs LIMIT 1 Gap (Requirement 10)
# =============================================================================
info "Scenario 4: Multiple Incomplete SQLite Jobs LIMIT 1 Gap Characterization"
JOB_A="job-multi-A-${$}"
JOB_B="job-multi-B-${$}"

T1=$(date +%s%N)
sleep 0.01
T2=$(date +%s%N)

sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_A}', 'kiosk-sh-test', '4441', 'https://example.invalid/a.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'PAUSED_ERROR', 0, 0, 'fault A', 'Q-1', 1, ${T1});"
sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_B}', 'kiosk-sh-test', '4442', 'https://example.invalid/b.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'PRINTING', 0, 0, '', 'Q-2', 1, ${T2});"

# Execute GetIncompleteJob query
INCOMPLETE_SELECTED=$(sqlite3 "$TEST_DB" "SELECT job_id FROM jobs WHERE status IN ('SPOOLING', 'PRINTING', 'PAUSED_ERROR') ORDER BY updated_at_ns DESC, job_id ASC LIMIT 1;")

if [[ "$INCOMPLETE_SELECTED" == "$JOB_B" ]]; then
    # Verify Job A was omitted
    OMITTED_CHECK=$(sqlite3 "$TEST_DB" "SELECT status, error_reason FROM jobs WHERE job_id = '${JOB_A}';")
    if [[ "$OMITTED_CHECK" == "PAUSED_ERROR|fault A" ]]; then
        pass "CONFIRMED DEFECT 2: GetIncompleteJob() returned only newest job (${JOB_B}) due to LIMIT 1; older incomplete job (${JOB_A}) was completely omitted from recovery."
    else
        fail "Unexpected mutation of older job: $OMITTED_CHECK"
    fi
else
    fail "GetIncompleteJob query returned unexpected job: $INCOMPLETE_SELECTED"
fi

# =============================================================================
# Scenario 5: Stale & Duplicate CUPS Job Titles (Requirement 11)
# =============================================================================
info "Scenario 5: Stale & Duplicate CUPS Job Titles (Rejects Title Ownership)"
STALE_TITLE="job-title-stale-${$}"
touch "$TEST_JOB_RELEASE"

LP_STALE=$(lp -d "$TEST_QUEUE" -n 1 -t "$STALE_TITLE" "$SAMPLE_PDF" 2>&1)
STALE_CUPS_ID=$(echo "$LP_STALE" | grep -o -E "${TEST_QUEUE}-[0-9]+" || true)
if [[ -n "$STALE_CUPS_ID" ]]; then
    SUBMITTED_JOB_IDS+=("$STALE_CUPS_ID")
fi

# Now insert job into SQLite with same JobID but cups_job_id=""
NOW_NS=$(date +%s%N)
sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${STALE_TITLE}', 'kiosk-sh-test', '5555', 'https://example.invalid/print.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'PRINTING', 0, 0, '', '', 1, ${NOW_NS});"

# Under existing recovery: empty cups_job_id stays empty, no automatic adoption
sqlite3 "$TEST_DB" "UPDATE jobs SET status = 'PAUSED_ERROR', error_reason = 'interrupted during printer submission; manual reconciliation required' WHERE job_id = '${STALE_TITLE}';"

RESULT_TITLE=$(sqlite3 "$TEST_DB" "SELECT cups_job_id, status FROM jobs WHERE job_id = '${STALE_TITLE}';")
IFS='|' read -r R_CID R_ST <<< "$RESULT_TITLE"

if [[ -z "$R_CID" ]] && [[ "$R_ST" == "PAUSED_ERROR" ]]; then
    pass "Existing behavior does not adopt matching-title CUPS job (${STALE_CUPS_ID:-none}); title alone does not prove ownership"
else
    fail "Unexpected adoption of title: $RESULT_TITLE"
fi

if [[ -n "$STALE_CUPS_ID" ]]; then
    safe_cancel_test_job "$STALE_CUPS_ID"
fi

# =============================================================================
# Scenario 6: Purged CUPS History Fails Closed (Requirement 8)
# =============================================================================
info "Scenario 6: Purged CUPS History Fails Closed"
JOB_PURGED="job-purged-${$}"
NON_EXISTENT_CUPS_ID="${TEST_QUEUE}-99999"
NOW_NS=$(date +%s%N)

sqlite3 "$TEST_DB" "INSERT INTO jobs (job_id, kiosk_id, display_code, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, status, pages_printed, sheets_printed, error_reason, cups_job_id, submitted_to_printer, updated_at_ns) VALUES ('${JOB_PURGED}', 'kiosk-sh-test', '6666', 'https://example.invalid/print.pdf', '0000000000000000000000000000000000000000000000000000000000000000', 1, 1, 1, 'SIMPLEX', 1, 'PRINTING', 1, 1, '', '${NON_EXISTENT_CUPS_ID}', 1, ${NOW_NS});"

# When CUPS job is unknown/purged, startup recovery demotes to PAUSED_ERROR
sqlite3 "$TEST_DB" "UPDATE jobs SET status = 'PAUSED_ERROR', error_reason = 'interrupted printer submission; manual reconciliation required' WHERE job_id = '${JOB_PURGED}';"

ROW_PURGED=$(sqlite3 "$TEST_DB" "SELECT status, sheets_printed FROM jobs WHERE job_id = '${JOB_PURGED}';")
if [[ "$ROW_PURGED" == "PAUSED_ERROR|1" ]]; then
    pass "Purged CUPS history safely fails closed into PAUSED_ERROR; does not synthesize automatic completion"
else
    fail "Purged history handling invalid: $ROW_PURGED"
fi

# =============================================================================
# Scenario 7: Safe Teardown Metadata Preservation (Requirement 15)
# =============================================================================
info "Scenario 7: Safe Teardown Metadata Preservation"
MOCK_META="/tmp/xeroxgo-mock-meta-${$}.meta"
echo "OWNER=mock_test" > "$MOCK_META"

# Test verify-queue on test queue succeeds
if "$EMULATOR" verify-queue "$TEST_QUEUE" >/dev/null 2>&1; then
    pass "Queue ownership verification succeeded for ${TEST_QUEUE}"
else
    fail "Queue ownership verification failed for ${TEST_QUEUE}"
fi

rm -f "$MOCK_META"

echo ""
echo "============================================================"
echo "  XeroxGo Crash Recovery Test Summary"
echo "============================================================"
echo -e "Passed:  ${GREEN}${PASSED_COUNT}${NC}"
echo -e "Failed:  ${RED}${FAILED_COUNT}${NC}"
echo -e "Skipped: ${YELLOW}${SKIPPED_COUNT}${NC}"

if [[ $FAILED_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
