# XeroxGo minimum core

XeroxGo is a cloud-connected printing kiosk with one customer checkout:
**XeroxGo demo payments**. There is no payment gateway, UPI charge,
webhook callback or external refund mechanism in the current application.

## Essential components
- Cloud API: mobile upload, canonical A4 PDF normalization, document
  settings, per-document signed PDFs, authoritative pricing and paper
  reservations in `cloud-backend/cmd/server`.
- Cloud store: PostgreSQL/SQLite order, payment-attempt, child-job,
  reservation, dispatch-intent and simulated refund records.
- Kiosk daemon: Raspberry Pi PIN handling, cloud WebSocket connection,
  signed PDF retrieval, SHA256 verification, local SQLite journal and
  CUPS job submission.
- Cloud demo checkout: `/pay/{token}` generated during
  `/api/k/{kiosk_id}/sessions/{session_id}/initiate-payment`.
- Frontend: upload, order settings, checkout status and attendant
  health/admin controls.

## Safety requirements
1. Reserve the entire order's physical sheets when issuing its unpaid PIN.
2. Keep a five-minute unpaid PIN timeout; paid claims survive this deadline.
3. Capture simulated payments idempotently so a duplicate confirmation
   cannot trigger a second print job.
4. Deliver immutable child-document commands through durable intents.
5. Use Pi-side SQLite/CUPS evidence to prevent printing a job twice after
   network loss, power interruption or uncertain spooler state.
6. Never mark simulated refunds processed before appropriate safe-stop
   verification for print faults.
7. Retain operator intervention for mechanical uncertainty or out-of-service
   conditions.

## Deliberately absent
No Razorpay integration, provider switching, UPI QR generation, public
webhook endpoint, legacy refund migration, old order-level PDF artifact
or alternate public payment simulation endpoint. Start a new demo
database rather than upgrading historical orders.

See `docs/workflows/minimum-print-path.md` and
`docs/architecture/paid-fulfillment-refunds.md` for the current flow.
