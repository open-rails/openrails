# Provider mutation uncertainty

A non-idempotent operation may have reached its provider once its durable
attempt has been claimed. After possible submission, an empty successful
search is not proof that it failed. NMI order references correlate an operation;
they do not themselves establish provider-enforced idempotency or definitive
negative/read-after-write semantics.

Checkout sales, recurring enrollment, custodian-proxied sales, manual rebills
and invoice collection therefore retain unknown state until a positive receipt
can be reconciled. Resumed intent handlers verify rather than submit another
mutation. A captured provider receipt survives local finalization failure in
the existing intent result evidence. Local effects are retried against that
same receipt. Query unavailability or delayed visibility never authorizes a
new sale. NMI processor communication/duplicate response codes 420, 421 and 430
also require verification rather than release/resend. Non-idempotent refund
receipt rules follow the same rule; an NMI refund's exact provider id is
retained on the operation if its local receipt write fails.

Known pre-submission refusal (unconfigured/read-only provider, failed local
prerequisite) parks the intent without consuming an attempt. It can be retried
when repaired. A crash between the durable attempt claim and the actual send
is conservatively uncertain; reconciliation is required even if no request
ultimately reached the provider. A parsed definitive decline terminates that
attempt. A provider-idempotent operation, such as the supported Stripe refund
path, may repeat the same provider idempotency key; this is a capability of
that operation, not an inference from an empty search.

Unknown attempts do not expire or become superseded merely because a business
deadline passes. An expired in-flight lease is still reclaimable for
verification. Re-enqueuing the operation cannot reset it to a fresh charge.
Verification retains the existing lease/backoff schedule; it is not a tight
polling loop. Queued operations that were never attempted can expire normally.

An unknown operation that provider reads cannot settle is resolved by an
operator with `openrails intents resolve`, never by resending. A
`--receipt` is read back by its exact provider id and must match the frozen
operation (sale: approved sale of the amount on the vault; enrollment: live
subscription on the vault and plan, unowned locally; refund: approved refund of
the reserved amount on the original sale's vault) before local effects commit
through the normal receipt path. `--not-executed` records provider-confirmed
non-execution and takes the type's definitive-refusal path; it is refused while
the operation's exact order reference shows a successful sale. Rebills and
custodian sales accept only non-execution because their receipts correlate
exactly by order reference. Every resolution records actor and reason on the
operation and in the mutation log. NMI subscription enrollment follows the
upgrade rule: a roster row matching only vault and plan is surfaced as an
operator candidate, not adopted.

Invoice collection is an `invoice_collection` operation on the same ledger:
one per attempt, enqueued atomically with its `invoice_payments` row and the
invoice's `collection_intent_id` pointer, which blocks every competing
collection, void, uncollectible and out-of-band payment until the operation
ends. The operation id is the provider identity (NMI order id, Stripe
idempotency-key root), so a client retry key, a restart or a resumed lease
never mints a second identity. A pre-submission failure (unarmed account,
missing secret, parked instrument) parks the operation without consuming an
attempt. After the write-ahead fence every adapter error is a possible
submission: NMI-family operations converge only from the order-reference
search or an exact receipt; Stripe replays the same idempotent sequence
through the executor while Stripe still holds the key (23h) and then waits
for operator resolution. A confirmed charge whose local settlement fails
keeps its transaction id on the operation and settles from it after restart.
`intents resolve --receipt` accepts an NMI transaction that is an approved sale
of the frozen amount on the instrument's vault, or a Stripe invoice stamped
with the operation key and paid for the frozen amount; `--not-executed`
fails the attempt without a decline and makes the invoice due again (an
unpaid Stripe invoice the sequence left behind is not voided automatically).
There is no evidence-free unpark/force-resend method. No automatic compensating
cancel or refund is triggered by uncertainty.

A manual rebill confirmed after dunning parked or the customer cancelled the
subscription still records its payment exactly once: a parked (`unknown`) or
active subscription renews from the confirmed charge; a terminally cancelled
one gets the completed payment row without reactivation, flagged for refund
review.

Local HTTP/PostgreSQL fixtures qualify response loss, delayed receipt
visibility, restart, expiry, positive receipt finalization and one-time local
effects. They do not demonstrate live NMI replication timing or qualify every
processor's duplicate checking. Provider-specific live guarantees remain
separate release evidence.

Provider reference: NMI's [transaction processing](https://docs.nmi.com/reference/transactions-processing)
describes merchant order correlation and processor-dependent duplicate checking;
its [Query API](https://docs.nmi.com/reference/query) does not establish a
terminal-negative guarantee for a missing search result. The engine therefore
uses absence as inconclusive evidence rather than assuming a provider guarantee.

NMI upgrades use the same intent runner with separate write-ahead step markers
and durable receipts for successor creation and proration. The frozen payload
owns the prices, billing period, instrument and account. A lost successor
response stays unresolved: a roster row matching only vault and plan is not
proof that this operation created it. Proration can recover through its stable
account-scoped order reference. Both receipts commit the local subscription
swap, payment, access effects and predecessor delete intent atomically. A
parsed proration refusal preserves the old subscription and queues a durable
delete for the unpaid successor. See [upgrade recovery](architecture/upgrade-recovery.md).
