-- $1 merchant, $2 as of: a close at every UTC month start up to it.
WITH mo AS (
    SELECT m.currency, date_trunc('month', m.occurred_at, 'UTC') AS month, sum(m.delta) AS d, sum(m.logo_delta) AS l
      FROM billing.mrr_movements m
     WHERE m.merchant_id = $1 AND m.occurred_at < $2
     GROUP BY 1, 2
), cal AS (
    SELECT c.currency, g.at
      FROM (SELECT DISTINCT currency FROM mo) c,
           generate_series((SELECT min(month) FROM mo), $2::timestamptz, interval '1 month', 'UTC') g(at)
)
INSERT INTO proto.mrr_month_close (merchant_id, currency, at, mrr, customers)
SELECT $1, cal.currency, cal.at,
       COALESCE((SELECT sum(mo.d) FROM mo WHERE mo.currency = cal.currency AND mo.month < cal.at), 0)::bigint,
       COALESCE((SELECT sum(mo.l) FROM mo WHERE mo.currency = cal.currency AND mo.month < cal.at), 0)::bigint
  FROM cal;
