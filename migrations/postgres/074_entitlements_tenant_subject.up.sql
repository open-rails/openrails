-- =============================================================================
-- 074 - Entitlement tenant-subject payable identity (#317)
--
-- Entitlement windows now carry the payable billing.tenant_subjects row used by
-- service-token reads. Legacy commerce/admin flows still keep user_id until the
-- wider subscriptions/payments hard cut lands, but service reads no longer
-- translate tenant_subject_id through user_id.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.entitlements
    ADD COLUMN IF NOT EXISTS tenant_subject_id UUID;

INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
SELECT DISTINCT ent.tenant_id, 'openrails:legacy-user', ent.user_id
  FROM billing.entitlements ent
 WHERE ent.tenant_subject_id IS NULL
   AND NOT EXISTS (
        SELECT 1
          FROM billing.tenant_subjects tsub
         WHERE tsub.tenant_id = ent.tenant_id
           AND tsub.subject = ent.user_id
   )
ON CONFLICT DO NOTHING;

WITH matched AS (
    SELECT ent.id AS entitlement_id,
           (
               SELECT tsub.id
                 FROM billing.tenant_subjects tsub
                WHERE tsub.tenant_id = ent.tenant_id
                  AND tsub.subject = ent.user_id
                ORDER BY (tsub.issuer = 'openrails:legacy-user') DESC,
                         tsub.created_at ASC,
                         tsub.id ASC
                LIMIT 1
           ) AS tenant_subject_id
      FROM billing.entitlements ent
     WHERE ent.tenant_subject_id IS NULL
)
UPDATE billing.entitlements ent
   SET tenant_subject_id = matched.tenant_subject_id
  FROM matched
 WHERE ent.id = matched.entitlement_id
   AND matched.tenant_subject_id IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_constraint
         WHERE conname = 'entitlements_tenant_subject_fk'
           AND conrelid = 'billing.entitlements'::regclass
    ) THEN
        ALTER TABLE billing.entitlements
            ADD CONSTRAINT entitlements_tenant_subject_fk
            FOREIGN KEY (tenant_subject_id)
            REFERENCES billing.tenant_subjects(id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_entitlements_tenant_subject_active_window
    ON billing.entitlements (tenant_id, tenant_subject_id, entitlement, start_at, end_at)
    WHERE tenant_subject_id IS NOT NULL
      AND revoked_at IS NULL
      AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_entitlements_tenant_subject_active
    ON billing.entitlements (tenant_id, tenant_subject_id, entitlement)
    WHERE tenant_subject_id IS NOT NULL
      AND revoked_at IS NULL
      AND deleted_at IS NULL
      AND end_at IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_constraint
         WHERE conname = 'entitlements_tenant_subject_no_overlap'
           AND conrelid = 'billing.entitlements'::regclass
    ) THEN
        ALTER TABLE billing.entitlements
            ADD CONSTRAINT entitlements_tenant_subject_no_overlap
            EXCLUDE USING gist (
                tenant_id          WITH =,
                tenant_subject_id  WITH =,
                entitlement        WITH =,
                period             WITH &&
            )
            WHERE (tenant_subject_id IS NOT NULL AND revoked_at IS NULL AND deleted_at IS NULL);
    END IF;
END $$;

COMMENT ON COLUMN billing.entitlements.tenant_subject_id IS
    'OpenRails payable tenant subject for this entitlement window. Legacy user_id remains only for pre-hard-cut commerce/admin compatibility.';
