# XG-004 / XG-CLOUD-DOCS-V2 — Mixed-document print fidelity review

Status: **analysis and release gate**, no automatic printer behavior change approved.

## Confirmed existing behavior
In `cloud-backend/cmd/server/main.go` multi-file `handleUpload`, each document has separate `docPrintOptions` and is normalized/priced independently. After that, all prepared PDFs are concatenated with `qpdf --empty --pages ...` into ONE PDF. The backend chooses ONE overall `PrintJobCommand.Duplex`: `DUPLEX_LONG_EDGE` if **any** document was duplex, otherwise `SIMPLEX`. It also sets one overall color mode: `COLOR` if any document was color. The Pi/CUPS submission therefore receives global settings rather than a command for each source document.

## Why this matters
Consider document A (two pages, simplex) and document B (two pages, duplex):
- Cloud prices A as **two simplex sheets** and B as **one duplex sheet**.
- The combined PDF has four pages and the Pi may run all pages as a single duplex job.
- The physical output can differ from the separately priced sheets, including document boundaries and different device settings.
- Applying a global color mode can similarly affect hardware output, even if individual prices were calculated separately.
- Blindly concatenating can also join odd duplex pages with the following document unless correct explicit padding and job boundaries are proven.

## V2 architectural requirement
Each immutable prepared variant must carry exact print settings and billable sheet count. Creating an order with multiple variants must **not** silently collapse dissimilar device settings into one CUPS command. Options requiring explicit review:
1. **Per-document durable print tasks** (preferred candidate): one order/PIN/payment, ordered sequence of per-variant CUPS tasks preserving per-doc duplex/color settings. Requires durable task progression, crash recovery and prevention of duplicate physical output, including stop between documents.
2. **Canonical merged PDF with explicit boundaries**: only if a tested, supported printer representation can accurately preserve all per-doc settings in one queue job. A PDF alone does not encode arbitrary CUPS driver duplex/color transitions.
3. **Restricted homogeneous batch**: initially permit multi-file orders only if settings are physically compatible and test boundary padding. Reject unsupported mixed settings clearly and before PIN creation.

## Mandatory tests before enabling V2 multi-file checkout
- 2+ documents with same settings; varied copies, page range, orientation, N-Up.
- Odd-page duplex boundary, duplex-long vs duplex-short, simplex then duplex and vice versa.
- B&W and color settings; printer color support/disabled policy.
- Price reflects exactly the physical sheets and sides, including no artificial duplex padding billed.
- A failure of document 2 after document 1 prints must recover without reprinting document 1.
- CUPS held, processing, unknown, completed, crash during submission, lost acknowledgements and physical odometer discrepancies.
- One order/PIN/payment across all constituent jobs, with durable recovery records.
- Verify on physical demo/production printers after emulator tests.

## Current decision
V2's reference checkout currently accepts only ONE variant. That is an intentional correctness guard, not a feature-complete replacement for today's multi-file cart. Do not enable production cutover or remove the old cart until a multi-document strategy is approved and its end-to-end safety is verified.


## V2 implementation checkpoint — 2026-11-18
The first safe multi-document implementation slice has been committed **without enabling checkout or altering the Raspberry Pi**:
- `BuildCloudPrintPlan` (Render/store package) validates an ordered list of READY, immutable prepared variants (1–20), preserving each variant's own duplex, B&W/color, checksum, baked copies, tariff revision, price, and expected physical sheets. It rejects duplicate variants, forged object keys, bad hashes, mixed tariff revisions and integer overflow. **It never concatenates PDFs**.
- The draft Supabase migration now defines private `cloud_print_tasks`, one task per ordered variant, with document-specific printer options, sequence number, output checkpoint state, optional CUPS job ID and dispatch attempt count.
- `ValidateCloudPrintTaskTransition` defines the next worker's required fail-closed progression: AWAITING_PAYMENT → READY → DISPATCHING → SUBMITTED → COMPLETED. Transitioning out of dispatch uncertainty requires a distinct RECONCILE state; it never automatically re-dispatches an ambiguous job. Releasing task N+1 requires durable evidence that task N completed.
- Go tests cover mixed simplex/duplex-long/duplex-short, mixed color/B&W, immutable checksums, aggregate prices, unsafe transitions, and terminal-state reprint prevention.
- The state model is a **policy/validation foundation only**. It is not currently connected to a transactional multi-file order checkout, payment callback, WebSocket delivery, spooler/CUPS, or recovery ledger. In particular, no unattended print task is being created or dispatched by this new code.

### Next implementation gate (requires printer lifecycle validation)
Implement atomic checkout that persists all order_documents and all cloud_print_tasks **inside the same transaction** as a single order/PIN, then a versioned Pi command for per-document task dispatch. Tie each CUPS request ID, accepted acknowledgement and physical progress to a durable task ID. On ambiguous acknowledgement, stop the remaining tasks and require operator reconciliation. Do not signal final COMPLETED until every task is physically confirmed. Never blindly retry a physically ambiguous task. Test mixed duplex, B&W/color, jam during document 2, power loss, Render restart and payment failure on both emulator and real printer before enablement.
