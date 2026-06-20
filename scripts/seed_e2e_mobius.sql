-- Seed a minimal NMI catalog for local E2E using the mobius provider slug.
--
-- Requires psql variables:
--   :nmi_plan_id
--   :nmi_recurring_amount
--   :nmi_one_off_amount
--   :merchant_slug
--
-- Example:
--   psql ... -v nmi_plan_id="123" -v nmi_recurring_amount="999" -v nmi_one_off_amount="499" -f scripts/seed_e2e_mobius.sql

\set ON_ERROR_STOP on

DO $$
BEGIN
  IF current_setting('server_version_num')::int < 120000 THEN
    RAISE EXCEPTION 'postgres 12+ required';
  END IF;
END $$;

-- Create product (idempotent by merchant + slug)
WITH merchant AS (
  SELECT id AS merchant_id FROM openrails.merchants WHERE slug = :'merchant_slug'
), updated AS (
  UPDATE openrails.products product
  SET display_name = 'E2E NMI Plan',
      description = 'Local E2E product for the NMI sandbox',
      tier_group = 'e2e',
      tier_rank = 1,
      status = 'active',
      updated_at = current_timestamp
  FROM merchant
  WHERE product.merchant_id = merchant.merchant_id
    AND product.slug = 'e2e_mobius'
  RETURNING product.id
)
INSERT INTO openrails.products (merchant_id, slug, display_name, description, tier_group, tier_rank, status)
SELECT merchant.merchant_id, 'e2e_mobius', 'E2E NMI Plan', 'Local E2E product for the NMI sandbox', 'e2e', 1, 'active'
FROM merchant
WHERE NOT EXISTS (SELECT 1 FROM updated);

-- Create recurring price (idempotent by financial-substance unique constraint).
-- There is no price slug: a price's identity is (product, amount, currency, cycle).
WITH p AS (
  SELECT p.id AS product_id, p.merchant_id
  FROM openrails.products p
  JOIN openrails.merchants m ON m.id = p.merchant_id
  WHERE m.slug = :'merchant_slug' AND p.slug = 'e2e_mobius'
)
INSERT INTO openrails.prices (merchant_id, product_id, amount, currency, billing_cycle_days, processors, status)
SELECT
  p.merchant_id,
  p.product_id,
  :nmi_recurring_amount,
  'usd',
  1,
  jsonb_build_object('mobius', jsonb_build_object('plan_id', :'nmi_plan_id', 'provider', 'mobius')),
  'active'
FROM p
ON CONFLICT (product_id, amount, currency, billing_cycle_days) DO UPDATE
SET processors = EXCLUDED.processors,
    status = 'active',
    updated_at = current_timestamp;

-- Create one-off price used to prove saved-vault manual charges through
-- OpenRails checkout. PostgreSQL UNIQUE constraints treat NULL billing cycles as
-- distinct, so do this as UPDATE-or-INSERT instead of ON CONFLICT.
WITH p AS (
  SELECT p.id AS product_id, p.merchant_id
  FROM openrails.products p
  JOIN openrails.merchants m ON m.id = p.merchant_id
  WHERE m.slug = :'merchant_slug' AND p.slug = 'e2e_mobius'
), updated AS (
  UPDATE openrails.prices price
  SET processors = jsonb_build_object('mobius', jsonb_build_object('provider', 'mobius')),
      status = 'active',
      updated_at = current_timestamp
  FROM p
  WHERE price.product_id = p.product_id
    AND price.merchant_id = p.merchant_id
    AND price.amount = :nmi_one_off_amount
    AND price.currency = 'usd'
    AND price.billing_cycle_days IS NULL
  RETURNING price.id
)
INSERT INTO openrails.prices (merchant_id, product_id, amount, currency, billing_cycle_days, processors, status)
SELECT
  p.merchant_id,
  p.product_id,
  :nmi_one_off_amount,
  'usd',
  NULL,
  jsonb_build_object('mobius', jsonb_build_object('provider', 'mobius')),
  'active'
FROM p
WHERE NOT EXISTS (SELECT 1 FROM updated);

-- Output IDs for copy/paste.
SELECT 'product_id' AS key, p.id::text AS value
FROM openrails.products p
JOIN openrails.merchants m ON m.id = p.merchant_id
WHERE m.slug = :'merchant_slug' AND p.slug='e2e_mobius'
UNION ALL
SELECT 'recurring_price_id' AS key, id::text AS value
FROM openrails.prices
WHERE product_id = (SELECT p.id FROM openrails.products p JOIN openrails.merchants m ON m.id = p.merchant_id WHERE m.slug = :'merchant_slug' AND p.slug='e2e_mobius')
  AND amount = :nmi_recurring_amount AND currency='usd' AND billing_cycle_days=1
UNION ALL
SELECT 'one_off_price_id' AS key, id::text AS value
FROM openrails.prices
WHERE product_id = (SELECT p.id FROM openrails.products p JOIN openrails.merchants m ON m.id = p.merchant_id WHERE m.slug = :'merchant_slug' AND p.slug='e2e_mobius')
  AND amount = :nmi_one_off_amount AND currency='usd' AND billing_cycle_days IS NULL;
