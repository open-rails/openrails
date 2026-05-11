ALTER TABLE billing.subscriptions
    ADD COLUMN IF NOT EXISTS entitlements_spec_snapshot JSONB,
    ADD COLUMN IF NOT EXISTS credits_spec_snapshot JSONB;

ALTER TABLE billing.payments
    ADD COLUMN IF NOT EXISTS entitlements_spec_snapshot JSONB,
    ADD COLUMN IF NOT EXISTS credits_spec_snapshot JSONB;

UPDATE billing.subscriptions sub
SET entitlements_spec_snapshot = COALESCE(sub.entitlements_spec_snapshot, prod.entitlements_spec),
    credits_spec_snapshot = COALESCE(sub.credits_spec_snapshot, prod.credits_spec)
FROM billing.products prod
WHERE sub.product_id = prod.id
  AND (sub.entitlements_spec_snapshot IS NULL OR sub.credits_spec_snapshot IS NULL);

UPDATE billing.payments pay
SET entitlements_spec_snapshot = COALESCE(pay.entitlements_spec_snapshot, prod.entitlements_spec),
    credits_spec_snapshot = COALESCE(pay.credits_spec_snapshot, prod.credits_spec)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE pay.price_id = price.id
  AND (pay.entitlements_spec_snapshot IS NULL OR pay.credits_spec_snapshot IS NULL);
