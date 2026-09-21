# Adopting subscription collection policy

The supported additive lineage is the published `0001_schema` and
`0002_creator_catalogs` from source `0b560bbf4`, followed by
`0003_collection_policy`. Applied migrations and their tracking records are not
rewritten. A different deployed lineage requires a reviewed upgrade before
activation; do not reset the book or restamp migration checksums.

The upgrade labels existing NMI subscriptions whose retained card explicitly
had `rebill_driver=openrails` as `provider_dunning`. Their provider schedule,
periods and payment history stay intact. Existing Stripe/CCBill and ordinary
NMI agreements remain `provider`. Existing Solana delegated-pull sidecars
establish `engine` authority while retaining their on-chain execution reference.
Other rows remain provider-owned for review, without enabling engine charges.
The old payment-method column is retained as inert archive/deployment data.
Current runtime authority is the immutable subscription policy. Sharing or
replacing a card cannot change it.

Card-engine subscriptions use the existing non-null string representation:
`rail_subscription_id=''` means no remote recurring schedule. JSON/archive
round trips preserve the empty string. Their payment method may be NULL while
awaiting replacement; that condition blocks new money admission and does not
cancel the agreement. Solana references are retained and remain serviced by
its delegated-pull worker.

Deploy all policy-aware workers and binaries with new engine enrollment held.
Do not overlap pre-policy workers with engine admission: an old worker can
still query subscriptions directly. Retaining the old database column or its
three-argument fan-out function does not make an old binary engine-safe.
After any engine agreement exists, rollback must retain policy-aware servicing
and receipt reconciliation; disabling new enrollment alone must not strand it.
Do not run an older migration loader against the extended ledger.

Before activation, apply the upgrade to an operator-controlled restored book
with provider egress disabled. Compare all preexisting subscription fields
apart from the new policy, payment methods, balances, grants, price/provider
identities, accepted operations and the prior migration ledger. Review missing
schedule references, NMI recovery rows with missing/contradictory card scope,
and Solana rows without sidecar proof. No production census or customer data
classification is claimed by the repository's synthetic regression tests.

Canonical archives produced after adoption retain the policy and legacy card
flag, reject unsupported policy/binding combinations, and restore the same
policy. For an archive produced by the previous schema, first restore it using
the matching published binary into an isolated database, then run the additive
upgrade and export with this version. A width-mismatched historical archive is
not silently reinterpreted as the new schema.

The trusted process setting `new_subscription_collection_policy: engine` opts
new supported enrollments into owned agreement confirmation. The default empty
or `provider` value preserves native enrollment during rollout. Recurring NMI
and Stripe prices need no provider catalog links in engine mode; explicit
historical links remain usable. CCBill engine signup and unsupported free/trial
terms refuse before payment. Fixed-hour catalog cycles are the supported new
engine terms; this does not reinterpret old provider calendar/finite schedules.

`engine_admission_hold: true` pauses creation of due renewal obligations while
retaining existing operation verification and webhook handling. Provider write
mode remains an additional existing execution gate. Neither setting mutates
stored ownership. Existing engine sessions and accepted operations replay their
stored terms after enrollment defaults change. Existing engine payments that
require Stripe authentication resume the original PaymentIntent through the
owned, non-cacheable authentication resource; a mutable declined PI is canceled
and read back before the obligation becomes retryable.
