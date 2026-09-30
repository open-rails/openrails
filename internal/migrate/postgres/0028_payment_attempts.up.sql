-- parent: 27 sha256:71d394c0d3eac5ed94b9868a7d6a04783f463864826cac340079e62ba5ee68fb
-- #1110: one row per authorization request that reached a PSP and got an
-- answer (approved, declined, or an error the PSP processed), classified at
-- write time by internal/billing/decline. Money stays in payments.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.payment_attempts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    kind text NOT NULL,
    owner text NOT NULL,
    card_entry text NOT NULL,
    source text NOT NULL,
    observed_via text NOT NULL,
    category text NOT NULL,
    reason text,
    action text,
    response_code text,
    response_text text,
    transaction_id text,
    avs_result text,
    cvv_result text,
    card_brand text,
    card_last4 text,
    token_type text,
    amount bigint NOT NULL,
    currency text,
    attempted_at timestamp with time zone NOT NULL,
    checkout_id uuid,
    checkout_target text,
    subscription_id uuid,
    payment_method_id uuid,
    payment_id uuid,
    rail_intent_id uuid,
    step text DEFAULT '' NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_attempts_pkey PRIMARY KEY (id),
    CONSTRAINT payment_attempts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT payment_attempts_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES openrails.customers(merchant_id, id),
    CONSTRAINT payment_attempts_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id),
    CONSTRAINT chk_payment_attempts_kind CHECK (kind IN ('verify', 'initial', 'upgrade', 'rebill', 'dunning_retry', 'customer_retry', 'invoice')),
    CONSTRAINT chk_payment_attempts_owner CHECK (owner IN ('engine', 'nmi_schedule', 'provider', 'none')),
    CONSTRAINT chk_payment_attempts_card_entry CHECK (card_entry IN ('new', 'saved')),
    CONSTRAINT chk_payment_attempts_source CHECK (source IN ('openrails', 'provider_schedule', 'external')),
    CONSTRAINT chk_payment_attempts_observed_via CHECK (observed_via IN ('response', 'webhook', 'pull')),
    CONSTRAINT chk_payment_attempts_category CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT chk_payment_attempts_outcome CHECK ((category = 'approved') = (reason IS NULL AND action IS NULL)),
    CONSTRAINT chk_payment_attempts_action CHECK (action IS NULL OR action IN ('retry', 'fix_payment_method', 'non_recoverable')),
    CONSTRAINT chk_payment_attempts_amount CHECK (amount >= 0),
    CONSTRAINT chk_payment_attempts_currency CHECK (currency ~ '^[A-Z0-9]{3,12}$' OR (currency IS NULL AND amount = 0)),
    CONSTRAINT chk_payment_attempts_text CHECK (length(response_text) <= 128),
    CONSTRAINT chk_payment_attempts_last4 CHECK (card_last4 ~ '^[0-9]{4}$'),
    CONSTRAINT chk_payment_attempts_token_type CHECK (token_type IN ('network_token', 'pan_via_proxy', 'psp_token')),
    CONSTRAINT chk_payment_attempts_checkout CHECK ((checkout_id IS NULL) = (checkout_target IS NULL))
);

COMMENT ON TABLE openrails.payment_attempts IS '#1110 one row per authorization answered by a PSP: the $0 card verification, sales, rebills and retries. Never the PAN or CVV. checkout_id groups one buyer''s attempts on one target (checkout_target: a price id or card_save) until the target is approved.';

CREATE UNIQUE INDEX uq_payment_attempts_transaction ON openrails.payment_attempts USING btree (merchant_id, psp_id, transaction_id) WHERE transaction_id IS NOT NULL;
CREATE UNIQUE INDEX uq_payment_attempts_operation_step ON openrails.payment_attempts USING btree (merchant_id, rail_intent_id, step) WHERE rail_intent_id IS NOT NULL;
CREATE INDEX idx_payment_attempts_time ON openrails.payment_attempts USING btree (merchant_id, attempted_at);
CREATE INDEX idx_payment_attempts_checkout ON openrails.payment_attempts USING btree (merchant_id, customer_id, checkout_target, attempted_at) WHERE checkout_target IS NOT NULL;
