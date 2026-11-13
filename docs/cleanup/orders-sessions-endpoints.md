# XeroxGo Legacy Orders & Sessions Endpoint Review

> **Document Version:** 1.0.0  
> **Status:** Active / Architectural Review  
> **Task Scope:** Task 4.4 — Comprehensive audit, consumer mapping, and classification of Order, PIN, Session, and Payment Simulation endpoints across the XeroxGo platform.  
> **Constraint Notice:** This document is an analytical review. No production code, API routes, database schemas, test suites, or deployment configurations have been modified during this task.

---

## 1. Executive Summary

This review investigates the orders, PIN, kiosk session, and payment simulation endpoints across the XeroxGo monorepo to determine which endpoints are canonical, which are redundant aliases or legacy dead code, and which represent essential safety or backward-compatibility mechanisms.

### Key Audit Conclusions:
1. **Three Redundant API Aliases Identified:**
   - `POST /api/k/{kiosk_id}/pin/lookup` is a duplicate alias for `POST /api/k/{kiosk_id}/orders/lookup`.
   - `POST /api/k/{kiosk_id}/orders/{order_id}/confirm` is a duplicate alias for `POST /api/k/{kiosk_id}/orders/{order_id}/claim`.
   - `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel-session` is a duplicate alias for `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel`.
   - **Consumer Status:** The production `kiosk-daemon` and `frontend` use **zero** of these three aliases. They exist solely as backward-compatibility route branches in [`cloud-backend/cmd/server/main.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1480-L1491) and are exercised only by legacy test suites.

2. **Dead / Unreachable Code in Session Cancel:**
   - In [`cloud-backend/cmd/server/session_handler.go:277-285`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L277-L285), logic exists to parse an order-scoped cancel path: `/api/k/{kiosk_id}/orders/{order_id}/cancel-session`.
   - However, the router in [`main.go:1490`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1490) strictly guards session cancellation behind `case strings.Contains(path, "/sessions/"):`.
   - Consequently, any client requesting `/orders/{id}/cancel-session` receives HTTP 404 Not Found. This branch is completely unreachable dead code in production.

3. **Dual Payment Initiation Paths:**
   - `handleInitiatePayment` accepts both `/sessions/{session_id}/initiate-payment` (canonical) and `/orders/{order_id}/initiate-payment` (legacy alias).
   - The edge `kiosk-daemon` exclusively issues session-scoped requests (`/sessions/{session_id}/initiate-payment`). Payment attempts are architecturally bound to physical kiosk sessions. The order-scoped variant is redundant and can safely be merged/retired.

4. **Status of `handleSimulatePayment`:**
   - `handleSimulatePayment` (`POST /api/orders/{order_id}/simulate-payment`) is **NOT** redundant dead code that can be deleted immediately.
   - It serves as a vital developer test harness for headless integration testing ([`scripts/run_local_demo.sh:256`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh#L256)), developer UI tools ([`frontend/payment.html:406`](file:///home/shabas/Projects/XeroxGo/frontend/payment.html#L406)), and security boundary probes ([`scripts/check_demo_config.sh:140`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh#L140)).
   - It is strictly gated to `cfg.devMode == true && cfg.demoMode == false` (returning HTTP 403 Forbidden in production and demo modes).
   - It must be **KEPT** under strict development gating until local automation scripts are refactored to use tokenized demo checkout (`/pay/{token}`).

5. **Essential Safety Mechanisms Retained:**
   - `POST /api/k/{kiosk_id}/orders/{order_id}/release` is **NOT** a redundant route; it is the core anti-ghost-print physical presence gate preventing paid jobs from printing unattended.
   - `POST /api/orders/{order_id}/recovery-pin` and `POST /api/k/{kiosk_id}/orders/{order_id}/recovery-pin` serve two distinct trust domains (public customer portal using a bearer claim token vs edge kiosk hardware using a shared kiosk secret). Both must be **KEPT**.

---

## 2. Complete Orders & Sessions Endpoint Inventory

The following table catalogs every Order, PIN, Kiosk Session, and Payment Simulation endpoint implemented in `cloud-backend`:

| HTTP Method & Route | Target Handler & File | Auth / Security Boundary | Active Monorepo Consumers | Owned State & Database Mutations | Classification |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `POST /api/k/{kiosk_id}/orders/lookup` | `app.handlePINLookup`<br>[`pin_lookup_handler.go:46`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go#L46) | Kiosk Secret Bearer Token (`verifyKioskAuth`) + DB Rate Limiter | `kiosk-daemon` ([`pin_client.go:156`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L156)), `frontend/payment.html:437`, integration tests | Increments failure rate limit; resets on success. Read-only on `orders` and `pin_reservations`. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/pin/lookup` | `app.handlePINLookup`<br>[`pin_lookup_handler.go:46`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go#L46) | Kiosk Secret Bearer Token (`verifyKioskAuth`) + DB Rate Limiter | None in production. Only in [`kiosk_routing_regression_test.go:69`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/kiosk_routing_regression_test.go#L69) | Same as canonical. | **DEPRECATE $\rightarrow$ REMOVE** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/claim` | `app.handleOrderClaim`<br>[`session_handler.go:164`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L164) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | `kiosk-daemon` ([`pin_client.go:245`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L245)), baseline & concurrency tests | Inserts/retrieves `kiosk_sessions` with `PAYMENT_PENDING`. Protected by PostgreSQL partial unique index & advisory lock. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/confirm` | `app.handleOrderClaim`<br>[`session_handler.go:164`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L164) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | None in production. Tested in routing regression suites. | Same as canonical. | **DEPRECATE $\rightarrow$ REMOVE** |
| `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel` | `app.handleSessionCancel`<br>[`session_handler.go:262`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L262) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | `kiosk-daemon` ([`pin_client.go:302`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L302)), spooler pin flow tests | Mutates `kiosk_sessions.status` $\rightarrow$ `CANCELED`. Releases committed paper reservation. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel-session` | `app.handleSessionCancel`<br>[`session_handler.go:262`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L262) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | None in production. Tested in routing regression suites. | Same as canonical. | **DEPRECATE $\rightarrow$ REMOVE** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/cancel-session` | None (Unreachable in `main.go:1490`) | Kiosk Secret Bearer Token | None. Returns HTTP 404 in current router. | None. Dead code in `session_handler.go:277-285`. | **REMOVE (DEAD CODE)** |
| `GET /api/k/{kiosk_id}/sessions/{session_id}` | `app.handleGetSession`<br>[`session_handler.go:311`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L311) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | `kiosk-daemon` polling fallback, session tests | Read-only query on `kiosk_sessions`. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment` | `app.handleInitiatePayment`<br>[`session_handler.go:363`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L363) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | `kiosk-daemon` ([`pin_client.go:418`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L418)), spooler flow tests | Checks active session & fresh paper stock. Creates `payment_attempts`. Generates demo checkout token or Razorpay QR. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/initiate-payment` | `app.handleInitiatePayment`<br>[`session_handler.go:363`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L363) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | None in production. Tested in routing regression suites. | Same as session-scoped variant (resolves session via order lookup). | **MERGE / DEPRECATE ALIAS** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/release` | `app.handleOrderRelease`<br>[`session_handler.go:566`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L566) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | `kiosk-daemon` ([`pin_client.go:366`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L366)), `controller.go:429` | Validates session & physical presence. Mutates `orders.status` $\rightarrow$ `DISPATCHED`. Enqueues job to WebSocket hub. | **KEEP (CORE SAFETY)** |
| `POST /api/orders/{order_id}/recovery-pin` | `app.handleRecoveryPIN`<br>[`session_handler.go:671`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L671) | Customer Bearer Claim Token (`X-Recovery-Token`, `X-Claim-Token`, `Authorization`) | `frontend/js/portal.js:2102` | Validates customer claim token. Generates new 4-digit PIN with collision retry. Mutates `orders.display_code` & `pin_reservations`. | **KEEP (PORTAL RECOVERY)** |
| `POST /api/k/{kiosk_id}/orders/{order_id}/recovery-pin` | `app.handleRecoveryPIN`<br>[`session_handler.go:671`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L671) | Kiosk Secret Bearer Token + Claim Token | Edge kiosk pin recovery tests ([`payment_safety_test.go:707`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/payment_safety_test.go#L707)) | Same as portal endpoint with kiosk ownership check. | **KEEP (KIOSK RECOVERY)** |
| `POST /api/orders/{order_id}/simulate-payment` | `app.handleSimulatePayment`<br>[`main.go:786`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L786) | Development Mode Only (`cfg.devMode && !cfg.demoMode`) | [`scripts/run_local_demo.sh:256`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh#L256), [`scripts/check_demo_config.sh:140`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh#L140), `frontend/payment.html:406` | Injects synthetic webhook event. Captures payment, verifies dispatch eligibility, records webhook deduplication key. | **KEEP (DEV TEST HARNESS)** |
| `GET /pay/{token}` | `app.handleDemoCheckoutPage`<br>[`demo_pay_handler.go:54`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/demo_pay_handler.go#L54) | Demo Mode Only (`cfg.demoMode`); Public Tokenized Link | Mobile browser users scanning QR during live demos | Queries `payment_attempts` by `demo_token`. Returns dynamic HTML checkout UI or JSON metadata. | **KEEP (DEMO CHECKOUT)** |
| `POST /pay/{token}` | `app.handleDemoPaymentSubmit`<br>[`demo_pay_handler.go:174`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/demo_pay_handler.go#L174) | Demo Mode Only (`cfg.demoMode`); Public Tokenized Link | Mobile browser checkout button submit | Mutates `payment_attempts.status` $\rightarrow$ `PAID`. Captures payment in DB, dispatches job if eligible. | **KEEP (DEMO CHECKOUT)** |
| `GET /api/orders/{order_id}/status` | `app.handleOrderStatus`<br>[`main.go:983`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L983) | Public (Polled by client portal) | `frontend/js/portal.js:1540`, `scripts/run_local_demo.sh:268` | Read-only query on `orders`. Returns order status, printed sheets, completion timestamps. | **KEEP (CANONICAL)** |
| `GET /api/orders/{order_id}/pdf` | `app.handleDownloadPDF`<br>[`main.go:1037`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1037) | Kiosk Secret Bearer Token or Authorized Downloader | Kiosk daemon job spooler download | Streams canonical PDF payload from disk for printing. | **KEEP (CANONICAL)** |
| `POST /api/k/{kiosk_id}/refill` | `app.handleKioskRefill`<br>[`session_handler.go:802`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L802) | Kiosk Secret Bearer Token (`verifyKioskAuth`) | Kiosk operator maintenance interface | Sends calibration WebSocket command to reset kiosk hardware sheet estimate. | **KEEP (CANONICAL)** |

---

## 3. Detailed Comparative Analysis: Canonical vs. Legacy Routes

### 3.1 PIN Lookup: `/orders/lookup` vs. `/pin/lookup`

- **Canonical Route:** `POST /api/k/{kiosk_id}/orders/lookup`
- **Legacy Alias:** `POST /api/k/{kiosk_id}/pin/lookup`
- **Router Implementation:** [`cloud-backend/cmd/server/main.go:1480`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1480)
  ```go
  case strings.HasSuffix(path, "/orders/lookup"), strings.HasSuffix(path, "/pin/lookup"):
      app.handlePINLookup(w, r)
  ```
- **Handler Implementation:** [`cloud-backend/cmd/server/pin_lookup_handler.go:46-189`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/pin_lookup_handler.go#L46-L189)
- **Behavioral Comparison:**
  - Both routes execute the exact same function `handlePINLookup`.
  - Both enforce the same authentication: `Authorization: Bearer <kioskSecret>`.
  - Both enforce uniform rate limiting backed by `pin_failures` in the database.
  - Both enforce uniform HTTP 404 response timing and payload (`{"error": "invalid or expired PIN"}`) to defeat PIN enumeration or existence oracles.
  - Both return identical JSON structure: `OrderID`, `KioskID`, `Status`, `TotalPricePaise`, `TotalPages`, `TotalSheets`, `Copies`, `Duplex`, `ColorMode`, `PINExpiresAt`, `SecondsRemaining`.
- **Consumer Trace:**
  - `kiosk-daemon/internal/cloud/pin_client.go:156`: `endpoint := baseURL + "/orders/lookup"` (Canonical).
  - `frontend/payment.html:437`: `var localLookup = '/api/k/' + encodeURIComponent(targetKiosk) + '/orders/lookup';` (Canonical).
  - No active client or script in the monorepo calls `/pin/lookup`.
- **Verdict:** `/pin/lookup` is a pure legacy alias. It can be deprecated and safely removed without breaking any active frontend or edge software.

---

### 3.2 Order Claim: `/orders/{id}/claim` vs. `/orders/{id}/confirm`

- **Canonical Route:** `POST /api/k/{kiosk_id}/orders/{order_id}/claim`
- **Legacy Alias:** `POST /api/k/{kiosk_id}/orders/{order_id}/confirm`
- **Router Implementation:** [`cloud-backend/cmd/server/main.go:1482`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1482)
  ```go
  case strings.HasSuffix(path, "/claim"), strings.HasSuffix(path, "/confirm"):
      app.handleOrderClaim(w, r)
  ```
- **Handler Implementation:** [`cloud-backend/cmd/server/session_handler.go:164-257`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L164-L257)
- **Behavioral Comparison:**
  - Both routes execute `handleOrderClaim`.
  - The handler inspects the URL path only to strip the matching suffix (`/claim` or `/confirm`) when extracting the order ID.
  - Both invoke `CreateOrGetKioskSessionWithPaperCheck` with identical arguments, paper checks, advisory locks, and expiration bounds (5 minutes).
  - Both return identical HTTP response status codes and JSON payloads (`sessionResponse`).
- **Consumer Trace:**
  - `kiosk-daemon/internal/cloud/pin_client.go:245`: `claimURL := fmt.Sprintf("%s/orders/%s/claim", c.baseURL, url.PathEscape(orderID))` (Canonical).
  - `kiosk-daemon/internal/spooler/pin_flow_test.go:413,547,797`: Uses `/claim` (Canonical).
  - `/confirm` appears only in legacy routing regression tests.
- **Verdict:** `/confirm` is an obsolete route name from early prototyping. It can be deprecated and safely removed.

---

### 3.3 Session Cancellation: `/sessions/{id}/cancel` vs. `/cancel-session`

- **Canonical Route:** `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel`
- **Legacy Aliases:**
  - `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel-session`
  - `POST /api/k/{kiosk_id}/orders/{order_id}/cancel-session` (Dead Code)
- **Router Implementation:** [`cloud-backend/cmd/server/main.go:1490`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1490)
  ```go
  case strings.Contains(path, "/sessions/") && (strings.HasSuffix(path, "/cancel") || strings.HasSuffix(path, "/cancel-session")):
      app.handleSessionCancel(w, r)
  ```
- **Handler Implementation:** [`cloud-backend/cmd/server/session_handler.go:262-307`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L262-L307)
- **Forensic Defect & Dead Code Discovery:**
  - `session_handler.go:277-285` contains code attempting to support order-scoped cancellation:
    ```go
    } else if strings.HasSuffix(r.URL.Path, "/cancel-session") {
        var orderID string
        kioskID, orderID, err = parseKioskAndOrder(r.URL.Path, "/cancel-session")
        if err == nil {
            activeSess, _ := s.db.GetActiveKioskSession(r.Context(), kioskID, orderID, now)
            if activeSess != nil {
                sessionID = activeSess.SessionID
            }
        }
    }
    ```
  - However, because `main.go:1490` specifies `strings.Contains(path, "/sessions/")`, any request to `/api/k/{kiosk_id}/orders/{order_id}/cancel-session` will **never** match this case branch! It falls through to `default: http.NotFound(w, r)`.
  - Furthermore, if a client requests `/api/k/{kiosk_id}/sessions/{session_id}/cancel-session`, line 276 executes `parseKioskAndSession(r.URL.Path, "/cancel")`. Because the suffix was `cancel-session`, trimming `/cancel` leaves `session_id` contaminated with `-session`, causing database lookup to fail with HTTP 404!
- **Consumer Trace:**
  - `kiosk-daemon/internal/cloud/pin_client.go:302`: `cancelURL := fmt.Sprintf("%s/sessions/%s/cancel", c.baseURL, url.PathEscape(sessionID))` (Canonical).
  - Edge kiosk software has always used canonical `/sessions/{session_id}/cancel`.
- **Verdict:** `/cancel-session` is broken dead code. The alias must be **REMOVED** from `main.go` and the dead fallback removed from `session_handler.go`.

---

### 3.4 Payment Initiation: `/sessions/{id}/initiate-payment` vs. `/orders/{id}/initiate-payment`

- **Canonical Route:** `POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment`
- **Legacy Order-Scoped Route:** `POST /api/k/{kiosk_id}/orders/{order_id}/initiate-payment`
- **Router Implementation:** [`cloud-backend/cmd/server/main.go:1488`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1488)
  ```go
  case strings.HasSuffix(path, "/initiate-payment"):
      app.handleInitiatePayment(w, r)
  ```
- **Handler Implementation:** [`cloud-backend/cmd/server/session_handler.go:363-562`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L363-L562)
- **Behavioral Comparison:**
  - In the session-scoped path:
    1. The URL explicitly provides `session_id`.
    2. The handler looks up the session and retrieves its `order_id`.
    3. The payment attempt is created explicitly for that `session_id`.
  - In the order-scoped path:
    1. The URL provides `order_id`.
    2. The handler must query the database to find an active session (`GetActiveKioskSession`). If no active session exists, it returns HTTP 409 Conflict.
- **Architectural Principle:**
  - Payment is an action taken within an active physical session. Binding the endpoint to `/sessions/{session_id}/initiate-payment` structurally enforces that payment initiation cannot bypass the session lifecycle.
- **Consumer Trace:**
  - `kiosk-daemon/internal/cloud/pin_client.go:418`: `initiateURL := fmt.Sprintf("%s/sessions/%s/initiate-payment", c.baseURL, url.PathEscape(sessionID))` (Canonical).
  - All kiosk tests (`pin_flow_test.go:557,637`, `pin_client_test.go:238`) use the session-scoped route.
- **Verdict:** Consolidate on the session-scoped route. Deprecate and remove the order-scoped URL branch.

---

## 4. Payment Simulation Review: `handleSimulatePayment` vs. Demo Checkout

### 4.1 Functional Comparison

| Dimension | Legacy Simulation (`handleSimulatePayment`) | Modern Demo Checkout (`handleDemoPayRoutes`) |
| :--- | :--- | :--- |
| **Endpoint(s)** | `POST /api/orders/{order_id}/simulate-payment` | `GET /pay/{token}` (HTML/JSON)<br>`POST /pay/{token}` (Payment Submit) |
| **Configuration Gate** | Strictly `cfg.devMode && !cfg.demoMode`<br>(403 Forbidden in Production and Demo Mode) | Strictly `cfg.demoMode`<br>(404 Not Found if demo mode is false) |
| **Authentication / Security** | No auth header required (gated by local server configuration). Constructs synthetic Razorpay webhook event signed with `webhookSecret`. | Unpredictable 24-byte crypto token (`dmo_<hex>`) generated during session payment initiation. |
| **Session Dependency** | Permissive: If `require_session != 1`, auto-creates synthetic `kiosk_session` on the fly to allow headless scripts to complete. | Strict: Verifies payment attempt is linked to an existing active, unexpired, non-canceled `kiosk_session`. |
| **Physical Presence Check** | Direct dispatch if WebSocket hub is connected; sets order to `DISPATCHED` immediately. | Enforces session validity; transitions attempt to `PAID` and order to `PAID_WAITING` or `DISPATCHED`. |
| **User Interface** | Headless JSON endpoint. (Referenced by legacy test page `frontend/payment.html`). | Full mobile-responsive HTML checkout screen displaying cost breakdown, print settings, and mock UPI submit button. |

### 4.2 Is `handleSimulatePayment` Still Required?

An analysis of codebase dependencies reveals that `handleSimulatePayment` is currently required by three operational workflows:

1. **Automated Monorepo Demo Script ([`scripts/run_local_demo.sh:256`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh#L256)):**
   ```python
   # 3. Simulate payment
   sim_url = f"{base_url}/api/orders/{order_id}/simulate-payment"
   sim_req = urllib.request.Request(sim_url, data=b"", method="POST")
   with urllib.request.urlopen(sim_req, timeout=10) as resp:
       ...
       sim_data = json.loads(resp.read().decode())
       if sim_data.get("status") not in ("DISPATCHED", "PAID_WAITING"):
           sys.exit(1)
   ```
   `run_local_demo.sh` starts the cloud server in dev mode (`XEROXGO_DEV_MODE=1`) without demo mode, uploads a sample document, and simulates payment headlessly via this endpoint. Deleting this endpoint immediately breaks `run_local_demo.sh`.

2. **Security Isolation Audit Script ([`scripts/check_demo_config.sh:136-155`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh#L136-L155)):**
   ```bash
   # Verify demo mode payment safety: /simulate-payment must return 403 Forbidden in demo mode
   SIMULATE_STATUS=$(python3 -c "
   req = urllib.request.Request('${CLOUD_HTTP_URL}/api/orders/non-existent-probe/simulate-payment', data=b'', method='POST')
   ...
   ")
   if [ "$SIMULATE_STATUS" = "403" ]; then
       pass "Demo payment isolation verified: unauthenticated simulate-payment endpoint is blocked (HTTP 403 Forbidden)"
   ```
   The operational configuration verification suite explicitly probes `/simulate-payment` to prove that dev-mode simulation is impossible in demo/production environments.

3. **Developer Simulation Page ([`frontend/payment.html:406-407`](file:///home/shabas/Projects/XeroxGo/frontend/payment.html#L406-L407)):**
   A static HTML utility used by developers during local UI testing to trigger payment capture without configuring Razorpay credentials.

### 4.3 Conclusion on `handleSimulatePayment`

`handleSimulatePayment` is **NOT** redundant dead code, nor is it a duplicate of `/pay/{token}`. It is a specialized, headless developer test fixture.
- **Recommendation:** **KEEP (DEV-ONLY TEST HARNESS)**.
- **Safety Precaution:** Retain the dual security gate:
  ```go
  if !s.cfg.devMode { http.Error(w, "payment simulation only available in development mode", http.StatusForbidden); return }
  if s.cfg.demoMode { http.Error(w, "direct order simulation is disabled in demo mode; use secure /pay/{token} checkout", http.StatusForbidden); return }
  ```
- **Future Migration:** In a subsequent task, `run_local_demo.sh` could be upgraded to use the `/pay/{token}` JSON API, and `check_demo_config.sh` updated. Until that migration is performed, removing `handleSimulatePayment` would introduce operational breakage.

---

## 5. Physical Presence, Release & Safety Mechanisms

### 5.1 Paid Order Release: `POST /api/k/{kiosk_id}/orders/{order_id}/release`

- **Implementation:** [`cloud-backend/cmd/server/session_handler.go:566-666`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L566-L666)
- **Role:** When an order is paid in advance or via mobile, its status becomes `PAID_WAITING`. The document is **NOT** dispatched to the physical printer.
- **Why This Is Critical:** If orders printed immediately upon payment, students paying from their dorm rooms would cause unattended print jobs to spill out of kiosk trays across campus, creating privacy violations, document theft risks, and tray overflows.
- **Enforcement:**
  1. The user must walk to the physical kiosk and enter their 4-digit PIN.
  2. The kiosk establishes an active session (`/claim`).
  3. The kiosk calls `/release` passing the active `session_id`.
  4. The server validates that the session belongs to this kiosk, the kiosk is connected to the WebSocket hub, and the printer is online with sufficient paper before dispatching the print command.
- **Classification:** **KEEP (CORE SAFETY MECHANISM)**.

---

### 5.2 Recovery PIN Generation: Customer vs. Kiosk Trust Boundaries

The platform exposes two recovery PIN endpoints:
1. `POST /api/orders/{order_id}/recovery-pin` (Customer Portal)
2. `POST /api/k/{kiosk_id}/orders/{order_id}/recovery-pin` (Edge Kiosk Daemon)

- **Implementation:** Both route to `handleRecoveryPIN` in [`session_handler.go:671-798`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/session_handler.go#L671-L798).
- **Authentication Analysis:**
  - **Customer Portal Endpoint (`/api/orders/{id}/recovery-pin`):**
    - Caller: Student's mobile web browser via [`frontend/js/portal.js:2102`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js#L2102).
    - Credential: `X-Recovery-Token` or `X-Claim-Token` issued to the customer during document upload.
    - Trust Boundary: The mobile user proves document ownership.
  - **Kiosk-Scoped Endpoint (`/api/k/{kiosk_id}/orders/{id}/recovery-pin`):**
    - Caller: Edge kiosk terminal or attendant interface.
    - Credential: Requires Kiosk Secret Bearer Token (`verifyKioskAuth`) AND the customer's claim token.
    - Trust Boundary: Enforces that the order belongs to this specific physical kiosk (`order.Command.KioskID == kioskID`).
- **Classification:** **KEEP BOTH**. They are not redundant duplicates; they enforce distinct trust boundaries and credential sets for two separate client types.

---

## 6. Classification Matrix & Evidence

| Endpoint | Status | Rationale & Evidence |
| :--- | :--- | :--- |
| `POST /api/k/{id}/orders/lookup` | **KEEP** | **Canonical PIN Lookup.** Used by `kiosk-daemon` ([`pin_client.go:156`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L156)), web portal, and tests. Rate-limited and enumeration-resistant. |
| `POST /api/k/{id}/pin/lookup` | **DEPRECATE $\rightarrow$ REMOVE** | **Legacy Alias.** Zero callers in frontend, kiosk-daemon, or scripts. Only tested in [`kiosk_routing_regression_test.go:69`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/kiosk_routing_regression_test.go#L69). |
| `POST /api/k/{id}/orders/{id}/claim` | **KEEP** | **Canonical Order Claim.** Used by `kiosk-daemon` ([`pin_client.go:245`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L245)). Enforces partial unique constraint & PostgreSQL advisory lock. |
| `POST /api/k/{id}/orders/{id}/confirm` | **DEPRECATE $\rightarrow$ REMOVE** | **Legacy Alias.** Zero callers in frontend, kiosk-daemon, or scripts. Tested only in route aliasing tests. |
| `POST /api/k/{id}/sessions/{id}/cancel` | **KEEP** | **Canonical Session Cancel.** Used by `kiosk-daemon` ([`pin_client.go:302`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L302)). Safely rolls back paper commitment. |
| `POST /api/k/{id}/sessions/{id}/cancel-session` | **DEPRECATE $\rightarrow$ REMOVE** | **Legacy Alias.** Zero production callers. |
| `POST /api/k/{id}/orders/{id}/cancel-session` | **REMOVE** | **Dead Code.** Unreachable due to router constraint `strings.Contains(path, "/sessions/")` in [`main.go:1490`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/main.go#L1490). |
| `POST /api/k/{id}/sessions/{id}/initiate-payment` | **KEEP** | **Canonical Payment Initiation.** Used by `kiosk-daemon` ([`pin_client.go:418`](file:///home/shabas/Projects/XeroxGo/kiosk-daemon/internal/cloud/pin_client.go#L418)). Enforces session-first transactional model. |
| `POST /api/k/{id}/orders/{id}/initiate-payment` | **MERGE / REMOVE** | **Redundant Route Multiplex.** Order-scoped path requires session lookup and is unused by the kiosk daemon. |
| `POST /api/k/{id}/orders/{id}/release` | **KEEP** | **Core Safety Gate.** Prevents ghost printing; enforces physical kiosk presence before dispatching paid jobs. |
| `POST /api/orders/{id}/recovery-pin` | **KEEP** | **Customer Portal Recovery.** Authenticated via customer `X-Recovery-Token`. |
| `POST /api/k/{id}/orders/{id}/recovery-pin` | **KEEP** | **Kiosk Attendant Recovery.** Authenticated via Kiosk Secret Bearer Token. |
| `POST /api/orders/{id}/simulate-payment` | **KEEP (DEV ONLY)** | **Development Test Fixture.** Required by [`run_local_demo.sh:256`](file:///home/shabas/Projects/XeroxGo/scripts/run_local_demo.sh#L256) and [`check_demo_config.sh:140`](file:///home/shabas/Projects/XeroxGo/scripts/check_demo_config.sh#L140). Blocked (403) in Demo and Production. |
| `GET /pay/{token}` & `POST /pay/{token}` | **KEEP** | **Demo Checkout Engine.** Tokenized, secure presentation checkout with mobile UI. |
| `GET /api/orders/{id}/status` | **KEEP** | **Canonical Status Polling.** Polled by customer portal and verification scripts. |
| `GET /api/orders/{id}/pdf` | **KEEP** | **Canonical Spooler Download.** Used by kiosk printer spooler to stream PDF. |
| `GET /api/k/{id}/sessions/{id}` | **KEEP** | **Canonical Session Query.** Polled by kiosk for session countdown. |
| `POST /api/k/{id}/refill` | **KEEP** | **Operator Maintenance.** Sends hardware tray calibration commands. |

---

## 7. Safety & Impact Analysis

An audit was conducted to verify whether removing the redundant legacy aliases (`/pin/lookup`, `/confirm`, `/cancel-session`, and order-scoped `/initiate-payment`) could negatively affect payment authorization, session ownership, or dispatch:

1. **Payment Authorization Integrity:**
   - Payment attempts are registered in `payment_attempts` with a foreign key pointing to `session_id`.
   - The canonical `/sessions/{session_id}/initiate-payment` endpoint provides direct session linkage. Retiring the order-scoped alias strengthens payment authorization by removing ambiguity about which active session owns the attempt.

2. **Session Ownership & Kiosk Binding:**
   - Both canonical and alias endpoints verify the Kiosk Secret Bearer token.
   - Retiring route aliases does not alter kiosk authorization or ownership validation.

3. **Dispatch & Printing Safety:**
   - Dispatch is triggered exclusively through WebSocket messages emitted during payment capture (`handleRazorpayWebhook`, `handleDemoPaymentSubmit`, `handleSimulatePayment`) or explicit physical release (`handleOrderRelease`).
   - None of the legacy aliases play any role in dispatch gating.

---

## 8. Proposed Cleanup Task & Regression Test Plan

### 8.1 Smallest Safe Cleanup Scope (Proposed Task 4.5)
1. **Remove Route Aliases from `main.go` Router:**
   - Remove `strings.HasSuffix(path, "/pin/lookup")` from line 1480.
   - Remove `strings.HasSuffix(path, "/confirm")` from line 1482.
   - Remove `strings.HasSuffix(path, "/cancel-session")` from line 1490.
   - In `handleInitiatePayment`, enforce that path contains `/sessions/` and parse `session_id` directly, removing the `/orders/` fallback.
2. **Remove Dead Code in `session_handler.go`:**
   - Clean up `handleOrderClaim` to only expect `/claim`.
   - Remove dead `/cancel-session` order parsing lines (277-285).
3. **Preserve Compatibility Safeguards:**
   - Keep `handleSimulatePayment` strictly gated to `devMode && !demoMode`.
   - Keep both customer and kiosk recovery-pin endpoints intact.

### 8.2 Required Regression Tests Before Alias Removal
1. `TestCanonicalRoutes_KioskDaemonContract`: Assert that `kiosk-daemon/internal/cloud/pin_client.go` functions (`LookupOrder`, `ClaimOrder`, `CancelSession`, `InitiatePayment`, `ReleaseOrder`) succeed against canonical routes.
2. `TestRetiredAliases_ReturnNotFound`: Assert that requests to `/pin/lookup`, `/confirm`, and `/cancel-session` return HTTP 404 Not Found.
3. `TestSimulatePayment_DemoModeIsolation`: Assert that `/simulate-payment` returns HTTP 403 Forbidden when `demoMode=true`.
4. `TestSimulatePayment_DevModeExecution`: Assert that `/simulate-payment` succeeds and triggers job dispatch when `devMode=true && demoMode=false`.
5. Run monorepo `./scripts/verify_gate.sh` across all 6 verification gates.

