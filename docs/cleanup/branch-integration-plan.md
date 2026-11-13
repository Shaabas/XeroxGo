# XeroxGo Branch Integration Assessment & Plan (Task INT-1)

> **Document Version:** 1.0.0  
> **Status:** Assessment Complete / Pending Approval  
> **Branches Assessed:**  
> - `main` (commit `60b11bf966d1dd0279e102c588d8ba3e570f4292`)  
> - `integration/agent-simplification` (commit `881249b377f6273d89255b231bf66af223ce413a`)  
> - `bibin` / `origin/bibin` (commit `3da94071657d24178591028cbbd791dfe8b57ea2`)  
> **Constraint Notice:** This document is an integration analysis. No merge, cherry-pick, rebase, reset, or production modification has been executed.

---

## 1. Branch Topology & Commit Lineage

A topological audit of the git commit graph reveals:

```
* 881249b (integration/agent-simplification) feat(checkpoint): architecture documentation, baseline tests, and core hardening (Tasks 1-4.4)
| 
| * 3da9407 (origin/bibin) user page edit button & preview update
|/  
* 60b11bf (main, origin/main, origin/HEAD, bibin) fix(kiosk): render error banner and loading state on order confirm page and auto-calibrate paper supply
```

### Key Graph Invariants:
1. **Common Ancestor:** Both `integration/agent-simplification` and `origin/bibin` fork directly from commit [`60b11bf966d1dd0279e102c588d8ba3e570f4292`](https://github.com/Shaabas/XeroxGo/commit/60b11bf966d1dd0279e102c588d8ba3e570f4292).
2. **Local `bibin` vs `origin/bibin`:** The local `bibin` tracking branch was at `60b11bf` (1 commit behind `origin/bibin`). `origin/bibin` contains the authoritative work (`3da9407`).
3. **Symmetry:** Neither branch has merged the other. Both are direct single-generation children of `60b11bf`.

---

## 2. Intended Behavior Comparison

### 2.1 Bibin's Changes (`origin/bibin: 3da9407`)
Bibin focused exclusively on customer-facing visual design, responsiveness, and touch interaction in the web portal:
1. **Visual Style & Brand Palette Refresh ([`frontend/css/portal.css`](file:///home/shabas/Projects/XeroxGo/frontend/css/portal.css)):**
   - Refined brand primary blue from `#2563eb` to a deeper `#0b63e5` (hover `#0952c2`).
   - Cleaned up container padding (`0 16px 32px`), header background, and upload card borders.
   - Refined card shadows, button shadows, and badge padding.
2. **Prominent Document Edit Button (`.doc-card:has(.btn-doc-edit)`):**
   - Restyled the existing edit button in document cart items into a dedicated full-height right-aligned action tile.
   - Improves thumb reachability on mobile touch screens for editing print options before checkout.
3. **Responsive High-Resolution PDF Preview Canvas ([`frontend/js/portal.js:1635-1655`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js#L1635-L1655)):**
   - Replaced fixed dimensions (`260x245px` desktop, `210x210px` mobile) with dynamic viewport sizing.
   - Calculates available width from parent container (`stageEl.clientWidth - padX`) up to 536px.
   - Sets dynamic height based on viewport height (`innerHeight * 0.45` to `0.48`, max 440px).
   - Allows zoom scaling up to 3.0x with crisp Device Pixel Ratio (`dpr`).
4. **Viewport Resize Listener ([`frontend/js/portal.js:1763-1774`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js#L1763-L1774)):**
   - Adds a debounced (150ms) window resize handler to re-render the preview sheet when orientation changes.

### 2.2 Agent's Changes (`integration/agent-simplification: 881249b`)
The agent focused on backend, kiosk hardware safety, architectural baseline testing, and conversion simplification:
1. **Async-Only Conversion Pipeline ([`frontend/js/portal.js:693-840`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js#L693-L840)):**
   - Converted `convertDocumentOnServer` to async-only (`/api/conversions`).
   - Removed legacy fallback to synchronous `/api/convert`.
   - Added transient network error retry during polling (up to 3 consecutive retries).
   - Explicit status reporting (`Uploading`, `Waiting in queue`, `Converting pages`, `Downloading PDF`).
2. **Headless Testing Support ([`frontend/js/portal.js:11-20, 2304`](file:///home/shabas/Projects/XeroxGo/frontend/js/portal.js#L11-L20)):**
   - Added Node.js / CommonJS export guard when `document` is undefined.
   - Exported `convertDocumentOnServer` in `window.XeroxPortal`.
3. **Automated Conversion Test Suite ([`frontend/js/conversion_test.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/conversion_test.js)):**
   - 8-suite Node.js test runner validating conversion lifecycle, network failures, retry, and cancellation.
4. **Verification Gate 6 & CI Enforcement ([`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh), [`.github/workflows/ci.yml`](file:///home/shabas/.github/workflows/ci.yml)):**
   - Enforces frontend test execution in local gate and GitHub Actions CI.
5. **Backend & Architecture Consolidation (Tasks 1.1–4.4):**
   - Centralized pricing module (`internal/pricing`), shared document preparation (`doc_prep.go`), PostgreSQL partial unique constraint (`idx_kiosk_sessions_active_order`), advisory transaction locks, live integration tests, and 14 architecture documents.

---

## 3. Conflict & Compatibility Analysis

### 3.1 Git Merge Conflict Analysis
A simulated 3-way merge using `git merge-tree` between `integration/agent-simplification` and `origin/bibin` across common ancestor `60b11bf` produces **zero conflict markers**:

| File | Agent Change | Bibin Change | Conflict Status |
| :--- | :--- | :--- | :--- |
| `frontend/css/portal.css` | Unchanged from `60b11bf` | Modified 270 lines | **Clean Automerge** (Bibin's changes apply cleanly) |
| `frontend/js/portal.js` | Modified lines 11–20, 693–840, 2304 | Modified lines 1635–1655, 1763–1774 | **Clean Automerge** (Edits separated by >790 lines) |
| All other files | 38 files modified/created | Unchanged | **Clean Automerge** (Agent changes apply cleanly) |

### 3.2 Runtime & Functional Compatibility
1. **Node.js Test Runner Compatibility:**
   - In `frontend/js/portal.js`, lines 11–20 guard container initialization:
     ```javascript
     const container = (typeof document !== 'undefined') ? (document.getElementById('app-container') || document.querySelector('.app-container')) : null;
     if (!container) {
       if (typeof module !== 'undefined' && module.exports) {
         module.exports = { convertDocumentOnServer };
       }
       return;
     }
     ```
   - When loaded by Node.js (`frontend/js/conversion_test.js`), `!container` is true, causing `portal.js` to return immediately at line 18.
   - Consequently, Bibin's added code in `renderPreviewSheet` and `window.addEventListener('resize')` is never reached in Node.js test runs. Headless testing remains 100% stable without DOM errors.

2. **Browser Execution Compatibility:**
   - In the browser DOM, `convertDocumentOnServer` operates during file upload.
   - `renderPreviewSheet` and `window.resize` operate during the preview settings stage.
   - The two features do not share mutable variables, state objects, or conflicting DOM selectors.

### 3.3 Visual & CSS Regression Risks
1. **Brand Palette Shift:**
   - The change of `--primary-blue` from `#2563eb` to `#0b63e5` is intentional. Because variables are defined in `:root`, all buttons, links, and accents shift consistently.
2. **`:has()` CSS Selector Support:**
   - `.doc-card:has(.btn-doc-edit)` is supported in modern mobile browsers (Chrome 105+, Safari 15.4+, Firefox 121+). In older browsers, document cards degrade gracefully to standard layout.
3. **Resize Debounce Overhead:**
   - The resize listener uses a 150ms debounce and explicitly checks `viewSettings && !viewSettings.classList.contains('hidden')`. It does not trigger wasteful re-renders when other views (upload, cart, success) are active.

---

## 4. Existing Test Coverage

| Test Suite | File | Coverage Area | Status |
| :--- | :--- | :--- | :--- |
| **Frontend Conversion Suite** | [`frontend/js/conversion_test.js`](file:///home/shabas/Projects/XeroxGo/frontend/js/conversion_test.js) | Async conversion submission, polling loop, network failure recovery, cancellation, 404/413 errors | 8/8 tests pass in Gate 6 |
| **Document Preparation Suite** | [`cloud-backend/cmd/server/doc_prep_test.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/doc_prep_test.go) | Form print options parsing, N-up validation, page range bounds, duplex padding | All pass |
| **Document Pricing Baseline** | [`cloud-backend/cmd/server/document_pricing_baseline_test.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go) | Upload vs Preview price equivalence, 4-tier tariffs, order price immutability | 10/10 tests pass |
| **Document Engine Baseline** | [`cloud-backend/internal/pdf/document_engine_baseline_test.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go) | ISO A4 geometry, Ghostscript normalization, N-up collation, encryption rejection | All pass |
| **Monorepo Verification Gate** | [`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh) | Gates 1–6 (Frozen contracts, CGO ban, Go vet, builds, race tests, frontend tests) | All pass |

---

## 5. Recommended Additional Regression Tests

To provide comprehensive verification following branch integration:
1. **Node.js Environment Purity Test:**
   - Ensure `portal.js` can be `require()`'d in Node without throwing `ReferenceError: window is not defined` or `ReferenceError: document is not defined`, even with Bibin's added event listeners.
2. **Preview Scaling Arithmetic Test:**
   - Verify `Math.min(maxStageW / rawViewport.width, maxStageH / rawViewport.height, 3.0)` produces positive, finite scale values across mobile (320px width), tablet (768px width), and desktop (1440px width) viewports.
3. **Full Gate 1–6 Regression:**
   - Run `./scripts/verify_gate.sh` on the merged tree.

---

## 6. Proposed Step-by-Step Merge Strategy (INT-2)

When approved, Task INT-2 should execute the following sequence:

1. **Step 1: Create Integration Branch or Merge into Checkpoint:**
   - Merge `origin/bibin` into `integration/agent-simplification`.
   - Command: `git merge origin/bibin -m "merge(bibin): integrate mobile edit button and responsive preview updates"`
2. **Step 2: Inspect Automerge Working Tree:**
   - Confirm git reports clean merge with 0 conflict markers in `frontend/js/portal.js` and `frontend/css/portal.css`.
3. **Step 3: Run Verification Gates:**
   - Execute `./scripts/verify_gate.sh` to ensure all 6 gates pass cleanly with `-race`.
4. **Step 4: Execute Frontend Node.js Test Suite:**
   - Execute `node --test frontend/js/conversion_test.js`.
5. **Step 5: Document Post-Merge State:**
   - Update `docs/README.md` and document verification results in `docs/cleanup/branch-integration-plan.md`.
6. **Step 6: Push Updated Integration Branch:**
   - Push updated `integration/agent-simplification` to origin for review.

---

## 7. Status & Next Action

- **Current Status:** Assessment complete. No branches merged or modified.
- **Recommended Next Task:** **Task INT-2: Execute Branch Merge & Verification Gate**.
- **Awaiting User Approval:** Ready to proceed with merging `origin/bibin` into `integration/agent-simplification` upon your confirmation.

