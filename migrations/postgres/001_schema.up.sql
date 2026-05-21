-- =============================================================================
-- OpenRails billing schema — consolidated from migrations 001-033.
--
-- This file is the structural state of the schema after all 33 historical
-- Postgres migrations have been applied. It is organized for readability:
--   * Topological order (referenced tables defined before referencing tables).
--   * Indexes, check constraints, and comments live next to their table.
--   * Backfills, repair logic, and renames from the original migrations are
--     omitted — they were one-time data fixes, not structural definitions.
-- =============================================================================

SET lock_timeout    = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- Extensions and schema
-- -----------------------------------------------------------------------------

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE SCHEMA IF NOT EXISTS billing;

-- -----------------------------------------------------------------------------
-- Enums
-- -----------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_type t
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'subscription_status' AND n.nspname = 'billing'
    ) THEN
        CREATE TYPE billing.subscription_status AS ENUM (
            'pending', 'active', 'expired', 'cancelled', 'failed', 'past_due'
        );
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_type t
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'processor_type' AND n.nspname = 'billing'
    ) THEN
        CREATE TYPE billing.processor_type AS ENUM (
            'paypal', 'solana', 'mobius', 'ccbill', 'stripe', 'admin', 'nmi', 'manual'
        );
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_type t
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'purchase_status' AND n.nspname = 'billing'
    ) THEN
        CREATE TYPE billing.purchase_status AS ENUM (
            'pending', 'completed', 'failed', 'refunded'
        );
    END IF;
END$$;

