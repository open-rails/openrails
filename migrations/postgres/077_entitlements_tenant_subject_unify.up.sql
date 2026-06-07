-- =============================================================================
-- 077 - Unify entitlement tenant_subject_id to the deterministic scheme (#317)
--
-- Migration 074 backfilled billing.entitlements.tenant_subject_id with GENERATED
-- tenant_subjects ids under the 'openrails:legacy-user' issuer. Migrations 075/076
-- converge commerce and credit rows on the credits/self-service DETERMINISTIC
-- scheme: a self-service subject's tenant_subjects row id EQUALS its user UUID
-- (issuer 'openrails:self'). This migration aligns historical entitlement rows to
-- that scheme so a user's entitlement, subscription, and credit balance all
-- reference one payable subject id.
--
-- Only rows whose user_id is a UUID are remapped (the self-service case). Rows
-- with non-UUID external subjects keep their 074 legacy-user reference. The
-- entitlement append logic locks per user_id, so windows remain non-overlapping
-- after the remap and the no-overlap exclusion constraint still holds.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DO $$
DECLARE
    uuid_re CONSTANT TEXT := '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$';
BEGIN
    -- Materialize the deterministic self-service tenant_subjects rows.
    INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
    SELECT DISTINCT ent.user_id::uuid, ent.tenant_id, 'openrails:self', ent.user_id
      FROM billing.entitlements ent
     WHERE ent.user_id ~ uuid_re
    ON CONFLICT DO NOTHING;

    -- Point UUID-user entitlement rows at their own UUID (overriding any 074
    -- generated legacy-user id).
    UPDATE billing.entitlements ent
       SET tenant_subject_id = ent.user_id::uuid
     WHERE ent.user_id ~ uuid_re
       AND (ent.tenant_subject_id IS DISTINCT FROM ent.user_id::uuid);
END $$;
