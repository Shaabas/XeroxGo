# XeroxGo Module Boundaries & Ownership

This document formally defines the 10 logical modules of the XeroxGo system, their strict responsibilities, component ownership, data boundaries, and dependency relationships.

---

## 1. System Module Architecture

```mermaid
flowchart TD
    subgraph Client["Client Tier (Mobile / Browser)"]
        FE["Frontend (PWA & Portal)"]
        ADM["Admin Dashboard"]
    end

    subgraph Cloud["Cloud Backend (Render / Docker)"]
        DOC["Documents Module"]
        PRI["Pricing Module"]
        ORD["Orders Module"]
        PAY["Payments Module"]
        DIS["Dispatch Module (WS Hub)"]
        REC_CLOUD["Recovery Module (Cloud Slicer)"]
    end

    subgraph Storage["Persistence Tier"]
        DB_SQLITE["SQLite (Local/Dev)"]
        DB_PG["PostgreSQL (Supabase)"]
    end

    subgraph Kiosk["Edge Kiosk Daemon (Raspberry Pi OS)"]
        KDK["Kiosk Core (Daemon & FSM)"]
        PIN_CTL["PIN Controller"]
        SPOOL["Spooler Client"]
        FB_UI["Framebuffer Renderer (/dev/fb0)"]
        KSTATE["Local SQLite State (/var/kiosk/state.db)"]
        REC_KIOSK["Recovery Module (Kiosk Reconciler)"]
    end

    subgraph Hardware["Physical Hardware"]
        KEYPAD["Physical Keypad (TTY)"]
        SCREEN["HDMI Display (/dev/fb0)"]
        CUPS["CUPS Queue (IPP/lp)"]
        PJL["PJL RAW Socket (:9100)"]
        SNMP["SNMP Hardware Sensors (:161)"]
    end

    FE -->|HTTP Multipart Upload / Preview| DOC
    FE -->|Create Order / Reserve PIN| ORD
    ADM -->|Admin REST API| PRI
    ADM -->|Analytics & Export| ORD

    DOC -->|Normalized PDF & Sheets| ORD
    PRI -->|Resolve Unit & Total Price| ORD
    PAY -->|Authorize & Webhook Capture| ORD
    ORD -->|Persist Order & Sessions| Storage
    ORD -->|Trigger Dispatch Intent| DIS

    DIS <-->|WebSocket wss://| SPOOL
    KDK -->|Manage Event Loop| PIN_CTL
    KEYPAD -->|Key Events| PIN_CTL
    PIN_CTL -->|REST Claim / Initiate Payment| ORD
    PIN_CTL -->|Render State| FB_UI

    SPOOL -->|Download PDF & Verify SHA256| Cloud
    SPOOL -->|Record Sheet Progress| KSTATE
    SPOOL -->|Submit Print Job (CUPS)| CUPS
    SPOOL -.->|Direct Print Stream (PJL)| PJL
    CUPS -->|Monitor Execution| SNMP
    SPOOL -->|Render Status Screens| FB_UI
    FB_UI -->|Raw Pixels| SCREEN

    REC_KIOSK <-->|Continuation Job Request| REC_CLOUD
    REC_CLOUD -->|qpdf Sliced PDF| ORD
```

---

## 2. Module Definitions & Responsibilities