-- =============================================================================
-- TABLES (topological order)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.products — product catalog
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.products (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    slug               TEXT         NOT NULL UNIQUE,
    display_name       TEXT         NOT NULL,
    description        TEXT,
    entitlements_spec  JSONB,                                         -- map: entitlement_name -> duration_days (null/0 = indefinite)
    credits_spec       JSONB,                                         -- bundled promo credits (amount, expiry, cadence)
    tier_group         VARCHAR(100),                                  -- mutually-exclusive group; products in same group require upgrade/downgrade
    tier_rank          INT          NOT NULL DEFAULT 0,               -- higher = more premium
    is_active          BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_products_slug       ON billing.products(slug);
CREATE INDEX IF NOT EXISTS idx_products_is_active  ON billing.products(is_active);
CREATE INDEX IF NOT EXISTS idx_products_tier_group ON billing.products(tier_group) WHERE tier_group IS NOT NULL;

COMMENT ON TABLE  billing.products                   IS 'Product definitions that can be purchased or subscribed to';
COMMENT ON COLUMN billing.products.credits_spec      IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';
COMMENT ON COLUMN billing.products.tier_group        IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN billing.products.tier_rank         IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';

-- -----------------------------------------------------------------------------
-- billing.prices — pricing tiers for products
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.prices (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id          UUID         NOT NULL REFERENCES billing.products(id) ON DELETE RESTRICT,
    display_name        TEXT         NOT NULL,
    amount              BIGINT       NOT NULL,                        -- smallest currency unit (cents)
    currency            TEXT         NOT NULL,
    billing_cycle_days  INTEGER,                                      -- 30=monthly, 365=yearly, NULL=one-time
    processors          JSONB,                                        -- {"stripe": {...}, "ccbill": {...}}
    is_active           BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,

    CONSTRAINT unique_prices_product_amount_cycle
        UNIQUE (product_id, amount, currency, billing_cycle_days)
);

CREATE INDEX IF NOT EXISTS idx_prices_product_id  ON billing.prices(product_id);
CREATE INDEX IF NOT EXISTS idx_prices_processors  ON billing.prices USING GIN (processors);
CREATE INDEX IF NOT EXISTS idx_prices_is_active   ON billing.prices(is_active);

COMMENT ON TABLE billing.prices IS 'Pricing tiers for products with processor-specific identifiers';

-- -----------------------------------------------------------------------------
-- billing.payment_methods — stored vault entries per processor
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.payment_methods (
    id                      UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 TEXT          NOT NULL,                      -- AuthKit/OIDC subject
    processor               VARCHAR(50)   NOT NULL,                      -- 'mobius', 'ccbill', 'stripe', 'nmi', ...
    vault_id                VARCHAR(255)  NOT NULL,                      -- primary identifier in processor's system
    billing_id              VARCHAR(255),                                -- secondary identifier (e.g., subscription ID)
    initial_transaction_id  VARCHAR(255)  NOT NULL,                      -- transaction that created this vault
    last_four               VARCHAR(4),
    card_type               VARCHAR(50),
    expiry_date             VARCHAR(5),                                  -- 'MM/YY'
    failure_reason          TEXT,
    metadata                JSONB,
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS        idx_payment_methods_user_id              ON billing.payment_methods(user_id);
CREATE INDEX IF NOT EXISTS        idx_payment_methods_processor            ON billing.payment_methods(processor);
CREATE INDEX IF NOT EXISTS        idx_payment_methods_vault_id             ON billing.payment_methods(vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_methods_processor_vault_id   ON billing.payment_methods(processor, vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_user_vault            ON billing.payment_methods(user_id, vault_id);

COMMENT ON TABLE  billing.payment_methods           IS 'Generalized payment method table supporting multiple processors.';
COMMENT ON COLUMN billing.payment_methods.processor IS 'Payment processor type: nmi, ccbill, stripe, etc.';
COMMENT ON COLUMN billing.payment_methods.vault_id  IS 'Primary payment method identifier in the processor system';

-- -----------------------------------------------------------------------------
-- billing.subscriptions — core subscription records
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.subscriptions (
    id                          UUID                         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                     TEXT                         NOT NULL,                              -- AuthKit/OIDC subject
    price_id                    UUID                         REFERENCES billing.prices(id),
    product_id                  UUID                         NOT NULL REFERENCES billing.products(id),
    status                      billing.subscription_status  NOT NULL DEFAULT 'pending',

    -- Processor wiring
    processor                   TEXT                         NOT NULL DEFAULT 'ccbill',
    processor_subscription_id   TEXT                         NOT NULL DEFAULT '',
    user_email                  TEXT,
    payment_method_id           UUID                         REFERENCES billing.payment_methods(id) ON DELETE SET NULL,

    -- Billing period tracking
    current_period_starts_at    TIMESTAMPTZ,
    current_period_ends_at      TIMESTAMPTZ,
    started_at                  TIMESTAMPTZ                  NOT NULL DEFAULT current_timestamp,
    ended_at                    TIMESTAMPTZ,
    grace_ends_at               TIMESTAMPTZ,                                                        -- dunning grace window

    -- Scheduled tier changes (applied at renewal)
    scheduled_price_id          UUID                         REFERENCES billing.prices(id),

    -- Retry / dunning
    last_retry_at               TIMESTAMPTZ,
    retry_attempts              INTEGER                      DEFAULT 0,
    next_retry_at               TIMESTAMPTZ,

    -- Cancellation
    cancelled_at                TIMESTAMPTZ,
    cancel_type                 TEXT,                                                               -- 'user', 'admin', 'failed_payment', 'expired'
    cancel_feedback             TEXT,

    -- Frozen catalog snapshot at activation time
    entitlements_spec_snapshot  JSONB,
    credits_spec_snapshot       JSONB,

    -- Free-form
    gateway_response            JSONB,

    created_at                  TIMESTAMPTZ                  NOT NULL DEFAULT current_timestamp,
    updated_at                  TIMESTAMPTZ                  NOT NULL DEFAULT current_timestamp,

    -- Lifecycle invariants
    CONSTRAINT chk_cancelled_has_timestamp
        CHECK (status <> 'cancelled' OR cancelled_at IS NOT NULL),
    CONSTRAINT chk_cancelled_has_type
        CHECK (status <> 'cancelled' OR cancel_type IS NOT NULL),
    CONSTRAINT chk_valid_period
        CHECK (
            current_period_starts_at IS NULL
            OR current_period_ends_at IS NULL
            OR current_period_starts_at < current_period_ends_at
        ),
    CONSTRAINT chk_ended_not_before_cancelled
        CHECK (
            ended_at IS NULL
            OR cancelled_at IS NULL
            OR ended_at >= cancelled_at
        ),
    CONSTRAINT chk_past_due_has_period_end
        CHECK (status <> 'past_due' OR current_period_ends_at IS NOT NULL),
    CONSTRAINT chk_cancelled_no_retry_schedule
        CHECK (
            status <> 'cancelled'
            OR (next_retry_at IS NULL AND grace_ends_at IS NULL)
        )
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_user_id                   ON billing.subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_price_id                  ON billing.subscriptions(price_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_product_id                ON billing.subscriptions(product_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_user_product              ON billing.subscriptions(user_id, product_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status                    ON billing.subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor                 ON billing.subscriptions(processor);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor_subscription    ON billing.subscriptions(processor, processor_subscription_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_payment_method_id         ON billing.subscriptions(payment_method_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_next_retry_at             ON billing.subscriptions(next_retry_at)  WHERE next_retry_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_grace_ends_at             ON billing.subscriptions(grace_ends_at)  WHERE grace_ends_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_due_dunning               ON billing.subscriptions(next_retry_at, processor)
    WHERE status = 'past_due' AND next_retry_at IS NOT NULL;

-- Uniqueness constraints expressed as partial indexes
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_product_lifecycle_owner
    ON billing.subscriptions(user_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due');                           -- one lifecycle owner per (user, product)
CREATE UNIQUE INDEX IF NOT EXISTS uniq_subscriptions_processor_subscription_id_nonempty
    ON billing.subscriptions(processor, processor_subscription_id)
    WHERE processor_subscription_id <> '';

COMMENT ON TABLE  billing.subscriptions                    IS 'Core subscription records tracking user billing relationships';
COMMENT ON COLUMN billing.subscriptions.product_id         IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN billing.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';

-- -----------------------------------------------------------------------------
-- billing.entitlements — SCD2-style entitlement windows
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.entitlements (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT         NOT NULL,
    entitlement     TEXT         NOT NULL,
    start_at        TIMESTAMPTZ  NOT NULL,
    end_at          TIMESTAMPTZ,
    source_id       UUID         NOT NULL,                                       -- subscription_id / payment_id / admin_grant_id
    source_type     TEXT         NOT NULL,                                       -- 'subscription' | 'one_off' | 'admin' | 'grace'
    revoked_at      TIMESTAMPTZ,
    revoke_reason   TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    deleted_at      TIMESTAMPTZ,

    -- Generated helper for overlap exclusion
    period          tstzrange    GENERATED ALWAYS AS (
                        tstzrange(start_at, COALESCE(end_at, 'infinity'::timestamptz), '[)')
                    ) STORED,

    CONSTRAINT chk_valid_time_window
        CHECK (end_at IS NULL OR start_at < end_at),
    CONSTRAINT chk_revoke_fields_together
        CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT chk_entitlements_source_type
        CHECK (source_type IN ('subscription', 'one_off', 'admin', 'grace')),

    -- No overlapping live windows per (user, entitlement)
    CONSTRAINT entitlements_no_overlap
        EXCLUDE USING gist (
            user_id     WITH =,
            entitlement WITH =,
            period      WITH &&
        )
        WHERE (revoked_at IS NULL AND deleted_at IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_entitlements_active_window
    ON billing.entitlements(user_id, entitlement, start_at, end_at)
    WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_source
    ON billing.entitlements(source_type, source_id)
    WHERE source_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_live_by_id
    ON billing.entitlements(id)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_grace_by_subscription_live
    ON billing.entitlements(source_id, entitlement, start_at, end_at)
    WHERE source_type = 'grace' AND revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_subscription_source_live
    ON billing.entitlements(source_id, entitlement, end_at)
    WHERE source_type = 'subscription' AND revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_one_off_source_live
    ON billing.entitlements(source_id, entitlement)
    WHERE source_type = 'one_off' AND revoked_at IS NULL AND deleted_at IS NULL;

-- At most one open-ended live entitlement per (user, entitlement)
CREATE UNIQUE INDEX IF NOT EXISTS uniq_entitlements_active
    ON billing.entitlements(user_id, entitlement)
    WHERE revoked_at IS NULL AND end_at IS NULL;

-- -----------------------------------------------------------------------------
-- billing.payments — payment transactions (formerly "purchases")
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.payments (
    id                          UUID                     PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                     TEXT                     NOT NULL,
    price_id                    UUID                     NOT NULL REFERENCES billing.prices(id),
    processor                   billing.processor_type   NOT NULL,
    transaction_id              TEXT                     NOT NULL,
    amount                      BIGINT                   NOT NULL,                            -- charged (may be discounted)
    list_amount                 BIGINT                   NOT NULL,                            -- canonical price amount at purchase time
    currency                    TEXT                     NOT NULL DEFAULT 'usd',
    status                      billing.purchase_status  NOT NULL DEFAULT 'completed',
    subscription_id             UUID                     REFERENCES billing.subscriptions(id) ON DELETE SET NULL,
    refunded_payment_id         UUID                     REFERENCES billing.payments(id),

    -- Discount metadata
    discount_code               TEXT,
    discount_reason             TEXT,
    discount_metadata           JSONB,

    -- Frozen catalog snapshot at purchase time
    entitlements_spec_snapshot  JSONB,
    credits_spec_snapshot       JSONB,

    metadata                    JSONB,

    purchased_at                TIMESTAMPTZ              NOT NULL DEFAULT current_timestamp,
    created_at                  TIMESTAMPTZ              NOT NULL DEFAULT current_timestamp,

    CONSTRAINT payments_processor_transaction_unique UNIQUE (processor, transaction_id),
    CONSTRAINT chk_payment_not_future
        CHECK (purchased_at <= NOW() + INTERVAL '5 minutes')
);

CREATE INDEX IF NOT EXISTS idx_payments_user_id              ON billing.payments(user_id);
CREATE INDEX IF NOT EXISTS idx_payments_price_id             ON billing.payments(price_id);
CREATE INDEX IF NOT EXISTS idx_payments_processor            ON billing.payments(processor);
CREATE INDEX IF NOT EXISTS idx_payments_purchased_at         ON billing.payments(purchased_at);
CREATE INDEX IF NOT EXISTS idx_payments_subscription_id      ON billing.payments(subscription_id);
CREATE INDEX IF NOT EXISTS idx_payments_refunded_payment_id  ON billing.payments(refunded_payment_id);

COMMENT ON TABLE  billing.payments                 IS 'Records of all payment transactions (formerly purchases table)';
COMMENT ON COLUMN billing.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';

-- -----------------------------------------------------------------------------
-- billing.admin_grants — admin-initiated product grants (comps, contests, etc.)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.admin_grants (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT         NOT NULL,
    price_id        UUID         REFERENCES billing.prices(id),                  -- nullable: admin can grant entitlements without a backing price
    granted_by      TEXT         NOT NULL,
    reason          TEXT         NOT NULL,
    payment_id      UUID         REFERENCES billing.payments(id),
    duration_days   INTEGER,                                                     -- NULL=use Product spec, 0=indefinite, N=N days
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_admin_grants_user_id     ON billing.admin_grants(user_id);
CREATE INDEX IF NOT EXISTS idx_admin_grants_granted_by  ON billing.admin_grants(granted_by);
CREATE INDEX IF NOT EXISTS idx_admin_grants_payment_id  ON billing.admin_grants(payment_id) WHERE payment_id IS NOT NULL;

COMMENT ON TABLE  billing.admin_grants               IS 'Records admin-initiated product grants (comps, contest winners, manual payments, partnerships)';
COMMENT ON COLUMN billing.admin_grants.user_id       IS 'User receiving the grant';
COMMENT ON COLUMN billing.admin_grants.price_id      IS 'Price/Product being granted - entitlements derived from Product.EntitlementsSpec';
COMMENT ON COLUMN billing.admin_grants.granted_by    IS 'Admin user ID who made the grant';
COMMENT ON COLUMN billing.admin_grants.reason        IS 'Reason for grant: comp, contest_winner, refund_compensation, partnership, manual_payment, etc.';
COMMENT ON COLUMN billing.admin_grants.payment_id    IS 'Optional link to Payment record if money was received';
COMMENT ON COLUMN billing.admin_grants.duration_days IS 'Override entitlement duration (NULL=use Product spec, 0=indefinite, N=N days)';

-- -----------------------------------------------------------------------------
-- billing.notification_queue — user-facing billing notifications
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.notification_queue (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      TEXT         NOT NULL,
    event_type   TEXT         NOT NULL,                                          -- premium_started, payment_failed, ...
    data         JSONB        NOT NULL,
    seen         BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_notification_queue_user_id     ON billing.notification_queue(user_id);
CREATE INDEX IF NOT EXISTS idx_notification_queue_event_type  ON billing.notification_queue(event_type);
CREATE INDEX IF NOT EXISTS idx_notification_queue_seen        ON billing.notification_queue(seen);
CREATE INDEX IF NOT EXISTS idx_notification_queue_created_at  ON billing.notification_queue(created_at);

COMMENT ON TABLE billing.notification_queue IS 'Queue for user notifications related to billing and subscriptions';

-- -----------------------------------------------------------------------------
-- billing.processor_customers — per-processor customer id mapping
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.processor_customers (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      TEXT         NOT NULL,
    processor    TEXT         NOT NULL,
    customer_id  TEXT         NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,

    UNIQUE(user_id, processor),
    UNIQUE(processor, customer_id)
);

CREATE INDEX IF NOT EXISTS idx_processor_customers_user ON billing.processor_customers(user_id);

-- =============================================================================
-- CREDITS subsystem
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.credit_types — credit "currencies" (e.g. api_credits)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_types (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT         NOT NULL UNIQUE,
    display_name    TEXT         NOT NULL,
    unit            TEXT         NOT NULL DEFAULT 'usd',
    decimal_places  INT          NOT NULL DEFAULT 2,
    is_active       BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

-- -----------------------------------------------------------------------------
-- billing.credit_transactions — append-only ledger (with hold lifecycle)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_transactions (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            TEXT         NOT NULL,
    credit_type_id     UUID         NOT NULL REFERENCES billing.credit_types(id),
    amount             BIGINT       NOT NULL,                                    -- + deposit, - withdrawal
    balance_after      BIGINT,                                                   -- nullable for non-posting (holds)
    transaction_type   TEXT         NOT NULL,                                    -- deposit|withdrawal|expiry|refund|admin_adjust|hold|hold_capture
    source             TEXT         NOT NULL,                                    -- purchase|promo|usage|admin|expiry_job|refund|hold
    source_id          TEXT,                                                     -- idempotency key / request id (not necessarily a UUID)
    expires_at         TIMESTAMPTZ,
    description        TEXT,

    -- Hold lifecycle fields
    status             TEXT         NOT NULL DEFAULT 'posted',                   -- posted|active|captured|released|expired
    authorized_amount  BIGINT,
    captured_amount    BIGINT,

    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_credit_transactions_user_created
    ON billing.credit_transactions(user_id, credit_type_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_credit_holds_active_expires
    ON billing.credit_transactions(expires_at)
    WHERE transaction_type = 'hold' AND status = 'active';

-- Idempotency: (user, type, source, source_id) must be unique per transaction kind
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_hold_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'hold';
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_deposit_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'deposit' AND source_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'withdrawal' AND source_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- billing.credit_blocks — immutable blocks of credits for FIFO consumption
--   (formerly billing.credit_expiry_batches, renamed in migration 022)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_blocks (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                TEXT         NOT NULL,
    credit_type_id         UUID         NOT NULL REFERENCES billing.credit_types(id),
    original_amount        BIGINT       NOT NULL,
    remaining_amount       BIGINT       NOT NULL,
    expires_at             TIMESTAMPTZ,                                          -- nullable: NULL = permanent block
    source_transaction_id  UUID         REFERENCES billing.credit_transactions(id),
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_credit_blocks_user_expires
    ON billing.credit_blocks(user_id, credit_type_id, expires_at ASC);
CREATE INDEX IF NOT EXISTS idx_credit_blocks_user_expires_created
    ON billing.credit_blocks(user_id, credit_type_id, expires_at, created_at);

-- -----------------------------------------------------------------------------
-- billing.user_credit_balances — denormalized rollup for fast reads
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.user_credit_balances (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT         NOT NULL,
    credit_type_id  UUID         NOT NULL REFERENCES billing.credit_types(id),
    balance         BIGINT       NOT NULL DEFAULT 0,                             -- total available (rolls up credit_blocks.remaining_amount)
    held_balance    BIGINT       NOT NULL DEFAULT 0,                             -- reserved by active holds
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,

    UNIQUE(user_id, credit_type_id)
);

CREATE INDEX IF NOT EXISTS idx_user_credit_balances_user
    ON billing.user_credit_balances(user_id);

-- =============================================================================
-- CHECKOUT and REBILL flow tables
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.checkout_sessions — in-flight checkout state per processor
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.checkout_sessions (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            TEXT         NOT NULL,
    price_id           UUID         NOT NULL REFERENCES billing.prices(id),
    mode               TEXT         NOT NULL CHECK (mode IN ('one_off', 'subscription')),
    processor          TEXT         NOT NULL,
    status             TEXT         NOT NULL,
    amount             BIGINT       NOT NULL,
    currency           TEXT         NOT NULL DEFAULT 'usd',
    expires_at         TIMESTAMPTZ,
    reference          TEXT,
    transaction_id     TEXT,
    payment_id         UUID         REFERENCES billing.payments(id),
    subscription_id    UUID         REFERENCES billing.subscriptions(id),
    processor_fields   JSONB,
    processor_state    JSONB,
    metadata           JSONB,
    idempotency_key    TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE UNIQUE INDEX IF NOT EXISTS checkout_sessions_processor_transaction_id_idx
    ON billing.checkout_sessions(processor, transaction_id)
    WHERE transaction_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS checkout_sessions_processor_reference_idx
    ON billing.checkout_sessions(processor, reference)
    WHERE reference IS NOT NULL;
CREATE INDEX IF NOT EXISTS checkout_sessions_user_status_idx
    ON billing.checkout_sessions(user_id, status);
CREATE INDEX IF NOT EXISTS checkout_sessions_expires_at_idx
    ON billing.checkout_sessions(expires_at);

-- -----------------------------------------------------------------------------
-- billing.manual_rebill_attempts — bookkeeping for manual dunning retries
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.manual_rebill_attempts (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id  UUID         NOT NULL REFERENCES billing.subscriptions(id),
    period_end       TIMESTAMPTZ  NOT NULL,
    processor        TEXT         NOT NULL,
    order_reference  TEXT         NOT NULL,
    status           TEXT         NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'unknown')),
    transaction_id   TEXT,
    failure_reason   TEXT,
    claimed_until    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,

    CONSTRAINT uniq_manual_rebill_subscription_period UNIQUE (subscription_id, period_end),
    CONSTRAINT uniq_manual_rebill_processor_order     UNIQUE (processor, order_reference)
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_manual_rebill_processor_transaction
    ON billing.manual_rebill_attempts(processor, transaction_id)
    WHERE transaction_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_manual_rebill_attempts_status_claimed
    ON billing.manual_rebill_attempts(status, claimed_until);
