ALTER TABLE openrails.provider_accounts
    RENAME TO payment_provider_accounts;

ALTER TABLE openrails.payment_provider_accounts
    RENAME COLUMN provider_type TO rail;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_pkey TO payment_provider_accounts_pkey;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_environment_check TO payment_provider_accounts_environment_check;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_nonempty TO payment_provider_accounts_nonempty;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_routing_check TO payment_provider_accounts_routing_check;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_status_check TO payment_provider_accounts_status_check;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_owner_check TO payment_provider_accounts_owner_check;

ALTER TABLE openrails.payment_provider_accounts
    RENAME CONSTRAINT provider_accounts_merchant_fk TO payment_provider_accounts_merchant_fk;

ALTER INDEX openrails.idx_provider_accounts_merchant
    RENAME TO idx_payment_provider_accounts_merchant;

ALTER INDEX openrails.uq_provider_accounts_identity
    RENAME TO uq_payment_provider_accounts_identity;

ALTER INDEX openrails.uq_provider_accounts_enabled_primary
    RENAME TO uq_payment_provider_accounts_enabled_primary;

COMMENT ON TABLE openrails.payment_provider_accounts IS
    'Merchant payment-provider account registry. A row is one merchant-owned account on one rail.';

COMMENT ON COLUMN openrails.payment_provider_accounts.rail IS
    'Payment rail/backend such as stripe, nmi, ccbill, solana, or a future rail.';
