# Final pre-launch schema baseline

The baseline contains the effective OpenRails schema through migration 0023,
including immutable merchant bindings/retirement, UUID custom-credit units and
unbound-only local name uniqueness. AuthKit and River retain independent migration
histories. New OpenRails migrations begin at 0002.

The source chain was captured from PR348 revision `b84cf0a66` through the real
standalone migrator on PostgreSQL18.6, then dumped with `--schema-only --no-owner`.
The baseline groups each object's constraints, indexes, triggers, row security and
grants beside it, preserving dependency order. Initial destructive-operation
state is one disabled switch. No other application rows are seeded.

The migrated chain and fresh baseline contain the same schema objects and grants.
The only dump difference is equivalent parentheses around an AND expression in
`provider_billing_qualification_evidence_shape`; operands and checks are unchanged.
Fresh migration, owned-schema teardown/reinstall, and the existing standalone and
embedded orphan-ledger refusal tests verify the new installation boundary.

A baseline rewrite is a fresh-database operation. Never clear a migration ledger
on an existing schema to make it appear current. The standalone and embedded
orphan-migration fences continue to refuse incompatible retained databases.

Validation uses disposable `openrails_or927_*` databases in the audit PostgreSQL
instance. A read-only census found no OpenRails/billing schema in the15 retained
AuthKit/Cozy databases visible on this host. Other audit fixtures are retained for
their owners; their histories must not be relabeled as the new baseline. No remote
deployment or retained application database was reset by this change.
