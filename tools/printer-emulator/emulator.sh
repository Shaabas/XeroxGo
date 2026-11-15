#!/usr/bin/env bash
# emulator.sh - Control script for local IPP Everywhere printer emulator
#
# Uses CUPS ippeveprinter to emulate an isolated IPP Everywhere printer on localhost.
# Never sends jobs to physical hardware. Dedicated spool directory preserves print artifacts.
# Includes comprehensive safety checks: queue ownership tracking, PID validation,
# spool path verification, and port readiness guarantees.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Configuration with environment variable overrides
EMU_HOST="${EMU_HOST:-localhost}"
EMU_PORT="${EMU_PORT:-8631}"
EMU_NAME="${EMU_NAME:-XeroxGoEmu}"
EMU_CUPS_QUEUE="${EMU_CUPS_QUEUE:-XeroxGoEmu}"
EMU_SPOOL_DIR="${EMU_SPOOL_DIR:-${SCRIPT_DIR}/spool}"
EMU_PID_FILE="${EMU_PID_FILE:-${SCRIPT_DIR}/emulator.pid}"
EMU_LOG_FILE="${EMU_LOG_FILE:-${SCRIPT_DIR}/emulator.log}"
EMU_META_FILE="${EMU_META_FILE:-${SCRIPT_DIR}/cups_queue.meta}"
EMU_LOCK_DIR="${SCRIPT_DIR}/.emulator_startup.lock"
EMU_PROCESSOR="${EMU_PROCESSOR:-${SCRIPT_DIR}/processor.sh}"
EMU_FORMATS="${EMU_FORMATS:-application/pdf,image/pwg-raster,image/urf,application/octet-stream,text/plain,application/postscript}"
EMU_SPEED="${EMU_SPEED:-20,20}"
EMU_DUPLEX="${EMU_DUPLEX:-true}"
SPOOL_MARKER_NAME=".xeroxgo_emulator_spool"
SPOOL_MANIFEST_NAME=".xeroxgo_emulator_manifest"

# SNMP emulator configuration with environment variable overrides
SNMP_HOST="${SNMP_HOST:-127.0.0.1}"
SNMP_PORT="${SNMP_PORT:-1161}"
SNMP_COMMUNITY="${SNMP_COMMUNITY:-public}"
SNMP_PID_FILE="${SNMP_PID_FILE:-${SCRIPT_DIR}/snmp_responder.pid}"
SNMP_LOG_FILE="${SNMP_LOG_FILE:-${SCRIPT_DIR}/snmp_responder.log}"
SNMP_STATE_FILE="${SNMP_STATE_FILE:-${SCRIPT_DIR}/snmp_state.json}"
SNMP_LOCK_DIR="${SCRIPT_DIR}/.snmp_startup.lock"

# Coordinated Fault Controller configuration with environment variable overrides
EMU_FAULT_STATE_FILE="${EMU_FAULT_STATE_FILE:-${SCRIPT_DIR}/fault_state.json}"
EMU_IPP_PAUSED_FILE="${EMU_IPP_PAUSED_FILE:-${SCRIPT_DIR}/ipp_paused.flag}"

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

# Validate spool directory path against root, system directories, and symlink escapes
validate_spool_dir() {
    local dir="$1"
    if [[ -z "${dir// }" ]]; then
        echo "ERROR: Spool directory cannot be empty." >&2
        return 1
    fi

    local target_dir="$dir"
    if [[ -d "$target_dir" ]]; then
        target_dir=$(realpath "$target_dir" 2>/dev/null || readlink -f "$target_dir" 2>/dev/null)
    else
        local parent
        parent=$(dirname "$target_dir")
        if [[ -d "$parent" ]]; then
            local real_parent
            real_parent=$(realpath "$parent" 2>/dev/null || readlink -f "$parent" 2>/dev/null)
            target_dir="${real_parent}/$(basename "$target_dir")"
        else
            echo "ERROR: Parent directory does not exist for spool path '$dir'." >&2
            return 1
        fi
    fi

    # Reject dangerous system roots and user roots
    case "$target_dir" in
        /|/bin|/bin/*|/sbin|/sbin/*|/boot|/boot/*|/dev|/dev/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/root|/root/*|/sys|/sys/*|/usr|/usr/*|/var|/var/log|/var/log/*|/opt|/home|/tmp)
            echo "ERROR: Refusing to use system directory '$target_dir' as spool directory." >&2
            return 1
            ;;
        "$HOME")
            echo "ERROR: Refusing to use user home root '$target_dir' as spool directory." >&2
            return 1
            ;;
    esac

    # Ensure target is not a symlink pointing to a forbidden directory
    if [[ -L "$dir" ]]; then
        local link_target
        link_target=$(realpath "$dir" 2>/dev/null || true)
        case "$link_target" in
            /|/bin|/bin/*|/sbin|/sbin/*|/boot|/boot/*|/dev|/dev/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/root|/root/*|/sys|/sys/*|/usr|/usr/*|/var|/var/log|/var/log/*|/opt|/home|/tmp|"$HOME")
                echo "ERROR: Spool symlink points to forbidden directory '$link_target'." >&2
                return 1
                ;;
        esac
    fi

    echo "$target_dir"
    return 0
}

# Verify if a filename matches standard IPP emulator print artifacts
# Format: <job-id>-<title>.<ext> or <job-id>.<ext>
# Extensions: pdf, prn, ps, pwg, urf, bin, txt, raw, pcl, ras, dat
is_emulator_artifact() {
    local fname="$1"
    [[ -n "$fname" ]] || return 1
    local regex="^[0-9]+(-[A-Za-z0-9_.\-]+)?\.(pdf|PDF|prn|PRN|ps|PS|pwg|PWG|urf|URF|bin|BIN|txt|TXT|raw|RAW|pcl|PCL|ras|RAS|dat|DAT)$"
    [[ "$fname" =~ $regex ]]
}

# Sync artifacts actually created by emulator into spool manifest
sync_spool_manifest() {
    local spool_dir="$1"
    local manifest="${spool_dir}/${SPOOL_MANIFEST_NAME}"

    if [[ ! -d "$spool_dir" ]]; then
        return 0
    fi

    # Read from emulator log if available
    if [[ -f "$EMU_LOG_FILE" ]]; then
        while IFS= read -r fpath; do
            if [[ -n "$fpath" ]]; then
                local bname
                bname=$(basename "$fpath")
                echo "$bname" >> "$manifest"
            fi
        done < <(sed -n 's/.*Created job file "\([^"]*\)".*/\1/p' "$EMU_LOG_FILE" 2>/dev/null || true)

        while IFS= read -r fpath; do
            if [[ -n "$fpath" ]]; then
                local bname
                bname=$(basename "$fpath")
                echo "$bname" >> "$manifest"
            fi
        done < <(sed -n 's/.*Saving print command output to "\([^"]*\)".*/\1/p' "$EMU_LOG_FILE" 2>/dev/null || true)
    fi

    # Deduplicate manifest safely if it exists
    if [[ -f "$manifest" ]]; then
        sort -u "$manifest" -o "$manifest" 2>/dev/null || true
    fi
}

# Check if a filename is verified as an artifact actually produced by the emulator
is_tracked_artifact() {
    local spool_dir="$1"
    local fname="$2"
    local manifest="${spool_dir}/${SPOOL_MANIFEST_NAME}"

    # Check spool manifest file
    if [[ -f "$manifest" ]] && grep -Fxq "$fname" "$manifest" 2>/dev/null; then
        return 0
    fi

    # Check emulator log file directly as secondary verification
    if [[ -f "$EMU_LOG_FILE" ]]; then
        if grep -q "Created job file \".*/${fname}\"" "$EMU_LOG_FILE" 2>/dev/null || \
           grep -q "Saving print command output to \".*/${fname}\"" "$EMU_LOG_FILE" 2>/dev/null; then
            return 0
        fi
    fi

    return 1
}

