# XeroxGo Document Engine & Pricing Baseline Test Report

This document records the baseline test suite, invariant verification results, discovered discrepancies, and uncovered edge cases for the **Document Ingestion & Imposition Engine** and the **Pricing & Currency Subsystem** of XeroxGo.

The tests establish an authoritative behavioral baseline prior to any subsequent refactoring or code cleanup.

---

## 1. Executive Summary

A comprehensive, deterministic test suite has been implemented across two packages without modifying production code:
1. **Low-Level Document Engine (`xeroxgo/cloud-backend/internal/pdf`):** [`document_engine_baseline_test.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go) (5 test suites).
2. **Cloud Server & Pricing APIs (`xeroxgo/cloud-backend/cmd/server`):** [`document_pricing_baseline_test.go`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go) (5 test suites).

All 10 test suites pass cleanly under `go test -race` and `./scripts/verify_gate.sh`.

---

## 2. Test Execution Matrix

| Test Function | Location | Primary Invariants Verified | Inputs & Configurations | Observed Result | Status |
| :--- | :--- | :--- | :--- | :--- | :---: |
| `TestDocumentEngine_NUp4And6DuplexOddPadding` | [`internal/pdf/document_engine_baseline_test.go:53`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go#L53) | N-up 4 & 6 imposition, duplex blank padding, physical sheet accounting | 9-page doc in 4-up Duplex; 13-page doc in 6-up Duplex | Odd imposed sides (3) receive 1 blank padding page; resulting PDF has 4 pages; physical sheets = 2; billed sides = 3. | **PASS** |
| `TestDocumentEngine_A4NormalizationGeometry` | [`internal/pdf/document_engine_baseline_test.go:138`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go#L138) | Strict ISO A4 geometry standardization | US Letter input (612x792 pt) in Portrait and Landscape | Output forced to ISO A4: $595.28 \times 841.89$ pt (portrait) and $841.89 \times 595.28$ pt (landscape) $\pm 2$ pt. | **PASS** |
| `TestDocumentEngine_PageRangeSubsetsAndReordering` | [`internal/pdf/document_engine_baseline_test.go:189`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go#L189) | Custom page selection, reverse ordering, non-contiguous N-up | 6-page doc: range "5,3,1" 1-up; range "2,4,6" 2-up | Output page 1 contains Page 5, page 2 contains Page 3; 2-up sheet 1 contains Pages 2 & 4. | **PASS** |
| `TestDocumentEngine_CopiesScalingAndOverflow` | [`internal/pdf/document_engine_baseline_test.go:244`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go#L244) | Copies multiplication, duplex sheets per copy, `math.MaxInt` overflow guard | 3-page doc: 4 copies Simplex (12 sheets); 3 copies Duplex (6 sheets); copies = `math.MaxInt` | Sheets scale linearly (`sheetsPerCopy * copies`); `math.MaxInt` returns explicit arithmetic overflow error. | **PASS** |
| `TestDocumentEngine_CorruptEmptyAndEncryptedHandling` | [`internal/pdf/document_engine_baseline_test.go:297`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/document_engine_baseline_test.go#L297) | Empty file, corrupt payload, user/owner encryption, path collision | 0-byte file, garbage text, password-encrypted PDF, same input/output paths | Cleanly rejected with HTTP 400 / error; user encryption fails at `qpdf --check`, owner encryption at `qpdf --is-encrypted`. | **PASS** |
| `TestDocumentPricing_PreviewAndUploadParity` | [`cmd/server/document_pricing_baseline_test.go:131`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go#L131) | Canonical print semantics equivalence between `/api/documents/preview` and `/api/k/{id}/upload` | 5-page PDF across 3 distinct parameter sets (N-up 1, 2, 4; Simplex, Duplex; BW, Color) | Exact equivalence in total billed sides, physical sheets, total price paise, and page-by-page extracted text content. | **PASS** |
| `TestDocumentPricing_MultiFileCollationOrderAndPadding` | [`cmd/server/document_pricing_baseline_test.go:273`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go#L273) | Multi-file document ordering, per-document duplex padding before merge | 3 documents: DocAlpha (2p Simplex), DocBeta (3p Duplex), DocGamma (1p Simplex) | Merged PDF strictly preserves Alpha $\rightarrow$ Beta $\rightarrow$ Gamma order; Beta receives blank padding on sheet 6 before Gamma. | **PASS** |
| `TestDocumentPricing_FourTierIntegerPaiseAndColorFallback` | [`cmd/server/document_pricing_baseline_test.go:343`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go#L343) | 4-tier integer paise pricing, color disabled hardware fallback | BW Simplex 250, BW Duplex 175, Color Simplex 1250, Color Duplex 900; ColorEnabled toggling | Exact integer calculations (e.g. 3 sides Duplex @ 175 = 525 paise); when `ColorEnabled=false`, color requests fall back to BW rates. | **PASS** |
| `TestDocumentPricing_PriceImmutabilityPostCreation` | [`cmd/server/document_pricing_baseline_test.go:431`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go#L431) | Order price immutability after creation (`INV-PRI-03`) | 3-page Simplex order created @ 200 paise = 600 paise; admin updates pricing to 800 paise | Status polling (`/status`), PIN lookup (`/lookup`), and kiosk session claim (`/claim`) all return original 600 paise. | **PASS** |
| `TestDocumentPricing_InputValidationAndBoundaryErrors` | [`cmd/server/document_pricing_baseline_test.go:502`](file:///home/shabas/Projects/XeroxGo/cloud-backend/cmd/server/document_pricing_baseline_test.go#L502) | Input validation and boundary enforcement | Missing file, `.exe`, corrupt binary, invalid duplex ("TRIPLEX"), copies 0/1000, N-up 3, orientation "DIAGONAL" | All rejected with HTTP 400 Bad Request and descriptive validation error messages. | **PASS** |

---

## 3. Document Engine Invariants Verified

### 3.1 Strict ISO A4 Normalization Geometry
- **Requirement:** All submitted documents, regardless of source paper geometry (e.g., US Letter $612 \times 792$ pt, US Legal $612 \times 1008$ pt, or custom dimensions), must be normalized to standard ISO A4.
- **Observed Behavior:**
  - Portrait jobs are rendered with bounding box $595.28 \times 841.89$ pt (tolerance $\pm 2.0$ pt).
  - Landscape jobs are rendered with bounding box $841.89 \times 595.28$ pt (tolerance $\pm 2.0$ pt).
  - Ghostscript parameters `-sPAPERSIZE=a4 -dFIXEDMEDIA -dPDFFitPage` scale and fit content to ISO A4 margins deterministically.

### 3.2 N-Up Imposition (Modes 1, 2, 4, 6)
- **Supported Values:** Strictly $1, 2, 4, 6$ per [`normalizer.go:98, 318`](file:///home/shabas/Projects/XeroxGo/cloud-backend/internal/pdf/normalizer.go#L98). All other values (e.g., $3, 5, 8$) return explicit errors.
- **Page Imposition Formula:**
  $$\text{ImposedSides} = \left\lceil \frac{\text{ContentPages}}{\text{PagesPerSheet}} \right\rceil$$
  - **4-Up Duplex:** 9 content pages $\rightarrow \lceil 9/4 \rceil = 3$ imposed content sides.
  - **6-Up Duplex:** 13 content pages $\rightarrow \lceil 13/6 \rceil = 3$ imposed content sides.

### 3.3 Duplex Blank Back Padding
- **Rule:** For any job where `Duplex != DuplexSimplex` and $\text{ImposedSides} \pmod 2 \neq 0$:
  - A blank back-page (`showpage`) is appended by Ghostscript at the end of the document.
  - **Accounting Invariant:** The blank padding page is included in the output PDF physical page count (`PaddedPages = ImposedSides + 1`), but is **strictly excluded** from billable sides (`TotalPages = ImposedSides`).
  - **Physical Sheet Formula:**
    $$\text{SheetsPerCopy} = \left\lfloor \frac{\text{ImposedSides}}{2} \right\rfloor + (\text{ImposedSides} \pmod 2)$$
    $$\text{TotalSheets} = \text{SheetsPerCopy} \times \text{Copies}$$

### 3.4 Multi-File Collation Order & Per-Document Padding
- When multiple documents are uploaded in a single cart:
  1. Each document is normalized according to its individual metadata settings (copies, duplex, N-up, color).
  2. If a document is duplex and has odd sides, it receives its own blank padding page **before** concatenation.
  3. All normalized intermediate PDFs are merged via `qpdf --empty --pages <doc1> 1-z <doc2> 1-z ... -- <output>`.
  4. Extracted page text verifies that page ordering strictly follows the array index order of the upload payload.

---

## 4. Pricing & Currency Invariants Verified

### 4.1 Strict Integer Currency (Paise)
- All price calculations occur in 64-bit integer paise ($1\text{ INR} = 100\text{ paise}$).
- No floating-point multiplication or division is used in the pricing path.
- Unit prices with fractional rupee amounts (e.g. ₹1.75 = 175 paise, ₹2.50 = 250 paise) calculate exact whole-paise totals without floating-point rounding errors ($3 \times 175 = 525$ paise).

### 4.2 Four-Tier Price Resolution & Hardware Color Fallback
- The pricing engine resolves unit prices across four distinct tiers:
  - B&W Simplex: `BWSimplexPricePaise`
  - B&W Duplex: `BWDuplexPricePaise`
  - Color Simplex: `ColorSimplexPricePaise`
  - Color Duplex: `ColorDuplexPricePaise`
- **Hardware Enforcement Invariant:** If `pricing.ColorEnabled == false`:
  - Any request with `color_mode = "COLOR"` is silently coerced to `"BW"`.
  - The unit price resolves to the B&W simplex/duplex tariff.
  - Ghostscript color strategy is forced to `-sColorConversionStrategy=Gray -dProcessColorModel=/DeviceGray`.

### 4.3 Price Immutability Post Creation (`INV-PRI-03`)
- Once an order is created, its total price is persisted in the `orders` table.
- If an administrator subsequent modifies active tariffs via `/api/admin/pricing` or `SetDetailedPricing`:
  - `GET /api/orders/{id}/status` returns the original agreed `price_paise`.
  - `POST /api/k/{id}/orders/lookup` returns the original agreed `total_price_paise`.
  - `POST /api/k/{id}/orders/{id}/claim` establishes a transaction session with the original `PricePaise`.

---

## 5. Preview vs. Upload Semantic Equivalence

Both `POST /api/documents/preview` and `POST /api/k/{id}/upload` execute the identical canonical normalization pipeline via `pdf.NormalizeAndCountWithOptions`.

### 5.1 Equivalence Verification
For identical inputs (5-page PDF with 2 copies, range 1-3, Color):
| Semantic Attribute | Preview API Response | Upload API Response | Equivalence Status |
| :--- | :--- | :--- | :---: |
| **Total Billed Sides** | `X-Billed-Sides: 6` | `total_pages * copies = 3 * 2 = 6` | **EQUIVALENT** |
| **Pages Per Single Copy** | `X-Normalized-Pages: 3` | `total_pages: 3` | **EQUIVALENT** |
| **Total Physical Sheets** | `X-Total-Sheets: 6` | `total_sheets: 6` | **EQUIVALENT** |
| **Total Price (Paise)** | `X-Total-Price-Paise: 6000` | `total_price_paise: 6000` | **EQUIVALENT** |
| **Output PDF Pages** | `X-Padded-Pages: 3` | `getPDFPageCount(job.pdf) = 3` | **EQUIVALENT** |
| **Rendered Page Text** | Extracted page-by-page text | Extracted page-by-page text | **IDENTICAL** |

### 5.2 Ghostscript Output Non-Determinism (Observed Behavior)
- **Observation:** Calling preview and upload with the exact same input PDF produces distinct file SHA256 checksums (`preview != upload`).
- **Root Cause Analysis:** Ghostscript (`gs -sDEVICE=pdfwrite`) embeds dynamic metadata into the output PDF by default:
  1. `/CreationDate (D:YYYYMMDDHHMMSS...)`
  2. `/ModDate (D:YYYYMMDDHHMMSS...)`
  3. `/ID [<uuid> <uuid>]`
- **Impact:** Each invocation generates a unique byte stream even though the visual content, geometry, page count, and print semantics are 100% equivalent.
- **Verification Guarantee:** Both preview and upload calculate authentic SHA256 checksums of their respective generated output files (`len(checksum) == 64`), which match the byte hash of the saved disk artifact.

---

## 6. Discrepancies & API Field Inconsistencies Discovered

During baseline test construction, three API field naming inconsistencies were uncovered:

### Discrepancy 1: `total_pages` in Upload vs Preview
* **Preview (`handleDocumentPreview`):** Returns `X-Billed-Sides` as the **total billable sides across all copies** (`normRes.TotalPages * copies`), and `X-Normalized-Pages` as the sides for a single copy (`normRes.TotalPages`).
* **Upload Single-File (`handleUpload`):** Returns `"total_pages"` as `normRes.TotalPages` (**sides per single copy**), requiring the caller to multiply by `"copies"` to determine total billed sides.
* **Upload Multi-File (`handleUpload`):** Returns `"total_pages"` as `totalPrintedPages` (**total billable sides across all copies**), while `"copies"` is set to 1.
* **Recommendation:** In future refactoring, standardize on explicit field names: `billed_sides` (overall total) and `pages_per_copy`.

### Discrepancy 2: `price_paise` vs `total_price_paise` Field Naming
* **`POST /api/k/{id}/upload`:** Returns `"total_price_paise"`.
* **`POST /api/k/{id}/orders/lookup`:** Returns `"total_price_paise"`.
* **`POST /api/k/{id}/orders/{id}/claim`:** Returns `"total_price_paise"`.
* **`GET /api/orders/{id}/status`:** Returns `"price_paise"`.
* **Recommendation:** Retain `"total_price_paise"` as canonical; alias `"price_paise"` in status responses for backward compatibility.

### Discrepancy 3: User-Password Encrypted PDFs vs `qpdf --is-encrypted`
* **Intended Logic:** `normalizer.go:381` runs `qpdf --is-encrypted` to detect and reject password-protected PDFs.
* **Observed Reality:** For a PDF with a user password, `qpdf --check` at line 364 fails first with exit code 2 (`qpdf: ...: invalid password: exit status 2`). The execution returns immediately without reaching `qpdf --is-encrypted`.
* **Security Evaluation:** Non-fatal. The encrypted PDF is safely rejected before ingestion; only the internal error message originates from `qpdf --check` rather than `qpdf --is-encrypted`. For owner-password protected PDFs with empty user password, `qpdf --check` passes and `qpdf --is-encrypted` catches it.

---

## 7. Verification Gate Compliance

The monorepo verification gate script [`scripts/verify_gate.sh`](file:///home/shabas/Projects/XeroxGo/scripts/verify_gate.sh) was run following the addition of the baseline test suites:
- **Gate 1 (Frozen Contracts):** PASSED (unmodified).
- **Gate 2 (Ban CGO SQLite):** PASSED (91 Go source files checked, 0 occurrences).
- **Gate 3 (Go Vet):** PASSED (no issues reported).
- **Gate 4 (Required Builds):** PASSED (`linux/arm64` kiosk daemon and host cloud backend build cleanly).
- **Gate 5 (Race-Enabled Tests):** PASSED (all tests pass with `-race`).
- **Production Code Status:** 100% preserved. Zero modifications, moves, or deletions made to production code.

