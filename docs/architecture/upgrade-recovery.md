# Durable NMI upgrade recovery

An upgrade is one durable rail intent with immutable pricing, billing period, instrument and predecessor/successor identities. Each provider step records its submission boundary before calling NMI and retains a positive receipt or definitive refusal after the response. A restart resumes an unsent step; a possibly submitted step only reads provider evidence. Empty search results never permit resubmission.

The successor enrollment and proration charge have separate receipts. Once both are established, the predecessor cancellation, successor registration, payment record, access changes and deferred provider-deletion intent commit atomically. Local failure leaves the provider receipts intact so restart can repeat only the local transaction.

The operation captures the merchant and immutable PSP. An unresolved operation
owns its predecessor, so changing the request key cannot start a competing
upgrade. Replays validate the customer and request coordinates and return the
original amounts and billing date, even after catalog edits or cancellation of
the predecessor. No request-cache entry is required for recovery.

A successor roster match on vault and plan is insufficient evidence. Without
an exact positive enrollment receipt, that step remains unresolved. A lost
proration response can recover from its stable account-scoped order reference;
an empty query never permits resend. A definitive proration refusal queues the
known successor for durable cancellation while leaving the predecessor active.

An unresolved step is closed with `openrails intents resolve --step successor|proration`.
A successor receipt must be a live subscription on the frozen vault and plan
that no other local subscription owns; a proration receipt must be an approved
sale of the frozen amount on the frozen vault. The resolved step then follows
the verifier path: an unsent proration is still submitted only by the executor.
Successor non-execution terminates the operation and releases the predecessor
for a new request; proration non-execution queues successor cancellation.

Provider search visibility and exact correlation remain adapter evidence; local loopback tests prove the application's no-resend and transaction behavior, not a live provider guarantee.
