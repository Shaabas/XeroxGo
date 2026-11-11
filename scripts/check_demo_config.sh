#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Demo Configuration & Readiness Verifier
#
# Validates environment secrets, network addresses, cloud status,
# mobile payment accessibility, mock failure parameters, and Pi connectivity
# ahead of the demonstration.
# ==============================================================================
set -euo pipefail

# ANSI color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

ERRORS=0
WARNINGS=0

pass() {
  echo -e "  [${GREEN}OK${NC}] $1"
}

warn() {
  echo -e "  [${YELLOW}WARN${NC}] $1"
  WARNINGS=$((WARNINGS + 1))
}

fail() {
  echo -e "  [${RED}FAIL${NC}] $1"
  ERRORS=$((ERRORS + 1))
}

info() {
  echo -e "  [${BLUE}INFO${NC}] $1"
}

echo "=============================================================================="
echo " XeroxGo Demo Configuration Verification"
echo "=============================================================================="

# 1. Environment & Secrets Check
echo ""
echo "1. Checking Secrets and Environment Variables..."
DEMO_MODE="${DEMO_MODE:-${XEROXGO_DEMO_MODE:-0}}"
DEV_MODE="${DEV_MODE:-${XEROXGO_DEV_MODE:-0}}"

if [ "$DEMO_MODE" = "1" ] || [ "$DEMO_MODE" = "true" ]; then
  pass "Demo mode is explicitly ENABLED (DEMO_MODE=$DEMO_MODE)"
else
  warn "Demo mode is not enabled in environment (DEMO_MODE is not 1/true). Ensure --demo-mode flag is passed to cloud server."
fi

DOWNLOAD_SECRET="${XEROXGO_DOWNLOAD_SECRET:-}"
WEBHOOK_SECRET="${XEROXGO_WEBHOOK_SECRET:-}"
KIOSK_SECRET="${XEROXGO_KIOSK_SECRET:-}"

if [ -n "$DOWNLOAD_SECRET" ]; then
  pass "XEROXGO_DOWNLOAD_SECRET is set (${#DOWNLOAD_SECRET} chars)"
else
  if [ "$DEV_MODE" = "1" ] || [ "$DEMO_MODE" = "1" ]; then
    warn "XEROXGO_DOWNLOAD_SECRET unset; server will auto-generate in dev/demo mode"
  else
    fail "XEROXGO_DOWNLOAD_SECRET is required in production mode"
  fi
fi

if [ -n "$WEBHOOK_SECRET" ]; then
  pass "XEROXGO_WEBHOOK_SECRET is set (${#WEBHOOK_SECRET} chars)"
else
  if [ "$DEV_MODE" = "1" ] || [ "$DEMO_MODE" = "1" ]; then
    warn "XEROXGO_WEBHOOK_SECRET unset; server will auto-generate in dev/demo mode"
  else
    fail "XEROXGO_WEBHOOK_SECRET is required in production mode"
  fi
fi

if [ -n "$KIOSK_SECRET" ]; then
  pass "XEROXGO_KIOSK_SECRET is set (${#KIOSK_SECRET} chars)"
else
  warn "XEROXGO_KIOSK_SECRET unset; defaulting to dev_kiosk_secret_32_bytes_long_1234"
fi

# 2. Payment Provider Check
echo ""
echo "2. Checking Payment Provider Configuration..."
RAZORPAY_KEY_ID="${RAZORPAY_KEY_ID:-${XEROXGO_RAZORPAY_KEY_ID:-}}"
if [ -n "$RAZORPAY_KEY_ID" ] && [ "$DEMO_MODE" != "1" ]; then
  warn "Razorpay Key ID is present ($RAZORPAY_KEY_ID). Production provider would be used unless DEMO_MODE=1 or --demo-mode is specified."
else
  pass "MockPaymentProvider active for safe demonstration (no real currency involved)"
fi

# 3. Mobile QR Checkout Network Accessibility Check
echo ""
echo "3. Checking Mobile QR Checkout URL (Phone Accessibility)..."
PUBLIC_URL="${XEROXGO_PUBLIC_URL:-${PUBLIC_URL:-}}"
if [ -z "$PUBLIC_URL" ]; then
  warn "XEROXGO_PUBLIC_URL is unset. Checkout QR will default to http://<cloud-addr>/pay/{token}"
else
  info "Configured public checkout URL: $PUBLIC_URL"
  if echo "$PUBLIC_URL" | grep -qE "localhost|127\.0\.0\.1"; then
    warn "XEROXGO_PUBLIC_URL uses localhost/127.0.0.1. A mobile phone on Wi-Fi will NOT be able to open this QR code. For physical phone scanning, use your laptop's LAN IP (e.g. http://192.168.1.X:8080) or an HTTPS tunnel (e.g. cloudflared/ngrok)."
  else
    pass "Public URL is non-local ($PUBLIC_URL); suitable for mobile phone scanning"
  fi
fi

# 4. Live Cloud Server Connectivity Check (if running)
echo ""
echo "4. Checking Cloud Server Connectivity..."
CLOUD_ADDR="${XEROXGO_CLOUD_ADDR:-${PORT:-8080}}"
if [[ "$CLOUD_ADDR" =~ ^[0-9]+$ ]]; then
  CLOUD_HTTP_URL="http://127.0.0.1:${CLOUD_ADDR}"
