# XeroxGo Module Inventory & Cleanup Assessment

This document provides a comprehensive component classification of the XeroxGo codebase into **CORE**, **SUPPORT**, **OPTIONAL**, **REDUNDANT**, and **UNREFERENCED** categories. It records observed vs. intended behavior, missing test coverage, uncertainties, and actionable cleanup candidates for subsequent tasks.

---

## 1. Classification Methodology

Each tracked component and subsystem is evaluated against the primary mission: **Providing a reliable, hardened campus self-service printing kiosk with cloud document ingestion, dynamic UPI payment, and edge printer spooling on Raspberry Pi**.

- **CORE:** Indispensable to the primary end-to-end production workflow (upload → preview → payment → dispatch → CUPS → completion).
- **SUPPORT:** Build infrastructure, quality assurance gates, CI/CD pipelines, database schemas, and test fixtures.
- **OPTIONAL:** Ancillary business or operational features that can be disabled without impacting the core printing pipeline (e.g. ads carousel, live preview viewer, admin dashboard).
- **REDUNDANT:** Duplicate or superseded implementations where two distinct mechanisms exist for the same responsibility (e.g. PJL RAW socket vs CUPS IPP).
- **UNREFERENCED:** Code blocks, endpoints, or assets that are never called in production, superseded by newer APIs, or unreachable.

---

## 2. Component Classification Matrix

