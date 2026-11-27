-- $1 merchant, $2 as of: distinct customers with MRR at every UTC month start.
WITH mo AS (
    SELECT date_trunc('month', x.occurred_at, 'UTC') AS month, sum(x.d) AS d
      FROM proto.customer_logos x
     WHERE x.merchant_id = $1 AND x.occurred_at < $2
     GROUP BY 1
)
INSERT INTO proto.customer_month_close (merchant_id, at, customers)
SELECT $1, g.at, COALESCE((SELECT sum(mo.d) FROM mo WHERE mo.month < g.at), 0)::bigint
  FROM generate_series((SELECT min(month) FROM mo), $2::timestamptz, interval '1 month', 'UTC') g(at);
