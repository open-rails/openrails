-- #511 CON plane queries (conPass). Each takes an OPTIONAL customer_id: when set
-- (inline Converge(customer)), the scan is restricted to that one customer's rows
-- so an after-every-mutation invocation is O(customer), not O(merchant); when
-- NULL (the merchant-wide sweep) it scans the whole merchant. All are RLS-scoped.
-- These replace the retired internal/audit checks (#511 Phase F hard cut).

-- Partition (#690): a LIVE window with a dangling subscription source is the
-- freeloader case — derive.entitlement.unjustified (severity high, revoke
-- recommendation) owns it. This check keeps only NON-LIVE dangling references
-- (revoked/expired history rows): referential hygiene, no access at stake.
-- name: ConOrphanEntitlementSubscriptionSource :many
SELECT ent.id AS ent_id, ent.customer_id::text AS user_id, ent.entitlement, ent.source_type, ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.subscriptions sub ON sub.merchant_id = sqlc.arg(merchant_id)::uuid AND ent.source_id = sub.id AND sub.deleted_at IS NULL
WHERE ent.merchant_id = sqlc.arg(merchant_id)::uuid AND ent.source_type = 'subscription'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND sub.id IS NULL
  AND NOT (ent.revoked_at IS NULL
           AND ent.start_at <= sqlc.arg(now)::timestamptz
           AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(now)::timestamptz))
  AND (sqlc.narg(customer_id)::uuid IS NULL OR ent.customer_id = sqlc.narg(customer_id)::uuid);

-- name: ConOrphanEntitlementPaymentSource :many
SELECT ent.id AS ent_id, ent.customer_id::text AS user_id, ent.entitlement, ent.source_type, ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.payments purch ON purch.merchant_id = sqlc.arg(merchant_id)::uuid AND ent.source_id = purch.id AND purch.deleted_at IS NULL
WHERE ent.merchant_id = sqlc.arg(merchant_id)::uuid AND ent.source_type = 'one_off'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND purch.id IS NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR ent.customer_id = sqlc.narg(customer_id)::uuid);


-- #690 CON `consistency.duplicate.ownership` — more than one LIVE
-- (un-terminated, window covering now) PAID ownership grant for the same
-- (customer, product): the cross-month one-off/lifetime double-purchase the
-- month-scoped duplicate.provider_charge check cannot see. Paid sources only
-- (purchase/subscription — admin/grace grants charge nobody twice) and
-- bundle-included child grants excluded (source_id 'include:%': two bundles
-- sharing a child is one charge per bundle, not a double charge for the
-- child). A refunded purchase no longer charges the customer, so grants whose
-- payment is refunded (status flip OR a linked refund row — the admin refund
-- path records a negative row and leaves the original 'completed') drop out:
-- the #692 approve→refund fix self-confirms on the next sweep instead of
-- reopening; the access-side residue is derive.grant.excess's domain.
-- Purchases ride as a jsonb array (payment linkage nullable) ordered
-- oldest-first, so the LAST element is the later purchase — the default
-- cancel/refund target. RLS scopes the merchant. customer_id nullable:
-- NULL = merchant-wide sweep.
-- name: ConDuplicateOwnershipGrants :many
WITH live_ownership AS (
    SELECT g.id, g.customer_id, g.product_id, g.source_type, g.source_id,
           g.payment_id, g.starts_at, g.created_at,
           pay.amount AS payment_amount, pay.currency AS payment_currency,
           pay.purchased_at
    FROM openrails.grants g
    LEFT JOIN openrails.payments pay ON pay.merchant_id = sqlc.arg(merchant_id)::uuid AND pay.id = g.payment_id AND pay.deleted_at IS NULL
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.event = 'grant' AND g.kind = 'ownership'
      AND g.product_id IS NOT NULL
      AND g.source_type IN ('purchase', 'subscription')
      AND g.source_id NOT LIKE 'include:%'
      AND g.starts_at <= sqlc.arg(now)::timestamptz
      AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(now)::timestamptz)
      AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
      AND NOT EXISTS (
          SELECT 1 FROM openrails.grants t
          WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
      )
      AND (pay.id IS NULL OR (pay.status <> 'refunded' AND NOT EXISTS (
          SELECT 1 FROM openrails.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = pay.id AND r.deleted_at IS NULL
      )))
)
SELECT lo.customer_id, lo.product_id, prod.key AS product_key,
       COUNT(*)::int AS count,
       jsonb_agg(jsonb_build_object(
           'grant_id', lo.id,
           'source_type', lo.source_type,
           'source_id', lo.source_id,
           'payment_id', lo.payment_id,
           'amount', lo.payment_amount,
           'currency', lo.payment_currency,
           'purchased_at', COALESCE(lo.purchased_at, lo.starts_at)
       ) ORDER BY COALESCE(lo.purchased_at, lo.starts_at), lo.created_at) AS purchases
