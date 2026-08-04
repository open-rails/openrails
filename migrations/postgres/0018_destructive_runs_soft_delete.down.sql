-- Reverses 0018. Soft-deleted rows become visible again (they were never
-- removed), and the prune-run record goes away with the stamps.

-- The views must lose their deleted_at references before the columns go.

CREATE OR REPLACE VIEW openrails.freeloader_episodes WITH (security_invoker='true') AS
 WITH win AS (
         SELECT e.merchant_id,
            e.customer_id,
            e.id AS entitlement_id,
            e.entitlement,
            e.source_type,
            e.source_id,
            e.start_at,
            LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone)) AS window_end,
            s.status AS sub_status,
            s.next_retry_at,
            GREATEST(s.current_period_ends_at, s.ended_at) AS paid_through,
            p.id AS payment_id,
            p.status AS payment_status,
            COALESCE(( SELECT max(r.purchased_at) AS max
                   FROM openrails.payments r
                  WHERE ((r.merchant_id = e.merchant_id) AND (r.refunded_payment_id = p.id))), p.purchased_at) AS refund_effective_at,
            ( SELECT max(COALESCE(g.ends_at, 'infinity'::timestamp with time zone)) AS max
                   FROM openrails.grants g
                  WHERE ((g.merchant_id = e.merchant_id) AND (g.customer_id = e.customer_id) AND (g.event = 'grant'::text) AND (g.kind = 'entitlement'::text) AND (g.starts_at <= now()) AND ((g.id = e.grant_id) OR ((g.source_id = (e.source_id)::text) AND (((e.source_type = 'subscription'::text) AND (g.source_type = 'subscription'::text)) OR ((e.source_type = 'one_off'::text) AND (g.source_type = 'purchase'::text))))) AND (NOT (EXISTS ( SELECT 1
                           FROM openrails.grants t
                          WHERE ((t.merchant_id = g.merchant_id) AND (t.supersedes_id = g.id) AND (t.event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])))))))) AS grant_covered_until
           FROM ((openrails.entitlements e
             LEFT JOIN openrails.subscriptions s ON (((e.source_type = 'subscription'::text) AND (s.id = e.source_id) AND (s.merchant_id = e.merchant_id))))
             LEFT JOIN openrails.payments p ON (((e.source_type = 'one_off'::text) AND (p.id = e.source_id) AND (p.merchant_id = e.merchant_id))))
          WHERE (e.source_type = ANY (ARRAY['subscription'::text, 'one_off'::text]))
        ), spans AS (
         SELECT w.merchant_id,
            w.customer_id,
            w.entitlement_id,
            w.entitlement,
            w.source_type,
            w.source_id,
            w.start_at,
            w.window_end,
            w.sub_status,
            w.next_retry_at,
            w.paid_through,
            w.payment_id,
            w.payment_status,
            w.refund_effective_at,
            w.grant_covered_until,
            GREATEST(w.start_at,
                CASE
                    WHEN (w.source_type = 'subscription'::text) THEN COALESCE(w.paid_through, '-infinity'::timestamp with time zone)
                    WHEN (w.payment_status = 'completed'::openrails.payment_status) THEN 'infinity'::timestamp with time zone
                    WHEN (w.payment_status = 'refunded'::openrails.payment_status) THEN w.refund_effective_at
                    ELSE '-infinity'::timestamp with time zone
                END, COALESCE(w.grant_covered_until, '-infinity'::timestamp with time zone)) AS unpaid_from,
            LEAST(w.window_end, now()) AS unpaid_until
           FROM win w
        )
 SELECT merchant_id,
    customer_id,
    entitlement_id,
    entitlement,
    source_type,
    source_id,
        CASE
            WHEN ((sub_status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL)) THEN 'sanctioned_dunning'::text
            WHEN (sub_status = 'unknown'::openrails.subscription_status) THEN 'awaiting_verification'::text
            ELSE 'unsanctioned'::text
        END AS cause,
    unpaid_from AS started_at,
    unpaid_until AS ended_at,
    (window_end > now()) AS open,
    ((EXTRACT(epoch FROM (unpaid_until - unpaid_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (unpaid_from < unpaid_until);

CREATE OR REPLACE VIEW openrails.orphaned_episodes WITH (security_invoker='true') AS
 WITH cov AS (
         SELECT s.merchant_id,
            s.customer_id,
            'subscription'::text AS source_type,
            s.id AS source_id,
            s.product_id,
            COALESCE(s.current_period_starts_at, s.started_at) AS cov_start,
            GREATEST(s.current_period_ends_at, s.ended_at) AS cov_end
           FROM (openrails.subscriptions s
             JOIN openrails.products pd ON (((pd.id = s.product_id) AND (pd.merchant_id = s.merchant_id))))
          WHERE ((s.status <> 'pending'::openrails.subscription_status) AND (GREATEST(s.current_period_ends_at, s.ended_at) IS NOT NULL) AND (((pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb)) OR ((s.entitlements_spec_snapshot IS NOT NULL) AND (s.entitlements_spec_snapshot <> '{}'::jsonb))))
        UNION ALL
         SELECT p.merchant_id,
            p.customer_id,
            'one_off'::text AS text,
            p.id,
            pr.product_id,
            p.purchased_at,
            (p.purchased_at + make_interval(hours => pr.access_duration_hours))
           FROM ((openrails.payments p
             JOIN openrails.prices pr ON (((pr.id = p.price_id) AND (pr.merchant_id = p.merchant_id))))
             JOIN openrails.products pd ON (((pd.id = pr.product_id) AND (pd.merchant_id = p.merchant_id))))
          WHERE ((p.status = 'completed'::openrails.payment_status) AND (p.amount > 0) AND (p.subscription_id IS NULL) AND (pr.access_duration_hours IS NOT NULL) AND (pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb))
        ), spans AS (
         SELECT c.merchant_id,
            c.customer_id,
            c.source_type,
            c.source_id,
            c.product_id,
            c.cov_start,
            c.cov_end,
            GREATEST(c.cov_start, COALESCE(( SELECT max(LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone))) AS max
                   FROM openrails.entitlements e
                  WHERE ((e.merchant_id = c.merchant_id) AND (e.customer_id = c.customer_id) AND (e.source_type = c.source_type) AND (e.source_id = c.source_id) AND (e.start_at <= now()))), '-infinity'::timestamp with time zone)) AS uncovered_from,
            LEAST(c.cov_end, now()) AS uncovered_until
           FROM cov c
        )
 SELECT merchant_id,
    customer_id,
    source_type,
    source_id,
    product_id,
    uncovered_from AS started_at,
    uncovered_until AS ended_at,
    (cov_end > now()) AS open,
    ((EXTRACT(epoch FROM (uncovered_until - uncovered_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (uncovered_from < uncovered_until);

DROP INDEX IF EXISTS openrails.checkout_sessions_rail_transaction_id_idx;
CREATE UNIQUE INDEX checkout_sessions_rail_transaction_id_idx
    ON openrails.checkout_sessions USING btree (rail, transaction_id)
    WHERE (transaction_id IS NOT NULL);

DROP INDEX IF EXISTS openrails.checkout_sessions_rail_reference_idx;
CREATE UNIQUE INDEX checkout_sessions_rail_reference_idx
    ON openrails.checkout_sessions USING btree (rail, reference)
    WHERE (reference IS NOT NULL);

DROP INDEX IF EXISTS openrails.uq_payments_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction
    ON openrails.payments USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), transaction_id);

DROP INDEX IF EXISTS openrails.uq_subscriptions_merchant_psp_subscription_id;
CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id
    ON openrails.subscriptions USING btree (
        merchant_id, rail, COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid), rail_subscription_id)
    WHERE (rail_subscription_id <> ''::text);

DROP INDEX IF EXISTS openrails.uq_subscriptions_customer_tier_group_active;
CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active
    ON openrails.subscriptions USING btree (customer_id, tier_group)
    WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status])) AND (tier_group IS NOT NULL));

DROP INDEX IF EXISTS openrails.uq_subscriptions_customer_product_lifecycle;
CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle
    ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id)
    WHERE (status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status, 'past_due'::openrails.subscription_status]));

DROP INDEX IF EXISTS openrails.idx_entitlements_destructive_run;
DROP INDEX IF EXISTS openrails.idx_checkout_sessions_destructive_run;
DROP INDEX IF EXISTS openrails.idx_payments_destructive_run;
DROP INDEX IF EXISTS openrails.idx_subscriptions_destructive_run;

ALTER TABLE openrails.entitlements DROP COLUMN IF EXISTS destructive_run_id;
ALTER TABLE openrails.checkout_sessions DROP COLUMN IF EXISTS destructive_run_id, DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE openrails.payments DROP COLUMN IF EXISTS destructive_run_id, DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE openrails.subscriptions DROP COLUMN IF EXISTS destructive_run_id, DROP COLUMN IF EXISTS deleted_at;

DROP TABLE IF EXISTS openrails.destructive_runs;
