# Paper reservation at mobile PIN issuance (PR #18)

## Customer workflow

1. Customer selects the kiosk, uploads prepared documents and confirms print settings on the mobile site.
2. The cloud normalizes each document and computes the **authoritative number of physical sheets** required by the entire order, including copies, N-Up, duplex and all documents.
3. The cloud checks the connected kiosk's latest printer health/estimated paper stock. Missing, stale, offline or faulted telemetry cannot issue a PIN.
4. The cloud generates a candidate PIN and enters an order-creation transaction. Within that transaction it locks paper allocation for the target kiosk, subtracts all current commitments, and inserts the pending order, child documents, print configuration and PIN reservation **only if sufficient paper remains**. PIN collisions retry within the same checked path.
5. The mobile site displays the PIN only after that transaction succeeds. A rejection shows an actionable error; no order/PIN is issued.
6. A customer enters the PIN at the kiosk. Valid PIN lookup automatically starts the payment session and QR request; **there is no intermediate order-review or Confirm-to-Pay screen**.
7. Verified payment authorizes printing via the existing payment/dispatch safety path. Sheets already printed reduce the outstanding commitment. An unpaid expired PIN releases its commitment.

## Accounting invariants

- Kiosk PIN lookup reports aggregate `orders.total_pages` and `orders.total_sheets`, never the first child document's figures.
- `orders.total_sheets` is the **only** physical sheet total used for allocation. `print_configs.total_sheets` refers to the first print job and is not an order-level stock count.
- At time `now`, committed stock is:
  - the full sheet total of every `PENDING_PAYMENT` order whose `pin_expires_at_ns` is still in the future, **whether or not the user has visited the kiosk**;
  - plus the positive remainder of paid/dispatching/printing/paused orders after subtracting child-job sheet progress.
- The current order is excluded from its own revalidation to avoid double-counting.
- During mobile order creation, the checked stock calculation and order+PIN insertion share a transaction. PostgreSQL uses the existing per-kiosk advisory lock `pg_advisory_xact_lock(22615, hashtext(kioskID))` so competing customers cannot simultaneously reserve the same sheet.
- The estimated stock is the **Pi-reported paper estimate**. This is not a physical tray-sheet sensor guarantee. Kiosk claim and payment initiation still revalidate live printer health and stock so faults or unexpected depletion fail safely.

## Expiration and cancellation

- Unpaid PINs issued by mobile upload currently expire after **5 minutes**; their stock commitment ends at that timestamp.
- Kiosk checkout/session cancellation, inactivity, or session expiration does **not** abandon the still-valid mobile reservation. The customer may re-enter their PIN until it expires.
- Paid/queued/printing jobs remain committed until their outstanding sheet requirement has been consumed or they reach an appropriate terminal state.
- Recovery PINs for paid, unprinted orders are **not subject to the five-minute unpaid timeout**. The backend currently holds these paid PINs with a far-future internal lease, and revokes them on fulfillment closure. Paid entitlements and short unpaid reservations are distinct.
- Kiosk "Esc" exits the checkout session; it is not a command to delete the customer's pending mobile order.
- There is no legacy paper-count fallback or automatic migration of historic development records with missing totals.

## Testing

- Mobile upload rejects offline/stale/insufficient printer stock without issuing any PIN.
- A one-sheet kiosk issues one one-sheet PIN but rejects the next one-sheet order without a refill.
- Concurrent mobile order-finalization requests cannot exceed kiosk stock.
- Multi-document orders reserve aggregate physical sheets, not the first document's sheets.
- A kiosk session does not double-reserve an already-held PIN order; cancellation/expiration preserves the reservation until PIN expiration.
- PIN submission opens the payment screen and initiates a QR automatically; a keypad Submit on an existing QR must not bypass payment authorization.
- Paid and partially printed orders hold only the remaining physical sheets.

## Deployment notes

This design lives in draft PR #18. It must pass all verification gates, including PostgreSQL concurrency tests on a **disposable** test database where available, before merge. No live Supabase writes are required for this change.
