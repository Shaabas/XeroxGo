# Canonical order, PIN and payment endpoints

The current XeroxGo implementation intentionally has one route per operation.
Retired alternatives are not supported.

| Operation | HTTP route |
|---|---|
| Upload one or more documents | `POST /api/k/{kiosk_id}/upload` |
| Look up kiosk PIN | `POST /api/k/{kiosk_id}/orders/lookup` |
| Claim order at kiosk | `POST /api/k/{kiosk_id}/orders/{order_id}/claim` |
| Read active transaction | `GET /api/k/{kiosk_id}/sessions/{session_id}` |
| Cancel transaction | `POST /api/k/{kiosk_id}/sessions/{session_id}/cancel` |
| Initiate demo payment | `POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment` |
| Confirm demo payment | `POST /pay/{token}` |
| Release an already-paid order | `POST /api/k/{kiosk_id}/orders/{order_id}/release` |
| Read order status | `GET /api/orders/{order_id}/status` |
| Download one signed child PDF | `GET /api/orders/{order_id}/jobs/{job_id}/pdf` |

The server has no real payment provider and does not charge customers.
Payment safety, session validity, idempotency and Raspberry Pi print
deduplication still apply. No backward-compatible route aliases exist.
