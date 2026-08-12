-- or#914 item 5: host-side dormant-merchant sweeper warning ledger (ak#264
-- ruling 5 v3 — authkit has ZERO dormancy machinery; the host owns the
-- policy; th#1774 is the reference shape).
--
-- One row per merchant currently on dormancy notice. The row IS the warning
-- record: the sweep logs loudly and persists the notice here, and deletion —
-- DeletePermissionGroup with an EXPLICIT ReleaseSlug plus a directory-row
-- soft-delete — happens no sooner than the configured warning lead after
-- first_warned_at. The row is withdrawn when the merchant shows activity,
-- deleted when the sweep deletes the merchant, and cascades away if the
-- directory row is ever hard-purged.
--
-- Merchant-scoped like any other merchant_id-bearing table: every read/write
-- happens inside MerchantTx alongside the activity probe, so the standard
-- merchant_isolation policy applies (there is deliberately NO cross-merchant
-- notice read anywhere in the sweep).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.merchant_dormancy_notices (
    merchant_id uuid NOT NULL,
    slug text NOT NULL,
    first_warned_at timestamptz NOT NULL DEFAULT now(),
    last_warned_at timestamptz NOT NULL DEFAULT now(),
    warn_count bigint NOT NULL DEFAULT 1,
    CONSTRAINT merchant_dormancy_notices_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_dormancy_notices_merchant_fkey FOREIGN KEY (merchant_id)
        REFERENCES openrails.merchants(id) ON DELETE CASCADE
);

ALTER TABLE openrails.merchant_dormancy_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.merchant_dormancy_notices FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_dormancy_notices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE openrails.merchant_dormancy_notices TO openrails_app;

COMMENT ON TABLE openrails.merchant_dormancy_notices IS 'or#914 dormant-merchant sweeper warning ledger: never-used merchants currently on deletion notice. first_warned_at + the sweep''s warning lead gates deletion (DeleteGroup ReleaseSlug + directory soft-delete); the row is withdrawn on activity. Accessed only inside MerchantTx beside the activity probe.';
