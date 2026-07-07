-- #773: subscription repricing — moving existing subscribers to a different
-- price at their next renewal on/after an effective date. Grandfathering
-- (archive-only) already exists by construction (#210); this is the explicit,
-- scheduled/cancelable "move them" primitive.
--
-- reprice_batches is the header row for a bulk operation (reprice_all_prior_
-- versions(key, effective_date) or a single reprice()), giving callers one
-- handle to inspect progress across every affected subscription.
CREATE TABLE openrails.reprice_batches (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    price_key text,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    subscriptions_matched integer DEFAULT 0 NOT NULL,
    subscriptions_scheduled integer DEFAULT 0 NOT NULL,
    subscriptions_skipped integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.reprice_batches FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.reprice_batches IS '#773: header row for one bulk reprice operation (reprice_all_prior_versions or a single ad-hoc reprice); subscription_reprices rows carry reprice_batch_id back to it for per-subscription progress.';

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_to_price_fk FOREIGN KEY (to_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

CREATE INDEX idx_reprice_batches_merchant ON openrails.reprice_batches USING btree (merchant_id, created_at DESC);

ALTER TABLE openrails.reprice_batches ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.reprice_batches USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reprice_batches TO openrails_app;

-- One row per subscription-level scheduled/applied/canceled price move.
-- At most one status='scheduled' row per subscription (uq below) so the
-- renewal boundary always has an unambiguous due reprice to pick up.
CREATE TABLE openrails.subscription_reprices (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    from_price_id uuid NOT NULL,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    status text DEFAULT 'scheduled'::text NOT NULL,
    reprice_batch_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    applied_at timestamp with time zone,
    canceled_at timestamp with time zone,
    CONSTRAINT subscription_reprices_status_chk CHECK ((status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text]))),
    CONSTRAINT subscription_reprices_applied_has_timestamp CHECK ((status <> 'applied'::text) OR (applied_at IS NOT NULL)),
    CONSTRAINT subscription_reprices_canceled_has_timestamp CHECK ((status <> 'canceled'::text) OR (canceled_at IS NOT NULL))
);

ALTER TABLE ONLY openrails.subscription_reprices FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.subscription_reprices IS '#773: a scheduled, applied, or canceled price move for one subscription. Applied at the subscription''s first renewal on/after effective_at (v1: no proration/mid-cycle).';

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_from_price_fk FOREIGN KEY (from_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_to_price_fk FOREIGN KEY (to_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_batch_fk FOREIGN KEY (reprice_batch_id) REFERENCES openrails.reprice_batches(id) ON DELETE SET NULL;

-- The due-reprice-at-the-renewal-boundary invariant: at most one scheduled
-- reprice per subscription at a time.
CREATE UNIQUE INDEX uq_subscription_reprices_one_scheduled ON openrails.subscription_reprices USING btree (subscription_id) WHERE (status = 'scheduled'::text);

CREATE INDEX idx_subscription_reprices_due ON openrails.subscription_reprices USING btree (effective_at) WHERE (status = 'scheduled'::text);

CREATE INDEX idx_subscription_reprices_merchant ON openrails.subscription_reprices USING btree (merchant_id, created_at DESC);

CREATE INDEX idx_subscription_reprices_subscription ON openrails.subscription_reprices USING btree (subscription_id);

CREATE INDEX idx_subscription_reprices_batch ON openrails.subscription_reprices USING btree (reprice_batch_id) WHERE (reprice_batch_id IS NOT NULL);

ALTER TABLE openrails.subscription_reprices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.subscription_reprices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.subscription_reprices TO openrails_app;
