-- parent: 3 sha256:cffc4a05bbf5e1f752f8de27f22c9419a334c4eb5993a1ce820321c796d2a530
-- Cadence definitions (#1069), one per concept, shared by triggers, fleet
-- analytics and the metrics registry. Go mirrors price_interval_label in
-- internal/shared/cadence; a database test pins the two together.
-- Existing price keys are external names and stay as they are.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE FUNCTION openrails.price_interval_label(p_hours integer, p_auto_renew boolean) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN NOT coalesce(p_auto_renew, false) OR p_hours IS NULL THEN 'onetime'
    WHEN p_hours = 168 THEN 'weekly'
    WHEN p_hours = 720 THEN 'monthly'
    WHEN p_hours = 2160 THEN 'quarterly'
    WHEN p_hours = 8760 THEN 'yearly'
    WHEN p_hours > 0 AND p_hours % 24 = 0 THEN (p_hours / 24)::text || 'd'
    ELSE p_hours::text || 'h'
END
$$;

COMMENT ON FUNCTION openrails.price_interval_label(p_hours integer, p_auto_renew boolean) IS 'Default price-key interval label: exact and collision-free (<n>h, or <n>d on whole days; weekly/monthly/quarterly/yearly only for exactly 168/720/2160/8760 hours).';

CREATE FUNCTION openrails.billing_cycle_label(p_hours integer) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN p_hours IS NULL THEN 'one_time'
    WHEN p_hours < 24 THEN 'hourly'
    WHEN p_hours < 48 THEN 'daily'
    WHEN p_hours < 336 THEN 'weekly'
    WHEN p_hours < 1440 THEN 'monthly'
    WHEN p_hours < 3600 THEN 'quarterly'
    WHEN p_hours < 6480 THEN 'semiannual'
    ELSE 'annual'
END
$$;

COMMENT ON FUNCTION openrails.billing_cycle_label(p_hours integer) IS 'Analytics cadence bucket for a price access window.';

CREATE FUNCTION openrails.monthly_normalized_amount(p_amount bigint, p_hours integer) RETURNS bigint
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN p_hours IS NULL OR p_hours <= 0 THEN 0
    WHEN p_hours >= 648 THEN round(p_amount::numeric / greatest(round(p_hours / 730.0), 1))::bigint
    ELSE round(p_amount::numeric * 730.0 / p_hours)::bigint
END
$$;

COMMENT ON FUNCTION openrails.monthly_normalized_amount(p_amount bigint, p_hours integer) IS 'The one MRR normalisation: windows of at least 27 days divide by their whole-month count; shorter windows scale by 730 hours per month.';

CREATE OR REPLACE FUNCTION openrails.prices_default_key() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    product_key text;
BEGIN
    IF NEW.key IS NOT NULL AND btrim(NEW.key) <> '' THEN
        RETURN NEW;
    END IF;
    SELECT key INTO product_key FROM openrails.products WHERE id = NEW.product_id AND merchant_id = NEW.merchant_id;
    IF product_key IS NULL THEN
        RAISE EXCEPTION 'prices_default_key: product % not found for price %', NEW.product_id, NEW.id;
    END IF;
    NEW.key := product_key || '-' || openrails.price_interval_label(NEW.access_duration_hours, NEW.auto_renew);
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION openrails.fleet_mrr_by_currency(p_exclude uuid) RETURNS TABLE(currency text, subscriptions bigint, monthly_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT pr.currency::text, count(*)::bigint,
           COALESCE(sum(openrails.monthly_normalized_amount(pr.amount, pr.access_duration_hours)), 0)::bigint
      FROM openrails.subscriptions s
      JOIN openrails.prices pr ON pr.id = s.price_id AND pr.merchant_id = s.merchant_id
     WHERE s.status = 'active' AND pr.auto_renew AND pr.access_duration_hours > 0
       AND (p_exclude IS NULL OR s.merchant_id <> p_exclude)
     GROUP BY pr.currency ORDER BY pr.currency;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_mrr_by_currency(p_exclude uuid) IS 'Fleet MRR per currency using monthly_normalized_amount, the dashboard mrr definition (or#861, #1069).';
