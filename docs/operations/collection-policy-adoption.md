# Subscription ownership in a fresh OpenRails database

This release initializes a fresh OpenRails database. It is a hard cut from the
previous database schema, not an in-place upgrade. Keep any source database
needed for import verification; never alter or cancel the external provider
schedules as part of initialization. No old-binary compatibility or provider-to-
engine ownership migration is supplied.

Each subscription has an immutable collection policy. Existing Stripe, CCBill
and ordinary NMI schedules import as `provider`. A reviewed NMI agreement where
OpenRails already manages retry recovery imports explicitly as
`provider_dunning`; it still has a provider-owned recurring schedule. Do not
infer engine ownership from a saved card or from the old card-level
`rebill_driver=openrails` flag.

The declared provider-book import accepts `provider` and NMI
`provider_dunning`, preserves remote schedule references and declared financial
history, and rejects a repeat import that changes ownership. It cannot invent
engine authority. Existing proven engine/noncard history requires a compatible
canonical archive with its accepted operations and execution references, rather
than a provider-book declaration. Missing or contradictory source evidence must
be reviewed before enrollment is enabled. Synthetic repository fixtures are
not a production census of the existing provider book.

Card-engine subscriptions use `rail_subscription_id=''` to mean no remote
recurring schedule. JSON/archive round trips preserve that representation.
Their payment method may be NULL while awaiting replacement; this blocks new
money admission without canceling the agreement. Solana delegated-pull
references are retained and remain serviced by its existing worker.

Canonical archives from the current schema preserve policy and reject
contradictory policy/binding records. Historical archives with different table
shapes are rejected, not silently reinterpreted or upgraded. Provider history
must be mapped and verified through the supported import boundary before the
fresh deployment serves it. Compare source and imported ownership, provider
identities, terms, paid-through/access and financial history with provider
egress disabled. No source records or schedules may be discarded before that
verification.

Deploy policy-aware workers with new engine enrollment held until import and
provider-account qualification pass. After an engine agreement exists,
disabling new enrollment must retain servicing and receipt reconciliation for
that agreement. Sharing or replacing a card cannot change a subscription's
owner.

New supported enrollments use engine-owned agreement confirmation by default.
There is no process-wide collection-owner selector. Recurring NMI
and Stripe prices need no provider catalog links for new engine-owned enrollment; explicit
historical links remain usable. CCBill engine signup and unsupported free/trial
terms refuse before payment. Fixed-hour catalog cycles are the supported new
engine terms; this does not reinterpret old provider calendar/finite schedules.

`engine_admission_hold: true` pauses new initial/renewal payment admission and
first submission while retaining possibly submitted operation verification and
webhook handling. Provider write
mode remains an additional existing execution gate. Neither setting mutates
stored ownership. Existing engine sessions and accepted operations replay their
stored terms across restarts. Existing engine payments that
require Stripe authentication resume the original PaymentIntent through the
owned, non-cacheable authentication resource; a mutable declined PI is canceled
and read back before the obligation becomes retryable.