| Component / Subsystem | Path / File References | Classification | Primary Responsibility & Status |
| :--- | :--- | :--- | :--- |
| **Shared Contracts** | [`pkg/contracts/models.go`](file:///home/shabas/Projects/XeroxGo/pkg/contracts/models.go) | **CORE** | Frozen contract definitions (`JobStatus`, `PrintJobCommand`, `JobProgressEvent`, `PrinterHealth`). Enforced by Gate 1 baseline hash. |
| **Cloud Order API & Document Prep** | [`cloud-backend/cmd/server/main.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go), [`doc_prep.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/doc_prep.go) | **CORE** | Multipart upload (`/upload`), document preview (`/api/documents/preview`), signed PDF download (`/pdf`), status polling (`/status`), Razorpay webhook handler. |
| **Pricing Engine & Tariff Matrix** | [`cloud-backend/internal/pricing/pricing.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pricing/pricing.go) | **CORE** | Authoritative provider-independent 4-tier tariff resolution, integer paise calculation, duplex discounts, and overflow protection. |
| **PIN Lookup & Rate Limiter** | [`cloud-backend/cmd/server/pin_lookup_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go), [`pin_limiter.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_limiter.go) | **CORE** | Authenticated, rate-limited PIN lookup endpoint with uniform 404 security responses. |
| **Kiosk Transaction Sessions** | [`cloud-backend/cmd/server/session_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go) | **CORE** | Mutex session claiming, paper reservation checks, cancellation, and payment initiation. |
| **PDF Normalization & Imposition** | [`cloud-backend/internal/pdf/normalizer.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/normalizer.go) | **CORE** | Imposition (`pdftopdf`), A4 sizing, duplex blank back padding, SHA256 checksumming. |
| **Primary Persistence Store** | [`cloud-backend/internal/store/db.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go) | **CORE** | Dual-dialect storage engine (SQLite + Supabase PostgreSQL) managing orders, sessions, payments, and paper stock. |
| **WebSocket Relay Hub** | [`cloud-backend/internal/ws/hub.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/ws/hub.go) | **CORE** | Bi-directional communication hub relaying print jobs, health heartbeats, and reconnect reconciliation. |
| **Payment Providers** | [`cloud-backend/internal/payments/razorpay.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/razorpay.go), [`mock.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/mock.go) | **CORE** | Razorpay production provider and Mock testing provider. |
| **Kiosk Spooler Client** | [`kiosk-daemon/internal/spooler/client.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/spooler/client.go) | **CORE** | Central daemon client handling WS messaging, download verification, job execution, and screen mode switching. |
| **Keypad PIN Controller** | [`kiosk-daemon/internal/pin/controller.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/pin/controller.go) | **CORE** | FSM managing physical keypad events, cloud lookup, inactivity timeouts, and session handoffs. |
| **CUPS Print Engine** | [`kiosk-daemon/internal/printer/cups.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/cups.go) | **CORE** | Modern print execution engine submitting to CUPS queues via `lp`, monitoring IPP attributes and SNMP odometer. |
| **SNMP Hardware Telemetry** | [`kiosk-daemon/internal/printer/snmp.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/snmp.go) | **CORE** | Hardware sensor poller querying RFC 1213, RFC 2790, and RFC 3805 MIB OIDs for jam, paper out, and toner level. |
| **Combined Health Merger** | [`kiosk-daemon/internal/printer/combined.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/combined.go) | **CORE** | Merges CUPS queue states with physical SNMP hardware flags. |
| **Framebuffer UI Renderer** | [`kiosk-daemon/internal/ui_ipc/renderer.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/renderer.go), [`draw_helpers.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/draw_helpers.go) | **CORE** | Pure Go software rasterizer rendering all screens directly to `/dev/fb0` (RGB565 and BGRA). |
| **Kiosk Local SQLite Store** | [`kiosk-daemon/internal/state/store.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/state/store.go) | **CORE** | Local SQLite store (`/var/kiosk/state.db`) with WAL mode and synchronous=FULL. |
| **Customer Print Portal** | [`frontend/index.html`](file:///home/shabas/Projects/XeroxGo/frontend/index.html), [`frontend/js/portal.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js), [`frontend/css/portal.css`](file:///home/shabas/Projects/XeroxGo/frontend/css/portal.css) | **CORE** | Customer upload SPA with PDF.js preview, configuration controls, and PIN presentation. |
| **Verification Gate** | [`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh) | **SUPPORT** | Monorepo enforcement script verifying frozen contracts, banning CGO SQLite, running `go vet`, building ARM64/host, and running tests. |
| **CI/CD Pipeline** | [`.github/workflows/ci.yml`](file:///home/shabas/Projects/XeroxGo/.github/workflows/ci.yml) | **SUPPORT** | Automated GitHub Actions workflow running `./scripts/verify_gate.sh` and deploying frontend to Vercel. |
| **Database Schema DDL** | [`deploy/supabase/schema.sql`](file:///home/shabas/Projects/XeroxGo/deploy/supabase/schema.sql) | **SUPPORT** | Supabase PostgreSQL schema with table definitions, check constraints, and indexes. |
| **Systemd Service Assets** | [`deploy/rpi-os/xeroxgo-kiosk.service`](file:///home/shabas/Projects/XeroxGo/deploy/rpi-os/xeroxgo-kiosk.service), [`xeroxgo-kiosk.default`](file:///home/shabas/Projects/XeroxGo/deploy/rpi-os/xeroxgo-kiosk.default) | **SUPPORT** | Production systemd unit and environment configuration template for Raspberry Pi OS. |
| **Operational Shell Scripts** | [`scripts/deploy_pi.sh`](file:///home/shabas/Projects/XeroxGo/scripts/deploy_pi.sh), [`scripts/install_kiosk_service.sh`](file:///home/shabas/Projects/XeroxGo/scripts/install_kiosk_service.sh), [`scripts/check_demo_config.sh`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh), [`scripts/run_local_demo.sh`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh) | **SUPPORT** | Deployment, installation, pre-flight checks, and local demonstration harness scripts. |
| **Automated Test Suites** | All `*_test.go` files (50 test files) | **SUPPORT** | Monorepo test suites ensuring contract, database, spooler, and renderer correctness. |
| **Cloud Recovery Generator** | [`cloud-backend/internal/pdf/recovery.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/recovery.go), [`cloud-backend/cmd/server/recovery_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/recovery_handler.go) | **CORE / RECOVERY** | Cloud continuation PDF generator using `qpdf` with sheet boundary alignment and multi-copy handling. |
| **Office File Conversion** | [`cloud-backend/internal/pdf/convert.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/convert.go), [`cloud-backend/cmd/server/conversion_handlers.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/conversion_handlers.go), [`store/conversions.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/conversions.go) | **SUPPORT** | Headless LibreOffice conversion worker for DOCX/PPTX/XLSX/Image formats. |
| **Promotional Ads Subsystem** | [`pkg/contracts/ads.go`](file:///home/shabas/Projects/XeroxGo/pkg/contracts/ads.go), [`cloud-backend/internal/store/ads.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/ads.go), [`cloud-backend/cmd/server/ads_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/ads_handler.go) | **OPTIONAL** | Ad banner carousel displayed during kiosk idle mode. System functions cleanly without custom ads (falls back to 3 default promo cards). |
| **Admin Dashboard & Analytics** | [`frontend/admin.html`](file:///home/shabas/Projects/XeroxGo/frontend/admin.html), [`frontend/js/admin.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/admin.js), [`frontend/css/admin.css`](file:///home/shabas/Projects/XeroxGo/frontend/css/admin.css), [`cloud-backend/cmd/server/admin_handlers.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/admin_handlers.go), [`store/analytics.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/analytics.go) | **OPTIONAL** | Administrative management console for KPI charts, order export to CSV, and live pricing overrides. |
| **Kiosk Audio Chime** | [`kiosk-daemon/internal/audio/audio.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/audio/audio.go) | **OPTIONAL** | Procedural WAV sound synthesis and `aplay` execution. Automatically degrades to silent mode if audio hardware is absent. |
| **Live Browser Preview Viewer** | [`kiosk-daemon/internal/viewer/server.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/viewer/server.go) | **OPTIONAL** | Embedded HTTP server (`:8081`) streaming the current screen image for headless remote viewing. |
| **Plymouth Bootsplash Theme** | [`kiosk-daemon/plymouth/xeroxgo/*`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/plymouth/xeroxgo/), [`scripts/setup_plymouth.sh`](file:///home/shabas/Projects/XeroxGo/scripts/setup_plymouth.sh), [`scripts/deploy_plymouth_to_pi.sh`](file:///home/shabas/Projects/XeroxGo/scripts/deploy_plymouth_to_pi.sh), [`scripts/generate_plymouth_shimmer.py`](file:///home/shabas/Projects/XeroxGo/scripts/generate_plymouth_shimmer.py) | **OPTIONAL** | Silent boot splash screen for Raspberry Pi OS. Cosmetic only; kiosk operates normally without it. |
| **Demo Payment Checkout Page** | [`cloud-backend/cmd/server/demo_pay_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/demo_pay_handler.go), [`frontend/payment.html`](file:///home/shabas/Projects/XeroxGo/frontend/payment.html) | **OPTIONAL** | Web-based simulated UPI payment checkout for mobile testing and evaluation. |
| **Built-in Mock Printer Server** | [`kiosk-daemon/cmd/daemon/main.go:90-218`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/cmd/daemon/main.go#L90-L218) (`startMockPrinter`), [`mock_printer_test.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/cmd/daemon/mock_printer_test.go) | **OPTIONAL / SUPPORT** | Loopback TCP mock printer responding to PJL USTATUS commands, used for headless testing and demos without hardware. |
| **Direct PJL RAW 9100 Engine** | [`kiosk-daemon/internal/printer/pjl.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/pjl.go) | **CORE (ALT ENGINE)** | Direct TCP RAW socket printing on port 9100. Dedicated to the production target Brother HL-L5210DN connected directly to the Raspberry Pi 4 over Ethernet (`192.168.99.2:9100`). Alternates with CUPS subsystem (`cups.go`) used for USB demo printers. |
| **Synchronous Document Conversion API** | `cloud-backend/cmd/server/main.go` (`handleConvert`) | **REMOVED (Task 3.6B)** | Synchronous multipart conversion endpoint `/api/convert`. Retired and removed in Task 3.6B; superseded by asynchronous job pipeline (`/api/conversions` in `conversion_handlers.go`). |
| **Legacy Payment Simulation Endpoint** | [`cloud-backend/cmd/server/main.go:1244-1415`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1244-L1415) (`handleSimulatePayment`) | **UNREFERENCED / DEAD** | Endpoint `POST /api/orders/{id}/simulate-payment`. Superseded by demo payment handler (`/pay/{token}/submit`) and automated test suites. |
| **Local Spooler Ad Disk Caching** | [`kiosk-daemon/internal/spooler/client.go:1658-1687`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/spooler/client.go#L1658-L1687) (`saveAdsCache`, `loadAdsCache`) | **UNREFERENCED** | File caching routines for ads. Largely bypassed because the WebSocket hub pushes active ads directly upon connection via `sync_ads` message. |
| **Database SeedDefaultAds Cleanup** | [`cloud-backend/internal/store/ads.go:122-130`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/ads.go#L122-L130) (`SeedDefaultAds`) | **UNREFERENCED** | Legacy migration routine deleting old seeded ad IDs from database; default ads are now static code constants. |

---

## 3. Observed Behavior vs. Intended Behavior

Documenting specific discrepancies observed in the implementation against the architectural specification:

### 3.1 Frontend Automated Test Coverage
- **Intended Behavior:** Critical customer flows (upload, PDF page extraction, multi-file merging, pricing calculation, PIN display, and admin KPI updates) should be covered by continuous automated regression tests.
- **Observed Behavior:** There are **zero** automated tests for the frontend codebase (`frontend/js/portal.js` and `frontend/js/admin.js`, totaling 3,632 LOC). All regression testing currently relies on Go backend unit tests simulating multipart requests.

### 3.2 Tiered Printing Engines (CUPS vs Direct PJL)
- **Intended Behavior:** Support distinct printer tiers without compromising telemetry or reliability: USB-connected printers for local testing/demos and direct Ethernet network printers for campus production.
- **Observed Behavior:** Two parallel printing implementations exist:
  1. `kiosk-daemon/internal/printer/cups.go` (submits via CUPS `lp`, queries IPP status and SNMP marker odometer). Used for USB printers: earlier test Brother DCP-T310 and current demo HP LaserJet Tank 1020w (`HPTank`).
  2. `kiosk-daemon/internal/printer/pjl.go` (opens direct TCP socket to port 9100, streams PJL headers, parses text `@PJL USTATUS PAGE` strings). Designed for the production target Brother HL-L5210DN connected directly to the Raspberry Pi 4 over Ethernet (`192.168.99.2:9100`).
  Rather than being redundant, these engines serve distinct hardware connection topologies. Consolidation should only be considered if CUPS raw socket spooling is thoroughly verified on the production Brother HL-L5210DN.

### 3.3 Printer Health Offline Fallback
- **Intended Behavior:** Printer status should reflect the unified state of the queue and physical hardware.
- **Observed Behavior:** In [`kiosk-daemon/internal/printer/combined.go:80-95`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/combined.go#L80-L95), if CUPS is online but SNMP is unreachable (e.g. UDP port 161 blocked or SNMP disabled), the printer is still reported as `ONLINE` with state `IDLE`. This design choice prevents transient network hiccups from disabling the kiosk, but also means hardware errors (paper jam, cover open, toner empty) cannot be detected if SNMP is offline.

### 3.4 Pi-Side Slicing vs. Cloud Continuation Slicing
- **Intended Behavior:** The Raspberry Pi must never perform PDF page slicing or manipulation; all continuation slicing must be cloud-owned.
- **Observed Behavior:** Cloud continuation slicing via `qpdf` is fully implemented in [`cloud-backend/internal/pdf/recovery.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/recovery.go) and enabled by default (`--enable-cloud-recovery=true`). However, legacy command fields like `StartFromPage` in `PrintJobCommand` ([`pkg/contracts/models.go:52`](file:///home/shabas/Projects/XeroxGo/pkg/contracts/models.go#L52)) and fallback handling in `pjl.go:75-79` still exist.

---

## 4. Test Coverage Gaps & Uncertainties

### 4.1 Missing Automated Tests
1. **Frontend Web Portal & Admin Console:** No unit tests, Jest tests, or Playwright/Cypress end-to-end tests exist for `frontend/js/portal.js` or `frontend/js/admin.js`.
2. **Operational Shell Scripts:** None of the shell scripts in `scripts/` (`deploy_pi.sh`, `install_kiosk_service.sh`, `check_demo_config.sh`, `setup_plymouth.sh`) have automated integration tests or shellcheck linting in CI.
3. **Hardware-Level Keypad TTY Reading:** [`kiosk-daemon/internal/input/keyboard.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/input/keyboard.go) reads raw termios syscalls from `/dev/tty1`. It is tested via simulated mocks (`simulated.go`), but lacks an automated Linux virtual console PTY test.
4. **Physical Framebuffer Sync:** [`kiosk-daemon/internal/ui_ipc/renderer.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/renderer.go) writing to `/dev/fb0` is tested using simulated in-memory byte buffers and PNG sinks, but actual `FBIOPUT_VSCREENINFO` ioctl calls are only verified manually on hardware.

### 4.2 Architectural Uncertainties
1. **Payment Attempt Expiration Cleanup:** In [`cloud-backend/internal/store/db.go:3129-3367`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go#L3129-L3367), payment attempts are created with `expires_at_ns`, but there is no periodic cron job purging old abandoned attempts from `payment_attempts`.
2. **Multi-File PDF Collation Order:** In [`frontend/js/portal.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js), multiple files in the cart are merged client-side via PDF-Lib. If a user uploads files in arbitrary order, collation follows client array ordering without explicit re-ordering controls.

---

## 5. Concrete Cleanup Candidates for Future Tasks

The following candidate cleanup actions are identified for subsequent tasks (preserving the rule that no production code is modified during Task 1):

1. **Preserve Dual Printing Engines Across Hardware Tiers:** Retain `pjl.go` for the production target Brother HL-L5210DN Ethernet direct connection (`192.168.99.2:9100`), and `cups.go` for USB-attached printers (current demo HP LaserJet Tank 1020w). Premature removal of `pjl.go` is retracted; any engine consolidation must first verify that CUPS IPP/socket drivers provide identical or superior sheet-level telemetry on the Brother HL-L5210DN.
2. **Remove Unused Synchronous `/api/convert` Endpoint (COMPLETED in Task 3.6B):** Retired `handleConvert` and route registration `/api/convert` in `cloud-backend/cmd/server/main.go` in favor of the asynchronous `/api/conversions` pipeline; covered by Gate 6 and `conversion_test.go:TestConvertRoute_RetiredReturns404`.
3. **Remove Legacy Payment Simulation Endpoint:** Deprecate `handleSimulatePayment` in `main.go:1244-1415` in favor of `demo_pay_handler.go`.
4. **Add Automated Frontend Test Suite:** Introduce headless browser tests (e.g. Playwright or Puppeteer) to cover `frontend/js/portal.js` and `frontend/js/admin.js`.
5. **Periodic Database Pruning Job:** Implement a background worker to prune expired `pin_reservations`, `kiosk_sessions`, and `payment_attempts` past their TTL.

