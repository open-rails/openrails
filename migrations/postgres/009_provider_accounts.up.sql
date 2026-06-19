-- #518: merchant-scoped provider account bindings.
--
-- provider_accounts records the provider-returned account identity for a
-- merchant rail. Mirror/outbound rows reference this local UUID so reconcile
-- and intent execution compare rows only inside the provider account they came
-- from.

CREATE TABLE openrails.provider_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider_type text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    vault_secret_ref text,
    role text DEFAULT 'primary'::text NOT NULL,
    status text DEFAULT 'enabled'::text NOT NULL,
    evidence jsonb,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamp with time zone,
    replaced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT provider_accounts_pkey PRIMARY KEY (id),
    CONSTRAINT provider_accounts_nonempty CHECK (
        btrim(provider_type) <> ''::text
        AND btrim(environment) <> ''::text
        AND btrim(account_id) <> ''::text
    ),
    CONSTRAINT provider_accounts_environment_check CHECK (environment = ANY (ARRAY['live'::text, 'test'::text])),
    CONSTRAINT provider_accounts_role_check CHECK (role = ANY (ARRAY['primary'::text, 'secondary'::text, 'legacy'::text])),
    CONSTRAINT provider_accounts_status_check CHECK (status = ANY (ARRAY['enabled'::text, 'disabled'::text])),
    CONSTRAINT provider_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);

ALTER TABLE ONLY openrails.provider_accounts FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_provider_accounts_identity ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment, account_id);
CREATE UNIQUE INDEX uq_provider_accounts_enabled_primary ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment) WHERE (role = 'primary'::text AND status = 'enabled'::text);
CREATE INDEX idx_provider_accounts_merchant ON openrails.provider_accounts USING btree (merchant_id);

ALTER TABLE openrails.provider_accounts ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.provider_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_accounts TO openrails_app;

COMMENT ON TABLE openrails.provider_accounts IS 'Merchant-scoped provider account registry (#518). account_id is the provider-returned account/profile identity, not a credential hash.';
COMMENT ON COLUMN openrails.provider_accounts.provider_type IS 'Provider rail/type such as stripe, nmi, ccbill, solana, or a future provider type.';
COMMENT ON COLUMN openrails.provider_accounts.environment IS 'Provider environment: live or test. Live and test accounts are distinct identities and may each have their own primary.';
COMMENT ON COLUMN openrails.provider_accounts.account_id IS 'Provider-returned account identity, e.g. Stripe acct_..., NMI profile account id, CCBill account/subaccount, or Solana authority address.';
COMMENT ON COLUMN openrails.provider_accounts.role IS 'primary routes new work by default; secondary is enabled but explicit/manual; legacy is for old rows/rebills/refunds/webhooks only.';
COMMENT ON COLUMN openrails.provider_accounts.status IS 'enabled participates in routing/reconcile; disabled is retained for history but should not receive new routine work.';

ALTER TABLE openrails.provider_intents ADD COLUMN provider_account_id uuid;

INSERT INTO openrails.provider_accounts (
    merchant_id, provider_type, environment, account_id, role, status, evidence, last_verified_at
)
SELECT DISTINCT
    pi.merchant_id,
    lower(pi.provider),
    'live',
    CASE
        WHEN pi.account_fingerprint LIKE 'stripe:%' THEN substring(pi.account_fingerprint FROM 8)
        ELSE pi.account_fingerprint
    END,
    'legacy',
    'enabled',
    jsonb_build_object('source', 'legacy_provider_intents.account_fingerprint'),
    now()
FROM openrails.provider_intents pi
WHERE pi.account_fingerprint IS NOT NULL
  AND btrim(pi.account_fingerprint) <> ''
ON CONFLICT (merchant_id, provider_type, environment, account_id) DO NOTHING;

UPDATE openrails.provider_intents pi
SET provider_account_id = pa.id
FROM openrails.provider_accounts pa
WHERE pa.merchant_id = pi.merchant_id
  AND pa.provider_type = lower(pi.provider)
  AND pa.environment = 'live'
  AND pa.account_id = CASE
        WHEN pi.account_fingerprint LIKE 'stripe:%' THEN substring(pi.account_fingerprint FROM 8)
        ELSE pi.account_fingerprint
      END
  AND pi.account_fingerprint IS NOT NULL
  AND btrim(pi.account_fingerprint) <> '';

ALTER TABLE openrails.provider_intents
    ADD CONSTRAINT provider_intents_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);

CREATE INDEX idx_provider_intents_provider_account ON openrails.provider_intents USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);

ALTER TABLE openrails.provider_intents DROP COLUMN account_fingerprint;

ALTER TABLE openrails.payments ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.payments
    ADD CONSTRAINT payments_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_payments_provider_account ON openrails.payments USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);
