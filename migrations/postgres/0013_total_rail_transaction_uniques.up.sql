-- #831: the (merchant, rail, transaction_id) uniques on payments and
-- subscriptions were split into two PARTIAL indexes on `psp_id IS NULL`.
-- Disjoint predicates cannot exclude each other, so the SAME provider
-- transaction was representable TWICE — once as a legacy NULL-PSP row, once
-- PSP-attributed. Double-counted revenue and double fulfilment.
--
-- The PSP dimension is dropped rather than COALESCEd to the nil UUID: keeping it
-- (as rail_refresh_watermarks does) would leave NULL and attributed as distinct
-- keys, which IS the bug. A provider transaction id is unique within a merchant
-- and rail regardless of which PSP account produced it — that is its identity.
-- Making it total also makes SnapshotPaymentCards provably bounded (#846).
--
-- These CREATE statements fail loudly if duplicates already exist; audit first:
--   SELECT merchant_id, rail, transaction_id, count(*) FROM openrails.payments
--    GROUP BY 1,2,3 HAVING count(*) > 1;

SET lock_timeout = '5s';
SET statement_timeout = '5min';

DROP INDEX openrails.uq_payments_merchant_rail_transaction_legacy;
DROP INDEX openrails.uq_payments_psp_transaction;

CREATE UNIQUE INDEX uq_payments_merchant_rail_transaction
    ON openrails.payments USING btree (merchant_id, rail, transaction_id);

DROP INDEX openrails.uq_subscriptions_merchant_rail_subscription_id_legacy;
DROP INDEX openrails.uq_subscriptions_psp_subscription;

-- '' is "no remote subscription yet" and is many-valued by design; excluding it
-- is a single predicate, not a disjoint split.
CREATE UNIQUE INDEX uq_subscriptions_merchant_rail_subscription_id
    ON openrails.subscriptions USING btree (merchant_id, rail, rail_subscription_id)
    WHERE (rail_subscription_id <> ''::text);
