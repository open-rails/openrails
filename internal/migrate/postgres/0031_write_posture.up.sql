-- parent: 30 sha256:a5f49ac1e6c1609723b3849130dbe0b721cf26b237b7fd27271cb43859a6a87a
-- Two copies of one billing book holding the same provider credentials both
-- bill the customers in it. book_identity records the database the book was
-- armed in: a copy (a dump restored elsewhere, a second schema) reads as
-- readonly until an operator arms it. merchant_write_posture holds one
-- merchant's provider writes: an exported merchant is readonly at its source,
-- a restored one at its destination, until armed.
CREATE TABLE billing.book_identity (
    singleton boolean NOT NULL,
    system_identifier text NOT NULL,
    database_oid oid NOT NULL,
    schema_oid oid NOT NULL,
    armed_by text NOT NULL,
    armed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT book_identity_singleton_check CHECK (singleton),
    CONSTRAINT book_identity_armed_by_check CHECK (armed_by <> '')
);
COMMENT ON TABLE billing.book_identity IS 'Global by design, one row: the PostgreSQL cluster (system identifier), database and schema this billing book was armed in. Every provider write reads it; anywhere else the book is a copy and is readonly until `openrails book arm`. A promoted physical replica keeps all three; a physical clone does too, so rotate PSP credentials before running one.';

ALTER TABLE ONLY billing.book_identity
    ADD CONSTRAINT book_identity_pkey PRIMARY KEY (singleton);

-- Whether this connection's database is the one the book was armed in.
CREATE FUNCTION billing.book_armed() RETURNS boolean
    LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RETURN EXISTS (SELECT 1 FROM billing.book_identity b, pg_control_system() s, pg_database d
        WHERE b.singleton AND b.system_identifier = s.system_identifier::text
          AND d.datname = current_database() AND b.database_oid = d.oid
          AND b.schema_oid = (SELECT c.relnamespace FROM pg_class c WHERE c.oid = 'billing.book_identity'::regclass));
END;
$$;

-- Records this connection's database as the book's one live copy.
CREATE FUNCTION billing.arm_book(p_armed_by text, p_armed_at timestamp with time zone) RETURNS void
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    INSERT INTO billing.book_identity (singleton, system_identifier, database_oid, schema_oid, armed_by, armed_at)
    SELECT true, s.system_identifier::text, d.oid, c.relnamespace, p_armed_by, p_armed_at
    FROM pg_control_system() s, pg_database d, pg_class c
    WHERE d.datname = current_database() AND c.oid = 'billing.book_identity'::regclass
    ON CONFLICT (singleton) DO UPDATE SET
        system_identifier = EXCLUDED.system_identifier,
        database_oid = EXCLUDED.database_oid,
        schema_oid = EXCLUDED.schema_oid,
        armed_by = EXCLUDED.armed_by,
        armed_at = EXCLUDED.armed_at;
END;
$$;

SELECT billing.arm_book('migration', now());

CREATE TABLE billing.merchant_write_posture (
    merchant_id uuid NOT NULL,
    mode text NOT NULL,
    reason text NOT NULL,
    set_by text NOT NULL,
    set_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_write_posture_mode_check CHECK (mode IN ('full', 'limited', 'readonly')),
    CONSTRAINT merchant_write_posture_reason_check CHECK (reason IN ('exported', 'restored', 'operator')),
    CONSTRAINT merchant_write_posture_set_by_check CHECK (set_by <> '')
);
COMMENT ON TABLE billing.merchant_write_posture IS 'One merchant''s provider write posture; no row is full. The effective mode is the lower of this and provider_write_mode. Export sets the source readonly and restore the destination; an operator arms it back to full.';

ALTER TABLE ONLY billing.merchant_write_posture
    ADD CONSTRAINT merchant_write_posture_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY billing.merchant_write_posture
    ADD CONSTRAINT merchant_write_posture_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;
