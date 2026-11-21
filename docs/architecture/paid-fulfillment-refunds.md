# Demo payment, paid printing and simulated refunds

XeroxGo currently has one payment implementation. The kiosk QR opens the cloud-hosted
`/pay/{token}` webpage, where customers confirm simulated payment. No money
moves, no UPI request is generated and no external payment gateway is configured.

## Order lifecycle
- Mobile upload reserves the complete order's physical sheet count and issues a 4-digit PIN, valid for five minutes while unpaid.
- Entering the PIN at the kiosk creates one active transaction session; paper and printer health are checked before the demo checkout.
- Payment initiation creates a stored attempt and returns a random demo token. The customer follows the kiosk QR and confirms on the demo webpage.
- `CapturePaymentAndCheckDispatch` authorizes the order exactly once; durable child-document intents are recorded for reconnect.
- Paid orders do not expire simply because the unpaid PIN deadline or kiosk session elapsed. A still-reserved PIN remains valid until fulfillment or terminal failure. An expired PIN can be securely rotated using the order's recovery credential.
- Successful printing closes the entitlement and releases the reservation. An interrupted WebSocket send is never interpreted as a printer failure.

## Faults and simulated refunds
- A printer or print-job fault pauses sibling jobs and triggers safe-stop verification. Ambiguous CUPS submissions and mechanically uncertain output are **not** considered stopped.
- The cloud records one full simulated refund obligation for the funding payment. It does not mark it processed until the Pi confirms the CUPS jobs are canceled, aborted or never submitted.
- The mock refund worker uses a durable idempotency key, retries without issuing a second simulated refund, and records processed/pending/failed status.
- Duplicate demo captures do not create new print entitlements. Unexpected second captures are recorded separately as simulated excess-refund obligations.
- An unavailable printer, stale kiosk heartbeat or absent dispatch intent by itself does not expire a paid entitlement or create an automatic refund.
- The attendant restores a blocked kiosk only after fresh healthy telemetry and verified safe stops using the independent operator credential.

## Data model
New demo databases use `orders`, `order_documents`, `pin_reservations`,
`kiosk_sessions`, `payment_attempts`, `payments`, `order_funding_payments`,
`payment_refund_requests`, `dispatch_intents` and `kiosk_service_flags`.
There is no migration of historical demo orders, retired refund tables,
older PDF artifacts or alternate payment endpoints. Begin with a fresh demo
database when deploying this branch.

The Raspberry Pi SQLite store still journals jobs and CUPS submission
uncertainty to prevent double printing. That is active crash recovery,
not backwards compatibility.

## Verification boundary
GitHub checks and simulated printer tests cannot establish physical sheet
delivery certainty. A Raspberry Pi and physical printer test remains necessary
before relying on hardware safe-stop or odometer evidence.
