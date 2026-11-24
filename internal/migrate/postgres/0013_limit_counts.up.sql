-- A limit counts meters instead of pricing them in its unit: counts
-- [{meter, weight, per, filter}] add each event's quantity × weight ÷ per,
-- rounded half up. A per-unit rate card becomes one count, a matrix one count
-- per cell whose filter names the cell; a cell or card that counts nothing is
-- dropped. A card counts cannot express refuses the migration.
DO $$
DECLARE refused record;
BEGIN
    SELECT l.merchant_id, l.key INTO refused
    FROM billing.catalog_limits l, jsonb_array_elements(l.rate_cards) card
    WHERE card->'price'->>'model' IS DISTINCT FROM 'per_unit'
       OR coalesce(card->'price'->'per_unit'->>'round', 'half_up') <> 'half_up'
       OR coalesce(card->'price'->'per_unit'->>'maximum_amount', '0')::numeric <> 0
       OR EXISTS (SELECT 1 FROM jsonb_each(coalesce(card->'price'->'per_unit'->'matrix'->'cells', '{}'::jsonb)) cell
                  WHERE coalesce(cell.value->>'maximum_amount', '0')::numeric <> 0)
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'limit % of merchant % has a rate card counts cannot express (only per_unit, rounded half up, without maximum_amount): set its rate_cards to such cards or [] and migrate again', refused.key, refused.merchant_id
            USING ERRCODE = '23514';
    END IF;
END $$;

ALTER TABLE billing.catalog_limits ADD COLUMN counts jsonb NOT NULL DEFAULT '[]'::jsonb;
UPDATE billing.catalog_limits l SET counts = (
    SELECT coalesce(jsonb_agg(c.entry ORDER BY c.ord, c.cell), '[]'::jsonb)
    FROM (
        SELECT card.ord, '' AS cell,
               jsonb_strip_nulls(jsonb_build_object('meter', card.value->>'meter',
                   'weight', (card.value->'price'->'per_unit'->>'unit_amount')::bigint,
                   'per', coalesce((card.value->'price'->'per_unit'->>'divide_by')::bigint, 1),
                   'filter', card.value->'filter')) AS entry
        FROM jsonb_array_elements(l.rate_cards) WITH ORDINALITY card(value, ord)
        WHERE card.value->'price'->'per_unit'->'matrix' IS NULL
          AND coalesce((card.value->'price'->'per_unit'->>'unit_amount')::bigint, 0) > 0
        UNION ALL
        SELECT card.ord, cell.key,
               jsonb_build_object('meter', card.value->>'meter',
                   'weight', (cell.value->>'unit_amount')::bigint,
                   'per', coalesce((card.value->'price'->'per_unit'->>'divide_by')::bigint, 1),
                   'filter', coalesce(card.value->'filter', '{}'::jsonb)
                       || jsonb_build_object(card.value->'price'->'per_unit'->'matrix'->>'dimension', jsonb_build_array(cell.key))) AS entry
        FROM jsonb_array_elements(l.rate_cards) WITH ORDINALITY card(value, ord),
             jsonb_each(card.value->'price'->'per_unit'->'matrix'->'cells') cell
        WHERE coalesce((cell.value->>'unit_amount')::bigint, 0) > 0
          AND (card.value->'filter'->(card.value->'price'->'per_unit'->'matrix'->>'dimension') IS NULL
               OR card.value->'filter'->(card.value->'price'->'per_unit'->'matrix'->>'dimension') @> jsonb_build_array(cell.key))
    ) c
)
WHERE l.rate_cards <> '[]'::jsonb;
ALTER TABLE billing.catalog_limits ALTER COLUMN counts DROP DEFAULT;
ALTER TABLE billing.catalog_limits DROP CONSTRAINT catalog_limits_currency_rate_cards_check;
ALTER TABLE billing.catalog_limits DROP COLUMN rate_cards;
ALTER TABLE billing.catalog_limits ADD CONSTRAINT catalog_limits_counts_check CHECK (jsonb_typeof(counts) = 'array');
ALTER TABLE billing.catalog_limits ADD CONSTRAINT catalog_limits_currency_counts_check CHECK (unit ~ '^[a-z]' OR counts = '[]'::jsonb);
COMMENT ON TABLE billing.catalog_limits IS 'Usage limits the merchant declares: what a limit counts (its unit and counts), when its window resets, and how a request that crosses it is admitted. Capacity comes from products (catalog_product_limits) or a customer override. The built-in limit balance is never declared.';
COMMENT ON COLUMN billing.catalog_limits.unit IS 'A named unit (lowercase) or a currency code. A currency limit counts the rated amount and has no counts.';
COMMENT ON COLUMN billing.catalog_limits.counts IS 'How usage counts toward the limit: each count (meter, weight, per, filter) adds an event''s quantity × weight ÷ per, rounded half up, in declaration order.';

-- A limit's owned field rate_cards is counts.
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
UPDATE billing.catalog_field_owners SET field = 'counts' WHERE object = 'limit' AND field = 'rate_cards';
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK (
    (object = 'product' AND field IN ('display_name', 'description', 'tier_group', 'tier_rank', 'archived', 'ownership', 'entitlements', 'required_entitlements', 'credit_grant', 'rate_cards', 'limits'))
    OR (object = 'price' AND field IN ('currency', 'unit_amount', 'access_duration_hours', 'billing_interval_hours', 'billing_interval_months', 'provider_access_default', 'trial_unit_amount', 'trial_duration_hours', 'customer_amount', 'quantity', 'archived', 'sell_via', 'provider'))
    OR (object = 'meter' AND field IN ('event_type', 'value_property', 'aggregation', 'unit', 'group_by'))
    OR (object = 'limit' AND field IN ('unit', 'reset', 'overrun', 'counts'))
);
