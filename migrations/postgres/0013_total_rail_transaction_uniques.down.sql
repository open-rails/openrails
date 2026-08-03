DROP INDEX openrails.uq_subscriptions_merchant_rail_subscription_id;
DROP INDEX openrails.uq_payments_merchant_rail_transaction;

CREATE UNIQUE INDEX uq_payments_merchant_rail_transaction_legacy
    ON openrails.payments USING btree (merchant_id, rail, transaction_id) WHERE (psp_id IS NULL);
CREATE UNIQUE INDEX uq_payments_psp_transaction
    ON openrails.payments USING btree (merchant_id, psp_id, transaction_id) WHERE (psp_id IS NOT NULL);

CREATE UNIQUE INDEX uq_subscriptions_merchant_rail_subscription_id_legacy
    ON openrails.subscriptions USING btree (merchant_id, rail, rail_subscription_id)
    WHERE ((psp_id IS NULL) AND (rail_subscription_id <> ''::text));
CREATE UNIQUE INDEX uq_subscriptions_psp_subscription
    ON openrails.subscriptions USING btree (merchant_id, psp_id, rail_subscription_id)
    WHERE ((psp_id IS NOT NULL) AND (rail_subscription_id <> ''::text));