# Verify spool directory ownership and initialize or reuse strictly
verify_and_init_spool_dir() {
    local dir="$1"
    local marker="${dir}/${SPOOL_MARKER_NAME}"
    local manifest="${dir}/${SPOOL_MANIFEST_NAME}"

    # Ensure directory exists
    if [[ ! -d "$dir" ]]; then
        mkdir -p "$dir"
    fi

    # Protect against directory being a symlink
    if [[ -L "$dir" ]]; then
        echo "ERROR: Refusing to use symlink '$dir' directly as spool directory." >&2
        return 1
    fi

    if [[ ! -f "$marker" ]]; then
        # Requirement 2 & 3: For unowned directory, require it to be empty before claiming ownership
        local entry fname
        for entry in "$dir"/* "$dir"/.*; do
            [[ -e "$entry" || -L "$entry" ]] || continue
            fname=$(basename "$entry")
            [[ "$fname" != "." && "$fname" != ".." ]] || continue

            # Allow .gitkeep in empty repository directories, reject any other file, directory, or symlink
            if [[ "$fname" != ".gitkeep" ]]; then
                echo "ERROR: Spool directory '$dir' is unowned and not empty (found '$fname')." >&2
                echo "Refusing to claim ownership of existing directory containing unrelated files." >&2
                return 1
            fi
        done

        # Directory is empty (or has only .gitkeep) - initialize ownership marker and manifest
        cat << EOF > "$marker"
EMULATOR_OWNED_SPOOL=true
CREATED_BY=xeroxgo_emulator
CREATED_AT=$(date -u +%FT%TZ)
HOST=${EMU_HOST}
PORT=${EMU_PORT}
EOF
        touch "$manifest"
        return 0
    fi

    # Requirement 4 & 8: Existing directories containing only valid emulator-owned artifacts may be reused
    if ! grep -q "EMULATOR_OWNED_SPOOL=true" "$marker" 2>/dev/null; then
        echo "ERROR: Spool directory ownership marker in '$dir' is invalid or corrupt. Refusing startup." >&2
        return 1
    fi

    # Ensure manifest file exists; treat existing manifest as authoritative
    [[ -f "$manifest" ]] || touch "$manifest"

    local entry fname
    for entry in "$dir"/* "$dir"/.*; do
        [[ -e "$entry" || -L "$entry" ]] || continue
        fname=$(basename "$entry")
        [[ "$fname" != "." && "$fname" != ".." ]] || continue

        # Refuse startup if directory contains symlinks, subdirectories, or special files
        if [[ -L "$entry" ]]; then
            echo "ERROR: Owned spool directory contains unexpected symlink '$fname'. Refusing startup for safety." >&2
            return 1
        fi
        if [[ -d "$entry" ]]; then
            echo "ERROR: Owned spool directory contains unexpected subdirectory '$fname'. Refusing startup for safety." >&2
            return 1
        fi
        if [[ ! -f "$entry" ]]; then
            echo "ERROR: Owned spool directory contains unexpected non-regular file '$fname'. Refusing startup for safety." >&2
            return 1
        fi

        # Marker, manifest, and .gitkeep are valid metadata entries
        if [[ "$fname" == "$SPOOL_MARKER_NAME" || "$fname" == "$SPOOL_MANIFEST_NAME" || "$fname" == ".gitkeep" ]]; then
            continue
        fi

        # Only recognized emulator artifacts are permitted for reuse
        if ! is_emulator_artifact "$fname"; then
            echo "ERROR: Owned spool directory contains unrecognized or foreign file '$fname'. Refusing startup for safety." >&2
            return 1
        fi

        # Do NOT automatically enroll existing files into manifest based on filename pattern alone.
        # The manifest and execution logs remain authoritative for genuine emulator artifacts.
    done

    return 0
}

# Verify that PID belongs to an active ippeveprinter process matching emulator configuration
verify_emulator_process() {
    local pid="$1"
    local expected_port="${2:-}"
    local expected_name="${3:-}"

    if [[ ! "$pid" =~ ^[0-9]+$ ]] || [[ "$pid" -le 0 ]]; then
        return 1
    fi

    if ! kill -0 "$pid" 2>/dev/null; then
        return 1
    fi

    local comm=""
    if [[ -f "/proc/${pid}/comm" ]]; then
        comm=$(cat "/proc/${pid}/comm" 2>/dev/null || true)
    else
        comm=$(ps -p "$pid" -o comm= 2>/dev/null || true)
    fi

    if [[ "$comm" != "ippeveprinter" ]]; then
        return 1
    fi

    if [[ -f "/proc/${pid}/cmdline" ]]; then
        local cmdline
        cmdline=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true)
        if [[ ! "$cmdline" =~ ippeveprinter ]]; then
            return 1
        fi
        if [[ -n "$expected_port" ]] && [[ ! "$cmdline" =~ -p[[:space:]]+"$expected_port" ]] && [[ ! "$cmdline" =~ :"$expected_port" ]]; then
            return 1
        fi
        if [[ -n "$expected_name" ]] && [[ ! "$cmdline" =~ "$expected_name" ]]; then
            return 1
        fi
    fi

    return 0
}

# Check if process is paused in kernel via SIGSTOP
is_process_paused() {
    local pid="$1"
    if [[ ! "$pid" =~ ^[0-9]+$ ]] || [[ ! -d "/proc/${pid}" ]]; then
        return 1
    fi
    local state
    state=$(awk '/^State:/ {print $2}' "/proc/${pid}/status" 2>/dev/null || true)
    if [[ "$state" == "T" ]]; then
        return 0
    fi
    return 1
}

# Check if port is in use
is_port_in_use() {
    local host="$1"
    local port="$2"

    if command -v ss >/dev/null 2>&1; then
        if ss -tln "sport = :$port" 2>/dev/null | grep -q ":$port "; then
            return 0
        fi
    fi

    if command -v lsof >/dev/null 2>&1; then
        if lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
            return 0
        fi
    fi

    if timeout 1 bash -c "cat < /dev/null > /dev/tcp/${host}/${port}" 2>/dev/null; then
        return 0
    fi

    return 1
}

is_running() {
    if [[ -f "$EMU_PID_FILE" ]]; then
        local pid
        pid=$(cat "$EMU_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && verify_emulator_process "$pid" "$EMU_PORT" "$EMU_NAME"; then
            return 0
        fi
    fi
    return 1
}

get_pid() {
    if [[ -f "$EMU_PID_FILE" ]]; then
        local pid
        pid=$(cat "$EMU_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && verify_emulator_process "$pid" "$EMU_PORT" "$EMU_NAME"; then
            echo "$pid"
        fi
    fi
}

cmd_start() {
    # Concurrency lock to prevent race conditions during startup
    if ! mkdir "$EMU_LOCK_DIR" 2>/dev/null; then
        # Check if lock is stale (> 15 seconds)
        local lock_age=0
        if [[ -d "$EMU_LOCK_DIR" ]]; then
            local now_ts lock_ts
            now_ts=$(date +%s)
            lock_ts=$(stat -c %Y "$EMU_LOCK_DIR" 2>/dev/null || echo "$now_ts")
            lock_age=$((now_ts - lock_ts))
        fi
        if [[ $lock_age -gt 15 ]]; then
            rm -rf "$EMU_LOCK_DIR" 2>/dev/null || true
            mkdir "$EMU_LOCK_DIR" 2>/dev/null || {
                echo "ERROR: Unable to acquire startup lock." >&2
                return 1
            }
        else
            echo "ERROR: Another startup operation is currently in progress." >&2
            return 1
        fi
    fi

    # Ensure lock cleanup on exit
    release_lock() {
        rm -rf "$EMU_LOCK_DIR" 2>/dev/null || true
    }

    # Validate spool path safety
    local clean_spool
    if ! clean_spool=$(validate_spool_dir "$EMU_SPOOL_DIR"); then
        release_lock
        return 1
    fi
    EMU_SPOOL_DIR="$clean_spool"

    # Verify spool directory ownership and initialize or validate contents
    if ! verify_and_init_spool_dir "$EMU_SPOOL_DIR"; then
        release_lock
        return 1
    fi

    # Check if already running
    if is_running; then
        local pid
        pid=$(get_pid)
        echo "IPP printer emulator is already running (PID: ${pid}) at ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
        release_lock
        return 0
    fi

    # Clean stale PID file if present
    if [[ -f "$EMU_PID_FILE" ]]; then
        rm -f "$EMU_PID_FILE"
    fi

    # Check if target port is occupied by another service
    if is_port_in_use "$EMU_HOST" "$EMU_PORT"; then
        echo "ERROR: Port ${EMU_PORT} is already in use on ${EMU_HOST}. Refusing startup." >&2
        release_lock
        return 1
    fi

    local ippeveprinter_bin
    if ! ippeveprinter_bin=$(find_ippeveprinter); then
        echo "ERROR: ippeveprinter binary not found." >&2
        echo "Please install CUPS printer-application tools for your distribution:" >&2
        echo "  - Fedora / RHEL: sudo dnf install cups-printerapp" >&2
        echo "  - Debian / Ubuntu: sudo apt-get install cups-ipp-utils" >&2
        echo "  - Arch Linux: sudo pacman -S cups" >&2
        release_lock
        return 1
    fi

    if [[ ! "$EMU_PROCESSOR" = /* ]]; then
        EMU_PROCESSOR="$(pwd)/${EMU_PROCESSOR}"
    fi

    # Assemble ippeveprinter arguments
    local args=(
        "-f" "$EMU_FORMATS"
        "-n" "$EMU_HOST"
        "-p" "$EMU_PORT"
        "-d" "$EMU_SPOOL_DIR"
        "-k"
        "-v"
        "-s" "$EMU_SPEED"
    )

    if [[ "$EMU_DUPLEX" == "true" ]]; then
        args+=("-2")
    fi

    if [[ -x "$EMU_PROCESSOR" ]]; then
        args+=("-c" "$EMU_PROCESSOR")
    fi

    args+=("$EMU_NAME")

    export EMU_FAULT_STATE_FILE

    # Start ippeveprinter in background
    "$ippeveprinter_bin" "${args[@]}" > "$EMU_LOG_FILE" 2>&1 &
    local emu_pid=$!
    echo "$emu_pid" > "$EMU_PID_FILE"

    # Wait up to 5 seconds and verify actual IPP readiness (running PID alone is not success)
    local ready=false
    for _ in {1..50}; do
        if ! kill -0 "$emu_pid" 2>/dev/null; then
            break
        fi
        if command -v ipptool >/dev/null 2>&1; then
            if ipptool "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
                ready=true
                break
            fi
        else
            if grep -q "printer-uri-supported" "$EMU_LOG_FILE" 2>/dev/null; then
                ready=true
                break
            fi
        fi
        sleep 0.1
    done

    if [[ "$ready" != "true" ]]; then
        echo "ERROR: Emulator process started (PID: ${emu_pid}), but IPP endpoint failed readiness verification." >&2
        if [[ -f "$EMU_LOG_FILE" ]]; then
            cat "$EMU_LOG_FILE" >&2
        fi
        # Terminate failed process
        if kill -0 "$emu_pid" 2>/dev/null; then
            kill "$emu_pid" 2>/dev/null || true
        fi
        rm -f "$EMU_PID_FILE"
        release_lock
        return 1
    fi

    echo "IPP printer emulator started (PID: ${emu_pid})"
    echo "  Endpoint:  ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
    echo "  Spool Dir: ${EMU_SPOOL_DIR}"
    echo "  Log File:  ${EMU_LOG_FILE}"

    # Re-apply active fault state if configured
    if [[ -f "$EMU_FAULT_STATE_FILE" ]]; then
        local ipp_off
        ipp_off=$(get_fault_state_field "ipp_offline" "false")
        if [[ "$ipp_off" == "true" ]]; then
            echo "Applying active offline fault to IPP emulator (SIGSTOP)..."
            kill -STOP "$emu_pid" 2>/dev/null || true
            echo "$emu_pid" > "$EMU_IPP_PAUSED_FILE"
        else
            sync_ipp_state "$EMU_HOST" "$EMU_PORT"
        fi
    fi

    release_lock
    return 0
}

cmd_stop() {
    if [[ ! -f "$EMU_PID_FILE" ]]; then
        echo "IPP printer emulator is not running (no PID file)."
        return 0
    fi

    local pid
    pid=$(cat "$EMU_PID_FILE" 2>/dev/null || true)

    if [[ -z "$pid" ]] || [[ ! "$pid" =~ ^[0-9]+$ ]]; then
        echo "Warning: PID file contains malformed PID '${pid}'. Removing invalid PID file."
        rm -f "$EMU_PID_FILE"
        return 0
    fi

    if ! kill -0 "$pid" 2>/dev/null; then
        echo "Notice: Stale PID file (${pid}). Process is already terminated."
        rm -f "$EMU_PID_FILE"
        return 0
    fi

    # Strictly validate that PID belongs to expected ippeveprinter process before signaling
    if ! verify_emulator_process "$pid" "$EMU_PORT" "$EMU_NAME"; then
        echo "ERROR: PID ${pid} does not match running ippeveprinter emulator on port ${EMU_PORT}." >&2
        echo "Refusing to signal unrelated process." >&2
        return 1
    fi

    # If process is paused in kernel via SIGSTOP (e.g. offline fault), resume it so it can handle SIGTERM
    if is_process_paused "$pid" || [[ -f "$EMU_IPP_PAUSED_FILE" ]]; then
        kill -CONT "$pid" 2>/dev/null || true
        rm -f "$EMU_IPP_PAUSED_FILE"
    fi

    echo "Stopping IPP printer emulator (PID: ${pid})..."
    kill "$pid" 2>/dev/null || true

    # Graceful wait for termination
    local terminated=false
    for _ in {1..30}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            terminated=true
            break
        fi
        sleep 0.1
    done

    # Only if still alive after timeout, re-verify PID before sending SIGKILL
    if [[ "$terminated" != "true" ]]; then
        if verify_emulator_process "$pid" "$EMU_PORT" "$EMU_NAME"; then
            echo "Process did not terminate gracefully after timeout; terminating PID ${pid}..."
            kill -9 "$pid" 2>/dev/null || true
        else
            echo "Warning: PID ${pid} changed before forced termination; aborted kill." >&2
        fi
    fi

    rm -f "$EMU_PID_FILE"
    echo "IPP printer emulator stopped."
    return 0
}

cmd_status() {
    if is_running; then
        local pid
        pid=$(get_pid)
        if [[ -n "$pid" ]] && is_process_paused "$pid"; then
            echo "Status: RUNNING (PID: ${pid}, PAUSED/OFFLINE)"
        else
            echo "Status: RUNNING (PID: ${pid})"
        fi
        echo "  Endpoint:  ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
        echo "  Spool Dir: ${EMU_SPOOL_DIR}"
        echo "  Log File:  ${EMU_LOG_FILE}"

        if [[ -d "$EMU_SPOOL_DIR" ]]; then
            local file_count=0
            local entry fname
            for entry in "$EMU_SPOOL_DIR"/*; do
                [[ -f "$entry" && ! -L "$entry" ]] || continue
                fname=$(basename "$entry")
                if is_emulator_artifact "$fname"; then
                    file_count=$((file_count + 1))
                fi
            done
            echo "  Jobs in spool: ${file_count}"
        fi
        return 0
    else
        echo "Status: STOPPED"
        return 1
    fi
}

cmd_setup_cups() {
    if ! command -v lpadmin >/dev/null 2>&1; then
        echo "ERROR: lpadmin command not found. Please install CUPS client tools." >&2
        return 1
    fi

    if ! lpstat -r >/dev/null 2>&1; then
        echo "ERROR: CUPS scheduler is not running. Please start cups service (sudo systemctl start cups)." >&2
        return 1
    fi

    if ! is_running; then
        echo "Notice: Emulator is not running. Starting emulator first..."
        cmd_start
    fi

    local expected_uri="ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"

    # Requirement 2 & 3: Never overwrite an existing CUPS queue; create queues only when unused
    if lpstat -p "$EMU_CUPS_QUEUE" >/dev/null 2>&1 || lpstat -v "$EMU_CUPS_QUEUE" >/dev/null 2>&1; then
        echo "ERROR: CUPS queue '${EMU_CUPS_QUEUE}' already exists. Refusing to overwrite existing queue." >&2
        return 1
    fi

    echo "Configuring driverless CUPS queue '${EMU_CUPS_QUEUE}' targeting ${expected_uri}..."

    # Configure driverless queue with -m everywhere
    if ! lpadmin -p "$EMU_CUPS_QUEUE" -v "$expected_uri" -E -m everywhere 2>&1; then
        echo "ERROR: lpadmin failed to create queue '${EMU_CUPS_QUEUE}'." >&2
        return 1
    fi

    # Verify queue status and actual device URI
    if ! lpstat -p "$EMU_CUPS_QUEUE" >/dev/null 2>&1; then
        echo "ERROR: CUPS queue '${EMU_CUPS_QUEUE}' was not created." >&2
        return 1
    fi

    local actual_uri
    actual_uri=$(lpstat -v "$EMU_CUPS_QUEUE" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')
    if [[ "$actual_uri" != "$expected_uri" ]]; then
        echo "ERROR: Created queue device URI ('$actual_uri') does not match expected URI ('$expected_uri'). Rolling back." >&2
        lpadmin -x "$EMU_CUPS_QUEUE" 2>/dev/null || true
        return 1
    fi

    # Requirement 4: Track emulator-created queues explicitly with expected IPP URI
    cat << EOF > "$EMU_META_FILE"
QUEUE_NAME=${EMU_CUPS_QUEUE}
DEVICE_URI=${expected_uri}
EMULATOR_PORT=${EMU_PORT}
CREATED_AT=$(date -u +%FT%TZ)
EOF

    echo "SUCCESS: CUPS queue '${EMU_CUPS_QUEUE}' is ready and ownership tracked."
    lpstat -p "$EMU_CUPS_QUEUE"
    return 0
}

# Verify queue ownership using metadata, queue name and actual device URI
verify_cups_queue_ownership() {
    local target_queue="$1"
    if [[ ! -f "$EMU_META_FILE" ]]; then
        echo "ERROR: Ownership metadata not found (${EMU_META_FILE})." >&2
        echo "Cannot verify that CUPS queue '${target_queue}' was created by this harness." >&2
        return 1
    fi

    local meta_queue meta_uri
    meta_queue=$(grep -E "^QUEUE_NAME=" "$EMU_META_FILE" | cut -d'=' -f2- || true)
    meta_uri=$(grep -E "^DEVICE_URI=" "$EMU_META_FILE" | cut -d'=' -f2- || true)

    if [[ "$meta_queue" != "$target_queue" ]]; then
        echo "ERROR: Metadata queue name ('${meta_queue}') does not match target queue ('${target_queue}')." >&2
        return 1
    fi

    # Check actual URI in CUPS
    local actual_uri
    actual_uri=$(lpstat -v "$target_queue" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')
    if [[ -z "$actual_uri" ]]; then
        echo "ERROR: CUPS queue '${target_queue}' does not exist in CUPS lpstat." >&2
        return 1
    fi

    if [[ "$actual_uri" != "$meta_uri" ]]; then
        echo "ERROR: CUPS queue '${target_queue}' device URI ('${actual_uri}') does not match expected emulator URI ('${meta_uri}')." >&2
        return 1
    fi

    return 0
}

cmd_verify_queue() {
    local target_queue="${1:-$EMU_CUPS_QUEUE}"
    verify_cups_queue_ownership "$target_queue"
}

cmd_remove_cups() {
    if ! command -v lpadmin >/dev/null 2>&1; then
        return 0
    fi

    local target_queue="${1:-$EMU_CUPS_QUEUE}"
    if [[ -z "$target_queue" ]]; then
        echo "ERROR: Queue name cannot be empty." >&2
        return 1
    fi

    # If queue does not exist in CUPS, clean metadata and return
    if ! lpstat -p "$target_queue" >/dev/null 2>&1 && ! lpstat -v "$target_queue" >/dev/null 2>&1; then
        echo "CUPS queue '${target_queue}' does not exist in CUPS (already removed)."
        rm -f "$EMU_META_FILE"
        return 0
    fi

    # Verify ownership and expected URI before deletion; refuse if cannot be verified
    if ! verify_cups_queue_ownership "$target_queue"; then
        echo "Refusing deletion of unverified CUPS queue '${target_queue}'." >&2
        return 1
    fi

    echo "Removing verified emulator-owned CUPS queue '${target_queue}'..."
    lpadmin -x "$target_queue" 2>/dev/null || true
    rm -f "$EMU_META_FILE"
    echo "CUPS queue '${target_queue}' removed."
    return 0
}

cmd_cleanup() {
    echo "=== Cleaning up emulator resources ==="

    # Remove CUPS queue if metadata exists and points to emulator
    if [[ -f "$EMU_META_FILE" ]]; then
        cmd_remove_cups || {
            echo "Warning: Could not remove CUPS queue during cleanup." >&2
        }
    fi

    # Sync any remaining produced artifacts from emulator log before stopping daemon
    if [[ -d "$EMU_SPOOL_DIR" ]]; then
        local clean_spool
        if clean_spool=$(validate_spool_dir "$EMU_SPOOL_DIR" 2>/dev/null); then
            sync_spool_manifest "$clean_spool"
        fi
    fi

    cmd_stop || true

    # Requirement 3, 4, 5, 6, 7, 8: Validate spool path, verify ownership marker,
    # and selectively remove only verified emulator artifacts actually produced by emulator.
    # Strictly preserves unrelated files (even if matching naming patterns like 2-notes.pdf),
    # subdirectories, symlinks, and metadata.
    if [[ -d "$EMU_SPOOL_DIR" ]]; then
        local clean_spool
        if clean_spool=$(validate_spool_dir "$EMU_SPOOL_DIR"); then
            local marker="${clean_spool}/${SPOOL_MARKER_NAME}"
            local manifest="${clean_spool}/${SPOOL_MANIFEST_NAME}"
            if [[ ! -f "$marker" ]] || ! grep -q "EMULATOR_OWNED_SPOOL=true" "$marker" 2>/dev/null; then
                echo "Warning: Spool directory '${clean_spool}' does not have verified emulator ownership marker." >&2
                echo "Skipping spool file deletion for safety." >&2
            else
                sync_spool_manifest "$clean_spool"
                echo "Cleaning verified emulator spool files in ${clean_spool}..."
                local entry fname
                for entry in "$clean_spool"/* "$clean_spool"/.*; do
                    [[ -e "$entry" || -L "$entry" ]] || continue
                    fname=$(basename "$entry")
                    [[ "$fname" != "." && "$fname" != ".." ]] || continue

                    # Protect symlinks, subdirectories, marker, manifest, and .gitkeep
                    if [[ -L "$entry" ]]; then
                        echo "Notice: Preserving symlink '${fname}' in spool directory."
                        continue
                    fi
                    if [[ -d "$entry" ]]; then
                        echo "Notice: Preserving subdirectory '${fname}' in spool directory."
                        continue
                    fi
                    if [[ ! -f "$entry" ]]; then
                        echo "Notice: Preserving special file '${fname}' in spool directory."
                        continue
                    fi
                    if [[ "$fname" == "$SPOOL_MARKER_NAME" || "$fname" == "$SPOOL_MANIFEST_NAME" || "$fname" == ".gitkeep" ]]; then
                        continue
                    fi

                    # Requirement 3, 4, 5: Filename patterns alone must not authorize deletion.
                    # Delete only if it matches artifact pattern AND is tracked as actually produced by emulator.
                    if is_emulator_artifact "$fname" && is_tracked_artifact "$clean_spool" "$fname"; then
                        rm -f "$entry"
                        echo "  Deleted emulator artifact: ${fname}"
                    else
                        echo "Notice: Preserving non-emulator file '${fname}' in spool directory."
                    fi
                done

                # Clear manifest of deleted artifacts
                if [[ -f "$manifest" ]]; then
                    > "$manifest"
                fi
            fi
        else
            echo "Warning: Spool directory path failed safety validation. Skipping spool cleanup." >&2
        fi
    fi

    rm -f "$EMU_PID_FILE" "$EMU_LOG_FILE" "$EMU_FAULT_STATE_FILE" "$EMU_IPP_PAUSED_FILE"
    rm -rf "$EMU_LOCK_DIR" 2>/dev/null || true

    # Clean up SNMP resources if present
    if is_snmp_running || [[ -f "$SNMP_PID_FILE" ]] || [[ -f "$SNMP_STATE_FILE" ]]; then
        if ! cmd_stop_snmp; then
            echo "ERROR: Failed to safely stop SNMP emulator process during cleanup. Preserving metadata." >&2
            return 1
        fi
        rm -f "$SNMP_PID_FILE" "$SNMP_LOG_FILE" "$SNMP_STATE_FILE"
        rm -rf "$SNMP_LOCK_DIR" 2>/dev/null || true
    fi

    echo "Cleanup complete. Verified emulator resources removed."
}

# -----------------------------------------------------------------------------
# SNMP Emulator Functions
# -----------------------------------------------------------------------------

# Validate that target host interface is loopback
validate_loopback_host() {
    local host="$1"
    local clean
    clean=$(echo "$host" | tr '[:upper:]' '[:lower:]' | xargs)
    if [[ "$clean" == "localhost" || "$clean" == "127.0.0.1" || "$clean" == "::1" || "$clean" =~ ^127\. ]]; then
        return 0
    fi
    echo "ERROR: Safety violation. Host '$host' is not loopback. Only localhost / 127.0.0.1 permitted." >&2
    return 1
}

# Verify that PID belongs to an active snmp_responder process
verify_snmp_process() {
    local pid="$1"
    local expected_port="${2:-}"

    if [[ ! "$pid" =~ ^[0-9]+$ ]] || [[ "$pid" -le 0 ]]; then
        return 1
    fi

    if ! kill -0 "$pid" 2>/dev/null; then
        return 1
    fi

    local comm=""
    if [[ -f "/proc/${pid}/comm" ]]; then
        comm=$(cat "/proc/${pid}/comm" 2>/dev/null || true)
    else
        comm=$(ps -p "$pid" -o comm= 2>/dev/null || true)
    fi

    if [[ "$comm" != "snmp_responder" && "$comm" != "main" ]]; then
        return 1
    fi

    if [[ -f "/proc/${pid}/cmdline" ]]; then
        local cmdline
        cmdline=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true)
        if [[ ! "$cmdline" =~ snmp_responder ]]; then
            return 1
        fi
        if [[ -n "$expected_port" ]] && [[ ! "$cmdline" =~ -port[[:space:]=]+"$expected_port" ]] && [[ ! "$cmdline" =~ :"$expected_port" ]]; then
            return 1
        fi
    fi

    return 0
}

# Check if UDP port is in use
is_udp_port_in_use() {
    local port="$1"

    if command -v ss >/dev/null 2>&1; then
        if ss -uln "sport = :$port" 2>/dev/null | grep -q ":$port "; then
            return 0
        fi
    fi

    if command -v lsof >/dev/null 2>&1; then
        if lsof -iUDP:"$port" >/dev/null 2>&1; then
            return 0
        fi
    fi

    return 1
}

ensure_snmp_responder_binary() {
    local bin="${SCRIPT_DIR}/snmp_responder"
    local src="${SCRIPT_DIR}/snmp_responder.go"
    if [[ ! -x "$bin" || "$src" -nt "$bin" ]]; then
        echo "Building snmp_responder..."
        (cd "$SCRIPT_DIR" && GOWORK=off go build -o snmp_responder snmp_responder.go)
    fi
}

is_snmp_running() {
    if [[ -f "$SNMP_PID_FILE" ]]; then
        local pid
        pid=$(cat "$SNMP_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && verify_snmp_process "$pid" "$SNMP_PORT"; then
            return 0
        fi
    fi
    return 1
}

get_snmp_pid() {
    if [[ -f "$SNMP_PID_FILE" ]]; then
        local pid
        pid=$(cat "$SNMP_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && verify_snmp_process "$pid" "$SNMP_PORT"; then
            echo "$pid"
        fi
    fi
}

cmd_start_snmp() {
    validate_loopback_host "$SNMP_HOST" || return 1

    # Concurrency lock
    if ! mkdir "$SNMP_LOCK_DIR" 2>/dev/null; then
        local lock_age=0
        if [[ -d "$SNMP_LOCK_DIR" ]]; then
            local now_ts lock_ts
            now_ts=$(date +%s)
            lock_ts=$(stat -c %Y "$SNMP_LOCK_DIR" 2>/dev/null || echo "$now_ts")
            lock_age=$((now_ts - lock_ts))
        fi
        if [[ $lock_age -gt 15 ]]; then
            rm -rf "$SNMP_LOCK_DIR" 2>/dev/null || true
            mkdir "$SNMP_LOCK_DIR" 2>/dev/null || {
                echo "ERROR: Unable to acquire SNMP startup lock." >&2
                return 1
            }
        else
            echo "ERROR: Another SNMP startup operation is currently in progress." >&2
            return 1
        fi
    fi

    release_snmp_lock() {
        rm -rf "$SNMP_LOCK_DIR" 2>/dev/null || true
    }

    if is_snmp_running; then
        local pid
        pid=$(get_snmp_pid)
        echo "SNMP printer emulator is already running (PID: ${pid}) on ${SNMP_HOST}:${SNMP_PORT}"
        release_snmp_lock
        return 0
    fi

    # Clean stale PID file if present
    if [[ -f "$SNMP_PID_FILE" ]]; then
        local old_pid
        old_pid=$(cat "$SNMP_PID_FILE" 2>/dev/null || true)
        if [[ -n "$old_pid" ]] && ! kill -0 "$old_pid" 2>/dev/null; then
            rm -f "$SNMP_PID_FILE"
        fi
    fi

    # Check if UDP port is in use
    if is_udp_port_in_use "$SNMP_PORT"; then
        echo "ERROR: UDP port ${SNMP_PORT} is already in use by another process. Refusing startup." >&2
        release_snmp_lock
        return 1
    fi

    ensure_snmp_responder_binary

    local bin="${SCRIPT_DIR}/snmp_responder"
    if [[ ! -x "$bin" ]]; then
        echo "ERROR: snmp_responder binary not found or not executable at $bin" >&2
        release_snmp_lock
        return 1
    fi

    # Launch daemon in background
    nohup "$bin" serve \
        -host "$SNMP_HOST" \
        -port "$SNMP_PORT" \
        -community "$SNMP_COMMUNITY" \
        -state "$SNMP_STATE_FILE" \
        -pid "$SNMP_PID_FILE" \
        </dev/null >> "$SNMP_LOG_FILE" 2>&1 &
    local snmp_pid=$!

    # Wait up to 5 seconds for readiness
    local ready=false
    for _ in {1..50}; do
        if ! kill -0 "$snmp_pid" 2>/dev/null; then
            break
        fi
        if [[ -f "$SNMP_PID_FILE" ]] && is_udp_port_in_use "$SNMP_PORT"; then
            ready=true
            break
        fi
        sleep 0.1
    done

    if [[ "$ready" != "true" ]]; then
        echo "ERROR: SNMP responder started (PID: ${snmp_pid}), but failed readiness verification." >&2
        if [[ -f "$SNMP_LOG_FILE" ]]; then
            cat "$SNMP_LOG_FILE" >&2
        fi
        if kill -0 "$snmp_pid" 2>/dev/null; then
            kill "$snmp_pid" 2>/dev/null || true
        fi
        rm -f "$SNMP_PID_FILE"
        release_snmp_lock
        return 1
    fi

    echo "SNMP printer emulator started (PID: ${snmp_pid})"
    echo "  Endpoint:   ${SNMP_HOST}:${SNMP_PORT} (UDP)"
    echo "  Community:  ${SNMP_COMMUNITY}"
    echo "  State File: ${SNMP_STATE_FILE}"
    echo "  Log File:   ${SNMP_LOG_FILE}"

    release_snmp_lock
    return 0
}

cmd_stop_snmp() {
    if [[ ! -f "$SNMP_PID_FILE" ]]; then
        echo "SNMP printer emulator is not running (no PID file)."
        return 0
    fi

    local pid
    pid=$(cat "$SNMP_PID_FILE" 2>/dev/null || true)

    if [[ -z "$pid" ]] || [[ ! "$pid" =~ ^[0-9]+$ ]]; then
        echo "Warning: PID file contains malformed PID '${pid}'. Removing invalid PID file."
        rm -f "$SNMP_PID_FILE"
        return 0
    fi

    if ! kill -0 "$pid" 2>/dev/null; then
        echo "Notice: Stale PID file (${pid}). Process is already terminated."
        rm -f "$SNMP_PID_FILE"
        return 0
    fi

    if ! verify_snmp_process "$pid" "$SNMP_PORT"; then
        echo "ERROR: PID ${pid} does not match running snmp_responder on port ${SNMP_PORT}." >&2
        echo "Refusing to signal unrelated process." >&2
        return 1
    fi

    echo "Stopping SNMP printer emulator (PID: ${pid})..."
    kill "$pid" 2>/dev/null || true

    local terminated=false
    for _ in {1..30}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            terminated=true
            break
        fi
        sleep 0.1
    done

    if [[ "$terminated" != "true" ]]; then
        if verify_snmp_process "$pid" "$SNMP_PORT"; then
            echo "Process did not terminate gracefully after timeout; terminating PID ${pid}..."
            kill -9 "$pid" 2>/dev/null || true
        else
            echo "Warning: PID ${pid} changed before forced termination; aborted kill." >&2
        fi
    fi

    rm -f "$SNMP_PID_FILE"
    echo "SNMP printer emulator stopped."
    return 0
}

cmd_status_snmp() {
    if is_snmp_running; then
        local pid
        pid=$(get_snmp_pid)
        echo "SNMP Status: RUNNING (PID: ${pid})"
        echo "  Endpoint:   ${SNMP_HOST}:${SNMP_PORT} (UDP)"
        echo "  Community:  ${SNMP_COMMUNITY}"
        echo "  State File: ${SNMP_STATE_FILE}"
        echo "  Log File:   ${SNMP_LOG_FILE}"
        ensure_snmp_responder_binary
        local bin="${SCRIPT_DIR}/snmp_responder"
        if [[ -x "$bin" && -f "$SNMP_STATE_FILE" ]]; then
            echo ""
            "$bin" get -state "$SNMP_STATE_FILE"
        fi
        return 0
    else
        echo "SNMP Status: STOPPED"
        return 1
    fi
}

cmd_cleanup_snmp() {
    if ! cmd_stop_snmp; then
        echo "ERROR: Failed to safely stop SNMP emulator process. Refusing to remove PID or state metadata for safety." >&2
        return 1
    fi
    rm -f "$SNMP_PID_FILE" "$SNMP_LOG_FILE" "$SNMP_STATE_FILE"
    rm -rf "$SNMP_LOCK_DIR" 2>/dev/null || true
    echo "SNMP cleanup complete. Verified emulator resources removed."
    return 0
}

cmd_snmp_set() {
    ensure_snmp_responder_binary
    "${SCRIPT_DIR}/snmp_responder" set -state "$SNMP_STATE_FILE" "$@"
}

cmd_snmp_get() {
    ensure_snmp_responder_binary
    "${SCRIPT_DIR}/snmp_responder" get -state "$SNMP_STATE_FILE" "$@"
}

cmd_snmp_reset() {
    ensure_snmp_responder_binary
    "${SCRIPT_DIR}/snmp_responder" reset -state "$SNMP_STATE_FILE" "$@"
}

# -----------------------------------------------------------------------------
# Coordinated Fault Controller Functions
# -----------------------------------------------------------------------------

verify_ipp_attributes() {
    local host="${1:-$EMU_HOST}"
    local port="${2:-$EMU_PORT}"
    local scenario="${3:-}"

    if [[ -z "$scenario" ]]; then
        scenario=$(get_fault_state_field "ipp_scenario" "healthy")
    fi

    if ! command -v ipptool >/dev/null 2>&1; then
        echo "ERROR: ipptool command not found in PATH." >&2
        return 1
    fi

    local matched=false
    local cur_state="" cur_reasons="" attrs_out=""
    for _ in {1..25}; do
        attrs_out=$(ipptool -tv "ipp://${host}:${port}/ipp/print" get-printer-attributes.test 2>&1) || true
        cur_state=$(echo "$attrs_out" | awk '/printer-state \(enum\)/ {print $NF}')
        cur_reasons=$(echo "$attrs_out" | grep "printer-state-reasons" | sed -E 's/.*=[[:space:]]*//' | tr -d ' ' || true)

        case "$scenario" in
            healthy|recovery|clear)
                if [[ "$cur_reasons" == "none" ]] && [[ "$cur_state" == "idle" || "$cur_state" == "3" ]]; then
                    matched=true
                    break
                fi
                ;;
            paper-jam)
                if [[ "$cur_reasons" =~ media-jam ]]; then
                    matched=true
                    break
                fi
                ;;
            cover-open)
                if [[ "$cur_reasons" =~ cover-open ]]; then
                    matched=true
                    break
                fi
                ;;
            paper-empty)
                if [[ "$cur_reasons" =~ media-empty ]]; then
                    matched=true
                    break
                fi
                ;;
            stopped)
                if [[ "$cur_reasons" =~ paused ]]; then
                    matched=true
                    break
                fi
                ;;
            printing)
                matched=true
                break
                ;;
            offline)
                matched=true
                break
                ;;
            *)
                matched=true
                break
                ;;
        esac
        sleep 0.1
    done

    if [[ "$matched" != "true" ]]; then
        echo "ERROR: IPP attribute verification failed. Expected scenario '${scenario}', but live IPP attributes report reasons='${cur_reasons}', state='${cur_state}'." >&2
        return 1
    fi

    return 0
}

