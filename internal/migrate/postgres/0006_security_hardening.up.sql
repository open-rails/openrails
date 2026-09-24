-- parent: 5 sha256:f7aca8e4d1ad1364afa2558de8405165346db24fa3147c82e638111ae398046a
-- SEC-26: an engine upgrade is an initial_membership operation naming the
-- membership it replaces. It joins the one-unresolved-tier-change-per-
-- subscription index, so two concurrent upgrades cannot both charge.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

DROP INDEX openrails.uq_rail_intents_tier_change_subscription;

CREATE UNIQUE INDEX uq_rail_intents_tier_change_subscription ON openrails.rail_intents(merchant_id, subscription_id)
WHERE intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
  AND subscription_id IS NOT NULL
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');

-- A negative access duration was granted as permanent while offer discovery
-- and the duplicate-purchase guard did not treat it so. Hours are 0/NULL
-- (permanent) or positive.
ALTER TABLE openrails.products ADD CONSTRAINT products_entitlement_hours_nonnegative
    CHECK (NOT jsonb_path_exists(coalesce(entitlements_spec, '{}'::jsonb), '$.* ? (@.type() == "number" && @ < 0)'));
