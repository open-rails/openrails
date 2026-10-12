-- Repair: none-needed A new column defaulting to no requirement and a widened field vocabulary: every stored row already satisfies them.
-- A product may sell only to customers who already hold its required
-- entitlements: an order line refuses one who lacks any of them.
ALTER TABLE billing.products ADD COLUMN required_entitlements text[] COLLATE "C" NOT NULL DEFAULT '{}';
ALTER TABLE billing.products ADD CONSTRAINT products_required_entitlements_check CHECK (
    cardinality(required_entitlements) <= 100 AND array_position(required_entitlements, NULL) IS NULL);
COMMENT ON COLUMN billing.products.required_entitlements IS 'Entitlement keys a customer must hold when ordering the product, in byte order; empty: anyone may buy it.';

ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK ((
    ((object = 'product'::text) AND (field = ANY (ARRAY['display_name'::text, 'description'::text, 'tier_group'::text, 'tier_rank'::text, 'archived'::text, 'ownership'::text, 'entitlements'::text, 'required_entitlements'::text, 'credit_grant'::text, 'rate_cards'::text])))
    OR ((object = 'price'::text) AND (field = ANY (ARRAY['currency'::text, 'unit_amount'::text, 'access_duration_hours'::text, 'billing_interval_hours'::text, 'trial_unit_amount'::text, 'trial_duration_hours'::text, 'customer_amount'::text, 'quantity'::text, 'archived'::text, 'psp_links'::text])))
    OR ((object = 'meter'::text) AND (field = ANY (ARRAY['event_type'::text, 'value_property'::text, 'aggregation'::text, 'unit'::text, 'group_by'::text])))));
