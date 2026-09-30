-- parent: 32 sha256:0fc10970fb3d371693ad0b51ad2d913d2538c651751a90e4732592394b59baca
-- #1116: each rebill cycle with what its attempts decided: the first attempt,
-- the attempt that collected it (the first approval), what collected it, and
-- when the cycle closes. Decline metrics and the cycle read API read this one
-- derivation.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE VIEW openrails.rebill_cycle_facts WITH (security_invoker = true) AS
SELECT c.merchant_id, c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency,
       c.missed_at, c.miss_reason, c.created_at,
       f.category AS first_category, f.reason AS first_reason, f.attempted_at AS first_at,
       w.id AS won_attempt_id, w.kind AS won_kind, w.source AS won_source, w.attempted_at AS won_at, w.ordinal AS won_ordinal,
       (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) AS first_failed,
       CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
            WHEN f.category IS NULL THEN 'pending'
            WHEN f.category = 'approved' THEN 'approved'
            WHEN f.category = 'system_error' THEN 'error'
            ELSE 'declined' END AS first_outcome,
       -- Collected, the subscription cancelled, or the dunning window (at most
       -- 14 days) passed: whichever comes first.
       LEAST(w.attempted_at, CASE WHEN s.cancelled_at IS NOT NULL THEN GREATEST(s.cancelled_at, c.due_at) END, c.due_at + interval '15 days') AS closed_at,
       CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) THEN ''
            WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
            WHEN EXISTS (SELECT 1 FROM openrails.payment_method_updates u
                          WHERE u.merchant_id = c.merchant_id AND u.payment_method_id = w.payment_method_id AND u.kind = 'updated'
                            AND u.at >= COALESCE(c.missed_at, f.attempted_at) AND u.at <= w.attempted_at) THEN 'updated_card'
            WHEN w.kind = 'customer_retry' THEN 'customer_retry'
            ELSE 'dunning_retry' END AS recovered_by
  FROM openrails.rebill_cycles c
  LEFT JOIN LATERAL (SELECT a.category, a.reason, a.attempted_at FROM openrails.payment_attempts a
                      WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
  LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id,
                            (SELECT count(*) FROM openrails.payment_attempts b
                              WHERE b.merchant_id = c.merchant_id AND b.cycle_id = c.id AND (b.attempted_at, b.id) <= (a.attempted_at, a.id)) AS ordinal
                       FROM openrails.payment_attempts a
                      WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id AND a.category = 'approved'
                      ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
  LEFT JOIN openrails.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id;

COMMENT ON VIEW openrails.rebill_cycle_facts IS '#1116 each rebill cycle with its first attempt, the attempt that collected it and when it closes (collected, cancelled, or 15 days past due). A cycle is open until closed_at, lost when closed uncollected.';