DROP INDEX openrails.uq_payments_merchant_processor_transaction;
CREATE UNIQUE INDEX uq_payments_merchant_processor_transaction_legacy ON openrails.payments USING btree (merchant_id, processor, transaction_id) WHERE (provider_account_id IS NULL);
CREATE UNIQUE INDEX uq_payments_provider_account_transaction ON openrails.payments USING btree (merchant_id, provider_account_id, transaction_id) WHERE (provider_account_id IS NOT NULL);

ALTER TABLE openrails.subscriptions ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.subscriptions
    ADD CONSTRAINT subscriptions_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_subscriptions_provider_account ON openrails.subscriptions USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);
DROP INDEX openrails.uq_subscriptions_merchant_processor_subscription_id;
CREATE UNIQUE INDEX uq_subscriptions_merchant_processor_subscription_id_legacy ON openrails.subscriptions USING btree (merchant_id, processor, processor_subscription_id) WHERE (provider_account_id IS NULL AND processor_subscription_id <> ''::text);
CREATE UNIQUE INDEX uq_subscriptions_provider_account_subscription ON openrails.subscriptions USING btree (merchant_id, provider_account_id, processor_subscription_id) WHERE (provider_account_id IS NOT NULL AND processor_subscription_id <> ''::text);

ALTER TABLE openrails.payment_methods ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.payment_methods
    ADD CONSTRAINT payment_methods_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_payment_methods_provider_account ON openrails.payment_methods USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);
DROP INDEX openrails.uq_payment_methods_merchant_processor_vault;
CREATE UNIQUE INDEX uq_payment_methods_merchant_processor_vault_legacy ON openrails.payment_methods USING btree (merchant_id, processor, vault_id) WHERE (provider_account_id IS NULL);
DROP INDEX openrails.uq_payment_methods_customer_vault;
CREATE UNIQUE INDEX uq_payment_methods_customer_vault_legacy ON openrails.payment_methods USING btree (merchant_id, customer_id, vault_id) WHERE (provider_account_id IS NULL);
CREATE UNIQUE INDEX uq_payment_methods_provider_account_vault ON openrails.payment_methods USING btree (merchant_id, provider_account_id, vault_id) WHERE (provider_account_id IS NOT NULL);

ALTER TABLE openrails.processor_customers ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.processor_customers
    ADD CONSTRAINT processor_customers_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_processor_customers_provider_account ON openrails.processor_customers USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);
DROP INDEX openrails.uq_processor_customers_merchant_processor_customer;
CREATE UNIQUE INDEX uq_processor_customers_merchant_processor_customer_legacy ON openrails.processor_customers USING btree (merchant_id, processor, processor_customer_id) WHERE (provider_account_id IS NULL);
DROP INDEX openrails.uq_processor_customers_customer_processor;
CREATE UNIQUE INDEX uq_processor_customers_customer_processor_legacy ON openrails.processor_customers USING btree (merchant_id, customer_id, processor) WHERE (provider_account_id IS NULL);
CREATE UNIQUE INDEX uq_processor_customers_provider_account_customer ON openrails.processor_customers USING btree (merchant_id, provider_account_id, processor_customer_id) WHERE (provider_account_id IS NOT NULL);
CREATE UNIQUE INDEX uq_processor_customers_customer_provider_account ON openrails.processor_customers USING btree (merchant_id, customer_id, provider_account_id) WHERE (provider_account_id IS NOT NULL);

ALTER TABLE openrails.checkout_sessions ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_checkout_sessions_provider_account ON openrails.checkout_sessions USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);

ALTER TABLE openrails.invoice_payments ADD COLUMN provider_account_id uuid;
ALTER TABLE openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);
CREATE INDEX idx_invoice_payments_provider_account ON openrails.invoice_payments USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);

COMMENT ON COLUMN openrails.provider_intents.provider_account_id IS 'Provider account row the outbound intent was enqueued against. Mismatch with current credentials parks/defers execution.';
COMMENT ON COLUMN openrails.payments.provider_account_id IS 'Provider account that produced this payment/charge mirror row.';
COMMENT ON COLUMN openrails.subscriptions.provider_account_id IS 'Provider account that produced this remote subscription mirror row.';
COMMENT ON COLUMN openrails.payment_methods.provider_account_id IS 'Provider account that produced this vaulted payment method mirror row.';
COMMENT ON COLUMN openrails.processor_customers.provider_account_id IS 'Provider account that produced this processor customer mirror row.';
COMMENT ON COLUMN openrails.checkout_sessions.provider_account_id IS 'Provider account selected for this provider checkout/session.';
COMMENT ON COLUMN openrails.invoice_payments.provider_account_id IS 'Provider account used for this invoice payment attempt or settled provider payment.';
