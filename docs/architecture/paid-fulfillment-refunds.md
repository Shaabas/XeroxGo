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

## Paid dispatch transport and reconnect

- Payment capture atomically records **one print authorization** plus a
  `PENDING` intent for each child document. It does **not** prove the Pi
  received a command or that the printer started printing.
- A WebSocket disconnect, unavailable cloud hub, failed write, or failed
  persistence of `SENT` is **delivery uncertainty**, not a printer fault.
  Such an event must never block a healthy kiosk or create a
  `FULFILLMENT_FAILED` refund obligation.
- The webhook acknowledges an already captured payment with
  `dispatch_pending` when immediate transport fails. Paid kiosk release
  responds HTTP 202 with `DISPATCH_PENDING`; the kiosk client treats this
  as durably accepted for eventual delivery, not as proof of printing.
- On the next authenticated Pi WebSocket connection, previously `SENT`
  but unacknowledged intents revert to `PENDING`. The cloud resends their
  immutable child JobIDs. The Pi's durable SQLite deduplication prevents
  accepted jobs from being submitted twice to CUPS; confirmed
  `ACKNOWLEDGED` and `COMPLETED` jobs are never replayed.
- Delivery errors are recorded in the intent's `last_error` and retry
  counter, without discarding its physical-print authorization. An
  interrupted multi-document send preserves all sibling intents.
- Reconnect verifies the **funding payment**, not whichever captured
  payment arrived most recently; an excess capture (including mismatched
  amount) cannot block legitimate print delivery.
- Real printer faults and safe-stop failures remain separate terminal
  safety paths. Existing orders previously latched out-of-service or
  marked for refund must be reviewed individually before any remediation;
  a deployment does not blindly un-block or reverse a provider refund.
- A valid authenticated session must have authorized the initial dispatch.
  A payment arriving after a canceled/expired session remains
  `PAID_WAITING` until the customer explicitly reclaims it.

## Physical-fault to refund sequence

1. Pi reports jam/cover/output fault or job `PAUSED_ERROR`.
2. Cloud durably latches `kiosk_service_flags.out_of_service`. All mobile
   stock checks and new checkout sessions are rejected, even after telemetry
   returns healthy, until an authenticated operator explicitly restores service.
3. Cloud creates a **full refund obligation for the funding payment**
   (`payment_refund_requests.REQUIRED`). This is not the same as a processed refund.
4. Pi stops queued sibling jobs and tries to cancel **every** CUPS child job.
   Only confirmed `canceled`/`aborted` or never-submitted jobs count as safe.
   A held, processing, unknown, or ambiguous job is **not** safe.
5. Pi sends an authenticated `safe_stop` attestation to cloud. Cloud verifies
   kiosk/order identity and persists `safe_stop_verified=1`.
6. Only then may the worker submit a full refund with the funding payment's
   durable idempotency key. Retries reuse the same key; pending refunds are polled for
   provider completion. The UI differentiates required, pending, retrying and
   processed. No bank-settlement timeline is promised.
7. A captured `PAID_WAITING` order remains a durable fulfillment entitlement
   even if no dispatch intent exists, the kiosk goes offline, or its checkout
   session expires. **Elapsed time alone must never produce `PAUSED_ERROR` or a
   `FULFILLMENT_FAILED` refund obligation.** The two-minute auto-refund worker
   path and its unsafe database helpers have been removed.
8. On return, an unprinted customer may claim their still-valid paid PIN, or
   securely rotate an expired/recycled PIN using their original recovery
   credential. A new session authorizes a first dispatch without recharging.
9. A genuine terminal fulfillment failure still creates a funding-payment
   refund obligation through an explicit failure path. Its safe-stop requirement
   remains in force. Preexisting refund obligations are not silently canceled,
   reversed, or reclassified by this policy change.
10. Failed/ambiguous CUPS cancellation must remain visibly unresolved pending
    operator reconciliation. **Never** tell the customer refund is completed
    until the provider reports it.
11. The operator restores the kiosk only after fresh healthy printer telemetry,
    verified safe-stop obligations and both kiosk and operator authentication.

## Payment-level capture and refund accounting

- Exactly one verified captured payment may fund an order and authorize printing.
  The binding is serialized per order and persisted in `order_funding_payments`.
- A provider-signed capture matched to a persisted attempt but carrying an
  incorrect amount is recorded as refund-only, never applied to printing.
  Unassociated captures still require secure provider/order reconciliation.
- Every additional distinct captured payment is persisted atomically with a
  separate `payment_refund_requests` obligation, even if the order has already
  completed. Repeated callbacks for the same payment ID do not duplicate money.
- `EXCESS_CAPTURE` refunds are actionable independently; they do **not** stop
  the legitimate order, require CUPS cancellation, or release its paid PIN.
- `FULFILLMENT_FAILED` refunds apply only to the funding payment and still
  require verified physical safe-stop before submission.
- Each payment's refund carries a unique stable Razorpay idempotency key,
  provider refund ID, status and retry history.
- The old `refund_requests` table is retained as a legacy archive. Startup
  migrates its existing obligations into the payment-scoped table, preserving
  in-flight state and provider identifiers, and backfills previously unrecorded
  surplus captured payments. Repeat startup must not reset these records.
- Orders with unresolved refund obligations are protected from normal artifact
  cleanup; completed legitimate orders may still have excess refunds pending.

## Refund states

`REQUIRED → REQUESTING → PENDING → PROCESSED`; a failed attempt can be
retried using the identical provider idempotency key. A processed refund is
terminal for printing. The DB stores provider IDs and paid/refund amounts per captured payment.

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
