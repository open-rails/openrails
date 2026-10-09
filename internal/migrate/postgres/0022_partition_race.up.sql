-- parent: 21 sha256:0e79a42562a198854248f988ee8054e1be0ef8bc11f7480d83e0f4671bd9a56b
-- Writers crossing into a new month create its partitions at the same moment.
-- The loser of that race meets the winner's partition as a duplicate relation,
-- object or catalog row, depending on timing; each means the partition exists,
-- so every writer goes on. A partition belongs to the role that creates it,
-- the one OpenRails runs as.
CREATE OR REPLACE FUNCTION billing.ensure_month_partitions(p_table name, p_from timestamptz, p_through timestamptz) RETURNS integer
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET timezone TO 'UTC' SET lock_timeout TO '2s' AS $$
DECLARE
    parent regclass := to_regclass(quote_ident(p_table));
    parent_schema name;
    month_start timestamptz := date_trunc('month', p_from);
    partition name;
    created integer := 0;
BEGIN
    SELECT n.nspname INTO parent_schema
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = parent AND c.relkind = 'p';
    IF NOT FOUND THEN
        RAISE EXCEPTION '% is not a partitioned table', p_table USING ERRCODE='42809';
    END IF;
    WHILE month_start <= p_through LOOP
        partition := p_table || to_char(month_start, '"_y"YYYY"m"MM');
        IF to_regclass(format('%I.%I', parent_schema, partition)) IS NULL THEN
            BEGIN
                EXECUTE format('CREATE TABLE %I.%I (LIKE %I.%I INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES INCLUDING GENERATED INCLUDING STORAGE)',
                    parent_schema, partition, parent_schema, p_table);
                EXECUTE format('ALTER TABLE %I.%I ATTACH PARTITION %I.%I FOR VALUES FROM (%L) TO (%L)',
                    parent_schema, p_table, parent_schema, partition, month_start, month_start + interval '1 month');
                created := created + 1;
            EXCEPTION WHEN duplicate_table OR duplicate_object OR unique_violation THEN
                NULL; -- another session created it first
            END;
        END IF;
        month_start := month_start + interval '1 month';
    END LOOP;
    RETURN created;
END;
$$;
COMMENT ON FUNCTION billing.ensure_month_partitions(name, timestamptz, timestamptz) IS 'Creates the missing monthly partitions of a partitioned table covering [p_from, p_through]; a partition another session creates first counts as present. Returns how many it created.';