### 2.1 Frontend Module
- **Primary Responsibility:** User-facing mobile and desktop web portal, PWA install prompts, document preview, client-side print customization (color, duplex, copies, page range), dynamic pricing estimation, and admin management UI.
- **Components & Files:**
  - Customer Portal: [`frontend/index.html`](file:///home/shabas/Projects/XeroxGo/frontend/index.html), [`frontend/css/portal.css`](file:///home/shabas/Projects/XeroxGo/frontend/css/portal.css), [`frontend/js/portal.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js)
  - Mobile Payment Checkout: [`frontend/payment.html`](file:///home/shabas/Projects/XeroxGo/frontend/payment.html)
  - Admin Portal: [`frontend/admin.html`](file:///home/shabas/Projects/XeroxGo/frontend/admin.html), [`frontend/css/admin.css`](file:///home/shabas/Projects/XeroxGo/frontend/css/admin.css), [`frontend/js/admin.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/admin.js)
  - Static Assets & Vendor Libraries: [`frontend/vendor/pdf.min.mjs`](file:///home/shabas/Projects/XeroxGo/frontend/vendor/pdf.min.mjs), [`frontend/vendor/pdf-lib.min.js`](file:///home/shabas/Projects/XeroxGo/frontend/vendor/pdf-lib.min.js), [`frontend/manifest.webmanifest`](file:///home/shabas/Projects/XeroxGo/frontend/manifest.webmanifest), [`frontend/vercel.json`](file:///home/shabas/Projects/XeroxGo/frontend/vercel.json)
- **External Dependencies:** Mozilla PDF.js, PDF-Lib.
- **Invariants:**
  - The frontend never connects directly to the kiosk Raspberry Pi or physical printer.
  - All communication is routed through Vercel Edge reverse-proxy to the cloud backend.

### 2.2 Documents Module
- **Primary Responsibility:** High-fidelity conversion of arbitrary office files and images into PDF, layout imposition (1-up, 2-up, 4-up, 6-up), page range slicing, duplex blank back-page padding, and canonical PDF page count extraction.
- **Components & Files:**
  - Office Conversion: [`cloud-backend/internal/pdf/convert.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/convert.go)
  - Normalization & Imposition: [`cloud-backend/internal/pdf/normalizer.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/normalizer.go)
  - Conversion HTTP Handlers: [`cloud-backend/cmd/server/conversion_handlers.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/conversion_handlers.go)
  - Conversion Store: [`cloud-backend/internal/store/conversions.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/conversions.go)
- **Underlying Binaries:** LibreOffice (`soffice`), Poppler (`pdfinfo`), CUPS filter (`pdftopdf`), Ghostscript (`gs`).
- **Invariants:**
  - Concurrency for LibreOffice conversions is strictly bounded (`libreOfficeSem` channel with capacity 1) to prevent out-of-memory crashes on resource-constrained cloud containers ([`convert.go:42`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/convert.go#L42)).
  - Duplex jobs with odd page counts must always be padded with a trailing blank page so that subsequent copies start on an odd page ([`normalizer.go:377-410`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/normalizer.go#L377-L410)).

### 2.3 Pricing Module
- **Primary Responsibility:** Calculation and maintenance of multi-tier printing tariffs: B&W Simplex, B&W Duplex, Color Simplex, Color Duplex, volume tier discounts, and administrative pricing overrides.
- **Components & Files:**
  - Data Store & KPIs: [`cloud-backend/internal/store/analytics.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/analytics.go)
  - Pricing Calculation: [`cloud-backend/cmd/server/main.go:210-239`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L210-L239) (`resolveUnitPrice`)
  - HTTP Endpoints: [`cloud-backend/cmd/server/admin_handlers.go:15-114`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/admin_handlers.go#L15-L114) (`handlePublicPricing`, `handleAdminPricing`)
- **Invariants:**
  - Unit price is always represented in integer paise (1 INR = 100 paise) to prevent floating-point calculation errors.
  - Price per sheet for duplex defaults to 75% of simplex price if not explicitly configured ([`analytics.go:66-71`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/analytics.go#L66-L71)).

### 2.4 Orders Module
- **Primary Responsibility:** Order creation, 4-digit numeric PIN allocation, reservation collision retry, kiosk-scoped PIN lookup, paper tray capacity checks, transaction session locking, and order status transitions.
- **Components & Files:**
  - Contracts: [`pkg/contracts/models.go`](file:///home/shabas/Projects/XeroxGo/pkg/contracts/models.go)
  - Order Management: [`cloud-backend/cmd/server/main.go:241-859`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L241-L859) (`handleUpload`), [`cloud-backend/cmd/server/pin_lookup_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go), [`cloud-backend/cmd/server/session_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go)
  - Rate Limiter: [`cloud-backend/cmd/server/pin_limiter.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_limiter.go)
  - Store Operations: [`cloud-backend/internal/store/db.go:853-1510`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go#L853-L1510)
- **Invariants:**
  - PIN collisions are actively prevented at generation time: a unique PIN is reserved for a specific kiosk until expiration ([`main.go:123-146`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L123-L146)).
  - PIN lookups are strictly rate-limited and return uniform 404 responses regardless of whether the PIN does not exist, belongs to another kiosk, or has expired, preventing PIN enumeration oracles ([`pin_lookup_handler.go:39-45`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go#L39-L45)).
  - Kiosk sessions enforce mutual exclusion: only one active transaction can claim an order at any given time ([`session_handler.go:164-260`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L164-L260)).

### 2.5 Payments Module
- **Primary Responsibility:** Dynamic UPI payment generation, Razorpay Orders/Payments integration, mock provider emulation for dev/demo modes, webhook signature verification, payment attempt tracking, and holding states for expired sessions.
- **Components & Files:**
  - Provider Abstraction: [`cloud-backend/internal/payments/provider.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/provider.go)
  - Razorpay Implementation: [`cloud-backend/internal/payments/razorpay.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/razorpay.go), [`razorpay_client.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/razorpay_client.go)
  - Mock Provider: [`cloud-backend/internal/payments/mock.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/payments/mock.go)
  - Demo Checkout: [`cloud-backend/cmd/server/demo_pay_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/demo_pay_handler.go)
  - Webhook Handler: [`cloud-backend/cmd/server/main.go:1417-1630`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1417-L1630)
- **Invariants:**
  - Every payment webhook must verify cryptographic HMAC signatures before processing ([`main.go:1432-1445`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1432-L1445)).
  - Duplicate webhooks are rejected idempotently via database primary key constraints on `payment_webhooks` ([`db.go:1733-1766`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go#L1733-L1766)).
  - If a payment arrives after a kiosk session expires, the order transitions to `PAID_WAITING` instead of auto-dispatching, allowing the customer to release or re-claim it ([`main.go:1537-1560`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1537-L1560)).

### 2.6 Dispatch Module
- **Primary Responsibility:** Real-time bi-directional WebSocket communication between cloud and physical kiosks, dispatch intent tracking, automatic retry on network disconnection, and printer health aggregation.
- **Components & Files:**
  - Hub: [`cloud-backend/internal/ws/hub.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/ws/hub.go)
  - Dispatch Intents: [`cloud-backend/internal/store/db.go:2949-3075`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go#L2949-L3075)
- **Invariants:**
  - Kiosk connections require pre-shared secret authentication (`Authorization: Bearer <secret>` or URL query token) ([`hub.go:81-96`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/ws/hub.go#L81-L96)).
  - When a kiosk reconnects, any unacknowledged dispatch intents in the database are automatically reconciled and dispatched ([`hub.go:142-155`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/ws/hub.go#L142-L155)).

### 2.7 Kiosk Core Module
- **Primary Responsibility:** Raspberry Pi runtime daemon management, systemd watchdog heartbeats (`WATCHDOG=1`), physical keypad event routing, finite state machine coordination (IDLE → PIN_ENTRY → ORDER_SUMMARY → PAYMENT_PENDING), local SQLite state tracking, and procedural audio chimes.
- **Components & Files:**
  - Daemon Main: [`kiosk-daemon/cmd/daemon/main.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/cmd/daemon/main.go)
  - PIN Controller FSM: [`kiosk-daemon/internal/pin/controller.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/pin/controller.go)
  - Input System: [`kiosk-daemon/internal/input/input.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/input/input.go), [`keyboard.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/input/keyboard.go), [`simulated.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/input/simulated.go)
  - Local State: [`kiosk-daemon/internal/state/store.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/state/store.go)
  - Audio Chime: [`kiosk-daemon/internal/audio/audio.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/audio/audio.go)
  - Cloud Client: [`kiosk-daemon/internal/cloud/pin_client.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go)
  - Framebuffer Renderer: [`kiosk-daemon/internal/ui_ipc/renderer.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/renderer.go), [`draw_helpers.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/draw_helpers.go)
  - Spooler Client: [`kiosk-daemon/internal/spooler/client.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/spooler/client.go)
- **Invariants:**
  - Direct rendering to `/dev/fb0` uses 100% pure Go software rasterization with zero CGO, X11, or Wayland dependencies.
  - Linux console cursor blinking is suppressed via VT100 escape codes and fbcon sysfs to maintain a clean commercial display ([`main.go:220-241`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/cmd/daemon/main.go#L220-L241)).
  - Kiosk state database enforces SQLite WAL mode and synchronous=FULL for power-loss durability ([`store.go:81-105`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/state/store.go#L81-L105)).

### 2.8 Printing Module
- **Primary Responsibility:** Physical document printing execution and status verification. Implements dual printing engines:
  1. **CUPS Engine:** Submits jobs to local/network CUPS queues (`lp`), monitors IPP job attributes (`ipptool` and `lpstat`), queries SNMP hardware sensors, and tracks physical sheet delivery via SNMP marker life odometer. Used for desktop tank and USB printers (earlier test Brother DCP-T310, demo HP LaserJet Tank 1020w).
  2. **PJL RAW Engine (Direct Socket 9100):** Streams PJL commands directly to port 9100, parses unsolicited `@PJL USTATUS PAGE` telemetry. Designed for the **production target Brother HL-L5210DN connected directly to Raspberry Pi 4 over Ethernet** (`192.168.99.2:9100`).
- **Components & Files:**
  - CUPS Execution & Monitoring: [`kiosk-daemon/internal/printer/cups.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/cups.go)
  - PJL RAW Socket: [`kiosk-daemon/internal/printer/pjl.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/pjl.go)
  - SNMP Poller: [`kiosk-daemon/internal/printer/snmp.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/snmp.go)
  - Combined Health Merger: [`kiosk-daemon/internal/printer/combined.go`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/combined.go)
- **Invariants:**
  - A job is only recorded as physically completed when confirmed by hardware telemetry (IPP sheets completed or SNMP odometer advancement) ([`cups.go:453-580`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/cups.go#L453-L580)).
  - Never overwrite a confirmed CUPS printing state with an unsupported SNMP state ([`combined.go:113-126`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/printer/combined.go#L113-L126)).

### 2.9 Recovery Module
- **Primary Responsibility:** Handling interrupted print jobs (jams, cover open, power loss, paper depletion). Cloud-owned continuation PDF generation via `qpdf` slices the canonical document to resume from exact physical sheet boundaries without Pi-side manipulation.
- **Components & Files:**
  - Cloud Slicer: [`cloud-backend/internal/pdf/recovery.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/recovery.go)
  - Cloud Recovery Handlers: [`cloud-backend/cmd/server/recovery_handler.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/recovery_handler.go)
  - Kiosk Recovery Reconciler: [`kiosk-daemon/internal/spooler/client.go:1689-1774`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/spooler/client.go#L1689-L1774)
  - Recovery Store: [`cloud-backend/internal/store/db.go:1511-1667`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/store/db.go#L1511-L1667)
- **Invariants:**
  - Slicing operates strictly on the canonical print-ready PDF, preserving all 1-up, 2-up, 4-up, 6-up impositions, rotations, and duplex margins.
  - Continuation jobs are generated with `start_from_page=1` and `copies=1`, requiring zero slicing or transformation on the Raspberry Pi ([`recovery.go:26-38`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/recovery.go#L26-L38)).
  - Physical output uncertainty prevents automatic resumption: continuation requires explicit reconciliation of delivered sheets ([`recovery_handler.go:123-132`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/recovery_handler.go#L123-L132)).

### 2.10 Infrastructure Module
- **Primary Responsibility:** Multi-stage container builds, deployment automation, OS silent boot configuration (Plymouth), CI/CD gate verification, and operational diagnosis scripts.
- **Components & Files:**
  - Scripts: [`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh), [`scripts/deploy_pi.sh`](file:///home/shabas/Projects/XeroxGo/scripts/deploy_pi.sh), [`scripts/setup_plymouth.sh`](file:///home/shabas/Projects/XeroxGo/scripts/setup_plymouth.sh), [`scripts/install_kiosk_service.sh`](file:///home/shabas/Projects/XeroxGo/scripts/install_kiosk_service.sh), [`scripts/check_demo_config.sh`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh), [`scripts/run_headless_pi.sh`](file:///home/shabas/Projects/XeroxGo/scripts/run_headless_pi.sh), [`scripts/run_local_demo.sh`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh)
  - Systemd Units: [`deploy/rpi-os/xeroxgo-kiosk.service`](file:///home/shabas/Projects/XeroxGo/deploy/rpi-os/xeroxgo-kiosk.service), [`deploy/rpi-os/xeroxgo-kiosk.default`](file:///home/shabas/Projects/XeroxGo/deploy/rpi-os/xeroxgo-kiosk.default)
  - Build & CI: [`Dockerfile`](file:///home/shabas/Projects/XeroxGo/Dockerfile), [`render.yaml`](file:///home/shabas/Projects/XeroxGo/render.yaml), [`.github/workflows/ci.yml`](file:///home/shabas/Projects/XeroxGo/.github/workflows/ci.yml)
  - Plymouth Theme: [`kiosk-daemon/plymouth/xeroxgo/*`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/plymouth/xeroxgo/)
- **Invariants:**
  - Verification gates enforce zero CGO SQLite drivers (`mattn/go-sqlite3`), frozen contract SHA validation, `go vet` cleanliness, and cross-compilation builds for ARM64.

---

## 3. Strict Boundary Invariants

| Invariant | Boundary | Enforcing Mechanism | Code Reference |
| :--- | :--- | :--- | :--- |
| **Contract Immutability** | Shared API Models | Gate 1 SHA256 checksum check against baseline commit `7a537f6a` | [`scripts/verify_gate.sh:17-55`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh#L17-L55) |
| **Zero CGO SQLite** | Storage Drivers | Gate 2 recursive grep scanning across all Go files for `mattn/go-sqlite3` | [`scripts/verify_gate.sh:61-92`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh#L61-L92) |
| **Direct Hardware Isolation** | Frontend $\rightarrow$ Kiosk | Network topology: frontend has no IP route or credentials for Raspberry Pi | [`frontend/vercel.json:3-12`](file:///home/shabas/Projects/XeroxGo/frontend/vercel.json#L3-L12) |
| **Kiosk Database Isolation** | Kiosk $\rightarrow$ Cloud DB | Kiosk daemon has no database credentials; interacts strictly via WebSocket and HTTPS APIs | [`kiosk-daemon/cmd/daemon/main.go:243-279`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/cmd/daemon/main.go#L243-L279) |
| **Framebuffer Purity** | UI Rasterizer | Pure Go drawing engine without X11, Wayland, or CGO graphics libraries | [`kiosk-daemon/internal/ui_ipc/renderer.go:1-60`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/ui_ipc/renderer.go#L1-L60) |
| **Signed PDF Downloads** | PDF Storage Access | HMAC-SHA256 URL signatures with strict Unix epoch expiration | [`cloud-backend/cmd/server/main.go:148-166`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L148-L166) |
| **Ingress Proxy Verification** | Cloud API Access | Frontend API routes require `X-XeroxGo-Proxy-Secret` header injected by Vercel Edge | [`cloud-backend/cmd/server/main.go:2008-2031`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L2008-L2031) |

