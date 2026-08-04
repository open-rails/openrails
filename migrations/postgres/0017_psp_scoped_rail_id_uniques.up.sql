-- or#831 correction. 0012 collapsed the payments/subscriptions provider-id uniques
-- to (merchant_id, rail, <provider id>), dropping the PSP dimension. That is wrong:
-- a provider id is only unique WITHIN a gateway account, so two PSPs on one rail
-- (mobius + paykings on nmi — a supported state, see credentials.go "multiple active
-- provider accounts configured") can legitimately both issue id "12345". They are two
-- different subscriptions. 0012 made that unrepresentable and would fail the insert.
-- The invariant is asserted by
-- TestRailMerchantAccountScopedLocalStateDoesNotBlendCollidingProviderIDs.
--
-- The real defect 0012 aimed at survives and is still closed here: pre-0012 the two
-- partial indexes were DISJOINT on `psp_id IS NULL`, so one legacy unstamped row and
-- its PSP-stamped counterpart could both exist for the same transaction. COALESCE-ing
-- the nullable psp_id to the nil uuid gives ONE total index that both closes that hole
-- and keeps per-PSP separation. Same technique as rail_refresh_watermarks.psp_key.

DROP INDEX IF EXISTS openrails.uq_payments_merchant_rail_transaction;
DROP INDEX IF EXISTS openrails.uq_subscriptions_merchant_rail_subscription_id;

CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction
    ON openrails.payments USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        transaction_id
    );

CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id
    ON openrails.subscriptions USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        rail_subscription_id
    )
    WHERE (rail_subscription_id <> ''::text);

-- Audit for genuine duplicates (legacy row + stamped row for the same provider id).
-- Both must return zero rows before this migration can apply:
--   SELECT merchant_id, rail, transaction_id, count(*) FROM openrails.payments
--    GROUP BY 1,2,3,COALESCE(psp_id,'00000000-0000-0000-0000-000000000000'::uuid)
--   HAVING count(*) > 1;
