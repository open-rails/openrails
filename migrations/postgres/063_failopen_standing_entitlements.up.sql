-- #691 fail-open entitlements: access ends only by proof.
--
-- Forward-only inversion of the LIVE projection: entitlement windows sourced
-- from AUTO-RENEW-priced subscriptions in NON-TERMINAL states (pending/active/
-- past_due/unknown) become STANDING (end_at = NULL). From now on those windows
-- are closed only by proven events (user cancel closure at period end,
-- FailMembership terminal, provider-confirmed death) — never by window math
-- when our own machinery (webhooks, converge, provider pulls) goes silent.
--
-- Conversion rule:
--   * only LIVE windows: not revoked, not deleted, end_at still in the future;
--   * only the LATEST live window per (merchant, customer, entitlement,
--     source_id) — older bounded windows are history and opening them would
--     overlap the newer ones;
--   * skipped when ANY other live window on the same (customer, entitlement)
--     timeline overlaps [start_at, infinity) — the entitlements_customer_no_
--     overlap GIST constraint must hold; such rows stay bounded and heal at
--     their next renewal push.
-- Cancelled/expired/failed subs are untouched: a cancelled sub's runway stays
-- bounded to its paid-through (including #731-imported cancelled-with-runway
-- rows). Bounded purchases (one-off/rental, non-auto-renew prices) untouched.

UPDATE openrails.entitlements ent
SET end_at = NULL,
    updated_at = now()
WHERE ent.id IN (
    SELECT DISTINCT ON (e.merchant_id, e.customer_id, e.entitlement, e.source_id) e.id
    FROM openrails.entitlements e
    JOIN openrails.subscriptions s
      ON s.id = e.source_id AND s.merchant_id = e.merchant_id
    JOIN openrails.prices p
      ON p.id = s.price_id AND p.merchant_id = s.merchant_id
    WHERE e.source_type = 'subscription'
      AND e.revoked_at IS NULL
      AND e.deleted_at IS NULL
      AND e.end_at IS NOT NULL
      AND e.end_at > now()
      AND p.auto_renew
      AND s.status IN ('pending', 'active', 'past_due', 'unknown')
      AND NOT EXISTS (
          SELECT 1 FROM openrails.entitlements o
          WHERE o.merchant_id = e.merchant_id
            AND o.customer_id = e.customer_id
            AND o.entitlement = e.entitlement
            AND o.id <> e.id
            AND o.revoked_at IS NULL
            AND o.deleted_at IS NULL
            AND o.period && tstzrange(e.start_at, 'infinity'::timestamptz, '[)')
      )
    ORDER BY e.merchant_id, e.customer_id, e.entitlement, e.source_id, e.end_at DESC
);
