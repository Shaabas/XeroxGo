# XeroxGo — Self-Service Printing Kiosk System

Hardware Target:
- **Controller:** Raspberry Pi 4 (ARM64, Raspberry Pi OS Lite)
- **Printer:** Brother HL-L5210DN (Direct Ethernet, IPP/PJL/SNMP)
- **Display:** Non-touch HDMI Monitor (1080p KMS/DRM)

## Repository Structure
- `cloud-backend/` — Cloud API, PDF Sanitization Pipeline, Payment Webhooks
- `web-portal/` — Student Mobile Web Interface
- `kiosk-daemon/` — Raspberry Pi Local Hardware & Print Recovery Controller
- `kiosk-ui/` — Embedded Full-Screen Kiosk Display Application
- `pkg/contracts/` — Shared WebSocket & Job State Schemas
- `deploy/rpi-os/` — Systemd Units, Watchdogs, and Provisioning Scripts