sync_ipp_state() {
    local host="${1:-$EMU_HOST}"
    local port="${2:-$EMU_PORT}"
    local expected_scenario="${3:-}"
    local sample_pdf="${SCRIPT_DIR}/testdata/sample.pdf"

    if ! is_running; then
        echo "ERROR: IPP emulator is not running on ${host}:${port}. Cannot synchronize state." >&2
        return 1
    fi

    local pid
    pid=$(get_pid)
    if [[ -n "$pid" ]] && is_process_paused "$pid"; then
        echo "ERROR: IPP emulator process (PID: ${pid}) is paused via SIGSTOP. Cannot synchronize state." >&2
        return 1
    fi

    if [[ ! -f "$sample_pdf" ]]; then
        echo "ERROR: Sample PDF document not found at '${sample_pdf}'." >&2
        return 1
    fi

    if ! command -v ipptool >/dev/null 2>&1; then
        echo "ERROR: ipptool command not found in PATH." >&2
        return 1
    fi

    local test_file="${SCRIPT_DIR}/sync_probe.test"
    if [[ ! -f "$test_file" ]]; then
        echo "ERROR: IPP sync probe test file '${test_file}' not found." >&2
        return 1
    fi

    local probe_out probe_rc=0
    probe_out=$(ipptool -tv -f "$sample_pdf" "ipp://${host}:${port}/ipp/print" "$test_file" 2>&1) || probe_rc=$?
    if [[ $probe_rc -ne 0 ]]; then
        echo "ERROR: IPP state synchronization probe failed (exit code ${probe_rc}):" >&2
        echo "$probe_out" >&2
        return 1
    fi

    # Requirement 5: Verify actual IPP attributes after injection
    verify_ipp_attributes "$host" "$port" "$expected_scenario"
}

