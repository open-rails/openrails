# Backup and recovery

For operators running their own OpenRails infrastructure.

Three things must be backed up, and **two of them are not in Postgres**. Restoring only the
database leaves you with an unreadable system.

| What | Where it lives | Lose it and… |
|---|---|---|
| Application data | Postgres | everything |
| `ENCRYPTION_MASTER_KEY` | your secret manager / env | **every DB-stored merchant secret is unrecoverable ciphertext** |
| Merchant secrets | Vault, when `secret_backend=vault` | rails cannot arm; Postgres holds no copy |

## The constraint that shapes everything

**Restoring the database does not undo what already happened at the payment provider.**

If OpenRails deleted an NMI customer vault, charged a card, or cancelled a Stripe subscription,
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

## The encryption master key

When `secret_backend` is the DB-encrypted store, merchant secrets are encrypted at rest with a
key derived from `ENCRYPTION_MASTER_KEY`. That key is **not in the database**. A database backup
without it restores ciphertext nobody can read — NMI security keys, Stripe secret keys, CCBill
DataLink passwords, webhook signing secrets, Solana keys, all unrecoverable.

Back the key up independently of the database, in a different trust domain, and verify you can
retrieve it *before* you need it. Rotating it requires re-encrypting stored secrets; do not
rotate as part of an incident.

Outside development, OpenRails refuses to boot the DB-encrypted store without this key set —
that refusal is deliberate (see `invariants.md`, FC-3).

## Vault

With `secret_backend=vault`, merchant secrets live in Vault and Postgres holds no copy. Back
Vault up on its own schedule with its own procedure (`vault.md`), and make sure its retention at
least matches the database's — a database restored to a point Vault can no longer serve is a
system that cannot arm any rail.

## What must never be rolled back

Some tables are append-only by role privilege: the application role holds only `SELECT, INSERT`,
and counters move solely through a `SECURITY DEFINER` trigger.

- `ledger_transfers`, `ledger_accounts` — the double-entry ledger
- `grants` — the grant log entitlements are derived from
- `subscription_status_transitions`
- `rail_intents` — the record of every external write attempted
- webhook dedup records — rolling these back invites reprocessing events as new

Selectively reverting any of these corrupts the audit trail rather than repairing it. A restore
takes them wholesale or not at all. Entitlements and credit balances are **recomputed** from the
grant log by convergence, not restored directly — which is why the grant log must survive intact.

## Restore procedure

1. **Stop the workers.** Set `provider_write_mode: readonly` before bringing the restored
   instance up, so nothing writes to a provider from stale state. Confirm the destructive-action
   kill switch is off (`operations.md`).
2. Restore Postgres to the target time; confirm `ENCRYPTION_MASTER_KEY` and Vault are available.
3. Bring OpenRails up **still in `readonly`**. Let the provider pull run and read the findings —
   this is your divergence report, produced without touching anything.
4. Review before enforcing. The first enforce pass after a restore is the highest-risk moment in
   the system's life; the first-enforce gate (`operations.md`) exists for exactly this.
5. Return to `full` once the findings look like what you expect.

Do not skip step 3. A restored database plus an immediate enforcing convergence pass is how a
recovery becomes a second incident.

## What is not a backup

**The merchant purge inventory.** `TakePurgeInventory` (was `Export`) writes row
counts and secret *names* to `openrails.merchant_purge_inventories`. It copies no
data — no customer, subscription, payment, entitlement or catalog row, and no
secret value. It exists so an operator sees the blast radius before confirming a
purge, and it can restore nothing. Its own recorded manifest says so
(`"is_backup": false`), and lists what it omits.

If you purge a merchant, PITR above is your only way back.

**A provider pull.** Reconciling against NMI/Stripe/CCBill/Solana repairs local
state from provider truth, but providers hold only what they were told: no
entitlements, no grant history, no ledger. A pull is recovery of the mirror, not
of the system.

**A `--prune` rollback.** `openrails prune rollback --run <id>` reverses one
prune run's soft deletes. That is scoped undo of one operation, not a restore
point — and per `operations.md` the complete recovery is `rollback → pull →
converge`.

## Verify your backups

An untested backup is a hypothesis. Periodically restore into a scratch instance and check that
migrations are at the expected version, that a merchant's secrets actually decrypt, and that a
provider pull produces a sane findings set. The failure you want to discover in a drill is a
missing master key — not at 3am.
