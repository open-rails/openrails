-- A balance is low once what the customer may spend is at or under an
-- amount: the customer's own, else the merchant's per currency, else none.
-- The usage-rate estimate and the runway line are gone.

ALTER TABLE billing.balance_watches DROP COLUMN usage_rate_per_hour, DROP COLUMN rate_sampled_at;

-- A customer that set only a runway inherits the merchant's amount again.
DELETE FROM billing.customer_low_balance_lines WHERE amount IS NULL;
ALTER TABLE billing.customer_low_balance_lines
    DROP CONSTRAINT customer_low_balance_lines_set_check,
    DROP COLUMN runway_hours,
    ALTER COLUMN amount SET NOT NULL;
COMMENT ON TABLE billing.customer_low_balance_lines IS 'A customer''s own low-balance amount per currency: its balance is low once what it may spend is at or under it; 0 turns the notice off. A customer with no row inherits the merchant''s settings.balance_notices.';

-- Each stored low line was a runway's or one unit under the amount: the next
-- billing pass looks at every watch with one again.
UPDATE billing.balance_watches SET next_check_at = to_timestamp(0), updated_at = now()
WHERE low_line_amount IS NOT NULL;

-- Balance events no longer carry a usage rate or a runway end.
UPDATE billing.host_outbox SET data = data - 'usage_rate_per_hour' - 'runway_ends_at'
WHERE event_type IN ('balance.low', 'balance.depleted');

CREATE OR REPLACE FUNCTION billing.event_log_data(value jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE key text; prefix text;
BEGIN
    value := COALESCE(value,'{}'::jsonb);
    -- Keep monetary integers lossless for JavaScript consumers, just like the
    -- existing typed billing API. Do not reinterpret arbitrary payload fields.
    FOREACH key IN ARRAY ARRAY['amount','total','overdue_amount','amount_floor',
      'available_amount','owed_amount','accrual_rate_per_hour',
      'low_balance_amount','old_amount','new_amount','amount_due'] LOOP
        IF jsonb_typeof(value->key)='number' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(value->>key));
        END IF;
    END LOOP;
    FOR key,prefix IN SELECT * FROM (VALUES
      ('payment_id','pay_'),('product_id','prod_'),('subscription_id','sub_'),
      ('invoice_id','in_'),('mandate_id','mdt_'),('price_migration_id','pmig_'),
      ('from_product_id','prod_'),('to_product_id','prod_')) AS ids(key,prefix) LOOP
        IF value->>key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(prefix||(value->>key)));
        END IF;
    END LOOP;
    RETURN value;
END;
$_$;
