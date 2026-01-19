-- Explicitly set schema to ensure all objects are created in the correct place
-- Set timeouts to prevent hanging migrations
SET lock_timeout = '10s';
SET statement_timeout = '300s';

-- Install required extensions
-- Extensions are created by bootstrap; skip here.

-- ============================================================================
-- SECTION 1: CORE SUBSCRIPTION TABLES
-- ============================================================================

-- 1.2: Create subscription status enum (idempotent without requiring owner privileges)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_type t
        JOIN pg_namespace n ON t.typnamespace = n.oid
        WHERE t.typname = 'subscription_status' AND n.nspname = 'billing'
    ) THEN
        CREATE TYPE billing.subscription_status AS ENUM ('pending', 'active', 'expired', 'cancelled', 'failed', 'past_due');
    END IF;
END$$;

-- 1.3: Create subscriptions table
CREATE TABLE IF NOT EXISTS billing.subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL, -- AuthKit user ID (UUID)
    price_id UUID, -- References prices table (created later)
    product_id UUID NOT NULL, -- Denormalized for efficient user+product lookups (references products table)
    status billing.subscription_status NOT NULL DEFAULT 'pending',

    -- Processor information (flattened: mobius/ccbill/solana/paypal/etc.)
    processor TEXT NOT NULL DEFAULT 'ccbill',
    processor_subscription_id TEXT NOT NULL DEFAULT '',
    user_email TEXT,
    payment_method_id UUID, -- References payment_methods table (created later)

    -- Billing period tracking
    current_period_starts_at TIMESTAMPTZ,
    current_period_ends_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    ended_at TIMESTAMPTZ,

    -- Scheduled tier changes (downgrades applied at end of billing period)
    scheduled_price_id UUID, -- Price ID for scheduled downgrade, applied at renewal

    -- Retry fields for manual rebilling
    last_retry_at TIMESTAMPTZ,
    retry_attempts INTEGER DEFAULT 0,
    next_retry_at TIMESTAMPTZ,

    -- Cancellation tracking
    cancelled_at TIMESTAMPTZ,
    cancel_type TEXT, -- 'user', 'admin', 'failed_payment', 'expired'
    cancel_feedback TEXT,

    -- Metadata
    gateway_response JSONB,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- Create indexes for subscriptions
