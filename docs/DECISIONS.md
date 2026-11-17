# XeroxGo — architecture decisions

> **Status update — 2026-11-17:** Pricing tariff revision 1 was activated previously and pricing PRs #9, #12 and #13 are merged. The sections below marked “Not yet approved” refer to the earlier release stage; they are preserved for decision history. Live rate revisions and backend deployment must be verified anew. The currently proposed checkout optimization is **not approved for implementation until its small architecture plan is reviewed**.


Record decisions with rationale, consequences, and approval status. The code and dated ADRs (when created) supersede guesses from old audit reports.

## Approved / established design constraints
- **Cloud-owned document processing.** A single canonical cloud PDF normalization/imposition path determines printable output and billable sides. No independent JavaScript or Pi imposition/pricing engine.
- **Canonical tariff revisions.** The cloud controls pricing in paise; a single revision applies to a quote/order, and stale accepted amounts/revisions are rejected. Existing persisted order prices must not be recalculated after admin rate changes.
- **Sheet billing.** Full duplex sheets use a duplex-sheet rate; an odd last printed side uses a simplex rate. Blank artificial duplex padding is not billed.
- **Order safety.** A customer order has a PIN and payment state; retries and crash recovery must avoid accidental duplicate prints. Cloud and Pi coordinate through durable job state.
- **Pi printer integration.** Prioritize IP/IPP/CUPS; SNMP augments hardware telemetry, and emulator testing is not equivalent to physical printer acceptance.
- **User experience.** Mobile-first existing visual language (spacing, blue branding, roundness), mixed file uploads, document-specific options; avoid major UI redesign during backend hardening.
- **Review before significant architecture changes.** Present changes, why, impact/trade-offs and get an opportunity for human review.

## Approved pricing policy — 2026-11-17
Human-approved intended production tariff (also used for development testing):
- B&W simplex: **300 paise per side (₹3)**.
- B&W duplex: **500 paise per duplex sheet (₹5)**.
- Color simplex: **800 paise per side (₹8)**.
- Color duplex: **1000 paise per duplex sheet (₹10)**.
- **Color printing initially disabled** (previously stated plan; re-confirm before later enabling).
- Odd final duplex side billed at simplex rate; artificial blank padding not billed.

**Approval scope:** rates/policy only. This does **not** authorize activating the tariff in a database, deploying to production, enabling payment processing, or merging into auto-deploy `main`.

## Not yet approved
- Whether mixed simplex/duplex documents should be represented as separate CUPS tasks, a specially padded merged PDF, or multiple durable print tasks per order. **Do not implement until audited and approved.**
- First tariff activation and deployment timing. Approved price figures are **not** active until a revision is explicitly activated.
- Broad RLS remediation for the 16 pre-existing tables. Needs access-model review and phased tests before migration.

## Decision record template
`ADR-ID | date | status | context | options | decision | consequences | approving person | supporting PR/commit`

Keep this file concise. Preserve deep specifications in code/tests or links to the technical design, not duplicated prose.
