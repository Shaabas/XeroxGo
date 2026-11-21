# Orders and sessions: demo-only architecture audit

The cloud owns order totals, paper commitments, payment state and
authorization. The Pi owns physical printing and local CUPS job
deduplication.

## Session rules
- Upload allocates full-order sheets and a five-minute unpaid PIN.
- Claiming the PIN creates a kiosk-scoped active checkout session.
- Only `/sessions/{session_id}/initiate-payment` can initiate checkout.
- The kiosk's QR opens `/pay/{token}`; the customer confirms a simulated
  payment and no external processor is contacted.
- One simulated capture funds one order. Duplicate confirmation is
  idempotent; reconnect replays only unacknowledged child intents.
- After successful simulated payment, the unpaid PIN deadline is not
  a printing entitlement timeout.
- Print-job fault handling requires verified safe-stop before simulated
  refunds are marked processed. Uncertain CUPS output requires attendant
  intervention.

## Intentional constraints
No provider choice, public gateway callback, alternate payment
simulation route, compatibility migration, old session aliases or
historical single-job records. Fresh demo data is required.
