# XeroxGo Documentation

Welcome to the XeroxGo architecture and system documentation. This directory contains the complete technical discovery, system architecture, file inventory, module boundary definitions, and cleanup assessments for the XeroxGo monorepo.

---

## 1. Documentation Index

The documentation suite is structured into the following navigable references:

- 📖 **[System Architecture & Overview](file:///home/shabas/Projects/XeroxGo/docs/architecture/overview.md)**
  - Distributed multi-tier topology (Client, Edge, Cloud, Persistence, Edge Kiosk).
  - End-to-end workflow tracing (Upload $\rightarrow$ Preview $\rightarrow$ PIN $\rightarrow$ Claim $\rightarrow$ Payment $\rightarrow$ Dispatch $\rightarrow$ CUPS $\rightarrow$ Completion).
  - State machine models (`JobStatus`, `KioskSessionStatus`, `ConversionStatus`).
  - Cloud-owned continuation recovery architecture via `qpdf` and hardware telemetry.
  
- 🧱 **[Module Boundaries & Ownership](file:///home/shabas/Projects/XeroxGo/docs/architecture/module-boundaries.md)**
  - Formal definitions for all 10 logical modules: Frontend, Documents, Pricing, Orders, Payments, Dispatch, Kiosk Core, Printing, Recovery, Infrastructure.
  - Inter-module dependency graphs and directional data contracts.
  - Strict boundary invariants (Frozen contracts, CGO SQLite bans, Framebuffer rasterizer purity, Ingress proxy secrets).

- 📂 **[Tracked File Inventory](file:///home/shabas/Projects/XeroxGo/docs/reference/file-inventory.md)**
  - Exhaustive reference cataloging all 178 tracked files in the repository.
  - Detailed mapping of file path, logical module, purpose, exported functions/APIs, consumers/callers, and automated test coverage.

- 🧹 **[Module Inventory & Cleanup Assessment](file:///home/shabas/Projects/XeroxGo/docs/cleanup/module-inventory.md)**
  - Systematic classification of components: **CORE**, **SUPPORT**, **OPTIONAL**, **REDUNDANT**, and **UNREFERENCED**.
  - Comparison of observed behavior versus intended architectural behavior.
  - Missing test coverage, architectural uncertainties, and concrete cleanup candidates for future tasks.

- 🔄 **[Conversion Pipeline Redundancy Analysis](file:///home/shabas/Projects/XeroxGo/docs/cleanup/conversion-analysis.md)**
  - Comparative analysis of synchronous `POST /api/convert` vs asynchronous `POST /api/conversions`.
  - Concurrency throttle (`libreOfficeSem`), thread blocking, proxy gateway timeout risks, token security, and crash recovery analysis.
  - Completed 3-phase migration plan: frontend made async-only in Task 3.6A, legacy synchronous endpoint retired and removed in Task 3.6B, with Gate 6 verification.

- 🔍 **[Legacy Orders & Sessions Endpoint Review](file:///home/shabas/Projects/XeroxGo/docs/cleanup/orders-sessions-endpoints.md)**
  - Forensic mapping of all Order, PIN, Kiosk Session, and Payment Simulation endpoints across `cloud-backend`, `kiosk-daemon`, `frontend`, and operational scripts.
  - Comparative analysis of canonical REST routes vs legacy aliases (`/pin/lookup`, `/confirm`, `/cancel-session`, and order-scoped `/initiate-payment`).
  - Analysis of unreachable dead code in `session_handler.go:277-285` blocked by the HTTP router.
  - Assessment of `handleSimulatePayment` vs demo checkout (`/pay/{token}`); retention justified as a developer-only test harness for `run_local_demo.sh` and security isolation checks.
  - Verification of physical presence safety gates (`/release`) and distinct dual trust domains for customer vs kiosk recovery PIN generation.
  - 18-endpoint classification matrix and smallest safe cleanup proposal with regression test specifications.

- 🔀 **[Branch Integration Assessment & Plan](file:///home/shabas/Projects/XeroxGo/docs/cleanup/branch-integration-plan.md)**
  - Topological analysis and lineage verification for `main`, `bibin`, and `integration/agent-simplification`.
  - Detailed behavioral comparison between Bibin's UI preview/edit updates and the agent's async conversion pipeline.
  - Conflict analysis proving 0 merge conflicts, runtime compatibility audit, visual palette documentation, and merge execution strategy.

- ⚡ **[Minimum Print Path Workflow](file:///home/shabas/Projects/XeroxGo/docs/workflows/minimum-print-path.md)**
  - Exact execution trace for a single A4 PDF (1 copy, B&W, simplex).
  - Function-by-function, file-by-file, and database mutation trace.
  - Concrete identification of authorization gates, CUPS submission points, duplicate prevention mechanisms, and safety dependencies.

- 🛡️ **[Minimum Core Dependency Classification](file:///home/shabas/Projects/XeroxGo/docs/architecture/minimum-core.md)**
  - Direct and transitive dependency mapping across startup, config, database, cloud, and edge hardware.
  - 6-tier component classification: `CORE`, `SAFETY-CRITICAL`, `SUPPORT`, `OPTIONAL`, `LEGACY`, and `UNVERIFIED`.
  - Non-negotiable safety invariant deep-dive (webhook deduplication, mutual exclusion, paper reservation, SHA256 verification, restart re-attachment).
  - Overlapping implementation analysis (CUPS vs PJL, async vs sync conversions, cloud vs Pi slicing, pricing endpoints).
  - Crash window failure analysis, technical debt documentation, and minimal target architecture.

- 📜 **[Module Contracts & Behavioral Specifications](file:///home/shabas/Projects/XeroxGo/docs/architecture/module-contracts.md)**
  - Formal contracts for all 10 logical modules: boundaries, inputs, outputs, owned state, dependencies, and permitted side effects.
  - Explicit invariants for pricing, payments, orders, dispatch, kiosk execution, CUPS printing, and recovery.
  - Lifecycle success, failure, timeout, retry, and restart behaviors.
  - Independent simplification potential and recommended modular cleanup sequencing.

- 🧪 **[Core Acceptance Matrix & Invariant Verification](file:///home/shabas/Projects/XeroxGo/docs/testing/core-acceptance-matrix.md)**
  - Verification matrix mapping 45 monorepo invariants to Go test suites (38 verified, 7 unverified gaps; 84.4% verification health).
  - Explicit delineation of test tiers: Unit-Tested vs Integration-Tested (Mock Runner) vs Hardware-Verified.
  - Identification of missing test coverage across frontend PWA, hardware TTY/IOCTL, and operational scripts.
  - Concrete acceptance test specifications (`AT-FE-001`, `AT-FE-002`, `AT-INP-001`, `AT-FB-001`, `AT-SNMP-001`, `AT-PAY-001`, `AT-INF-001`) for all gaps.
  - Retraction of premature PJL deprecation; recommended simplification sequence targeting Documents (`/api/convert`), Pricing (`/api/pricing`), and Recovery (`StartFromPage`).

- 📑 **[Document Engine & Pricing Baseline Tests](file:///home/shabas/Projects/XeroxGo/docs/testing/document-pricing-baseline.md)**
  - Authoritative behavioral baseline test matrix across 10 deterministic test suites.
  - Verification of strict ISO A4 geometry, N-up modes (1, 2, 4, 6), duplex blank padding, and collation order preservation.
  - 4-tier integer paise pricing, color disabled fallback, and order price immutability post-creation.
  - Semantic equivalence analysis between `/api/documents/preview` and `/api/k/{id}/upload`, documenting Ghostscript metadata non-determinism and API field naming discrepancies.

- 🔍 **[Orders & Sessions Architecture Audit](file:///home/shabas/Projects/XeroxGo/docs/architecture/orders-sessions-audit.md)**
  - Comprehensive architectural audit of Order, PIN, Kiosk Session, Paper Reservation, Payment Authorization, and Expiration logic.
  - End-to-end lifecycle sequence and state transition matrix across 9 distinct order statuses.
  - Verification of 6 non-negotiable safety invariants (atomic PIN collision retry, paper tray overcommitment gating, anti-ghost-print physical presence gate, webhook deduplication, local spooler re-print prevention, and unacknowledged intent reconciliation).
  - Forensic analysis of duplicate API routing aliases, legacy simulation code vs demo checkout, PostgreSQL phantom read risks, and missing background expiration pruning.
  - Proposed smallest reliable architecture and prioritized 4-phase consolidation roadmap.

- ⚙️ **[Orders & Sessions Concurrency & Transaction Safety](file:///home/shabas/Projects/XeroxGo/docs/testing/orders-sessions-concurrency.md)**
  - Empirical concurrency baseline and transaction safety analysis for `CreateOrGetKioskSessionWithPaperCheck`.
  - Contrast of SQLite single-connection serialization (`MaxOpenConns=1`) vs PostgreSQL multi-connection pooled execution (`MaxOpenConns=25`, `READ COMMITTED`).
  - Empirical proof and remediation of PostgreSQL race vulnerabilities (duplicate active sessions and tray paper overcommitment phantom reads).
  - Database-level partial unique index `idx_kiosk_sessions_active_order` and preflight duplicate active session detection.
  - Transaction-scoped PostgreSQL advisory locks (`pg_advisory_xact_lock`) and safe uniqueness conflict recovery.
  - Status documentation: SQLite invariants verified; live PostgreSQL guarantees unverified in local CI.

---

## 2. Project Overview

**XeroxGo** is a self-service campus printing kiosk platform. It enables students and faculty to upload documents from mobile devices, customize print settings (duplex, page ranges, color), receive a 4-digit numeric PIN, and execute automated physical printing at a dedicated Raspberry Pi kiosk.

### Hardware Printer Tiers:
- **Production Target:** Brother HL-L5210DN (enterprise duplex monochrome laser) connected directly to Raspberry Pi 4 over Ethernet (`192.168.99.2:9100`) using raw PJL/PCL streaming and SNMP RFC 3805 telemetry.
- **Current Demo System:** HP LaserJet Tank 1020w connected over USB, managed via CUPS queue (`HPTank`) and IPP/SNMP telemetry.
- **Earlier Test System:** Brother DCP-T310 connected over USB, managed via CUPS queue (`DCPT310`).

### Core Architectural Highlights:
1. **Edge Hardware Autonomy:** The kiosk daemon runs natively on Raspberry Pi OS. It renders status screens directly to the Linux framebuffer (`/dev/fb0`) in pure Go with zero CGO or window manager dependencies, captures keypad inputs from physical TTYs, and spools jobs to CUPS queues or direct Ethernet PJL sockets.
2. **Deterministic Sheet-Level Accounting:** Jobs are tracked and confirmed using physical hardware sensors (IPP sheets-completed and SNMP marker life odometer) rather than blind timer heuristics.
3. **Pessimistic Paper Stock Tracking:** The system models a 250-sheet paper tray. Orders actively reserve paper during kiosk transaction sessions, rejecting competing claims if paper supply is exhausted.
4. **Cloud-Owned Sheet-Boundary Recovery:** When printing is interrupted (jam, cover open, paper out), a cloud service slices the canonical PDF using `qpdf` starting at the exact physical sheet boundary, requiring zero slicing or manipulation on the Raspberry Pi.
5. **Frozen API Contracts:** Shared models in `pkg/contracts/models.go` are cryptographically verified against a baseline commit hash, ensuring mutual wire compatibility across all tiers.
6. **Filesystem Unlinking Hygiene:** Completed or failed job PDF payloads are immediately unlinked (`os.Remove`) from the kiosk local disk to prevent local data retention. (Standard Linux filesystem unlinking, not cryptographic disk shredding).

---

## 3. Verification Gate Status

The repository enforces a 6-stage verification gate via [`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh). The current verification status:

```text
Starting XeroxGo Monorepo Verification Gate...
Working directory: /home/shabas/Projects/XeroxGo

=== Gate 1: Frozen Contracts ===
Gate 1 PASSED: pkg/contracts/models.go matches frozen baseline from commit 7a537f6a708da945ffbed07182a0cbaea03327f7 (30360cd42feef54d4e4890fbfcc7ac5dfddd3710f03ddb8170be913830cc455a).

=== Gate 2: Ban CGO SQLite Driver ===
Gate 2 PASSED: Zero occurrences of 'mattn/go-sqlite3' across 95 Go source files.

=== Gate 3: Go Vet ===
Gate 3 PASSED: go vet reported no issues.

=== Gate 4: Required Builds ===
Building kiosk daemon (linux/arm64, CGO_ENABLED=0)...
Building cloud backend (host, CGO_ENABLED=0)...
Gate 4 PASSED: Both required binaries built successfully.

=== Gate 5: Race-Enabled Test Suite ===
Gate 5 PASSED: All tests passed with race detector enabled.

=== Gate 6: Frontend Conversion Test Suite ===
Gate 6 PASSED: Frontend conversion tests passed.

=== Verification Summary ===
✅ ALL GATES PASSED
```

All 6 verification gates pass with zero regressions or errors across all test suites, including dedicated pricing engine unit tests, document preparation tests, frontend conversion tests, and baseline test suites.

