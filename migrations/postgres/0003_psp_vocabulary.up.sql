-- PSP vocabulary hardcut: a merchant's account on a rail is a PSP (payment
-- service provider) — "mobius", "paykings" on rail nmi; stripe/ccbill/solana
-- are their own PSP names. One term everywhere: table, provenance columns,
-- the manifest key column, and the price link column.

ALTER TABLE openrails.rail_merchant_accounts RENAME TO psps;
ALTER TABLE openrails.psps RENAME COLUMN display_name TO key;
COMMENT ON TABLE openrails.psps IS 'Merchant PSP registry. A row is one merchant-owned payment-service-provider account on one rail.';
COMMENT ON COLUMN openrails.psps.key IS 'The PSP''s manifest key (e.g. mobius) — the vocabulary catalog psp_links and checkout speak.';

ALTER TABLE openrails.psps RENAME CONSTRAINT rail_merchant_accounts_environment_check TO psps_environment_check;
ALTER TABLE openrails.psps RENAME CONSTRAINT rail_merchant_accounts_nonempty TO psps_nonempty;
ALTER TABLE openrails.psps RENAME CONSTRAINT rail_merchant_accounts_pkey TO psps_pkey;
ALTER TABLE openrails.psps RENAME CONSTRAINT rail_merchant_accounts_merchant_fk TO psps_merchant_fk;
ALTER INDEX openrails.idx_rail_merchant_accounts_merchant RENAME TO idx_psps_merchant;
ALTER INDEX openrails.idx_rail_merchant_accounts_new_work RENAME TO idx_psps_new_work;
ALTER INDEX openrails.uq_rail_merchant_accounts_identity RENAME TO uq_psps_identity;

ALTER TABLE openrails.payment_methods RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.payment_methods.psp_id IS 'PSP that produced this vaulted payment method mirror row.';
ALTER TABLE openrails.payment_methods RENAME CONSTRAINT payment_methods_rail_merchant_account_fk TO payment_methods_psp_fk;
ALTER INDEX openrails.idx_payment_methods_rail_merchant_account RENAME TO idx_payment_methods_psp;
ALTER INDEX openrails.uq_payment_methods_rail_merchant_account_instrument RENAME TO uq_payment_methods_psp_instrument;

ALTER TABLE openrails.subscriptions RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.subscriptions.psp_id IS 'PSP that produced this remote subscription mirror row.';
ALTER TABLE openrails.subscriptions RENAME CONSTRAINT subscriptions_rail_merchant_account_fk TO subscriptions_psp_fk;
ALTER INDEX openrails.idx_subscriptions_rail_merchant_account RENAME TO idx_subscriptions_psp;
ALTER INDEX openrails.uq_subscriptions_rail_merchant_account_subscription RENAME TO uq_subscriptions_psp_subscription;

ALTER TABLE openrails.payments RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.payments.psp_id IS 'PSP that produced this payment/charge mirror row.';
ALTER TABLE openrails.payments RENAME CONSTRAINT payments_rail_merchant_account_fk TO payments_psp_fk;
ALTER INDEX openrails.idx_payments_rail_merchant_account RENAME TO idx_payments_psp;
ALTER INDEX openrails.uq_payments_rail_merchant_account_transaction RENAME TO uq_payments_psp_transaction;

ALTER TABLE openrails.checkout_sessions RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.checkout_sessions.psp_id IS 'PSP selected for this provider checkout/session.';
ALTER TABLE openrails.checkout_sessions RENAME CONSTRAINT checkout_sessions_rail_merchant_account_fk TO checkout_sessions_psp_fk;
ALTER INDEX openrails.idx_checkout_sessions_rail_merchant_account RENAME TO idx_checkout_sessions_psp;

ALTER TABLE openrails.invoice_payments RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.invoice_payments.psp_id IS 'PSP used for this invoice payment attempt or settled provider payment.';
ALTER TABLE openrails.invoice_payments RENAME CONSTRAINT invoice_payments_rail_merchant_account_fk TO invoice_payments_psp_fk;
ALTER INDEX openrails.idx_invoice_payments_rail_merchant_account RENAME TO idx_invoice_payments_psp;

ALTER TABLE openrails.rail_intents RENAME COLUMN rail_merchant_account_id TO psp_id;
COMMENT ON COLUMN openrails.rail_intents.psp_id IS 'PSP row the outbound intent was enqueued against. Mismatch with current credentials parks/defers execution.';
ALTER TABLE openrails.rail_intents RENAME CONSTRAINT rail_intents_rail_merchant_account_fk TO rail_intents_psp_fk;
ALTER INDEX openrails.idx_rail_intents_rail_merchant_account RENAME TO idx_rail_intents_psp;

ALTER TABLE openrails.rail_mutation_logs RENAME COLUMN rail_merchant_account_id TO psp_id;
ALTER TABLE openrails.rail_mutation_logs RENAME CONSTRAINT rail_mutation_logs_rail_merchant_account_fk TO rail_mutation_logs_psp_fk;
ALTER INDEX openrails.idx_rail_mutation_logs_rail_merchant_account RENAME TO idx_rail_mutation_logs_psp;

ALTER TABLE openrails.rail_refresh_watermarks RENAME COLUMN rail_merchant_account_id TO psp_id;
ALTER TABLE openrails.rail_refresh_watermarks RENAME COLUMN rail_merchant_account_key TO psp_key;
COMMENT ON COLUMN openrails.rail_refresh_watermarks.psp_id IS 'Current PSP row when resolvable; NULL is the compatibility/global lane for providers without a bound account identity.';
ALTER TABLE openrails.rail_refresh_watermarks RENAME CONSTRAINT rail_refresh_watermarks_account_nonzero TO rail_refresh_watermarks_psp_nonzero;
ALTER TABLE openrails.rail_refresh_watermarks RENAME CONSTRAINT rail_refresh_watermarks_rail_merchant_account_fk TO rail_refresh_watermarks_psp_fk;

-- The price provider-link column: entries key on the PSP key, rail recorded
-- inside each entry.
ALTER TABLE openrails.prices RENAME COLUMN rails TO psp_links;
ALTER INDEX openrails.idx_prices_rails RENAME TO idx_prices_psp_links;
COMMENT ON COLUMN openrails.prices.psp_links IS 'PSP link entries keyed by PSP key (e.g. mobius); each entry records its rail and the provider-side object ids (plan_id, price_id, ...).';