elif [[ "$CLOUD_ADDR" =~ ^:([0-9]+)$ ]]; then
  CLOUD_HTTP_URL="http://127.0.0.1:${BASH_REMATCH[1]}"
elif [[ "$CLOUD_ADDR" =~ ^http ]]; then
  CLOUD_HTTP_URL="$CLOUD_ADDR"
else
  CLOUD_HTTP_URL="http://${CLOUD_ADDR}"
fi

HEALTH_STATUS=$(python3 -c "
import urllib.request, json
try:
    with urllib.request.urlopen('${CLOUD_HTTP_URL}/healthz', timeout=2) as resp:
        print(resp.status)
except Exception as e:
    print('UNREACHABLE')
" 2>/dev/null || echo "UNREACHABLE")

if [ "$HEALTH_STATUS" = "200" ]; then
  pass "Cloud server is running and healthy at ${CLOUD_HTTP_URL}/healthz (HTTP 200)"

  # Verify demo mode payment safety: /simulate-payment must return 403 Forbidden in demo mode
  SIMULATE_STATUS=$(python3 -c "
import urllib.request
try:
    req = urllib.request.Request('${CLOUD_HTTP_URL}/api/orders/non-existent-probe/simulate-payment', data=b'', method='POST')
    with urllib.request.urlopen(req, timeout=2) as resp:
        print(resp.status)
except urllib.error.HTTPError as e:
    print(e.code)
except Exception:
    print('ERROR')
" 2>/dev/null || echo "ERROR")

  if [ "$SIMULATE_STATUS" = "403" ]; then
    pass "Demo payment isolation verified: unauthenticated simulate-payment endpoint is blocked (HTTP 403 Forbidden)"
  elif [ "$SIMULATE_STATUS" = "404" ] || [ "$SIMULATE_STATUS" = "ERROR" ]; then
    info "Simulate payment probe returned status: $SIMULATE_STATUS"
  else
    warn "Simulate payment returned unexpected status: $SIMULATE_STATUS"
  fi
else
  info "Cloud server is not currently running at ${CLOUD_HTTP_URL} (this is normal if starting fresh)"
fi

# 5. Raspberry Pi & Kiosk Connectivity Check (optional)
echo ""
echo "5. Checking Raspberry Pi & Kiosk Connectivity..."
PI_HOST="${PI_HOST:-}"
if [ -n "$PI_HOST" ]; then
  info "Probing Raspberry Pi host: ${PI_HOST}"
  if ping -c 1 -W 2 "${PI_HOST}" >/dev/null 2>&1; then
    pass "Raspberry Pi (${PI_HOST}) is reachable via ICMP ping"
  else
    fail "Cannot ping Raspberry Pi (${PI_HOST})"
  fi

  if nc -z -w 2 "${PI_HOST}" 22 >/dev/null 2>&1; then
    pass "SSH service (port 22) is open on Raspberry Pi (${PI_HOST})"
  else
    warn "SSH port 22 not reachable on ${PI_HOST}"
  fi

  VIEWER_PORT="${VIEWER_PORT:-8081}"
  if nc -z -w 2 "${PI_HOST}" "${VIEWER_PORT}" >/dev/null 2>&1; then
    pass "Live kiosk monitor viewer (port ${VIEWER_PORT}) is accessible on ${PI_HOST}"
  else
    info "Live viewer port ${VIEWER_PORT} not currently listening on ${PI_HOST} (daemon may not be started yet)"
  fi
else
  info "PI_HOST is not set; skipping remote hardware probe. (Set PI_HOST=<ip> to verify remote Raspberry Pi connectivity)"
fi

# 6. Binary & Directory Prerequisites Check
echo ""
echo "6. Checking Binaries & Tooling Prerequisites..."
for tool in go python3 gs qpdf; do
  if command -v "$tool" >/dev/null 2>&1; then
    pass "Tool '$tool' found: $(command -v "$tool")"
  else
    fail "Required tool '$tool' is missing in PATH"
  fi
done

VIEWER_DIR="${VIEWER_DIR:-/tmp/xeroxgo-viewer}"
if [ "$VIEWER_DIR" = "/tmp" ] || [ "$VIEWER_DIR" = "/tmp/" ]; then
  fail "VIEWER_DIR cannot be bare root '/tmp'; must be an isolated directory such as /tmp/xeroxgo-viewer"
else
  pass "Viewer directory is safely isolated: ${VIEWER_DIR}"
fi

echo ""
echo "=============================================================================="
if [ "$ERRORS" -gt 0 ]; then
  echo -e " ${RED}Configuration Check Failed with ${ERRORS} error(s) and ${WARNINGS} warning(s).${NC}"
  echo " Please address the critical items above before the demonstration."
  exit 1
elif [ "$WARNINGS" -gt 0 ]; then
  echo -e " ${YELLOW}Configuration Check Passed with ${WARNINGS} warning(s).${NC}"
  echo " Review warnings above (e.g. LAN IP for mobile phone QR access)."
  exit 0
else
  echo -e " ${GREEN}All Demo Configuration Checks Passed! System is READY.${NC}"
  exit 0
fi