get_fault_state_field() {
    local field="$1"
    local default="${2:-}"
    if [[ ! -f "$EMU_FAULT_STATE_FILE" ]]; then
        echo "$default"
        return 0
    fi
    python3 -c "import json, sys; data = json.load(open(sys.argv[1])); v = data.get(sys.argv[2], sys.argv[3]); print(json.dumps(v) if isinstance(v, (list, dict)) else v)" "$EMU_FAULT_STATE_FILE" "$field" "$default" 2>/dev/null || echo "$default"
}

write_fault_state() {
    local scenario="$1"
    local target="$2"
    local ipp_scen="$3"
    local snmp_scen="$4"
    local ipp_reasons="$5"
    local ipp_offline="$6"
    local snmp_offline="$7"
    local hold_job="$8"

    local ts
    ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

    cat > "$EMU_FAULT_STATE_FILE" << EOF
{
  "scenario": "${scenario}",
  "target": "${target}",
  "ipp_scenario": "${ipp_scen}",
  "snmp_scenario": "${snmp_scen}",
  "ipp_reasons": ${ipp_reasons},
  "ipp_offline": ${ipp_offline},
  "snmp_offline": ${snmp_offline},
  "hold_job": ${hold_job},
  "updated_at": "${ts}"
}
EOF
}

