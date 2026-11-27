-- The facts-only end state: pending scheduled churn becomes a movement dated
-- at its end, counted once that date passes. Classified per customer and
-- currency in date order from the customer's MRR now. $1 merchant, $2 as of.
WITH e AS (
    SELECT ms.merchant_id, ms.customer_id, ms.currency, ms.effective_at, sum(ms.mrr_delta)::bigint AS delta, min(ms.cause) AS cause
      FROM billing.mrr_scheduled ms
     WHERE ms.merchant_id = $1 AND ms.status = 'scheduled' AND ms.kind = 'churn' AND ms.effective_at > $2
     GROUP BY 1, 2, 3, 4
), r AS (
    SELECT e.*, cm.first_active_at,
           cm.mrr + sum(e.delta) OVER (PARTITION BY e.customer_id, e.currency ORDER BY e.effective_at) AS after
      FROM e
      JOIN billing.customer_mrr cm ON cm.merchant_id = e.merchant_id AND cm.customer_id = e.customer_id AND cm.currency = e.currency
)
INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
    cohort_at, cause, voluntary, occurred_at)
SELECT merchant_id, customer_id, currency, CASE WHEN after = 0 THEN 'churn' ELSE 'contraction' END, delta, after - delta, after,
       CASE WHEN after = 0 THEN -1 ELSE 0 END, first_active_at, cause,
       CASE WHEN after = 0 THEN cause IN ('canceled', 'merchant_canceled') END, effective_at
  FROM r;
