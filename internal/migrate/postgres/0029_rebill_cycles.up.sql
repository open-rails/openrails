-- parent: 28 sha256:7f11fc7a95de07c7fa6433c8702551ee3b2e1e14141e79a2e6f8397a70789cf8
-- #1111: one row per expected rebill: a subscription's paid period came due
-- at due_at. A cycle's first attempt is kind rebill (whoever sent it); later
-- ones are dunning or customer retries. Outcomes are derived from its attempts
-- and the subscription, never copied here.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.rebill_cycles (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    owner text NOT NULL,
    due_at timestamp with time zone NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rebill_cycles_pkey PRIMARY KEY (id),
    CONSTRAINT rebill_cycles_merchant_id_key UNIQUE (merchant_id, id),
    CONSTRAINT rebill_cycles_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT rebill_cycles_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES openrails.customers(merchant_id, id),
    CONSTRAINT rebill_cycles_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id),
    CONSTRAINT chk_rebill_cycles_owner CHECK (owner IN ('engine', 'nmi_schedule', 'provider')),
    CONSTRAINT chk_rebill_cycles_amount CHECK (amount >= 0),
    CONSTRAINT chk_rebill_cycles_currency CHECK (currency ~ '^[A-Z0-9]{3,12}$')
);

COMMENT ON TABLE openrails.rebill_cycles IS '#1111 one expected rebill per (subscription, due_at): the moment its paid period came due. Its attempts are payment_attempts.cycle_id.';

CREATE UNIQUE INDEX uq_rebill_cycles_due ON openrails.rebill_cycles USING btree (merchant_id, subscription_id, due_at);
CREATE INDEX idx_rebill_cycles_time ON openrails.rebill_cycles USING btree (merchant_id, due_at);

ALTER TABLE openrails.payment_attempts ADD COLUMN cycle_id uuid;
ALTER TABLE openrails.payment_attempts
    ADD CONSTRAINT payment_attempts_cycle_fk FOREIGN KEY (merchant_id, cycle_id) REFERENCES openrails.rebill_cycles(merchant_id, id) NOT VALID;
ALTER TABLE openrails.payment_attempts
    ADD CONSTRAINT chk_payment_attempts_cycle CHECK ((kind IN ('rebill', 'dunning_retry', 'customer_retry')) = (cycle_id IS NOT NULL)) NOT VALID;
CREATE INDEX idx_payment_attempts_cycle ON openrails.payment_attempts USING btree (merchant_id, cycle_id) WHERE cycle_id IS NOT NULL;
