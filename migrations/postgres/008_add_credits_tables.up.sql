SET lock_timeout = '10s';
SET statement_timeout = '300s';

-- Add credits spec to products (bundled promo credits for subscriptions)
ALTER TABLE billing.products
    ADD COLUMN IF NOT EXISTS credits_spec JSONB;

COMMENT ON COLUMN billing.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';

-- Credit types (currencies)
CREATE TABLE IF NOT EXISTS billing.credit_types (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT UNIQUE NOT NULL,          -- e.g. api_credits
    display_name TEXT NOT NULL,         -- e.g. API Credits
    unit TEXT NOT NULL DEFAULT 'usd',   -- display unit
    decimal_places INT NOT NULL DEFAULT 2,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- Per-user balances (denormalized for fast reads)
CREATE TABLE IF NOT EXISTS billing.user_credit_balances (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credit_type_id UUID NOT NULL REFERENCES billing.credit_types(id),
    balance BIGINT NOT NULL DEFAULT 0,          -- total (permanent + expiring - held)
    held_balance BIGINT NOT NULL DEFAULT 0,     -- reserved for holds
    permanent_balance BIGINT NOT NULL DEFAULT 0,
    expiring_balance BIGINT NOT NULL DEFAULT 0,
    earliest_expiry TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    UNIQUE(user_id, credit_type_id)
);

CREATE INDEX IF NOT EXISTS idx_user_credit_balances_user ON billing.user_credit_balances(user_id);

-- Ledger (append-only)
CREATE TABLE IF NOT EXISTS billing.credit_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credit_type_id UUID NOT NULL REFERENCES billing.credit_types(id),
    amount BIGINT NOT NULL,          -- positive = deposit, negative = withdrawal
    balance_after BIGINT NOT NULL,   -- balance after this transaction
    transaction_type TEXT NOT NULL,  -- deposit|withdrawal|expiry|refund|admin_adjust|hold_capture
    source TEXT NOT NULL,            -- purchase|promo|usage|admin|expiry_job|refund|hold
    source_id UUID,
    expires_at TIMESTAMPTZ,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_credit_transactions_user_created ON billing.credit_transactions(user_id, credit_type_id, created_at DESC);

-- Expiry batches for FIFO consumption
CREATE TABLE IF NOT EXISTS billing.credit_expiry_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credit_type_id UUID NOT NULL REFERENCES billing.credit_types(id),
    original_amount BIGINT NOT NULL,
    remaining_amount BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    source_transaction_id UUID REFERENCES billing.credit_transactions(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_credit_expiry_batches_user_expires ON billing.credit_expiry_batches(user_id, credit_type_id, expires_at ASC);

-- Holds for long-running jobs
CREATE TABLE IF NOT EXISTS billing.credit_holds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credit_type_id UUID NOT NULL REFERENCES billing.credit_types(id),
    amount BIGINT NOT NULL,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active', -- active|captured|released|expired
    expires_at TIMESTAMPTZ NOT NULL,
    captured_amount BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_credit_holds_user_status ON billing.credit_holds(user_id, credit_type_id, status);
CREATE INDEX IF NOT EXISTS idx_credit_holds_expires ON billing.credit_holds(expires_at);

-- Seed API credits type if missing
INSERT INTO billing.credit_types (name, display_name, unit, decimal_places)
SELECT 'api_credits', 'API Credits', 'usd', 2
WHERE NOT EXISTS (SELECT 1 FROM billing.credit_types WHERE name='api_credits');
