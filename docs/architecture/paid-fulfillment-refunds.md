# Paid fulfillment, printer faults and refund lifecycle

## Invariant

The five-minute deadline controls **unpaid reservation** only. A verified captured
payment moves the customer into the paid fulfillment lifecycle, preserving its
paper commitment. The customer may leave after a fault; no customer action
is necessary for the refund worker to run.

## Order / PIN

- Unpaid order: five-minute PIN expiry; physical sheets reserved at mobile upload.
- Captured payment: the existing still-valid PIN is extended and its countdown
  disappears. This is not a paid payment-session timeout.
- An expired or recycled PIN is **not** revived by late capture. The mobile
  checkout can use its previously issued secret recovery credential to rotate a
  new paid PIN for a `PAID_WAITING` order, without further payment.
- Completed / provider-confirmed refunded order: PIN becomes invalid and cannot
  authorize a duplicate print.
- Paid orders do not lose their paper commitment when an unpaid PIN would expire.

## Physical-fault to refund sequence

1. Pi reports jam/cover/output fault or job `PAUSED_ERROR`.
2. Cloud durably latches `kiosk_service_flags.out_of_service`. All mobile
   stock checks and new checkout sessions are rejected, even after telemetry
   returns healthy, until an authenticated operator explicitly restores service.
3. Cloud creates a single **full refund obligation** from the captured payment
   (`refund_requests.REQUIRED`). This is not the same as a processed refund.
4. Pi stops queued sibling jobs and tries to cancel **every** CUPS child job.
   Only confirmed `canceled`/`aborted` or never-submitted jobs count as safe.
   A held, processing, unknown, or ambiguous job is **not** safe.
5. Pi sends an authenticated `safe_stop` attestation to cloud. Cloud verifies
   kiosk/order identity and persists `safe_stop_verified=1`.
6. Only then may the worker submit a full refund with the order's durable
   idempotency key. Retries reuse the same key; pending refunds are polled for
   provider completion. The UI differentiates required, pending, retrying and
   processed. No bank-settlement timeline is promised.
7. A paid `PAID_WAITING` order with **no dispatch intent at all** and no
   active kiosk session is auto-marked `PAUSED_ERROR` after two minutes,
   and a refund is safe without Pi evidence because no print was ever dispatched.
8. Failed/ambiguous CUPS cancellation must remain visibly unresolved pending
   operator reconciliation. **Never** tell the customer refund is completed
   until the provider reports it.
9. The operator restores the kiosk only after fresh healthy printer telemetry,
   verified safe-stop obligations and both kiosk and operator authentication.

## Refund states

`REQUIRED → REQUESTING → PENDING → PROCESSED`; a failed attempt can be
retried using the identical provider idempotency key. A processed refund is
terminal for printing. The DB stores provider IDs and paid/refund amounts.

## Required runtime configuration

- `PAYMENT_PROVIDER=razorpay` and Razorpay credentials for actual refund
  requests; mock provider performs no real financial transfer.
- `XEROXGO_OPERATOR_TOKEN` (at least 16 characters, **independent** of the
  kiosk secret) is needed for an attendant to restore service.
- Authenticated `POST /api/k/{kiosk_id}/restore-service`, including a valid
  kiosk Authorization Bearer header and `X-XeroxGo-Operator-Token`, performs
  restoration only after clean live telemetry and no unsafe outstanding stops.
- `XEROXGO_TEST_POSTGRES_URL` points to disposable Postgres in GitHub CI.
  Never use the live Supabase DB for tests.

## Deployment caveats

- PR #18 remains draft. GitHub CI, disposable PostgreSQL, real Raspberry Pi and
  physical printer safe-stop evidence are required before live refunds.
- The stopped job can have mechanical pages in its physical output path even
  after CUPS cancellation. Hardware fault validation remains mandatory.
- Operator credential provisioning/communication is not automated. The cloud
  stores an out-of-service flag and logs fault reasons; a separate notification
  channel can be added if the venue needs one.
- Absence of a matching safe-stop confirmation intentionally blocks a monetary
  refund rather than risking a refund followed by continued printing. Those
  orders need operator attention and must not be falsely labeled refunded.
