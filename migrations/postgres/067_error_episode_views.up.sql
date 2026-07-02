-- #690 episode analytics: the HISTORICAL/interval measure behind the three
-- error categories. The always-zero gauges stay point-in-time; these two views
-- express errors as SPANS ("user X had access over [A, B) without payment
-- covering it" and the mirror "payment covered [A, B) with no access"), with
-- total error-days derivable by SUM(days). Plain SQL views over the ledger —
-- no new storage, no workers; open episodes end at now().
--
-- security_invoker: the views run with the QUERYING role's privileges, so the
-- merchant RLS policies on the underlying tables (app.merchant_id GUC) apply
-- exactly as they do for direct table reads.
--
-- HONEST APPROXIMATIONS (both views — the columns simply do not carry more):
--   * paid-through = GREATEST(subscriptions.current_period_ends_at, ended_at)
--     is a SNAPSHOT of the current/latest period, not per-payment history.
--     Renewals overwrite the same row, so a historical lapse that later healed
--     is invisible; only the CURRENT uncovered span is measured.
--   * coverage is measured contiguous-from-the-left: the episode is the
--     uncovered TAIL (from where coverage stopped to window/period end).
--     Leading or mid-span gaps are not detected.
--   * cause labels read the subscription's CURRENT state (dunning/unknown are
--     live states, not reconstructed history).
--   * a refund's effective time is the linked refund row's purchased_at; when
--     a payment is status='refunded' with no linked refund row, the original
--     purchase time is used (upper-bound span).

