SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DO $$
DECLARE
    col record;
BEGIN
    FOR col IN
        SELECT c.table_name
          FROM information_schema.columns c
          JOIN pg_class pc ON pc.relname = c.table_name
          JOIN pg_namespace pn ON pn.oid = pc.relnamespace AND pn.nspname = 'openrails'
         WHERE c.table_schema = 'openrails'
           AND c.column_name = 'currency'
           AND pc.relkind = 'r'
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I DROP CONSTRAINT IF EXISTS %I',
                       col.table_name, col.table_name || '_currency_shape');
    END LOOP;
END
$$;

DROP INDEX IF EXISTS openrails.uq_catalog_drift_open;
CREATE UNIQUE INDEX uq_catalog_drift_open ON openrails.catalog_drift_events USING btree (rail, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE (resolved_at IS NULL);

DROP INDEX IF EXISTS openrails.uq_checkout_sessions_merchant_psp_reference;
DROP INDEX IF EXISTS openrails.uq_checkout_sessions_merchant_psp_transaction;

CREATE UNIQUE INDEX checkout_sessions_rail_reference_idx
    ON openrails.checkout_sessions USING btree (rail, reference) WHERE (reference IS NOT NULL);
CREATE UNIQUE INDEX checkout_sessions_rail_transaction_id_idx
    ON openrails.checkout_sessions USING btree (rail, transaction_id) WHERE (transaction_id IS NOT NULL);