cmd_fault_set() {
    local scenario=""
    local target="both"
    local opt_ipp=""
    local opt_snmp=""
    local opt_counter=""
    local opt_toner=""
    local opt_drum=""
    local opt_speed=""

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --target)
                target="$2"
                shift 2
                ;;
            --ipp)
                opt_ipp="$2"
                shift 2
                ;;
            --snmp)
                opt_snmp="$2"
                shift 2
                ;;
            --counter)
                opt_counter="$2"
                shift 2
                ;;
            --toner)
                opt_toner="$2"
                shift 2
                ;;
            --drum)
                opt_drum="$2"
                shift 2
                ;;
            --speed)
                opt_speed="$2"
                shift 2
                ;;
            --fault-state)
                EMU_FAULT_STATE_FILE="$2"
                shift 2
                ;;
            *)
                if [[ -z "$scenario" ]]; then
                    scenario="$1"
                    shift
                else
                    echo "Unexpected argument for fault set: $1" >&2
                    return 1
                fi
                ;;
        esac
    done

    if [[ -z "$scenario" && -z "$opt_ipp" && -z "$opt_snmp" && -z "$opt_counter" && -z "$opt_toner" && -z "$opt_drum" ]]; then
        echo "ERROR: Missing scenario name for fault set." >&2
        echo "Valid scenarios: healthy, printing, paper-empty, paper-jam, cover-open, stopped, offline, recovery, clear" >&2
        return 1
    fi

    if [[ -n "$scenario" ]]; then
        case "$scenario" in
            healthy|printing|paper-empty|paper-jam|cover-open|stopped|offline|recovery|clear)
                ;;
            *)
                echo "ERROR: Unknown fault scenario '${scenario}'." >&2
                echo "Valid scenarios: healthy, printing, paper-empty, paper-jam, cover-open, stopped, offline, recovery, clear" >&2
                return 1
                ;;
        esac
    else
        scenario=$(get_fault_state_field "scenario" "healthy")
    fi

    case "$target" in
        both|ipp|snmp)
            ;;
        *)
            echo "ERROR: Invalid target '${target}'. Must be 'both', 'ipp', or 'snmp'." >&2
            return 1
            ;;
    esac

    local prev_ipp
    prev_ipp=$(get_fault_state_field "ipp_scenario" "healthy")
    local prev_ipp_reasons
    prev_ipp_reasons=$(get_fault_state_field "ipp_reasons" "[]")
    local prev_snmp
    prev_snmp=$(get_fault_state_field "snmp_scenario" "healthy")

    local ipp_scen="$prev_ipp"
    local snmp_scen="$prev_snmp"

    if [[ -n "$opt_ipp" ]]; then
        ipp_scen="$opt_ipp"
    elif [[ "$target" == "both" || "$target" == "ipp" ]]; then
        ipp_scen="$scenario"
    fi

    if [[ -n "$opt_snmp" ]]; then
        snmp_scen="$opt_snmp"
    elif [[ "$target" == "both" || "$target" == "snmp" ]]; then
        snmp_scen="$scenario"
    fi

    local ipp_reasons='[]'
    local ipp_offline=false
    local snmp_offline=false
    local hold_job=false

    # IPP state configuration
    case "$ipp_scen" in
        healthy|recovery|clear)
            ipp_scen="healthy"
            ipp_reasons='[]'
            ipp_offline=false
            hold_job=false
            ;;
        printing)
            ipp_reasons='[]'
            ipp_offline=false
            hold_job=true
            ;;
        paper-empty)
            ipp_reasons='["media-empty"]'
            ipp_offline=false
            hold_job=false
            ;;
        paper-jam)
            ipp_reasons='["media-jam"]'
            ipp_offline=false
            hold_job=false
            ;;
        cover-open)
            ipp_reasons='["cover-open"]'
            ipp_offline=false
            hold_job=false
            ;;
        stopped)
            ipp_reasons='["paused"]'
            ipp_offline=false
            hold_job=false
            ;;
        offline)
            ipp_offline=true
            hold_job=false
            ipp_scen="$prev_ipp"
            ipp_reasons="$prev_ipp_reasons"
            ;;
        *)
            echo "ERROR: Unknown IPP scenario '${ipp_scen}'." >&2
            return 1
            ;;
    esac

    # SNMP state configuration
    local snmp_args=()
    case "$snmp_scen" in
        healthy|recovery|clear)
            snmp_scen="healthy"
            snmp_offline=false
            snmp_args=(-status idle -clear-all-errors -offline false)
            ;;
        printing)
            snmp_offline=false
            snmp_args=(-status printing -clear-all-errors -offline false)
            ;;
        paper-empty)
            snmp_offline=false
            snmp_args=(-status other -clear-all-errors -set-error no_paper -offline false)
            ;;
        paper-jam)
            snmp_offline=false
            snmp_args=(-status other -clear-all-errors -set-error paper_jam -offline false)
            ;;
        cover-open)
            snmp_offline=false
            snmp_args=(-status other -clear-all-errors -set-error cover_open -offline false)
            ;;
        stopped)
            snmp_offline=false
            snmp_args=(-status other -offline false)
            ;;
        offline)
            snmp_offline=true
            snmp_args=(-offline true)
            ;;
        *)
            echo "ERROR: Unknown SNMP scenario '${snmp_scen}'." >&2
            return 1
            ;;
    esac

    # Apply IPP subsystem mutations
    if [[ "$target" == "both" || "$target" == "ipp" ]]; then
        if [[ "$ipp_offline" == "true" ]]; then
            # Freeze IPP process via SIGSTOP to simulate network/host communication loss
            write_fault_state "$scenario" "$target" "$ipp_scen" "$snmp_scen" "$ipp_reasons" "$ipp_offline" "$snmp_offline" "$hold_job"
            if is_running; then
                local pid
                pid=$(get_pid)
                if [[ -n "$pid" ]] && ! is_process_paused "$pid"; then
                    kill -STOP "$pid" 2>/dev/null || true
                    echo "$pid" > "$EMU_IPP_PAUSED_FILE"
                fi
            fi
        else
            # Unpause if process was previously paused (offline recovery)
            if is_running; then
                local pid
                pid=$(get_pid)
                if [[ -n "$pid" ]] && (is_process_paused "$pid" || [[ -f "$EMU_IPP_PAUSED_FILE" ]]); then
                    kill -CONT "$pid" 2>/dev/null || true
                    rm -f "$EMU_IPP_PAUSED_FILE"
                fi
            fi

            # If media-empty was active and we are transitioning away, ippeveprinter's in-memory
            # media-empty wait loop must be cleared via a fast restart (< 0.1s).
            local need_restart=false
            if [[ "$ipp_scen" != "paper-empty" ]]; then
                if [[ "$prev_ipp" == "paper-empty" || "$prev_ipp_reasons" =~ media-empty ]]; then
                    need_restart=true
                elif is_running; then
                    sleep 0.1
                    local cur_attrs
                    cur_attrs=$(timeout 1 ipptool -T 1 "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test 2>/dev/null || true)
                    if [[ "$cur_attrs" =~ media-empty || "$cur_attrs" =~ media-needed ]]; then
                        need_restart=true
                    fi
                fi
            fi

            write_fault_state "$scenario" "$target" "$ipp_scen" "$snmp_scen" "$ipp_reasons" "$ipp_offline" "$snmp_offline" "$hold_job"

            if [[ "$need_restart" == "true" ]] && is_running; then
                if ! cmd_stop; then
                    echo "ERROR: Failed to stop IPP emulator during paper-empty recovery." >&2
                    return 1
                fi
                if ! cmd_start; then
                    echo "ERROR: Failed to restart IPP emulator during paper-empty recovery." >&2
                    return 1
                fi
            else
                # Synchronize state attributes via ippeveprinter filter protocol
                if ! sync_ipp_state "$EMU_HOST" "$EMU_PORT" "$ipp_scen"; then
                    echo "ERROR: Failed to synchronize IPP fault state." >&2
                    return 1
                fi
            fi

            # Requirement 7: Never report successful recovery if the endpoint remains unavailable or retains the fault
            if [[ "$scenario" == "recovery" || "$scenario" == "healthy" ]]; then
                if ! verify_ipp_attributes "$EMU_HOST" "$EMU_PORT" "healthy"; then
                    echo "ERROR: Recovery verification failed. IPP endpoint is unavailable or retains faults." >&2
                    return 1
                fi
            fi

            # Requirement 8 & 9: Before cupsdisable or cupsenable, verify queue ownership using metadata,
            # queue name and actual device URI. Refuse to modify any queue whose ownership cannot be verified.
            if [[ "$ipp_scen" == "stopped" ]]; then
                if command -v cupsdisable >/dev/null 2>&1 && [[ -n "$EMU_CUPS_QUEUE" ]] && lpstat -p "$EMU_CUPS_QUEUE" >/dev/null 2>&1; then
                    if verify_cups_queue_ownership "$EMU_CUPS_QUEUE"; then
                        cupsdisable "$EMU_CUPS_QUEUE" 2>/dev/null || true
                    else
                        echo "Warning: Refusing to disable unverified CUPS queue '${EMU_CUPS_QUEUE}'." >&2
                    fi
                fi
            elif [[ "$ipp_scen" == "healthy" ]]; then
                if command -v cupsenable >/dev/null 2>&1 && [[ -n "$EMU_CUPS_QUEUE" ]] && lpstat -p "$EMU_CUPS_QUEUE" >/dev/null 2>&1; then
                    if verify_cups_queue_ownership "$EMU_CUPS_QUEUE"; then
                        cupsenable "$EMU_CUPS_QUEUE" 2>/dev/null || true
                    else
                        echo "Warning: Refusing to enable unverified CUPS queue '${EMU_CUPS_QUEUE}'." >&2
                    fi
                fi
            fi
        fi
    else
        # Only SNMP targeted: preserve existing IPP state in fault_state.json
        local cur_ipp_reasons
        cur_ipp_reasons=$(get_fault_state_field "ipp_reasons" "[]")
        local cur_ipp_off
        cur_ipp_off=$(get_fault_state_field "ipp_offline" "false")
        local cur_hold
        cur_hold=$(get_fault_state_field "hold_job" "false")
        write_fault_state "$scenario" "$target" "$ipp_scen" "$snmp_scen" "$cur_ipp_reasons" "$cur_ipp_off" "$snmp_offline" "$cur_hold"
    fi

    # Apply SNMP subsystem mutations
    if [[ "$target" == "both" || "$target" == "snmp" ]]; then
        if [[ -n "$opt_counter" ]]; then
            snmp_args+=(-counter "$opt_counter")
        fi
        if [[ -n "$opt_toner" ]]; then
            snmp_args+=(-toner-level "$opt_toner" -toner-max 100)
        fi
        if [[ -n "$opt_drum" ]]; then
            snmp_args+=(-drum-level "$opt_drum" -drum-max 100)
        fi
        if ! cmd_snmp_set "${snmp_args[@]}"; then
            echo "ERROR: Failed to update SNMP emulator state." >&2
            return 1
        fi

        if [[ "$scenario" == "recovery" || "$scenario" == "healthy" ]]; then
            local snmp_chk_err
            snmp_chk_err=$(python3 -c "import json, sys; data=json.load(open(sys.argv[1])); print(data.get('error_byte_0', 0) | data.get('error_byte_1', 0))" "$SNMP_STATE_FILE" 2>/dev/null || echo "1")
            if [[ "$snmp_chk_err" != "0" ]]; then
                echo "ERROR: SNMP recovery verification failed. Active errors remain in SNMP state." >&2
                return 1
            fi
        fi
    fi

    echo "Coordinated Fault Injected:"
    echo "  Scenario:      ${scenario}"
    echo "  Target:        ${target}"
    echo "  IPP State:     ${ipp_scen} (reasons: ${ipp_reasons}, offline: ${ipp_offline})"
    echo "  SNMP State:    ${snmp_scen} (offline: ${snmp_offline})"
    return 0
}

