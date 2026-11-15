#!/usr/bin/env bash
# processor.sh - Print processing command for ippeveprinter
#
# Reads the document stream to complete the print job.
# Supports coordinated fault injection by emitting CUPS filter protocol
# state messages (STATE: +<reason>-error / STATE: -<reason>-error)
# based on active configuration in fault_state.json.
#
# ippeveprinter transitions the IPP job to 'completed' with reason
# 'job-completed-successfully' when this command exits 0.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAULT_STATE="${EMU_FAULT_STATE_FILE:-${SCRIPT_DIR}/fault_state.json}"
JOB_NAME="${IPP_JOB_NAME:-unknown}"
USER_NAME="${IPP_JOB_ORIGINATING_USER_NAME:-unknown}"
CONTENT_TYPE="${CONTENT_TYPE:-unknown}"

emit_fault_reasons() {
    # Synchronize IPP printer-state-reasons based on configured fault state:
    # Always clear previous fault reasons first, then emit active reasons from fault state.
    # In ippeveprinter, each 'STATE: -<reason>-error' line explicitly strips '-' and clears the bit.
    echo 'STATE: -other-error' >&2
    echo 'STATE: -cover-open-error' >&2
    echo 'STATE: -input-tray-missing-error' >&2
    echo 'STATE: -marker-supply-empty-error' >&2
    echo 'STATE: -marker-supply-low-error' >&2
    echo 'STATE: -marker-waste-almost-full-error' >&2
    echo 'STATE: -marker-waste-full-error' >&2
    echo 'STATE: -media-empty-error' >&2
    echo 'STATE: -media-jam-error' >&2
    echo 'STATE: -media-low-error' >&2
    echo 'STATE: -media-needed-error' >&2
    echo 'STATE: -moving-to-paused-error' >&2
    echo 'STATE: -paused-error' >&2
    echo 'STATE: -spool-area-full-error' >&2
    echo 'STATE: -toner-empty-error' >&2
    echo 'STATE: -toner-low-error' >&2

    if [[ -f "$FAULT_STATE" ]]; then
        if grep -q '"media-jam"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +media-jam-error' >&2
        fi
        if grep -q '"cover-open"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +cover-open-error' >&2
        fi
        if grep -q '"media-empty"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +media-empty-error' >&2
        fi
        if grep -q '"input-tray-missing"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +input-tray-missing-error' >&2
        fi
        if grep -q '"toner-empty"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +toner-empty-error' >&2
        fi
        if grep -q '"paused"' "$FAULT_STATE" 2>/dev/null; then
            echo 'STATE: +paused-error' >&2
        fi
    fi
}

emit_fault_reasons

# Track produced print artifact in spool directory manifest
if [[ -n "${1:-}" ]]; then
    spool_dir="$(dirname "$1")"
    base_file="$(basename "$1")"
    manifest="${spool_dir}/.xeroxgo_emulator_manifest"
    if [[ -d "$spool_dir" ]]; then
        echo "$base_file" >> "$manifest"
        # ippeveprinter also creates the corresponding .prn output file for command output
        echo "${base_file%.*}.prn" >> "$manifest"
    fi
fi

# Test-only job execution controls (D4b-1)
# Allows tests to synchronize fault injection with a confirmed active job rather than fixed sleep assumptions.
IPP_JOB_NUM="${IPP_JOB_ID:-unknown}"

write_active_marker() {
    local target_state="$1"
    if [[ -n "${EMU_JOB_ACTIVE_FILE:-}" ]] && [[ "$JOB_NAME" != "XeroxGoFaultSync" ]] && [[ "$USER_NAME" != "XeroxGoFaultSync" ]]; then
        local marker_dir tmp_marker
        marker_dir="$(dirname "$EMU_JOB_ACTIVE_FILE")"
        if [[ -d "$marker_dir" ]]; then
            tmp_marker="${marker_dir}/.job_active_tmp_${$}_${RANDOM}"
            cat << STATE_EOF > "$tmp_marker"
JOB_NAME=${JOB_NAME}
IPP_JOB_ID=${IPP_JOB_NUM}
USER_NAME=${USER_NAME}
PID=$$
STATE=${target_state}
TIMESTAMP=$(date +%s%N)
STATE_EOF
            mv -f "$tmp_marker" "$EMU_JOB_ACTIVE_FILE"
        fi
    fi
}

cleanup_active_marker() {
    write_active_marker "completed"
}
trap cleanup_active_marker EXIT INT TERM

if [[ "$JOB_NAME" != "XeroxGoFaultSync" ]] && [[ "$USER_NAME" != "XeroxGoFaultSync" ]]; then
    # 1. Announce active job processing with authoritative job-specific marker written atomically
    write_active_marker "processing"

    # 2. If a gate file is set and exists, hold execution until release file is created (with safety timeout)
    if [[ -n "${EMU_JOB_GATE_FILE:-}" ]] && [[ -f "$EMU_JOB_GATE_FILE" ]]; then
        release_file="${EMU_JOB_RELEASE_FILE:-${EMU_JOB_GATE_FILE}.release}"
        max_wait_tenths=${EMU_GATE_TIMEOUT_TENTHS:-50}
        elapsed=0
        while [[ ! -f "$release_file" ]] && [[ $elapsed -lt $max_wait_tenths ]]; do
            emit_fault_reasons
            sleep 0.1
            elapsed=$((elapsed + 1))
        done
    fi
fi

# Simulate printing delay if printing scenario is active and this is not an internal sync job
if [[ "$JOB_NAME" != "XeroxGoFaultSync" ]] && [[ "$USER_NAME" != "XeroxGoFaultSync" ]] && [[ -f "$FAULT_STATE" ]]; then
    if grep -q '"hold_job": true' "$FAULT_STATE" 2>/dev/null || grep -q '"scenario": "printing"' "$FAULT_STATE" 2>/dev/null; then
        sleep 2
    fi
fi

# Log completion details to stderr (captured by ippeveprinter log)
echo "[PRINT-PROCESSOR] Job completed successfully: name='${JOB_NAME}' user='${USER_NAME}' format='${CONTENT_TYPE}'" >&2
cleanup_active_marker

exit 0
