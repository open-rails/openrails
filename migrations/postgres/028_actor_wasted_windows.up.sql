--
-- Operator-configurable FLAT per-ACTOR wasted-spend windows (#492).
--
-- The per-PAYER wasted-spend budget is tier-graduated (tier_policies.bad_spend,
-- #488). The per-ACTOR budget is FLAT (an account can mint unlimited invokers, so
-- it is a fixed backstop rather than tier-graduated). Until now OpenRails
-- hardcoded that flat budget (service.DefaultActorWastedWindows, $5/15m + $20/5h)
-- and had NO client setter, so a host's declared per-invoker budget was silently
-- ignored. This table lets the operator configure it.
--
-- windows is a JSONB array of {key, window_seconds, limit_micros}: at most
-- limit_micros of host-reported wasted $ per window; a later admit denies when an
-- actor is over. DefaultActorWastedWindows() stays the FALLBACK when no row is
-- stored.
--
-- SCOPE: customer_id NULL = the tenant-wide default (the common case — one flat
-- backstop for the whole tenant). A non-NULL customer_id is a per-customer
-- override taking precedence for that customer, mirroring tier_schedules'
-- "= $2 OR IS NULL" effective resolution so the store stays generic.
--

CREATE TABLE IF NOT EXISTS openrails.actor_wasted_windows (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL DEFAULT COALESCE((NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid),
    customer_id uuid,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    windows_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT actor_wasted_windows_pkey PRIMARY KEY (id)
);

-- One config per (tenant, scope): a per-customer override and the tenant-wide
-- default (customer_id IS NULL) coexist. Partial unique indexes because NULL is
-- not equal to NULL in a multi-column UNIQUE.
CREATE UNIQUE INDEX uq_actor_wasted_windows_merchant_default ON openrails.actor_wasted_windows (merchant_id)
    WHERE (customer_id IS NULL);
CREATE UNIQUE INDEX uq_actor_wasted_windows_customer ON openrails.actor_wasted_windows (merchant_id, customer_id)
    WHERE (customer_id IS NOT NULL);

ALTER TABLE ONLY openrails.actor_wasted_windows
    ADD CONSTRAINT actor_wasted_windows_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

COMMENT ON TABLE openrails.actor_wasted_windows IS 'Operator-configurable FLAT per-actor wasted-spend windows (#492): windows[{key,window_seconds,limit_micros}] declared once per tenant (or per-customer override). Falls back to service.DefaultActorWastedWindows() when no row is stored.';
COMMENT ON COLUMN openrails.actor_wasted_windows.customer_id IS 'NULL = tenant-wide default; non-NULL = per-customer override taking precedence for that customer.';
COMMENT ON COLUMN openrails.actor_wasted_windows.windows IS 'JSONB array of {key, window_seconds, limit_micros}: at most limit_micros of wasted $ per window; admit denies when an actor is over.';

ALTER TABLE openrails.actor_wasted_windows ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.actor_wasted_windows FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.actor_wasted_windows USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.actor_wasted_windows TO openrails_app;
