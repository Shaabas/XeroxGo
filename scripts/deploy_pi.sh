#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
KIOSK_BIN="bin/xeroxgo-kiosk-arm64"

echo "=== Deploying XeroxGo Kiosk to ${PI_HOST} ==="

if [ ! -f "${KIOSK_BIN}" ]; then
    echo "Building ARM64 binary..."
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o "${KIOSK_BIN}" ./kiosk-daemon/cmd/daemon
fi

echo "1. Copying binary and service configuration to ${PI_HOST}..."
scp "${KIOSK_BIN}" "${PI_HOST}:/home/xg1/xeroxgo-kiosk.new"
scp "${SCRIPT_DIR}/../deploy/rpi-os/xeroxgo-kiosk.service" "${PI_HOST}:/tmp/xeroxgo-kiosk.service"

echo "2. Activating new binary and restarting kiosk on ${PI_HOST}..."
ssh -t "${PI_HOST}" "bash -c '
    sudo plymouth --quit 2>/dev/null || true
    echo 0 | sudo tee /sys/class/graphics/fb0/blank 2>/dev/null || true
    sudo chvt 1 2>/dev/null || true
    sudo systemctl stop xeroxgo-kiosk 2>/dev/null || true
    sudo killall -9 xeroxgo-kiosk 2>/dev/null || true
    mv /home/xg1/xeroxgo-kiosk.new /home/xg1/xeroxgo-kiosk
    chmod +x /home/xg1/xeroxgo-kiosk
    sudo cp /tmp/xeroxgo-kiosk.service /etc/systemd/system/xeroxgo-kiosk.service 2>/dev/null || true
    sudo systemctl daemon-reload 2>/dev/null || true
    if systemctl is-enabled xeroxgo-kiosk >/dev/null 2>&1; then
        sudo systemctl restart xeroxgo-kiosk
        echo \"xeroxgo-kiosk service restarted via systemd.\"
        sudo systemctl status xeroxgo-kiosk --no-pager || true
    else
        nohup /home/xg1/xeroxgo-kiosk \
            --kiosk-id=kiosk-demo \
            --cloud-url=wss://xeroxgo-cloud.onrender.com/ws \
            --cups-printer=DCPT310 \
            --framebuffer=/dev/fb0 \
            --idle-qr-content=https://xeroxgo.vercel.app \
            --enable-keypad=true \
            --keypad-device=/dev/tty1 > /tmp/xeroxgo-kiosk.log 2>&1 &
        echo \"xeroxgo-kiosk launched directly on /dev/fb0.\"
    fi
'"

echo "=== Deployment Complete! Check your kiosk display. ==="

