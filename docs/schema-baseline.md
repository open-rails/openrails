# Fresh pre-v1 schema baseline

OpenRails installs one baseline, `0001_schema.up.sql`. AuthKit and River retain
independent schemas and migration ownership. The owner has declared all pre-v1
OpenRails data disposable: this release targets fresh databases, with no legacy
upgrade or backfill path. Published tags remain immutable.

Customer identity is `(merchant_id, id)`, where `id` is the stable subject UUID.
The same person can hold separate balances, payment methods, subscriptions and
invoices at different merchants. Issuer is last-seen audit metadata. There is no
second stored subject string or global customer ownership claim.

Operational foreign keys carry merchant identity. References that bind a saved
method, payment, grant or invoice to a payer also carry payer identity; invoice
and ledger-movement relationships carry their unit where required. Nullable
relationship deletion clears the reference alone, preserving merchant/payer
identity. Immutable ledger attribution remains free of control-plane FKs so
history does not cascade when operational rows are removed.

The baseline includes the previously qualified Solana cancel/tier-change modes
and successful-insert-only ledger counters. Duplicate operation attempts do not
change counters or recheck an already consumed balance. The obsolete0002–0004
files are folded into this fresh installation target.

Apply the baseline to a new database or explicitly disposable task-owned schema.
Do not relabel an old migration ledger as current. Normal startup migration
verification remains in place to reject mismatched artifacts. Tests use the
actual migrator, PostgreSQL18 and the enforcing application role; schema checks
cover customer keys, operational relationships and deliberate immutable-history
exceptions, alongside shared-subject HTTP and embedded workflows.