cmd_fault_clear() {
    cmd_fault_set healthy --target both "$@"
}

cmd_fault_status() {
    echo "============================================================"
    echo "  XeroxGo Coordinated Fault Controller Status"
    echo "============================================================"

    local scen target ipp_scen snmp_scen ipp_reasons ipp_off snmp_off updated_at
    scen=$(get_fault_state_field "scenario" "healthy")
    target=$(get_fault_state_field "target" "both")
    ipp_scen=$(get_fault_state_field "ipp_scenario" "healthy")
    snmp_scen=$(get_fault_state_field "snmp_scenario" "healthy")
    ipp_reasons=$(get_fault_state_field "ipp_reasons" "[]")
    ipp_off=$(get_fault_state_field "ipp_offline" "false")
    snmp_off=$(get_fault_state_field "snmp_offline" "false")
    updated_at=$(get_fault_state_field "updated_at" "unknown")

    echo "Configured Fault Scenario: ${scen}"
    echo "  Target:       ${target}"
    echo "  IPP Target:   ${ipp_scen} (reasons: ${ipp_reasons}, offline: ${ipp_off})"
    echo "  SNMP Target:  ${snmp_scen} (offline: ${snmp_off})"
    echo "  Updated At:   ${updated_at}"
    echo ""

    echo "--- IPP Subsystem Telemetry ---"
    local ipp_running=false
    local ipp_paused=false
    local ipp_pid=""
    if is_running; then
        ipp_running=true
        ipp_pid=$(get_pid)
        if [[ -n "$ipp_pid" ]] && is_process_paused "$ipp_pid"; then
            ipp_paused=true
        fi
    fi

    local live_ipp_state="UNKNOWN"
    local live_ipp_reasons="unknown"
    local live_ipp_accepting="unknown"

    if [[ "$ipp_running" == "false" ]]; then
        echo "  IPP Emulator:  STOPPED"
    elif [[ "$ipp_paused" == "true" ]]; then
        echo "  IPP Emulator:  RUNNING (PID: ${ipp_pid}, PAUSED/OFFLINE via SIGSTOP)"
        echo "  Endpoint:      ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
        echo "  Communication: OFFLINE (TCP socket unresponsive)"
        live_ipp_state="OFFLINE"
        live_ipp_reasons="none"
    else
        echo "  IPP Emulator:  RUNNING (PID: ${ipp_pid})"
        echo "  Endpoint:      ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
        echo "  Communication: ONLINE"
        if command -v ipptool >/dev/null 2>&1; then
            local ipp_attrs
            ipp_attrs=$(ipptool -tv "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test 2>/dev/null || true)
            live_ipp_state=$(echo "$ipp_attrs" | awk '/printer-state \(enum\)/ {print $NF}')
            live_ipp_reasons=$(echo "$ipp_attrs" | grep "printer-state-reasons" | sed -E 's/.*=[[:space:]]*//' | tr -d ' ' || true)
            live_ipp_accepting=$(echo "$ipp_attrs" | awk '/printer-is-accepting-jobs \(boolean\)/ {print $NF}')
            echo "  printer-state:               ${live_ipp_state:-unknown}"
            echo "  printer-state-reasons:       ${live_ipp_reasons:-none}"
            echo "  printer-is-accepting-jobs:   ${live_ipp_accepting:-unknown}"
        fi
    fi

    if [[ -f "$EMU_META_FILE" ]] && command -v lpstat >/dev/null 2>&1; then
        echo "  CUPS Queue (${EMU_CUPS_QUEUE}):"
        lpstat -p "$EMU_CUPS_QUEUE" 2>&1 | sed 's/^/    /' || true
    fi
    echo ""

    echo "--- SNMP Subsystem Telemetry ---"
    local snmp_running=false
    local snmp_pid=""
    if is_snmp_running; then
        snmp_running=true
        snmp_pid=$(get_snmp_pid)
    fi

    local snmp_state_json="{}"
    local snmp_comm="UNKNOWN"
    local snmp_flags=""
    local snmp_status_val=0

    if [[ "$snmp_running" == "false" ]]; then
        echo "  SNMP Emulator: STOPPED"
    else
        echo "  SNMP Emulator: RUNNING (PID: ${snmp_pid})"
        echo "  Endpoint:      ${SNMP_HOST}:${SNMP_PORT} (UDP, community: ${SNMP_COMMUNITY})"
        ensure_snmp_responder_binary
        "${SCRIPT_DIR}/snmp_responder" get -state "$SNMP_STATE_FILE" | sed 's/^/  /'
        snmp_state_json=$("${SCRIPT_DIR}/snmp_responder" get -state "$SNMP_STATE_FILE" -json 2>/dev/null || echo "{}")
        snmp_comm=$(python3 -c "import json, sys; data=json.load(open(sys.argv[1])); print('OFFLINE' if data.get('offline') else 'ONLINE')" "$SNMP_STATE_FILE" 2>/dev/null || echo "UNKNOWN")
        snmp_flags=$(python3 -c "
import json, sys
try:
    data = json.load(open(sys.argv[1]))
    b0 = data.get('error_byte_0', 0)
    b1 = data.get('error_byte_1', 0)
    flags = []
    if b0 & 0x80: flags.append('lowPaper')
    if b0 & 0x40: flags.append('noPaper')
    if b0 & 0x20: flags.append('lowToner')
    if b0 & 0x10: flags.append('noToner')
    if b0 & 0x08: flags.append('doorOpen')
    if b0 & 0x04: flags.append('jammed')
    if b0 & 0x02: flags.append('offline')
    if b0 & 0x01: flags.append('serviceRequested')
    if b1 & 0x80: flags.append('inputTrayMissing')
    if b1 & 0x40: flags.append('outputTrayMissing')
    if b1 & 0x20: flags.append('markerSupplyMissing')
    if b1 & 0x10: flags.append('outputNearFull')
    if b1 & 0x08: flags.append('outputFull')
    if b1 & 0x04: flags.append('inputTrayEmpty')
    if b1 & 0x02: flags.append('overduePreventMaint')
    print(','.join(flags))
except Exception:
    print('')
" "$SNMP_STATE_FILE" 2>/dev/null || echo "")
        snmp_status_val=$(python3 -c "import json, sys; data=json.load(open(sys.argv[1])); print(data.get('hr_printer_status', 0))" "$SNMP_STATE_FILE" 2>/dev/null || echo "0")
    fi
    echo ""

    echo "--- Coordinated Telemetry Analysis ---"
    if [[ "$ipp_paused" == "true" || "$live_ipp_state" == "OFFLINE" ]] && [[ "$snmp_comm" == "OFFLINE" ]]; then
        echo "  Status: [AGREEMENT] Both IPP and SNMP are OFFLINE (IPP TCP unresponsive, SNMP UDP dropped)."
    elif [[ "$ipp_paused" == "true" || "$live_ipp_state" == "OFFLINE" ]] && [[ "$snmp_comm" == "ONLINE" ]]; then
        echo "  Status: [CONTRADICTION] IPP is OFFLINE (frozen), but SNMP reports ONLINE."
    elif [[ "$ipp_paused" == "false" && "$live_ipp_state" != "OFFLINE" ]] && [[ "$snmp_comm" == "OFFLINE" ]]; then
        echo "  Status: [CONTRADICTION] SNMP is OFFLINE (packet drop), but IPP reports ONLINE."
    else
        local ipp_jam=false ipp_cover=false ipp_empty=false ipp_paused_r=false
        if echo "$live_ipp_reasons" | grep -q "media-jam"; then ipp_jam=true; fi
        if echo "$live_ipp_reasons" | grep -q "cover-open"; then ipp_cover=true; fi
        if echo "$live_ipp_reasons" | grep -q "media-empty"; then ipp_empty=true; fi
        if echo "$live_ipp_reasons" | grep -q "paused"; then ipp_paused_r=true; fi

        local snmp_jam=false snmp_cover=false snmp_empty=false
        if echo "$snmp_flags" | grep -qiE "jam|jammed|paper_jam"; then snmp_jam=true; fi
        if echo "$snmp_flags" | grep -qiE "door|cover"; then snmp_cover=true; fi
        if echo "$snmp_flags" | grep -qiE "nopaper|no_paper|empty"; then snmp_empty=true; fi

        if [[ "$scen" == "printing" || "$snmp_status_val" == "4" ]]; then
            echo "  Status: [AGREEMENT] Both IPP and SNMP report PRINTING."
        elif [[ "$ipp_jam" == "true" && "$snmp_jam" == "true" ]]; then
            echo "  Status: [AGREEMENT] Both IPP and SNMP report PAPER-JAM."
        elif [[ "$ipp_cover" == "true" && "$snmp_cover" == "true" ]]; then
            echo "  Status: [AGREEMENT] Both IPP and SNMP report COVER-OPEN."
        elif [[ "$ipp_empty" == "true" && "$snmp_empty" == "true" ]]; then
            echo "  Status: [AGREEMENT] Both IPP and SNMP report PAPER-EMPTY."
        elif [[ "$ipp_jam" == "true" && "$snmp_jam" == "false" ]]; then
            echo "  Status: [CONTRADICTION] IPP reports paper-jam (${live_ipp_reasons}), but SNMP reports no paper-jam."
        elif [[ "$ipp_jam" == "false" && "$snmp_jam" == "true" ]]; then
            echo "  Status: [CONTRADICTION] SNMP reports paper-jam, but IPP reports healthy (${live_ipp_reasons})."
        elif [[ "$ipp_cover" == "true" && "$snmp_cover" == "false" ]]; then
            echo "  Status: [CONTRADICTION] IPP reports cover-open (${live_ipp_reasons}), but SNMP reports no cover-open."
        elif [[ "$ipp_cover" == "false" && "$snmp_cover" == "true" ]]; then
            echo "  Status: [CONTRADICTION] SNMP reports cover-open, but IPP reports healthy (${live_ipp_reasons})."
        elif [[ "$ipp_empty" == "true" && "$snmp_empty" == "false" ]]; then
            echo "  Status: [CONTRADICTION] IPP reports paper-empty (${live_ipp_reasons}), but SNMP reports no empty."
        elif [[ "$ipp_empty" == "false" && "$snmp_empty" == "true" ]]; then
            echo "  Status: [CONTRADICTION] SNMP reports paper-empty, but IPP reports healthy (${live_ipp_reasons})."
        elif [[ "$ipp_paused_r" == "true" ]]; then
            echo "  Status: [IPP-STOPPED] IPP reports paused reason (state: ${live_ipp_state})."
        else
            echo "  Status: [AGREEMENT] Both IPP and SNMP report HEALTHY / IDLE."
        fi
    fi
    echo "  Limitation note: Standalone ippeveprinter only reports states 3 (idle) and 4 (processing). State 5 (stopped) is managed via CUPS queue (cupsdisable)."
    echo "============================================================"
    return 0
}

cmd_fault() {
    local subcmd="${1:-}"
    if [[ -z "$subcmd" ]]; then
        cmd_fault_status
        return 0
    fi
    shift
    case "$subcmd" in
        set)
            cmd_fault_set "$@"
            ;;
        clear)
            cmd_fault_clear "$@"
            ;;
        status)
            cmd_fault_status "$@"
            ;;
        *)
            case "$subcmd" in
                healthy|printing|paper-empty|paper-jam|cover-open|stopped|offline|recovery|clear)
                    cmd_fault_set "$subcmd" "$@"
                    ;;
                *)
                    echo "Unknown fault subcommand: $subcmd" >&2
                    echo "Usage: $(basename "$0") fault [set|clear|status] [scenario] [options]" >&2
                    return 1
                    ;;
            esac
            ;;
    esac
}

