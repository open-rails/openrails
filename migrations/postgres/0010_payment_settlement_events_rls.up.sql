-- #827: payment_settlement_events (0005, #165 host feed) shipped without RLS —
-- in a shared DB any openrails_app connection could read/ack every merchant's
-- settlement events. Fail-closed merchant_isolation, same pattern as 0001/0002.

ALTER TABLE openrails.payment_settlement_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.payment_settlement_events FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payment_settlement_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- RLS-exemption markers (audited by TestRLSExemptionsAreClassifiedAndReviewed):
-- deliberately-global tables must say so explicitly.
COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory: a dumb billing bucket (whose books a row goes on). GLOBAL (control-plane) table, not tenant-scoped. Carries ONLY billing/money-rail state, NO auth. Merchants are registered explicitly; there is no default merchant. RLS-exempt by design: it IS the tenant directory — the scope, not a scoped row.';
COMMENT ON TABLE openrails.worker_health IS '#689 per-River-worker-kind health: last success/error + failure streak, written by the worker middleware. Operator-global control-plane table. RLS-exempt by design: process health per worker kind, not tenant data.';
