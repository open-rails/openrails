-- Structural reverse of 0063. Rows the up-migration deleted as unattributable
-- are NOT resurrected — nothing recorded them, and a prelaunch dev database is
-- rebuilt, not un-deleted.

SET LOCAL statement_timeout = '120s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------- rail_customer_accounts (#704) ---

DROP INDEX openrails.idx_rail_customer_accounts_psp;
DROP INDEX openrails.uq_rail_customer_accounts_customer_psp;
DROP INDEX openrails.uq_rail_customer_accounts_psp_account;

ALTER TABLE openrails.rail_customer_accounts DROP CONSTRAINT rail_customer_accounts_psp_fk;
ALTER TABLE openrails.rail_customer_accounts DROP COLUMN psp_id;

CREATE UNIQUE INDEX uq_rail_customer_accounts_customer_rail
    ON openrails.rail_customer_accounts USING btree (merchant_id, customer_id, rail);
CREATE UNIQUE INDEX uq_rail_customer_accounts_merchant_rail_customer
    ON openrails.rail_customer_accounts USING btree (merchant_id, rail, account_id);

COMMENT ON TABLE openrails.rail_customer_accounts IS 'customer <-> rail customer-id mapping. Keyed per (merchant, customer, rail); rail_merchant_account_id provenance was dropped (#704) — no writer ever set it.';

-- ------------------------------------------------------------ psp indexes ---

DROP INDEX openrails.idx_subscriptions_psp;
CREATE INDEX idx_subscriptions_psp ON openrails.subscriptions USING btree (psp_id) WHERE (psp_id IS NOT NULL);
DROP INDEX openrails.idx_payment_methods_psp;
CREATE INDEX idx_payment_methods_psp ON openrails.payment_methods USING btree (psp_id) WHERE (psp_id IS NOT NULL);
DROP INDEX openrails.idx_checkout_sessions_psp;
CREATE INDEX idx_checkout_sessions_psp ON openrails.checkout_sessions USING btree (psp_id) WHERE (psp_id IS NOT NULL);
DROP INDEX openrails.idx_rail_intents_psp;
CREATE INDEX idx_rail_intents_psp ON openrails.rail_intents USING btree (psp_id) WHERE (psp_id IS NOT NULL);
DROP INDEX openrails.idx_rail_mutation_logs_psp;
CREATE INDEX idx_rail_mutation_logs_psp ON openrails.rail_mutation_logs USING btree (psp_id) WHERE (psp_id IS NOT NULL);

DROP INDEX openrails.uq_payment_methods_psp_instrument;
CREATE UNIQUE INDEX uq_payment_methods_psp_instrument
    ON openrails.payment_methods USING btree (merchant_id, psp_id, rail_customer_ref, rail_method_ref)
    WHERE (psp_id IS NOT NULL);
CREATE UNIQUE INDEX uq_payment_methods_customer_instrument_legacy
    ON openrails.payment_methods USING btree (merchant_id, customer_id, rail_customer_ref, rail_method_ref)
    WHERE (psp_id IS NULL);
CREATE UNIQUE INDEX uq_payment_methods_merchant_rail_instrument_legacy
    ON openrails.payment_methods USING btree (merchant_id, rail, rail_customer_ref, rail_method_ref)
    WHERE (psp_id IS NULL);

DROP INDEX openrails.uq_checkout_sessions_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_transaction
    ON openrails.checkout_sessions USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), transaction_id)
    WHERE (transaction_id IS NOT NULL AND deleted_at IS NULL);

DROP INDEX openrails.uq_checkout_sessions_merchant_psp_reference;
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_reference
    ON openrails.checkout_sessions USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), reference)
    WHERE (reference IS NOT NULL AND deleted_at IS NULL);

DROP INDEX openrails.uq_subscriptions_merchant_psp_subscription_id;
CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id
    ON openrails.subscriptions USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), rail_subscription_id)
    WHERE (rail_subscription_id <> ''::text AND deleted_at IS NULL);

DROP INDEX openrails.uq_payments_merchant_offrail_transaction;
DROP INDEX openrails.uq_payments_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction
    ON openrails.payments USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), transaction_id)
    WHERE (deleted_at IS NULL);

-- ---------------------------------------------------------- the columns ---

ALTER TABLE openrails.rail_mutation_logs DROP CONSTRAINT rail_mutation_logs_psp_fk;
ALTER TABLE openrails.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_psp_fk FOREIGN KEY (psp_id)
    REFERENCES openrails.psps(id) ON DELETE SET NULL;

ALTER TABLE openrails.invoice_payments DROP CONSTRAINT invoice_payments_psp_required_on_rail;
ALTER TABLE openrails.payments DROP CONSTRAINT payments_psp_required_on_rail;

ALTER TABLE openrails.rail_mutation_logs ALTER COLUMN psp_id DROP NOT NULL;
ALTER TABLE openrails.rail_intents ALTER COLUMN psp_id DROP NOT NULL;
ALTER TABLE openrails.checkout_sessions ALTER COLUMN psp_id DROP NOT NULL;
ALTER TABLE openrails.payment_methods ALTER COLUMN psp_id DROP NOT NULL;
ALTER TABLE openrails.subscriptions ALTER COLUMN psp_id DROP NOT NULL;

COMMENT ON COLUMN openrails.payments.psp_id IS 'PSP that produced this payment/charge mirror row.';
COMMENT ON COLUMN openrails.subscriptions.psp_id IS 'PSP that produced this remote subscription mirror row.';
COMMENT ON COLUMN openrails.payment_methods.psp_id IS 'PSP that produced this vaulted payment method mirror row.';
COMMENT ON COLUMN openrails.checkout_sessions.psp_id IS 'PSP selected for this provider checkout/session.';
COMMENT ON COLUMN openrails.invoice_payments.psp_id IS 'PSP used for this invoice payment attempt or settled provider payment.';
COMMENT ON COLUMN openrails.rail_intents.psp_id IS 'PSP row the outbound intent was enqueued against. Mismatch with current credentials parks/defers execution.';
COMMENT ON COLUMN openrails.rail_mutation_logs.psp_id IS NULL;
