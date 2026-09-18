# Durable tier change recovery

## NMI upgrades

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

## Stripe tier changes

A Stripe tier change is one `stripe_tier_change` operation keyed by the client's `Idempotency-Key`. Its payload freezes the subscription, the current and target prices (local and Stripe ids), the action, the proration decision, the local now-amount estimate and the period; Stripe is read once before freezing and must bill the frozen current price and carry no schedule. An upgrade is one request (`update`): the subscription item moves to the target price with `proration_behavior=always_invoice` and `billing_cycle_anchor=now`. A downgrade is two (`schedule`, then `phases`): a schedule is created from the subscription, then given the current phase to the frozen period end and the target price after it, released at its end.

The upgrade sends `payment_behavior=error_if_incomplete` ([subscription update](https://docs.stripe.com/api/subscriptions/update)): Stripe either applies the change with its invoice paid or refuses it (`402`) with nothing changed. Its default, `allow_incomplete`, would answer an updated subscription with an unpaid invoice — a change that looks committed but is not paid.

Every request carries `<operation id>:<step>` as its Stripe idempotency key and stamps `metadata[openrails_tier_change]=<operation id>` on the object it mutates. Each step records its fence before sending and then exactly one of a matched receipt or a definitive refusal. A receipt is the subscription or schedule Stripe answers with, or that the verifier reads back by exact id, matched to the frozen facts (operation key, Stripe price, local price, switch date); a 2xx object that does not match commits nothing. A lost response is reconciled by the exact read first; while Stripe still holds the key (23h) the executor replays the identical request and Stripe answers the stored result; afterwards only an operator closes the step with `openrails intents resolve --step update|schedule|phases`, whose `--receipt` is read back and matched the same way and whose `--not-executed` is refused while the provider shows the step's effect. An attached schedule the operation cannot attribute by key is never adopted.

The local commit — the price, product and period for an upgrade; the scheduled price for a downgrade — happens only from a matched receipt and from the receipt's own dates: `billing_cycle_anchor=now` is Stripe's clock at execution, not the enqueue-time estimate the payload froze. It is idempotent and fails closed if the subscription left both the frozen predecessor and the target. The webhook converger may mirror the same Stripe subscription first; a subscription already on the target price is complete, and only a period older than the receipt's is brought up to it, so a webhook-first recovery converges instead of stranding.

One `Idempotency-Key` is required and names one tier change: a key that already belongs to a different customer, subscription or target is refused (`409 tier_change_idempotency_conflict`) before the operation it names can run or answer, including when two requests under the same key both miss the replay lookup and the later enqueue meets the earlier row. The stored result answers every same-key replay (`200`), an unresolved operation answers `202` with its id, a request under another key is refused `409 tier_change_in_flight` naming it, and a definitive Stripe refusal is coded (`tier_change_refused`; a 402 keeps its decline code). Loopback Stripe fixtures prove no-resend, identical replay, restart convergence and one-time local effects; Stripe's idempotency retention and object read-back are modelled, not measured.
