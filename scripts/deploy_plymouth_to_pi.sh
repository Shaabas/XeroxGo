#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Kiosk — Remote Plymouth Theme Deployment Script
# ==============================================================================
# Copies the Plymouth theme assets and installer to the Raspberry Pi over SSH
# and executes the setup with root privileges.
#
# Usage:
#   ./scripts/deploy_plymouth_to_pi.sh [user@host]
#   ./scripts/deploy_plymouth_to_pi.sh xg1@10.252.178.225
# ==============================================================================
set -euo pipefail

# Auto-detect Pi on local ARP/neighbors if not provided
TARGET_ARG="${1:-}"
if [ -n "${TARGET_ARG}" ]; then
    PI_HOST="${TARGET_ARG}"
else
    DETECTED_IP="$(ip neigh 2>/dev/null | grep -E "d8:3a:dd|b8:27:eb|e4:5f:01|28:cd:c1" | awk '{print $1}' | head -n 1 || true)"
    if [ -n "${DETECTED_IP}" ]; then
        PI_HOST="xg1@${DETECTED_IP}"
    else
        PI_HOST="xg1@10.71.34.225"
    fi
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${SCRIPT_DIR}/.."
THEME_DIR="${REPO_ROOT}/kiosk-daemon/plymouth"
INSTALLER="${SCRIPT_DIR}/setup_plymouth.sh"

echo "=== Deploying XeroxGo Plymouth Boot Splash to ${PI_HOST} ==="

if [ ! -d "${THEME_DIR}/xeroxgo" ]; then
    echo "ERROR: Theme directory not found at ${THEME_DIR}/xeroxgo" >&2
    exit 1
fi

echo "1. Uploading theme package to ${PI_HOST}..."
ssh "${PI_HOST}" "mkdir -p /tmp/xeroxgo-plymouth/kiosk-daemon/plymouth /tmp/xeroxgo-plymouth/scripts"
scp -r "${THEME_DIR}/xeroxgo" "${PI_HOST}:/tmp/xeroxgo-plymouth/kiosk-daemon/plymouth/"
scp "${INSTALLER}" "${PI_HOST}:/tmp/xeroxgo-plymouth/scripts/setup_plymouth.sh"

echo "2. Running installer on ${PI_HOST}..."
ssh -t "${PI_HOST}" "sudo bash /tmp/xeroxgo-plymouth/scripts/setup_plymouth.sh"

echo ""
echo "=== Plymouth Deployment Finished ==="

