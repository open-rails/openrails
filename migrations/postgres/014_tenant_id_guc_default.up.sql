-- #336: remove the "default tenant". tenant_id is strictly required — its
-- column DEFAULT derives from the per-connection app.tenant_id GUC (set by
-- WithTenantConn from the resolved/configured tenant), with NO fallback. When
-- the GUC is unset, the default evaluates to NULL and the NOT NULL constraint
-- rejects the write — a missing tenant is now a loud error instead of silently
-- landing under a fake default tenant. Writers that pass tenant_id explicitly
-- are unaffected. Applies to every tenant_id-owner table; the '…0001' literal
-- default is gone. (platform_audit/platform_break_glass have no tenant_id owner
-- column — only target_tenant_id — so they are not listed here.)

ALTER TABLE openrails.admin_grants
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.budget_reservations
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.budget_window_state
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.catalog_drift_events
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.checkout_sessions
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_account_settings
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_balances
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_blocks
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_spend_limits
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_transactions
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_types
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.credit_windows
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.entitlement_features
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.entitlements
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.invoices
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.linked_wallets
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.manual_rebill_attempts
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.notification_queue
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.payment_blocklist
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.payment_methods
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.payments
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.prices
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.processor_customers
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.product_access_grants
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.product_entitlement_features
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.products
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.provider_intents
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.reconciliation_findings
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.reconciliation_runs
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.solana_subscriptions
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.subscriptions
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_credential_audit
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_deks
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_delegated_issuers
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_exports
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_secrets
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tenant_subjects
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.tier_policies
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.usage_events
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
ALTER TABLE openrails.usdc_funding_sessions
    ALTER COLUMN tenant_id SET DEFAULT (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid;
