# Backup and recovery

For operators running their own OpenRails infrastructure.

Two things must be backed up, and **one of them is not in Postgres**. Restoring only the
database leaves you with a system that cannot charge anyone.

| What | Where it lives | Lose it and… |
|---|---|---|
| Application data | Postgres | everything |
| Merchant configuration | Vault (with `vault.kv_mount`), else the merchant manifest | rails cannot arm; Postgres holds no copy |

## The constraint that shapes everything

**Restoring the database does not undo what already happened at the payment provider.**

If OpenRails deleted an NMI customer vault, charged a card, or canceled a Stripe subscription,
a restore does not reverse any of it. A restore *creates* divergence between local state and
provider truth rather than removing it.

That is survivable, because provider truth is authoritative and OpenRails is built to reconcile
against it. After any restore, the recovery is **restore → re-converge**: the convergence engine
pulls provider rosters and repairs local state (see `operations.md`, "The Convergence Engine").
Plan for it explicitly; it is not automatic reassurance.

Money already moved is never un-moved by a restore. Reversal in this system is a compensating
entry — a refund, a `credit_reinstate` transfer — never a deletion.

## Postgres point-in-time recovery

Postgres supports true PITR: a base backup plus continuously archived WAL lets you restore to
any moment in between. Turn it on.

```
# postgresql.conf
wal_level = replica
archive_mode = on
archive_command = '... copy %p to durable off-host storage ...'
```

Take periodic base backups (`pg_basebackup`) and retain WAL segments for at least your recovery
window. Managed providers (RDS, Cloud SQL, Neon, Supabase) expose this directly — confirm the
retention window rather than assuming a default.

To restore to a point in time, provision from the base backup and set a recovery target:

```
# postgresql.conf on the restored instance
restore_command = '... fetch archived WAL segment %f to %p ...'
recovery_target_time = '2026-07-28 14:00:00+00'
```

**PITR is whole-cluster.** It restores every merchant together, and everything after the target
time is gone. That makes it the right tool for genuine disaster — hardware loss, a destructive
migration, a compromised instance — and the wrong tool for "merchant X's book was damaged."
Scoped recovery is tracked separately; see the private `specs/application-rollback.md`.

## Vault

With `vault.kv_mount`, every merchant's configuration, credentials included, lives in Vault and
Postgres holds no copy. Back Vault up on its own schedule with its own procedure (`vault.md`),
and keep `credential_fingerprint_key` with it. Restoring Vault to an older point serves older
configuration: credentials rotated since may no longer be accepted by the provider. A PSP
identity Postgres records but Vault no longer describes is archived: it takes no new work and its
obligations drain.

## What must never be rolled back

Some tables are append-only: their triggers refuse UPDATE and DELETE, and ledger counters move
solely through the transfer insert trigger.

- `ledger_transfers`, `ledger_accounts` — the double-entry ledger
- `grants` — the grant log entitlements are derived from
- `subscription_status_transitions`
- `provider_intents` — the record of every external write attempted
- webhook dedup records — rolling these back invites reprocessing events as new

Selectively reverting any of these corrupts the audit trail rather than repairing it. A restore
takes them wholesale or not at all. Entitlements and credit balances are **recomputed** from the
grant log by convergence, not restored directly — which is why the grant log must survive intact.

## Restore procedure

1. **Stop and drain the source writers, and disable their automatic restart.** A restored
   copy must become the sole authoritative database. Replicas may share that database;
   independently writable copies are not a supported active-active deployment.
2. Restore Postgres to the target time; confirm Vault (or the merchant manifest) is available.
3. Start the restored instance. A restore into another cluster, database or schema is a copy
   of the book: every provider write stays readonly until `openrails book arm --by NAME`,
   which you run only once the source is stopped for good. A point-in-time restore of the same
   cluster keeps its identity. For established NMI and Stripe accounts, stale completed
   coverage holds new collection and destructive work while provider reads recover qualified
   financial receipts. Failed or incomplete reads and unresolved financial conflicts keep
   that work held. See [provider recovery](provider-recovery.md) for coverage and limitations.
4. If configured with `provider_write_mode: full`, eligible work resumes automatically after
   verified catch-up. The separate destructive-action and ownership controls still apply.
   To inspect before permitting any provider writes, start with `readonly`: reads and verified
   local recovery continue, but this explicit setting never changes automatically. Change it
   to `full` only when ready to resume.
5. Review unresolved findings and application-only state that the provider cannot reconstruct.
   Ignoring a finding does not prove that its financial conflict has been settled.

CCBill and Solana retain their existing recovery controls and are outside the automatic NMI/Stripe
freshness gate. Use `readonly` and their established review procedure for those books. A fresh
coverage timestamp cannot detect every arbitrary clone, provider indexing delay, or account
created after the backup. Never restart the old source copy after the destination begins writing.

## What is not a backup

**The merchant purge inventory.** `TakePurgeInventory` (was `Export`) writes row
counts and secret *names* to `billing.maintenance_runs`. It copies no
data — no customer, subscription, payment, entitlement or catalog row, and no
secret value. It exists so an operator sees the blast radius before confirming a
purge, and it can restore nothing. Its own recorded manifest says so
(`"is_backup": false`), and lists what it omits.

If you purge a merchant, PITR above is your only way back.

**A provider pull.** Reconciling against NMI/Stripe/CCBill/Solana repairs local
state from provider truth, but providers hold only what they were told: no
entitlements, no grant history, no ledger. A pull is recovery of the mirror, not
of the system.

**A `--prune` rollback.** `openrails undo-run --run <id>` reverses one
prune run's soft deletes. That is scoped undo of one operation, not a restore
point — and per `operations.md` the complete recovery is `rollback → pull →
converge`.

## Verify your backups

An untested backup is a hypothesis. Periodically restore into a scratch instance and check that
migrations are at the expected version, that a merchant's PSPs arm from Vault, and that a
provider pull produces a sane findings set. The failure you want to discover in a drill is a
Vault backup that does not match the database — not at 3am.
