-- =============================================================================
-- 077 (down) - no-op (#317)
--
-- The unification remaps tenant_subject_id to the deterministic self-service id
-- and is not meaningfully reversible (the prior generated ids are not retained).
-- The materialized tenant_subjects rows are left in place; other tables reference
-- them. Rolling back the FK/columns is handled by 075/076 down migrations.
-- =============================================================================

SELECT 1;
