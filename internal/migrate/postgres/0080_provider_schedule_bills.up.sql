-- Repair: none-needed New accepted schedules only; no historical reconstruction.
-- Accepted provider schedule terms are recorded at enrollment, before future
-- receipts arrive. They are not reconstructed from mutable subscription periods.
CREATE TABLE billing.provider_schedules (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    initial_order_id uuid,
    accepted_version bigint NOT NULL CHECK (accepted_version > 0),
    source_reference text NOT NULL CHECK (octet_length(source_reference) BETWEEN 1 AND 512),
    provider_reference text NOT NULL CHECK (octet_length(provider_reference) BETWEEN 1 AND 512),
    first_due_at timestamptz NOT NULL,
    cadence_hours integer NOT NULL CHECK (cadence_hours >= 0),
    cadence_months integer NOT NULL CHECK (cadence_months BETWEEN 0 AND 1200),
    CHECK ((cadence_hours > 0) <> (cadence_months > 0)),
    accepted_bill jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, subscription_id, source_reference),
    UNIQUE (merchant_id, subscription_id, accepted_version),
    FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id),
    FOREIGN KEY (merchant_id, initial_order_id) REFERENCES billing.orders(merchant_id, id),
    FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id)
);
COMMENT ON TABLE billing.provider_schedules IS 'Immutable accepted provider schedule versions: first due instant, cadence and the accepted bill a provider-run renewal must match. Retention: permanent, never pruned.';

CREATE TABLE billing.provider_cycle_receipts (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    transaction_id text NOT NULL CHECK (transaction_id <> ''),
    subscription_id uuid NOT NULL,
    schedule_source_reference text NOT NULL,
    due_at timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    currency text NOT NULL,
    occurred_at timestamptz NOT NULL,
    approved boolean NOT NULL,
    PRIMARY KEY (merchant_id, psp_id, transaction_id),
    FOREIGN KEY (merchant_id, subscription_id, schedule_source_reference) REFERENCES billing.provider_schedules(merchant_id, subscription_id, source_reference),
    FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id)
);
COMMENT ON TABLE billing.provider_cycle_receipts IS 'Verified provider transactions bound once to one due cycle of an accepted schedule version. Retention: permanent, never pruned.';

-- Resolve the latest approved cycle of one immutable schedule version without
-- scanning every transaction on the merchant's provider account.
CREATE INDEX provider_cycle_receipts_settled_schedule_idx
    ON billing.provider_cycle_receipts (merchant_id, subscription_id, schedule_source_reference, due_at DESC)
    WHERE approved;