FROM live_ownership lo
JOIN openrails.products prod ON prod.id = lo.product_id

WHERE prod.merchant_id = sqlc.arg(merchant_id)::uuid
GROUP BY lo.customer_id, lo.product_id, prod.key
HAVING COUNT(*) > 1;

-- name: ConDuplicateChargesSamePeriod :many
-- More than one captured charge for ONE subscription period. A charge carries
-- the period it paid for (metadata period_start); charges without it (older
-- rows, other writers) are judged by the subscription's cadence: two at one
-- price within half its shortest cycle (billing or trial) of each other; a
-- price change (upgrade, reprice) starts a new coverage. Distinct periods,
-- however close, are never a duplicate. One-time purchases are
-- consistency.duplicate.ownership's domain. Refunds net out both ways (#690).
WITH charges AS (
    SELECT purch.id, purch.customer_id, purch.subscription_id, purch.price_id, purch.amount, purch.purchased_at,
           price.product_id, prod.key AS product_key,
           purch.metadata->>'period_start' AS period_start,
           LEAST(price.access_duration_hours, COALESCE(price.trial_duration_hours, price.access_duration_hours)) AS cycle_hours
    FROM openrails.payments purch
    JOIN openrails.prices price ON purch.price_id = price.id
    JOIN openrails.products prod ON price.product_id = prod.id
    WHERE purch.merchant_id = sqlc.arg(merchant_id)::uuid AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND purch.deleted_at IS NULL
      AND purch.subscription_id IS NOT NULL
      AND purch.status = 'completed'
      AND purch.money_movement = 'rail'
      AND purch.amount > 0
      AND purch.refunded_payment_id IS NULL
      AND NOT EXISTS (
          SELECT 1 FROM openrails.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = purch.id AND r.deleted_at IS NULL
      )
      AND (sqlc.narg(customer_id)::uuid IS NULL OR purch.customer_id = sqlc.narg(customer_id)::uuid)
),
unstamped AS (
    SELECT c.*, LAG(c.id) OVER w AS prev_id, LAG(c.purchased_at) OVER w AS prev_at
    FROM charges c
    WHERE c.period_start IS NULL
    WINDOW w AS (PARTITION BY c.subscription_id, c.price_id ORDER BY c.purchased_at, c.id)
),
groups AS (
    SELECT subscription_id, period_start AS period_key, id, customer_id, product_id, product_key, amount, purchased_at
    FROM charges
    WHERE period_start IS NOT NULL
      AND (subscription_id, period_start) IN (
          SELECT subscription_id, period_start FROM charges WHERE period_start IS NOT NULL
          GROUP BY subscription_id, period_start HAVING COUNT(*) > 1)
    UNION ALL
    SELECT u.subscription_id, to_char(p.purchased_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), x.id, x.customer_id, x.product_id, x.product_key, x.amount, x.purchased_at
    FROM unstamped u
    JOIN charges p ON p.id = u.prev_id
    JOIN charges x ON x.id IN (u.id, u.prev_id)
    WHERE u.cycle_hours IS NOT NULL AND u.cycle_hours > 0
      AND u.purchased_at - u.prev_at < make_interval(secs => u.cycle_hours * 1800)
)
SELECT
    subscription_id,
    period_key::text AS period_key,
    MIN(customer_id::text)::text AS user_id,
    MIN(product_id::text)::uuid AS product_id,
    MIN(product_key)::text AS product_key,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY purchased_at DESC)::uuid[] AS payment_ids,
    SUM(amount)::bigint AS total_amount,
    MIN(purchased_at)::timestamptz AS first_date,
    MAX(purchased_at)::timestamptz AS last_date
FROM groups
GROUP BY subscription_id, period_key;

-- name: ResolveVanishedFindings :execrows
-- Open findings of one type under a subject prefix that the latest full scan
-- no longer reports close themselves.
UPDATE openrails.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now()
 WHERE merchant_id = sqlc.arg(merchant_id)::uuid
   AND finding_type = sqlc.arg(finding_type)::text
   AND subject_key LIKE sqlc.arg(subject_prefix)::text || '%'
   AND NOT (subject_key = ANY(sqlc.arg(keep_subjects)::text[]))
   AND status IN ('reconcile_required', 'requires_review');
