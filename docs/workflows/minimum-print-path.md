# XeroxGo canonical demo print workflow

The current implementation uses **only simulated payment**. The QR on the
kiosk opens XeroxGo's own `/pay/{token}` webpage. There is no UPI
transaction, gateway webhook or external refund provider.

## Sequence

```mermaid
sequenceDiagram
    actor Customer
    participant Cloud as Cloud Backend
    participant DB as Order Store
    participant Pi as Raspberry Pi
    participant Printer as CUPS Printer
    Customer->>Cloud: Upload PDF(s) with print settings
    Cloud->>DB: Reserve full-order paper and five-minute unpaid PIN
    Cloud-->>Customer: PIN, order details
    Customer->>Pi: Enter PIN and confirm order
    Pi->>Cloud: POST /api/k/{kiosk}/orders/lookup
    Pi->>Cloud: POST /api/k/{kiosk}/orders/{order}/claim
    Cloud-->>Pi: Active checkout session
    Pi->>Cloud: POST /api/k/{kiosk}/sessions/{session}/initiate-payment
    Cloud-->>Pi: XeroxGo demo QR (/pay/{token})
    Customer->>Cloud: Confirm demo payment on /pay/{token}
    Cloud->>DB: Capture simulated payment exactly once; save child intents
    Cloud-->>Pi: Send signed per-document print jobs over WebSocket
    Pi->>Cloud: Download and checksum-verify each normalized PDF
    Pi->>Printer: Submit jobs via CUPS with local SQLite deduplication
    Pi->>Cloud: Report progress/completion
    Cloud->>DB: Close paid entitlement after all child jobs complete
```

## Key invariants

- Authoritative A4 normalization, imposed pages, billed sides and physical
  sheet accounting are calculated in the cloud.
- The **whole order** is reserved when the mobile upload completes, not
  when the customer reaches the kiosk.
- No duplicate payment capture can authorize a second print.
- CUPS submission uncertainty blocks unsafe automatic reprinting.
- The cloud reconnects pending child-document intents using immutable job IDs.
- Printer faults may require safe-stop verification before completing a
  **simulated** refund; a network disconnect alone is not a printer fault.
- Paid PINs are not subject to the unpaid five-minute timeout.

## Canonical endpoints

| Action | Route |
|---|---|
| Mobile upload | `POST /api/k/{kiosk_id}/upload` |
| Kiosk PIN lookup | `POST /api/k/{kiosk_id}/orders/lookup` |
| Kiosk claim | `POST /api/k/{kiosk_id}/orders/{order_id}/claim` |
| Demo checkout initiation | `POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment` |
| Customer demo confirmation | `POST /pay/{token}` |
| Per-document PDF | `GET /api/orders/{order_id}/jobs/{job_id}/pdf` (signed) |
| Order status | `GET /api/orders/{order_id}/status` |

The old order-scoped PDF, alternative payment initiation and generic
payment simulation routes are intentionally unsupported. Older demo database
records are not migrated.
