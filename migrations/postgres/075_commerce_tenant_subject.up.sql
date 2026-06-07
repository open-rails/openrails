-- =============================================================================
-- 075 - Commerce-table tenant-subject payable identity (#317)
--
-- Extends the entitlements hard cut (074) to the remaining commerce tables that
-- still key payable identity by `user_id`: subscriptions, payments,
-- payment_methods, processor_customers, checkout_sessions, product_access_grants,
-- admin_grants, and notification_queue.
--
-- Each table gains a nullable `tenant_subject_id` referencing billing.tenant_subjects
-- and is backfilled to converge on ONE payable subject id with the credits/
-- self-service hot path:
--
--   * When user_id IS a UUID (self-hosted AuthKit users), the tenant_subjects row
--     id EQUALS that UUID (issuer 'openrails:self') — the same value
--     identity.TenantSubjectIDFromString(user.ID) already uses as the payable id —
--     so a subscription and a credit balance for the same user reference the same
--     subject. tenant_subject_id is set to user_id::uuid directly.
--   * When user_id is NOT a UUID (external/delegated subjects), a generated id is
--     used under the 'openrails:legacy-user' issuer, matching migration 074.
--
-- This is the additive step: writers populate tenant_subject_id via the same
-- scheme (internal/db/repo.EnsureTenantSubjectID); a later migration drops user_id
-- once every reader references the tenant subject. There are NO compatibility views.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DO $$
DECLARE
    tbl TEXT;
    uuid_re CONSTANT TEXT := '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$';
    commerce_tables TEXT[] := ARRAY[
        'subscriptions',
        'payments',
        'payment_methods',
        'processor_customers',
        'checkout_sessions',
        'product_access_grants',
        'admin_grants',
        'notification_queue'
    ];
BEGIN
    FOREACH tbl IN ARRAY commerce_tables
    LOOP
        -- 1. Add the nullable payable tenant-subject reference.
        EXECUTE format(
            'ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS tenant_subject_id UUID',
            tbl
        );

        -- 2a. UUID user_ids: materialize a tenant_subjects row whose id IS the user
        --     UUID, so commerce and credit rows share one payable subject.
        EXECUTE format($q$
            INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
            SELECT DISTINCT t.user_id::uuid, t.tenant_id, 'openrails:self', t.user_id
              FROM billing.%I t
             WHERE t.tenant_subject_id IS NULL
               AND t.user_id ~ %L
            ON CONFLICT DO NOTHING
        $q$, tbl, uuid_re);

        -- 2b. Non-UUID subjects: generated id keyed by the legacy-user issuer.
        EXECUTE format($q$
            INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
            SELECT DISTINCT t.tenant_id, 'openrails:legacy-user', t.user_id
              FROM billing.%I t
             WHERE t.tenant_subject_id IS NULL
               AND t.user_id IS NOT NULL
               AND t.user_id !~ %L
               AND NOT EXISTS (
                    SELECT 1 FROM billing.tenant_subjects tsub
                     WHERE tsub.tenant_id = t.tenant_id
                       AND tsub.subject = t.user_id
               )
            ON CONFLICT DO NOTHING
        $q$, tbl, uuid_re);

        -- 3a. UUID rows reference their own UUID as the payable subject id.
        EXECUTE format($q$
            UPDATE billing.%I t
               SET tenant_subject_id = t.user_id::uuid
             WHERE t.tenant_subject_id IS NULL
               AND t.user_id ~ %L
        $q$, tbl, uuid_re);

        -- 3b. Non-UUID rows reference their generated legacy-user subject.
        EXECUTE format($q$
            WITH matched AS (
                SELECT t.id AS row_id,
                       (
                           SELECT tsub.id
                             FROM billing.tenant_subjects tsub
                            WHERE tsub.tenant_id = t.tenant_id
                              AND tsub.subject = t.user_id
                            ORDER BY (tsub.issuer = 'openrails:legacy-user') DESC,
                                     tsub.created_at ASC,
                                     tsub.id ASC
                            LIMIT 1
                       ) AS tenant_subject_id
                  FROM billing.%I t
                 WHERE t.tenant_subject_id IS NULL
                   AND t.user_id IS NOT NULL
            )
            UPDATE billing.%I t
               SET tenant_subject_id = matched.tenant_subject_id
              FROM matched
             WHERE t.id = matched.row_id
               AND matched.tenant_subject_id IS NOT NULL
        $q$, tbl, tbl);

        -- 4. Foreign key to the payable identity table.
        EXECUTE format($q$
            DO $inner$
            BEGIN
                IF NOT EXISTS (
                    SELECT 1 FROM pg_constraint
                     WHERE conname = '%1$s_tenant_subject_fk'
                       AND conrelid = 'billing.%1$s'::regclass
                ) THEN
                    ALTER TABLE billing.%1$s
                        ADD CONSTRAINT %1$s_tenant_subject_fk
                        FOREIGN KEY (tenant_subject_id)
                        REFERENCES billing.tenant_subjects(id);
                END IF;
            END $inner$
        $q$, tbl);

        -- 5. Lookup index by payable tenant subject.
        EXECUTE format(
            'CREATE INDEX IF NOT EXISTS idx_%1$s_tenant_subject ON billing.%1$s (tenant_subject_id) WHERE tenant_subject_id IS NOT NULL',
            tbl
        );

        EXECUTE format($q$
            COMMENT ON COLUMN billing.%I.tenant_subject_id IS
                'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject. Legacy user_id remains until writers/readers are fully converted.'
        $q$, tbl);
    END LOOP;
END $$;
