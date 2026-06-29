-- products.entitlements_spec and products.credits_spec carry durations as bare
-- integers inside JSONB. Those were DAYS; duration is HOURS everywhere now (hard
-- cut). Convert stored values x24 and, for credit grants, fold the day fields
-- (expiry_days, and the legacy expires_days) into a single expiry_hours.
--
-- ponytail: the *_spec_snapshot columns on payments/subscriptions are immutable
-- audit records of what was granted at purchase time and are intentionally left
-- as-is — they are never re-read as a live duration source.

-- entitlements_spec: {entitlement: days|null} -> {entitlement: hours|null}
UPDATE openrails.products
SET entitlements_spec = (
    SELECT jsonb_object_agg(
        e.key,
        CASE WHEN jsonb_typeof(e.value) = 'number'
             THEN to_jsonb((e.value::text)::int * 24)
             ELSE e.value END
    )
    FROM jsonb_each(entitlements_spec) AS e
)
WHERE entitlements_spec IS NOT NULL AND entitlements_spec <> '{}'::jsonb;

-- credits_spec: each grant's expiry_days / legacy expires_days (DAYS) -> expiry_hours (HOURS).
UPDATE openrails.products
SET credits_spec = (
    SELECT jsonb_object_agg(
        g.key,
        (g.value - 'expiry_days' - 'expires_days')
        || CASE
             WHEN jsonb_typeof(COALESCE(g.value->'expiry_days', g.value->'expires_days')) = 'number'
             THEN jsonb_build_object('expiry_hours',
                    ((COALESCE(g.value->'expiry_days', g.value->'expires_days'))::text)::int * 24)
             ELSE '{}'::jsonb
           END
    )
    FROM jsonb_each(credits_spec) AS g
)
WHERE credits_spec IS NOT NULL AND credits_spec <> '{}'::jsonb;