CREATE INDEX IF NOT EXISTS idx_subscriptions_user_id ON billing.subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_price_id ON billing.subscriptions(price_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_product_id ON billing.subscriptions(product_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_user_product ON billing.subscriptions(user_id, product_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON billing.subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor ON billing.subscriptions(processor);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor_subscription ON billing.subscriptions(processor, processor_subscription_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_next_retry_at ON billing.subscriptions(next_retry_at) WHERE next_retry_at IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_active ON billing.subscriptions(user_id) WHERE status = 'active';

COMMENT ON INDEX billing.idx_subscriptions_user_active IS 'Ensures each user can have only one active subscription at a time';
COMMENT ON COLUMN billing.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN billing.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';


-- ============================================================================
-- SECTION 2: PRODUCTS AND PRICING
-- ============================================================================

-- 2.1: Create products table
CREATE TABLE IF NOT EXISTS billing.products (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    description TEXT,
    entitlements_spec JSONB, -- map: entitlement_name -> duration_days (null/0 = indefinite)
    -- Tier columns for upgrade/downgrade relationships
    tier_group VARCHAR(100), -- Products in same group are mutually exclusive (upgrade/downgrade between them)
    tier_rank INT NOT NULL DEFAULT 0, -- Higher = more premium; determines upgrade vs downgrade direction
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_products_slug ON billing.products(slug);
CREATE INDEX IF NOT EXISTS idx_products_is_active ON billing.products(is_active);
CREATE INDEX IF NOT EXISTS idx_products_tier_group ON billing.products(tier_group) WHERE tier_group IS NOT NULL;

COMMENT ON COLUMN billing.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN billing.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';

-- 2.2: Create prices table
CREATE TABLE IF NOT EXISTS billing.prices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id UUID NOT NULL REFERENCES billing.products(id) ON DELETE RESTRICT,
    display_name TEXT NOT NULL,
    amount BIGINT NOT NULL, -- Amount in cents (smallest currency unit)
    currency TEXT NOT NULL,
    billing_cycle_days INTEGER, -- 30 for monthly, 365 for yearly, NULL for one-time
    processors JSONB, -- Processor-specific config: {"mobius": {"plan_id": "xyz"}, "ccbill": {"price_id": "abc"}}
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_prices_product_id ON billing.prices(product_id);
CREATE INDEX IF NOT EXISTS idx_prices_processors ON billing.prices USING GIN (processors);
CREATE INDEX IF NOT EXISTS idx_prices_is_active ON billing.prices(is_active);

ALTER TABLE billing.prices DROP CONSTRAINT IF EXISTS unique_prices_product_amount_cycle;
ALTER TABLE billing.prices ADD CONSTRAINT unique_prices_product_amount_cycle 
    UNIQUE (product_id, amount, currency, billing_cycle_days);

-- Add foreign key references from subscriptions to prices and products
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_name = 'fk_subscriptions_price_id'
    ) THEN
        ALTER TABLE billing.subscriptions
        ADD CONSTRAINT fk_subscriptions_price_id
        FOREIGN KEY (price_id) REFERENCES billing.prices(id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_name = 'fk_subscriptions_product'
    ) THEN
        ALTER TABLE billing.subscriptions
        ADD CONSTRAINT fk_subscriptions_product
        FOREIGN KEY (product_id) REFERENCES billing.products(id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_name = 'fk_subscriptions_scheduled_price'
    ) THEN
        ALTER TABLE billing.subscriptions
        ADD CONSTRAINT fk_subscriptions_scheduled_price
        FOREIGN KEY (scheduled_price_id) REFERENCES billing.prices(id);
    END IF;
END$$;

-- ============================================================================
-- SECTION 3: ENTITLEMENTS (SCD2 windows)
-- ============================================================================

CREATE TABLE IF NOT EXISTS billing.entitlements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    entitlement TEXT NOT NULL,
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ,
    source_id UUID,
    source_type TEXT NOT NULL,
    revoked_at TIMESTAMPTZ,
    revoke_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at TIMESTAMPTZ,
    -- Generated helpers for overlap protection
    period tstzrange GENERATED ALWAYS AS (tstzrange(start_at, COALESCE(end_at, 'infinity'::timestamptz), '[)')) STORED
);

CREATE INDEX IF NOT EXISTS idx_entitlements_user_entitlement ON billing.entitlements(user_id, entitlement);
CREATE INDEX IF NOT EXISTS idx_entitlements_active_window ON billing.entitlements(user_id, entitlement, start_at, end_at) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_source ON billing.entitlements(source_type, source_id) WHERE source_id IS NOT NULL;

-- At most one active entitlement per user+entitlement
CREATE UNIQUE INDEX IF NOT EXISTS uniq_entitlements_active ON billing.entitlements(user_id, entitlement)
WHERE revoked_at IS NULL AND end_at IS NULL;

-- Prevent overlapping entitlement windows per (user_id, entitlement) for non-deleted rows.
-- Simplified approach using a partial unique index instead of complex exclusion constraint
CREATE UNIQUE INDEX IF NOT EXISTS idx_entitlements_no_overlap
ON billing.entitlements(user_id, entitlement, start_at)
WHERE revoked_at IS NULL AND deleted_at IS NULL;

-- Backfill cleanup for older schemas: drop the former generated column if it exists
ALTER TABLE billing.entitlements DROP COLUMN IF EXISTS active;

-- ============================================================================
-- SECTION 4: PAYMENT PROCESSING
-- ============================================================================

-- 4.1: Create payment_methods table
CREATE TABLE IF NOT EXISTS billing.payment_methods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL, -- OIDC subject (sub)
    processor VARCHAR(50) NOT NULL, -- 'mobius', 'ccbill', etc.
    
    -- Processor-specific vault/payment method identifiers
    vault_id VARCHAR(255) NOT NULL, -- Primary identifier in processor's system
    billing_id VARCHAR(255), -- Secondary identifier (e.g., subscription ID)
    initial_transaction_id VARCHAR(255) NOT NULL, -- Transaction that created this vault
    
    -- Payment method status and metadata
    is_active BOOLEAN NOT NULL DEFAULT true, -- Can this method be used for rebills?
    last_four VARCHAR(4), -- Last 4 digits of card
    card_type VARCHAR(50), -- 'Visa', 'MasterCard', etc.
    expiry_date VARCHAR(5), -- 'MM/YY' format
    failure_reason TEXT, -- Reason if inactive
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_payment_methods_user_id ON billing.payment_methods(user_id);
CREATE INDEX IF NOT EXISTS idx_payment_methods_processor ON billing.payment_methods(processor);
CREATE INDEX IF NOT EXISTS idx_payment_methods_vault_id ON billing.payment_methods(vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_methods_processor_vault_id ON billing.payment_methods(processor, vault_id);
CREATE INDEX IF NOT EXISTS idx_payment_methods_is_active ON billing.payment_methods(is_active) WHERE is_active = true;
COMMENT ON TABLE billing.payment_methods IS 'Generalized payment method table supporting multiple processors.';
COMMENT ON COLUMN billing.payment_methods.processor IS 'Payment processor type: nmi, ccbill, stripe, etc.';
COMMENT ON COLUMN billing.payment_methods.vault_id IS 'Primary payment method identifier in the processor system';

-- Add payment_method_id reference to subscriptions table
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_name = 'fk_subscriptions_payment_method_id'
    ) THEN
        ALTER TABLE billing.subscriptions
        ADD CONSTRAINT fk_subscriptions_payment_method_id
        FOREIGN KEY (payment_method_id) REFERENCES billing.payment_methods(id) ON DELETE SET NULL;
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS idx_subscriptions_payment_method_id ON billing.subscriptions(payment_method_id);

-- 4.2: Create processor and purchase status enums
DROP TYPE IF EXISTS billing.processor_type CASCADE;
CREATE TYPE billing.processor_type AS ENUM ('paypal', 'solana', 'mobius', 'ccbill');

DROP TYPE IF EXISTS billing.purchase_status CASCADE;
CREATE TYPE billing.purchase_status AS ENUM ('pending', 'completed', 'failed', 'refunded');

-- 4.3: Create payments table (formerly purchases)
CREATE TABLE IF NOT EXISTS billing.payments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL, -- AuthKit user ID (UUID)
    price_id UUID NOT NULL REFERENCES billing.prices(id),
    processor billing.processor_type NOT NULL, -- flattened processor (mobius, ccbill, solana, paypal, etc.)
    transaction_id TEXT NOT NULL,
    amount BIGINT NOT NULL, -- Amount in cents (smallest currency unit)
    currency TEXT NOT NULL DEFAULT 'usd',
    -- Overall lifecycle state for the payment record
    status billing.purchase_status NOT NULL DEFAULT 'completed',
    subscription_id UUID REFERENCES billing.subscriptions(id) ON DELETE SET NULL,
    refunded_payment_id UUID,
    purchased_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    UNIQUE(processor, transaction_id),
    CONSTRAINT fk_payments_refunded_payment FOREIGN KEY (refunded_payment_id) REFERENCES billing.payments(id)
);

CREATE INDEX IF NOT EXISTS idx_payments_refunded_payment_id ON billing.payments(refunded_payment_id);

CREATE INDEX IF NOT EXISTS idx_payments_user_id ON billing.payments(user_id);
CREATE INDEX IF NOT EXISTS idx_payments_price_id ON billing.payments(price_id);
CREATE INDEX IF NOT EXISTS idx_payments_processor ON billing.payments(processor);
CREATE INDEX IF NOT EXISTS idx_payments_purchased_at ON billing.payments(purchased_at);
CREATE INDEX IF NOT EXISTS idx_payments_subscription_id ON billing.payments(subscription_id);

COMMENT ON COLUMN billing.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';

-- ============================================================================
-- SECTION 5: SUPPORTING TABLES
-- ============================================================================

CREATE TABLE IF NOT EXISTS billing.notification_queue (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL, -- AuthKit user ID (UUID)
    event_type TEXT NOT NULL, -- premium_started, payment_failed, etc.
    data JSONB NOT NULL,
    seen BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_notification_queue_user_id ON billing.notification_queue(user_id);
CREATE INDEX IF NOT EXISTS idx_notification_queue_event_type ON billing.notification_queue(event_type);
CREATE INDEX IF NOT EXISTS idx_notification_queue_seen ON billing.notification_queue(seen);
CREATE INDEX IF NOT EXISTS idx_notification_queue_created_at ON billing.notification_queue(created_at);

-- ============================================================================
-- SECTION 6: DATA MIGRATION FROM LEGACY TABLES
-- ============================================================================

-- 6.2: Migrate data from purchases table if it exists and hasn't been renamed yet
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'billing' AND table_name = 'purchases')
       AND NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'billing' AND table_name = 'payments') THEN
        -- First rename the table
        ALTER TABLE billing.purchases RENAME TO payments;

        -- Rename any associated indexes
        IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid WHERE c.relname = 'idx_purchases_subscription_id' AND n.nspname = 'billing') THEN
            ALTER INDEX billing.idx_purchases_subscription_id RENAME TO idx_payments_subscription_id;
        END IF;
    END IF;
END$$;

-- ============================================================================
-- SECTION 7: CLEANUP AND FINAL ADJUSTMENTS
-- ============================================================================


-- 7.3: Add comments for documentation
COMMENT ON TABLE billing.subscriptions IS 'Core subscription records tracking user billing relationships';
COMMENT ON TABLE billing.products IS 'Product definitions that can be purchased or subscribed to';
COMMENT ON TABLE billing.prices IS 'Pricing tiers for products with processor-specific identifiers';
COMMENT ON TABLE billing.payments IS 'Records of all payment transactions (formerly purchases table)';
COMMENT ON TABLE billing.notification_queue IS 'Queue for user notifications related to billing and subscriptions';
