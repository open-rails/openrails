# Critical security and money coverage

The required public-interface suite is `critical/`, selected by `critical/coverage.json` and its exact executable map. It uses real AuthKit, PostgreSQL, Redis, Vault and registered OpenRails workers. Passing endpoint refusals alone do not establish complete successful-function coverage.

- J01/J02/J03: credential and merchant boundaries, current customer-network proof, hosted checkout capabilities and origins.
- J09–J21/J28/J29: accepted monetary terms, payment/rebill/refund identity, unknown provider outcomes, authenticated callbacks and process-death recovery.
- J22–J27: conservation of credits, reservations, measured usage, running charges, paid access and entitlement tenure.
- J31/J32/J34: archive/customer/merchant authority, destructive controls and custody boundaries.
- J37: the selected Solana transfer/recurring protocol and settlement evidence.
- J04/J05 and the public client cases: typed wire/amount/identifier boundaries, embedded/HTTP behavior, cache/owner lifecycle and actual shipped SDK behavior.

Exact provider amounts and ledger effects come from independent literal examples and external provider journals, not from counting requests or recomputing expectations using the implementation. Focused parsing/arithmetic/signature vectors complement the real workflows. Faults act on transport, process lifetime, external provider evidence or declared time boundaries.

The current network cases stay qualified while that feature ships in OpenRails. SaaS issue67 will move their implementation and tests together; it must not remove the protections from both repositories. Optional protocols and UI behavior outside the selected manifest remain explicitly deferred.

Source/security scanners, SQLC preparation, migration checks and the embedded dependency-boundary build remain additional gates. No source-managed consumer/ACK protocol or internal authorization mock is introduced by the replacement.
