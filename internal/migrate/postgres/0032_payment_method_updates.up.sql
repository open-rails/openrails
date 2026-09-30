-- parent: 31 sha256:8a5fb2ca30844f725ab50de4c599e96dbdc098c01a2a5ff7a1d0b6083c752768
-- #1115: one row per change to a stored card's standing: an account updater
-- (NMI's ACU, Basis Theory's) or the customer replaced it, or the updater
-- reported the account closed or the cardholder to be contacted.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.payment_method_updates (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    source text NOT NULL,
    kind text NOT NULL,
    event_ref text NOT NULL,
    at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_method_updates_pkey PRIMARY KEY (id),
    CONSTRAINT payment_method_updates_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT payment_method_updates_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES openrails.customers(merchant_id, id),
    CONSTRAINT payment_method_updates_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id),
    CONSTRAINT chk_payment_method_updates_source CHECK (source IN ('nmi_acu', 'bt_account_updater', 'customer')),
    CONSTRAINT chk_payment_method_updates_kind CHECK (kind IN ('updated', 'closed_account', 'contact_customer')),
    CONSTRAINT chk_payment_method_updates_event_ref CHECK (event_ref <> '')
);

COMMENT ON TABLE openrails.payment_method_updates IS '#1115 changes to a stored card''s standing, by source (nmi_acu, bt_account_updater, customer) and kind; event_ref makes a redelivered notice a no-op.';

CREATE UNIQUE INDEX uq_payment_method_updates_event ON openrails.payment_method_updates USING btree (merchant_id, source, event_ref, payment_method_id);
CREATE INDEX idx_payment_method_updates_method ON openrails.payment_method_updates USING btree (merchant_id, payment_method_id, at);
