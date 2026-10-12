-- parent: 78 sha256:b640ca9e608da82e28973a3399249b4e32f67c1b36c53549d7b71edf09faf87a
-- Repair: none-needed New accepted schedules only; no historical reconstruction.
-- Accepted provider schedule terms are recorded at enrollment, before future
-- receipts arrive. They are not reconstructed from mutable subscription periods.
CREATE TABLE billing.provider_schedules (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    initial_order_id uuid NOT NULL,
    first_due_at timestamptz NOT NULL,
    cadence_hours integer NOT NULL CHECK (cadence_hours > 0),
    accepted_bill jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, subscription_id),
    FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id),
    FOREIGN KEY (merchant_id, initial_order_id) REFERENCES billing.orders(merchant_id, id),
    FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id)
);

CREATE TABLE billing.provider_cycle_receipts (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    transaction_id text NOT NULL CHECK (transaction_id <> ''),
    subscription_id uuid NOT NULL,
    due_at timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    currency text NOT NULL,
    occurred_at timestamptz NOT NULL,
    approved boolean NOT NULL,
    PRIMARY KEY (merchant_id, psp_id, transaction_id),
    FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.provider_schedules(merchant_id, subscription_id),
    FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id)
);

