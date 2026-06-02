-- =============================================================================
-- 042 DOWN — drop the cross-tenant platform audit log + break-glass grants.
-- =============================================================================

DROP TABLE IF EXISTS billing.platform_break_glass;
DROP TABLE IF EXISTS billing.platform_audit;
