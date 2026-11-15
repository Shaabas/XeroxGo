#!/usr/bin/env bash
# processor.sh - Print processing command for ippeveprinter
#
# Reads the document stream from stdin to complete the print job.
# ippeveprinter transitions the IPP job to 'completed' with reason
# 'job-completed-successfully' when this command exits 0.

set -euo pipefail

# Read and discard the document data from stdin
cat > /dev/null

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

# Log completion details to stderr (captured by ippeveprinter log)
job_name="${IPP_JOB_NAME:-unknown}"
user_name="${IPP_JOB_ORIGINATING_USER_NAME:-unknown}"
content_type="${CONTENT_TYPE:-unknown}"
echo "[PRINT-PROCESSOR] Job completed successfully: name='${job_name}' user='${user_name}' format='${content_type}'" >&2

exit 0
