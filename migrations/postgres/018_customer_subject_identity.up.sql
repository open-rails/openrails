-- #539: customer identity is merchant + stable host/AuthKit UUID subject.
-- Issuer remains audit/last-seen source metadata only.

DO $$
DECLARE
    fk record;
BEGIN
    -- Native/embedded rows already use id == subject UUID. Backfill subject so
    -- reverse lookups and the new natural key see those rows.
    UPDATE openrails.customers
       SET subject = id::text
     WHERE subject IS NULL;

    -- For old delegated rows that minted a generated id for (issuer, subject),
    -- move references onto the canonical subject UUID row. Non-UUID legacy
    -- subjects cannot satisfy the new auth boundary and are left untouched.
    CREATE TEMP TABLE _openrails_customer_rewrites ON COMMIT DROP AS
    SELECT c.id AS old_id,
           c.subject::uuid AS new_id,
           c.merchant_id,
           c.issuer,
           c.subject,
           c.created_at,
           c.last_seen_at
      FROM openrails.customers c
     WHERE c.subject ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       AND c.id <> c.subject::uuid;

    INSERT INTO openrails.customers (id, merchant_id, issuer, subject, created_at, last_seen_at)
    SELECT DISTINCT ON (new_id)
           new_id,
           merchant_id,
           issuer,
           subject,
           created_at,
           last_seen_at
      FROM _openrails_customer_rewrites
     ORDER BY new_id, last_seen_at DESC
    ON CONFLICT (id) DO UPDATE SET
           subject = EXCLUDED.subject,
           issuer = COALESCE(EXCLUDED.issuer, openrails.customers.issuer),
           last_seen_at = GREATEST(openrails.customers.last_seen_at, EXCLUDED.last_seen_at);

    FOR fk IN
        SELECT con.conrelid::regclass AS table_name,
               att.attname AS column_name
          FROM pg_constraint con
          JOIN unnest(con.conkey) WITH ORDINALITY AS ck(attnum, ord) ON true
          JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ck.attnum
         WHERE con.contype = 'f'
           AND con.confrelid = 'openrails.customers'::regclass
    LOOP
        EXECUTE format(
            'UPDATE %s t SET %I = r.new_id FROM _openrails_customer_rewrites r WHERE t.%I = r.old_id',
            fk.table_name,
            fk.column_name,
            fk.column_name
        );
    END LOOP;

    DELETE FROM openrails.customers c
     USING _openrails_customer_rewrites r
     WHERE c.id = r.old_id;
END $$;

DROP INDEX IF EXISTS openrails.uq_customers_merchant_org;
DROP INDEX IF EXISTS openrails.uq_customers_merchant_issuer_subject;

CREATE UNIQUE INDEX IF NOT EXISTS uq_customers_merchant_subject
    ON openrails.customers (merchant_id, subject)
    WHERE subject IS NOT NULL;

COMMENT ON TABLE openrails.customers IS 'OpenRails payable identity. Customer identity is merchant_id plus the host/AuthKit stable UUID subject; id is that payable UUID. issuer is audit/last-seen source only.';
COMMENT ON COLUMN openrails.customers.org_id IS 'Deprecated customer identity metadata. Merchant ownership is represented by merchant_id; org/issuer do not key customers.';
COMMENT ON COLUMN openrails.customers.issuer IS 'Audit/last-seen source issuer for delegated/remote customer touches. Not part of customer identity.';
COMMENT ON COLUMN openrails.customers.subject IS 'Host/AuthKit stable UUID subject. Natural key is (merchant_id, subject); issuer does not participate.';
