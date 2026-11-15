#!/usr/bin/env bash
# emulator.sh - Control script for local IPP Everywhere printer emulator
#
# Uses CUPS ippeveprinter to emulate an isolated IPP Everywhere printer on localhost.
# Never sends jobs to physical hardware. Dedicated spool directory preserves print artifacts.

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
EMU_PROCESSOR="${EMU_PROCESSOR:-${SCRIPT_DIR}/processor.sh}"
EMU_FORMATS="${EMU_FORMATS:-application/pdf,image/pwg-raster,image/urf,application/octet-stream,text/plain,application/postscript}"
EMU_SPEED="${EMU_SPEED:-20,20}"
EMU_DUPLEX="${EMU_DUPLEX:-true}"

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

is_running() {
    if [[ -f "$EMU_PID_FILE" ]]; then
        local pid
        pid=$(cat "$EMU_PID_FILE" 2>/dev/null || true)
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    return 1
}

get_pid() {
    if [[ -f "$EMU_PID_FILE" ]]; then
        cat "$EMU_PID_FILE" 2>/dev/null || true
    fi
}

cmd_start() {
    if is_running; then
        local pid
        pid=$(get_pid)
        echo "IPP printer emulator is already running (PID: ${pid}) at ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
        return 0
    fi

    local ippeveprinter_bin
    if ! ippeveprinter_bin=$(find_ippeveprinter); then
        echo "ERROR: ippeveprinter binary not found." >&2
        echo "Please install CUPS printer-application tools for your distribution:" >&2
        echo "  - Fedora / RHEL: sudo dnf install cups-printerapp" >&2
        echo "  - Debian / Ubuntu: sudo apt-get install cups-ipp-utils" >&2
        echo "  - Arch Linux: sudo pacman -S cups" >&2
        return 1
    fi

    # Ensure spool directory exists and has absolute path
    if [[ ! "$EMU_SPOOL_DIR" = /* ]]; then
        EMU_SPOOL_DIR="$(pwd)/${EMU_SPOOL_DIR}"
    fi
    mkdir -p "$EMU_SPOOL_DIR"

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

    # Wait up to 3 seconds for readiness
    local ready=false
    for _ in {1..30}; do
        if ! kill -0 "$emu_pid" 2>/dev/null; then
            echo "ERROR: ippeveprinter failed to start. Log output:" >&2
            cat "$EMU_LOG_FILE" >&2
            rm -f "$EMU_PID_FILE"
            return 1
        fi
        if command -v ipptool >/dev/null 2>&1; then
            if ipptool "ipp://${EMU_HOST}:${EMU_PORT}/ipp/print" get-printer-attributes.test >/dev/null 2>&1; then
                ready=true
                break
            fi
        fi
        if grep -q "printer-uri-supported" "$EMU_LOG_FILE" 2>/dev/null; then
            ready=true
            break
        fi
        sleep 0.1
    done

    if [[ "$ready" != "true" ]]; then
        # Check if process is still running even if string wasn't found immediately
        if kill -0 "$emu_pid" 2>/dev/null; then
            ready=true
        fi
    fi

    echo "IPP printer emulator started (PID: ${emu_pid})"
    echo "  Endpoint:  ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
    echo "  Spool Dir: ${EMU_SPOOL_DIR}"
    echo "  Log File:  ${EMU_LOG_FILE}"
}

cmd_stop() {
    if ! is_running; then
        echo "IPP printer emulator is not running."
        rm -f "$EMU_PID_FILE"
        return 0
    fi

    local pid
    pid=$(get_pid)
    echo "Stopping IPP printer emulator (PID: ${pid})..."
    kill "$pid" 2>/dev/null || true

    for _ in {1..20}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        sleep 0.1
    done

    if kill -0 "$pid" 2>/dev/null; then
        echo "Force killing PID ${pid}..."
        kill -9 "$pid" 2>/dev/null || true
    fi

    rm -f "$EMU_PID_FILE"
    echo "IPP printer emulator stopped."
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
            local file_count
            file_count=$(find "$EMU_SPOOL_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l)
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

    local uri="ipp://${EMU_HOST}:${EMU_PORT}/ipp/print"
    echo "Configuring driverless CUPS queue '${EMU_CUPS_QUEUE}' targeting ${uri}..."

    # Configure driverless queue with -m everywhere
    lpadmin -p "$EMU_CUPS_QUEUE" -v "$uri" -E -m everywhere 2>&1

    # Verify queue status
    if lpstat -p "$EMU_CUPS_QUEUE" >/dev/null 2>&1; then
        echo "SUCCESS: CUPS queue '${EMU_CUPS_QUEUE}' is ready."
        lpstat -p "$EMU_CUPS_QUEUE"
    else
        echo "ERROR: Failed to configure CUPS queue '${EMU_CUPS_QUEUE}'." >&2
        return 1
    fi
}

cmd_remove_cups() {
    if ! command -v lpadmin >/dev/null 2>&1; then
        return 0
    fi

    # Safety check: Never delete empty, wildcard, or unexpected queue
    local queue_to_delete="${EMU_CUPS_QUEUE}"
    if [[ -z "$queue_to_delete" ]]; then
        echo "ERROR: Queue name is empty; refusing to delete." >&2
        return 1
    fi

    # Verify queue exists before attempting removal
    if lpstat -p "$queue_to_delete" >/dev/null 2>&1; then
        echo "Removing temporary CUPS queue '${queue_to_delete}'..."
        lpadmin -x "$queue_to_delete" 2>/dev/null || true
        echo "CUPS queue '${queue_to_delete}' removed."
    else
        echo "CUPS queue '${queue_to_delete}' does not exist (already clean)."
    fi
}

cmd_cleanup() {
    echo "=== Cleaning up emulator resources ==="
    cmd_remove_cups || true
    cmd_stop || true

    # Clean spool files but keep directory
    if [[ -d "$EMU_SPOOL_DIR" ]]; then
        echo "Cleaning spool files in ${EMU_SPOOL_DIR}..."
        find "$EMU_SPOOL_DIR" -maxdepth 1 -type f -delete 2>/dev/null || true
    fi

    rm -f "$EMU_PID_FILE" "$EMU_LOG_FILE"
    echo "Cleanup complete. All emulator resources removed."
}

show_help() {
    cat << EOF
Usage: $(basename "$0") [options] <command>

Commands:
  start        Start the IPP printer emulator daemon
  stop         Stop the IPP printer emulator daemon
  status       Check emulator daemon status
  setup-cups   Create a temporary CUPS driverless queue pointing to emulator
  remove-cups  Remove the temporary CUPS driverless queue
  cleanup      Stop emulator, delete CUPS queue, and clean spool artifacts

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
