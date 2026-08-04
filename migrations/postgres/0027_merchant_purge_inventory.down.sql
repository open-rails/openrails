ALTER INDEX openrails.idx_merchant_purge_inventories_merchant RENAME TO idx_merchant_exports_merchant;
ALTER INDEX openrails.merchant_purge_inventories_pkey RENAME TO merchant_exports_pkey;
ALTER TABLE openrails.merchant_purge_inventories
    RENAME CONSTRAINT merchant_purge_inventories_merchant_fk TO merchant_exports_merchant_fk;
ALTER TABLE openrails.merchant_purge_inventories
    RENAME CONSTRAINT merchant_purge_inventories_status_check TO merchant_exports_status_check;
ALTER TABLE openrails.merchant_purge_inventories RENAME COLUMN manifest TO row_counts;
ALTER TABLE openrails.merchant_purge_inventories RENAME TO merchant_exports;

COMMENT ON TABLE openrails.merchant_exports IS 'Merchant logical-export bookkeeping (issue #225). Merchant deletion is gated on a completed export row (export-before-delete). Merchant-owned and RLS protected.';
