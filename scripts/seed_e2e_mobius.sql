-- Seed a minimal Mobius/NMI catalog for local E2E.
--
-- Requires psql variables:
--   :mobius_plan_id
--
-- Example:
--   psql ... -v mobius_plan_id="123" -f scripts/seed_e2e_mobius.sql

\set ON_ERROR_STOP on

DO $$
BEGIN
  IF current_setting('server_version_num')::int < 120000 THEN
    RAISE EXCEPTION 'postgres 12+ required';
  END IF;
END $$;

-- Create product (idempotent by slug)
INSERT INTO billing.products (slug, display_name, description, tier_group, tier_rank, is_active)
VALUES ('e2e_mobius', 'E2E Mobius Plan', 'Local E2E product for Mobius/NMI sandbox', 'e2e', 1, true)
ON CONFLICT (slug) DO UPDATE
SET display_name = EXCLUDED.display_name,
    description = EXCLUDED.description,
    tier_group = EXCLUDED.tier_group,
    tier_rank = EXCLUDED.tier_rank,
    is_active = true,
    updated_at = current_timestamp;

-- Create price (idempotent by unique constraint)
WITH p AS (
  SELECT id AS product_id FROM billing.products WHERE slug = 'e2e_mobius'
)
INSERT INTO billing.prices (product_id, slug, amount, currency, billing_cycle_days, processors, is_active)
SELECT
  p.product_id,
  'e2e_mobius_daily',
  999,
  'usd',
  1,
  jsonb_build_object('mobius', jsonb_build_object('plan_id', :'mobius_plan_id', 'provider', 'mobius')),
  true
FROM p
ON CONFLICT (product_id, amount, currency, billing_cycle_days) DO UPDATE
SET processors = EXCLUDED.processors,
    is_active = true,
    updated_at = current_timestamp;

-- Output IDs for copy/paste.
SELECT 'product_id' AS key, id::text AS value FROM billing.products WHERE slug='e2e_mobius'
UNION ALL
SELECT 'price_id' AS key, id::text AS value
FROM billing.prices
WHERE product_id = (SELECT id FROM billing.products WHERE slug='e2e_mobius')
  AND amount = 999 AND currency='usd' AND billing_cycle_days=1;