-- freeloader_episodes: spans where a subscription- or one_off-sourced
-- entitlement window granted access NOT covered by payment. One row per
-- (window, uncovered tail). Causes:
--   sanctioned_dunning    - sub is past_due WITH a retry scheduled: unpaid
--                           access is deliberate dunning policy, not failure
--   awaiting_verification - sub is `unknown` past paid-through: the #691
--                           fail-open carve-out (access intact by policy
--                           until the provider verdict lands)
--   unsanctioned          - everything else (missing sub row, terminal sub
--                           whose closure never landed, refunded payment,
--                           stalled dunning without a retry, ...): the
--                           failure class the always-zero gauges alert on
-- Coverage credited to a window: the source subscription's paid-through, a
-- completed non-refunded one_off payment (covers the window it created), and
-- any live un-terminated entitlement grant matching the window (same matching
-- as the derive.entitlement.unjustified detector) through the grant's own
-- [starts_at, ends_at) — a standing grant (ends_at NULL) covers forever.
-- Admin/grace/grant-sourced windows are operator-sanctioned by construction
-- and never counted.
CREATE VIEW openrails.freeloader_episodes WITH (security_invoker = true) AS
WITH win AS (
    SELECT e.merchant_id,
           e.customer_id,
           e.id AS entitlement_id,
           e.entitlement,
           e.source_type,
           e.source_id,
           e.start_at,
           LEAST(COALESCE(e.revoked_at, 'infinity'::timestamptz),
                 COALESCE(e.deleted_at, 'infinity'::timestamptz),
                 COALESCE(e.end_at,     'infinity'::timestamptz)) AS window_end,
           s.status AS sub_status,
           s.next_retry_at,
           GREATEST(s.current_period_ends_at, s.ended_at) AS paid_through,
           p.id AS payment_id,
           p.status AS payment_status,
           COALESCE((SELECT max(r.purchased_at)
                       FROM openrails.payments r
                      WHERE r.merchant_id = e.merchant_id
                        AND r.refunded_payment_id = p.id),
                    p.purchased_at) AS refund_effective_at,
           (SELECT max(COALESCE(g.ends_at, 'infinity'::timestamptz))
              FROM openrails.grants g
             WHERE g.merchant_id = e.merchant_id
               AND g.customer_id = e.customer_id
               AND g.event = 'grant' AND g.kind = 'entitlement'
               AND g.starts_at <= now()
               AND (g.id = e.grant_id
                    OR (g.source_id = e.source_id::text
                        AND ((e.source_type = 'subscription' AND g.source_type = 'subscription')
                             OR (e.source_type = 'one_off' AND g.source_type = 'purchase'))))
               AND NOT EXISTS (
                   SELECT 1 FROM openrails.grants t
                   WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
                     AND t.event IN ('revoke', 'expire', 'supersede'))) AS grant_covered_until
    FROM openrails.entitlements e
    LEFT JOIN openrails.subscriptions s
           ON e.source_type = 'subscription' AND s.id = e.source_id AND s.merchant_id = e.merchant_id
    LEFT JOIN openrails.payments p
           ON e.source_type = 'one_off' AND p.id = e.source_id AND p.merchant_id = e.merchant_id
    WHERE e.source_type IN ('subscription', 'one_off')
),
spans AS (
    SELECT w.*,
           GREATEST(w.start_at,
                    CASE
                        WHEN w.source_type = 'subscription'
                            THEN COALESCE(w.paid_through, '-infinity'::timestamptz)
                        WHEN w.payment_status = 'completed'
                            THEN 'infinity'::timestamptz -- covers the window it created
                        WHEN w.payment_status = 'refunded'
                            THEN w.refund_effective_at
                        ELSE '-infinity'::timestamptz    -- missing/pending/failed payment: no coverage
                    END,
                    COALESCE(w.grant_covered_until, '-infinity'::timestamptz)) AS unpaid_from,
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
           WHEN sub_status = 'past_due' AND next_retry_at IS NOT NULL THEN 'sanctioned_dunning'
           WHEN sub_status = 'unknown' THEN 'awaiting_verification'
           ELSE 'unsanctioned'
       END AS cause,
       unpaid_from AS started_at,
       unpaid_until AS ended_at,
       (window_end > now()) AS open,
       (EXTRACT(EPOCH FROM (unpaid_until - unpaid_from)) / 86400.0)::double precision AS days
FROM spans
WHERE unpaid_from < unpaid_until;

COMMENT ON VIEW openrails.freeloader_episodes IS
    '#690 episode analytics: spans of entitlement access NOT covered by payment (subscription paid-through snapshot, completed one_off payment, or a live matching grant). Open episodes (window still granting) end at now(). Causes label sanctioned unpaid access (sanctioned_dunning, awaiting_verification) vs failure (unsanctioned). Approximations: paid-through is the current-period snapshot (renewals overwrite it, healed historical lapses are invisible); coverage is contiguous-from-the-left (uncovered TAIL only); cause reads the sub''s CURRENT state; refund time falls back to the purchase time when no refund row links.';

-- orphaned_episodes: the mirror — spans where PAYMENT coverage existed
-- (subscription paid-through, or a completed one_off payment for a product
-- that promises entitlement windows) but NO entitlement window covered the
-- time. One row per (coverage source, uncovered tail). This is the
-- paying-without-access direction: derive.grant.missing's 47-open shape and
-- the legacy 95+2 paying-with-no-member-role cohort, as an interval measure.
-- Windows credited: any window (live OR historically closed — revoked/deleted
-- windows covered until their effective end) sourced by the same
-- subscription/payment. pending subs are excluded (nothing collected yet);
-- one_off coverage needs a finite access window (access_duration_hours) —
-- durable/ownership purchases project no entitlement window to miss.
CREATE VIEW openrails.orphaned_episodes WITH (security_invoker = true) AS
WITH cov AS (
    SELECT s.merchant_id,
           s.customer_id,
           'subscription'::text AS source_type,
           s.id AS source_id,
           s.product_id,
           COALESCE(s.current_period_starts_at, s.started_at) AS cov_start,
           GREATEST(s.current_period_ends_at, s.ended_at) AS cov_end
    FROM openrails.subscriptions s
    JOIN openrails.products pd ON pd.id = s.product_id AND pd.merchant_id = s.merchant_id
    WHERE s.status <> 'pending'
      AND GREATEST(s.current_period_ends_at, s.ended_at) IS NOT NULL
      AND (   (pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb)
           OR (s.entitlements_spec_snapshot IS NOT NULL AND s.entitlements_spec_snapshot <> '{}'::jsonb))
    UNION ALL
    SELECT p.merchant_id,
           p.customer_id,
           'one_off'::text,
           p.id,
           pr.product_id,
           p.purchased_at,
           p.purchased_at + make_interval(hours => pr.access_duration_hours)
    FROM openrails.payments p
    JOIN openrails.prices pr ON pr.id = p.price_id AND pr.merchant_id = p.merchant_id
    JOIN openrails.products pd ON pd.id = pr.product_id AND pd.merchant_id = p.merchant_id
    WHERE p.status = 'completed'
      AND p.amount > 0
      AND p.subscription_id IS NULL
      AND pr.access_duration_hours IS NOT NULL
      AND pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb
),
spans AS (
    SELECT c.*,
           GREATEST(c.cov_start,
                    COALESCE((SELECT max(LEAST(COALESCE(e.revoked_at, 'infinity'::timestamptz),
                                               COALESCE(e.deleted_at, 'infinity'::timestamptz),
                                               COALESCE(e.end_at,     'infinity'::timestamptz)))
                                FROM openrails.entitlements e
                               WHERE e.merchant_id = c.merchant_id
                                 AND e.customer_id = c.customer_id
                                 AND e.source_type = c.source_type
                                 AND e.source_id = c.source_id
                                 AND e.start_at <= now()),
                             '-infinity'::timestamptz)) AS uncovered_from,
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
       (EXTRACT(EPOCH FROM (uncovered_until - uncovered_from)) / 86400.0)::double precision AS days
FROM spans
WHERE uncovered_from < uncovered_until;

COMMENT ON VIEW openrails.orphaned_episodes IS
    '#690 episode analytics, the mirror of freeloader_episodes: spans where payment coverage existed (subscription paid-through snapshot, or a completed one_off payment with a finite access window for an entitlement-promising product) but no entitlement window covered the time. Open episodes (paid-through still in the future) end at now(). Same approximations: paid-through is the current-period snapshot; window coverage is contiguous-from-the-left (uncovered TAIL only — a wrongly-early revocation shows as the tail from revoked_at to paid-through).';

GRANT SELECT ON openrails.freeloader_episodes TO openrails_app;
GRANT SELECT ON openrails.orphaned_episodes TO openrails_app;
