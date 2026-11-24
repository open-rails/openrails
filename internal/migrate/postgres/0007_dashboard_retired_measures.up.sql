-- Saved dashboards drop every widget whose query names a retired metrics
-- measure or the attempt_kind dimension (now kind); a layout left without
-- widgets is deleted, so its merchant gets the health overview.
WITH retired AS (
    SELECT ARRAY['gross_revenue', 'net_revenue', 'refunds', 'chargebacks', 'payment_count', 'refund_count', 'chargeback_count',
        'unique_rebilled_customers', 'realized_revenue_per_customer', 'avg_membership_duration_days', 'approved_attempts',
        'failed_attempts', 'failed_purchases', 'purchase_recovery_rate', 'attempts_per_purchase', 'rebills_due', 'rebills_open',
        'rebills_attempted', 'rebill_first_failures', 'rebills_missed', 'dunning_recovered', 'rebill_collection_rate',
        'rebill_missed_rate', 'rebill_loss_rate', 'nmi_history_approved', 'nmi_history_refused', 'nmi_history_refusal_rate',
        'usage_revenue', 'forgiven_usage', 'credit_utilization', 'billable_subscriptions', 'orphaned_members', 'freeloaders',
        'duplicate_coverage'] AS measures
), widgets AS (
    SELECT c.merchant_id, w.widget, w.position,
        COALESCE(w.widget #> '{query,measures}' ?| r.measures, false)
            OR COALESCE(w.widget #> '{query,by}' ? 'attempt_kind', false)
            OR COALESCE(w.widget #> '{query,filters}' ? 'attempt_kind', false) AS stale
    FROM billing.dashboard_configs c
    CROSS JOIN retired r
    CROSS JOIN LATERAL jsonb_array_elements(c.layout) WITH ORDINALITY AS w(widget, position)
    WHERE jsonb_typeof(c.layout) = 'array'
), cleaned AS (
    SELECT merchant_id, COALESCE(jsonb_agg(widget ORDER BY position) FILTER (WHERE NOT stale), '[]'::jsonb) AS layout
    FROM widgets
    GROUP BY merchant_id
    HAVING bool_or(stale)
), emptied AS (
    DELETE FROM billing.dashboard_configs c
    USING cleaned k
    WHERE c.merchant_id = k.merchant_id AND k.layout = '[]'::jsonb
)
UPDATE billing.dashboard_configs c
SET layout = k.layout, updated_at = now()
FROM cleaned k
WHERE c.merchant_id = k.merchant_id AND k.layout <> '[]'::jsonb;

COMMENT ON TABLE billing.dashboard_configs IS 'Per-merchant dashboard widget layout: [{id, title, viz (kpi|stat|line|area|bar|donut|table|heading), query, grid{x,y,w,h}, fixed_range}]. Absent row = the health overview built in code for the merchant''s profile.';
