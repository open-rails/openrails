-- or#892 (1 of 2): make the idempotency coordinate STRUCTURAL, so once-only
-- stops resting on the order Go took its locks in.
--
-- After or#894 every ledger leg carries (operation, source, source_id), but the
-- schema still allowed source/source_id to be NULL and only two PARTIAL unique
-- indexes covered the coordinate — credit_spend and owed_accrual. Every other
-- transfer type (deposit, owed_payment, credit_expire, credit_revoke,
-- credit_reinstate) was once-only by CONVENTION: a check-then-insert in Go
-- behind lockBalance. A new write path that forgets the lock compiles, passes
-- single-threaded tests, and double-posts only under concurrency.
--
-- MEASURED, not asserted: with idx_ledger_transfers_operation_once dropped, the
-- same deposit coordinate inserted twice leaves 2 rows and 10,000 micros where
-- 5,000 moved. With it, the second insert is refused by the database.
--
-- ONE unique index over ALL transfer types replaces the two partials, so the
-- DATABASE refuses a duplicate instead of the code path remembering to look.
-- That is what lets every durable money write funnel through one
-- INSERT ... ON CONFLICT DO NOTHING RETURNING seam (ledger.ApplyIdempotent).
--
-- grant_id is in the key because a spend fans out one credit_spend transfer PER
-- FIFO lot drawn: the lot is part of the physical row identity, and a
-- coordinate-only index would break every multi-lot spend. NULLS NOT DISTINCT
-- (PG15+) is required — grant_id is NULL on the owed/payment legs and
-- customer_id is NULL on system-only transfers, and under default NULLS
-- DISTINCT those rows would escape the index entirely, which is exactly the
-- silent hole this migration exists to close.
--
-- idx_ledger_transfers_lot_once is deliberately KEPT: it enforces a different
-- fact (a lot is deposited/expired/revoked at most once, whatever operation
-- coordinate asks) and the new index does not subsume it.
--
-- BACKFILL: on any database built from migrations this is a NO-OP — 0001
-- creates the table empty and every insert since or#894 supplies the full
-- coordinate. It exists for long-lived DEV databases carrying rows written
-- before or#894, which hold operation = '' (0034 added the column with that
-- default) and may hold NULL source/source_id. Those rows get an explicit,
-- greppable 'legacy' sentinel rather than being left to make the constraints
-- unvalidatable: a NOT VALID constraint nobody can ever validate is a lie in
-- the schema. source_id takes the row's own id so legacy rows stay mutually
-- distinct and cannot collide under the new index.
--
-- The constraints land NOT VALID here and are VALIDATEd in 0043, which is also
-- where source/source_id become NOT NULL. That split is not ceremony: inside
-- this single-transaction migrator the validating scan holds ACCESS EXCLUSIVE
-- to COMMIT, so the two-step only reduces lock time when the halves are in
-- SEPARATE files — the shape internal/db/queries/EXEMPTIONS.md requires of new
-- migrations instead of an inline squawk-ignore.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

UPDATE openrails.ledger_transfers
   SET operation = 'legacy'
 WHERE operation = '';

UPDATE openrails.ledger_transfers
   SET source = 'legacy'
 WHERE source IS NULL OR source = '';

UPDATE openrails.ledger_transfers
   SET source_id = 'legacy:' || id::text
 WHERE source_id IS NULL OR source_id = '';

-- Written as `IS NOT NULL` on purpose: a VALIDATEd CHECK of exactly this shape
-- is what lets 0043's SET NOT NULL skip its own full scan (PG12+).
ALTER TABLE openrails.ledger_transfers
    ADD CONSTRAINT chk_ledger_transfers_source_present
    CHECK (source IS NOT NULL AND source_id IS NOT NULL) NOT VALID;

ALTER TABLE openrails.ledger_transfers
    ADD CONSTRAINT chk_ledger_transfers_coordinate_not_blank
    CHECK (operation <> '' AND source <> '' AND source_id <> '') NOT VALID;

DROP INDEX openrails.idx_ledger_transfers_credit_spend_once;
DROP INDEX openrails.idx_ledger_transfers_owed_accrual_once;

CREATE UNIQUE INDEX idx_ledger_transfers_operation_once
    ON openrails.ledger_transfers
       (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, grant_id)
    NULLS NOT DISTINCT;

COMMENT ON INDEX openrails.idx_ledger_transfers_operation_once IS
    'or#892: the structural once-only key for EVERY transfer type. ledger.ApplyIdempotent inserts ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. grant_id is part of the identity (one debit per operation per FIFO lot); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';
