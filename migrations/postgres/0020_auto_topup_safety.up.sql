SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

-- Durable safety reservations outlive generic provider-intent pruning. The
-- credit ledger remains the sole accounting authority; these rows count charge
-- episodes and preserve exact provider receipts for local finalization.
ALTER TABLE openrails.money_settings
    ADD COLUMN auto_topup_failures bigint NOT NULL DEFAULT 0 CHECK (auto_topup_failures >= 0);

CREATE TABLE openrails.auto_topup_episodes (
    intent_id uuid PRIMARY KEY,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    CONSTRAINT auto_topup_episodes_currency_shape CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    reserved_at timestamptz NOT NULL,
    amount_native bigint NOT NULL CHECK (amount_native > 0),
    receipt jsonb,
    finalized_at timestamptz,
    FOREIGN KEY (merchant_id, customer_id, currency)
        REFERENCES openrails.money_settings (merchant_id, customer_id, currency) ON DELETE CASCADE
);
CREATE INDEX auto_topup_episodes_account_time
    ON openrails.auto_topup_episodes (merchant_id, customer_id, currency, reserved_at);
CREATE UNIQUE INDEX auto_topup_episodes_one_pending
    ON openrails.auto_topup_episodes (merchant_id, customer_id, currency)
    WHERE finalized_at IS NULL;
ALTER TABLE openrails.auto_topup_episodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE openrails.auto_topup_episodes FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.auto_topup_episodes
    USING (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON openrails.auto_topup_episodes TO openrails_app;
