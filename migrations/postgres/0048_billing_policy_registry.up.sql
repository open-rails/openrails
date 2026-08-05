-- or#897 PR 2: the merchant billing-policy registry replaces payer_spend_limits.
--
-- payer_spend_limits was already a policy registry in everything but name: a
-- JSONB policy body resolved per-customer-else-merchant-default, keyed by trust
-- tier. What it could not express is the thing the two seed businesses actually
-- differ on — WHICH QUANTITY is capped. An API business caps OUTSTANDING owed
-- ($200 line, $155 unpaid ⇒ $45 left); a cloud business caps NEW spend per
-- window ($2k/month) and lets prior debt drive delinquency instead. One table
-- with one implicit meaning cannot hold both, so the policy grows a `kind` and
-- the binding becomes a NAME reference the merchant can re-point at runtime.
--
-- HARD CUT (prelaunch, no aliases): payer_spend_limits is dropped, not shadowed.
-- Its trust-tier rung survives as the bindings table's `tier` column, and its
-- window list survives verbatim inside a window_spend_cap policy body.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.billing_policies (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE openrails.billing_policies IS 'or#897: the merchant''s named billing policies. The policy body declares WHICH quantity is capped (kind=outstanding_cap | window_spend_cap | accrual_rate_cap) and the limit. Merchants bind names to customers/tiers via billing_policy_bindings; OpenRails enforces, the merchant decides who gets which.';

COMMENT ON COLUMN openrails.billing_policies.policy IS 'JSONB policy body: kind, the kind''s limit (outstanding_cap_amount micros / spend_windows), bad_spend_windows (#497 wasted-spend grace) and policy_currency. Validated by ONE normalizer shared by the manifest loader and the config API.';

-- A named UNIQUE constraint (not a bare index): the bindings FK below references
-- it, so a binding can never name a policy that does not exist.
ALTER TABLE ONLY openrails.billing_policies
    ADD CONSTRAINT billing_policies_name_key UNIQUE (merchant_id, name);

ALTER TABLE ONLY openrails.billing_policies
    ADD CONSTRAINT billing_policies_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.billing_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.billing_policies FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.billing_policies USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.billing_policies TO openrails_app;

CREATE TABLE openrails.billing_policy_bindings (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    tier text,
    policy_name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE openrails.billing_policy_bindings IS 'or#897: which named policy applies to whom. Three rungs, most specific wins: per-customer (customer_id set) > per-tier (tier set) > merchant default (both NULL). The binding is JUST a name reference — rebinding is the merchant''s runtime lever and moves no money.';

COMMENT ON COLUMN openrails.billing_policy_bindings.tier IS 'Trust tier this binding applies to (the surviving rung of the retired payer_spend_limits.tier). NULL on the customer and default rungs.';

-- A binding that named BOTH a customer and a tier would be ambiguous under the
-- most-specific-wins resolution, so the shape forbids it rather than picking.
ALTER TABLE ONLY openrails.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_rung_ck CHECK ((customer_id IS NULL) OR (tier IS NULL));

ALTER TABLE ONLY openrails.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_policy_fk FOREIGN KEY (merchant_id, policy_name) REFERENCES openrails.billing_policies(merchant_id, name) ON DELETE RESTRICT;

-- Non-partial and merchant-leading: it backs BOTH the RLS predicate and the
-- resolution read, which considers all three rungs in one scan
-- (`customer_id = $2 OR IS NULL` AND `tier = $3 OR IS NULL`). The partial
-- uniques below enforce one row per rung; they cannot serve either.
CREATE INDEX idx_billing_policy_bindings_merchant_id ON openrails.billing_policy_bindings USING btree (merchant_id);

CREATE UNIQUE INDEX uq_billing_policy_bindings_default ON openrails.billing_policy_bindings USING btree (merchant_id) WHERE ((customer_id IS NULL) AND (tier IS NULL));

CREATE UNIQUE INDEX uq_billing_policy_bindings_tier ON openrails.billing_policy_bindings USING btree (merchant_id, tier) WHERE ((customer_id IS NULL) AND (tier IS NOT NULL));

CREATE UNIQUE INDEX uq_billing_policy_bindings_customer ON openrails.billing_policy_bindings USING btree (merchant_id, customer_id) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.billing_policy_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.billing_policy_bindings FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.billing_policy_bindings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.billing_policy_bindings TO openrails_app;

-- The retired table. Dropped, not deprecated: two registries answering the same
-- question is exactly the second-substrate disease or#878 removed from exposure.
-- Breaking a caller that still names it IS the point of the hard cut, and there
-- is no launched deployment holding rows worth keeping (prelaunch).
-- squawk-ignore ban-drop-table
DROP TABLE IF EXISTS openrails.payer_spend_limits;