show_help() {
    cat << EOF
Usage: $(basename "$0") [options] <command> [command-options]

IPP Emulator Commands:
  start        Start the IPP printer emulator daemon
  stop         Stop the IPP printer emulator daemon
  status       Check emulator daemon status
  setup-cups   Create a temporary CUPS driverless queue pointing to emulator
  remove-cups  Remove the temporary CUPS driverless queue (verified ownership)
  cleanup      Stop emulator, delete verified CUPS queue, and clean spool artifacts

SNMP Emulator Commands:
  start-snmp   Start the SNMPv2c printer emulator daemon
  stop-snmp    Stop the SNMPv2c printer emulator daemon
  status-snmp  Check SNMP emulator daemon status and simulated MIB values
  cleanup-snmp Stop SNMP emulator and remove PID, log, and state files
  snmp-set     Update simulated SNMP values (error bits, consumables, status, etc.)
  snmp-get     Inspect current simulated SNMP state
  snmp-reset   Reset simulated SNMP values to default healthy/idle state

Coordinated Fault Injection Commands:
  fault set <scenario> [options]  Inject coordinated fault (healthy, printing, paper-empty, paper-jam, cover-open, stopped, offline, recovery)
  fault clear                     Clear active fault and restore healthy state across IPP and SNMP
  fault status                    Show coordinated fault controller telemetry and agreement/contradiction analysis

IPP Options:
  -h, --host <host>          Host interface to bind (default: ${EMU_HOST})
  -p, --port <port>          Port number to bind (default: ${EMU_PORT})
  -n, --name <name>          Printer service name (default: ${EMU_NAME})
  -q, --queue <queue>        CUPS queue name (default: ${EMU_CUPS_QUEUE})
  -d, --spool-dir <dir>      Spool directory (default: ${EMU_SPOOL_DIR})

SNMP Options:
  --snmp-host <host>         SNMP host interface (loopback only, default: ${SNMP_HOST})
  --snmp-port <port>         SNMP UDP port (default: ${SNMP_PORT})
  --snmp-community <string>  SNMPv2c community (default: ${SNMP_COMMUNITY})
  --snmp-state <path>        Path to SNMP state JSON (default: ${SNMP_STATE_FILE})
  --snmp-pid <path>          Path to SNMP PID file (default: ${SNMP_PID_FILE})
  --snmp-log <path>          Path to SNMP log file (default: ${SNMP_LOG_FILE})

Fault Injection Options:
  --target <both|ipp|snmp>   Subsystem telemetry target (default: both)
  --ipp <scenario>           Explicit IPP scenario override
  --snmp <scenario>          Explicit SNMP scenario override
  --counter <N>              Explicit odometer marker count (Counter32)
  --toner <pct>              Toner level percentage (0-100)
  --drum <pct>               Drum life percentage (0-100)
  --fault-state <path>       Path to fault state JSON (default: ${EMU_FAULT_STATE_FILE})

General Options:
  --help                     Show this help message

Environment Variables:
  EMU_HOST, EMU_PORT, EMU_NAME, EMU_CUPS_QUEUE, EMU_SPOOL_DIR
  SNMP_HOST, SNMP_PORT, SNMP_COMMUNITY, SNMP_STATE_FILE, SNMP_PID_FILE
  EMU_FAULT_STATE_FILE
EOF
}

