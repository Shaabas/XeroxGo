# XG-CLOUD-DOCS-V2 — Cloud-first documents (review draft)

Status: **implementation in progress, feature gated, NOT deployed**. Architecture approved in principle by product owner: **separation of concerns** and **fast, predictable UX**. Do not merge without end-to-end tests, security review and explicit approval.

## Boundaries
- Vercel/browser: upload controls, preview, honest network progress, local **display-only** estimates. No canonical normalization or billed amount decisions.
- Supabase private Storage: immutable original and print-ready artifact bytes. No publicly readable buckets or client service key.
- Render: signed-upload grant issuance, object verification, conversion, the ONLY canonical normalization/imposition and billing logic, artifact lifecycle, orders/PIN atomic transaction, authorization of printer downloads.
- Supabase Postgres: upload identities, variant states, immutable order references, existing orders and payments.
- Raspberry Pi: existing authenticated claim/payment/dispatch protocol, verify checksum and CUPS print; **do not change the printer's execution model**.

## Desired dataflow
1. Client asks Render for a narrowly scoped upload authorization. Render creates a pending upload record, reserves a unique object key, and issues a short-lived signed URL.
2. Browser uploads file bytes **directly to Supabase Storage once**. Client progress measures real bytes. Completion alone is not trustworthy.
3. Render checks the object server-side (size, content, digest and format) and transitions the upload to UPLOADED. No processing from an unverified object.
4. Render asynchronously converts Office files if needed and generates a normalized immutable variant when settings are confirmed; deduplicate by (upload_id, **canonical** settings hash, normalizer version).
5. Price is a quick display estimate until a READY variant receives canonical billing and the active tariff revision. A new tariff need not regenerate identical PDF bytes; it must recalculate the accepted bill.
6. Place Order submits variant IDs, idempotency key, exact accepted total and revision (no files). Render checks session ownership, all READY states, unexpired objects, integrity and tariff revision **inside its transaction**, atomically creates order and reserves PIN.
7. A prepared, immutable final order PDF remains in private Storage. Kiosk downloads via existing secure Render authorization/signed proxy, validates digest and uses existing spooler/recovery paths.
8. Clean up unused originals and variants without deleting any artifact held by an active/recoverable order. Apply retention only after terminal states with recovery margin.

## Guarantees
- Never return a PIN before the order and all document references are durable.
- Never trust browser-supplied object paths, prices, page counts, type or upload-complete assertions.
- A signed-upload URL is a capability; don't return it to unauthorized users or log it.
- Only cloud PDF normalization determines printable output and billing. Preserve existing tariff snapshot and mixed-document handling; the latter still needs the independent XG-004 fidelity audit.
- Immutable variants cannot be overwritten (signed upload uses new random keys; no upsert).
- Network loss on order submission is ambiguous. Persist idempotency key and implement lookup/recovery before allowing retry.
- Render local disk is **scratch space only**, not durable order artifact storage.
- Rate-limits and capacity quotas must be enforced before enabling anonymous direct uploads (avoid open storage abuse).
- Preserve the old multipart checkout until all end-to-end tests pass and the cutover is explicitly approved.

## Implementation stages / acceptance gates
1. Storage + schema + upload signing/verification foundation, off by default. Confirm private buckets, RLS/revoked grants, token lifetime, quotas, browser upload CORS, and rollback.
2. Browser direct-to-Storage upload with recoverable states and real progress; cross-device test; validate file acceptance/expiry without cloud processing.
3. Render async preparation and immutable variant retention, including Office and images; baseline checksums, 1/2/4/6-up, page ranges, portrait/landscape/auto and simplex/duplex.
4. Reference-only atomic checkout, idempotency, stale tariff 409 handling and PIN recovery; **no PDF transfer or normalization after Place Order**.
5. Cloud-backed secure kiosk download, checksum and crash recovery; test temporary storage outage and stale references.
6. Enable for canary kiosk, measure latency/operational cost, expand and remove obsolete paths only after confirming parity.

## Rollback
Keep the original multipart pipeline live until Stage 5 validation. Do not migrate active, payable orders mid-flight. Feature flag defaults OFF and database changes are additive. Never automatically delete customer files or alter payment flows on rollback.

## Verification currently incomplete
- Supabase Storage is empty as of 2026-11-18; migration is a *file*, not an applied change.
- Legacy Supabase public tables have unreviewed RLS/grants, separate task XG-006.
- Actual Render deploy, storage API credentials, CORS and browser-to-storage performance not established.
