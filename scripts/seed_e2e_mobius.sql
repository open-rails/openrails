-- Seed a minimal Mobius/NMI catalog for local E2E.
--
-- Requires psql variables:
--   :mobius_plan_id
--   :mobius_recurring_amount
--   :mobius_one_off_amount
--
-- Example:
--   psql ... -v mobius_plan_id="123" -v mobius_recurring_amount="999" -v mobius_one_off_amount="499" -f scripts/seed_e2e_mobius.sql

\set ON_ERROR_STOP on

DO $$
BEGIN
  IF current_setting('server_version_num')::int < 120000 THEN
    RAISE EXCEPTION 'postgres 12+ required';
  END IF;
END $$;

-- Create product (idempotent by slug)
INSERT INTO billing.products (slug, display_name, description, tier_group, tier_rank, status)
VALUES ('e2e_mobius', 'E2E Mobius Plan', 'Local E2E product for Mobius/NMI sandbox', 'e2e', 1, 'active')
ON CONFLICT (slug) DO UPDATE
SET display_name = EXCLUDED.display_name,
    description = EXCLUDED.description,
    tier_group = EXCLUDED.tier_group,
    tier_rank = EXCLUDED.tier_rank,
    status = 'active',
    updated_at = current_timestamp;

-- Create recurring price (idempotent by financial-substance unique constraint).
-- There is no price slug: a price's identity is (product, amount, currency, cycle).
WITH p AS (
  SELECT id AS product_id FROM billing.products WHERE slug = 'e2e_mobius'
)
INSERT INTO billing.prices (product_id, amount, currency, billing_cycle_days, processors, status)
SELECT
  p.product_id,
  :mobius_recurring_amount,
  'usd',
  1,
  jsonb_build_object('mobius', jsonb_build_object('plan_id', :'mobius_plan_id', 'provider', 'mobius')),
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
  SELECT id AS product_id FROM billing.products WHERE slug = 'e2e_mobius'
), updated AS (
  UPDATE billing.prices price
  SET processors = jsonb_build_object('mobius', jsonb_build_object('provider', 'mobius')),
      status = 'active',
      updated_at = current_timestamp
  FROM p
  WHERE price.product_id = p.product_id
    AND price.amount = :mobius_one_off_amount
    AND price.currency = 'usd'
    AND price.billing_cycle_days IS NULL
  RETURNING price.id
)
INSERT INTO billing.prices (product_id, amount, currency, billing_cycle_days, processors, status)
SELECT
  p.product_id,
  :mobius_one_off_amount,
  'usd',
  NULL,
  jsonb_build_object('mobius', jsonb_build_object('provider', 'mobius')),
  'active'
FROM p
WHERE NOT EXISTS (SELECT 1 FROM updated);

-- Output IDs for copy/paste.
SELECT 'product_id' AS key, id::text AS value FROM billing.products WHERE slug='e2e_mobius'
UNION ALL
SELECT 'recurring_price_id' AS key, id::text AS value
FROM billing.prices
WHERE product_id = (SELECT id FROM billing.products WHERE slug='e2e_mobius')
  AND amount = :mobius_recurring_amount AND currency='usd' AND billing_cycle_days=1
UNION ALL
SELECT 'one_off_price_id' AS key, id::text AS value
FROM billing.prices
WHERE product_id = (SELECT id FROM billing.products WHERE slug='e2e_mobius')
  AND amount = :mobius_one_off_amount AND currency='usd' AND billing_cycle_days IS NULL;
