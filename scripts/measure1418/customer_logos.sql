-- $1 merchant: the customer-level logo changes, in the order of each
-- customer's movements.
INSERT INTO proto.customer_logos (merchant_id, occurred_at, d)
SELECT $1, x.occurred_at, (x.n > 0)::integer - (x.n - x.logo_delta > 0)::integer
  FROM (SELECT m.occurred_at, m.logo_delta,
               sum(m.logo_delta) OVER (PARTITION BY m.customer_id ORDER BY m.occurred_at, m.id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS n
          FROM billing.mrr_movements m
         WHERE m.merchant_id = $1 AND m.logo_delta <> 0) x
 WHERE (x.n > 0)::integer - (x.n - x.logo_delta > 0)::integer <> 0;