# Parse options and dispatch
COMMAND=""
COMMAND_ARGS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--host)
            EMU_HOST="$2"
            shift 2
            ;;
        -p|--port)
            EMU_PORT="$2"
            shift 2
            ;;
        -n|--name)
            EMU_NAME="$2"
            shift 2
            ;;
        -q|--queue)
            EMU_CUPS_QUEUE="$2"
            shift 2
            ;;
        -d|--spool-dir)
            EMU_SPOOL_DIR="$2"
            shift 2
            ;;
        --snmp-host)
            SNMP_HOST="$2"
            shift 2
            ;;
        --snmp-port)
            SNMP_PORT="$2"
            shift 2
            ;;
        --snmp-community)
            SNMP_COMMUNITY="$2"
            shift 2
            ;;
        --snmp-state)
            SNMP_STATE_FILE="$2"
            shift 2
            ;;
        --snmp-pid)
            SNMP_PID_FILE="$2"
            shift 2
            ;;
        --snmp-log)
            SNMP_LOG_FILE="$2"
            shift 2
            ;;
        --fault-state)
            EMU_FAULT_STATE_FILE="$2"
            shift 2
            ;;
        --help)
            show_help
            exit 0
            ;;
        start|stop|status|setup-cups|remove-cups|cleanup|start-snmp|stop-snmp|status-snmp|cleanup-snmp)
            COMMAND="$1"
            shift
            ;;
        snmp-set|snmp-get|snmp-reset)
            COMMAND="$1"
            shift
            COMMAND_ARGS=("$@")
            break
            ;;
        fault|fault-set|fault-clear|fault-status)
            COMMAND="$1"
            shift
            COMMAND_ARGS=("$@")
            break
            ;;
        *)
            if [[ -z "$COMMAND" ]]; then
                COMMAND="$1"
                shift
            else
                COMMAND_ARGS+=("$1")
                shift
            fi
            ;;
    esac
done

if [[ -z "$COMMAND" ]]; then
    show_help
    exit 1
fi

case "$COMMAND" in
    start)
        cmd_start
        ;;
    stop)
        cmd_stop
        ;;
    status)
        cmd_status
        ;;
    setup-cups)
        cmd_setup_cups
        ;;
    remove-cups)
        cmd_remove_cups "${COMMAND_ARGS[@]}"
        ;;
    verify-queue)
        cmd_verify_queue "${COMMAND_ARGS[@]}"
        ;;
    cleanup)
        cmd_cleanup
        ;;
    start-snmp)
        cmd_start_snmp
        ;;
    stop-snmp)
        cmd_stop_snmp
        ;;
    status-snmp)
        cmd_status_snmp
        ;;
    cleanup-snmp)
        cmd_cleanup_snmp
        ;;
    snmp-set)
        cmd_snmp_set "${COMMAND_ARGS[@]}"
        ;;
    snmp-get)
        cmd_snmp_get "${COMMAND_ARGS[@]}"
        ;;
    snmp-reset)
        cmd_snmp_reset "${COMMAND_ARGS[@]}"
        ;;
    fault)
        cmd_fault "${COMMAND_ARGS[@]}"
        ;;
    fault-set)
        cmd_fault_set "${COMMAND_ARGS[@]}"
        ;;
    fault-clear)
        cmd_fault_clear "${COMMAND_ARGS[@]}"
        ;;
    fault-status)
        cmd_fault_status "${COMMAND_ARGS[@]}"
        ;;
    *)
        echo "Unknown command: $COMMAND" >&2
        show_help
        exit 1
        ;;
esac
