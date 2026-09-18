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
the operation's exact order reference shows a successful sale. Custodian
sales accept only non-execution because their receipts correlate exactly by
order reference. Every resolution records actor and reason on the
operation and in the mutation log. NMI subscription enrollment follows the
upgrade rule: a roster row matching only vault and plan is surfaced as an
operator candidate, not adopted.

Invoice collection is an `invoice_collection` operation on the same ledger:
one per attempt, enqueued atomically with its `invoice_payments` row and the
invoice's `collection_intent_id` pointer, which blocks every competing
collection, void, uncollectible and out-of-band payment until the operation
ends. The operation id is the provider identity (NMI order id, Stripe
idempotency-key root), so a client retry key, a restart or a resumed lease
never mints a second identity. The operation also freezes the instrument it
charges — provider account, custody and the handles that address the card,
read under a shared lock on the method row — and is judged against those
frozen facts forever after: it refuses to submit while the method no longer
matches them, and every receipt read (verifier, `--receipt`, `--not-executed`)
uses the frozen account and the frozen custody's rule, never the method's
current row. An unresolved operation therefore blocks an or#297 custody remap
of the instrument it names (`operation_unresolved`) until it resolves.
A pre-submission failure (unarmed account, missing secret, parked instrument)
parks the operation without consuming an attempt. After the write-ahead fence every adapter error is a possible
submission: NMI-family operations converge only from an exact receipt —
the Query API's successful sale for the operation's order reference, read
back approved, in the frozen currency, for the frozen amount, on the
frozen customer vault (a card frozen as custodian-held has no vault at NMI;
its read binds approval, currency and amount); Stripe replays the same
idempotent sequence through the executor while Stripe still holds the key
(23h) and then waits for operator resolution. The Stripe sequence creates the invoice first,
excluding the customer's pending items, and attaches its line by invoice id,
so nothing it creates can be swept into another invoice; a parsed refusal
becomes definitive only after every draft/open invoice and pending item
stamped with the operation key has been deleted or voided (a failed cleanup
keeps the outcome unknown). A confirmed charge whose local settlement fails
keeps its transaction id on the operation and settles from it after restart;
an invoice that no longer accepts the frozen snapshot fails closed (nothing
written, pointer kept) until an operator repairs it.
The autonomous verifier and `intents resolve --receipt` share that one
exact-receipt path (`--receipt` additionally requires the named transaction
to be the order reference's sale); for Stripe the paid invoice — returned by
the sequence or named by the operator — must carry the operation key and be
paid in the frozen currency for exactly the frozen amount. A provider object
under the operation's identity that contradicts the frozen facts settles
nothing: it is retained as `provider_contradiction` evidence, the operation
stays unknown and `--not-executed` is refused until an operator repairs from
the provider record.
`--not-executed` refuses while the provider shows the operation's charge (for
Stripe it first deletes/voids the operation's unpaid objects and refuses on a
paid one), then fails the attempt without a decline and makes the invoice due
again. A submission refused because the instrument no longer matches the
frozen one is provable non-execution (`instrument_changed`): the attempt fails
without a decline and the invoice is due again, so the next collection freezes
the instrument as it now is. A `pending` operation that never crossed its
submission fence (an account that never armed) has no verifier; `intents
resolve --not-executed` releases it on the strength of the absent fence, and
refuses one that carries the fence. There is no evidence-free unpark/force-resend method. No automatic
compensating cancel or refund is triggered by uncertainty.

A manual rebill (scheduled dunning or a customer retry-now, #809) freezes its
charge at enqueue: the subscription's instrument and customer vault, the price
amount and currency, and the provider account on the operation. It converges
through the same exact-receipt path as invoice collection
(`nmi.ConfirmOrderSale`): the verifier and `intents resolve --receipt` accept
only the order reference's sale approved on the frozen vault for the frozen
amount and currency; a contradicting sale is retained as
`provider_contradiction`, keeps the operation unknown and refuses
`--not-executed`. The renewal records the frozen amount. Before any provider
traffic the operation is re-checked under the instrument's shared row lock
(the #297 custody remap takes it exclusively and refuses while the rebill is
in flight): an instrument moved to another provider account (#657), a changed
subscription method or vault supersedes the operation, and re-deriving the
period's attempt revives it with a fresh freeze.

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
