# NMI dispatch qualification and receipt recovery (#1055)

Owner: `/root/astra_checkout_core`. Branch:
`feat/1055-nmi-dispatch-qualification-20260923`. Base:
`a47c1a9fe2e5e7b0550b4efab2d0c1314fa34600` (OpenRails 0.159.0).

## Implementation contract

- Separate the declared NMI endpoint deployment (`gateway` or `sandbox`) from
  the deployment's existing test/live credential posture. Default selection
  preserves existing dedicated-sandbox behavior. Never infer provider identity
  or replace the required Gateway AccountID with a credential hash.
- For a regular gateway under test posture, qualify the exact captured credential
  with a fresh read-only Query `test_mode_status` response before dispatch.
  Only one well-formed `nm_response/test_mode_enabled` equal to `true` permits
  dispatch. False, unavailable, malformed, contradictory and unknown results
  refuse without a customer mutation or submission fence. No cached verdict,
  redirects, endpoint fallback or query-to-financial-probe fallback.
- Dedicated sandbox qualification retains the existing bounded synthetic probe;
  its internal auth/void must not recursively invoke qualification. Reads and
  accepted-attempt replay never create synthetic probes.
- Reuse existing intent submission fences and financial result authority. Test
  that refusal precedes these fences and cannot be classified as an uncertain
  charge; unresolved previously submitted attempts remain reserved.
- Add customer/merchant-scoped read-only checkout receipt lookup by idempotency
  key plus an accepted resource filter. It needs no payment token and does not
  weaken the existing full-request Create/Lookup fingerprint contract.
- Project NMI receipts from the existing rail intent state. A lost response or
  browser reload can recover processing/success without tokenizing or paying
  again. Losing a token before provider dispatch never authorizes a replacement
  token under the old request fingerprint.

## Qualification

Use deterministic HTTP provider fixtures and real PostgreSQL tests for fresh and
flipped mode, malformed/error/redirect responses, no dispatch on refusal,
customer/merchant/resource isolation, and submitted receipt convergence. Keep
embedded/remote behavior identical. No real provider writes belong to this lane.
