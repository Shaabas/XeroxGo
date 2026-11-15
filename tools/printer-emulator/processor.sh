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

# Simulate printing delay if printing scenario is active and this is not an internal sync job
if [[ "$JOB_NAME" != "XeroxGoFaultSync" ]] && [[ "$USER_NAME" != "XeroxGoFaultSync" ]] && [[ -f "$FAULT_STATE" ]]; then
    if grep -q '"hold_job": true' "$FAULT_STATE" 2>/dev/null || grep -q '"scenario": "printing"' "$FAULT_STATE" 2>/dev/null; then
        sleep 2
    fi
fi

# Log completion details to stderr (captured by ippeveprinter log)
echo "[PRINT-PROCESSOR] Job completed successfully: name='${JOB_NAME}' user='${USER_NAME}' format='${CONTENT_TYPE}'" >&2

exit 0
