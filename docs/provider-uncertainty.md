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
through the normal receipt path. `--not-executed` requires the operation type's supported non-execution proof; it is not an override of provider uncertainty.
For invoice collection, an empty NMI search after possible submission is insufficient. Rebills and
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
never mints a second identity. The operation also freezes the instrument it
charges — provider account, custody and the handles that address the card,
read under a shared lock on the method row — and is judged against those
frozen facts forever after: it refuses to submit while the method no longer
matches them, and every receipt read (verifier, `--receipt`, `--not-executed`)
uses the frozen account and the frozen custody's rule, never the method's
current row. An unresolved operation therefore blocks an or#297 custody remap
of the instrument it names (`operation_unresolved`) until it resolves.
Automatic NMI collection requires an approved unscheduled stored-credential
reference. Creating a vault record alone does not establish that agreement:
designating an unanchored card as the collection method is refused. A prior
customer-present charge can establish the scoped reference; an unscoped initial
transaction id is not a fallback. Customer-present invoice onboarding and its
subsequent off-session workflow are a separate #809 acceptance item.

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
first commits its qualified receipt on the operation and settles from that receipt after restart;
an invoice that no longer accepts the frozen snapshot fails closed (nothing
written, pointer kept) until an operator repairs it.
The autonomous verifier and `intents resolve --receipt` share that one
exact-receipt path (`--receipt` additionally requires the named transaction
to be the order reference's sale). Stripe also reads the invoice's actual captured
charge and checks its customer, payment method, amount and currency. A paid
invoice or its default payment method alone is not proof of which card paid it.
Both readers are bound to the credential plane's immutable merchant/account;
callers cannot label a reader for account B as account A.

The NMI receipt independently proves account, order, vault, approved sale,
amount and currency. NMI does not expose a documented historical billing-id
link in this receipt. Shared-vault billing-id is frozen and sent exactly, and
local drift before submission refuses the charge, but the receipt does not
independently prove that billing record. Current vault contents or an unqualified
card hash are not substituted for missing historical evidence.

Qualified receipt custody is insert-once against the canonical accepted payload.
A bare transaction id remains only a candidate; it cannot finalize a payment.
Generic progress writes cannot replace the receipt, and pruning and portable
archives retain and revalidate its binding. Receipt custody categorically
precludes non-execution, including a crash before the safe result projection
was written. Computed invoice dues round up to provider minor units; the exact
due is settled and the remainder becomes spendable purchased credit in the same
local transaction. A provider object
under the operation's identity that contradicts the frozen facts settles
nothing: it is retained as `provider_contradiction` evidence, the operation
stays unknown and `--not-executed` is refused until an operator repairs from
the provider record.
For a submitted NMI invoice collection, `--not-executed` refuses even when the
order search is empty: query visibility is not authoritative non-execution.
The invoice remains owned by its unresolved operation, so no new charge is
admitted. Stripe can close after deleting/voiding the scoped operation's unpaid
objects and reading back that none remains chargeable; a paid object or a
retained receipt always refuses. A submission refused because the instrument no longer matches the
frozen one is provable non-execution (`instrument_changed`): the attempt fails
without a decline and the invoice is due again, so the next collection freezes
the instrument as it now is. A `pending` operation that never crossed its
submission fence (an account that never armed) has no verifier; `intents
resolve --not-executed` releases it on the strength of the absent fence, and
refuses one that carries the fence. There is no evidence-free unpark/force-resend method. No automatic
compensating cancel or refund is triggered by uncertainty.

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

A Stripe tier change is a `stripe_tier_change` operation on the same ledger,
keyed by the request's `Idempotency-Key`. The payload freezes the
subscription, the from/to prices (local and Stripe), the proration decision
(`always_invoice` with the cycle reset for an upgrade; a two-phase schedule
with no proration for a downgrade), the local now-amount estimate and the
period before anything is sent; Stripe is read once beforehand and must bill
the price the local subscription records. Each Stripe request carries an
idempotency key rooted in the operation id and stamps it on the object
(`metadata[openrails_tier_change]`), behind a write-ahead fence. The receipt
is the subscription (or schedule) Stripe answers with or that the verifier
reads back by exact id, and it must match the frozen facts — the operation
key, the frozen Stripe price, the frozen local price (and for a downgrade
the frozen switch date) — before the local subscription changes; a 2xx
object that does not match commits nothing. The update also carries
`payment_behavior=error_if_incomplete`, so a 2xx receipt means the change
applied with its invoice paid and a 402 that nothing changed. A lost response
answers `202` with the operation id; the verifier reads the exact object back,
and while Stripe still holds the key (23h) the executor replays the identical
request;
after that only `intents resolve` closes it (`--receipt` is the exact
subscription or schedule id, read back and matched; `--not-executed` is
refused while the provider shows the change). A parsed Stripe 4xx is a
definitive refusal (coded `tier_change_refused`, a 402 keeps its decline
code); an operator closure answers `409`. The same key replays the stored
result byte for byte; another key while the operation is unresolved is
refused `409 tier_change_in_flight` naming it. One unresolved tier change
(NMI upgrade or Stripe) owns its subscription. The local commit uses the
period the receipt carries, so a webhook-first convergence (the converger
mirrors the price before the verifier runs) settles the operation instead of
stranding it. Every tier change requires the client's `Idempotency-Key`, and a
key already naming a different request is refused
(`409 tier_change_idempotency_conflict`) before that operation can run or
answer.

NMI upgrades use the same intent runner with separate write-ahead step markers
and durable receipts for successor creation and proration. The frozen payload
owns the prices, billing period, instrument and account. A lost successor
response stays unresolved: a roster row matching only vault and plan is not
proof that this operation created it. Proration can recover through its stable
account-scoped order reference. Both receipts commit the local subscription
swap, payment, access effects and predecessor delete intent atomically. A
parsed proration refusal preserves the old subscription and queues a durable
delete for the unpaid successor. The route answers exactly as for a Stripe tier
change: `202` with the operation id while unresolved, the stored result under
the same key, `409 tier_change_in_flight` under another key, and a coded
`tier_change_refused` (a card decline keeps its code) once terminal. See
[upgrade recovery](architecture/upgrade-recovery.md).

### NMI refund currency and non-execution

Refund operations retain the original payment's currency with their accepted
minor-unit amount. The NMI adapter renders that amount in major units using the
currency's scale; the local refund reservation remains in native currency units.
For example, 100 JPY is 100 provider minor units and is sent as `100.00`, not `1.00`.
Missing or unknown currency is refused rather than treated as USD.

A possibly submitted NMI refund cannot be resolved with `--not-executed` from an
empty action list, an unmatched amount, or an operator's claim about a dashboard.
The reservation remains held, and the verifier does not send another refund.
Known response receipts retain their existing recovery path; operator receipt
reads additionally require the named identities, currency and exact refund
amount. This is an engine/wire correction tested with loopback provider fixtures;
it does not qualify a particular NMI account or establish missing provider
operation-correlation guarantees.
