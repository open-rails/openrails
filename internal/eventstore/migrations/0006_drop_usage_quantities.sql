-- Tier positions and postings read PostgreSQL's running totals
-- (usage_totals), so the period quantities rollup goes.
DROP VIEW IF EXISTS {database}.usage_quantities_mv;
DROP TABLE IF EXISTS {database}.usage_quantities;
