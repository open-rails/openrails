-- #336: multi-tenant writers must stamp tenant_id. Several checkout/billing
-- INSERTs omit the tenant_id column entirely and so take its static
-- default-tenant default ('…0001'), landing rows under the WRONG tenant on a
-- delegated/multi-tenant write (and getting rejected by the RLS tenant_isolation
-- WITH CHECK under the openrails_app role).
--
-- Fix: derive the column DEFAULT from the same `app.tenant_id` GUC the RLS
-- policy already reads (set per-connection by WithTenantConn from the request /
-- pinned-tenant context). When the GUC is set, an omitted-tenant_id INSERT now
-- stamps the connection's tenant; when it is unset (single-tenant standalone),
-- it falls back to the default tenant exactly as before. Inserts that already
-- pass tenant_id explicitly are unaffected (the default is never consulted).
--
-- Scoped to the tenant-scoped DATA tables the #336 audit found writing under the
-- default tenant; platform-global tables (platform_audit, platform_break_glass,
-- credit_types) are intentionally left on the static default.

ALTER TABLE openrails.checkout_sessions
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.subscriptions
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.payments
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.payment_methods
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.processor_customers
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.admin_grants
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.notification_queue
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.catalog_drift_events
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
ALTER TABLE openrails.entitlements
    ALTER COLUMN tenant_id SET DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid);
