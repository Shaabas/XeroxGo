# Pricing: single source of truth (feature-branch rollout)

## Ownership

- `cloud-backend/internal/pricing/pricing.go` is the only financial calculator.
- `cloud-backend/cmd/server/doc_prep.go` supplies **nonblank imposed sides**, copies, duplex and color after canonical PDF normalization.
- `frontend/js/portal.js` requests `POST /api/documents/preview` and displays the returned exact `total_price_paise`. It no longer computes a monetary price.
- Both single-file and multi-file uploads use the canonical Go quote engine. The portal submits `accepted_total_price_paise`; the backend rejects mismatches with HTTP 409 **before creating an order**.
- Payments and kiosk PIN views use the persisted order amount, not a recalculation.

## Pricing policies and compatibility

The new pricing package supports two interpretations:

1. `per-side` (default): exactly preserves previous deployment behavior. A duplex rate is per **printed side**; the billed side count excludes padding.
2. `per-sheet` (opt-in): uses the model from the sheet-based tariff model: a complete two-sided sheet costs the duplex tariff, a trailing one-sided sheet costs the simplex tariff, and blank padding is not billed.

The target rates are B&W simplex **300 paise per side**, B&W duplex **500 paise per full sheet**, color simplex **800 paise per side**, and color duplex **1000 paise per full sheet**.

**IMPORTANT: This branch does not migrate or rewrite production tariffs.** Existing database duplex rates were historically defined **per side**; they must not be interpreted as **per sheet** without explicit operational review. Existing orders and their stored monetary amounts are unchanged.

To enable after approving a tariff review:

1. Back up current `pricing` rows and record currently stored values.
2. Explicitly set the four configured tariffs to agreed values (The reference example is `300,500,800,1000`) using the existing authorized administrator API or approved maintenance process, and verify the color-enabled hardware policy.
3. Set `XEROXGO_PRICING_POLICY=per-sheet` **and** `XEROXGO_SHEET_TARIFF_REVIEWED=1` in the cloud deployment. Never enable only one.
4. Confirm live preview/upload/order/payment parity with a fresh test order before directing real customer traffic.
5. Roll back to `XEROXGO_PRICING_POLICY=per-side` only after checking which tariffs are currently stored; merely toggling the mode with sheet rates can also misprice new orders.

## Performance

Selections and page navigation remain local and immediate. The portal requests an authoritative PDF quote once after file preparation, and once when settings are saved; it does **not** re-normalize on every tap. Checkout is disabled until every ready document has a matching confirmed quote. Quotes can take longer than local arithmetic because PDF processing and network transfer are required.

This is an initial correctness-oriented integration; caching normalized documents and avoiding repeated upload for unchanged files is a separate future performance optimization, not implemented here.

## Required verification

Run `./scripts/verify_gate.sh`, `go test ./cloud-backend/...`, frontend conversion tests, and integration tests for 1/2/3/5 imposed sides, all four tariffs, copies, mixed N-up, page ranges, blank duplex padding, multi-file totals, stale quote HTTP 409, and immutable order pricing. No production merge until CI and an explicit tariff-rollout review are complete.
