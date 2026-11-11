#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Headless Raspberry Pi Kiosk Launcher
#
# Launches the XeroxGo kiosk daemon in headless mode with:
#  - Simulated mock PJL printer (no CUPS / hardware required)
#  - Stdin keypad reader for interactive PIN entry & confirmation over SSH
#  - Continuous atomic PNG rendering to a dedicated viewer directory
#  - Built-in live browser viewer server on dedicated port (default: 8081)
#  - Graceful audio completion chime (synthesized in-memory WAV)
#  - Configurable simulated paper stock and failure injection (jam, paper-empty, offline)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Configuration defaults (overridable via environment variables or flags)
CLOUD_URL="${CLOUD_URL:-ws://127.0.0.1:8080/ws}"
KIOSK_ID="${KIOSK_ID:-kiosk-demo}"
KIOSK_SECRET="${KIOSK_SECRET:-${XEROXGO_KIOSK_SECRET:-dev_kiosk_secret_32_bytes_long_1234}}"
VIEWER_PORT="${VIEWER_PORT:-8081}"
SIMULATED_STOCK="${SIMULATED_STOCK:-250}"
SIMULATED_FAILURE="${SIMULATED_FAILURE:-}"
SIMULATED_FAILURE_SHEET="${SIMULATED_FAILURE_SHEET:-0}"
ENABLE_SOUND="${ENABLE_SOUND:-true}"
VIEWER_DIR="${VIEWER_DIR:-/tmp/xeroxgo-viewer}"

# Parse optional CLI flags
while [[ $# -gt 0 ]]; do
  case "$1" in
    --cloud-url=*) CLOUD_URL="${1#*=}"; shift ;;
    --cloud-url) CLOUD_URL="$2"; shift 2 ;;
    --kiosk-id=*) KIOSK_ID="${1#*=}"; shift ;;
    --kiosk-id) KIOSK_ID="$2"; shift 2 ;;
    --kiosk-secret=*) KIOSK_SECRET="${1#*=}"; shift ;;
    --kiosk-secret) KIOSK_SECRET="$2"; shift 2 ;;
    --viewer-port=*) VIEWER_PORT="${1#*=}"; shift ;;
    --viewer-port) VIEWER_PORT="$2"; shift 2 ;;
    --viewer-dir=*) VIEWER_DIR="${1#*=}"; shift ;;
    --viewer-dir) VIEWER_DIR="$2"; shift 2 ;;
    --simulated-stock=*) SIMULATED_STOCK="${1#*=}"; shift ;;
    --simulated-stock) SIMULATED_STOCK="$2"; shift 2 ;;
    --simulated-failure=*) SIMULATED_FAILURE="${1#*=}"; shift ;;
    --simulated-failure) SIMULATED_FAILURE="$2"; shift 2 ;;
    --simulated-failure-sheet=*) SIMULATED_FAILURE_SHEET="${1#*=}"; shift ;;
    --simulated-failure-sheet) SIMULATED_FAILURE_SHEET="$2"; shift 2 ;;
    --enable-sound=*) ENABLE_SOUND="${1#*=}"; shift ;;
    --no-sound) ENABLE_SOUND="false"; shift ;;
    --help|-h)
      echo "Usage: $0 [options]"
      echo "Options:"
      echo "  --cloud-url=URL                Cloud WebSocket URL (default: $CLOUD_URL)"
      echo "  --kiosk-id=ID                  Kiosk ID (default: $KIOSK_ID)"
      echo "  --viewer-port=PORT             Port for live web viewer (default: $VIEWER_PORT)"
      echo "  --viewer-dir=DIR               Dedicated viewer directory (default: $VIEWER_DIR)"
      echo "  --simulated-stock=N            Initial paper stock sheets (default: $SIMULATED_STOCK)"
      echo "  --simulated-failure=TYPE       Simulate failure: none, paper-empty, jam, offline"
      echo "  --simulated-failure-sheet=N    Sheet after which failure triggers (0 = immediate)"
      echo "  --no-sound                     Disable audio completion chime"
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

