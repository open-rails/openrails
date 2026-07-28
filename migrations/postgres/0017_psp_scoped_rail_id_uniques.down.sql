DROP INDEX IF EXISTS openrails.uq_payments_merchant_psp_transaction;
DROP INDEX IF EXISTS openrails.uq_subscriptions_merchant_psp_subscription_id;

CREATE UNIQUE INDEX uq_payments_merchant_rail_transaction
    ON openrails.payments USING btree (merchant_id, rail, transaction_id);

CREATE UNIQUE INDEX uq_subscriptions_merchant_rail_subscription_id
    ON openrails.subscriptions USING btree (merchant_id, rail, rail_subscription_id)
    WHERE (rail_subscription_id <> ''::text);
