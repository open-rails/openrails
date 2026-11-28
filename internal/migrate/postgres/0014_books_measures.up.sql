-- Saved dashboards drop every widget whose query names a measure the books'
-- measures replaced (outstanding_credit_liability, outstanding_owed: balance),
-- the stream dimension (now account), or credits_sold by customer (it reads
-- the books, by currency and product); a layout left without widgets is
-- deleted, so its merchant gets the health overview.
WITH retired AS (
    SELECT ARRAY['outstanding_credit_liability', 'outstanding_owed'] AS measures
), widgets AS (
    SELECT c.merchant_id, w.widget, w.position,
        COALESCE(w.widget #> '{query,measures}' ?| r.measures, false)
            OR COALESCE(w.widget #> '{query,by}' ? 'stream', false)
            OR COALESCE(w.widget #> '{query,filters}' ? 'stream', false)
            OR COALESCE(w.widget #> '{query,measures}' ? 'credits_sold', false)
               AND (COALESCE(w.widget #> '{query,by}' ? 'customer', false) OR COALESCE(w.widget #> '{query,filters}' ? 'customer', false)) AS stale
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

-- The live schedules by merchant: the forward waterfall of deferred revenue
-- reads them. Each statement here may run again.
CREATE INDEX IF NOT EXISTS revenue_schedules_live_idx ON billing.revenue_schedules (merchant_id, next_recognition_at)
    WHERE next_recognition_at IS NOT NULL;
