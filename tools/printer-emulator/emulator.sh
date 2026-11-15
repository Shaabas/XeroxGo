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

# Verify spool directory ownership and initialize or reuse strictly
verify_and_init_spool_dir() {
    local dir="$1"
    local marker="${dir}/${SPOOL_MARKER_NAME}"

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

        # Directory is empty (or has only .gitkeep) - initialize ownership marker
        cat << EOF > "$marker"
EMULATOR_OWNED_SPOOL=true
CREATED_BY=xeroxgo_emulator
CREATED_AT=$(date -u +%FT%TZ)
HOST=${EMU_HOST}
PORT=${EMU_PORT}
EOF
        return 0
    fi

    # Requirement 4 & 8: Existing directories containing only valid emulator-owned artifacts may be reused
    if ! grep -q "EMULATOR_OWNED_SPOOL=true" "$marker" 2>/dev/null; then
        echo "ERROR: Spool directory ownership marker in '$dir' is invalid or corrupt. Refusing startup." >&2
        return 1
    fi

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

        # Marker and .gitkeep are valid metadata entries
        if [[ "$fname" == "$SPOOL_MARKER_NAME" || "$fname" == ".gitkeep" ]]; then
            continue
        fi

        # Only recognized emulator artifacts are permitted for reuse
        if ! is_emulator_artifact "$fname"; then
            echo "ERROR: Owned spool directory contains unrecognized or foreign file '$fname'. Refusing startup for safety." >&2
            return 1
        fi
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
        echo "Status: RUNNING (PID: ${pid})"
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
                    ((file_count++))
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

cmd_remove_cups() {
    if ! command -v lpadmin >/dev/null 2>&1; then
        return 0
    fi

    local target_queue="${EMU_CUPS_QUEUE}"
    if [[ -z "$target_queue" ]]; then
        echo "ERROR: Queue name cannot be empty." >&2
        return 1
    fi

    # Requirement 5 & 6: Verify ownership and expected URI before deletion; refuse if cannot be verified
    if [[ ! -f "$EMU_META_FILE" ]]; then
        echo "ERROR: Ownership metadata not found (${EMU_META_FILE})." >&2
        echo "Cannot verify that CUPS queue '${target_queue}' was created by this harness. Refusing deletion." >&2
        return 1
    fi

    local meta_queue meta_uri
    meta_queue=$(grep -E "^QUEUE_NAME=" "$EMU_META_FILE" | cut -d'=' -f2- || true)
    meta_uri=$(grep -E "^DEVICE_URI=" "$EMU_META_FILE" | cut -d'=' -f2- || true)

    if [[ "$meta_queue" != "$target_queue" ]]; then
        echo "ERROR: Metadata queue name ('${meta_queue}') does not match requested queue ('${target_queue}'). Refusing deletion." >&2
        return 1
    fi

    # If queue does not exist in CUPS, clean metadata and return
    if ! lpstat -p "$target_queue" >/dev/null 2>&1 && ! lpstat -v "$target_queue" >/dev/null 2>&1; then
        echo "CUPS queue '${target_queue}' does not exist in CUPS (already removed)."
        rm -f "$EMU_META_FILE"
        return 0
    fi

    # Check actual URI in CUPS
    local actual_uri
    actual_uri=$(lpstat -v "$target_queue" 2>/dev/null | sed -n 's/^device for [^:]*: *//p' | tr -d ' \t\r\n')
    if [[ "$actual_uri" != "$meta_uri" ]]; then
        echo "ERROR: CUPS queue '${target_queue}' device URI ('${actual_uri}') does not match expected emulator URI ('${meta_uri}')." >&2
        echo "Queue may have been reconfigured or points elsewhere. Refusing deletion." >&2
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

    cmd_stop || true

    # Requirement 6, 7, 8, 9, 10: Validate spool path, verify ownership marker,
    # and selectively remove only verified emulator artifacts while strictly
    # preserving any unrelated files, subdirectories, symlinks, and metadata.
    if [[ -d "$EMU_SPOOL_DIR" ]]; then
        local clean_spool
        if clean_spool=$(validate_spool_dir "$EMU_SPOOL_DIR"); then
            local marker="${clean_spool}/${SPOOL_MARKER_NAME}"
            if [[ ! -f "$marker" ]] || ! grep -q "EMULATOR_OWNED_SPOOL=true" "$marker" 2>/dev/null; then
                echo "Warning: Spool directory '${clean_spool}' does not have verified emulator ownership marker." >&2
                echo "Skipping spool file deletion for safety." >&2
            else
                echo "Cleaning verified emulator spool files in ${clean_spool}..."
                local entry fname
                for entry in "$clean_spool"/* "$clean_spool"/.*; do
                    [[ -e "$entry" || -L "$entry" ]] || continue
                    fname=$(basename "$entry")
                    [[ "$fname" != "." && "$fname" != ".." ]] || continue

                    # Protect symlinks, subdirectories, marker, and .gitkeep
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
                    if [[ "$fname" == "$SPOOL_MARKER_NAME" || "$fname" == ".gitkeep" ]]; then
                        continue
                    fi

                    # Only delete verified emulator artifacts
                    if is_emulator_artifact "$fname"; then
                        rm -f "$entry"
                        echo "  Deleted emulator artifact: ${fname}"
                    else
                        echo "Notice: Preserving non-emulator file '${fname}' in spool directory."
                    fi
                done
            fi
        else
            echo "Warning: Spool directory path failed safety validation. Skipping spool cleanup." >&2
        fi
    fi

    rm -f "$EMU_PID_FILE" "$EMU_LOG_FILE"
    rm -rf "$EMU_LOCK_DIR" 2>/dev/null || true
    echo "Cleanup complete. Verified emulator resources removed."
}

show_help() {
    cat << EOF
Usage: $(basename "$0") [options] <command>

Commands:
  start        Start the IPP printer emulator daemon
  stop         Stop the IPP printer emulator daemon
  status       Check emulator daemon status
  setup-cups   Create a temporary CUPS driverless queue pointing to emulator
  remove-cups  Remove the temporary CUPS driverless queue (verified ownership)
  cleanup      Stop emulator, delete verified CUPS queue, and clean spool artifacts

Options:
  -h, --host <host>       Host interface to bind (default: ${EMU_HOST})
  -p, --port <port>       Port number to bind (default: ${EMU_PORT})
  -n, --name <name>       Printer service name (default: ${EMU_NAME})
  -q, --queue <queue>     CUPS queue name (default: ${EMU_CUPS_QUEUE})
  -d, --spool-dir <dir>   Spool directory (default: ${EMU_SPOOL_DIR})
  --help                  Show this help message

Environment Variables:
  EMU_HOST, EMU_PORT, EMU_NAME, EMU_CUPS_QUEUE, EMU_SPOOL_DIR
EOF
}

# Parse options
POSITIONAL_ARGS=()
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
        --help)
            show_help
            exit 0
            ;;
        *)
            POSITIONAL_ARGS+=("$1")
            shift
            ;;
    esac
done

set -- "${POSITIONAL_ARGS[@]:-}"

if [[ $# -lt 1 ]]; then
    show_help
    exit 1
fi

case "$1" in
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
        cmd_remove_cups
        ;;
    cleanup)
        cmd_cleanup
        ;;
    *)
        echo "Unknown command: $1" >&2
        show_help
        exit 1
        ;;
esac
