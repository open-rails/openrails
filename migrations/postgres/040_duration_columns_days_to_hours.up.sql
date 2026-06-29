-- Internal duration representation is hours, not days (continues #622). The Go
-- and sqlc layers already moved to hours (entitlement feature duration, the
-- per-account default credit expiry); this renames the two remaining storage
-- columns and converts existing values (x24) so the queries — which already
-- reference the *_hours names — match the schema. Without this, any read of
-- money_settings or product_entitlement_features references a column that does
-- not exist.
--
-- PostgreSQL rewrites any CHECK/constraint expressions onto the new column names
-- automatically on RENAME. max_spend_per_day is intentionally left alone — it is
-- a per-day spend RATE, not a duration.
ALTER TABLE openrails.product_entitlement_features RENAME COLUMN duration_days TO duration_hours;
ALTER TABLE openrails.money_settings RENAME COLUMN default_credit_expiry_days TO default_credit_expiry_hours;

UPDATE openrails.product_entitlement_features SET duration_hours = duration_hours * 24 WHERE duration_hours IS NOT NULL;
UPDATE openrails.money_settings SET default_credit_expiry_hours = default_credit_expiry_hours * 24 WHERE default_credit_expiry_hours IS NOT NULL;

COMMENT ON COLUMN openrails.product_entitlement_features.duration_hours IS 'entitlement grant window in HOURS; NULL = indefinite.';
COMMENT ON COLUMN openrails.money_settings.default_credit_expiry_hours IS 'per-account default credit-grant expiry in HOURS; NULL = no default.';
