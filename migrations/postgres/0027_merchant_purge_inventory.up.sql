-- or#858 hazard 2: `merchant_exports` is not an export, and merchant deletion
-- was gated on it as though it were a backup.
--
-- The table never held one byte of merchant data. Its `row_counts` column holds
-- per-table COUNTS plus the NAMES of the merchant's secrets. An operator who
-- read the table name, ran the export, saw a `completed` row and then ran the
-- purge had no restore point of any kind — the only real one is Postgres PITR
-- plus the ENCRYPTION_MASTER_KEY plus Vault (docs/backup-and-recovery.md).
--
-- So the name goes. It is a PURGE INVENTORY: the manifest of what a purge is
-- about to destroy, taken so the operator sees the blast radius before typing
-- the confirmation — never so the data can come back.
--
-- Greenfield rename, no alias: any caller still naming merchant_exports fails
-- loudly rather than silently writing to a table that no longer means what it
-- says.

SET statement_timeout = '60s';
SET lock_timeout = '10s';

ALTER TABLE openrails.merchant_exports RENAME TO merchant_purge_inventories;

-- `row_counts` undersold itself in the other direction: the column holds the
-- whole manifest (counts, secret names, and the explicit not-captured list).
ALTER TABLE openrails.merchant_purge_inventories RENAME COLUMN row_counts TO manifest;

ALTER TABLE openrails.merchant_purge_inventories
    RENAME CONSTRAINT merchant_exports_status_check TO merchant_purge_inventories_status_check;
ALTER TABLE openrails.merchant_purge_inventories
    RENAME CONSTRAINT merchant_exports_merchant_fk TO merchant_purge_inventories_merchant_fk;
ALTER INDEX openrails.merchant_exports_pkey RENAME TO merchant_purge_inventories_pkey;
ALTER INDEX openrails.idx_merchant_exports_merchant RENAME TO idx_merchant_purge_inventories_merchant;

COMMENT ON TABLE openrails.merchant_purge_inventories IS
    'or#858: the manifest of what a merchant purge is ABOUT TO DESTROY — per-table row counts, merchant secret NAMES, and the explicit list of what is not captured. It is NOT a backup and restores nothing; the only restore path is Postgres PITR (docs/backup-and-recovery.md). Merchant deletion is gated on a matching inventory so the operator has seen the blast radius, not so the data can come back. Was merchant_exports (#225), a name that promised a restore point that never existed.';
COMMENT ON COLUMN openrails.merchant_purge_inventories.manifest IS
    'The inventory manifest: row_counts, total_rows, secret_names (never values), not_captured, is_backup=false. total_rows must still match at purge time — a stale inventory does not authorise a purge.';
