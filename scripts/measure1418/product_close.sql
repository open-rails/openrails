-- $1 merchant, $2 as of: each product's MRR and subscriptions at every UTC
-- month start up to it.
WITH mo AS (
    SELECT l.currency, l.product_id, date_trunc('month', l.occurred_at, 'UTC') AS month, sum(l.delta) AS d, sum(l.subscriber_delta) AS s
      FROM proto.lines_wide l
     WHERE l.merchant_id = $1 AND l.occurred_at < $2
     GROUP BY 1, 2, 3
), cal AS (
    SELECT p.currency, p.product_id, g.at
      FROM (SELECT DISTINCT currency, product_id FROM mo) p,
           generate_series((SELECT min(month) FROM mo), $2::timestamptz, interval '1 month', 'UTC') g(at)
)
INSERT INTO proto.product_month_close (merchant_id, currency, product_id, at, mrr, subscribers)
SELECT $1, cal.currency, cal.product_id, cal.at,
       COALESCE((SELECT sum(mo.d) FROM mo WHERE mo.currency = cal.currency AND mo.product_id = cal.product_id AND mo.month < cal.at), 0)::bigint,
       COALESCE((SELECT sum(mo.s) FROM mo WHERE mo.currency = cal.currency AND mo.product_id = cal.product_id AND mo.month < cal.at), 0)::bigint
  FROM cal;
