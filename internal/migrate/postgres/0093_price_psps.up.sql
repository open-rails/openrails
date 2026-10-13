-- Repair: none-needed Renames in place: field rows naming psp_links are rewritten before the vocabulary narrows, and removing form_name keeps every configuration an object.
-- A price declares its PSPs in one psps map, so the owned price field is
-- psps. A CCBill FlexForm is addressed by its flex_id alone: form_name goes.
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
UPDATE billing.catalog_field_owners SET field = 'psps' WHERE object = 'price' AND field = 'psp_links';
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK ((
    ((object = 'product'::text) AND (field = ANY (ARRAY['display_name'::text, 'description'::text, 'tier_group'::text, 'tier_rank'::text, 'archived'::text, 'ownership'::text, 'entitlements'::text, 'required_entitlements'::text, 'credit_grant'::text, 'rate_cards'::text])))
    OR ((object = 'price'::text) AND (field = ANY (ARRAY['currency'::text, 'unit_amount'::text, 'access_duration_hours'::text, 'billing_interval_hours'::text, 'trial_unit_amount'::text, 'trial_duration_hours'::text, 'customer_amount'::text, 'quantity'::text, 'archived'::text, 'psps'::text])))
    OR ((object = 'meter'::text) AND (field = ANY (ARRAY['event_type'::text, 'value_property'::text, 'aggregation'::text, 'unit'::text, 'group_by'::text])))));

UPDATE billing.price_psp_bindings SET configuration = configuration - 'form_name' WHERE configuration ? 'form_name';
