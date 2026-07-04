-- #735: PG home for the one-time legacy dunning import (doujins #387:
-- users_logs rebill attempts + mobius_schedulers events). Append-only
-- forensics evidence for the reconcile dunning report; never read for
-- money/entitlement decisions. Go-forward dunning evidence lives in
-- payments (status='failed') and, later, subscription status transitions.

CREATE TABLE openrails.imported_dunning_history (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid,
    customer_id uuid,
    event_type text NOT NULL,
    rail text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    source text NOT NULL,
    detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.imported_dunning_history FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.imported_dunning_history IS 'Append-only imported legacy dunning history (#735; doujins #387 import target). Display/forensics evidence only.';

COMMENT ON COLUMN openrails.imported_dunning_history.source IS 'Legacy origin of the imported row, e.g. doujins_users_logs, mobius_schedulers.';

COMMENT ON COLUMN openrails.imported_dunning_history.detail IS 'Verbatim normalized legacy payload. Correlation keys the reconcile history source extracts when present: rail_subscription_id, rail_transaction_id, status, amount_micros.';

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE SET NULL;

CREATE INDEX idx_imported_dunning_history_merchant_occurred ON openrails.imported_dunning_history USING btree (merchant_id, occurred_at);

ALTER TABLE openrails.imported_dunning_history ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.imported_dunning_history USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.imported_dunning_history TO openrails_app;
