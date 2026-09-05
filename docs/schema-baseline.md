# Final pre-launch schema baseline

The final baseline contains the effective OpenRails schema after the identity,
retirement and custom-credit changes. AuthKit and River retain their own migration
histories. Fold application migrations only after those changes are merged.

A baseline rewrite is a fresh-database operation. Do not clear a migration ledger
on an existing schema to make it appear current. The standalone and embedded
orphan-migration fences continue to refuse incompatible retained databases.

Acceptance compares a database built by the complete current migration chain with
an independently created database built by the consolidated baseline. Compare
schema, constraints, indexes, functions, row security, privileges and required
initial state. Repeat the fresh installation and exercise the existing orphan-ledger
refusal tests. Use task-owned databases; retain shared development and E2E data.

The completion record will identify source revisions, the captured migration chain,
schema parity results and consumer validation. A source/test result does not assert
that retained deployment databases have been recreated.
