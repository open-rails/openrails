-- Saved dashboards drop every widget whose query names a measure the MRR and
-- authorization measures replaced, the status dimension (now risk_reason),
-- or mrr by a dimension it no longer takes (it reads the MRR rollups, by
-- currency and product only); a layout left without widgets is deleted, so
-- its merchant gets the health overview.
WITH retired AS (
    SELECT ARRAY['new_subscriptions', 'net_new_subscriptions', 'churn_rate', 'subscriptions', 'attempt_failure_rate',
        'purchase_failure_rate', 'rebill_first_failure_rate'] AS measures,
        ARRAY['rail', 'psp', 'price_id', 'billing_cycle'] AS mrr_dims
), widgets AS (
    SELECT c.merchant_id, w.widget, w.position,
        COALESCE(w.widget #> '{query,measures}' ?| r.measures, false)
            OR COALESCE(w.widget #> '{query,by}' ? 'status', false)
            OR COALESCE(w.widget #> '{query,filters}' ? 'status', false)
            OR COALESCE(w.widget #> '{query,measures}' ? 'mrr', false)
               AND (COALESCE(w.widget #> '{query,by}' ?| r.mrr_dims, false) OR COALESCE(w.widget #> '{query,filters}' ?| r.mrr_dims, false)) AS stale
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

COMMENT ON TABLE billing.dashboard_configs IS 'Per-merchant dashboard widget layout: [{id, title, viz (kpi|stat|line|area|bar|donut|table|cohort|heading|programs), query, grid{x,y,w,h}, fixed_range}]. Absent row = the health overview built in code for the merchant''s profile.';
