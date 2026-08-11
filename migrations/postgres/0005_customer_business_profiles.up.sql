-- or#908: customer business profile — B2B onboarding as a first-class record.
--
-- The row IS the business posture. There is deliberately no is_business flag
-- anywhere in the schema: a payer is a business customer exactly when this
-- onboarding record (terms acceptance + KYC reference + billing currency)
-- exists, and stops being one when it is deleted — which the offboard
-- chokepoint refuses while the payer still owes (see
-- money.OffboardBusinessCustomer). Invoice-document fields (net terms,
-- collection method, PO, tax, contacts) stay on customer_invoice_profiles;
-- the arrears credit line stays on money_settings. This table carries only
-- what onboarding itself asserts: who accepted which terms when, the KYC
-- reference behind the credit decision, the profile's billing currency, and
-- the notify-only budget-alert thresholds (native units at the currency's
-- registered scale, ascending).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.customer_business_profiles (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    terms_version text NOT NULL,
    terms_accepted_at timestamp with time zone NOT NULL,
    terms_accepted_by text DEFAULT ''::text NOT NULL,
    kyc_reference text DEFAULT ''::text NOT NULL,
    currency text NOT NULL,
    budget_alert_thresholds bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_business_profiles_pkey PRIMARY KEY (merchant_id, customer_id),
    CONSTRAINT customer_business_profiles_terms_version_chk CHECK ((terms_version <> ''::text)),
    CONSTRAINT customer_business_profiles_currency_shape CHECK (((currency ~ '^[A-Z0-9]{3,12}$'::text) OR (currency ~ '^[a-z0-9][a-z0-9_-]*/[^/[:space:]]+$'::text))),
    CONSTRAINT customer_business_profiles_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES openrails.customers(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_business_profiles_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT
);

ALTER TABLE openrails.customer_business_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.customer_business_profiles FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.customer_business_profiles IS 'or#908 B2B onboarding record. Row presence IS the business posture (no settable flag exists); created only through the onboard chokepoint (terms acceptance required), deleted only through offboard (refused while the payer owes). Budget-alert thresholds are notify-only — alerts never cap.';

CREATE POLICY merchant_isolation ON openrails.customer_business_profiles USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customer_business_profiles TO openrails_app;