# 1. Build kiosk daemon binary if missing or stale
BIN_DIR="${REPO_ROOT}/bin"
KIOSK_BIN="${BIN_DIR}/xeroxgo-kiosk"
mkdir -p "${BIN_DIR}"

if [ ! -f "${KIOSK_BIN}" ] || [ "${REPO_ROOT}/kiosk-daemon/cmd/daemon/main.go" -nt "${KIOSK_BIN}" ]; then
  echo "Building xeroxgo-kiosk binary..."
  (cd "${REPO_ROOT}/kiosk-daemon" && CGO_ENABLED=0 go build -o "${KIOSK_BIN}" ./cmd/daemon)
fi

# 2. Setup dedicated viewer directory (isolated from /tmp)
mkdir -p "${VIEWER_DIR}" "${VIEWER_DIR}/jobs"
PREVIEW_PNG="${VIEWER_DIR}/screen.png"
STATE_DB="${VIEWER_DIR}/kiosk.db"

# 3. Detect primary IP address for presenter display
PRIMARY_IP="127.0.0.1"
if command -v hostname >/dev/null 2>&1; then
  IP_CANDIDATE="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
  if [ -n "${IP_CANDIDATE}" ]; then
    PRIMARY_IP="${IP_CANDIDATE}"
  fi
fi

# 4. Configure terminal settings for instant interactive responsiveness
OLD_STTY=""
if [ -t 0 ]; then
  OLD_STTY="$(stty -g 2>/dev/null || true)"
  # Set raw single-character mode so keystrokes register immediately without Enter
  stty -icanon -echo min 1 time 0 2>/dev/null || true
  trap 'if [ -n "${OLD_STTY}" ]; then stty "${OLD_STTY}" 2>/dev/null || true; fi' EXIT INT TERM
fi

# 5. Display banner
echo "=============================================================================="
echo " XeroxGo Headless Raspberry Pi Kiosk Active"
echo "=============================================================================="
echo " * Kiosk ID:            ${KIOSK_ID}"
echo " * Cloud Target:        ${CLOUD_URL}"
echo " * Live Screen Monitor: http://${PRIMARY_IP}:${VIEWER_PORT}/"
echo " * Dedicated Directory: ${VIEWER_DIR} (isolated from root /tmp)"
echo " * Paper Stock:         ${SIMULATED_STOCK} sheets"
if [ -n "${SIMULATED_FAILURE}" ]; then
echo " * Simulated Failure:   ${SIMULATED_FAILURE} (sheet: ${SIMULATED_FAILURE_SHEET})"
fi
echo " * Completion Sound:    ${ENABLE_SOUND}"
echo "------------------------------------------------------------------------------"
echo " Interactive SSH Keypad Controls:"
echo "   [0-9]        Type 4-digit PIN"
echo "   [Enter]      Submit PIN / Confirm Order Summary"
echo "   [c] / [C]    Clear PIN buffer"
echo "   [q] / [Esc]  Cancel / Return to Idle Screen"
echo "   Ctrl+C       Stop Kiosk Daemon"
echo "=============================================================================="

# 6. Execute kiosk daemon
"${KIOSK_BIN}" \
  --mock-printer \
  --enable-keypad \
  --preview-png="${PREVIEW_PNG}" \
  --viewer-addr="0.0.0.0:${VIEWER_PORT}" \
  --cloud-url="${CLOUD_URL}" \
  --kiosk-id="${KIOSK_ID}" \
  --kiosk-secret="${KIOSK_SECRET}" \
  --state-db="${STATE_DB}" \
  --download-dir="${VIEWER_DIR}/jobs" \
  --simulated-stock="${SIMULATED_STOCK}" \
  --simulated-failure="${SIMULATED_FAILURE}" \
  --simulated-failure-sheet="${SIMULATED_FAILURE_SHEET}" \
  --enable-sound="${ENABLE_SOUND}"

