-- parent: 9 sha256:3243b8fd0aea3306510c36e6b4ed22f05e325ebf27474fd61088be29e9f71102
-- Repair: preserve Entitlement names replace catalog duration maps. Already
-- issued grants remain unchanged. Historical one-time payment feature durations
-- are retained as private accepted evidence; subscription grants always used
-- the price access duration and need no per-feature duration state.
ALTER TABLE billing.products DROP CONSTRAINT products_entitlement_hours_nonnegative_check;
ALTER TABLE billing.products RENAME COLUMN entitlements_spec TO entitlements;
ALTER TABLE billing.subscriptions RENAME COLUMN entitlements_spec_snapshot TO entitlements_snapshot;
ALTER TABLE billing.payments RENAME COLUMN entitlements_spec_snapshot TO entitlements_snapshot;
ALTER INDEX billing.products_entitlements_spec_idx RENAME TO products_entitlements_idx;
ALTER TABLE billing.payments ADD COLUMN legacy_entitlement_hours jsonb;

UPDATE billing.payments payment
SET legacy_entitlement_hours = (
    SELECT jsonb_object_agg(feature.key, feature.value)
    FROM jsonb_each(payment.entitlements_snapshot) feature
    WHERE jsonb_typeof(feature.value) = 'number' AND (feature.value #>> '{}')::numeric > 0
)
FROM billing.prices price
WHERE price.merchant_id = payment.merchant_id AND price.id = payment.price_id
  AND payment.subscription_id IS NULL AND price.access_duration_hours IS NULL
  AND jsonb_typeof(payment.entitlements_snapshot) = 'object';

-- This is a representation change, not a catalog edit or a new benefit promise.
ALTER TABLE billing.products DISABLE TRIGGER catalog_authored_product;
ALTER TABLE billing.products DISABLE TRIGGER product_revision;
UPDATE billing.products SET entitlements = CASE
    WHEN entitlements IS NULL OR entitlements = 'null'::jsonb THEN '[]'::jsonb
    ELSE COALESCE((SELECT jsonb_agg(name ORDER BY name COLLATE "C") FROM jsonb_object_keys(entitlements) name), '[]'::jsonb) END;
ALTER TABLE billing.products ENABLE TRIGGER product_revision;
ALTER TABLE billing.products ENABLE TRIGGER catalog_authored_product;
UPDATE billing.subscriptions SET entitlements_snapshot =
    COALESCE((SELECT jsonb_agg(name ORDER BY name COLLATE "C") FROM jsonb_object_keys(entitlements_snapshot) name), '[]'::jsonb)
WHERE jsonb_typeof(entitlements_snapshot) = 'object';
UPDATE billing.payments SET entitlements_snapshot =
    COALESCE((SELECT jsonb_agg(name ORDER BY name COLLATE "C") FROM jsonb_object_keys(entitlements_snapshot) name), '[]'::jsonb)
WHERE jsonb_typeof(entitlements_snapshot) = 'object';

ALTER TABLE billing.products ALTER COLUMN entitlements SET DEFAULT '[]'::jsonb;
ALTER TABLE billing.products ALTER COLUMN entitlements SET NOT NULL;
ALTER TABLE billing.products ADD CONSTRAINT products_entitlements_list_check CHECK (
    jsonb_typeof(entitlements) = 'array' AND NOT jsonb_path_exists(entitlements, '$[*] ? (@.type() != "string")'));
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_entitlements_list_check CHECK (
    entitlements_snapshot IS NULL OR entitlements_snapshot = 'null'::jsonb OR
    (jsonb_typeof(entitlements_snapshot) = 'array' AND NOT jsonb_path_exists(entitlements_snapshot, '$[*] ? (@.type() != "string")')));
ALTER TABLE billing.payments ADD CONSTRAINT payments_entitlements_list_check CHECK (
    entitlements_snapshot IS NULL OR entitlements_snapshot = 'null'::jsonb OR
    (jsonb_typeof(entitlements_snapshot) = 'array' AND NOT jsonb_path_exists(entitlements_snapshot, '$[*] ? (@.type() != "string")')));
ALTER TABLE billing.payments ADD CONSTRAINT payments_legacy_entitlement_hours_check CHECK (
    legacy_entitlement_hours IS NULL OR (jsonb_typeof(legacy_entitlement_hours) = 'object' AND
    NOT jsonb_path_exists(legacy_entitlement_hours, '$.* ? (@.type() != "number" || @ <= 0 || @ > 2562047)')));
COMMENT ON COLUMN billing.products.entitlements IS 'Opaque entitlement names granted for the purchased access duration; canonical sorted JSON array.';
COMMENT ON COLUMN billing.subscriptions.entitlements_snapshot IS 'Accepted opaque entitlement names; NULL is unknown historical evidence and [] is an explicitly empty promise.';
COMMENT ON COLUMN billing.payments.entitlements_snapshot IS 'Accepted opaque entitlement names; NULL is unknown historical evidence and [] is an explicitly empty promise.';
COMMENT ON COLUMN billing.payments.legacy_entitlement_hours IS 'Private historical positive per-feature durations for previously accepted indefinite one-time purchases; absent for new purchases.';
