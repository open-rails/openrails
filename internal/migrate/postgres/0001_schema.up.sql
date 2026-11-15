-- OpenRails PostgreSQL schema. Objects are authored in `billing`, the default
-- schema; the migrator relocates them to any other configured schema.
-- AuthKit and River are migrated separately by their own libraries.
--
-- Tables follow foreign-key order. Each is followed by its comments, the checks
-- that cannot be inline, its indexes, then its triggers with the trigger
-- functions only it uses. A foreign key that closes a cycle is added after the
-- later of its two tables.

CREATE SCHEMA IF NOT EXISTS billing;

-- ---------------------------------------------------------------------------
-- Shared functions
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.current_merchant_id() RETURNS uuid
LANGUAGE sql STABLE SET search_path TO 'billing', 'pg_catalog' AS $$
    SELECT NULLIF(current_setting('openrails.merchant_id', true), '')::uuid
$$;
COMMENT ON FUNCTION billing.current_merchant_id() IS 'The request''s merchant from the openrails.merchant_id GUC, or NULL when unset. Used only by explicitly scoped SQL and restore transaction guards. Merely setting it does not filter other queries; their merchant predicates are mandatory.';
REVOKE ALL ON FUNCTION billing.current_merchant_id() FROM PUBLIC;

-- Financial facts are immutable to ordinary DML, the schema owner included.
CREATE FUNCTION billing.reject_immutable_billing_fact() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;

CREATE FUNCTION billing.guard_billing_fact_columns() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' OR (to_jsonb(NEW)-TG_ARGV) IS DISTINCT FROM (to_jsonb(OLD)-TG_ARGV) THEN
        RAISE EXCEPTION 'immutable billing facts in % cannot be rewritten', TG_TABLE_NAME USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Retention is the one path that deletes an append-only fact. The cleanup job
-- names the table in openrails.retention_table for its transaction, and the row must
-- be older than the period the trigger declares: TG_ARGV[0] is the timestamp
-- column the period counts from, TG_ARGV[1] the period.
CREATE FUNCTION billing.guard_retention_delete() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF current_setting('openrails.retention_table', true) IS NOT DISTINCT FROM TG_TABLE_NAME::text
       AND (to_jsonb(OLD)->>TG_ARGV[0])::timestamptz < now() - TG_ARGV[1]::interval THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% rows are deleted only by retention, % after %', TG_TABLE_NAME, TG_ARGV[1], TG_ARGV[0] USING ERRCODE='23514';
END;
$$;
COMMENT ON FUNCTION billing.guard_retention_delete() IS 'Refuses every DELETE except the retention sweep''s: the transaction names the table in openrails.retention_table and the row is past the period the trigger declares.';

-- Monthly range partitions, named <table>_yYYYYmMM on UTC month bounds. These
-- functions work from the calendar and the catalog; none reads a row. A
-- partition is built beside the table and attached, which takes SHARE UPDATE
-- EXCLUSIVE on the table: reads and writes carry on. lock_timeout bounds the
-- wait for the locks its foreign keys need on the referenced tables.
CREATE FUNCTION billing.month_partitions(p_table name) RETURNS TABLE(partition name, range_from timestamptz, range_to timestamptz)
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT c.relname,
           (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz,
           (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
      FROM pg_inherits i
      JOIN pg_class c ON c.oid = i.inhrelid
     WHERE i.inhparent = to_regclass(quote_ident(p_table))
     ORDER BY 2
$$;
COMMENT ON FUNCTION billing.month_partitions(p_table name) IS 'The partitions of a partitioned table with the range each holds, oldest first.';

CREATE FUNCTION billing.ensure_month_partitions(p_table name, p_from timestamptz, p_through timestamptz) RETURNS integer
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET "TimeZone" TO 'UTC' SET lock_timeout TO '2s' AS $$
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
COMMENT ON FUNCTION billing.ensure_month_partitions(p_table name, p_from timestamptz, p_through timestamptz) IS 'Creates the missing monthly partitions of a partitioned table covering [p_from, p_through]; a partition another session creates first counts as present. Returns how many it created.';

CREATE FUNCTION billing.drop_month_partitions(p_table name, p_before timestamptz) RETURNS integer
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET lock_timeout TO '2s' AS $$
DECLARE
    parent regclass := to_regclass(quote_ident(p_table));
    parent_schema name;
    child record;
    dropped integer := 0;
BEGIN
    SELECT n.nspname INTO parent_schema
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = parent AND c.relkind = 'p';
    IF NOT FOUND THEN
        RAISE EXCEPTION '% is not a partitioned table', p_table USING ERRCODE='42809';
    END IF;
    FOR child IN SELECT m.partition FROM billing.month_partitions(p_table) m WHERE m.range_to <= p_before LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I.%I', parent_schema, child.partition);
        dropped := dropped + 1;
    END LOOP;
    RETURN dropped;
END;
$$;
COMMENT ON FUNCTION billing.drop_month_partitions(p_table name, p_before timestamptz) IS 'Drops every partition of a partitioned table whose range ends at or before p_before. Dropping a partition is the only way its rows leave: row triggers do not fire. The drop locks the table briefly; lock_timeout gives up rather than queue writers behind a long reader, and the next pass tries again. Returns how many it dropped.';

-- Used by a generated column and a check.
CREATE FUNCTION billing.rail_subscription_key(rail text, ref text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT CASE WHEN rail = 'ccbill' THEN ltrim(ref, '0') ELSE ref END $$;
COMMENT ON FUNCTION billing.rail_subscription_key(rail text, ref text) IS 'The identity of a provider subscription id on its rail: a CCBill id without its leading zeros, any other rail''s id as spelled.';

CREATE FUNCTION billing.running_charge_total(p_rate_per_hour bigint, p_started_at timestamptz, p_at timestamptz) RETURNS bigint
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT div(p_rate_per_hour::numeric * greatest(floor(extract(epoch FROM p_at - p_started_at)), 0) * 2 + 3600, 7200)::bigint
$$;
COMMENT ON FUNCTION billing.running_charge_total(p_rate_per_hour bigint, p_started_at timestamptz, p_at timestamptz) IS 'What a running charge has charged by p_at: its rate times the whole seconds since p_started_at (never negative), over 3600, rounded half up.';

-- Reporting helpers.
CREATE FUNCTION billing.billing_cycle_label(p_hours integer) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE
    WHEN p_hours IS NULL THEN 'one_time'
    WHEN p_hours < 24 THEN 'hourly'
    WHEN p_hours < 48 THEN 'daily'
    WHEN p_hours < 336 THEN 'weekly'
    WHEN p_hours < 1440 THEN 'monthly'
    WHEN p_hours < 3600 THEN 'quarterly'
    WHEN p_hours < 6480 THEN 'semiannual'
    ELSE 'annual'
END
$$;
COMMENT ON FUNCTION billing.billing_cycle_label(p_hours integer) IS 'Analytics cadence bucket for a price access window.';

CREATE FUNCTION billing.monthly_normalized_amount(p_amount bigint, p_hours integer) RETURNS bigint
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE
    WHEN p_hours IS NULL OR p_hours <= 0 THEN 0
    WHEN p_hours >= 648 THEN round(p_amount::numeric / greatest(round(p_hours / 730.0), 1))::bigint
    ELSE round(p_amount::numeric * 730.0 / p_hours)::bigint
END
$$;
COMMENT ON FUNCTION billing.monthly_normalized_amount(p_amount bigint, p_hours integer) IS 'The one MRR normalisation: windows of at least 27 days divide by their whole-month count; shorter windows scale by 730 hours per month.';

-- Trigger helpers shared by tables in several sections.
CREATE FUNCTION billing.subscription_history_attribution(mid uuid, entity uuid, action text, version bigint) RETURNS jsonb
LANGUAGE plpgsql AS $$
DECLARE a jsonb := COALESCE(NULLIF(current_setting('openrails.subscription_history',true),''),'{}')::jsonb;
BEGIN
    IF a->>'merchant_id'=mid::text AND a->>'action'=action AND (
        (a->>'entity_id'=entity::text AND (a->>'version')::bigint=version)
        OR (action='update' AND (a->'versions'->>entity::text)::bigint=version)
    ) THEN RETURN a; END IF;
    RETURN '{}'::jsonb;
END;
$$;

CREATE FUNCTION billing.release_ended_ownership_claims() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_TABLE_NAME = 'subscriptions' THEN
        IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
            DELETE FROM billing.ownership_claims
            WHERE merchant_id = NEW.merchant_id AND holder_type = 'subscription' AND holder_id = NEW.id;
        END IF;
    ELSIF NEW.revoked_at IS NOT NULL OR NEW.deleted_at IS NOT NULL THEN
        DELETE FROM billing.ownership_claims
        WHERE merchant_id = NEW.merchant_id AND holder_type = 'product_access' AND holder_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- Merchants
-- ---------------------------------------------------------------------------

CREATE TABLE billing.merchants (
    id uuid NOT NULL DEFAULT uuidv7(),
    slug text NOT NULL,
    status text NOT NULL CHECK (status IN ('active', 'deleted')),
    permission_group_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    retired_at timestamptz,
    group_release_completed_at timestamptz,
    catalog_revision bigint NOT NULL DEFAULT 0 CHECK (catalog_revision >= 0),
    slug_changed_at timestamptz,
    entitlement_generation bigint NOT NULL DEFAULT 0,
    tenant_id uuid,
    PRIMARY KEY (id),
    CONSTRAINT merchants_tenant_group_check CHECK (tenant_id IS NULL OR (permission_group_id IS NOT NULL AND permission_group_id = id::text))
);
COMMENT ON TABLE billing.merchants IS 'Merchant directory: whose books a row goes on. Global by design; every other table is scoped by merchant_id. Carries billing state only, no authorization. Merchants are registered explicitly; there is no default merchant.';
COMMENT ON COLUMN billing.merchants.slug IS 'The merchant''s public name: unique among live rows and never equal to another merchant''s unexpired former name (merchant_slug_aliases). OpenRails owns it; AuthKit groups carry no name.';
COMMENT ON COLUMN billing.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id: a merchant IS a top-level `merchant` group, child of `root`. Bare `text`, NO FK into the auth schema. NULL for a host-owned merchant without a control plane.';
COMMENT ON COLUMN billing.merchants.slug_changed_at IS 'When the merchant was last renamed; NULL if never. The rename interval counts from here.';
COMMENT ON COLUMN billing.merchants.entitlement_generation IS 'Steps with every statement that changes a product''s keys: a cached key set from an older generation is stale.';
COMMENT ON COLUMN billing.merchants.tenant_id IS 'Explicit immutable business tenant associated one-to-one with this billing identity. NULL is the legacy unbound mode; never inferred from an issuer, name, default account tenant or request. The matching permission group has this merchant ID.';

CREATE UNIQUE INDEX merchants_permission_group_id_key ON billing.merchants (permission_group_id)
    WHERE permission_group_id IS NOT NULL;
CREATE INDEX merchants_retired_at_id_idx ON billing.merchants (retired_at, id)
    WHERE retired_at IS NOT NULL AND group_release_completed_at IS NULL;
CREATE UNIQUE INDEX merchants_slug_key ON billing.merchants (slug) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX merchants_tenant_id_key ON billing.merchants (tenant_id) WHERE tenant_id IS NOT NULL;

CREATE FUNCTION billing.create_event_log_stream() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
 INSERT INTO billing.eventlog_streams(source,scope) VALUES('openrails',NEW.id::text) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;

CREATE FUNCTION billing.discard_deleted_merchant_eventlog() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
 DELETE FROM billing.eventlog_pending WHERE source='openrails' AND scope=OLD.id::text;
 RETURN OLD;
END;
$$;

CREATE FUNCTION billing.guard_merchant_group_binding() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing' AS $$
BEGIN
    IF OLD.permission_group_id IS NOT NULL AND OLD.permission_group_id IS DISTINCT FROM NEW.permission_group_id THEN
        RAISE EXCEPTION 'merchant group binding is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

-- One name namespace across live names and aliases. Claims serialize per name
-- on a transaction lock, so a rename's alias and a concurrent claim of the same
-- name cannot both win.
CREATE FUNCTION billing.guard_merchant_name() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing' AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
            DELETE FROM billing.merchant_slug_aliases WHERE merchant_id = NEW.id;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
        IF OLD.slug = NEW.slug THEN
            RETURN NEW;
        END IF;
        PERFORM pg_advisory_xact_lock(1106, hashtext(least(OLD.slug, NEW.slug)));
        PERFORM pg_advisory_xact_lock(1106, hashtext(greatest(OLD.slug, NEW.slug)));
    ELSE
        PERFORM pg_advisory_xact_lock(1106, hashtext(NEW.slug));
    END IF;
    DELETE FROM billing.merchant_slug_aliases
     WHERE slug = NEW.slug AND (merchant_id = NEW.id OR expires_at <= now());
    IF EXISTS (SELECT 1 FROM billing.merchant_slug_aliases WHERE slug = NEW.slug) THEN
        RAISE EXCEPTION 'merchant name % is another merchant''s former name', NEW.slug
            USING ERRCODE = '23505', CONSTRAINT = 'merchant_slug_aliases_pkey';
    END IF;
    RETURN NEW;
END;
$$;
COMMENT ON FUNCTION billing.guard_merchant_name() IS 'A live merchant name is never another merchant''s unexpired former name; leaving the directory releases a merchant''s former names. Raises 23505 on merchant_slug_aliases_pkey.';
REVOKE ALL ON FUNCTION billing.guard_merchant_name() FROM PUBLIC;

CREATE FUNCTION billing.guard_merchant_restore() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing' AS $$
BEGIN
    IF OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at THEN
        RAISE EXCEPTION 'merchant retirement is irreversible' USING ERRCODE='23514';
    END IF;
    IF NEW.deleted_at IS NULL AND NEW.status='active' THEN

        IF (
        OLD.retired_at IS NOT NULL OR EXISTS (
            SELECT 1 FROM billing.maintenance_runs r
             WHERE r.merchant_id=OLD.id AND r.kind='merchant_purge'
               AND r.affected->>'database_purged'='true'
        )
    ) THEN
        RAISE EXCEPTION 'retired or purged merchant cannot be restored' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION billing.guard_merchant_restore() FROM PUBLIC;

CREATE FUNCTION billing.guard_merchant_tenant_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.tenant_id IS NOT NULL AND (
        NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR
        NEW.permission_group_id IS DISTINCT FROM OLD.permission_group_id
    ) THEN
        RAISE EXCEPTION 'merchant tenant binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER discard_deleted_merchant_eventlog BEFORE DELETE ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.discard_deleted_merchant_eventlog();
CREATE TRIGGER guard_merchant_name BEFORE INSERT OR UPDATE OF slug, deleted_at ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_name();
CREATE TRIGGER guard_merchant_restore BEFORE UPDATE ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_restore();
CREATE TRIGGER immutable_merchant_group_binding BEFORE UPDATE OF permission_group_id ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_group_binding();
CREATE TRIGGER merchant_event_log_stream AFTER INSERT ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.create_event_log_stream();
CREATE TRIGGER merchants_tenant_binding_immutable BEFORE UPDATE OF tenant_id, permission_group_id ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_tenant_binding();

CREATE TABLE billing.merchant_slug_aliases (
    slug text NOT NULL,
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (slug)
);
COMMENT ON TABLE billing.merchant_slug_aliases IS 'Former merchant names. An unexpired alias forwards to its merchant and blocks every other claim of the name; expires_at NULL keeps it forever. Written by a rename, removed when its merchant takes the name back, when it expires and is claimed, or when its merchant leaves the directory.';

CREATE INDEX merchant_slug_aliases_merchant_id_idx ON billing.merchant_slug_aliases (merchant_id);

CREATE TABLE billing.merchant_write_posture (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('full', 'limited', 'readonly')),
    reason text NOT NULL CHECK (reason IN ('exported', 'restored', 'operator')),
    set_by text NOT NULL CHECK (set_by <> ''),
    set_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.merchant_write_posture IS 'One merchant''s provider write posture; no row is full. The effective mode is the lower of this and provider_write_mode. Export sets the source readonly and restore the destination; an operator arms it back to full.';

CREATE TABLE billing.provisioning_tokens (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    name text NOT NULL,
    declared boolean NOT NULL,
    token_sha256 bytea NOT NULL CHECK (octet_length(token_sha256) = 32),
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CHECK (name = btrim(name) AND name <> '' AND octet_length(name) <= 128)
);
COMMENT ON TABLE billing.provisioning_tokens IS 'Bearer tokens a merchant''s directory presents at /scim/v2, stored as their SHA-256. Revoking deletes the row.';
COMMENT ON COLUMN billing.provisioning_tokens.declared IS 'The merchant declaration''s secrets.scim_token: at most one per merchant, replaced when the declaration changes it.';

CREATE UNIQUE INDEX provisioning_tokens_declared_key ON billing.provisioning_tokens (merchant_id)
    WHERE declared;
CREATE UNIQUE INDEX provisioning_tokens_token_sha256_key ON billing.provisioning_tokens (token_sha256);

CREATE TABLE billing.dashboard_configs (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    layout jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text,
    PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.dashboard_configs IS 'Per-merchant dashboard widget layout: [{id, title, viz(stat|line|area|bar|donut|table), query, grid{x,y,w,h}}]. Absent row = seeded default template (in code, not DB).';
COMMENT ON COLUMN billing.dashboard_configs.updated_by IS 'Acting principal (user id) of the last PUT; informational.';

CREATE TABLE billing.idempotency_keys (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    operation text NOT NULL,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    status text NOT NULL CHECK (status IN ('processing', 'succeeded', 'failed')),
    token uuid NOT NULL,
    claims bigint NOT NULL DEFAULT 1 CHECK (claims > 0),
    result jsonb,
    error text,
    lease_expires_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, operation, idempotency_key),
    CONSTRAINT idempotency_keys_identity_check CHECK (operation <> '' AND idempotency_key <> ''),
    CONSTRAINT idempotency_keys_result_check CHECK (status = 'succeeded' OR result IS NULL),
    CONSTRAINT idempotency_keys_expiry_check CHECK (expires_at >= lease_expires_at)
);
COMMENT ON TABLE billing.idempotency_keys IS 'One claim per (merchant, operation, key). processing = owned until lease_expires_at, then reclaimable by exactly one caller; succeeded = replay result; failed = reclaimable. token fences a superseded owner; claims counts claims. Rows past expires_at are deleted by retention. Retention: rows are deleted at expires_at.';

CREATE INDEX idempotency_keys_expires_at_idx ON billing.idempotency_keys (expires_at);

-- ---------------------------------------------------------------------------
-- Deployment-wide state
-- ---------------------------------------------------------------------------

CREATE TABLE billing.book_identity (
    singleton boolean NOT NULL CHECK (singleton),
    system_identifier text NOT NULL,
    database_oid oid NOT NULL,
    schema_oid oid NOT NULL,
    armed_by text NOT NULL CHECK (armed_by <> ''),
    armed_at timestamptz NOT NULL DEFAULT now(),
    book_id uuid NOT NULL DEFAULT uuidv7(),
    tagging_started_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton)
);
COMMENT ON TABLE billing.book_identity IS 'Global by design, one row: the PostgreSQL cluster (system identifier), database and schema this billing book was armed in. Every provider write reads it; anywhere else the book is a copy and is readonly until `openrails book arm`. A promoted physical replica keeps all three; a physical clone does too, so rotate PSP credentials before running one.';
COMMENT ON COLUMN billing.book_identity.book_id IS 'The book''s logical identity, written into every provider object OpenRails creates; kept by arm_book and by a dump and restore of the database.';
COMMENT ON COLUMN billing.book_identity.tagging_started_at IS 'When the book began tagging provider objects with book_id; an untagged object created before it predates the tag.';

CREATE FUNCTION billing.arm_book(p_armed_by text, p_armed_at timestamptz) RETURNS void
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

CREATE FUNCTION billing.book_armed() RETURNS boolean
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RETURN EXISTS (SELECT 1 FROM billing.book_identity b, pg_control_system() s, pg_database d
        WHERE b.singleton AND b.system_identifier = s.system_identifier::text
          AND d.datname = current_database() AND b.database_oid = d.oid
          AND b.schema_oid = (SELECT c.relnamespace FROM pg_class c WHERE c.oid = 'billing.book_identity'::regclass));
END;
$$;

SELECT billing.arm_book('migration', now());

CREATE TABLE billing.periodic_leases (
    kind text NOT NULL CHECK (kind <> ''),
    holder text NOT NULL CHECK (holder <> ''),
    expires_at timestamptz NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind)
);
COMMENT ON TABLE billing.periodic_leases IS 'Global by design: which fleet (River schema) queues a periodic kind''s pass for the database; one row per kind, taken over once expired, deleted 30 days after it expired.';
COMMENT ON COLUMN billing.periodic_leases.kind IS 'The periodic job''s id: its job kind, or the name its schedule gave.';
COMMENT ON COLUMN billing.periodic_leases.holder IS 'The holding fleet''s River schema; a fleet renews its own lease.';
COMMENT ON COLUMN billing.periodic_leases.expires_at IS 'When another fleet may take the kind over: a fraction of the period after the claim.';

CREATE TABLE billing.solana_crankers (
    name text NOT NULL CHECK (btrim(name) <> ''),
    public_key text NOT NULL CHECK (btrim(public_key) <> ''),
    pinned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (name)
);
COMMENT ON TABLE billing.solana_crankers IS 'Global by design: the deployment''s Solana crankers, each pinned to the public key it first had. A cranker whose key now differs is refused, so a Vault Transit rotation never silently becomes another cranker. A row is never rewritten.';
COMMENT ON COLUMN billing.solana_crankers.name IS 'The listed cranker without its secret: vault_transit:<key> or local_keypair:<public key>.';
COMMENT ON COLUMN billing.solana_crankers.public_key IS 'The public key the cranker had on its first use.';

-- ---------------------------------------------------------------------------
-- PSPs and custodians
-- ---------------------------------------------------------------------------

CREATE TABLE billing.custodians (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    key text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('basis_theory', 'hyperswitch', 'platform')),
    environment text NOT NULL CHECK (environment IN ('live', 'test')),
    account_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT custodians_id_kind_key UNIQUE (merchant_id, id, kind),
    CONSTRAINT custodians_nonempty_check CHECK (btrim(key) <> '' AND btrim(account_id) <> '')
);
COMMENT ON TABLE billing.custodians IS 'Custodian identities. A row is one merchant-owned account with a third-party card custodian, the target of the cards it holds, whose configuration (settings, credentials, archived) is the custodian document in a file or Vault; or, kind platform, the merchant''s partition of the deployment''s platform vault, which the engine records and no document configures. Custody is orthogonal to the rail: this says who holds the card, psps says who charges it.';
COMMENT ON COLUMN billing.custodians.key IS 'The custodian''s manifest key (merchants.<slug>.custodians.<key>) — the name a PSP entry references.';
COMMENT ON COLUMN billing.custodians.kind IS 'The custodian: basis_theory or hyperswitch (a merchant''s vendor account), or platform (the deployment''s platform vault). Same vocabulary as payment_methods.custodian, minus ''psp'' (which is the absence of a third-party custodian, not an account).';
COMMENT ON COLUMN billing.custodians.account_id IS 'The custodian-native identity: the vendor''s tenant id, operator-declared (there is no runtime whoami); for platform, <tenant id>/<merchant id>.';

CREATE UNIQUE INDEX custodians_key_key ON billing.custodians (merchant_id, lower(key));
CREATE UNIQUE INDEX custodians_kind_environment_account_id_key ON billing.custodians (kind, environment, account_id);

CREATE TABLE billing.psps (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    key text NOT NULL,
    rail text NOT NULL CHECK (rail IN ('nmi', 'ccbill', 'stripe', 'solana')),
    environment text NOT NULL,
    account_id text NOT NULL,
    credentials_validated_at timestamptz,
    webhook_endpoint_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    credential_fingerprint text CHECK (credential_fingerprint ~ '^[0-9a-f]{64}$'),
    credential_duplicate_at timestamptz,
    superseded_at timestamptz,
    live boolean NOT NULL GENERATED ALWAYS AS (environment IN ('live', 'mainnet')) STORED,
    webhook_digest text CHECK (webhook_digest ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT psps_id_rail_key UNIQUE (merchant_id, id, rail),
    CONSTRAINT psps_nonempty_check CHECK (btrim(key) <> '' AND btrim(account_id) <> ''),
    CONSTRAINT psps_environment_check CHECK (CASE WHEN (rail = 'solana') THEN (environment IN ('mainnet', 'devnet', 'testnet')) ELSE (environment IN ('live', 'test', 'sandbox')) END)
);
COMMENT ON TABLE billing.psps IS 'Merchant PSP identities. A row is one merchant-owned account on one rail, the target of every payment, subscription and card that names it, with what OpenRails discovers about it; its configuration (settings, credentials, archived) is the PSP document in a file or Vault. The rail vocabulary lives here only; every table that stores rail beside psp_id references (merchant_id, id, rail).';
COMMENT ON COLUMN billing.psps.key IS 'The merchant''s name for the PSP (e.g. mobius): the key of its document. One current PSP holds a key.';
COMMENT ON COLUMN billing.psps.environment IS 'The PSP''s declared environment, its rail''s own: nmi live, test or sandbox; stripe live or test; ccbill live or sandbox; solana mainnet, devnet or testnet.';
COMMENT ON COLUMN billing.psps.account_id IS 'Operator-declared account identity on the rail (Stripe acct_, NMI gateway id, CCBill account-subaccount, Solana signer address).';
COMMENT ON COLUMN billing.psps.credentials_validated_at IS 'When the provider last accepted the stored credentials; NULL when never checked.';
COMMENT ON COLUMN billing.psps.webhook_endpoint_id IS 'The provider webhook endpoint OpenRails manages for this PSP.';
COMMENT ON COLUMN billing.psps.credential_fingerprint IS 'HMAC-SHA256, under a key held in Vault, of the credential naming the gateway account (NMI security_key, Stripe secret_key). Cleared when the PSP archives.';
COMMENT ON COLUMN billing.psps.credential_duplicate_at IS 'When this PSP was found to declare a gateway account another live PSP on its rail already declares. Its credentials are not read, so it stays disarmed, until it is archived or given its own.';
COMMENT ON COLUMN billing.psps.superseded_at IS 'When OpenRails found the PSP''s key naming another account. A superseded PSP takes no new work and drains, as an archived one does.';
COMMENT ON COLUMN billing.psps.live IS 'The PSP''s money class: whether its environment moves real money (live, mainnet). A PSP''s identity is (rail, live, account_id); a sandbox merchant''s PSPs are non-live, a live merchant''s live.';
COMMENT ON COLUMN billing.psps.webhook_digest IS 'SHA-256 of the pinned Stripe API version, the handled event types and the webhook URL the PSP''s managed endpoint was last settled at; NULL until one is.';

CREATE INDEX psps_environment_rail_created_at_id_idx ON billing.psps (merchant_id, environment, rail, created_at DESC, id DESC);
CREATE UNIQUE INDEX psps_key_key ON billing.psps (merchant_id, lower(key)) WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX psps_live_credential_fingerprint_key ON billing.psps (rail, environment, credential_fingerprint)
    WHERE credential_duplicate_at IS NULL AND credential_fingerprint IS NOT NULL;
CREATE UNIQUE INDEX psps_rail_live_account_id_key ON billing.psps (rail, live, account_id);

CREATE TABLE billing.psp_refresh_watermarks (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    psp_id uuid NOT NULL,
    event_domain text NOT NULL,
    watermark_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, event_domain),
    CONSTRAINT psp_refresh_watermarks_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CHECK (event_domain IN ('events', 'applied_events', 'completed_events'))
);
COMMENT ON TABLE billing.psp_refresh_watermarks IS 'PSP refresh cursors: the exclusive lower bound of the next bounded event window, per (merchant, PSP, domain). A failed or partial provider read never advances watermark_at.';
COMMENT ON COLUMN billing.psp_refresh_watermarks.event_domain IS 'events is observed provider-window progress; applied_events is complete provider-window coverage with required financial receipts recovered; completed_events is a fully caught-up captured target. None is provider finality or a cross-database clone fence.';

CREATE TABLE billing.provider_catalog_objects (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('product', 'price')),
    ref text NOT NULL CHECK (ref <> '' AND octet_length(ref) <= 256),
    product_ref text,
    name text,
    active boolean NOT NULL,
    deleted_at timestamptz,
    amount text CHECK (amount IS NULL OR amount ~ '^[0-9]+(\.[0-9]+)?$'),
    currency text CHECK (currency IS NULL OR currency ~ '^[A-Z][A-Z0-9]{2,9}$'),
    billing_interval_hours integer,
    billing_interval_months integer,
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'object'),
    observed_at timestamptz NOT NULL,
    observed_via text NOT NULL CHECK (observed_via IN ('pull', 'webhook', 'read')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, kind, ref),
    CONSTRAINT provider_catalog_objects_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_catalog_objects_terms_check CHECK (
        kind = 'price'
        OR (product_ref IS NULL AND amount IS NULL AND currency IS NULL AND billing_interval_hours IS NULL AND billing_interval_months IS NULL)
    ),
    CONSTRAINT provider_catalog_objects_text_check CHECK ((product_ref IS NULL OR product_ref <> '') AND (name IS NULL OR name <> '')),
    CONSTRAINT provider_catalog_objects_interval_check CHECK (billing_interval_hours IS NULL OR billing_interval_months IS NULL),
    CONSTRAINT provider_catalog_objects_interval_positive_check CHECK (
        (billing_interval_hours IS NULL OR billing_interval_hours > 0)
        AND (billing_interval_months IS NULL OR billing_interval_months > 0)
    )
);
COMMENT ON TABLE billing.provider_catalog_objects IS 'What a PSP''s catalog says, as OpenRails last observed it: Stripe products and prices (active or not) and NMI plans (prices). Provider truth only, never written by a person: pulls, webhooks and by-id reads upsert it, the latest observation winning. amount is the decimal major-unit amount the provider states; currency is NULL where the provider names none (NMI). deleted_at is set only when a by-id read finds the object gone. A provider price is mapped by a provider price bound to it.';

-- ---------------------------------------------------------------------------
-- Maintenance runs and destructive controls
-- ---------------------------------------------------------------------------

CREATE TABLE billing.destructive_action_switch (
    singleton boolean NOT NULL DEFAULT true CHECK (singleton = true),
    enabled boolean NOT NULL,
    updated_by text,
    reason text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton)
);
COMMENT ON TABLE billing.destructive_action_switch IS 'Global by design: instance-level operator kill switch for destructive convergence, not tenant data. One row. Read from the no-GUC background connections the intent runner and sweep scheduler use, so it cannot be defeated by the connection scope it polices. Default disabled: a fresh deployment cancels nothing until an operator arms it.';

INSERT INTO billing.destructive_action_switch (enabled, reason)
VALUES (false, 'default safe (#836): arm deliberately once the first pull''s findings have been reviewed');

CREATE TABLE billing.merchant_destructive_policy (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    destructive_actions_enabled boolean CONSTRAINT merchant_destructive_policy_destructive_actions_enable_not_null NOT NULL DEFAULT true,
    enforce_armed_at timestamptz,
    first_pull_completed_at timestamptz,
    updated_by text,
    reason text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.merchant_destructive_policy IS 'Per-merchant destructive-action policy: destructive_actions_enabled is the per-merchant emergency stop (the instance switch in destructive_action_switch gates it globally); enforce_armed_at is the first-enforce gate — NULL means the merchant''s provider pull runs advisory (findings only, zero mutations) until an operator reviews the first pull and arms it.';
COMMENT ON COLUMN billing.merchant_destructive_policy.enforce_armed_at IS 'NULL = advisory-only pulls for this merchant. Absence of a row is the same as NULL, so a newly onboarded merchant is surveyed before it is enforced.';

CREATE TABLE billing.maintenance_runs (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    kind text NOT NULL,
    actor text CHECK (actor <> ''),
    psp_id uuid,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'completed', 'failed', 'reversed')),
    coverage jsonb,
    expected_rows bigint CHECK (expected_rows IS NULL OR expected_rows >= 0),
    affected jsonb,
    reversed_at timestamptz,
    reversed_by text,
    note text,
    summary jsonb,
    inventory_manifest jsonb,
    inventory_total_rows bigint,
    run_class text NOT NULL GENERATED ALWAYS AS (CASE WHEN kind = 'purge_inventory' THEN 'inventory' WHEN kind = 'billing_restore' THEN 'restore' ELSE 'destructive' END) STORED,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT maintenance_runs_id_run_class_key UNIQUE (merchant_id, id, run_class),
    CONSTRAINT maintenance_runs_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT maintenance_runs_kind_check CHECK ((
        (kind IN ('prune', 'approval', 'merchant_purge') AND btrim(actor) <> ''
         AND summary IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind = 'billing_restore' AND actor = 'merchantarchive' AND status IN ('running', 'completed')
         AND psp_id IS NULL AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND note IS NULL
         AND inventory_manifest IS NULL AND inventory_total_rows IS NULL
         AND ((status = 'running' AND finished_at IS NULL AND summary IS NULL)
              OR (status = 'completed' AND finished_at IS NOT NULL AND jsonb_typeof(summary) = 'object'
                  AND summary ?& ARRAY['digest', 'rows'] AND jsonb_typeof(summary -> 'digest') = 'string'
                  AND jsonb_typeof(summary -> 'rows') = 'number' AND (summary ->> 'digest') ~ '^[0-9a-f]{64}$' AND (summary ->> 'rows')::bigint >= 0)))
        OR (kind = 'purge_inventory' AND status = 'completed' AND finished_at IS NOT NULL AND psp_id IS NULL
         AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND summary IS NULL
         AND inventory_manifest IS NOT NULL AND jsonb_typeof(inventory_manifest) = 'object'
         AND jsonb_typeof(inventory_manifest -> 'total_rows') = 'number'
         AND inventory_total_rows IS NOT NULL AND inventory_total_rows >= 0
         AND inventory_total_rows = (inventory_manifest ->> 'total_rows')::bigint)
    ) IS TRUE)
);
COMMENT ON TABLE billing.maintenance_runs IS 'Typed maintenance run headers: reversible destructive work, billing restore receipts and immutable purge inventories. Each kind has explicit columns and constraints; before-images remain in destructive_run_before_images. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.maintenance_runs.coverage IS 'The coverage proof authorizing a destructive run, retained unchanged for audit and undo.';
COMMENT ON COLUMN billing.maintenance_runs.expected_rows IS 'The operator-confirmed or planned affected row count.';
COMMENT ON COLUMN billing.maintenance_runs.inventory_manifest IS 'Purge row counts, secret names, and omitted resources. Not a backup and never an undo image.';
COMMENT ON COLUMN billing.maintenance_runs.run_class IS 'Derived from the immutable kind. Child foreign keys include it, so stamped rows, intents and before-images reference only destructive runs.';

CREATE INDEX maintenance_runs_kind_started_at_idx ON billing.maintenance_runs (merchant_id, kind, started_at DESC);
-- One receipt, on the maintenance ledger, protects the narrow
-- restore-only trigger suppression. An application-set GUC alone does nothing.
CREATE UNIQUE INDEX maintenance_runs_merchant_id_key ON billing.maintenance_runs (merchant_id)
    WHERE kind = 'billing_restore';

CREATE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM billing.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.status<>'running' OR NEW.finished_at IS NOT NULL OR NEW.summary IS NOT NULL THEN
            RAISE EXCEPTION 'billing restore receipts must begin running and unfinished' USING ERRCODE='23514';
        END IF;
        FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
            WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN ('r','p')
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'access_cutover_approvals',
                  'admission_denials_hourly',
                  'admission_operations',
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'custodians',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_low_balance_lines',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'eventlog_events',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_bills',
                  'provider_charge_attributions',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_mutation_logs',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
    ELSE
        IF OLD.kind<>'billing_restore' OR NEW.kind<>'billing_restore'
           OR OLD.status<>'running' OR NEW.status<>'completed'
           OR NEW.finished_at IS NULL
           OR (to_jsonb(NEW)-ARRAY['status','finished_at','summary','run_class']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','finished_at','summary','run_class'])
           OR NOT EXISTS (SELECT 1 FROM billing.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$_$;

CREATE FUNCTION billing.require_finished_billing_restore() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.kind='billing_restore' THEN
        IF NOT EXISTS (SELECT 1 FROM billing.maintenance_runs WHERE id=NEW.id AND merchant_id=NEW.merchant_id AND status='completed') THEN
            RAISE EXCEPTION 'unfinished billing restore receipts cannot commit' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER guard_billing_restore_receipt BEFORE INSERT OR DELETE OR UPDATE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_restore_receipt();
CREATE TRIGGER immutable_maintenance_run_facts BEFORE UPDATE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('finished_at', 'status', 'summary', 'affected', 'reversed_at', 'reversed_by', 'note', 'run_class');
CREATE TRIGGER immutable_maintenance_runs_delete BEFORE DELETE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_maintenance_runs_truncate BEFORE TRUNCATE ON billing.maintenance_runs
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE CONSTRAINT TRIGGER require_finished_billing_restore AFTER INSERT OR UPDATE ON billing.maintenance_runs DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.require_finished_billing_restore();

CREATE FUNCTION billing.begin_billing_restore(p_merchant uuid) RETURNS uuid
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE receipt uuid;
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    PERFORM 1 FROM billing.merchants WHERE id=p_merchant AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    SELECT id INTO receipt FROM billing.maintenance_runs WHERE merchant_id=p_merchant AND kind='billing_restore' AND status='completed';
    IF receipt IS NOT NULL THEN RETURN receipt; END IF;
    INSERT INTO billing.maintenance_runs(merchant_id,kind,actor) VALUES(p_merchant,'billing_restore','merchantarchive') RETURNING id INTO receipt;
    PERFORM set_config('openrails.billing_restore_id',receipt::text,true);
    RETURN receipt;
END;
$$;
REVOKE ALL ON FUNCTION billing.begin_billing_restore(p_merchant uuid) FROM PUBLIC;

CREATE FUNCTION billing.billing_restore_active(p_merchant uuid) RETURNS boolean
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN RETURN false; END IF;
    RETURN EXISTS (SELECT 1 FROM billing.maintenance_runs r
        WHERE r.merchant_id=p_merchant AND r.kind='billing_restore' AND r.status='running'
        AND r.id::text=current_setting('openrails.billing_restore_id',true)
        AND r.xmin=pg_current_xact_id_if_assigned()::xid);
END;
$$;
REVOKE ALL ON FUNCTION billing.billing_restore_active(p_merchant uuid) FROM PUBLIC;

CREATE FUNCTION billing.finish_billing_restore(p_merchant uuid, p_digest text, p_rows bigint) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
BEGIN
    IF NOT billing.billing_restore_active(p_merchant) OR p_digest IS NULL OR p_rows IS NULL OR p_digest !~ '^[0-9a-f]{64}$' OR p_rows<0 THEN
        RAISE EXCEPTION 'invalid billing restore finalization' USING ERRCODE='23514';
    END IF;
    UPDATE billing.maintenance_runs SET status='completed',finished_at=now(),summary=jsonb_build_object('digest',p_digest,'rows',p_rows)
        WHERE merchant_id=p_merchant AND kind='billing_restore' AND id::text=current_setting('openrails.billing_restore_id',true);
END;
$_$;
REVOKE ALL ON FUNCTION billing.finish_billing_restore(p_merchant uuid, p_digest text, p_rows bigint) FROM PUBLIC;

CREATE TABLE billing.destructive_run_before_images (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    destructive_run_id uuid NOT NULL,
    table_name text NOT NULL,
    row_id uuid NOT NULL,
    before jsonb NOT NULL,
    captured_at timestamptz NOT NULL DEFAULT now(),
    restored_at timestamptz,
    destructive_run_class text NOT NULL GENERATED ALWAYS AS ('destructive') STORED,
    after_lifecycle_rev bigint,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT destructive_run_before_images_run_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT destructive_run_before_images_table_check CHECK (table_name IN ('subscriptions', 'entitlements', 'product_access'))
);
COMMENT ON TABLE billing.destructive_run_before_images IS 'The row as it stood immediately before a destructive run updated it, so the run can be reversed. A soft-delete stamp reverses deletes; this reverses updates. One image per (run, table, row), pinned to exactly one run. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.destructive_run_before_images.before IS 'to_jsonb(row) verbatim, captured server-side inside the run. Complete evidence; the restore reads an explicit typed column projection out of it rather than rewriting the whole row.';
COMMENT ON COLUMN billing.destructive_run_before_images.restored_at IS 'When the reverse replayed this image. NULL after a completed reversal means the image was captured as evidence but deliberately never replayed: entitlement rows are RECOMPUTED from the append-only grant log by Converge, never restored. Restoring one directly could make it disagree with its grant, which recomputation cannot.';
COMMENT ON COLUMN billing.destructive_run_before_images.after_lifecycle_rev IS 'The subscription''s lifecycle_rev right after the run''s write. The reverse restores the row only while lifecycle_rev still equals it; NULL (an access image, or a write the run never sealed) is never restored.';

CREATE UNIQUE INDEX destructive_run_before_images_identity_key ON billing.destructive_run_before_images (merchant_id, destructive_run_id, table_name, row_id);
COMMENT ON INDEX billing.destructive_run_before_images_identity_key IS 'Merchant-led. One image per (run, table, row) WITHIN a merchant — the second capture inside a run is the run''s own later write, not the state it inherited, and must never displace the first (the capture is ON CONFLICT DO NOTHING for that reason). Also serves the by-run reads of the reverse, so no separate (merchant_id, destructive_run_id) index is kept.';

CREATE FUNCTION billing.guard_before_image() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE'
       OR (OLD.after_lifecycle_rev IS NOT NULL AND NEW.after_lifecycle_rev IS DISTINCT FROM OLD.after_lifecycle_rev)
       OR (to_jsonb(NEW) - ARRAY['restored_at','destructive_run_class','after_lifecycle_rev'])
          IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['restored_at','destructive_run_class','after_lifecycle_rev']) THEN
        RAISE EXCEPTION 'immutable billing facts in % cannot be rewritten', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER immutable_destructive_before_images BEFORE DELETE OR UPDATE ON billing.destructive_run_before_images
    FOR EACH ROW EXECUTE FUNCTION billing.guard_before_image();

CREATE TABLE billing.destructive_actions (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    admission_sequence bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    kind text NOT NULL,
    target text NOT NULL CHECK (btrim(target) <> '' AND octet_length(target) <= 2048),
    action_key text NOT NULL CHECK (btrim(action_key) <> '' AND octet_length(action_key) <= 2048),
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    origin text NOT NULL CHECK (origin IN ('staff', 'host', 'automation')),
    actor_issuer text NOT NULL CHECK (btrim(actor_issuer) <> ''),
    actor_id text NOT NULL CHECK (btrim(actor_id) <> ''),
    actor_key text NOT NULL CHECK (actor_key ~ '^[0-9a-f]{64}$'),
    credential text CHECK (credential <> ''),
    units bigint NOT NULL CHECK (units > 0 AND units <= 1000000000),
    admitted_at timestamptz NOT NULL,
    state text NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    completion_sequence bigint,
    result text CHECK (result <> '' AND octet_length(result) <= 2048),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT destructive_actions_admission_sequence_key UNIQUE (admission_sequence),
    CHECK (kind IN ('refund', 'cancel_subscription', 'delete_payment_method', 'revoke_credit', 'revoke_access', 'waive_order', 'mark_uncollectible', 'catalog_change', 'configuration_change')),
    CONSTRAINT destructive_actions_lifecycle_check CHECK (
        (state = 'reserved' AND started_at IS NULL AND finished_at IS NULL)
        OR (state IN ('started', 'uncertain') AND started_at IS NOT NULL AND finished_at IS NULL)
        OR (state = 'succeeded' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND completion_sequence IS NOT NULL)
        OR (state = 'no_effect' AND finished_at IS NOT NULL)
    ),
    CHECK (state IN ('reserved', 'started', 'succeeded', 'no_effect', 'uncertain'))
);
COMMENT ON TABLE billing.destructive_actions IS 'Admitted destructive command audit, immutable authority and admission facts with explicit outcome; not a counter or request-body store. Proven no-effect retries create another immutable attempt. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.destructive_actions.origin IS 'staff: a staff or application actor, counted and held. host: the embedded host''s own in-process work, audited but never counted or held. automation: named autonomous work on its own budget.';

CREATE INDEX destructive_actions_actor_recent_idx ON billing.destructive_actions (actor_key, GREATEST(admitted_at, finished_at)) INCLUDE (units)
    WHERE state = 'succeeded' AND origin = 'staff';
CREATE INDEX destructive_actions_completion_sequence_idx ON billing.destructive_actions (completion_sequence)
    WHERE completion_sequence IS NOT NULL;
CREATE INDEX destructive_actions_key_history_idx ON billing.destructive_actions (merchant_id, action_key, admitted_at DESC, id DESC);
CREATE UNIQUE INDEX destructive_actions_live_key ON billing.destructive_actions (merchant_id, action_key)
    WHERE state <> 'no_effect';
CREATE INDEX destructive_actions_merchant_recent_idx ON billing.destructive_actions (merchant_id, origin, GREATEST(admitted_at, finished_at)) INCLUDE (units)
    WHERE state = 'succeeded';
CREATE INDEX destructive_actions_pending_idx ON billing.destructive_actions (merchant_id, actor_key) INCLUDE (units)
    WHERE state IN ('reserved', 'started', 'uncertain');
CREATE INDEX destructive_actions_pending_target_idx ON billing.destructive_actions (merchant_id, target, actor_issuer, actor_id, request_digest)
    WHERE state IN ('reserved', 'started', 'uncertain');
CREATE INDEX destructive_actions_recent_idx ON billing.destructive_actions (GREATEST(admitted_at, finished_at)) INCLUDE (units)
    WHERE state = 'succeeded' AND origin = 'staff';

CREATE FUNCTION billing.guard_destructive_action_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.state IN ('succeeded','no_effect') AND NEW IS DISTINCT FROM OLD THEN
   RAISE EXCEPTION 'destructive action is final' USING ERRCODE='23514';
 END IF;
 IF OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at THEN
   RAISE EXCEPTION 'destructive action start is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.completion_sequence IS NOT NULL AND NEW.completion_sequence IS DISTINCT FROM OLD.completion_sequence THEN
   RAISE EXCEPTION 'destructive action outcome order is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.state<>'succeeded' AND NEW.completion_sequence IS NOT NULL THEN
   RAISE EXCEPTION 'only successful actions have a completion sequence' USING ERRCODE='23514';
 END IF;
 IF OLD.state <> 'reserved' AND NEW.state='reserved' THEN
   RAISE EXCEPTION 'destructive action cannot be unstarted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;

CREATE TRIGGER destructive_actions_immutable BEFORE UPDATE ON billing.destructive_actions
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('state', 'started_at', 'finished_at', 'completion_sequence', 'result');
CREATE TRIGGER destructive_actions_no_delete BEFORE DELETE ON billing.destructive_actions
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER destructive_actions_state BEFORE UPDATE ON billing.destructive_actions
    FOR EACH ROW EXECUTE FUNCTION billing.guard_destructive_action_state();

CREATE TABLE billing.destructive_holds (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    scope text NOT NULL CHECK (scope IN ('actor', 'merchant', 'deployment')),
    scope_key text NOT NULL CHECK (btrim(scope_key) <> ''),
    actor_issuer text NOT NULL CHECK (btrim(actor_issuer) <> ''),
    actor_id text NOT NULL CHECK (btrim(actor_id) <> ''),
    credential text CHECK (credential <> ''),
    action_kind text NOT NULL,
    target text NOT NULL CHECK (btrim(target) <> ''),
    window_name text NOT NULL CHECK (window_name IN ('hour', 'day')),
    observed bigint NOT NULL CHECK (observed >= 0),
    requested bigint NOT NULL CHECK (requested > 0),
    ceiling bigint NOT NULL CHECK (ceiling > 0),
    opened_at timestamptz NOT NULL,
    cleared_at timestamptz,
    clear_through_sequence bigint,
    cleared_by_issuer text,
    cleared_by_id text,
    clear_reason text,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT destructive_holds_clear_check CHECK (
        (cleared_at IS NULL AND clear_through_sequence IS NULL AND cleared_by_issuer IS NULL AND cleared_by_id IS NULL AND clear_reason IS NULL)
        OR (cleared_at IS NOT NULL AND clear_through_sequence IS NOT NULL AND clear_through_sequence >= 0 AND cleared_by_issuer IS NOT NULL AND btrim(cleared_by_issuer) <> '' AND cleared_by_id IS NOT NULL AND btrim(cleared_by_id) <> '' AND clear_reason IS NOT NULL AND btrim(clear_reason) <> '' AND (cleared_by_issuer IS DISTINCT FROM actor_issuer OR cleared_by_id IS DISTINCT FROM actor_id))
    )
);
COMMENT ON TABLE billing.destructive_holds IS 'Deployment-authoritative destructive incident episodes. Merchant_id identifies only the triggering tenant; actor/deployment holds span tenants. An audited clear ends one episode, never an ordinary finding resolution. Retention: permanent, never pruned.';

CREATE INDEX destructive_holds_epoch_idx ON billing.destructive_holds (scope_key, cleared_at DESC)
    WHERE cleared_at IS NOT NULL;
CREATE UNIQUE INDEX destructive_holds_open_scope_key ON billing.destructive_holds (scope_key)
    WHERE cleared_at IS NULL;

CREATE FUNCTION billing.guard_destructive_hold_clear() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.cleared_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
   RAISE EXCEPTION 'destructive hold clear is final' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;

CREATE TRIGGER destructive_holds_clear BEFORE UPDATE ON billing.destructive_holds
    FOR EACH ROW EXECUTE FUNCTION billing.guard_destructive_hold_clear();
CREATE TRIGGER destructive_holds_immutable BEFORE UPDATE ON billing.destructive_holds
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('cleared_at', 'clear_through_sequence', 'cleared_by_issuer', 'cleared_by_id', 'clear_reason');
CREATE TRIGGER destructive_holds_no_delete BEFORE DELETE ON billing.destructive_holds
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.user_destructive_reservations (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE CASCADE,
    customer_id uuid NOT NULL,
    action_key text NOT NULL CHECK (action_key <> '' AND octet_length(action_key) <= 512),
    kind text NOT NULL CHECK (kind IN ('delete_payment_method', 'cancel_subscription')),
    reserved_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, action_key)
);
COMMENT ON TABLE billing.user_destructive_reservations IS 'Merchant-global customer card-delete and soft-cancel rolling-hour/day rate reservations. No actor can obtain a separate budget. customer_id is an opaque identity and deliberately has no cascading customer FK. The canonical operation/card/subscription remains authoritative after pruning. Retention: rows are deleted 24 hours after reserved_at.';

CREATE INDEX user_destructive_reservations_expiry_idx ON billing.user_destructive_reservations (reserved_at, merchant_id, action_key);
CREATE INDEX user_destructive_reservations_window_idx ON billing.user_destructive_reservations (merchant_id, reserved_at) INCLUDE (customer_id);

-- ---------------------------------------------------------------------------
-- Customers
-- ---------------------------------------------------------------------------

CREATE TABLE billing.customers (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    issuer text CHECK (issuer IS NULL OR (issuer = btrim(issuer) AND issuer <> '')),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    access_version bigint NOT NULL DEFAULT 0,
    billing_policy text CHECK (billing_policy <> ''),
    PRIMARY KEY (merchant_id, id)
);
COMMENT ON TABLE billing.customers IS 'OpenRails payable identity: (merchant_id, issuer, id), id being the issuer''s stable UUID subject. A credential of another issuer never acts on the row.';
COMMENT ON COLUMN billing.customers.issuer IS 'The trusted issuer whose subject id is, fixed when the row is created; NULL is the host''s own users (its native issuer).';
COMMENT ON COLUMN billing.customers.access_version IS 'Steps with every statement that changes the customer''s product access: a cached key set from an older version is stale.';
COMMENT ON COLUMN billing.customers.billing_policy IS 'The billing policy staff assigned this customer, by its name in the merchant''s settings; NULL follows the tier, then the default.';

CREATE INDEX customers_billing_policy_idx ON billing.customers (merchant_id, billing_policy)
    WHERE billing_policy IS NOT NULL;
CREATE INDEX customers_id_idx ON billing.customers (id, merchant_id);

CREATE TABLE billing.customer_contacts (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    email text,
    display_name text,
    user_name text,
    active boolean,
    provisioned_at timestamptz,
    directory_updated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id),
    CONSTRAINT customer_contacts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CHECK (email IS NULL OR (email = btrim(email) AND email <> '' AND octet_length(email) <= 320)),
    CHECK (
        display_name IS NULL
        OR (display_name = btrim(display_name) AND display_name <> '' AND octet_length(display_name) <= 256)
    ),
    CHECK (user_name IS NULL OR (user_name = btrim(user_name) AND user_name <> '' AND octet_length(user_name) <= 256))
);
COMMENT ON TABLE billing.customer_contacts IS 'A customer''s contact as the merchant''s directory last reported it, when OpenRails is not embedded beside that directory: pushed over SCIM, or recorded from a verified access token''s claims. Newest wins by directory_updated_at. Erasure keeps the row without its values, so an older report cannot restore them.';
COMMENT ON COLUMN billing.customer_contacts.active IS 'SCIM active, shown and never enforced: the host''s auth decides who signs in. NULL when no SCIM client reported it.';
COMMENT ON COLUMN billing.customer_contacts.provisioned_at IS 'When a SCIM client created this User (its meta.created); NULL when no SCIM client holds it: claims alone recorded it, or it was deleted.';
COMMENT ON COLUMN billing.customer_contacts.directory_updated_at IS 'When the directory last changed these values, as it reported (a SCIM resource''s meta.lastModified, a token''s updated_at claim), else when OpenRails received a SCIM write. NULL when claims without updated_at recorded it: any dated report replaces it. An older report is ignored.';
COMMENT ON COLUMN billing.customer_contacts.updated_at IS 'When OpenRails last wrote the contact: its SCIM meta.lastModified, the customer read''s synced_at.';

CREATE INDEX customer_contacts_email_idx ON billing.customer_contacts (merchant_id, lower(email))
    WHERE email IS NOT NULL;
CREATE INDEX customer_contacts_provisioned_idx ON billing.customer_contacts (merchant_id, provisioned_at, customer_id)
    WHERE provisioned_at IS NOT NULL;
CREATE UNIQUE INDEX customer_contacts_user_name_key ON billing.customer_contacts (merchant_id, lower(user_name))
    WHERE provisioned_at IS NOT NULL;

CREATE TABLE billing.customer_invoice_profiles (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    net_terms_days integer NOT NULL,
    collection_method text NOT NULL,
    po_number text,
    tax jsonb NOT NULL,
    billing_contacts jsonb NOT NULL,
    memo text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id),
    CONSTRAINT customer_invoice_profiles_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_invoice_profiles_net_terms_check CHECK (net_terms_days >= 0),
    CHECK (collection_method IN ('charge_automatically', 'send_invoice'))
);
COMMENT ON TABLE billing.customer_invoice_profiles IS 'Per-payer enterprise invoicing profile: net-N terms, collection method (charge_automatically | send_invoice for manual remittance) and document fields (PO, tax, contacts) snapshotted onto invoices at finalize.';

CREATE TABLE billing.customer_delinquency (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    state text NOT NULL CHECK (state IN ('current', 'grace', 'delinquent')),
    overdue_started_at timestamptz,
    entered_at timestamptz NOT NULL,
    overdue_amount bigint NOT NULL,
    overdue_orders bigint CONSTRAINT customer_delinquency_overdue_invoices_not_null NOT NULL,
    transition_seq bigint NOT NULL,
    evaluated_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    next_evaluation_at timestamptz,
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_delinquency_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_delinquency_overdue_started_at_check CHECK (
        (state = 'current' AND overdue_started_at IS NULL)
        OR (state <> 'current' AND overdue_started_at IS NOT NULL)
    ),
    CONSTRAINT customer_delinquency_amount_check CHECK (overdue_amount >= 0 AND overdue_orders >= 0)
);
COMMENT ON TABLE billing.customer_delinquency IS 'Per-(merchant, payer, currency) arrears delinquency state: current -> grace -> delinquent, derived from overdue open usage orders against the merchant''s declared grace window and amount floor. Evaluated when a usage order of the payer changes and at next_evaluation_at; a payer with an open usage order has a row. A projection of order truth; only the transition watermarks (entered_at, transition_seq) are not recomputable. Delinquency NEVER revokes an entitlement: it refuses new spend at admission and emits a host_outbox signal; the operator owns the shutoff.';
COMMENT ON COLUMN billing.customer_delinquency.overdue_started_at IS 'The oldest overdue due_at behind this state — the clock the grace window is measured on, not the moment we noticed.';
COMMENT ON COLUMN billing.customer_delinquency.transition_seq IS 'Bumped only when state changes; the idempotency coordinate of the emitted host_outbox row.';
COMMENT ON COLUMN billing.customer_delinquency.next_evaluation_at IS 'The next instant time alone changes this payer''s delinquency in the currency: a usage order falling due or its grace window running out. Null: only a change to its usage orders or policy does.';

CREATE INDEX customer_delinquency_customer_id_currency_idx ON billing.customer_delinquency (merchant_id, customer_id, currency)
    WHERE state <> 'current';
CREATE INDEX customer_delinquency_due_idx ON billing.customer_delinquency (next_evaluation_at, merchant_id, customer_id, currency)
    WHERE next_evaluation_at IS NOT NULL;

CREATE TABLE billing.psp_customers (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    remote_customer_ref text NOT NULL CHECK (btrim(remote_customer_ref) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT psp_customers_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT psp_customers_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.psp_customers IS 'A customer''s customer object at one PSP. Two PSPs on one rail hold independent mappings.';
COMMENT ON COLUMN billing.psp_customers.remote_customer_ref IS 'The PSP''s own customer id (Stripe cus_). Unique only within the PSP that minted it.';

CREATE UNIQUE INDEX psp_customers_customer_id_psp_id_key ON billing.psp_customers (merchant_id, customer_id, psp_id);
CREATE UNIQUE INDEX psp_customers_psp_id_remote_customer_ref_key ON billing.psp_customers (merchant_id, psp_id, remote_customer_ref);

-- ---------------------------------------------------------------------------
-- Catalog
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.assign_object_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        NEW.revision := COALESCE(NEW.revision, 1);
    ELSIF NEW.revision IS DISTINCT FROM OLD.revision
       OR (to_jsonb(NEW)-ARRAY['updated_at','revision']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at','revision']) THEN
        NEW.revision := OLD.revision + 1;
    ELSE
        NEW.revision := OLD.revision;
    END IF;
    RETURN NEW;
END $$;

-- Individual services acquire the merchant lock first. This trigger also
-- fences direct imports and module writes, keeping them visible to application CAS.
CREATE FUNCTION billing.catalog_authored_write() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_OP='DELETE' THEN mid := OLD.merchant_id; ELSE mid := NEW.merchant_id; END IF;
    IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text THEN
        -- A legacy raw writer may already hold a child-row lock. Do not wait
        -- behind a merchant-first transaction while holding that child: refuse
        -- with a retryable serialization conflict instead of deadlocking.
        BEGIN
            PERFORM id FROM billing.merchants WHERE id=mid FOR UPDATE NOWAIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
        END;
        UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE TABLE billing.products (
    id uuid NOT NULL DEFAULT uuidv7(),
    key text NOT NULL,
    display_name text NOT NULL,
    description text,
    tier_group text CHECK (char_length(tier_group) BETWEEN 1 AND 100),
    tier_rank integer NOT NULL,
    archived boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision >= 1),
    credit_grant jsonb,
    ownership text CHECK (ownership IN ('unique', 'consumable', 'extend')),
    deleted_at timestamptz,
    required_entitlements text[] COLLATE "C" NOT NULL DEFAULT '{}',
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT products_key_key UNIQUE (merchant_id, key),
    CONSTRAINT products_deleted_archived_check CHECK (deleted_at IS NULL OR archived),
    CONSTRAINT products_credit_grant_object CHECK (
        credit_grant IS NULL
        OR (jsonb_typeof(credit_grant) = 'object' AND COALESCE(((credit_grant ->> 'currency') ~ '^[A-Z0-9]{3,12}$'), false) AND COALESCE((((credit_grant ->> 'expires_after_days'))::integer BETWEEN 1 AND 36500), false) AND ((COALESCE((((credit_grant ->> 'amount'))::bigint > 0), false) AND NOT (COALESCE(((credit_grant ->> 'from_payment'))::boolean, false))) OR (NOT (credit_grant ? 'amount') AND COALESCE(((credit_grant ->> 'from_payment'))::boolean, false))))
    ),
    CHECK (cardinality(required_entitlements) <= 100 AND array_position(required_entitlements, NULL::text) IS NULL)
);
COMMENT ON TABLE billing.products IS 'Catalog products; retire with archived, never delete. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). NULL: in no group (an empty name is stored as NULL). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN billing.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';
COMMENT ON COLUMN billing.products.revision IS 'Advances on every change to the product, its keys or its rate cards; never on a change to its prices.';
COMMENT ON COLUMN billing.products.credit_grant IS 'Purchased currency credit policy; accepted purchases freeze amount and expiry duration.';
COMMENT ON COLUMN billing.products.ownership IS 'unique (one live holding per customer, per tier group when set), consumable (units stack, quantity counts them) or extend (a later purchase starts when the current window ends). NULL derives it: consumable for a credit product, extend when every price is one-time with an access duration, unique otherwise.';
COMMENT ON COLUMN billing.products.deleted_at IS 'When the product was deleted: nothing referenced it. Reads leave it out; declaring its key again revives it.';
COMMENT ON COLUMN billing.products.required_entitlements IS 'Entitlement keys a customer must hold when ordering the product, in byte order; empty: anyone may buy it.';

CREATE INDEX products_archived_idx ON billing.products (archived);
CREATE INDEX products_created_at_id_idx ON billing.products (merchant_id, created_at DESC, id DESC);
CREATE INDEX products_key_idx ON billing.products (key);
CREATE INDEX products_tier_group_idx ON billing.products (tier_group) WHERE tier_group IS NOT NULL;

-- Product eligibility is a separate lifecycle flag. Its changes affect offers
-- without changing their own archive flags or repricing existing subscribers.
CREATE FUNCTION billing.catalog_product_offer_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.archived IS DISTINCT FROM OLD.archived THEN
  INSERT INTO billing.price_key_movements(merchant_id,key,price_id,effective_at,archived)
   SELECT p.merchant_id,p.key,p.id,clock_timestamp(),NEW.archived FROM billing.prices p
   WHERE p.merchant_id=NEW.merchant_id AND p.product_id=NEW.id AND NOT p.archived;
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION billing.guard_product_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id OR NEW.key IS DISTINCT FROM OLD.key THEN
        RAISE EXCEPTION 'product identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Product identity cannot change underneath live billing or paid access.
-- The product UPDATE owns the row lock; subscription inserts and reactivations take
-- FOR SHARE on that same row before copying tier_group, serializing both orders.
CREATE FUNCTION billing.products_guard_tier_group() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group AND EXISTS (
        SELECT 1 FROM billing.subscriptions s
        WHERE s.merchant_id = OLD.merchant_id AND s.product_id = OLD.id
          AND s.deleted_at IS NULL
          AND s.status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified')
          AND (EXISTS (
                SELECT 1 FROM billing.scheduled_changes c
                WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.status = 'scheduled')
            OR EXISTS (
                SELECT 1 FROM billing.provider_intents i
                WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                  AND i.intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
                  AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')))
    ) THEN
        RAISE EXCEPTION 'product tier group cannot change while a live subscription has a plan change in flight'
            USING ERRCODE = '23514', CONSTRAINT = 'products_live_subscription_tier_group';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.products_propagate_tier_group() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group THEN
        UPDATE billing.subscriptions SET tier_group = NEW.tier_group
        WHERE merchant_id = NEW.merchant_id AND product_id = NEW.id AND deleted_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER catalog_authored_product BEFORE INSERT OR DELETE OR UPDATE ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_product_offer_state AFTER UPDATE OF archived ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_product_offer_state();
CREATE TRIGGER immutable_product_identity BEFORE UPDATE ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.guard_product_identity();
CREATE TRIGGER no_product_delete BEFORE DELETE ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER no_product_truncate BEFORE TRUNCATE ON billing.products
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER product_revision BEFORE INSERT OR UPDATE ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();
CREATE TRIGGER trg_products_guard_tier_group BEFORE UPDATE OF tier_group ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.products_guard_tier_group();
CREATE TRIGGER trg_products_propagate_tier_group AFTER UPDATE OF tier_group ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.products_propagate_tier_group();

CREATE TABLE billing.product_entitlements (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    product_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    added_at timestamptz NOT NULL,
    removed_at timestamptz,
    added_by text NOT NULL CHECK (octet_length(added_by) BETWEEN 1 AND 255),
    removed_by text,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT product_entitlements_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CHECK (octet_length(entitlement) BETWEEN 1 AND 256 AND btrim(entitlement) <> ''),
    CONSTRAINT product_entitlements_window_check CHECK (removed_at IS NULL OR removed_at >= added_at),
    CONSTRAINT product_entitlements_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
    CHECK (removed_by IS NULL OR octet_length(removed_by) BETWEEN 1 AND 255)
);
COMMENT ON TABLE billing.product_entitlements IS 'The entitlement keys each product grants, with valid-time history: a key holds for [added_at, removed_at). Holders derive their keys from these rows at check time. Rows are never deleted; removal closes a row. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.product_entitlements.added_at IS 'When the key started being granted. Keys that predate this history (migrated or restored from older archives) hold from 0001-01-01.';
COMMENT ON COLUMN billing.product_entitlements.removed_at IS 'When the key stopped being granted; equal to added_at for a key added and removed at the same instant.';
COMMENT ON COLUMN billing.product_entitlements.added_by IS 'Who added the key: a catalog application id (sha256:...), an operator, or migration.';

CREATE INDEX product_entitlements_entitlement_idx ON billing.product_entitlements (merchant_id, entitlement) INCLUDE (product_id, added_at, removed_at);
COMMENT ON INDEX billing.product_entitlements_entitlement_idx IS 'The products granting a key: exact checks, reverse lookups and ReplaceEntitlements.';
CREATE UNIQUE INDEX product_entitlements_live_key ON billing.product_entitlements (merchant_id, product_id, entitlement)
    WHERE removed_at IS NULL;
CREATE INDEX product_entitlements_product_idx ON billing.product_entitlements (merchant_id, product_id, entitlement, added_at) INCLUDE (removed_at);
COMMENT ON INDEX billing.product_entitlements_product_idx IS 'One product''s keys in byte order: prefix reads probe it once per held product.';

CREATE FUNCTION billing.catalog_authored_entitlements() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text THEN
            BEGIN
                PERFORM id FROM billing.merchants WHERE id=mid FOR UPDATE NOWAIT;
            EXCEPTION WHEN lock_not_available THEN
                RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
            END;
            UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
        END IF;
    END LOOP;
    RETURN NULL;
END $$;

CREATE FUNCTION billing.guard_product_entitlement_history() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'product entitlement history is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL
       OR (to_jsonb(NEW)-ARRAY['removed_at','removed_by']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['removed_at','removed_by']) THEN
        RAISE EXCEPTION 'a product entitlement row only closes, once' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION billing.step_entitlement_generation() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        UPDATE billing.merchants SET entitlement_generation = entitlement_generation + 1 WHERE id = mid;
    END LOOP;
    RETURN NULL;
END $$;

CREATE TRIGGER catalog_authored_entitlements_insert AFTER INSERT ON billing.product_entitlements REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.catalog_authored_entitlements();
CREATE TRIGGER catalog_authored_entitlements_update AFTER UPDATE ON billing.product_entitlements REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.catalog_authored_entitlements();
CREATE TRIGGER immutable_product_entitlement_history BEFORE DELETE OR UPDATE ON billing.product_entitlements
    FOR EACH ROW EXECUTE FUNCTION billing.guard_product_entitlement_history();
CREATE TRIGGER immutable_product_entitlement_history_truncate BEFORE TRUNCATE ON billing.product_entitlements
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER product_entitlements_step_generation_insert AFTER INSERT ON billing.product_entitlements REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.step_entitlement_generation();
CREATE TRIGGER product_entitlements_step_generation_update AFTER UPDATE ON billing.product_entitlements REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.step_entitlement_generation();

CREATE FUNCTION billing.escape_price_key(key text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT SET search_path TO 'pg_catalog' AS $$
DECLARE
    bytes bytea := convert_to(key, 'UTF8');
    result text := '';
    value integer;
BEGIN
    FOR i IN 0..length(bytes)-1 LOOP
        value := get_byte(bytes, i);
        IF value BETWEEN 65 AND 90 OR value BETWEEN 97 AND 122 OR value BETWEEN 48 AND 57 OR value IN (45, 46, 95, 126) THEN
            result := result || chr(value);
        ELSE
            result := result || '%' || upper(lpad(to_hex(value), 2, '0'));
        END IF;
    END LOOP;
    RETURN result;
END;
$$;

CREATE TABLE billing.prices (
    id uuid NOT NULL DEFAULT uuidv7(),
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    archived boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    access_duration_hours integer,
    trial_unit_amount bigint,
    trial_duration_hours integer,
    key text NOT NULL,
    revision bigint NOT NULL,
    customer_amount jsonb,
    billing_interval_hours integer,
    quantity jsonb,
    deleted_at timestamptz,
    public_id text NOT NULL,
    provider_owned boolean NOT NULL DEFAULT false,
    provider_psp_id uuid,
    provider_snapshot jsonb,
    billing_interval_months integer,
    provider_access_default boolean NOT NULL DEFAULT false,
    sell_via text[],
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT prices_id_product_id_key UNIQUE (merchant_id, id, product_id),
    CONSTRAINT prices_product_amount_window_key UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, billing_interval_hours, trial_unit_amount, trial_duration_hours, customer_amount, billing_interval_months, provider_access_default),
    CONSTRAINT prices_product_key_revision_key UNIQUE (merchant_id, product_id, key, revision),
    CONSTRAINT prices_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT prices_provider_psp_fkey FOREIGN KEY (merchant_id, provider_psp_id) REFERENCES billing.psps (merchant_id, id),
    CONSTRAINT prices_amount_nonneg_check CHECK (amount >= 0),
    CONSTRAINT prices_customer_amount_terms CHECK (
        customer_amount IS NULL
        OR (jsonb_typeof(customer_amount) = 'object' AND amount = 0 AND billing_interval_hours IS NULL AND access_duration_hours IS NULL AND trial_unit_amount IS NULL AND trial_duration_hours IS NULL AND customer_amount ? 'min_amount' AND customer_amount ? 'max_amount' AND COALESCE((((customer_amount ->> 'min_amount'))::bigint > 0), false) AND COALESCE((((customer_amount ->> 'max_amount'))::bigint >= ((customer_amount ->> 'min_amount'))::bigint), false))
    ),
    CONSTRAINT prices_deleted_archived_check CHECK (deleted_at IS NULL OR archived),
    CONSTRAINT prices_access_duration_positive_check CHECK (access_duration_hours IS NULL OR access_duration_hours > 0),
    CONSTRAINT prices_provider_access_default_check CHECK (NOT provider_access_default OR (provider_owned AND access_duration_hours IS NULL)),
    CONSTRAINT prices_trial_amount_nonneg_check CHECK (trial_unit_amount IS NULL OR trial_unit_amount >= 0),
    CONSTRAINT prices_trial_both_or_neither_check CHECK ((trial_unit_amount IS NULL) = (trial_duration_hours IS NULL)),
    CONSTRAINT prices_trial_needs_billing_interval_check CHECK (trial_unit_amount IS NULL OR billing_interval_hours IS NOT NULL),
    CONSTRAINT prices_trial_period_positive_check CHECK (trial_duration_hours IS NULL OR trial_duration_hours > 0),
    CONSTRAINT prices_key_nonempty_check CHECK (btrim(key) <> ''),
    CONSTRAINT prices_revision_nonnegative CHECK (revision >= 0),
    CONSTRAINT prices_quantity_terms CHECK (
        quantity IS NULL
        OR (jsonb_typeof(quantity) = 'object' AND billing_interval_hours IS NOT NULL AND customer_amount IS NULL AND quantity ? 'min' AND quantity ? 'max' AND COALESCE((((quantity ->> 'min'))::integer >= 1), false) AND COALESCE((((quantity ->> 'max'))::integer >= ((quantity ->> 'min'))::integer AND ((quantity ->> 'max'))::integer <= 10000), false))
    ),
    CONSTRAINT prices_billing_interval_positive_check CHECK (billing_interval_hours IS NULL OR billing_interval_hours > 0),
    CONSTRAINT prices_calendar_cadence_check CHECK (
        billing_interval_months IS NULL
        OR (provider_owned AND billing_interval_hours IS NULL AND billing_interval_months BETWEEN 1 AND 1200)
    ),
    CONSTRAINT prices_provider_snapshot_check CHECK (
        (provider_psp_id IS NULL AND provider_snapshot IS NULL AND NOT provider_owned)
        OR (provider_owned AND provider_psp_id IS NOT NULL AND provider_snapshot IS NOT NULL AND jsonb_typeof(provider_snapshot) = 'object' AND COALESCE(((provider_snapshot ->> 'psp_id') = provider_psp_id::text), false))
    ),
    CONSTRAINT prices_sell_via_check CHECK (
        sell_via IS NULL
        OR (NOT provider_owned AND cardinality(sell_via) > 0 AND sell_via <@ ARRAY['card', 'solana'])
    )
);
COMMENT ON TABLE billing.prices IS 'Immutable financial price versions; retire with archived, never delete. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.prices.amount IS 'Price amount in row currency micros (1 major unit = 1,000,000).';
COMMENT ON COLUMN billing.prices.access_duration_hours IS 'Access granted by each purchase in hours; NULL means no scheduled expiry, independently of billing cadence.';
COMMENT ON COLUMN billing.prices.trial_unit_amount IS 'Optional first-phase price (micros); 0 = free trial; NULL = no trial.';
COMMENT ON COLUMN billing.prices.trial_duration_hours IS 'Optional trial first-phase length in HOURS; NULL = no trial.';
COMMENT ON COLUMN billing.prices.key IS 'Product-local handle for an immutable price version chain. One live row per (merchant_id, product_id, key); archived versions retain their key and product.';
COMMENT ON COLUMN billing.prices.customer_amount IS 'Immutable inclusive customer-selected deposit bounds in currency micros; NULL means fixed amount.';
COMMENT ON COLUMN billing.prices.billing_interval_hours IS 'Recurring billing cadence in hours; NULL means one-time. Recurring subscriptions bill until canceled.';
COMMENT ON COLUMN billing.prices.quantity IS 'Seat bounds {min, max} of a recurring price sold per seat; amount is per seat. NULL: the price has no quantity.';
COMMENT ON COLUMN billing.prices.deleted_at IS 'When the price was deleted, with its product or its key: nothing referenced it. Reads leave it out; declaring the same terms again revives it.';
COMMENT ON COLUMN billing.prices.public_id IS 'Public immutable ID product-key:price-key:v1. Percent-encoded keys; financial versions start at one. UUID id remains a private foreign key.';
COMMENT ON COLUMN billing.prices.provider_owned IS 'A provider owns and runs the price: its one price_psp_bindings row names the PSP and the provider object, whose terms this row caches. False: ours, bound only to the Solana plans OpenRails published for it.';
COMMENT ON COLUMN billing.prices.provider_snapshot IS 'Immutable provider object and native cadence accepted for this price version; retained even when its normalized lookup binding moves to a newer version.';
COMMENT ON COLUMN billing.prices.billing_interval_months IS 'Provider calendar cadence, independent of fixed hours; a year is twelve months. NULL on our prices and fixed-duration provider prices.';
COMMENT ON COLUMN billing.prices.provider_access_default IS 'Access ends at the verified provider service-period end. False retains the explicit hourly access duration, including all pre-split accepted terms.';
COMMENT ON COLUMN billing.prices.sell_via IS 'The methods one of our prices sells through: card (the merchant''s card vault and charge PSPs) and solana. NULL: every method the merchant has. Settable like archived; it is not a term.';

CREATE INDEX prices_archived_created_at_id_idx ON billing.prices (merchant_id, archived, created_at DESC, id DESC);
CREATE INDEX prices_created_at_id_idx ON billing.prices (merchant_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX prices_key_key ON billing.prices (merchant_id, product_id, key) WHERE NOT archived;
CREATE INDEX prices_public_id_idx ON billing.prices (merchant_id, sha256(public_id::bytea));

CREATE FUNCTION billing.assign_price_public_id() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing' AS $$
DECLARE product_key text;
BEGIN
    SELECT key INTO STRICT product_key FROM billing.products
    WHERE merchant_id = NEW.merchant_id AND id = NEW.product_id;
    NEW.public_id := billing.escape_price_key(product_key) || ':'
        || billing.escape_price_key(NEW.key) || ':v' || (NEW.revision + 1)::text;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.assign_price_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.revision IS NULL THEN
        SELECT COALESCE(MAX(revision) + 1, 0) INTO NEW.revision FROM billing.prices
        WHERE merchant_id=NEW.merchant_id AND product_id=NEW.product_id AND key=NEW.key;
    END IF;
    RETURN NEW;
END;
$$;

-- An unassigned pointer is a real historical state, not the previous active
-- offer. UUIDv7 movement IDs give deterministic tie ordering within a timestamp.
CREATE FUNCTION billing.catalog_price_retired() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF NOT OLD.archived AND (NEW.archived OR NEW.key <> OLD.key) THEN
  INSERT INTO billing.price_key_movements(merchant_id,key,price_id,effective_at,archived) VALUES(OLD.merchant_id,OLD.key,OLD.id,clock_timestamp(),true);
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION billing.step_price_key() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='UPDATE' AND (to_jsonb(NEW)-ARRAY['updated_at']) IS NOT DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at']) THEN
        RETURN NULL;
    END IF;
    INSERT INTO billing.price_keys AS k (merchant_id, product_id, key, revision)
    VALUES (NEW.merchant_id, NEW.product_id, NEW.key, 1)
    ON CONFLICT (merchant_id, product_id, key) DO UPDATE SET revision = k.revision + 1, updated_at = now();
    RETURN NULL;
END $$;

CREATE TRIGGER catalog_authored_price BEFORE INSERT OR DELETE OR UPDATE ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_price_retired AFTER UPDATE OF archived, key ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_price_retired();
CREATE TRIGGER immutable_price_terms BEFORE DELETE OR UPDATE ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('archived', 'updated_at', 'deleted_at', 'sell_via');
CREATE TRIGGER no_price_truncate BEFORE TRUNCATE ON billing.prices
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER price_key_revision AFTER INSERT OR UPDATE ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.step_price_key();
CREATE TRIGGER price_revision BEFORE INSERT ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.assign_price_revision();
CREATE TRIGGER price_z_public_id BEFORE INSERT ON billing.prices
    FOR EACH ROW EXECUTE FUNCTION billing.assign_price_public_id();

CREATE TABLE billing.price_keys (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    key text NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, product_id, key),
    CONSTRAINT price_keys_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.price_keys IS 'One row per price key of a product: its revision advances on every change to any of its versions or their PSP links. Written only by the prices and price_psp_bindings triggers, which rebuild it on restore.';

CREATE TABLE billing.price_key_movements (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    key text NOT NULL,
    price_id uuid NOT NULL,
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    archived boolean NOT NULL,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT price_key_movements_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.price_key_movements IS 'Append-only log of when a price key''s current pointer moved to which price row. History, not row identity — a row can appear more than once (reactivation). Retention: permanent, never pruned.';

CREATE INDEX price_key_movements_key_effective_at_idx ON billing.price_key_movements (merchant_id, key, effective_at DESC);
CREATE INDEX price_key_movements_price_id_idx ON billing.price_key_movements (merchant_id, price_id);

-- Account identity owns provider price objects. Labels live only on psps.
CREATE TABLE billing.price_psp_bindings (
    merchant_id uuid NOT NULL,
    price_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    plan_id text,
    price_ref text,
    recurring_billing_option_id text,
    plan_pda text,
    flex_id text,
    configuration jsonb NOT NULL,
    PRIMARY KEY (merchant_id, price_id, psp_id),
    CONSTRAINT price_psp_bindings_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_psp_bindings_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_psp_bindings_plan_ref_nonempty_check CHECK (plan_id IS NULL OR btrim(plan_id) <> ''),
    CONSTRAINT price_psp_bindings_price_ref_nonempty_check CHECK (price_ref IS NULL OR btrim(price_ref) <> ''),
    CONSTRAINT price_psp_bindings_configuration_identity_check CHECK (NOT (configuration ?| ARRAY['psp_id', 'rail', 'plan_id', 'price_id', 'recurring_billing_option_id', 'plan_pda', 'flex_id'])),
    CONSTRAINT price_psp_bindings_configuration_object_check CHECK (jsonb_typeof(configuration) = 'object')
);
COMMENT ON TABLE billing.price_psp_bindings IS 'A price''s objects at its PSPs: the one provider object a provider price mirrors, or the Solana plans OpenRails published for one of ours. Each rail uses its own reference column.';

CREATE UNIQUE INDEX price_psp_bindings_psp_id_plan_id_key ON billing.price_psp_bindings (merchant_id, psp_id, plan_id)
    WHERE plan_id IS NOT NULL;
CREATE UNIQUE INDEX price_psp_bindings_psp_id_plan_pda_key ON billing.price_psp_bindings (merchant_id, psp_id, plan_pda)
    WHERE plan_pda IS NOT NULL;
CREATE UNIQUE INDEX price_psp_bindings_psp_id_price_ref_key ON billing.price_psp_bindings (merchant_id, psp_id, price_ref)
    WHERE price_ref IS NOT NULL;
CREATE UNIQUE INDEX price_psp_bindings_psp_id_recurring_billing_option_id_key ON billing.price_psp_bindings (merchant_id, psp_id, recurring_billing_option_id)
    WHERE recurring_billing_option_id IS NOT NULL;
CREATE UNIQUE INDEX provider_price_one_binding_key ON billing.price_psp_bindings (merchant_id, price_id)
    WHERE plan_pda IS NULL AND (plan_id IS NOT NULL OR price_ref IS NOT NULL OR flex_id IS NOT NULL OR recurring_billing_option_id IS NOT NULL);

CREATE FUNCTION billing.guard_price_binding_owner() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE owned boolean; binding_rail text;
BEGIN
    SELECT p.provider_owned INTO owned FROM billing.prices p WHERE p.merchant_id = NEW.merchant_id AND p.id = NEW.price_id FOR UPDATE;
    SELECT s.rail INTO binding_rail FROM billing.psps s WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.psp_id;
    IF owned IS DISTINCT FROM (binding_rail IN ('nmi', 'stripe', 'ccbill')) THEN
        RAISE EXCEPTION 'price % cannot bind a % object: our prices bind only Solana plans, provider prices only their provider''s object', NEW.price_id, binding_rail USING ERRCODE = '23514';
    END IF;
    IF owned AND EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = NEW.merchant_id AND b.price_id = NEW.price_id AND b.psp_id <> NEW.psp_id) THEN
        RAISE EXCEPTION 'provider price % binds one PSP', NEW.price_id USING ERRCODE = '23514';
    END IF;
    IF owned AND (NEW.plan_pda IS NOT NULL
        OR (binding_rail = 'nmi' AND (NEW.plan_id IS NULL OR NEW.price_ref IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL))
        OR (binding_rail = 'stripe' AND (NEW.price_ref IS NULL OR NEW.plan_id IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL))
        OR (binding_rail = 'ccbill' AND (NEW.plan_id IS NOT NULL OR NEW.price_ref IS NOT NULL OR (NEW.flex_id IS NULL AND NEW.recurring_billing_option_id IS NULL)))) THEN
        RAISE EXCEPTION 'provider price % must name its provider object on %', NEW.price_id, binding_rail USING ERRCODE = '23514';
    END IF;
    IF NOT owned AND (NEW.price_ref IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL OR (NEW.plan_id IS NOT NULL AND NEW.plan_pda IS NULL)) THEN
        RAISE EXCEPTION 'our price % binds only a published Solana plan', NEW.price_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION billing.step_price_key_binding() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE binding billing.price_psp_bindings%ROWTYPE;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NULL;
    END IF;
    IF TG_OP='DELETE' THEN binding := OLD; ELSE binding := NEW; END IF;
    UPDATE billing.price_keys k SET revision = k.revision + 1, updated_at = now()
      FROM billing.prices p
     WHERE p.merchant_id = binding.merchant_id AND p.id = binding.price_id
       AND k.merchant_id = p.merchant_id AND k.product_id = p.product_id AND k.key = p.key;
    RETURN NULL;
END $$;

CREATE TRIGGER catalog_authored_binding BEFORE INSERT OR DELETE OR UPDATE ON billing.price_psp_bindings
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER price_binding_owner BEFORE INSERT OR UPDATE ON billing.price_psp_bindings
    FOR EACH ROW EXECUTE FUNCTION billing.guard_price_binding_owner();
CREATE TRIGGER price_key_binding_revision AFTER INSERT OR DELETE OR UPDATE ON billing.price_psp_bindings
    FOR EACH ROW EXECUTE FUNCTION billing.step_price_key_binding();

CREATE TABLE billing.price_migrations (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    from_price_id uuid,
    from_product_id uuid,
    from_price_key text CHECK (from_price_key <> ''),
    to_price_id uuid NOT NULL,
    effective_at timestamptz NOT NULL,
    fallback_policy text NOT NULL,
    subscriptions_matched integer NOT NULL,
    subscriptions_skipped integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    canceled_at timestamptz,
    destructive_action_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT price_migrations_destructive_action_fkey FOREIGN KEY (merchant_id, destructive_action_id) REFERENCES billing.destructive_actions (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_migrations_from_price_id_fkey FOREIGN KEY (merchant_id, from_price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_migrations_from_product_id_fkey FOREIGN KEY (merchant_id, from_product_id) REFERENCES billing.products (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_migrations_to_price_id_fkey FOREIGN KEY (merchant_id, to_price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_migrations_source_check CHECK (
        (from_price_id IS NULL) = (from_price_key IS NOT NULL)
        AND (from_product_id IS NULL) = (from_price_key IS NULL)
    ),
    CHECK (fallback_policy IN ('keep_grandfathered', 'cancel_at_period_end')),
    CONSTRAINT price_migrations_counts_check CHECK (subscriptions_matched >= 0 AND subscriptions_skipped BETWEEN 0 AND subscriptions_matched)
);
COMMENT ON TABLE billing.price_migrations IS 'One move of subscribers from a price (from_price_id), or from every version of a price key but the target (from_product_id, from_price_key), to to_price_id at each subscription''s first renewal on or after effective_at. Matched and skipped are facts of creation (a skipped subscription gets no scheduled change); progress is counted from the scheduled_changes that carry price_migration_id. Its command settles when its last move resolves. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.price_migrations.fallback_policy IS 'The operator''s choice for subscriptions whose provider cannot be moved from OpenRails (CCBill, Solana, an unreachable NMI schedule): keep_grandfathered leaves them on the old price; cancel_at_period_end records that they should end, which OpenRails does not do.';
COMMENT ON COLUMN billing.price_migrations.canceled_at IS 'When its still-scheduled changes were canceled; a canceled migration is never re-driven.';
COMMENT ON COLUMN billing.price_migrations.destructive_action_id IS 'Original admitted cohort action; unresolved scheduled or blocked changes retain its reservation.';

CREATE INDEX price_migrations_created_at_id_idx ON billing.price_migrations (merchant_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX price_migrations_destructive_action_key ON billing.price_migrations (merchant_id, destructive_action_id)
    WHERE destructive_action_id IS NOT NULL;
CREATE INDEX price_migrations_from_key_created_at_id_idx ON billing.price_migrations (merchant_id, from_product_id, from_price_key, created_at DESC, id DESC)
    WHERE from_price_key IS NOT NULL;
CREATE INDEX price_migrations_from_price_id_idx ON billing.price_migrations (merchant_id, from_price_id)
    WHERE from_price_id IS NOT NULL;
CREATE INDEX price_migrations_to_price_id_idx ON billing.price_migrations (merchant_id, to_price_id);

CREATE TABLE billing.catalog_meters (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    event_type text,
    value_property text,
    aggregation text,
    unit text,
    group_by jsonb NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 1),
    PRIMARY KEY (merchant_id, key),
    CONSTRAINT catalog_meters_key_nonempty_check CHECK (btrim(key) <> ''),
    CHECK (aggregation IS NULL OR aggregation IN ('sum', 'count', 'max', 'min', 'unique_count', 'latest'))
);
COMMENT ON TABLE billing.catalog_meters IS 'Billing meter registry. Meters are billed-later usage streams, distinct from usage limits.';
COMMENT ON COLUMN billing.catalog_meters.event_type IS 'Usage event type for rate-card meters; defaults to key when omitted.';
COMMENT ON COLUMN billing.catalog_meters.value_property IS 'JSON/dimension property carrying the numeric quantity to aggregate.';
COMMENT ON COLUMN billing.catalog_meters.aggregation IS 'Aggregation mode for rate-card meters.';
COMMENT ON COLUMN billing.catalog_meters.group_by IS 'Dimension name -> event metadata/dimension property mapping for matrix pricing.';
COMMENT ON COLUMN billing.catalog_meters.revision IS 'Advances on every change to the meter or its rate card.';

CREATE INDEX catalog_meters_event_type_idx ON billing.catalog_meters (merchant_id, COALESCE(NULLIF(event_type, ''::text), key));

CREATE TRIGGER catalog_authored_meter BEFORE INSERT OR DELETE OR UPDATE ON billing.catalog_meters
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_meter_revision BEFORE INSERT OR UPDATE ON billing.catalog_meters
    FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();

CREATE TABLE billing.catalog_rate_cards (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    product_id uuid NOT NULL,
    ordinal integer NOT NULL,
    meter_key text,
    payment_term text CHECK (payment_term IN ('in_advance', 'in_arrears')),
    filter jsonb NOT NULL,
    allowance jsonb,
    price jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT catalog_rate_cards_meter_key_fkey FOREIGN KEY (merchant_id, meter_key) REFERENCES billing.catalog_meters (merchant_id, key) ON DELETE RESTRICT,
    CONSTRAINT catalog_rate_cards_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT catalog_rate_cards_ordinal_positive_check CHECK (ordinal >= 1),
    CONSTRAINT catalog_rate_cards_payment_term_metered_check CHECK ((meter_key IS NULL) = (payment_term IS NOT NULL))
);
COMMENT ON TABLE billing.catalog_rate_cards IS 'Rate cards: product usage and flat prices expressed as charge-model JSON. The only metered-pricing engine.';
COMMENT ON COLUMN billing.catalog_rate_cards.payment_term IS 'When a flat fee is collected; null for a metered card, whose charges are posted when usage is recorded.';

CREATE UNIQUE INDEX catalog_rate_cards_meter_key_key ON billing.catalog_rate_cards (merchant_id, meter_key)
    WHERE meter_key IS NOT NULL;
CREATE UNIQUE INDEX catalog_rate_cards_product_id_ordinal_key ON billing.catalog_rate_cards (merchant_id, product_id, ordinal);

CREATE TRIGGER catalog_authored_rate_card BEFORE INSERT OR DELETE OR UPDATE ON billing.catalog_rate_cards
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

CREATE TABLE billing.catalog_applications (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    application_id text NOT NULL CHECK (length(application_id) BETWEEN 1 AND 128),
    schema_version bigint NOT NULL,
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    base_revision bigint NOT NULL CHECK (base_revision >= 0),
    applied_revision bigint NOT NULL,
    result jsonb NOT NULL CHECK (octet_length(result::text) <= 16384),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, application_id),
    CONSTRAINT catalog_applications_applied_revision_check CHECK (applied_revision = (base_revision + 1))
);
COMMENT ON TABLE billing.catalog_applications IS 'Permanent compact replay receipts, retained and restored with the merchant billing book; never expire by HTTP idempotency TTL. Retention: permanent, never pruned.';

CREATE INDEX catalog_applications_merchant_hash_idx ON billing.catalog_applications (merchant_id, request_sha256, applied_revision DESC);

CREATE FUNCTION billing.guard_catalog_application_receipt() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'catalog application receipts are immutable' USING ERRCODE='23514';
END $$;

CREATE TRIGGER immutable_catalog_application_receipt BEFORE DELETE OR UPDATE ON billing.catalog_applications
    FOR EACH ROW EXECUTE FUNCTION billing.guard_catalog_application_receipt();

CREATE TABLE billing.catalog_field_owners (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    object text NOT NULL CHECK (object IN ('product', 'price', 'meter')),
    key text NOT NULL CHECK (key <> '' AND octet_length(key) <= 255),
    price_key text NOT NULL,
    field text NOT NULL,
    manager text NOT NULL CHECK (manager IN ('apply', 'edit')),
    actor text NOT NULL CHECK (actor <> '' AND octet_length(actor) <= 255),
    set_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, object, key, price_key, field, manager),
    CONSTRAINT catalog_field_owners_field_check CHECK (
        (object = 'product' AND field IN ('display_name', 'description', 'tier_group', 'tier_rank', 'archived', 'ownership', 'entitlements', 'required_entitlements', 'credit_grant', 'rate_cards'))
        OR (object = 'price' AND field IN ('currency', 'unit_amount', 'access_duration_hours', 'billing_interval_hours', 'billing_interval_months', 'provider_access_default', 'trial_unit_amount', 'trial_duration_hours', 'customer_amount', 'quantity', 'archived', 'sell_via', 'provider'))
        OR (object = 'meter' AND field IN ('event_type', 'value_property', 'aggregation', 'unit', 'group_by'))
    ),
    CONSTRAINT catalog_field_owners_price_key_check CHECK ((object = 'price') = (price_key <> '') AND octet_length(price_key) <= 255)
);
COMMENT ON TABLE billing.catalog_field_owners IS 'Which manager set each catalog field: apply (a catalog document) or edit (an individual edit), and who and when. Both own a field set to an equal value. A document skips an object whose field it names an edit set differently.';
COMMENT ON COLUMN billing.catalog_field_owners.key IS 'The product or meter key; a price''s product key.';
COMMENT ON COLUMN billing.catalog_field_owners.price_key IS 'A price''s key; empty for a product or meter.';
COMMENT ON COLUMN billing.catalog_field_owners.actor IS 'apply: the document''s application id; edit: the signed-in user, or api.';

CREATE TABLE billing.catalog_restore_receipts (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    rows bigint NOT NULL CHECK (rows >= 0),
    restored_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.catalog_restore_receipts IS 'Local catalog snapshot restore receipts; one completed atomic restore per merchant. Identical artifact retries are no-ops even after later catalog edits. Excluded from exported snapshots. Retention: permanent, never pruned.';

CREATE TRIGGER immutable_catalog_restore_receipt BEFORE DELETE OR UPDATE ON billing.catalog_restore_receipts
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER no_catalog_restore_receipt_truncate BEFORE TRUNCATE ON billing.catalog_restore_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- ---------------------------------------------------------------------------
-- Payment methods
-- ---------------------------------------------------------------------------

CREATE TABLE billing.payment_methods (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid,
    custodian text NOT NULL,
    custodian_id uuid,
    rail_customer_ref text CHECK (rail_customer_ref <> ''),
    rail_method_ref text CHECK (rail_method_ref <> ''),
    card_brand text,
    card_last4 text CHECK (card_last4 ~ '^[0-9]{4}$'),
    card_exp_month smallint CHECK (card_exp_month BETWEEN 1 AND 12),
    card_exp_year smallint CHECK (card_exp_year BETWEEN 2000 AND 2199),
    metadata jsonb,
    fingerprint text CHECK (fingerprint <> ''),
    network_token_id text CHECK (network_token_id <> ''),
    network_token_status text,
    network_token_par text CHECK (network_token_par <> ''),
    charge_via text NOT NULL CHECK (charge_via IN ('pan_proxy', 'network_token')),
    park_reason text CHECK (park_reason <> ''),
    parked_at timestamptz,
    account_updater_checked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    status text NOT NULL,
    replaced_by_id uuid,
    contact_cardholder_at timestamptz,
    setup_ref text,
    capture jsonb,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT payment_methods_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT payment_methods_custodian_id_custodian_fkey FOREIGN KEY (merchant_id, custodian_id, custodian) REFERENCES billing.custodians (merchant_id, id, kind) ON DELETE RESTRICT,
    CONSTRAINT payment_methods_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT payment_methods_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT payment_methods_replaced_by_id_fkey FOREIGN KEY (merchant_id, customer_id, replaced_by_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id),
    CONSTRAINT payment_methods_replaced_by_check CHECK ((status = 'replaced') = (replaced_by_id IS NOT NULL) AND replaced_by_id <> id),
    CONSTRAINT payment_methods_psp_custody_check CHECK ((custodian = 'psp') = (psp_id IS NOT NULL)),
    CONSTRAINT payment_methods_capture_check CHECK (
        capture IS NULL
        OR (custodian = 'hyperswitch' AND jsonb_typeof(capture) = 'object' AND octet_length(capture::text) <= 16384)
    ),
    CHECK (custodian IN ('psp', 'basis_theory', 'hyperswitch', 'platform')),
    CONSTRAINT payment_methods_custodian_identity_check CHECK ((custodian = 'psp') = (custodian_id IS NULL)),
    CONSTRAINT payment_methods_card_expiry_pair_check CHECK ((card_exp_month IS NULL) = (card_exp_year IS NULL)),
    CHECK (network_token_status IN ('active', 'inactive', 'suspended', 'deleted')),
    CONSTRAINT payment_methods_setup_ref_check CHECK ((status = 'requires_action') = (setup_ref IS NOT NULL) AND setup_ref <> ''),
    CHECK (status IN ('requires_action', 'active', 'closed', 'replaced', 'removed'))
);
COMMENT ON TABLE billing.payment_methods IS 'The card account a customer chose, held at one PSP or custodian. Its card is the latest of its payment_method_versions; its issuer may reissue it under the same method.';
COMMENT ON COLUMN billing.payment_methods.rail IS 'Rail the instrument is charged on: nmi or stripe.';
COMMENT ON COLUMN billing.payment_methods.psp_id IS 'The PSP that holds the instrument when custodian = psp. NULL for a card a third-party custodian holds: routing picks the PSP for each charge.';
COMMENT ON COLUMN billing.payment_methods.custodian IS 'Who holds the instrument: psp (the processor itself), basis_theory or hyperswitch (the merchant''s third-party vault), or platform (the deployment''s platform vault, in the merchant''s partition); a vault proxies the card to the processor at charge time.';
COMMENT ON COLUMN billing.payment_methods.rail_customer_ref IS 'Customer-scope rail handle (NMI customer_vault_id, one per card); empty when the customer scope lives in psp_customers (Stripe).';
COMMENT ON COLUMN billing.payment_methods.rail_method_ref IS 'Instrument-scope handle: NMI billing_id, Stripe pm_, or the custodian token.';
COMMENT ON COLUMN billing.payment_methods.card_brand IS 'Card brand as the provider or custodian reports it.';
COMMENT ON COLUMN billing.payment_methods.card_last4 IS 'Last four digits of the card number.';
COMMENT ON COLUMN billing.payment_methods.card_exp_month IS 'Expiry month, 1-12; the card is valid through the end of that month.';
COMMENT ON COLUMN billing.payment_methods.card_exp_year IS 'Four-digit expiry year.';
COMMENT ON COLUMN billing.payment_methods.metadata IS 'Billing details the customer entered with the card.';
COMMENT ON COLUMN billing.payment_methods.fingerprint IS 'The holder''s fingerprint of the card''s current number; earlier numbers'' are in payment_method_versions.';
COMMENT ON COLUMN billing.payment_methods.network_token_id IS 'Custodian network token id; empty when none is provisioned.';
COMMENT ON COLUMN billing.payment_methods.network_token_status IS 'Network token lifecycle status; empty when none is provisioned.';
COMMENT ON COLUMN billing.payment_methods.network_token_par IS 'Payment account reference from network token provisioning.';
COMMENT ON COLUMN billing.payment_methods.charge_via IS 'How a custodian card reaches the processor: pan_proxy or network_token.';
COMMENT ON COLUMN billing.payment_methods.park_reason IS 'Non-empty while the method cannot be charged for a holder-side reason (a deletion in flight, a custody token gone): charges fail loudly and nothing is canceled because of it.';
COMMENT ON COLUMN billing.payment_methods.parked_at IS 'When the instrument was parked; NULL when it is not.';
COMMENT ON COLUMN billing.payment_methods.account_updater_checked_at IS 'When the card''s vault last answered an account-updater request for it; NULL when never.';
COMMENT ON COLUMN billing.payment_methods.status IS 'requires_action (being saved: the customer completes the bank''s authentication, setup_ref); active; closed (the bank closed the account); replaced (another method took its place, replaced_by_id); removed. The last three are final.';
COMMENT ON COLUMN billing.payment_methods.replaced_by_id IS 'The method that replaced this one, when status is replaced.';
COMMENT ON COLUMN billing.payment_methods.contact_cardholder_at IS 'When the issuer last asked for the cardholder to be contacted; cleared by the next change to the card.';
COMMENT ON COLUMN billing.payment_methods.setup_ref IS 'The provider''s setup awaiting the customer (a Stripe SetupIntent) while status is requires_action.';
COMMENT ON COLUMN billing.payment_methods.capture IS 'Private accepted custodian card-save binding, encrypted SDK authorization until completion, then token hash and canonical method identity. Never payment authority.';

CREATE INDEX payment_methods_custodian_id_network_token_id_idx ON billing.payment_methods (merchant_id, custodian_id, network_token_id)
    WHERE custodian <> 'psp' AND network_token_id IS NOT NULL;
CREATE INDEX payment_methods_custodian_id_rail_method_ref_idx ON billing.payment_methods (merchant_id, custodian_id, rail_method_ref)
    WHERE custodian <> 'psp';
CREATE INDEX payment_methods_customer_id_created_at_id_idx ON billing.payment_methods (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX payment_methods_fingerprint_idx ON billing.payment_methods (merchant_id, fingerprint)
    WHERE fingerprint IS NOT NULL;
CREATE INDEX payment_methods_psp_id_idx ON billing.payment_methods (psp_id);
CREATE UNIQUE INDEX payment_methods_psp_instrument_key ON billing.payment_methods (merchant_id, psp_id, custodian_id, rail_customer_ref, rail_method_ref) NULLS NOT DISTINCT
    WHERE rail_customer_ref IS NOT NULL OR rail_method_ref IS NOT NULL;
CREATE INDEX payment_methods_rail_rail_method_ref_idx ON billing.payment_methods (rail, rail_method_ref);
CREATE INDEX payment_methods_replaced_by_id_idx ON billing.payment_methods (merchant_id, replaced_by_id)
    WHERE replaced_by_id IS NOT NULL;
CREATE INDEX payment_methods_requires_action_idx ON billing.payment_methods (merchant_id, created_at)
    WHERE status = 'requires_action';

CREATE FUNCTION billing.guard_payment_method_status() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status = 'requires_action' AND NEW.status IN ('requires_action', 'active', 'replaced', 'removed')
        AND (NEW.status <> 'replaced' OR OLD.capture IS NOT NULL) THEN
        RETURN NEW;
    END IF;
    IF OLD.status <> 'active' AND (NEW.status, NEW.replaced_by_id) IS DISTINCT FROM (OLD.status, OLD.replaced_by_id) THEN
        RAISE EXCEPTION 'payment method % is %, which is final', OLD.id, OLD.status USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'requires_action' AND OLD.status <> 'requires_action' THEN
        RAISE EXCEPTION 'payment method % was already saved', OLD.id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.release_inactive_default() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status = 'active' AND NEW.status <> 'active' THEN
        UPDATE billing.money_settings SET default_payment_method_id = NULL, updated_at = now()
        WHERE merchant_id = NEW.merchant_id AND customer_id = NEW.customer_id AND default_payment_method_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER guard_payment_method_status BEFORE UPDATE OF status, replaced_by_id ON billing.payment_methods
    FOR EACH ROW EXECUTE FUNCTION billing.guard_payment_method_status();
CREATE TRIGGER release_inactive_default AFTER UPDATE OF status ON billing.payment_methods
    FOR EACH ROW EXECUTE FUNCTION billing.release_inactive_default();

CREATE TABLE billing.payment_method_versions (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    source text NOT NULL,
    kind text NOT NULL,
    event_ref text NOT NULL CHECK (event_ref <> ''),
    psp_id uuid,
    custodian_id uuid,
    rail_customer_ref text CHECK (rail_customer_ref <> ''),
    rail_method_ref text CHECK (rail_method_ref <> ''),
    card_brand text CHECK (card_brand <> ''),
    card_last4 text CHECK (card_last4 ~ '^[0-9]{4}$'),
    card_exp_month smallint CHECK (card_exp_month BETWEEN 1 AND 12),
    card_exp_year smallint CHECK (card_exp_year BETWEEN 2000 AND 2199),
    fingerprint text CHECK (fingerprint <> ''),
    network_token_id text CHECK (network_token_id <> ''),
    network_token_status text,
    network_token_par text CHECK (network_token_par <> ''),
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT payment_method_versions_event_key UNIQUE (merchant_id, payment_method_id, source, event_ref),
    CONSTRAINT payment_method_versions_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT payment_method_versions_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT payment_method_versions_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CHECK (source IN ('customer_save', 'customer_edit', 'stripe_updater', 'nmi_acu', 'basis_theory_updater', 'hyperswitch_updater', 'network_token', 'provider_read')),
    CHECK (kind IN ('saved', 'updated', 'brand_changed', 'closed', 'contact_cardholder')),
    CONSTRAINT payment_method_versions_holder_check CHECK ((psp_id IS NULL) <> (custodian_id IS NULL)),
    CONSTRAINT payment_method_versions_card_expiry_pair_check CHECK ((card_exp_month IS NULL) = (card_exp_year IS NULL)),
    CHECK (network_token_status IN ('active', 'inactive', 'suspended', 'deleted'))
);
COMMENT ON TABLE billing.payment_method_versions IS 'Append-only history of the card behind a payment method: one row per save, customer edit, updater or network change, closure or contact advice, with the card as its holder reported it at that version. Replays of one source event are one row. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.payment_method_versions.source IS 'Who reported the change: customer_save, customer_edit, stripe_updater, nmi_acu, basis_theory_updater, hyperswitch_updater, network_token or provider_read (OpenRails read the holder).';
COMMENT ON COLUMN billing.payment_method_versions.kind IS 'saved; updated (a new number, expiry, holder handle or network token, under the same brand); brand_changed; closed; contact_cardholder.';
COMMENT ON COLUMN billing.payment_method_versions.event_ref IS 'The source''s event, notice or operation; one row per method, source and event.';
COMMENT ON COLUMN billing.payment_method_versions.psp_id IS 'The PSP holding the card at this version, for a PSP-held card; with custodian_id, the scope of its fingerprint.';
COMMENT ON COLUMN billing.payment_method_versions.custodian_id IS 'The custodian holding the card at this version, for a custodian-held card.';
COMMENT ON COLUMN billing.payment_method_versions.fingerprint IS 'The holder''s fingerprint of the card number at this version; a reissued number has a new one.';
COMMENT ON COLUMN billing.payment_method_versions.network_token_par IS 'Payment account reference, where the holder reports one.';
COMMENT ON COLUMN billing.payment_method_versions.effective_at IS 'When the change was learned.';

CREATE INDEX payment_method_versions_custodian_id_idx ON billing.payment_method_versions (merchant_id, custodian_id)
    WHERE custodian_id IS NOT NULL;
CREATE INDEX payment_method_versions_customer_id_fingerprint_idx ON billing.payment_method_versions (merchant_id, customer_id, fingerprint)
    WHERE fingerprint IS NOT NULL;
CREATE INDEX payment_method_versions_payment_method_id_effective_at_idx ON billing.payment_method_versions (merchant_id, payment_method_id, effective_at, id);
CREATE INDEX payment_method_versions_psp_id_idx ON billing.payment_method_versions (merchant_id, psp_id)
    WHERE psp_id IS NOT NULL;

CREATE FUNCTION billing.guard_payment_method_version_update() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RAISE EXCEPTION 'card version % is append-only', OLD.id USING ERRCODE = '23514';
END;
$$;

CREATE FUNCTION billing.subscription_history_card_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'card_version',0);
    INSERT INTO billing.subscription_billing_changes
        (merchant_id,subscription_id,kind,decision,from_payment_method_id,to_payment_method_id,method_version_id,origin,actor,executor,operation_id)
    SELECT s.merchant_id,s.id,'card_version',NEW.kind,NEW.payment_method_id,NEW.payment_method_id,NEW.id,
        COALESCE(a->>'origin',NEW.source),a->>'actor',COALESCE(a->>'executor',NEW.source),(a->>'operation_id')::uuid
    FROM billing.subscriptions s
    WHERE s.merchant_id=NEW.merchant_id AND s.customer_id=NEW.customer_id
      AND billing.subscription_payment_method_id(s.merchant_id,s.customer_id,s.payment_method_id,s.price_id,s.rail,s.collection_policy)=NEW.payment_method_id
      AND s.deleted_at IS NULL AND s.status IN ('active','past_due','awaiting_method','unverified');
    RETURN NEW;
END;
$$;

CREATE TRIGGER guard_payment_method_version_update BEFORE UPDATE ON billing.payment_method_versions
    FOR EACH ROW EXECUTE FUNCTION billing.guard_payment_method_version_update();
CREATE TRIGGER trg_subscription_history_card_version AFTER INSERT ON billing.payment_method_versions
    FOR EACH ROW EXECUTE FUNCTION billing.subscription_history_card_version();

CREATE TABLE billing.stored_card_checks (
    merchant_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    due_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, payment_method_id),
    CONSTRAINT stored_card_checks_payment_method_id_fkey FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.stored_card_checks IS 'When the ahead-of-renewal pass next looks at a card a vault must be asked about; -infinity after its renewals changed. Derived from subscriptions; deleted when nothing is due.';

CREATE INDEX stored_card_checks_due_at_idx ON billing.stored_card_checks (due_at);

CREATE FUNCTION billing.mark_stored_card_check(p_merchant uuid, p_method uuid) RETURNS void
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
INSERT INTO billing.stored_card_checks (merchant_id, payment_method_id, due_at)
SELECT pm.merchant_id, pm.id, '-infinity' FROM billing.payment_methods pm
 WHERE pm.merchant_id = p_merchant AND pm.id = p_method
   AND pm.custodian IN ('basis_theory', 'platform') AND pm.status = 'active'
ON CONFLICT (merchant_id, payment_method_id) DO UPDATE SET due_at = '-infinity';
$$;

CREATE TABLE billing.money_settings (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    billing_mode text NOT NULL CHECK (billing_mode IN ('prepaid', 'arrears')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    tier text,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    credit_limit_amount bigint NOT NULL DEFAULT 0,
    default_payment_method_id uuid,
    collection_revoked_at timestamptz,
    credit_limit_override boolean NOT NULL DEFAULT false,
    usage_billing_anchor timestamptz,
    usage_next_bill_at timestamptz,
    usage_minimum_invoice_amount bigint CHECK (usage_minimum_invoice_amount >= 0),
    usage_collection_due boolean NOT NULL DEFAULT false,
    usage_due_at timestamptz,
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT money_settings_customer_id_default_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, default_payment_method_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE SET NULL (default_payment_method_id),
    CONSTRAINT money_settings_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT money_settings_credit_limit_amount_nonneg_check CHECK (credit_limit_amount >= 0)
);
COMMENT ON TABLE billing.money_settings IS 'Per-(merchant, customer, currency) spend policy and money-in config. Amount values use the row currency internal precision. Admission reads billing_mode + credit_limit_amount + the ledger balance; arrears owed exposure is derived from open invoices.';
COMMENT ON COLUMN billing.money_settings.currency IS 'System currency code (USD/EUR/JPY); the Go registry is the authority. Stablecoins and crypto tokens are payment assets, not account currencies.';
COMMENT ON COLUMN billing.money_settings.credit_limit_amount IS 'Admin-set arrears credit line in the row currency internal precision. 0 = no arrears capacity; prepaid balance may still be spent.';
COMMENT ON COLUMN billing.money_settings.default_payment_method_id IS 'The customer''s default card in this currency: it collects their invoices and pays every card subscription in the currency without a card of its own.';
COMMENT ON COLUMN billing.money_settings.collection_revoked_at IS 'When the customer revoked its collection agreement in the currency: from then admission spends only its balance, whatever billing_mode, until a new default card gives another.';
COMMENT ON COLUMN billing.money_settings.credit_limit_override IS 'False inherits the merchant usage-billing limit; true honors credit_limit_amount including zero.';
COMMENT ON COLUMN billing.money_settings.usage_billing_anchor IS 'Fixed monthly UTC anchor used for usage pricing and invoicing; threshold payments never alter it.';
COMMENT ON COLUMN billing.money_settings.usage_next_bill_at IS 'Next monthly usage bill date; null while nothing is pending, set again from the next accrual. A below-minimum balance advances it while its unbilled accruals carry forward.';
COMMENT ON COLUMN billing.money_settings.usage_due_at IS 'When the usage account is next checked: its next bill date, or now once spending crossed its threshold, an admission asked to collect or the merchant''s usage terms changed. Null: nothing is pending.';

CREATE INDEX money_settings_usage_due_at_idx ON billing.money_settings (usage_due_at, merchant_id, customer_id, currency)
    WHERE usage_due_at IS NOT NULL;

CREATE FUNCTION billing.default_card_marks_stored_card_check() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.default_payment_method_id IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.default_payment_method_id IS DISTINCT FROM NEW.default_payment_method_id) THEN
        PERFORM billing.mark_stored_card_check(NEW.merchant_id, NEW.default_payment_method_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.subscription_history_default_card() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.customer_id,'default:'||NEW.currency,0);
    PERFORM set_config('openrails.subscription_history','',true);
    IF billing.billing_restore_active(NEW.merchant_id) OR OLD.default_payment_method_id IS NOT DISTINCT FROM NEW.default_payment_method_id THEN RETURN NEW; END IF;
    INSERT INTO billing.subscription_billing_changes
        (merchant_id,subscription_id,kind,from_payment_method_id,to_payment_method_id,origin,actor,executor,operation_id)
    SELECT s.merchant_id,s.id,'default_card',OLD.default_payment_method_id,NEW.default_payment_method_id,
        a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid
    FROM billing.subscriptions s JOIN billing.prices p ON p.merchant_id=s.merchant_id AND p.id=s.price_id
    WHERE s.merchant_id=NEW.merchant_id AND s.customer_id=NEW.customer_id AND p.currency=NEW.currency
      AND s.payment_method_id IS NULL AND s.rail IN ('stripe','nmi') AND s.collection_policy IN ('engine','nmi_schedule')
      AND s.deleted_at IS NULL AND s.status IN ('active','past_due','awaiting_method','unverified');
    RETURN NEW;
END;
$$;

CREATE TRIGGER money_settings_mark_stored_card_check AFTER INSERT OR UPDATE OF default_payment_method_id ON billing.money_settings
    FOR EACH ROW EXECUTE FUNCTION billing.default_card_marks_stored_card_check();
CREATE TRIGGER trg_subscription_history_default AFTER UPDATE OF default_payment_method_id ON billing.money_settings
    FOR EACH ROW EXECUTE FUNCTION billing.subscription_history_default_card();

CREATE FUNCTION billing.subscription_payment_method_id(p_merchant uuid, p_customer uuid, p_own uuid, p_price uuid, p_rail text, p_policy text) RETURNS uuid
LANGUAGE sql STABLE PARALLEL SAFE AS $$
SELECT COALESCE(p_own, (
    SELECT ms.default_payment_method_id
    FROM billing.prices p
    JOIN billing.money_settings ms ON ms.merchant_id = p.merchant_id AND ms.customer_id = p_customer AND ms.currency = p.currency
    WHERE p.merchant_id = p_merchant AND p.id = p_price
      AND p_rail IN ('nmi', 'stripe') AND p_policy IN ('engine', 'nmi_schedule')))
$$;
COMMENT ON FUNCTION billing.subscription_payment_method_id(p_merchant uuid, p_customer uuid, p_own uuid, p_price uuid, p_rail text, p_policy text) IS 'The card a subscription charges: its own (payment_method_id), else on a card subscription its customer''s default for its price''s currency.';

-- ---------------------------------------------------------------------------
-- Subscriptions
-- ---------------------------------------------------------------------------

CREATE TABLE billing.subscriptions (
    id uuid NOT NULL DEFAULT uuidv7(),
    price_id uuid,
    product_id uuid NOT NULL,
    status text NOT NULL,
    rail text NOT NULL,
    collection_policy text NOT NULL CHECK (collection_policy IN ('provider', 'nmi_schedule', 'engine')),
    rail_subscription_id text CHECK (rail_subscription_id <> ''),
    payment_method_id uuid,
    current_period_starts_at timestamptz,
    current_period_ends_at timestamptz,
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    grace_ends_at timestamptz,
    last_retry_at timestamptz,
    retry_attempts integer DEFAULT 0,
    next_retry_at timestamptz,
    canceled_at timestamptz,
    cancel_type text,
    cancel_feedback text,
    gateway_response jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    tier_group text,
    deletion_scheduled_at timestamptz,
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    deleted_at timestamptz,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' ELSE NULL END) STORED,
    transient_retries integer NOT NULL DEFAULT 0 CHECK (transient_retries >= 0),
    lifecycle_rev bigint NOT NULL DEFAULT 0,
    row_version bigint NOT NULL DEFAULT 0,
    dunning_policy jsonb,
    access_duration_hours_snapshot integer,
    quantity integer,
    provider_access_default_snapshot boolean NOT NULL DEFAULT false,
    facts_authority text NOT NULL DEFAULT 'provider' CHECK (facts_authority IN ('host', 'provider')),
    due_recheck_at timestamptz,
    rail_subscription_key text GENERATED ALWAYS AS (billing.rail_subscription_key(rail, rail_subscription_id)) STORED,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT subscriptions_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id),
    CONSTRAINT subscriptions_price_id_product_id_fkey FOREIGN KEY (merchant_id, price_id, product_id) REFERENCES billing.prices (merchant_id, id, product_id),
    CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CONSTRAINT subscriptions_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT subscriptions_customer_id_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id),
    CONSTRAINT subscriptions_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT subscriptions_destructive_run_id_destructive_run_class_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT subscriptions_canceled_has_timestamp_check CHECK (status <> 'canceled' OR canceled_at IS NOT NULL),
    CONSTRAINT subscriptions_canceled_has_type_check CHECK (status <> 'canceled' OR cancel_type IS NOT NULL),
    CONSTRAINT subscriptions_canceled_no_retry_schedule_check CHECK (status <> 'canceled' OR (next_retry_at IS NULL AND grace_ends_at IS NULL)),
    CONSTRAINT subscriptions_past_due_has_period_end_check CHECK (status <> 'past_due' OR current_period_ends_at IS NOT NULL),
    CHECK (status IN ('pending', 'active', 'past_due', 'awaiting_method', 'canceled', 'unverified')),
    CONSTRAINT subscriptions_stop_pending_canceled_check CHECK (status = 'canceled' OR deletion_scheduled_at IS NULL),
    CONSTRAINT subscriptions_engine_binding_check CHECK (
        collection_policy <> 'engine'
        OR ((rail IN ('nmi', 'stripe') AND rail_subscription_id IS NULL) OR rail = 'solana')
    ),
    CONSTRAINT subscriptions_nmi_schedule_rail_check CHECK ((collection_policy = 'nmi_schedule') = (rail = 'nmi' AND collection_policy <> 'engine')),
    CONSTRAINT subscriptions_quantity_check CHECK (quantity IS NULL OR (quantity >= 1 AND collection_policy = 'engine' AND rail IN ('nmi', 'stripe'))),
    CONSTRAINT subscriptions_valid_period_check CHECK (
        current_period_starts_at IS NULL
        OR current_period_ends_at IS NULL
        OR current_period_starts_at < current_period_ends_at
    ),
    CONSTRAINT subscriptions_ended_not_before_canceled_check CHECK (ended_at IS NULL OR canceled_at IS NULL OR ended_at >= canceled_at),
    CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade')),
    CHECK (access_duration_hours_snapshot IS NULL OR access_duration_hours_snapshot > 0),
    CONSTRAINT subscriptions_provider_access_choice_check CHECK (NOT provider_access_default_snapshot OR access_duration_hours_snapshot IS NULL)
);
COMMENT ON TABLE billing.subscriptions IS 'Core subscription records tracking user billing relationships';
COMMENT ON COLUMN billing.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN billing.subscriptions.status IS 'Local lifecycle, answering one question: will we attempt to rebill? pending = not started; active/past_due/awaiting_method = yes; unverified = the provider must tell us; canceled = never again, with cancel_type saying why. Provider vocabulary is mapped onto this set at the boundary.';
COMMENT ON COLUMN billing.subscriptions.payment_method_id IS 'The subscription''s own card. NULL on a card subscription (NMI, or Stripe collected by OpenRails) follows its customer''s default card for the currency of its price.';
COMMENT ON COLUMN billing.subscriptions.tier_group IS 'Copied from products.tier_group by trg_subscriptions_set_tier_group. Backs subscriptions_customer_id_tier_group_key: one live subscription per (customer, tier group). Regrouping is refused while the product has a live plan change.';
COMMENT ON COLUMN billing.subscriptions.deletion_scheduled_at IS 'Set while the provider schedule of a canceled subscription may still bill: from the local cancel until its stop intent verifies the schedule gone. Holds the customer''s product and tier-group slot. On NMI it is also when the deferred delete runs.';
COMMENT ON COLUMN billing.subscriptions.psp_id IS 'PSP that produced this remote subscription mirror row. Required.';
COMMENT ON COLUMN billing.subscriptions.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.subscriptions.access_duration_hours_snapshot IS 'Access duration accepted for the current paid phase in hours; NULL means no scheduled expiry. Retained independently of repricing.';
COMMENT ON COLUMN billing.subscriptions.quantity IS 'Seats of a per-seat price: renewals bill the unit price times this. NULL unless the price is sold per seat; only an engine-owned NMI or Stripe subscription has seats.';
COMMENT ON COLUMN billing.subscriptions.provider_access_default_snapshot IS 'The accepted access choice for this phase: verified provider service-period end, otherwise the explicit access_duration_hours_snapshot. Existing accepted windows are never inferred to be defaults.';
COMMENT ON COLUMN billing.subscriptions.facts_authority IS 'host: a host''s legacy book landed the row and may still converge it; provider: provider evidence owns it, and a host book skips it. Moves only from host to provider.';
COMMENT ON COLUMN billing.subscriptions.due_recheck_at IS 'The due pass left the row unchanged; it is not due again before this instant.';
COMMENT ON COLUMN billing.subscriptions.rail_subscription_key IS 'rail_subscription_id as an identity (billing.rail_subscription_key): every lookup by provider id matches it. A provider contact matching it spelled otherwise rewrites rail_subscription_id to the provider''s spelling.';

CREATE INDEX subscriptions_canceled_at_id_idx ON billing.subscriptions (merchant_id, canceled_at DESC, id DESC)
    WHERE deleted_at IS NULL AND canceled_at IS NOT NULL;
CREATE INDEX subscriptions_canceled_at_idx ON billing.subscriptions (merchant_id, canceled_at)
    WHERE canceled_at IS NOT NULL;
CREATE INDEX subscriptions_created_at_id_idx ON billing.subscriptions (merchant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX subscriptions_current_period_ends_at_engine_due_global_idx ON billing.subscriptions (current_period_ends_at, merchant_id)
    WHERE collection_policy = 'engine' AND status IN ('active', 'past_due') AND deleted_at IS NULL;
CREATE INDEX subscriptions_current_period_ends_at_id_idx ON billing.subscriptions (merchant_id, current_period_ends_at, id)
    WHERE deleted_at IS NULL AND current_period_ends_at IS NOT NULL;
CREATE INDEX subscriptions_current_period_ends_at_next_retry_at_idx ON billing.subscriptions (merchant_id, current_period_ends_at, next_retry_at)
    WHERE collection_policy = 'engine' AND status IN ('active', 'past_due') AND deleted_at IS NULL;
CREATE INDEX subscriptions_current_period_ends_at_period_overdue_idx ON billing.subscriptions (current_period_ends_at)
    WHERE status = 'active';
CREATE INDEX subscriptions_current_period_ends_at_rebill_watch_idx ON billing.subscriptions (merchant_id, current_period_ends_at)
    WHERE status IN ('active', 'unverified', 'awaiting_method') AND collection_policy IN ('engine', 'nmi_schedule') AND deleted_at IS NULL;
CREATE INDEX subscriptions_customer_id_created_at_id_idx ON billing.subscriptions (merchant_id, customer_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX subscriptions_customer_id_product_id_key ON billing.subscriptions (merchant_id, customer_id, product_id)
    WHERE deleted_at IS NULL AND (status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') OR deletion_scheduled_at IS NOT NULL);
CREATE UNIQUE INDEX subscriptions_customer_id_tier_group_key ON billing.subscriptions (merchant_id, customer_id, tier_group)
    WHERE tier_group IS NOT NULL AND deleted_at IS NULL AND (status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') OR deletion_scheduled_at IS NOT NULL);
CREATE INDEX subscriptions_destructive_run_id_idx ON billing.subscriptions (merchant_id, destructive_run_id)
    WHERE destructive_run_id IS NOT NULL;
CREATE INDEX subscriptions_destructive_scale_idx ON billing.subscriptions (merchant_id, customer_id, price_id)
    WHERE status IN ('active', 'past_due') AND deleted_at IS NULL;
CREATE INDEX subscriptions_ended_at_idx ON billing.subscriptions (merchant_id, ended_at)
    WHERE ended_at IS NOT NULL;
CREATE INDEX subscriptions_gateway_order_id_idx ON billing.subscriptions (merchant_id, rail, (gateway_response ->> 'order_id'::text))
    WHERE (gateway_response ->> 'order_id') IS NOT NULL;
CREATE INDEX subscriptions_grace_ends_at_idx ON billing.subscriptions (grace_ends_at)
    WHERE grace_ends_at IS NOT NULL;
CREATE INDEX subscriptions_next_retry_at_rail_idx ON billing.subscriptions (next_retry_at, rail)
    WHERE status = 'past_due' AND next_retry_at IS NOT NULL;
CREATE INDEX subscriptions_payment_method_id_current_period_ends_at_idx ON billing.subscriptions (merchant_id, payment_method_id, current_period_ends_at)
    WHERE deleted_at IS NULL AND payment_method_id IS NOT NULL AND status IN ('active', 'past_due', 'awaiting_method');
CREATE INDEX subscriptions_payment_method_id_idx ON billing.subscriptions (merchant_id, payment_method_id)
    WHERE payment_method_id IS NOT NULL;
CREATE INDEX subscriptions_price_id_idx ON billing.subscriptions (merchant_id, price_id);
CREATE INDEX subscriptions_product_id_idx ON billing.subscriptions (merchant_id, product_id);
CREATE INDEX subscriptions_psp_id_idx ON billing.subscriptions (merchant_id, psp_id);
CREATE UNIQUE INDEX subscriptions_psp_id_rail_subscription_key_key ON billing.subscriptions (merchant_id, psp_id, rail_subscription_key)
    WHERE rail_subscription_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX subscriptions_started_at_idx ON billing.subscriptions (merchant_id, started_at);
CREATE INDEX subscriptions_status_idx ON billing.subscriptions (status);

CREATE FUNCTION billing.close_landed_park() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.rail_subscription_id IS NULL OR NEW.deleted_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    WITH closed AS (
        UPDATE billing.parked_provider_subscriptions p
        SET released_at = now(), released_by = 'landed'
        WHERE p.merchant_id = NEW.merchant_id AND p.psp_id = NEW.psp_id
          AND billing.rail_subscription_key(p.rail, p.subscription_ref) = NEW.rail_subscription_key
          AND p.released_at IS NULL
        RETURNING p.subscription_ref
    )
    UPDATE billing.reconciliation_findings f
    SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(), notified_at = NULL, notified_severity = NULL, updated_at = now()
    FROM closed
    WHERE f.merchant_id = NEW.merchant_id AND f.psp_id = NEW.psp_id AND f.finding_type = 'life.subscription.unattributed'
      AND f.subject_key = closed.subscription_ref AND f.status IN ('reconcile_required', 'requires_review');
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.preserve_subscription_collection_policy() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'billing', 'pg_catalog' AS $$
BEGIN
 IF NEW.collection_policy IS DISTINCT FROM OLD.collection_policy THEN
  RAISE EXCEPTION 'subscription collection policy is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscription_marks_stored_card_check() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL OR NEW.current_period_ends_at IS NULL OR NEW.status NOT IN ('active', 'past_due', 'awaiting_method') THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.current_period_ends_at IS NOT DISTINCT FROM NEW.current_period_ends_at AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.payment_method_id IS NOT DISTINCT FROM NEW.payment_method_id AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NULL;
    END IF;
    PERFORM billing.mark_stored_card_check(NEW.merchant_id,
        billing.subscription_payment_method_id(NEW.merchant_id, NEW.customer_id, NEW.payment_method_id, NEW.price_id, NEW.rail, NEW.collection_policy));
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.subscriptions_facts_authority_one_way() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.facts_authority = 'provider' AND NEW.facts_authority = 'host' THEN
        RAISE EXCEPTION 'subscription % facts authority moves only from host to provider', OLD.id
            USING ERRCODE = '23514', CONSTRAINT = 'subscriptions_facts_authority_one_way';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_lifecycle_single_writer() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    changed text[] := '{}';
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN changed := changed || format('status %s->%s', OLD.status, NEW.status); END IF;
    IF NEW.current_period_starts_at IS DISTINCT FROM OLD.current_period_starts_at THEN changed := changed || 'current_period_starts_at'::text; END IF;
    IF NEW.current_period_ends_at IS DISTINCT FROM OLD.current_period_ends_at THEN changed := changed || 'current_period_ends_at'::text; END IF;
    IF NEW.cancel_type IS DISTINCT FROM OLD.cancel_type THEN changed := changed || 'cancel_type'::text; END IF;
    IF NEW.canceled_at IS DISTINCT FROM OLD.canceled_at THEN changed := changed || 'canceled_at'::text; END IF;
    IF NEW.ended_at IS DISTINCT FROM OLD.ended_at THEN changed := changed || 'ended_at'::text; END IF;
    IF NEW.lifecycle_rev IS DISTINCT FROM OLD.lifecycle_rev AND NEW.lifecycle_rev <> OLD.lifecycle_rev + 1 THEN
        RAISE EXCEPTION 'subscription % lifecycle_rev must advance by one (% -> %)', OLD.id, OLD.lifecycle_rev, NEW.lifecycle_rev
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    IF cardinality(changed) > 0 AND NEW.lifecycle_rev = OLD.lifecycle_rev THEN
        RAISE EXCEPTION 'subscription % lifecycle fields changed outside a lifecycle decision: %', OLD.id, array_to_string(changed, ', ')
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_record_billing_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'update',OLD.row_version);
    IF OLD.payment_method_id IS DISTINCT FROM NEW.payment_method_id THEN
        INSERT INTO billing.subscription_billing_changes
            (merchant_id,subscription_id,kind,decision,from_payment_method_id,to_payment_method_id,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,'card_selection',a->>'decision',OLD.payment_method_id,NEW.payment_method_id,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    IF ROW(OLD.next_retry_at,OLD.last_retry_at,OLD.retry_attempts,OLD.transient_retries,OLD.grace_ends_at) IS DISTINCT FROM ROW(NEW.next_retry_at,NEW.last_retry_at,NEW.retry_attempts,NEW.transient_retries,NEW.grace_ends_at) THEN
        INSERT INTO billing.subscription_billing_changes
            (merchant_id,subscription_id,kind,decision,from_next_retry_at,to_next_retry_at,from_last_retry_at,to_last_retry_at,from_retry_attempts,to_retry_attempts,from_transient_retries,to_transient_retries,from_grace_ends_at,to_grace_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,'retry_schedule',a->>'decision',OLD.next_retry_at,NEW.next_retry_at,OLD.last_retry_at,NEW.last_retry_at,OLD.retry_attempts,NEW.retry_attempts,OLD.transient_retries,NEW.transient_retries,OLD.grace_ends_at,NEW.grace_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_record_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF TG_OP='INSERT' THEN
        a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'insert',0);
        INSERT INTO billing.subscription_status_transitions
            (merchant_id,subscription_id,to_status,cancel_type,decision,to_current_period_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,NEW.status,NEW.cancel_type,'created',NEW.current_period_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    ELSIF OLD.status IS DISTINCT FROM NEW.status OR OLD.current_period_ends_at IS DISTINCT FROM NEW.current_period_ends_at THEN
        a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'update',OLD.row_version);
        INSERT INTO billing.subscription_status_transitions
            (merchant_id,subscription_id,from_status,to_status,cancel_type,decision,from_current_period_ends_at,to_current_period_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,OLD.status,NEW.status,NEW.cancel_type,a->>'decision',OLD.current_period_ends_at,NEW.current_period_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_row_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    unchanged billing.subscriptions := NEW;
BEGIN
    unchanged.due_recheck_at := OLD.due_recheck_at;
    unchanged.facts_authority := OLD.facts_authority;
    unchanged.row_version := OLD.row_version;
    unchanged.destructive_run_class := OLD.destructive_run_class;
    unchanged.rail_subscription_key := OLD.rail_subscription_key;
    IF billing.rail_subscription_key(NEW.rail, NEW.rail_subscription_id) IS NOT DISTINCT FROM OLD.rail_subscription_key THEN
        unchanged.rail_subscription_id := OLD.rail_subscription_id;
    END IF;
    IF unchanged IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_set_tier_group() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    SELECT prod.tier_group INTO NEW.tier_group
    FROM billing.products AS prod
    WHERE prod.id = NEW.product_id AND prod.merchant_id = NEW.merchant_id
    FOR SHARE;
    RETURN NEW;
END;
$$;

-- Deferred to commit so a row that enters and leaves unverified in one
-- transaction (an import resolving what it seeded) is neither tracked nor read.
CREATE FUNCTION billing.subscriptions_track_unverified() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_status text;
    entered_at timestamp with time zone;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NULL; END IF;
    SELECT s.status, s.updated_at INTO current_status, entered_at
      FROM billing.subscriptions s
     WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.id AND s.deleted_at IS NULL;
    IF current_status = 'unverified' THEN
        INSERT INTO billing.subscription_verifications (merchant_id, subscription_id, unverified_at)
        VALUES (NEW.merchant_id, NEW.id, entered_at)
        ON CONFLICT (merchant_id, subscription_id) DO NOTHING;
        IF FOUND THEN
            PERFORM pg_notify('openrails_unverified:' || TG_TABLE_SCHEMA, NEW.merchant_id::text || ':' || NEW.id::text);
        END IF;
    ELSE
        DELETE FROM billing.subscription_verifications WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.void_ended_renewal_orders() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
        WITH voided AS (
            UPDATE billing.orders o
            SET status = 'void', payment_status = 'requires_payment_method', voided_at = now(), updated_at = now()
            WHERE o.merchant_id = NEW.merchant_id AND o.subscription_id = NEW.id AND o.origin = 'subscription' AND o.collection_owner = 'engine' AND o.status = 'open'
            RETURNING o.id, o.customer_id, o.total, o.currency)
        INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, amount, currency, occurred_at, data, dedupe_key)
        SELECT NEW.merchant_id, 'order.voided', 'order', v.id, v.total, v.currency, now(),
               jsonb_build_object('customer_id', v.customer_id, 'status', 'void', 'payment_status', 'requires_payment_method'),
               'order.voided:' || v.id
        FROM voided v
        ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER subscriptions_close_landed_park AFTER INSERT ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.close_landed_park();
CREATE TRIGGER subscriptions_collection_policy_immutable BEFORE UPDATE OF collection_policy ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.preserve_subscription_collection_policy();
CREATE TRIGGER subscriptions_facts_authority_one_way BEFORE UPDATE OF facts_authority ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_facts_authority_one_way();
CREATE TRIGGER subscriptions_lifecycle_single_writer BEFORE UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_lifecycle_single_writer();
CREATE TRIGGER subscriptions_mark_stored_card_check AFTER INSERT OR UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscription_marks_stored_card_check();
CREATE TRIGGER subscriptions_release_ownership_claims AFTER UPDATE OF status, deleted_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.release_ended_ownership_claims();
CREATE TRIGGER subscriptions_row_version BEFORE UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_row_version();
CREATE TRIGGER subscriptions_void_renewal_orders AFTER UPDATE OF status, deleted_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.void_ended_renewal_orders();
CREATE TRIGGER trg_subscriptions_billing_changes AFTER UPDATE OF payment_method_id, next_retry_at, last_retry_at, retry_attempts, transient_retries, grace_ends_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_record_billing_change();
CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id, tier_group, status, deleted_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_set_tier_group();
CREATE TRIGGER trg_subscriptions_status_transition AFTER INSERT OR UPDATE OF status, current_period_ends_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_record_status_transition();
CREATE CONSTRAINT TRIGGER trg_subscriptions_track_unverified AFTER INSERT OR UPDATE OF status ON billing.subscriptions DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_track_unverified();

CREATE TABLE billing.subscription_status_transitions (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    subscription_id uuid NOT NULL,
    from_status text,
    to_status text NOT NULL,
    cancel_type text,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    decision text,
    from_current_period_ends_at timestamptz,
    to_current_period_ends_at timestamptz,
    origin text,
    actor text,
    executor text,
    operation_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT subscription_status_transitions_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE RESTRICT,
    CHECK (from_status IN ('pending', 'active', 'past_due', 'awaiting_method', 'canceled', 'unverified')),
    CONSTRAINT subscription_status_transitions_real_transition_check CHECK (
        from_status IS DISTINCT FROM to_status
        OR from_current_period_ends_at IS DISTINCT FROM to_current_period_ends_at
    ),
    CHECK (to_status IN ('pending', 'active', 'past_due', 'awaiting_method', 'canceled', 'unverified')),
    CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade'))
);
COMMENT ON TABLE billing.subscription_status_transitions IS 'Append-only subscription status audit, written by trg_subscriptions_status_transition in the SAME tx as the status change. from_status NULL = row creation. Retention: rows are deleted 25 months (761 days) after occurred_at, by the cleanup job only.';
COMMENT ON COLUMN billing.subscription_status_transitions.cancel_type IS 'The subscription''s cancel_type at transition time (meaningful for to_status=canceled).';

CREATE INDEX subscription_status_transitions_occurred_at_idx ON billing.subscription_status_transitions (merchant_id, occurred_at);
CREATE INDEX subscription_status_transitions_subscription_id_occurred_at_idx ON billing.subscription_status_transitions (merchant_id, subscription_id, occurred_at);

CREATE TRIGGER immutable_subscription_status_transitions BEFORE UPDATE ON billing.subscription_status_transitions
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER retained_subscription_status_transitions BEFORE DELETE ON billing.subscription_status_transitions
    FOR EACH ROW EXECUTE FUNCTION billing.guard_retention_delete('occurred_at', '761 days');

CREATE TABLE billing.subscription_verifications (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    unverified_at timestamptz NOT NULL,
    reads integer NOT NULL DEFAULT 0,
    last_read_at timestamptz,
    last_error text,
    PRIMARY KEY (merchant_id, subscription_id),
    CONSTRAINT subscription_verifications_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.subscription_verifications IS 'One row per unverified subscription, kept by trg_subscriptions_track_unverified at commit. unverified_at dates entry (the row''s updated_at); reads/last_read_at record provider reads. Feeds life.unverified.backlog and the unresolved escalation.';

CREATE INDEX subscription_verifications_unverified_at_idx ON billing.subscription_verifications (merchant_id, unverified_at);

CREATE TABLE billing.subscription_billing_changes (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    subscription_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    decision text,
    from_payment_method_id uuid,
    to_payment_method_id uuid,
    from_next_retry_at timestamptz,
    to_next_retry_at timestamptz,
    from_last_retry_at timestamptz,
    to_last_retry_at timestamptz,
    from_retry_attempts integer,
    to_retry_attempts integer,
    from_transient_retries integer,
    to_transient_retries integer,
    from_grace_ends_at timestamptz,
    to_grace_ends_at timestamptz,
    method_version_id uuid,
    origin text,
    actor text,
    executor text,
    operation_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT subscription_billing_changes_merchant_id_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE RESTRICT,
    CHECK (kind IN ('card_selection', 'default_card', 'card_version', 'retry_schedule'))
);
COMMENT ON TABLE billing.subscription_billing_changes IS 'Append-only observed subscription card and retry changes. References and before/after values are snapshots, not charging instructions. Retention: rows are deleted 25 months (761 days) after occurred_at, by the cleanup job only.';

CREATE INDEX subscription_billing_changes_history_idx ON billing.subscription_billing_changes (merchant_id, subscription_id, occurred_at DESC, id DESC);
CREATE INDEX subscription_billing_changes_retention_idx ON billing.subscription_billing_changes (merchant_id, occurred_at);

CREATE TRIGGER immutable_subscription_billing_changes BEFORE UPDATE ON billing.subscription_billing_changes
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER retained_subscription_billing_changes BEFORE DELETE ON billing.subscription_billing_changes
    FOR EACH ROW EXECUTE FUNCTION billing.guard_retention_delete('occurred_at', '761 days');

CREATE TABLE billing.scheduled_changes (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    subscription_id uuid NOT NULL,
    from_price_id uuid NOT NULL,
    price_id uuid NOT NULL,
    quantity integer CHECK (quantity IS NULL OR quantity >= 1),
    effective_at timestamptz NOT NULL,
    source text NOT NULL,
    price_migration_id uuid,
    status text NOT NULL,
    blocked_reason text,
    acknowledged_short_notice boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    applied_at timestamptz,
    canceled_at timestamptz,
    invoker text CHECK (invoker <> ''),
    reason text CHECK (reason <> ''),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT scheduled_changes_from_price_id_fkey FOREIGN KEY (merchant_id, from_price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT scheduled_changes_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT scheduled_changes_price_migration_id_fkey FOREIGN KEY (merchant_id, price_migration_id) REFERENCES billing.price_migrations (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT scheduled_changes_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT scheduled_changes_blocked_check CHECK (
        (status IN ('blocked', 'skipped')) = (blocked_reason IS NOT NULL)
        AND (status NOT IN ('blocked', 'skipped') OR source = 'migration')
        AND blocked_reason <> ''
    ),
    CONSTRAINT scheduled_changes_source_check CHECK (source IN ('change', 'migration') AND (source = 'migration') = (price_migration_id IS NOT NULL)),
    CONSTRAINT scheduled_changes_staff_check CHECK ((invoker IS NULL) = (reason IS NULL) AND (invoker IS NULL OR source = 'change')),
    CONSTRAINT scheduled_changes_applied_check CHECK ((status = 'applied') = (applied_at IS NOT NULL)),
    CONSTRAINT scheduled_changes_canceled_check CHECK ((status = 'canceled') = (canceled_at IS NOT NULL)),
    CHECK (status IN ('scheduled', 'applied', 'canceled', 'blocked', 'skipped'))
);
COMMENT ON TABLE billing.scheduled_changes IS 'A subscription''s change waiting for its renewal: the price (price_id) and, for a per-seat price, the seats (quantity; NULL keeps the subscription''s) it bills from its first renewal on or after effective_at. At most one is scheduled per subscription. source is change (a tier or seat change) or migration (price_migration_id). A migration''s move its provider cannot take is blocked, one that no longer applies is skipped; both are terminal and blocked_reason says why. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.scheduled_changes.from_price_id IS 'The price the subscription billed when the change was scheduled; the change applies only while it still does.';
COMMENT ON COLUMN billing.scheduled_changes.blocked_reason IS 'Why a blocked or skipped move did not happen; NULL otherwise.';
COMMENT ON COLUMN billing.scheduled_changes.acknowledged_short_notice IS 'A price increase scheduled inside the merchant''s notice window under the request''s acknowledge_short_notice.';
COMMENT ON COLUMN billing.scheduled_changes.invoker IS 'The staff member who scheduled the change at the customer''s request, with reason; NULL for the customer''s own change and a migration.';

CREATE INDEX scheduled_changes_from_price_id_idx ON billing.scheduled_changes (merchant_id, from_price_id);
CREATE INDEX scheduled_changes_price_id_idx ON billing.scheduled_changes (merchant_id, price_id);
CREATE INDEX scheduled_changes_price_migration_id_idx ON billing.scheduled_changes (merchant_id, price_migration_id)
    WHERE price_migration_id IS NOT NULL;
CREATE INDEX scheduled_changes_subscription_id_idx ON billing.scheduled_changes (merchant_id, subscription_id);
CREATE UNIQUE INDEX scheduled_changes_subscription_id_key ON billing.scheduled_changes (merchant_id, subscription_id)
    WHERE status = 'scheduled';

CREATE TABLE billing.solana_subscriptions (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    subscription_id uuid NOT NULL,
    subscriber_wallet text NOT NULL,
    authority_pda text NOT NULL,
    subscription_pda text NOT NULL,
    plan_pda text NOT NULL,
    merchant_address text NOT NULL,
    mint text NOT NULL,
    plan_created_at_fingerprint bigint NOT NULL,
    last_pulled_period_starts_at timestamptz,
    last_signature text,
    next_pull_at timestamptz NOT NULL,
    status text NOT NULL CHECK (status IN ('active', 'canceled', 'expired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    treasury text NOT NULL,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda),
    CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.solana_subscriptions IS 'On-chain mirror of one Solana subscription: its program accounts, mint and next pull.';
COMMENT ON COLUMN billing.solana_subscriptions.merchant_address IS 'The plan''s owner: the deployment cranker that publishes it and pulls each renewal.';
COMMENT ON COLUMN billing.solana_subscriptions.treasury IS 'The merchant''s treasury: the plan''s destination, where every pull lands.';

CREATE INDEX solana_subscriptions_next_pull_at_idx ON billing.solana_subscriptions (merchant_id, next_pull_at)
    WHERE status = 'active';
CREATE INDEX solana_subscriptions_subscription_id_idx ON billing.solana_subscriptions (merchant_id, subscription_id);

CREATE TABLE billing.rebill_cycles (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    owner text NOT NULL CHECK (owner IN ('engine', 'nmi_schedule', 'provider')),
    due_at timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    missed_at timestamptz,
    miss_reason text,
    quantity integer CHECK (quantity IS NULL OR quantity >= 1),
    probe_failed_at timestamptz,
    next_probe_at timestamptz,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT rebill_cycles_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT rebill_cycles_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT rebill_cycles_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT rebill_cycles_missed_check CHECK ((missed_at IS NULL) = (miss_reason IS NULL)),
    CHECK (miss_reason IN ('held', 'refused', 'method_unusable', 'not_attempted', 'provider_skipped', 'provider_stalled', 'provider_reversed', 'provider_unrecorded', 'schedule_gone')),
    CONSTRAINT rebill_cycles_probe_check CHECK ((probe_failed_at IS NULL) = (next_probe_at IS NULL))
);
COMMENT ON TABLE billing.rebill_cycles IS 'One expected rebill per (subscription, due_at): the moment its paid period came due. Its attempts are payment_attempts.cycle_id. Retention: rows are deleted 25 months (761 days) after due_at, once their attempts are gone.';
COMMENT ON COLUMN billing.rebill_cycles.missed_at IS 'When the cycle passed its owner''s deadline with no attempt; a later attempt still attaches to the cycle.';
COMMENT ON COLUMN billing.rebill_cycles.quantity IS 'Seats the cycle bills; NULL unless the subscription is per seat.';
COMMENT ON COLUMN billing.rebill_cycles.probe_failed_at IS 'First failed look at this overdue cycle since the last success; NULL when none failed.';
COMMENT ON COLUMN billing.rebill_cycles.next_probe_at IS 'When the rebill watch looks at this overdue cycle again after a failed look.';

CREATE INDEX rebill_cycles_customer_id_idx ON billing.rebill_cycles (merchant_id, customer_id);
CREATE INDEX rebill_cycles_due_at_idx ON billing.rebill_cycles (merchant_id, due_at);
CREATE INDEX rebill_cycles_psp_id_due_at_idx ON billing.rebill_cycles (merchant_id, psp_id, due_at);
CREATE UNIQUE INDEX rebill_cycles_subscription_id_due_at_key ON billing.rebill_cycles (merchant_id, subscription_id, due_at);

-- ---------------------------------------------------------------------------
-- Ledger
-- ---------------------------------------------------------------------------

CREATE TABLE billing.ledger_accounts (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid,
    account_type text NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    debits_must_not_exceed_credits boolean NOT NULL,
    credits_must_not_exceed_debits boolean NOT NULL,
    credits_posted bigint NOT NULL DEFAULT 0,
    debits_posted bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT ledger_accounts_customer_id_currency_id_key UNIQUE (merchant_id, customer_id, currency, id),
    CONSTRAINT ledger_accounts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT ledger_accounts_type_check CHECK (account_type IN ('customer_balance', 'platform_revenue', 'processor_clearing', 'arrears_liability', 'expired_credits', 'revoked_credits', 'promotional_funding', 'credit_refund_clearing', 'credit_refund_loss'))
);
COMMENT ON TABLE billing.ledger_accounts IS 'Double-entry ledger accounts. One account belongs to exactly one (merchant, currency) ledger; TB-style posted/pending counters are maintained from immutable ledger_transfers and verified by reconciliation. account_type identifies its role (customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, revoked_credits). Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.ledger_accounts.customer_id IS 'NULL for system accounts (one per merchant+currency); set for per-customer balance accounts.';
COMMENT ON COLUMN billing.ledger_accounts.account_type IS 'Account role within a (merchant, currency) ledger. arrears_liability is PER-CUSTOMER: its negated balance is that payer''s outstanding owed, read O(1) on the admission path. customer_balance is per-customer; processor_clearing / platform_revenue / expired_credits / revoked_credits are merchant-wide system accounts.';
COMMENT ON COLUMN billing.ledger_accounts.debits_must_not_exceed_credits IS 'TB sign flag: balance (credits-debits) may not go below zero (minus an applier-supplied arrears floor). Set on customer_balance.';
COMMENT ON COLUMN billing.ledger_accounts.credits_posted IS 'Maintained counter: posted credits, for O(1) balance reads.';
COMMENT ON COLUMN billing.ledger_accounts.debits_posted IS 'Maintained counter: posted debits, for O(1) balance reads.';

CREATE UNIQUE INDEX ledger_accounts_account_type_currency_key ON billing.ledger_accounts (merchant_id, account_type, currency)
    WHERE customer_id IS NULL;
CREATE UNIQUE INDEX ledger_accounts_customer_id_account_type_currency_key ON billing.ledger_accounts (merchant_id, customer_id, account_type, currency)
    WHERE customer_id IS NOT NULL;

CREATE FUNCTION billing.guard_ledger_account_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    -- Only a nested trigger may maintain counters. The existing transfer
    -- AFTER INSERT trigger is their sole writer; ordinary UPDATE cannot forge
    -- trigger nesting, and no caller-set session setting grants this permission.
    -- Keep the existing row locks/arithmetic: rescanning transfers here would
    -- change multi-row insertion and concurrent transfer snapshot semantics.
    IF TG_OP='DELETE' OR pg_trigger_depth()<2
       OR (to_jsonb(NEW)-ARRAY['debits_posted','credits_posted']) IS DISTINCT FROM
          (to_jsonb(OLD)-ARRAY['debits_posted','credits_posted']) THEN
        RAISE EXCEPTION 'ledger account facts are immutable; counters are maintained by transfer insertion' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER guard_ledger_account_facts BEFORE DELETE OR UPDATE ON billing.ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION billing.guard_ledger_account_facts();
CREATE TRIGGER immutable_ledger_accounts_truncate BEFORE TRUNCATE ON billing.ledger_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.ledger_transfers (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    debit_account_id uuid NOT NULL,
    credit_account_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    transfer_type text NOT NULL,
    allow_debit_negative_up_to bigint NOT NULL,
    source text NOT NULL CHECK (octet_length(source) BETWEEN 1 AND 512),
    source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 512),
    grant_id uuid,
    customer_id uuid,
    invoker_id text,
    resource text,
    order_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    operation text NOT NULL CHECK (octet_length(operation) BETWEEN 1 AND 512),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT ledger_transfers_customer_id_currency_id_key UNIQUE (merchant_id, customer_id, currency, id),
    CONSTRAINT ledger_transfers_credit_account_id_fkey FOREIGN KEY (merchant_id, credit_account_id) REFERENCES billing.ledger_accounts (merchant_id, id),
    CONSTRAINT ledger_transfers_debit_account_id_fkey FOREIGN KEY (merchant_id, debit_account_id) REFERENCES billing.ledger_accounts (merchant_id, id),
    CONSTRAINT ledger_transfers_distinct_accounts_check CHECK (debit_account_id <> credit_account_id),
    CONSTRAINT ledger_transfers_amount_positive_check CHECK (amount > 0),
    CONSTRAINT ledger_transfers_type_check CHECK (transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'credit_refund', 'credit_refund_restore', 'credit_refund_cash', 'credit_refund_cash_restore', 'credit_refund_funding')),
    CONSTRAINT ledger_transfers_debit_floor_nonnegative_check CHECK (allow_debit_negative_up_to >= 0),
    CONSTRAINT ledger_transfers_coordinate_not_blank_check CHECK (operation <> '' AND source <> '' AND source_id <> '')
);
COMMENT ON TABLE billing.ledger_transfers IS 'Immutable double-entry transfers. Append-only. A transfer moves amount debit->credit within ONE (merchant, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. ledger_accounts counters are a maintained projection of this table. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.ledger_transfers.allow_debit_negative_up_to IS 'Debit-account floor used by the counter trigger for debits_must_not_exceed_credits accounts. Usually 0; arrears paths pass the current credit-line allowance.';
COMMENT ON COLUMN billing.ledger_transfers.source IS 'Opaque origin key (e.g. ''grant''/grant_id, ''payment''/transaction_id). Ledger purity: business joins live in control-plane tables.';
COMMENT ON COLUMN billing.ledger_transfers.grant_id IS 'Credit-lot attribution. grant_id/order_id/customer_id deliberately carry no FKs (ledger purity): the append-only ledger never blocks or cascades on business rows.';
COMMENT ON COLUMN billing.ledger_transfers.operation IS 'Engine-composed money-operation kind (capture / spend / withdraw / usage:<event_type> / deposit / ...). Part of the idempotency coordinate together with (source, source_id): two different operations sharing a caller key must not alias.';

CREATE INDEX ledger_transfers_created_at_idx ON billing.ledger_transfers (merchant_id, created_at);
CREATE INDEX ledger_transfers_credit_account_id_idx ON billing.ledger_transfers (merchant_id, credit_account_id);
CREATE INDEX ledger_transfers_customer_id_currency_created_at_idx ON billing.ledger_transfers (merchant_id, customer_id, currency, created_at DESC)
    WHERE customer_id IS NOT NULL;
CREATE INDEX ledger_transfers_debit_account_id_idx ON billing.ledger_transfers (merchant_id, debit_account_id);
CREATE INDEX ledger_transfers_grant_id_idx ON billing.ledger_transfers (merchant_id, grant_id)
    WHERE grant_id IS NOT NULL;
CREATE UNIQUE INDEX ledger_transfers_grant_id_transfer_type_key ON billing.ledger_transfers (merchant_id, grant_id, transfer_type)
    WHERE grant_id IS NOT NULL AND transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_expire', 'credit_revoke');
CREATE UNIQUE INDEX ledger_transfers_operation_once_key ON billing.ledger_transfers (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, grant_id) NULLS NOT DISTINCT;
COMMENT ON INDEX billing.ledger_transfers_operation_once_key IS 'The structural once-only key for EVERY transfer type. ledger.ApplyIdempotent inserts ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. grant_id is part of the identity (one debit per operation per FIFO lot); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';
CREATE INDEX ledger_transfers_source_source_id_idx ON billing.ledger_transfers (merchant_id, source, source_id);

CREATE FUNCTION billing.ledger_transfers_apply_counters() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'billing', 'pg_catalog' AS $$
DECLARE
    acc billing.ledger_accounts%ROWTYPE;
    debit billing.ledger_accounts%ROWTYPE;
    credit billing.ledger_accounts%ROWTYPE;
    debit_balance bigint;
    credit_balance bigint;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    FOR acc IN
        SELECT *
        FROM billing.ledger_accounts
        WHERE merchant_id = NEW.merchant_id
          AND id IN (NEW.debit_account_id, NEW.credit_account_id)
        ORDER BY id
        -- Counters do not change account keys. FK checks may already hold
        -- KEY SHARE locks; upgrading those to FOR UPDATE can deadlock peers.
        FOR NO KEY UPDATE
    LOOP
        IF acc.id = NEW.debit_account_id THEN
            debit := acc;
        ELSIF acc.id = NEW.credit_account_id THEN
            credit := acc;
        END IF;
    END LOOP;

    IF debit.id IS NULL OR credit.id IS NULL THEN
        RAISE EXCEPTION 'ledger_transfers: debit/credit account not found';
    END IF;

    IF debit.currency <> NEW.currency OR credit.currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger_transfers: cross-currency transfer (debit=%, credit=%, transfer=%) - a transfer never crosses ledgers', debit.currency, credit.currency, NEW.currency;
    END IF;

    debit_balance := debit.credits_posted - debit.debits_posted - NEW.amount;
    credit_balance := credit.debits_posted - credit.credits_posted - NEW.amount;
    IF debit.debits_must_not_exceed_credits AND debit_balance < -NEW.allow_debit_negative_up_to THEN
        RAISE EXCEPTION 'ledger_insufficient_funds: balance %, amount %, floor %', debit.credits_posted - debit.debits_posted, NEW.amount, NEW.allow_debit_negative_up_to;
    END IF;
    IF credit.credits_must_not_exceed_debits AND credit_balance < 0 THEN
        RAISE EXCEPTION 'ledger_credit_constraint: credit account % would exceed debits', NEW.credit_account_id;
    END IF;

    UPDATE billing.ledger_accounts
    SET debits_posted = debits_posted + NEW.amount
    WHERE merchant_id = NEW.merchant_id AND id = NEW.debit_account_id;
    UPDATE billing.ledger_accounts
    SET credits_posted = credits_posted + NEW.amount
    WHERE merchant_id = NEW.merchant_id AND id = NEW.credit_account_id;

    RETURN NEW;
END;
$$;

CREATE TRIGGER immutable_ledger_transfers BEFORE DELETE OR UPDATE ON billing.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_ledger_transfers_truncate BEFORE TRUNCATE ON billing.ledger_transfers
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER trg_ledger_transfers_apply_counters AFTER INSERT ON billing.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION billing.ledger_transfers_apply_counters();

CREATE FUNCTION billing.check_billing_restore_ledger(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.ledger_accounts a
        LEFT JOIN (SELECT credit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY credit_account_id) c ON c.id=a.id
        LEFT JOIN (SELECT debit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY debit_account_id) d ON d.id=a.id
        WHERE a.merchant_id=p_merchant AND (a.credits_posted<>coalesce(c.amount,0) OR a.debits_posted<>coalesce(d.amount,0)))
       OR EXISTS (SELECT currency FROM billing.ledger_accounts WHERE merchant_id=p_merchant GROUP BY currency HAVING sum(credits_posted::numeric-debits_posted::numeric)<>0)
       OR EXISTS (SELECT 1 FROM billing.ledger_transfers t
           JOIN billing.ledger_accounts d ON d.merchant_id=t.merchant_id AND d.id=t.debit_account_id
           JOIN billing.ledger_accounts c ON c.merchant_id=t.merchant_id AND c.id=t.credit_account_id WHERE t.merchant_id=p_merchant
           AND (d.currency<>t.currency OR c.currency<>t.currency
                OR (d.customer_id IS NOT NULL AND d.customer_id IS DISTINCT FROM t.customer_id)
                OR (c.customer_id IS NOT NULL AND c.customer_id IS DISTINCT FROM t.customer_id))) THEN
        RAISE EXCEPTION 'billing restore ledger integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_ledger(p_merchant uuid) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Orders, payments and access
-- ---------------------------------------------------------------------------

CREATE SEQUENCE billing.order_number_seq START WITH 1001 CACHE 64;
SELECT setval('billing.order_number_seq'::regclass, 1000);
COMMENT ON SEQUENCE billing.order_number_seq IS 'Source of orders.number, one per schema, starting at 1001. A number is taken when an order completes; nextval never blocks and is not rolled back, so numbers may skip or arrive out of order. Unique per merchant by orders_number_key. A restore raises it past the restored numbers.';

-- orders_attempt_id_fkey references order_attempts and is added after it.
-- orders_payment_id_fkey references payments and is added after it.
CREATE TABLE billing.orders (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    origin text NOT NULL CHECK (origin IN ('customer', 'merchant', 'subscription', 'usage')),
    status text NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    total bigint NOT NULL CHECK (total >= 0),
    number bigint CHECK (number >= 1),
    idempotency_key text CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    request_digest bytea,
    payment_method_id uuid,
    psp_id uuid,
    attempt_id uuid,
    payment_id uuid,
    last_payment_error jsonb,
    expires_at timestamptz,
    completed_at timestamptz,
    canceled_at timestamptz,
    expired_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    payment_status text NOT NULL,
    pay_url_secret_hash bytea,
    pay_url_expires_at timestamptz,
    success_url text CHECK (success_url <> ''),
    cancel_url text CHECK (cancel_url <> ''),
    saved_payment_methods boolean,
    subscription_id uuid,
    period_starts_at timestamptz,
    period_ends_at timestamptz,
    due_at timestamptz,
    voided_at timestamptz,
    amount_paid bigint,
    uncollectible_at timestamptz,
    collection_method text,
    po_number text CHECK (po_number <> ''),
    memo text CHECK (memo <> ''),
    tax jsonb CHECK (jsonb_typeof(tax) = 'object'),
    billing_contacts jsonb CHECK (jsonb_typeof(billing_contacts) = 'array'),
    collection_failure_count integer,
    collection_failed_at timestamptz,
    next_collection_attempt_at timestamptz,
    collection_owner text NOT NULL DEFAULT 'engine' CHECK (collection_owner IN ('engine', 'nmi_schedule', 'provider')),
    subscription_bill_kind text,
    lines_digest bytea,
    embedded boolean,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT orders_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT orders_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT orders_payment_method_id_fkey FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, id),
    CONSTRAINT orders_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id),
    CONSTRAINT orders_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id),
    CONSTRAINT orders_complete_payment_check CHECK (
        status <> 'complete'
        OR total = 0
        OR payment_id IS NOT NULL
        OR origin = 'usage'
        OR collection_owner <> 'engine'
    ),
    CONSTRAINT orders_lines_digest_check CHECK (lines_digest IS NULL OR (origin = 'customer' AND octet_length(lines_digest) = 32)),
    CONSTRAINT orders_pay_url_check CHECK (
        (pay_url_secret_hash IS NULL) = (pay_url_expires_at IS NULL)
        AND (pay_url_secret_hash IS NULL) = (saved_payment_methods IS NULL)
        AND (pay_url_secret_hash IS NULL) = (embedded IS NULL)
        AND (success_url IS NULL OR pay_url_secret_hash IS NOT NULL)
        AND (cancel_url IS NULL OR pay_url_secret_hash IS NOT NULL)
        AND (pay_url_secret_hash IS NULL OR (octet_length(pay_url_secret_hash) = 32 AND pay_url_expires_at <= expires_at AND ((origin = 'merchant' AND success_url IS NOT NULL) OR (origin = 'customer' AND success_url IS NULL AND cancel_url IS NULL AND embedded))))
    ),
    CONSTRAINT orders_period_check CHECK (
        (origin IN ('subscription', 'usage')) = (period_starts_at IS NOT NULL AND period_ends_at IS NOT NULL AND due_at IS NOT NULL)
        AND (origin = 'subscription') = (subscription_id IS NOT NULL)
        AND (collection_owner <> 'engine' OR (origin IN ('subscription', 'usage')) = (expires_at IS NULL))
        AND (period_ends_at IS NULL OR period_ends_at > period_starts_at)
    ),
    CONSTRAINT orders_subscription_bill_kind_check CHECK (
        subscription_bill_kind IN ('initial', 'renewal', 'adjustment')
        AND (subscription_bill_kind IS NULL OR origin = 'subscription')
    ),
    CONSTRAINT orders_uncollectible_check CHECK (
        (status = 'uncollectible') = (uncollectible_at IS NOT NULL)
        AND (status <> 'uncollectible' OR origin = 'usage')
    ),
    CONSTRAINT orders_usage_check CHECK (
        (origin = 'usage') = (amount_paid IS NOT NULL AND collection_method IS NOT NULL AND tax IS NOT NULL AND billing_contacts IS NOT NULL AND collection_failure_count IS NOT NULL)
        AND (amount_paid IS NULL OR amount_paid BETWEEN 0 AND total)
        AND (status <> 'complete' OR amount_paid IS NULL OR amount_paid = total)
        AND collection_failure_count >= 0
        AND (next_collection_attempt_at IS NULL OR origin = 'usage')
        AND (collection_failed_at IS NULL OR origin = 'usage')
    ),
    CONSTRAINT orders_canceled_check CHECK (status <> 'canceled' OR canceled_at IS NOT NULL),
    CONSTRAINT orders_complete_check CHECK ((status = 'complete') = (number IS NOT NULL AND completed_at IS NOT NULL)),
    CONSTRAINT orders_expired_check CHECK (status <> 'expired' OR expired_at IS NOT NULL),
    CONSTRAINT orders_payment_status_check CHECK (CASE status WHEN 'open' THEN (payment_status IN ('requires_payment_method', 'requires_action')) WHEN 'processing' THEN (payment_status = 'processing') WHEN 'complete' THEN (payment_status = 'succeeded') ELSE (payment_status IN ('requires_payment_method', 'succeeded')) END),
    CHECK (status IN ('open', 'processing', 'complete', 'canceled', 'expired', 'void', 'uncollectible')),
    CONSTRAINT orders_succeeded_check CHECK ((payment_status = 'succeeded') = (status = 'complete' OR payment_id IS NOT NULL)),
    CONSTRAINT orders_void_check CHECK ((status = 'void') = (voided_at IS NOT NULL)),
    CONSTRAINT orders_request_digest_check CHECK (
        (idempotency_key IS NULL) = (request_digest IS NULL)
        AND (request_digest IS NULL OR octet_length(request_digest) = 32)
    ),
    CHECK (collection_method IN ('charge_automatically', 'send_invoice'))
);
COMMENT ON TABLE billing.orders IS 'One payable (ord_ id): frozen lines and total in one currency, paid by at most one live order attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from order_number_seq when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 90 days after they closed; every other order is permanent.';
COMMENT ON COLUMN billing.orders.origin IS 'customer (the customer''s own purchase), merchant (an unpaid order the merchant created for them), subscription (a renewal of an engine-owned subscription) or usage (owed charges billed in arrears for a period).';
COMMENT ON COLUMN billing.orders.status IS 'open (takes payment, or awaits the customer''s action on it), processing (the provider has its payment), complete (paid and fulfilled), canceled or expired (a purchase closed unpaid), void (a renewal its subscription''s end left unowed, or a usage order the merchant voided), uncollectible (a usage order the merchant wrote off; collecting it reopens it).';
COMMENT ON COLUMN billing.orders.number IS 'The order number, taken from order_number_seq when the order completes; unique per merchant, not consecutive.';
COMMENT ON COLUMN billing.orders.payment_method_id IS 'The card the latest attempt charged; a card the attempt saved from the customer''s token stays as evidence after it is removed.';
COMMENT ON COLUMN billing.orders.attempt_id IS 'The order attempt last started for the order.';
COMMENT ON COLUMN billing.orders.expires_at IS 'When an unpaid purchase expires; a renewal has none.';
COMMENT ON COLUMN billing.orders.payment_status IS 'The payment''s own status: requires_payment_method (none yet, or the last declined: last_payment_error), requires_action, processing or succeeded. A closed order paid late is succeeded with its payment refunded.';
COMMENT ON COLUMN billing.orders.pay_url_secret_hash IS 'sha256 of the pay URL''s secret; the secret is never stored. Set once on a merchant order; on a customer''s own order, again each time the customer asks for its pay URL, which ends the last.';
COMMENT ON COLUMN billing.orders.pay_url_expires_at IS 'When the pay URL stops working; no later than the order''s own expiry.';
COMMENT ON COLUMN billing.orders.success_url IS 'Where a merchant order''s pay URL returns once paid. NULL on a customer''s own embedded pay URL, which returns nowhere: the frame tells its page.';
COMMENT ON COLUMN billing.orders.due_at IS 'When a renewal is owed: its period''s start. An open order past it is overdue.';
COMMENT ON COLUMN billing.orders.amount_paid IS 'A usage order''s paid part: its payments, remittances and the funded balance that repaid it. Complete once it is the total.';
COMMENT ON COLUMN billing.orders.collection_method IS 'A usage order''s collection: charge_automatically (charged to the currency''s collection card) or send_invoice (the customer remits).';
COMMENT ON COLUMN billing.orders.collection_failed_at IS 'A usage order''s first declined collection, which its retry schedule counts from.';
COMMENT ON COLUMN billing.orders.next_collection_attempt_at IS 'When a usage order''s next scheduled charge runs: its due date when it opens, then the retry schedule''s; NULL while none is scheduled (stopped until the customer designates a card).';
COMMENT ON COLUMN billing.orders.collection_owner IS 'Frozen collection authority: engine, NMI schedule with existing OpenRails dunning, or provider billing. A provider bill is never a generic local charge command.';
COMMENT ON COLUMN billing.orders.subscription_bill_kind IS 'Initial observed enrollment, renewal, or a separate adjustment bill. Provider bills use scoped provider identity rather than period timestamps as document identity.';
COMMENT ON COLUMN billing.orders.lines_digest IS 'sha256 of a customer order''s lines (prices, quantities, amounts), which POST /v1/me/orders finds its open order by. NULL on a cart''s order and every other origin.';
COMMENT ON COLUMN billing.orders.embedded IS 'The merchant''s page frames the pay URL, only on its registered origins (settings payment_page.embed_origins); otherwise the customer is sent to it.';

CREATE INDEX orders_closed_at_unpaid_idx ON billing.orders (merchant_id, COALESCE(canceled_at, expired_at))
    WHERE status IN ('canceled', 'expired') AND payment_id IS NULL AND attempt_id IS NULL;
CREATE INDEX orders_created_at_id_idx ON billing.orders (merchant_id, created_at DESC, id DESC);
CREATE INDEX orders_customer_id_created_at_id_idx ON billing.orders (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX orders_due_at_open_idx ON billing.orders (merchant_id, due_at)
    WHERE status = 'open' AND due_at IS NOT NULL;
CREATE INDEX orders_expires_at_live_idx ON billing.orders (merchant_id, expires_at) WHERE status = 'open';
CREATE INDEX orders_id_idx ON billing.orders (id);
CREATE UNIQUE INDEX orders_idempotency_key_key ON billing.orders (merchant_id, customer_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX orders_number_key ON billing.orders (merchant_id, number) WHERE number IS NOT NULL;
CREATE UNIQUE INDEX orders_open_lines_key ON billing.orders (merchant_id, customer_id, lines_digest)
    WHERE status = 'open' AND lines_digest IS NOT NULL;
CREATE UNIQUE INDEX orders_pay_url_secret_hash_key ON billing.orders (pay_url_secret_hash)
    WHERE pay_url_secret_hash IS NOT NULL;
CREATE INDEX orders_payment_method_id_idx ON billing.orders (merchant_id, payment_method_id)
    WHERE payment_method_id IS NOT NULL;
CREATE UNIQUE INDEX orders_renewal_key ON billing.orders (merchant_id, subscription_id, period_starts_at)
    WHERE origin = 'subscription' AND collection_owner = 'engine' AND subscription_bill_kind = 'renewal';
CREATE INDEX orders_subscription_period_idx ON billing.orders (merchant_id, subscription_id, period_starts_at)
    WHERE origin = 'subscription';
CREATE INDEX orders_usage_collection_due_idx ON billing.orders (next_collection_attempt_at, merchant_id, id)
    WHERE origin = 'usage' AND status = 'open' AND collection_method = 'charge_automatically' AND next_collection_attempt_at IS NOT NULL;
CREATE INDEX orders_usage_open_idx ON billing.orders (merchant_id, customer_id, currency)
    WHERE origin = 'usage' AND status IN ('open', 'processing', 'uncollectible');
CREATE UNIQUE INDEX orders_usage_period_key ON billing.orders (merchant_id, customer_id, currency, period_starts_at, period_ends_at)
    WHERE origin = 'usage';

CREATE TABLE billing.payments (
    id uuid NOT NULL DEFAULT uuidv7(),
    channel text NOT NULL CHECK (channel IN ('rail', 'manual')),
    rail text,
    transaction_id text NOT NULL,
    amount bigint NOT NULL,
    list_amount bigint NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    status text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'refunded')),
    subscription_id uuid,
    refunded_payment_id uuid,
    discount_code text,
    discount_reason text,
    discount_metadata jsonb,
    metadata jsonb,
    purchased_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    card_brand text,
    card_last4 text,
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    psp_id uuid,
    attempt_kind text,
    failure_code text,
    failure_reason text,
    reversal_kind text,
    token_type text,
    deleted_at timestamptz,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' ELSE NULL END) STORED,
    money_movement text NOT NULL DEFAULT 'none' CHECK (money_movement IN ('rail', 'none')),
    credit_grant_snapshot jsonb,
    order_id uuid,
    ledger_transfer_id uuid,
    quantity integer,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT payments_customer_id_currency_id_key UNIQUE (merchant_id, customer_id, currency, id),
    CONSTRAINT payments_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT payments_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT payments_customer_id_currency_refunded_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, currency, refunded_payment_id) REFERENCES billing.payments (merchant_id, customer_id, currency, id),
    CONSTRAINT payments_ledger_transfer_id_fkey FOREIGN KEY (merchant_id, customer_id, currency, ledger_transfer_id) REFERENCES billing.ledger_transfers (merchant_id, customer_id, currency, id) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT payments_customer_id_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id) ON DELETE SET NULL (subscription_id),
    CONSTRAINT payments_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT payments_destructive_run_id_destructive_run_class_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT payments_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id),
    CONSTRAINT payments_channel_psp_check CHECK (CASE WHEN (channel = 'rail') THEN (rail IS NOT NULL AND psp_id IS NOT NULL) ELSE (rail IS NULL AND psp_id IS NULL) END),
    CONSTRAINT payments_sign_check CHECK (CASE WHEN (reversal_kind IS NULL OR reversal_kind = 'dispute_reversal') THEN (amount >= 0) ELSE (amount <= 0) END),
    CONSTRAINT payments_quantity_check CHECK (quantity IS NULL OR (quantity >= 1 AND subscription_id IS NOT NULL)),
    CONSTRAINT payments_ledger_transfer_check CHECK (ledger_transfer_id IS NULL OR (order_id IS NOT NULL AND refunded_payment_id IS NULL)),
    CONSTRAINT payments_reversal_check CHECK ((reversal_kind IS NULL) = (refunded_payment_id IS NULL)),
    CONSTRAINT payments_not_future_check CHECK (purchased_at <= (now() + '00:05:00'::interval)),
    CHECK (attempt_kind IS NULL OR attempt_kind IN ('initial', 'renewal')),
    CHECK (reversal_kind IS NULL OR reversal_kind IN ('refund', 'chargeback', 'dispute_reversal')),
    CHECK (token_type IS NULL OR token_type IN ('network_token', 'pan_via_proxy', 'psp_token')),
    CONSTRAINT payments_order_required CHECK (order_id IS NOT NULL)
);
COMMENT ON TABLE billing.payments IS 'Records of all payment transactions. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.payments.channel IS 'How the money arrived: rail (through a PSP) or manual (recorded by the merchant). Manual rows have no rail and no PSP.';
COMMENT ON COLUMN billing.payments.status IS 'pending (a refund awaiting its PSP), succeeded, refunded (an NMI same-id refund marks its sale), or failed: a refund the PSP refused, or a decline recorded before declines were payment attempts. No writer records a failed charge.';
COMMENT ON COLUMN billing.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';
COMMENT ON COLUMN billing.payments.psp_id IS 'PSP that took this charge. Set exactly when channel = rail (payments_channel_psp_check).';
COMMENT ON COLUMN billing.payments.attempt_kind IS 'initial|renewal, stamped at write time by the purchase vs rebill paths; NULL = unknown (imported/pre-instrumentation rows).';
COMMENT ON COLUMN billing.payments.failure_code IS 'Raw rail decline code, recorded verbatim (no fabrication).';
COMMENT ON COLUMN billing.payments.failure_reason IS 'Normalized decline category, derived deterministically from failure_code per rail.';
COMMENT ON COLUMN billing.payments.reversal_kind IS 'Discriminates mirror rows: refund | chargeback | dispute_reversal (dispute won). NULL on sale rows.';
COMMENT ON COLUMN billing.payments.token_type IS 'Credential form presented to the network: network_token | pan_via_proxy | psp_token. NULL = unknown/legacy; excluded from token_type-dimensioned metrics.';
COMMENT ON COLUMN billing.payments.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.payments.money_movement IS 'rail|none — positive marker for real money movement at the payment rail. ''rail'' rows carry a rail-issued transaction_id and are the ONLY rows the host settlement feed publishes; ''none'' rows are bookkeeping (attempt anchors, declines, placeholders). Fail-closed default: undeclared = ''none''.';
COMMENT ON COLUMN billing.payments.credit_grant_snapshot IS 'Accepted credit promise and first successful fulfillment dates; independent of subsequent catalog edits.';
COMMENT ON COLUMN billing.payments.order_id IS 'Required immutable bill identity for every payment and reversal. Prices and products belong to the order lines.';
COMMENT ON COLUMN billing.payments.ledger_transfer_id IS 'The owed payment that settled a usage order''s charge or remittance. A balance repayment moves no money and is only its ledger transfer.';
COMMENT ON COLUMN billing.payments.quantity IS 'Seats a per-seat subscription payment billed: a period at that many seats, or the seats a mid-period increase added. NULL otherwise.';

CREATE UNIQUE INDEX payments_channel_transaction_id_key ON billing.payments (merchant_id, channel, transaction_id)
    WHERE channel <> 'rail' AND deleted_at IS NULL;
CREATE INDEX payments_created_at_id_idx ON billing.payments (merchant_id, created_at DESC, id DESC);
CREATE INDEX payments_customer_id_created_at_id_idx ON billing.payments (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX payments_destructive_run_id_idx ON billing.payments (merchant_id, destructive_run_id)
    WHERE destructive_run_id IS NOT NULL;
CREATE INDEX payments_destructive_scale_idx ON billing.payments (merchant_id, customer_id, purchased_at)
    WHERE status = 'succeeded' AND amount > 0 AND refunded_payment_id IS NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX payments_ledger_transfer_id_key ON billing.payments (merchant_id, ledger_transfer_id)
    WHERE ledger_transfer_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX payments_metadata_nmi_order_idx ON billing.payments (merchant_id, (metadata ->> 'nmi_subscription_order_id'::text))
    WHERE (metadata ->> 'nmi_subscription_order_id') IS NOT NULL;
CREATE INDEX payments_metadata_stripe_invoice_idx ON billing.payments (merchant_id, (metadata ->> 'stripe_invoice_id'::text))
    WHERE (metadata ->> 'stripe_invoice_id') IS NOT NULL;
CREATE INDEX payments_order_id_idx ON billing.payments (merchant_id, order_id) WHERE order_id IS NOT NULL;
CREATE INDEX payments_pending_credit_refunds_idx ON billing.payments (merchant_id, customer_id, currency, refunded_payment_id)
    WHERE status = 'pending' AND amount < 0 AND deleted_at IS NULL;
CREATE INDEX payments_psp_id_idx ON billing.payments (merchant_id, psp_id) WHERE psp_id IS NOT NULL;
CREATE UNIQUE INDEX payments_psp_id_transaction_id_key ON billing.payments (merchant_id, psp_id, transaction_id)
    WHERE psp_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX payments_purchased_at_merchant_purchased_idx ON billing.payments (merchant_id, purchased_at);
CREATE INDEX payments_purchased_at_purchased_at_idx ON billing.payments (purchased_at);
CREATE INDEX payments_rail_transaction_id_idx ON billing.payments (merchant_id, rail, transaction_id);
CREATE INDEX payments_refunded_payment_id_idx ON billing.payments (merchant_id, refunded_payment_id)
    WHERE refunded_payment_id IS NOT NULL;
CREATE INDEX payments_subscription_id_idx ON billing.payments (merchant_id, subscription_id)
    WHERE subscription_id IS NOT NULL;

CREATE FUNCTION billing.enqueue_payment_settlement_event() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF NEW.status = 'succeeded'
       AND NEW.amount > 0
       AND NEW.refunded_payment_id IS NULL
       AND NEW.money_movement = 'rail'
       AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status)
    THEN
        INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, payment_id, amount, currency, occurred_at, dedupe_key)
        VALUES (NEW.merchant_id, 'payment.settled', 'payment', NEW.id, NEW.id, NEW.amount, NEW.currency,
                COALESCE(NEW.purchased_at, NEW.created_at, now()), 'payment:' || NEW.id::text)
        ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_enqueue_settlement_event AFTER INSERT OR UPDATE OF status ON billing.payments
    FOR EACH ROW EXECUTE FUNCTION billing.enqueue_payment_settlement_event();

ALTER TABLE billing.orders ADD CONSTRAINT orders_payment_id_fkey
    FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE billing.order_attempts (
    id uuid NOT NULL DEFAULT uuidv7(),
    rail text NOT NULL,
    status text NOT NULL,
    amount bigint,
    currency text CHECK (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'),
    expires_at timestamptz,
    reference text,
    transaction_id text,
    payment_id uuid,
    subscription_id uuid,
    rail_fields jsonb,
    rail_state jsonb,
    metadata jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    deleted_at timestamptz,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' ELSE NULL END) STORED,
    order_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT order_attempts_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT order_attempts_customer_id_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT order_attempts_customer_id_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id),
    CONSTRAINT order_attempts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT order_attempts_destructive_run_id_destructive_run_class_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT order_attempts_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id),
    CHECK (status IN ('created', 'requires_action', 'processing', 'succeeded', 'failed', 'expired', 'canceled'))
);
COMMENT ON TABLE billing.order_attempts IS 'An order payment attempt: provider execution and any customer action are tied to the order. Retention: attempts that expired without reaching a provider are deleted 90 days after expires_at; every other attempt is permanent.';
COMMENT ON COLUMN billing.order_attempts.psp_id IS 'PSP selected for this attempt. Required.';
COMMENT ON COLUMN billing.order_attempts.deleted_at IS 'Soft delete: set, the row is invisible to every live read. Only `openrails prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.order_attempts.order_id IS 'The order this payment attempt pays.';

CREATE INDEX order_attempts_customer_id_idx ON billing.order_attempts (merchant_id, customer_id);
CREATE INDEX order_attempts_destructive_run_id_idx ON billing.order_attempts (merchant_id, destructive_run_id)
    WHERE destructive_run_id IS NOT NULL;
CREATE INDEX order_attempts_expires_at_abandoned_idx ON billing.order_attempts (merchant_id, expires_at)
    WHERE status = 'expired' AND deleted_at IS NULL AND payment_id IS NULL AND subscription_id IS NULL AND transaction_id IS NULL;
CREATE INDEX order_attempts_expires_at_expirable_idx ON billing.order_attempts (merchant_id, expires_at)
    WHERE expires_at IS NOT NULL AND deleted_at IS NULL AND status IN ('created', 'requires_action');
CREATE INDEX order_attempts_order_id_idx ON billing.order_attempts (merchant_id, order_id)
    WHERE order_id IS NOT NULL;
CREATE UNIQUE INDEX order_attempts_order_id_live_key ON billing.order_attempts (merchant_id, order_id)
    WHERE order_id IS NOT NULL AND status IN ('created', 'requires_action', 'processing') AND deleted_at IS NULL;
CREATE INDEX order_attempts_payment_id_idx ON billing.order_attempts (merchant_id, payment_id)
    WHERE payment_id IS NOT NULL;
CREATE INDEX order_attempts_psp_id_idx ON billing.order_attempts (merchant_id, psp_id);
CREATE UNIQUE INDEX order_attempts_psp_id_reference_key ON billing.order_attempts (merchant_id, psp_id, reference)
    WHERE reference IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX order_attempts_psp_id_transaction_id_key ON billing.order_attempts (merchant_id, psp_id, transaction_id)
    WHERE transaction_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX order_attempts_subscription_id_idx ON billing.order_attempts (merchant_id, subscription_id)
    WHERE subscription_id IS NOT NULL;
CREATE UNIQUE INDEX order_attempts_transaction_id_key ON billing.order_attempts (transaction_id)
    WHERE rail = 'solana' AND transaction_id IS NOT NULL AND deleted_at IS NULL;

ALTER TABLE billing.orders ADD CONSTRAINT orders_attempt_id_fkey
    FOREIGN KEY (merchant_id, attempt_id) REFERENCES billing.order_attempts (merchant_id, id) ON DELETE SET NULL (attempt_id) DEFERRABLE INITIALLY DEFERRED;

-- Holds for every new row; rows that predate it are not checked.
ALTER TABLE billing.order_attempts ADD CONSTRAINT order_attempts_order_terms_check
    CHECK (order_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL) NOT VALID;

CREATE TABLE billing.grants (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    product_id uuid,
    kind text NOT NULL CHECK (kind IN ('entitlement', 'ownership', 'credit', 'access')),
    source_type text NOT NULL,
    source_id text CHECK (octet_length(source_id) BETWEEN 1 AND 255),
    payment_id uuid,
    event text NOT NULL CHECK (event IN ('grant', 'revoke', 'expire', 'supersede')),
    supersedes_id uuid,
    spec_snapshot jsonb,
    starts_at timestamptz NOT NULL,
    ends_at timestamptz,
    amount bigint,
    currency text CHECK (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'),
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    actor text CHECK (actor IS NULL OR octet_length(actor) BETWEEN 1 AND 255),
    grant_reason text,
    quantity integer CHECK (quantity IS NULL OR quantity >= 1),
    import_as_of timestamptz,
    import_root_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT grants_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT grants_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT grants_customer_id_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT grants_customer_id_supersedes_id_fkey FOREIGN KEY (merchant_id, customer_id, supersedes_id) REFERENCES billing.grants (merchant_id, customer_id, id),
    CONSTRAINT grants_import_root_id_fkey FOREIGN KEY (merchant_id, customer_id, import_root_id) REFERENCES billing.grants (merchant_id, customer_id, id),
    CONSTRAINT grants_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CONSTRAINT grants_access_product_check CHECK (kind <> 'access' OR (product_id IS NOT NULL AND spec_snapshot IS NULL AND source_id IS NOT NULL)),
    CONSTRAINT grants_access_grant_reason_check CHECK (
        NOT (kind = 'access' AND source_type = 'grant' AND event = 'grant')
        OR (grant_reason IS NOT NULL AND actor IS NOT NULL)
    ),
    CONSTRAINT grants_credit_amount_check CHECK (kind <> 'credit' OR (amount IS NOT NULL AND currency IS NOT NULL)),
    CONSTRAINT grants_grant_reason_check CHECK (
        grant_reason IS NULL
        OR (grant_reason IN ('comp', 'staff', 'import', 'migration') AND kind = 'access' AND source_type = 'grant')
    ),
    CONSTRAINT grants_import_metadata_check CHECK (
        (import_as_of IS NULL AND import_root_id IS NULL)
        OR (import_as_of IS NOT NULL AND kind = 'access' AND event = 'grant' AND source_type = 'grant' AND grant_reason = 'import' AND actor = 'import')
    ),
    CHECK (source_type IN ('purchase', 'subscription', 'admin', 'grace', 'grant')),
    CONSTRAINT grants_event_supersedes_check CHECK ((event = 'grant') = (supersedes_id IS NULL)),
    CONSTRAINT grants_termination_no_window_check CHECK (event = 'grant' OR ends_at IS NULL),
    CONSTRAINT grants_valid_window_check CHECK (ends_at IS NULL OR starts_at < ends_at),
    CONSTRAINT grants_amount_positive_check CHECK (amount IS NULL OR amount > 0)
);
COMMENT ON TABLE billing.grants IS 'Append-only grant ledger: the access-domain sibling of the money ledger. Immutable events (grant/revoke/expire/supersede); the live entitlement windows, product ownership, and credit lots are DERIVED projections folded from this log. A credit grant carries the lot amount and currency and is the FIFO credit lot; its deposit transfer is tagged source=grant. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.grants.kind IS 'access: a product for a window, whose keys are read from the product at check time; credit: a credit lot. entitlement and ownership are history from before product access.';
COMMENT ON COLUMN billing.grants.event IS 'Grant roots a grant; revoke/expire/supersede are new rows referencing it via supersedes_id. The grant row is never updated.';
COMMENT ON COLUMN billing.grants.spec_snapshot IS 'Product entitlements/credits spec captured at issuance so derive-2 (grant->projection) is a pure function and replay is exact + historical.';
COMMENT ON COLUMN billing.grants.actor IS 'Who granted a free product: the operator or credential. Required on grant-sourced access grants.';
COMMENT ON COLUMN billing.grants.grant_reason IS 'Why a free product was granted: comp, staff, import or migration. Required on grant-sourced access grants; reason stays a free-text note.';
COMMENT ON COLUMN billing.grants.quantity IS 'Seats an access grant gives: its per-seat subscription''s quantity; NULL otherwise.';
COMMENT ON COLUMN billing.grants.import_as_of IS 'Evidence horizon of an imported admin grant declaration. NULL on grants recorded before import revisions were supported.';
COMMENT ON COLUMN billing.grants.import_root_id IS 'Original imported admin grant for a replacement window. Replacements take effect no earlier than import_as_of; the root retains the permanent source key.';
COMMENT ON CONSTRAINT grants_termination_no_window_check ON billing.grants IS 'Only grant events carry an access window; revoke/expire/supersede are window-less point events (valid-time instant on starts_at, transaction time on created_at).';

CREATE UNIQUE INDEX grants_access_idempotency_key ON billing.grants (merchant_id, source_id)
    WHERE kind = 'access' AND event = 'grant' AND source_type = 'grant' AND grant_reason <> 'migration' AND import_root_id IS NULL;
COMMENT ON INDEX billing.grants_access_idempotency_key IS 'A free product grant root permanently owns its source id. Imported replacements reference that root.';
CREATE UNIQUE INDEX grants_access_once_key ON billing.grants (merchant_id, customer_id, product_id, source_type, source_id, starts_at)
    WHERE kind = 'access' AND event = 'grant' AND source_type <> 'grace' AND import_root_id IS NULL;
CREATE UNIQUE INDEX grants_access_purchase_key ON billing.grants (merchant_id, payment_id, product_id)
    WHERE kind = 'access' AND event = 'grant' AND source_type = 'purchase';
CREATE INDEX grants_created_at_idx ON billing.grants (merchant_id, created_at) WHERE kind = 'credit';
CREATE INDEX grants_customer_id_currency_starts_at_ends_at_idx ON billing.grants (merchant_id, customer_id, currency, starts_at, ends_at)
    WHERE kind = 'credit' AND event = 'grant';
CREATE INDEX grants_customer_id_kind_idx ON billing.grants (merchant_id, customer_id, kind)
    WHERE event = 'grant';
CREATE UNIQUE INDEX grants_customer_id_source_id_key ON billing.grants (merchant_id, customer_id, source_id)
    WHERE kind = 'credit' AND event = 'grant' AND source_id IS NOT NULL;
COMMENT ON INDEX billing.grants_customer_id_source_id_key IS 'A deposit happens at most once at the caller''s key (merchant, customer, source_id). Merchant-led. source_type is NOT part of the key — the same source_id under a different source label is the same deposit. Once-only is a database fact, not a consequence of depositTx''s lockBalance serialization.';
CREATE INDEX grants_ends_at_idx ON billing.grants (merchant_id, ends_at)
    WHERE kind = 'credit' AND event = 'grant' AND ends_at IS NOT NULL;
CREATE UNIQUE INDEX grants_import_revision_key ON billing.grants (merchant_id, source_id, import_as_of)
    WHERE import_as_of IS NOT NULL;
CREATE INDEX grants_payment_id_idx ON billing.grants (merchant_id, payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX grants_product_id_idx ON billing.grants (merchant_id, product_id) WHERE product_id IS NOT NULL;
CREATE UNIQUE INDEX grants_purchased_credit_payment_key ON billing.grants (merchant_id, payment_id)
    WHERE kind = 'credit' AND event = 'grant' AND payment_id IS NOT NULL AND ((spec_snapshot -> 'deposit') -> 'paid_amount') IS NOT NULL;
CREATE INDEX grants_source_type_source_id_idx ON billing.grants (merchant_id, source_type, source_id)
    WHERE source_id IS NOT NULL;
CREATE UNIQUE INDEX grants_supersedes_id_key ON billing.grants (merchant_id, supersedes_id)
    WHERE supersedes_id IS NOT NULL;

CREATE TRIGGER immutable_grants BEFORE DELETE OR UPDATE ON billing.grants
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_grants_truncate BEFORE TRUNCATE ON billing.grants
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.product_access (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    product_id uuid NOT NULL,
    grant_id uuid NOT NULL,
    source_type text NOT NULL,
    source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 255),
    payment_id uuid,
    starts_at timestamptz NOT NULL,
    ends_at timestamptz,
    revoked_at timestamptz,
    revoke_reason text,
    deleted_at timestamptz,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' ELSE NULL END) STORED,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    quantity integer CHECK (quantity IS NULL OR quantity >= 1),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT product_access_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT product_access_destructive_run_id_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT product_access_grant_id_fkey FOREIGN KEY (merchant_id, customer_id, grant_id) REFERENCES billing.grants (merchant_id, customer_id, id),
    CONSTRAINT product_access_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT product_access_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CHECK (source_type IN ('purchase', 'subscription', 'grace', 'grant')),
    CONSTRAINT product_access_valid_time_window_check CHECK (ends_at IS NULL OR starts_at < ends_at),
    CONSTRAINT product_access_revoke_fields_together_check CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
);
COMMENT ON TABLE billing.product_access IS 'Product access windows projected from access grants: a purchase, a subscription period, a grace allowance or a free grant of one product for [starts_at, ends_at). A customer holds the keys the product grants while a window is live; windows may overlap and reads take their union. Rebuildable from the grant ledger.';
COMMENT ON COLUMN billing.product_access.source_id IS 'The source''s own id: the payment of a purchase, the subscription of a period or grace allowance, the idempotency key of a free grant.';
COMMENT ON COLUMN billing.product_access.deleted_at IS 'A window removed before it started (a canceled future period or a refunded future rental): it never granted access.';
COMMENT ON COLUMN billing.product_access.quantity IS 'Seats the window gives, from its grant; NULL unless per seat.';

CREATE INDEX product_access_closed_at_idx ON billing.product_access (merchant_id, LEAST(COALESCE(ends_at, 'infinity'::timestamp with time zone), COALESCE(revoked_at, 'infinity'::timestamp with time zone)))
    WHERE (ends_at IS NOT NULL OR revoked_at IS NOT NULL) AND deleted_at IS NULL;
CREATE INDEX product_access_customer_created_idx ON billing.product_access (merchant_id, customer_id, id);
CREATE INDEX product_access_customer_idx ON billing.product_access (merchant_id, customer_id, product_id) INCLUDE (starts_at, ends_at)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
COMMENT ON INDEX billing.product_access_customer_idx IS 'A customer''s live products: exact checks probe it per product granting a key; prefix reads walk it once.';
CREATE INDEX product_access_destructive_run_id_idx ON billing.product_access (merchant_id, destructive_run_id)
    WHERE destructive_run_id IS NOT NULL;
CREATE UNIQUE INDEX product_access_grant_key ON billing.product_access (merchant_id, grant_id)
    WHERE deleted_at IS NULL;
CREATE INDEX product_access_payment_idx ON billing.product_access (merchant_id, payment_id)
    WHERE payment_id IS NOT NULL;
CREATE INDEX product_access_product_idx ON billing.product_access (merchant_id, product_id, customer_id) INCLUDE (starts_at, ends_at)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
COMMENT ON INDEX billing.product_access_product_idx IS 'A product''s live holders in customer order: reverse lookups and affected-holder counts.';
CREATE INDEX product_access_source_idx ON billing.product_access (merchant_id, source_type, source_id);

CREATE FUNCTION billing.step_access_version() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid; cid uuid;
BEGIN
    FOR mid, cid IN SELECT DISTINCT merchant_id, customer_id FROM changed ORDER BY merchant_id, customer_id LOOP
        UPDATE billing.customers SET access_version = access_version + 1 WHERE merchant_id = mid AND id = cid;
    END LOOP;
    RETURN NULL;
END $$;

CREATE TRIGGER product_access_release_ownership_claims AFTER UPDATE OF revoked_at, deleted_at ON billing.product_access
    FOR EACH ROW EXECUTE FUNCTION billing.release_ended_ownership_claims();
CREATE TRIGGER product_access_step_version_delete AFTER DELETE ON billing.product_access REFERENCING OLD TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();
CREATE TRIGGER product_access_step_version_insert AFTER INSERT ON billing.product_access REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();
CREATE TRIGGER product_access_step_version_update AFTER UPDATE ON billing.product_access REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();

CREATE TABLE billing.order_lines (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    order_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    position integer NOT NULL CHECK ("position" >= 0 AND "position" < 100),
    price_id uuid,
    product_id uuid,
    description text NOT NULL CHECK (description <> ''),
    quantity integer CHECK (quantity >= 1),
    unit_amount bigint NOT NULL,
    amount bigint NOT NULL,
    ownership text CHECK (ownership IN ('unique', 'consumable', 'extend')),
    claim_key text,
    billing_interval_hours integer CHECK (billing_interval_hours > 0),
    access_duration_hours integer CHECK (access_duration_hours > 0),
    credit_grant jsonb,
    subscription_id uuid,
    product_access_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    source text,
    provider_amount boolean NOT NULL DEFAULT false,
    provider_access_default boolean NOT NULL DEFAULT false,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT order_lines_order_id_position_key UNIQUE (merchant_id, order_id, "position"),
    CONSTRAINT order_lines_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT order_lines_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id),
    CONSTRAINT order_lines_product_access_id_fkey FOREIGN KEY (merchant_id, product_access_id) REFERENCES billing.product_access (merchant_id, id),
    CONSTRAINT order_lines_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CONSTRAINT order_lines_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id),
    CONSTRAINT order_lines_source_check CHECK (
        (source IS NULL) = (price_id IS NOT NULL AND product_id IS NOT NULL AND ownership IS NOT NULL)
        AND source <> ''
    ),
    CONSTRAINT order_lines_amount_check CHECK (
        (NOT provider_amount AND unit_amount >= 0 AND amount = (unit_amount * COALESCE(quantity, 1)))
        OR (provider_amount AND (amount >= 0 OR source = 'provider_adjustment'))
    ),
    CONSTRAINT order_lines_quantified_check CHECK (quantity IS NOT NULL OR billing_interval_hours IS NOT NULL OR source IS NOT NULL),
    CONSTRAINT order_lines_seats_check CHECK (quantity IS NULL OR quantity = 1 OR ownership = 'consumable' OR billing_interval_hours IS NOT NULL),
    CONSTRAINT order_lines_claim_key_check CHECK ((claim_key IS NOT NULL) = (ownership = 'unique')),
    CONSTRAINT order_lines_provider_access_choice_check CHECK (NOT provider_access_default OR access_duration_hours IS NULL)
);
COMMENT ON TABLE billing.order_lines IS 'One line of an order, frozen at creation: a price, its quantity (seats on a per-seat recurring price, units of a consumable, NULL on any other recurring price) and amounts, the ownership rule it was sold under, and what paying it produced (subscription_id, product_access_id). Retention: deleted with their order.';
COMMENT ON COLUMN billing.order_lines.claim_key IS 'product:<id> or tier_group:<group>: the ownership a unique line claims while its order is live.';
COMMENT ON COLUMN billing.order_lines.source IS 'A usage line''s accrual source (e.g. metered:<meter>); NULL on a priced line.';
COMMENT ON COLUMN billing.order_lines.provider_amount IS 'The finalized provider amount is authoritative, including exact seat rounding and explicit signed provider adjustments; quantity and unit amount remain the accepted snapshot.';
COMMENT ON COLUMN billing.order_lines.provider_access_default IS 'Frozen access choice accepted on a provider bill: use its verified service-period end, otherwise the explicit hourly duration.';

CREATE INDEX order_lines_price_id_idx ON billing.order_lines (merchant_id, price_id);
CREATE INDEX order_lines_subscription_id_idx ON billing.order_lines (merchant_id, subscription_id)
    WHERE subscription_id IS NOT NULL;

CREATE TABLE billing.access_cutover_approvals (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    change text NOT NULL CHECK (change IN ('lost', 'gained')),
    approved_by text NOT NULL CHECK (octet_length(approved_by) BETWEEN 1 AND 255),
    approved_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT access_cutover_approvals_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id)
);
COMMENT ON TABLE billing.access_cutover_approvals IS 'Access changes an operator approved before the cutover from per-key entitlement windows to product access: a key a customer loses (their product dropped it after purchase) or gains (their product added it). The cutover refuses any change not listed. Retention: permanent, never pruned.';

CREATE UNIQUE INDEX access_cutover_approvals_key ON billing.access_cutover_approvals (merchant_id, customer_id, entitlement, change);

CREATE FUNCTION billing.convert_entitlement_windows(p_merchant uuid, p_source regclass, p_at timestamptz) RETURNS bigint
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE converted bigint;
BEGIN
    DROP TABLE IF EXISTS pg_temp.access_conversion;
    EXECUTE format($q$
    CREATE TEMP TABLE access_conversion ON COMMIT DROP AS
    WITH win AS (
        SELECT e.customer_id, e.entitlement::text COLLATE "C" AS entitlement, e.starts_at, e.ends_at,
               e.revoked_at, e.revoke_reason, e.destructive_run_id::text AS destructive_run_id,
               g.source_type, COALESCE(g.source_id, g.id::text) AS source_id,
               CASE WHEN g.source_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN g.source_id::uuid END AS source_uuid
        FROM %1$s e
        JOIN grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id AND g.id = e.grant_id
        WHERE e.merchant_id = %2$L AND e.deleted_at IS NULL
    ), purchases AS (
        SELECT w.customer_id, line.product_id, 'purchase'::text AS source_type, w.source_id, pay.id AS payment_id,
               min(w.starts_at) AS starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id
        FROM win w
        JOIN payments pay ON pay.merchant_id = %2$L AND pay.id = w.source_uuid AND pay.customer_id = w.customer_id
        CROSS JOIN LATERAL (
            SELECT l.product_id FROM order_lines l
            WHERE l.merchant_id = %2$L AND l.order_id = pay.order_id AND l.product_id IS NOT NULL
            ORDER BY (SELECT count(*) FROM product_entitlements pe
                      WHERE pe.merchant_id = %2$L AND pe.product_id = l.product_id AND pe.removed_at IS NULL
                        AND pe.entitlement = w.entitlement) DESC, l.position
            LIMIT 1
        ) line
        WHERE w.source_type = 'purchase'
        GROUP BY w.customer_id, line.product_id, w.source_id, pay.id
    ), periods AS (
        SELECT w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement) AS keys
        FROM win w WHERE w.source_type IN ('subscription', 'grace')
        GROUP BY w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at
    ), subscription_windows AS (
        SELECT p.customer_id, best.product_id, p.source_type, p.source_id, NULL::uuid AS payment_id, p.starts_at, p.ends_at,
               p.revoked, p.revoked_at, p.revoke_reason, p.destructive_run_id
        FROM periods p
        JOIN subscriptions s ON s.merchant_id = %2$L AND s.id = p.source_uuid
        CROSS JOIN LATERAL (
            SELECT c.product_id FROM (
                SELECT s.product_id
                UNION SELECT l.product_id FROM payments pay
                JOIN order_lines l ON l.merchant_id = pay.merchant_id AND l.order_id = pay.order_id
                WHERE pay.merchant_id = %2$L AND pay.subscription_id = s.id AND l.product_id IS NOT NULL
            ) c
            ORDER BY (SELECT count(*) FROM product_entitlements pe
                      WHERE pe.merchant_id = %2$L AND pe.product_id = c.product_id AND pe.removed_at IS NULL
                        AND pe.entitlement = ANY (p.keys)) DESC,
                     (c.product_id = s.product_id) DESC, c.product_id
            LIMIT 1
        ) best
    ), admin_groups AS (
        SELECT w.customer_id, w.source_id, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement ORDER BY w.entitlement) AS keys
        FROM win w WHERE w.source_type = 'admin'
        GROUP BY w.customer_id, w.source_id, w.starts_at
    ), admin_windows AS (
        SELECT a.customer_id, match.product_id, 'grant'::text AS source_type, a.source_id, NULL::uuid AS payment_id, a.starts_at, a.ends_at,
               a.revoked, a.revoked_at, a.revoke_reason, a.destructive_run_id
        FROM admin_groups a
        CROSS JOIN LATERAL (
            SELECT candidate.product_id
            FROM product_entitlements candidate
            JOIN products p ON p.merchant_id = candidate.merchant_id AND p.id = candidate.product_id
            WHERE candidate.merchant_id = %2$L AND candidate.entitlement = a.keys[1] AND candidate.removed_at IS NULL
              AND (SELECT array_agg(pe.entitlement ORDER BY pe.entitlement) FROM product_entitlements pe
                   WHERE pe.merchant_id = candidate.merchant_id AND pe.product_id = candidate.product_id AND pe.removed_at IS NULL) = a.keys
            ORDER BY p.archived, p.key
            LIMIT 1
        ) match
    ), ownership_windows AS (
        SELECT g.customer_id, g.product_id, CASE g.source_type WHEN 'admin' THEN 'grant' ELSE g.source_type END AS source_type,
               COALESCE(g.source_id, g.id::text) AS source_id, g.payment_id, g.starts_at, g.ends_at,
               t.id IS NOT NULL AS revoked, t.starts_at AS revoked_at, COALESCE(t.reason, t.event) AS revoke_reason, NULL::text AS destructive_run_id
        FROM grants g
        LEFT JOIN grants t ON t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
        WHERE g.merchant_id = %2$L AND g.kind = 'ownership' AND g.event = 'grant' AND g.product_id IS NOT NULL
          AND NOT EXISTS (SELECT 1 FROM purchases p WHERE p.payment_id = g.payment_id AND p.product_id = g.product_id)
    )
    SELECT uuidv7() AS grant_id, uuidv7() AS access_id, c.customer_id, c.product_id, c.source_type, c.source_id, c.payment_id,
           c.starts_at, c.ends_at, c.revoked AND c.revoked_at IS NOT NULL AS revoked, c.revoked_at,
           COALESCE(c.revoke_reason, 'revoked') AS revoke_reason, c.destructive_run_id::uuid AS destructive_run_id
    FROM (
        SELECT * FROM purchases UNION ALL SELECT * FROM subscription_windows
        UNION ALL SELECT * FROM admin_windows UNION ALL SELECT * FROM ownership_windows
    ) c
    $q$, p_source, p_merchant);

    INSERT INTO billing.grants (id, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, starts_at, ends_at, reason, actor, grant_reason)
    SELECT c.grant_id, p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'grant', c.starts_at, c.ends_at,
           'product access migration', CASE WHEN c.source_type = 'grant' THEN 'migration' END, CASE WHEN c.source_type = 'grant' THEN 'migration' END
    FROM access_conversion c;
    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, starts_at, reason)
    SELECT p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'revoke', c.grant_id, c.revoked_at, c.revoke_reason
    FROM access_conversion c WHERE c.revoked;
    INSERT INTO billing.product_access (id, merchant_id, customer_id, product_id, grant_id, source_type, source_id, payment_id,
                                        starts_at, ends_at, revoked_at, revoke_reason, destructive_run_id)
    SELECT c.access_id, p_merchant, c.customer_id, c.product_id, c.grant_id, c.source_type, c.source_id, c.payment_id, c.starts_at, c.ends_at,
           CASE WHEN c.revoked THEN c.revoked_at END, CASE WHEN c.revoked THEN c.revoke_reason END, c.destructive_run_id
    FROM access_conversion c;
    GET DIAGNOSTICS converted = ROW_COUNT;

    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, spec_snapshot, starts_at, reason)
    SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id, 'supersede', g.id, g.spec_snapshot, p_at, 'product access migration'
    FROM billing.grants g
    WHERE g.merchant_id = p_merchant AND g.kind IN ('entitlement', 'ownership') AND g.event = 'grant'
      AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
                        AND t.event IN ('revoke', 'expire', 'supersede'));
    RETURN converted;
END $_$;

CREATE FUNCTION billing.product_access_changes(p_merchant uuid, p_source regclass, p_at timestamptz) RETURNS TABLE(customer_id uuid, entitlement text, change text)
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
BEGIN
    RETURN QUERY EXECUTE format($q$
    WITH old AS (
        SELECT e.customer_id, e.entitlement::text COLLATE "C" AS entitlement,
               range_agg(tstzrange(GREATEST(e.starts_at, %3$L::timestamptz), e.ends_at)) AS r
        FROM %1$s e
        WHERE e.merchant_id = %2$L AND e.revoked_at IS NULL AND e.deleted_at IS NULL
          AND (e.ends_at IS NULL OR e.ends_at > %3$L::timestamptz)
        GROUP BY 1, 2
    ), new AS (
        SELECT pa.customer_id, pe.entitlement::text COLLATE "C" AS entitlement,
               range_agg(tstzrange(GREATEST(pa.starts_at, pe.added_at, %3$L::timestamptz), LEAST(pa.ends_at, pe.removed_at))) AS r
        FROM product_access pa
        JOIN product_entitlements pe ON pe.merchant_id = pa.merchant_id AND pe.product_id = pa.product_id
        WHERE pa.merchant_id = %2$L AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
          AND (pa.ends_at IS NULL OR pa.ends_at > %3$L::timestamptz)
          AND (pe.removed_at IS NULL OR pe.removed_at > %3$L::timestamptz)
          AND GREATEST(pa.starts_at, pe.added_at, %3$L::timestamptz) < COALESCE(LEAST(pa.ends_at, pe.removed_at), 'infinity'::timestamptz)
        GROUP BY 1, 2
    )
    SELECT COALESCE(o.customer_id, n.customer_id), COALESCE(o.entitlement, n.entitlement)::text, d.change
    FROM old o FULL JOIN new n ON n.customer_id = o.customer_id AND n.entitlement = o.entitlement
    CROSS JOIN LATERAL (VALUES
        ('lost', NOT isempty(COALESCE(o.r, '{}'::tstzmultirange) - COALESCE(n.r, '{}'::tstzmultirange))),
        ('gained', NOT isempty(COALESCE(n.r, '{}'::tstzmultirange) - COALESCE(o.r, '{}'::tstzmultirange)))
    ) d(change, changed)
    WHERE d.changed
    ORDER BY 1, 2, 3
    $q$, p_source, p_merchant, p_at);
END $_$;

CREATE FUNCTION billing.product_access_conversion_notes(p_merchant uuid, p_source regclass, p_at timestamptz) RETURNS TABLE(customer_id uuid, note text, source_type text, source_id text, entitlements text[])
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
BEGIN
    RETURN QUERY EXECUTE format($q$
    WITH live AS (
        SELECT e.customer_id, e.entitlement::text AS entitlement, e.ends_at, g.source_type, COALESCE(g.source_id, g.id::text) AS source_id
        FROM %1$s e
        JOIN grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id AND g.id = e.grant_id
        WHERE e.merchant_id = %2$L AND e.revoked_at IS NULL AND e.deleted_at IS NULL
          AND (e.ends_at IS NULL OR e.ends_at > %3$L::timestamptz)
    )
    SELECT l.customer_id, 'unmapped'::text, l.source_type, l.source_id, array_agg(DISTINCT l.entitlement ORDER BY l.entitlement)
    FROM live l
    WHERE NOT EXISTS (SELECT 1 FROM product_access pa
        WHERE pa.merchant_id = %2$L AND pa.customer_id = l.customer_id AND pa.source_id = l.source_id
          AND pa.source_type = CASE l.source_type WHEN 'admin' THEN 'grant' ELSE l.source_type END)
    GROUP BY 1, 2, 3, 4
    UNION ALL
    SELECT l.customer_id, 'mixed_duration'::text, l.source_type, l.source_id, array_agg(DISTINCT l.entitlement ORDER BY l.entitlement)
    FROM live l WHERE l.source_type = 'purchase'
    GROUP BY 1, 2, 3, 4
    HAVING count(DISTINCT COALESCE(l.ends_at, 'infinity'::timestamptz)) > 1
    ORDER BY 1, 2, 3, 4
    $q$, p_source, p_merchant, p_at);
END $_$;

CREATE TABLE billing.customer_entitlement_cache (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    quantity integer CHECK (quantity IS NULL OR quantity >= 1),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT customer_entitlement_cache_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id)
);
COMMENT ON TABLE billing.customer_entitlement_cache IS 'A heavy buyer''s keys as of their stamps row: the keys of the products they hold. Derived; rebuilt on read.';
COMMENT ON COLUMN billing.customer_entitlement_cache.quantity IS 'The most seats of the key a held per-seat product gives; NULL when none is per seat.';

CREATE UNIQUE INDEX customer_entitlement_cache_key ON billing.customer_entitlement_cache (merchant_id, customer_id, entitlement);

CREATE TABLE billing.customer_entitlement_cache_stamps (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    entitlement_generation bigint CONSTRAINT customer_entitlement_cache_stam_entitlement_generation_not_null NOT NULL,
    access_version bigint NOT NULL,
    valid_from timestamptz NOT NULL,
    valid_until timestamptz,
    keys integer NOT NULL,
    held_products integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT customer_entitlement_cache_stamps_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT customer_entitlement_cache_stamps_window_check CHECK (valid_until IS NULL OR valid_from < valid_until),
    CONSTRAINT customer_entitlement_cache_stamps_counts_check CHECK (keys >= 0 AND held_products >= 0)
);
COMMENT ON TABLE billing.customer_entitlement_cache_stamps IS 'When a heavy buyer''s cached keys are valid: built at entitlement_generation and access_version, for instants in [valid_from, valid_until), the next start or end of one of their windows. Derived; rebuilt on read.';

CREATE UNIQUE INDEX customer_entitlement_cache_stamps_customer_key ON billing.customer_entitlement_cache_stamps (merchant_id, customer_id);

CREATE TABLE billing.carts (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    lines jsonb NOT NULL,
    last_changed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id),
    CONSTRAINT carts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CHECK (jsonb_typeof(lines) = 'array' AND jsonb_array_length(lines) <= 100)
);
COMMENT ON TABLE billing.carts IS 'A customer''s cart: up to 100 lines ({price_id, quantity, amount}, in the order added), priced live as an order preview. Every change goes through one function in one transaction and sets last_changed_at; an order made from the cart takes its lines. Retention: carts are deleted 30 days after last_changed_at.';
COMMENT ON COLUMN billing.carts.lines IS 'JSON array of {price_id, quantity, amount}: quantity and amount as on an order line, absent when not given.';

CREATE INDEX carts_last_changed_at_idx ON billing.carts (last_changed_at);

CREATE TABLE billing.owed_items (
    id uuid CONSTRAINT invoice_items_id_not_null NOT NULL DEFAULT uuidv7(),
    merchant_id uuid CONSTRAINT invoice_items_merchant_id_not_null NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid CONSTRAINT invoice_items_customer_id_not_null NOT NULL,
    currency text CONSTRAINT invoice_items_currency_not_null NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    order_id uuid,
    source_type text CONSTRAINT invoice_items_source_type_not_null NOT NULL,
    source_id text CONSTRAINT invoice_items_source_id_not_null NOT NULL,
    accrued_at timestamptz CONSTRAINT invoice_items_invoice_at_not_null NOT NULL,
    amount bigint CONSTRAINT invoice_items_amount_not_null NOT NULL CHECK (amount >= 0),
    status text CONSTRAINT invoice_items_status_not_null NOT NULL DEFAULT 'pending',
    metadata jsonb CONSTRAINT invoice_items_metadata_not_null NOT NULL,
    created_at timestamptz CONSTRAINT invoice_items_created_at_not_null NOT NULL DEFAULT now(),
    updated_at timestamptz CONSTRAINT invoice_items_updated_at_not_null NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT owed_items_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT owed_items_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT owed_items_status_check CHECK (status IN ('pending', 'billed', 'voided') AND (order_id IS NOT NULL) = (status = 'billed'))
);
COMMENT ON TABLE billing.owed_items IS 'Owed charges awaiting their usage order: an accrual is born pending and gates arrears exposure; finalization moves it onto an order (order_id, status billed), so it bills once. Retention: permanent, never pruned.';

CREATE INDEX owed_items_billed_idx ON billing.owed_items (merchant_id, customer_id, currency, accrued_at)
    WHERE order_id IS NOT NULL;
CREATE INDEX owed_items_order_id_idx ON billing.owed_items (merchant_id, order_id);
CREATE INDEX owed_items_pending_idx ON billing.owed_items (merchant_id, customer_id, currency, accrued_at)
    WHERE order_id IS NULL AND status = 'pending';
CREATE UNIQUE INDEX owed_items_source_key ON billing.owed_items (merchant_id, customer_id, currency, source_type, source_id);

CREATE TABLE billing.ownership_claims (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    claim_key text NOT NULL CHECK (claim_key ~ '^(product|tier_group):.+$'),
    order_id uuid NOT NULL,
    holder_type text NOT NULL CHECK (holder_type IN ('order', 'subscription', 'product_access')),
    holder_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, claim_key),
    CONSTRAINT ownership_claims_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT ownership_claims_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT ownership_claims_holder_check CHECK (holder_type <> 'order' OR holder_id = order_id)
);
COMMENT ON TABLE billing.ownership_claims IS 'At most one live ownership per customer and key (product or tier group). An order claims its unique lines when created; paying hands each claim to the subscription or product access it produced; cancel, expiry, the subscription''s end or the access''s revocation deletes it.';

CREATE INDEX ownership_claims_holder_idx ON billing.ownership_claims (merchant_id, holder_type, holder_id);
CREATE INDEX ownership_claims_order_id_idx ON billing.ownership_claims (merchant_id, customer_id, order_id);

CREATE TABLE billing.solana_pay_references (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    reference text NOT NULL,
    order_attempt_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('purchase', 'subscribe')),
    status text NOT NULL CHECK (status IN ('pending', 'confirmed', 'expired')),
    expires_at timestamptz NOT NULL,
    watch_ends_at timestamptz NOT NULL,
    next_poll_at timestamptz NOT NULL,
    signature text,
    seen_until text,
    scan_stack text[] NOT NULL DEFAULT '{}',
    scan_below text,
    built_transaction text,
    built_valid_height bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, reference),
    CONSTRAINT solana_pay_references_order_attempt_id_key UNIQUE (merchant_id, order_attempt_id),
    CONSTRAINT solana_pay_references_order_attempt_id_fkey FOREIGN KEY (merchant_id, order_attempt_id) REFERENCES billing.order_attempts (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT solana_pay_references_signature_check CHECK ((status = 'confirmed') = (signature IS NOT NULL)),
    CONSTRAINT solana_pay_references_window_check CHECK (watch_ends_at >= expires_at),
    CONSTRAINT solana_pay_references_built_check CHECK ((built_transaction IS NULL) = (built_valid_height IS NULL))
);
COMMENT ON TABLE billing.solana_pay_references IS 'One Solana Pay reference per order attempt. pending = awaiting a transfer landed by expires_at; confirmed = one signature credited (or mirrored); expired = nothing credited by expires_at. Purchase references stay watched until watch_ends_at so a second or late transfer is recorded, then retention deletes the settled row. seen_until is the newest signature whose older history is fully processed; scan_stack holds the before-cursors of an unfinished walk down the history and scan_below the cursor whose older signatures were just processed, so no signature is ever skipped however many land on the reference; a reference is never collected mid-walk. built_transaction is the one transaction-request tx offered while its blockhash can still land. Retention: settled references are deleted after their 7-day watch window.';

CREATE INDEX solana_pay_references_next_poll_at_idx ON billing.solana_pay_references (next_poll_at);
CREATE INDEX solana_pay_references_watch_ends_at_idx ON billing.solana_pay_references (watch_ends_at)
    WHERE status <> 'pending';

CREATE FUNCTION billing.solana_pay_references_wake() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('openrails_solana_pay:' || TG_TABLE_SCHEMA, '');
    RETURN NULL;
END;
$$;

CREATE TRIGGER solana_pay_references_wake AFTER INSERT OR UPDATE OF next_poll_at, status, watch_ends_at ON billing.solana_pay_references
    FOR EACH ROW EXECUTE FUNCTION billing.solana_pay_references_wake();

CREATE TABLE billing.solana_pay_receipts (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    reference text NOT NULL,
    signature text NOT NULL,
    order_attempt_id uuid NOT NULL,
    disposition text NOT NULL,
    review_reason text,
    recipient text NOT NULL,
    token_mint text NOT NULL,
    expected_amount bigint NOT NULL,
    received_amount bigint NOT NULL,
    payer text,
    landed_at timestamptz,
    payment_id uuid,
    resolved_at timestamptz,
    resolution text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, reference, signature),
    CONSTRAINT solana_pay_receipts_order_attempt_id_fkey FOREIGN KEY (merchant_id, order_attempt_id) REFERENCES billing.order_attempts (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT solana_pay_receipts_payment_id_fkey FOREIGN KEY (merchant_id, payment_id) REFERENCES billing.payments (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT solana_pay_receipts_disposition_check CHECK (
        (disposition = 'credited' AND payment_id IS NOT NULL AND (review_reason IS NULL OR review_reason = 'overpaid'))
        OR (disposition = 'review' AND payment_id IS NULL AND review_reason IN ('already_paid', 'late', 'underpaid', 'session_closed', 'wrong_asset', 'unreadable', 'settle_failed'))
        OR (disposition = 'duplicate' AND payment_id IS NULL AND review_reason = 'claimed_elsewhere')
        OR (disposition = 'ignored' AND payment_id IS NULL AND review_reason IS NULL)
    ),
    CONSTRAINT solana_pay_receipts_resolution_check CHECK ((resolved_at IS NULL) = (resolution IS NULL) AND (resolved_at IS NULL OR review_reason IS NOT NULL)),
    CONSTRAINT solana_pay_receipts_amounts_check CHECK (expected_amount >= 0 AND received_amount >= 0)
);
COMMENT ON TABLE billing.solana_pay_receipts IS 'Every signature observed on a Solana Pay reference, recorded once. credited = the order attempt was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed, wrong_asset, unreadable, settle_failed) and needs a refund or operator decision, closed by resolved_at; duplicate = the transfer already settled another reference; ignored = no value to the merchant (deleted with its reference). A transfer to one recipient in one mint is credited or reviewed at most once across every reference. Unresolved reviews refuse the billing archive. Retention: permanent for credited and review receipts; an ignored receipt goes with its settled reference.';

CREATE INDEX solana_pay_receipts_created_at_idx ON billing.solana_pay_receipts (merchant_id, created_at)
    WHERE review_reason IS NOT NULL AND resolved_at IS NULL;
CREATE INDEX solana_pay_receipts_order_attempt_id_idx ON billing.solana_pay_receipts (merchant_id, order_attempt_id);
CREATE UNIQUE INDEX solana_pay_receipts_signature_recipient_token_mint_key ON billing.solana_pay_receipts (signature, recipient, token_mint)
    WHERE disposition IN ('credited', 'review');

-- ---------------------------------------------------------------------------
-- Provider operations
-- ---------------------------------------------------------------------------

CREATE TABLE billing.provider_intents (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    rail text NOT NULL,
    intent_type text NOT NULL,
    subscription_id uuid,
    payment_id uuid,
    price_id uuid,
    payload jsonb,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    status text NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    lease_expires_at timestamptz,
    origin text NOT NULL CHECK (origin IN ('user', 'admin', 'system', 'provider')),
    origin_reason text,
    actor text,
    last_failure_reason text,
    expires_at timestamptz,
    result_evidence jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    executed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    psp_id uuid,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' ELSE NULL END) STORED,
    custodian_id uuid,
    subject text CHECK (subject <> ''),
    credential text CHECK (credential <> ''),
    destructive_action_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT provider_intents_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_intents_destructive_action_fkey FOREIGN KEY (merchant_id, destructive_action_id) REFERENCES billing.destructive_actions (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_intents_destructive_run_id_destructive_run_class_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs (merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT provider_intents_payment_id_fkey FOREIGN KEY (merchant_id, payment_id) REFERENCES billing.payments (merchant_id, id) ON DELETE SET NULL (payment_id),
    CONSTRAINT provider_intents_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE SET NULL (price_id),
    CONSTRAINT provider_intents_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT provider_intents_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE SET NULL (subscription_id),
    CONSTRAINT provider_intents_executed_check CHECK (status <> 'succeeded' OR executed_at IS NOT NULL),
    CHECK (status IN ('pending', 'in_flight', 'succeeded', 'unknown_needs_verify', 'failed_retryable', 'failed_terminal', 'superseded', 'expired')),
    CONSTRAINT provider_intents_addressed_check CHECK (psp_id IS NOT NULL OR custodian_id IS NOT NULL)
);
COMMENT ON TABLE billing.provider_intents IS 'Durable, effectively-once outbox for outbound provider mutations. One row per logical intent (unique per merchant on idempotency_key). Each is run by its own openrails.provider_operation River job, queued in the transaction that records it (a manual card removal runs only on request); an ambiguous outcome is resolved by provider reads before any retry. Retention: finished intents that only instructed a provider (cancel, update, archive, vault, token, account updater) are deleted 25 months (761 days) after they last changed; an intent that moved or refused money, enrolled a membership or erased a card is permanent.';
COMMENT ON COLUMN billing.provider_intents.rail IS 'Rail the mutation targets (e.g. ''nmi'', ''stripe'').';
COMMENT ON COLUMN billing.provider_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff), for example nmi_delete_subscription or manual_rebill.';
COMMENT ON COLUMN billing.provider_intents.idempotency_key IS 'Deterministic identity of the logical intent within the merchant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';
COMMENT ON COLUMN billing.provider_intents.lease_expires_at IS 'The running attempt''s claim. An in_flight row whose lease elapsed lost its attempt and becomes unknown_needs_verify: a provider read settles it before any retry.';
COMMENT ON COLUMN billing.provider_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';
COMMENT ON COLUMN billing.provider_intents.actor IS 'The invoker that produced a user/admin-origin intent: the party that acted, the subject itself when it acted itself. NULL for system-origin. Keys the anti-credential-compromise rate ceiling (per-invoker + per-merchant rolling-hour count of destructive ops).';
COMMENT ON COLUMN billing.provider_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';
COMMENT ON COLUMN billing.provider_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';
COMMENT ON COLUMN billing.provider_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';
COMMENT ON COLUMN billing.provider_intents.psp_id IS 'PSP the outbound intent was enqueued against. Required unless the intent is custodian-addressed (provider_intents_addressed_check).';
COMMENT ON COLUMN billing.provider_intents.destructive_run_id IS 'The destructive run whose pass enqueued this intent. The reverse of that run supersedes the ones still pending/failed_retryable and reports the rest — succeeded ones as irreversible provider-side divergence, in_flight/unknown_needs_verify ones as ambiguous. Attribution only: never cleared, never used to delete a row.';
COMMENT ON COLUMN billing.provider_intents.custodian_id IS 'The custodian this outbound write is addressed to, for intents that target a custodian rather than a gateway account (the batch account updater). NULL for the ordinary PSP-addressed intent. Composite FK: an intent can only reference ITS OWN merchant''s custodian.';
COMMENT ON COLUMN billing.provider_intents.subject IS 'The subject whose authority produced a user/admin-origin intent: the staff member, application or customer. NULL for system-origin.';
COMMENT ON COLUMN billing.provider_intents.credential IS 'How the subject proved itself for a user/admin-origin intent, as kind:id (session:…, api_key:…). NULL for system-origin.';
COMMENT ON COLUMN billing.provider_intents.destructive_action_id IS 'Original admitted command; recovery and necessary consequences reuse its authority and never spend another unit.';

CREATE INDEX provider_intents_actor_created_at_intent_type_idx ON billing.provider_intents (actor, created_at, intent_type)
    WHERE origin IN ('user', 'admin');
-- Exact handle lookup serves both pending exclusion and permanent erasure history.
CREATE INDEX provider_intents_custodian_method_delete_idx ON billing.provider_intents (merchant_id, custodian_id, ((payload -> 'instrument'::text) ->> 'rail_method_ref'::text))
    WHERE intent_type = 'hyperswitch_method_delete';
CREATE INDEX provider_intents_destructive_action_idx ON billing.provider_intents (merchant_id, destructive_action_id)
    WHERE destructive_action_id IS NOT NULL;
CREATE INDEX provider_intents_destructive_run_id_idx ON billing.provider_intents (merchant_id, destructive_run_id)
    WHERE destructive_run_id IS NOT NULL;
CREATE UNIQUE INDEX provider_intents_idempotency_key_key ON billing.provider_intents (merchant_id, idempotency_key);
-- Initial membership identity is frozen in terms, including failed attempts
-- with no subscription row. Paid agreement lookup must not scan the whole book.
CREATE INDEX provider_intents_initial_membership_history_idx ON billing.provider_intents (merchant_id, ((payload -> 'terms'::text) ->> 'subscription_id'::text))
    WHERE intent_type = 'initial_membership' AND status = 'succeeded';
CREATE INDEX provider_intents_native_vault_delete_idx ON billing.provider_intents (merchant_id, psp_id, (payload ->> 'rail_customer_ref'::text))
    WHERE intent_type = 'nmi_vault_delete';
CREATE INDEX provider_intents_next_attempt_at_idx ON billing.provider_intents (next_attempt_at)
    WHERE status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify');
CREATE UNIQUE INDEX provider_intents_obligation_unreleased_key ON billing.provider_intents (merchant_id, subscription_id, (payload ->> 'order_reference'::text))
    WHERE intent_type IN ('subscription_collection', 'manual_rebill') AND status NOT IN ('failed_terminal', 'superseded', 'expired');
CREATE UNIQUE INDEX provider_intents_open_subscription_collection_key ON billing.provider_intents (merchant_id, subscription_id)
    WHERE intent_type = 'subscription_collection' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
CREATE INDEX provider_intents_order_attempt_idx ON billing.provider_intents (merchant_id, (payload ->> 'order_attempt_id'::text))
    WHERE payload ? 'order_attempt_id';
-- An engine renewal finds the sale that paid an order's first period by the
-- order it paid: one index read, not every past sale of the merchant.
CREATE INDEX provider_intents_order_sale_idx ON billing.provider_intents (merchant_id, (payload ->> 'order_id'::text))
    WHERE intent_type = 'nmi_sale' AND status = 'succeeded' AND payload ? 'order_id';
CREATE INDEX provider_intents_origin_created_at_intent_type_idx ON billing.provider_intents (merchant_id, origin, created_at, intent_type);
CREATE INDEX provider_intents_psp_id_idx ON billing.provider_intents (merchant_id, psp_id)
    WHERE psp_id IS NOT NULL;
CREATE UNIQUE INDEX provider_intents_subscription_collection_slot_key ON billing.provider_intents (merchant_id, subscription_id, (payload ->> 'previous_period_end'::text), (payload ->> 'attempt'::text))
    WHERE intent_type = 'subscription_collection';
CREATE INDEX provider_intents_subscription_id_idx ON billing.provider_intents (merchant_id, subscription_id)
    WHERE subscription_id IS NOT NULL;
CREATE UNIQUE INDEX provider_intents_subscription_id_open_manual_rebill_key ON billing.provider_intents (merchant_id, subscription_id)
    WHERE intent_type = 'manual_rebill' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
CREATE UNIQUE INDEX provider_intents_subscription_id_tier_change_subscription_key ON billing.provider_intents (merchant_id, subscription_id)
    WHERE intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership') AND subscription_id IS NOT NULL AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
-- What retention deletes: finished intents that only carried an instruction to
-- a provider. Intents that moved or refused money, enrolled a membership or
-- erased a card are the record of that and are not in this index.
CREATE INDEX provider_intents_updated_at_idx ON billing.provider_intents (merchant_id, updated_at)
    WHERE status IN ('succeeded', 'failed_terminal', 'superseded', 'expired') AND destructive_run_id IS NULL AND intent_type IN ('nmi_delete_subscription', 'stripe_cancel_subscription', 'ccbill_cancel_subscription', 'nmi_payment_method_update', 'nmi_payment_source_update', 'nmi_card_vault', 'network_token', 'stripe_archive_price', 'stripe_archive_product', 'solana_sunset_plan', 'account_updater');

CREATE TABLE billing.provider_mutation_logs (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    rail text NOT NULL,
    psp_id uuid,
    provider_intent_id uuid,
    intent_type text,
    idempotency_key text,
    attempt integer NOT NULL,
    phase text NOT NULL,
    reason text,
    evidence jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    custodian_id uuid,
    origin text,
    actor text,
    executor text,
    operation_id uuid,
    subscription_id uuid,
    reason_code text,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT provider_mutation_logs_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_mutation_logs_provider_intent_id_fkey FOREIGN KEY (merchant_id, provider_intent_id) REFERENCES billing.provider_intents (merchant_id, id) ON DELETE SET NULL (provider_intent_id),
    CONSTRAINT provider_mutation_logs_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT provider_mutation_logs_addressed_check CHECK (psp_id IS NOT NULL OR custodian_id IS NOT NULL),
    CHECK (phase IN ('attempting', 'succeeded', 'failed', 'unknown', 'parked'))
);
COMMENT ON TABLE billing.provider_mutation_logs IS 'Append-only operator history for external provider mutations executed from provider intents/convergence: the record of what we did to the outside world — INSERT plus the whole-merchant purge DELETE only, never UPDATE, and never rolled back. Retention: rows are deleted 25 months (761 days) after created_at.';
COMMENT ON COLUMN billing.provider_mutation_logs.psp_id IS 'PSP the logged mutation was addressed to. Required unless the mutation is custodian-addressed (provider_mutation_logs_addressed_check).';
COMMENT ON COLUMN billing.provider_mutation_logs.phase IS 'Provider mutation lifecycle phase: attempting before the remote call, then succeeded/failed/unknown/parked after the handler classifies the result.';
COMMENT ON COLUMN billing.provider_mutation_logs.evidence IS 'Scrubbed structured metadata only. Never store API keys, authorization headers, card data, private keys, or unsanitized provider bodies.';
COMMENT ON COLUMN billing.provider_mutation_logs.custodian_id IS 'The custodian the logged mutation was addressed to, for custodian-addressed intents. NULL for the ordinary PSP-addressed mutation.';

CREATE INDEX provider_mutation_logs_created_at_idx ON billing.provider_mutation_logs (merchant_id, created_at DESC);
CREATE INDEX provider_mutation_logs_custodian_id_idx ON billing.provider_mutation_logs (merchant_id, custodian_id)
    WHERE custodian_id IS NOT NULL;
CREATE INDEX provider_mutation_logs_operation_history_idx ON billing.provider_mutation_logs (merchant_id, operation_id, created_at DESC, id DESC)
    WHERE phase IN ('parked', 'attempting', 'succeeded', 'failed');
CREATE INDEX provider_mutation_logs_provider_intent_id_idx ON billing.provider_mutation_logs (merchant_id, provider_intent_id)
    WHERE provider_intent_id IS NOT NULL;
CREATE INDEX provider_mutation_logs_psp_id_idx ON billing.provider_mutation_logs (merchant_id, psp_id)
    WHERE psp_id IS NOT NULL;
CREATE INDEX provider_mutation_logs_rail_phase_created_at_idx ON billing.provider_mutation_logs (rail, phase, created_at DESC);
CREATE INDEX provider_mutation_logs_subscription_history_idx ON billing.provider_mutation_logs (merchant_id, subscription_id, created_at DESC, id DESC)
    WHERE subscription_id IS NOT NULL;

-- An entry's content never changes. The one update let through is the foreign
-- key's own: the link to an intent that retention deleted going NULL.
CREATE TRIGGER immutable_provider_mutation_log_content BEFORE UPDATE ON billing.provider_mutation_logs
    FOR EACH ROW WHEN (NOT (new.provider_intent_id IS NULL AND (to_jsonb(new.*) - 'provider_intent_id') = (to_jsonb(old.*) - 'provider_intent_id'))) EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.provider_bills (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    source_kind text NOT NULL,
    source_key text NOT NULL,
    order_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    snapshot jsonb NOT NULL,
    provider_reference text NOT NULL,
    settlement_source text,
    non_cash_amount bigint NOT NULL DEFAULT 0,
    settled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, source_kind, source_key),
    CONSTRAINT provider_bills_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_bills_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_bills_identity_check CHECK (
        octet_length(source_kind) BETWEEN 1 AND 128
        AND octet_length(source_key) BETWEEN 1 AND 512
        AND octet_length(provider_reference) <= 512
    ),
    CHECK (jsonb_typeof(snapshot) = 'object' AND octet_length(snapshot::text) <= 262144),
    CONSTRAINT provider_bills_settlement_check CHECK (
        non_cash_amount >= 0
        AND (settled_at IS NULL) = (settlement_source IS NULL)
        AND settlement_source IN ('cash', 'credit', 'out_of_band', 'mixed')
    )
);
COMMENT ON TABLE billing.provider_bills IS 'Scoped immutable provider bill identity and accepted order snapshot. Multiple verified aliases can name one order. Settlement proof never executes collection. Retention: permanent with the financial order.';

CREATE INDEX provider_bills_order_id_idx ON billing.provider_bills (merchant_id, order_id);

CREATE TABLE billing.provider_charge_attributions (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    transaction_id text NOT NULL CHECK (octet_length(transaction_id) BETWEEN 1 AND 128),
    customer_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    declared_at timestamptz,
    status text NOT NULL,
    attempts integer NOT NULL CHECK (attempts >= 0),
    next_attempt_at timestamptz,
    payment_id uuid,
    resolved_at timestamptz,
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, transaction_id),
    CONSTRAINT provider_charge_attributions_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_charge_attributions_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_charge_attributions_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_charge_attributions_status_check CHECK (
        (status = 'pending' AND next_attempt_at IS NOT NULL AND payment_id IS NULL AND resolved_at IS NULL AND reason IS NULL)
        OR (status = 'resolved' AND next_attempt_at IS NULL AND payment_id IS NOT NULL AND resolved_at IS NOT NULL AND reason IS NULL)
        OR (status = 'unresolvable' AND next_attempt_at IS NULL AND payment_id IS NULL AND resolved_at IS NOT NULL AND reason IS NOT NULL AND reason <> '')
    )
);
COMMENT ON TABLE billing.provider_charge_attributions IS 'A declared provider charge without an amount: its provider transaction id names the subscription it belongs to. pending until OpenRails reads the transaction from the provider; resolved names the payment it recorded; unresolvable names why no payment was recorded. Retention: permanent, never pruned.';

CREATE TABLE billing.provider_schedules (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    initial_order_id uuid,
    accepted_version bigint NOT NULL CHECK (accepted_version > 0),
    source_reference text NOT NULL CHECK (octet_length(source_reference) BETWEEN 1 AND 512),
    provider_reference text NOT NULL CHECK (octet_length(provider_reference) BETWEEN 1 AND 512),
    first_due_at timestamptz NOT NULL,
    cadence_hours integer NOT NULL CHECK (cadence_hours >= 0),
    cadence_months integer NOT NULL CHECK (cadence_months BETWEEN 0 AND 1200),
    accepted_bill jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, subscription_id, source_reference),
    CONSTRAINT provider_schedules_merchant_id_subscription_id_accepted_ver_key UNIQUE (merchant_id, subscription_id, accepted_version),
    CONSTRAINT provider_schedules_merchant_id_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT provider_schedules_merchant_id_initial_order_id_fkey FOREIGN KEY (merchant_id, initial_order_id) REFERENCES billing.orders (merchant_id, id),
    CONSTRAINT provider_schedules_merchant_id_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id),
    CONSTRAINT provider_schedules_merchant_id_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id),
    CONSTRAINT provider_schedules_check CHECK ((cadence_hours > 0) <> (cadence_months > 0))
);
COMMENT ON TABLE billing.provider_schedules IS 'Immutable accepted provider schedule versions: first due instant, cadence and the accepted bill a provider-run renewal must match. Retention: permanent, never pruned.';

CREATE TABLE billing.provider_cycle_receipts (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    transaction_id text NOT NULL CHECK (transaction_id <> ''),
    subscription_id uuid NOT NULL,
    schedule_source_reference text NOT NULL,
    due_at timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    currency text NOT NULL,
    occurred_at timestamptz NOT NULL,
    approved boolean NOT NULL,
    PRIMARY KEY (merchant_id, psp_id, transaction_id),
    CONSTRAINT provider_cycle_receipts_merchant_id_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id),
    CONSTRAINT provider_cycle_receipts_merchant_id_subscription_id_schedu_fkey FOREIGN KEY (merchant_id, subscription_id, schedule_source_reference) REFERENCES billing.provider_schedules (merchant_id, subscription_id, source_reference)
);
COMMENT ON TABLE billing.provider_cycle_receipts IS 'Verified provider transactions bound once to one due cycle of an accepted schedule version. Retention: permanent, never pruned.';

CREATE INDEX provider_cycle_receipts_settled_schedule_idx ON billing.provider_cycle_receipts (merchant_id, subscription_id, schedule_source_reference, due_at DESC)
    WHERE approved;

CREATE TABLE billing.webhook_events (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    psp_id uuid,
    custodian_id uuid,
    op text NOT NULL,
    event_id text NOT NULL CHECK (octet_length(event_id) BETWEEN 1 AND 255),
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_events_psp_id_custodian_id_op_event_id_key UNIQUE NULLS NOT DISTINCT (merchant_id, psp_id, custodian_id, op, event_id),
    CONSTRAINT webhook_events_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_events_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_events_source_check CHECK ((psp_id IS NULL) <> (custodian_id IS NULL))
);
COMMENT ON TABLE billing.webhook_events IS 'webhook dedup truth: one row per applied event of a source (a PSP, or a custodian). Event ids are unique within the account that sent them. Pending/lease state is the claim in idempotency_keys; a row here means effects are durably applied. Retention: completed events are deleted 90 days after completed_at.';
COMMENT ON COLUMN billing.webhook_events.op IS 'webhook.<source>.<event_type>.';

CREATE INDEX webhook_events_completed_at_idx ON billing.webhook_events (merchant_id, completed_at);

CREATE TRIGGER immutable_webhook_event_content BEFORE UPDATE ON billing.webhook_events
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.webhook_health (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    psp_id uuid,
    custodian_id uuid,
    last_accepted_at timestamptz,
    last_pull_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_health_psp_id_custodian_id_key UNIQUE NULLS NOT DISTINCT (merchant_id, psp_id, custodian_id),
    CONSTRAINT webhook_health_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_health_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_health_source_check CHECK ((psp_id IS NULL) <> (custodian_id IS NULL))
);
COMMENT ON TABLE billing.webhook_health IS 'inbound-webhook health per event source (a PSP, or a custodian): accepted and pull watermarks. last_accepted_at is stamped only by verified webhooks; last_pull_at is the PSP refresh watermark the drift gate uses.';
COMMENT ON COLUMN billing.webhook_health.last_accepted_at IS 'Last verified webhook from the source; silence age is measured from here (or created_at when nothing was ever accepted).';

CREATE TABLE billing.webhook_health_daily (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    psp_id uuid,
    custodian_id uuid,
    day_at timestamptz NOT NULL,
    rejected bigint NOT NULL DEFAULT 0,
    drift bigint NOT NULL DEFAULT 0,
    CONSTRAINT webhook_health_daily_psp_id_custodian_id_day_at_key UNIQUE NULLS NOT DISTINCT (merchant_id, psp_id, custodian_id, day_at),
    CONSTRAINT webhook_health_daily_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_health_daily_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT webhook_health_daily_source_check CHECK ((psp_id IS NULL) <> (custodian_id IS NULL))
);
COMMENT ON TABLE billing.webhook_health_daily IS 'UTC-day webhook counter buckets per event source, backing the webhook_rejects and webhook_drift_events windowed metrics. Retention: rows are deleted 13 months (397 days) after day_at.';

CREATE INDEX webhook_health_daily_day_at_idx ON billing.webhook_health_daily (merchant_id, day_at);

-- ---------------------------------------------------------------------------
-- Payment attempts and mandates
-- ---------------------------------------------------------------------------

-- mandates_storing_attempt_id_fkey references payment_attempts and is added after it.
CREATE TABLE billing.mandates (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    payment_method_id uuid,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('recurring', 'unscheduled', 'card_on_file')),
    subscription_id uuid,
    currency text CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    status text NOT NULL,
    end_reason text,
    ended_at timestamptz,
    card_brand text CHECK (card_brand <> ''),
    initial_transaction_id text CHECK (initial_transaction_id <> ''),
    network_transaction_id text CHECK (network_transaction_id <> ''),
    transaction_link_id text CHECK (length(transaction_link_id) = 22),
    storing_attempt_id uuid,
    accepted_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mandates_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT mandates_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id),
    CONSTRAINT mandates_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT mandates_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id),
    CONSTRAINT mandates_live_method_check CHECK (status IN ('revoked', 'ended') OR payment_method_id IS NOT NULL),
    CONSTRAINT mandates_scope_check CHECK ((kind = 'recurring') = (subscription_id IS NOT NULL) AND (kind = 'unscheduled') = (currency IS NOT NULL)),
    CONSTRAINT mandates_end_check CHECK ((status IN ('revoked', 'ended')) = (end_reason IS NOT NULL) AND (end_reason IS NULL) = (ended_at IS NULL)),
    CONSTRAINT mandates_end_reason_check CHECK (
        end_reason IN ('replaced', 'brand_changed', 'closed', 'customer_revoked', 'subscription_ended', 'payment_method_removed')
        AND (status = 'revoked') = (end_reason = 'customer_revoked')
    ),
    CHECK (status IN ('active', 'requires_reconsent', 'revoked', 'ended'))
);
COMMENT ON TABLE billing.mandates IS 'A customer''s consent to stored-credential use of one saved card, and the network lineage its storing transaction established: recurring for one subscription, unscheduled for collection in one currency, card_on_file for one-click reuse. Merchant-initiated charges run only under an active mandate and send its references. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mandates.payment_method_id IS 'The card the agreement is on. NULL only after an ended or revoked mandate''s card was deleted.';
COMMENT ON COLUMN billing.mandates.psp_id IS 'The gateway account its provider references belong to; a charge through another account never cites them.';
COMMENT ON COLUMN billing.mandates.kind IS 'recurring (one subscription), unscheduled (collection in one currency) or card_on_file (reuse for one-click buys).';
COMMENT ON COLUMN billing.mandates.subscription_id IS 'The subscription a recurring mandate covers.';
COMMENT ON COLUMN billing.mandates.currency IS 'The collection currency an unscheduled mandate covers.';
COMMENT ON COLUMN billing.mandates.status IS 'active; requires_reconsent (merchant-initiated charges wait for fresh consent); revoked (the customer withdrew); ended.';
COMMENT ON COLUMN billing.mandates.end_reason IS 'Why a revoked or ended mandate stopped: replaced, brand_changed, closed, customer_revoked, subscription_ended or payment_method_removed.';
COMMENT ON COLUMN billing.mandates.card_brand IS 'The card brand the agreement was established on.';
COMMENT ON COLUMN billing.mandates.initial_transaction_id IS 'The provider''s id of the storing transaction (NMI transactionid, Stripe pi_ or seti_). NULL until one is approved; written once.';
COMMENT ON COLUMN billing.mandates.network_transaction_id IS 'The scheme transaction id (Visa TID, Mastercard Trace ID) of the storing transaction, where the provider returns it; written once.';
COMMENT ON COLUMN billing.mandates.transaction_link_id IS 'The Mastercard Transaction Link ID of the storing transaction, where the provider returns it; written once.';
COMMENT ON COLUMN billing.mandates.storing_attempt_id IS 'The payment attempt of the storing transaction, while it is retained.';
COMMENT ON COLUMN billing.mandates.accepted_at IS 'When the customer gave the consent.';

CREATE UNIQUE INDEX mandates_card_on_file_live_key ON billing.mandates (merchant_id, payment_method_id, psp_id)
    WHERE kind = 'card_on_file' AND status IN ('active', 'requires_reconsent');
CREATE INDEX mandates_customer_id_created_at_id_idx ON billing.mandates (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX mandates_payment_method_id_idx ON billing.mandates (merchant_id, payment_method_id, psp_id)
    WHERE payment_method_id IS NOT NULL;
CREATE UNIQUE INDEX mandates_recurring_live_key ON billing.mandates (merchant_id, subscription_id)
    WHERE kind = 'recurring' AND status IN ('active', 'requires_reconsent');
CREATE UNIQUE INDEX mandates_unscheduled_live_key ON billing.mandates (merchant_id, customer_id, currency)
    WHERE kind = 'unscheduled' AND status IN ('active', 'requires_reconsent');

CREATE FUNCTION billing.guard_mandate_update() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['payment_method_id', 'status', 'end_reason', 'ended_at', 'initial_transaction_id', 'network_transaction_id', 'transaction_link_id', 'storing_attempt_id', 'updated_at'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['payment_method_id', 'status', 'end_reason', 'ended_at', 'initial_transaction_id', 'network_transaction_id', 'transaction_link_id', 'storing_attempt_id', 'updated_at'])
       OR (OLD.payment_method_id IS NOT NULL AND NEW.payment_method_id IS NOT NULL AND NEW.payment_method_id <> OLD.payment_method_id)
       OR (OLD.initial_transaction_id IS NOT NULL AND NEW.initial_transaction_id IS DISTINCT FROM OLD.initial_transaction_id)
       OR (OLD.network_transaction_id IS NOT NULL AND NEW.network_transaction_id IS DISTINCT FROM OLD.network_transaction_id)
       OR (OLD.transaction_link_id IS NOT NULL AND NEW.transaction_link_id IS DISTINCT FROM OLD.transaction_link_id)
       OR (OLD.status IN ('revoked', 'ended') AND (NEW.status, NEW.end_reason, NEW.ended_at) IS DISTINCT FROM (OLD.status, OLD.end_reason, OLD.ended_at)) THEN
        RAISE EXCEPTION 'mandate % changes what it recorded', OLD.id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER guard_mandate_update BEFORE UPDATE ON billing.mandates
    FOR EACH ROW EXECUTE FUNCTION billing.guard_mandate_update();

CREATE TABLE billing.payment_attempts (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    kind text NOT NULL,
    owner text NOT NULL CHECK (owner IN ('engine', 'nmi_schedule', 'provider', 'none')),
    card_entry text NOT NULL CHECK (card_entry IN ('new', 'saved')),
    source text NOT NULL CHECK (source IN ('openrails', 'provider_schedule', 'external')),
    observed_via text NOT NULL CHECK (observed_via IN ('response', 'webhook', 'pull')),
    category text NOT NULL,
    reason text,
    action text,
    response_code text,
    response_text text,
    transaction_id text,
    avs_result text,
    cvv_result text,
    card_brand text,
    card_last4 text,
    token_type text,
    amount bigint NOT NULL CHECK (amount >= 0),
    currency text,
    attempted_at timestamptz NOT NULL,
    purchase_id uuid,
    purchase_target text,
    subscription_id uuid,
    payment_method_id uuid,
    payment_id uuid,
    provider_intent_id uuid,
    step text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    cycle_id uuid,
    card_bin text CHECK (card_bin ~ '^[0-9]{6,8}$'),
    issuer_code text,
    issuer_text text,
    mandate_id uuid,
    sent_initial_transaction_id text CHECK (sent_initial_transaction_id <> ''),
    order_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT payment_attempts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT payment_attempts_cycle_id_fkey FOREIGN KEY (merchant_id, cycle_id) REFERENCES billing.rebill_cycles (merchant_id, id),
    CONSTRAINT payment_attempts_mandate_id_fkey FOREIGN KEY (merchant_id, mandate_id) REFERENCES billing.mandates (merchant_id, id) ON DELETE SET NULL (mandate_id),
    CONSTRAINT payment_attempts_order_id_fkey FOREIGN KEY (merchant_id, order_id) REFERENCES billing.orders (merchant_id, id) ON DELETE SET NULL (order_id),
    CONSTRAINT payment_attempts_payment_id_fkey FOREIGN KEY (merchant_id, payment_id) REFERENCES billing.payments (merchant_id, id) ON DELETE SET NULL (payment_id),
    CONSTRAINT payment_attempts_payment_method_id_fkey FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, id) ON DELETE SET NULL (payment_method_id),
    CONSTRAINT payment_attempts_provider_intent_id_fkey FOREIGN KEY (merchant_id, provider_intent_id) REFERENCES billing.provider_intents (merchant_id, id) ON DELETE SET NULL (provider_intent_id),
    CONSTRAINT payment_attempts_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT payment_attempts_cycle_check CHECK ((kind IN ('rebill', 'dunning_retry', 'customer_retry')) = (cycle_id IS NOT NULL)),
    CHECK (kind IN ('verify', 'initial', 'upgrade', 'rebill', 'dunning_retry', 'customer_retry', 'collection')),
    CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT payment_attempts_outcome_check CHECK ((category = 'approved') = (reason IS NULL AND action IS NULL)),
    CHECK (action IS NULL OR action IN ('retry', 'fix_payment_method', 'non_recoverable')),
    CONSTRAINT payment_attempts_text_check CHECK (length(response_text) <= 128),
    CONSTRAINT payment_attempts_last4_check CHECK (card_last4 ~ '^[0-9]{4}$'),
    CHECK (token_type IN ('network_token', 'pan_via_proxy', 'psp_token')),
    CONSTRAINT payment_attempts_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$' OR (currency IS NULL AND amount = 0)),
    CONSTRAINT payment_attempts_purchase_check CHECK ((purchase_id IS NULL) = (purchase_target IS NULL)),
    CONSTRAINT payment_attempts_issuer_check CHECK (length(issuer_code) <= 32 AND length(issuer_text) <= 128)
);
COMMENT ON TABLE billing.payment_attempts IS 'One row per authorization answered by a PSP: the $0 card verification, sales, rebills and retries. Never the PAN or CVV. purchase_id groups one buyer''s attempts on one target (purchase_target: a price id or card_save) until the target is approved. Retention: rows are deleted 25 months (761 days) after attempted_at.';
COMMENT ON COLUMN billing.payment_attempts.subscription_id IS 'The subscription the attempt renews or would create. No foreign key: a declined first charge names a subscription that is never written.';
COMMENT ON COLUMN billing.payment_attempts.issuer_code IS 'The issuer''s raw answer (NMI processor_response_code); response_code is the gateway''s.';
COMMENT ON COLUMN billing.payment_attempts.mandate_id IS 'The mandate whose references the attempt sent: a merchant-initiated charge, or a customer-present one on a stored credential.';
COMMENT ON COLUMN billing.payment_attempts.sent_initial_transaction_id IS 'The initial transaction id the attempt sent the provider, verbatim.';

CREATE INDEX payment_attempts_attempted_at_time_idx ON billing.payment_attempts (merchant_id, attempted_at);
CREATE INDEX payment_attempts_customer_id_purchase_target_attempted_at_idx ON billing.payment_attempts (merchant_id, customer_id, purchase_target, attempted_at)
    WHERE purchase_target IS NOT NULL;
CREATE INDEX payment_attempts_cycle_id_idx ON billing.payment_attempts (merchant_id, cycle_id)
    WHERE cycle_id IS NOT NULL;
CREATE INDEX payment_attempts_order_id_attempted_at_idx ON billing.payment_attempts (merchant_id, order_id, attempted_at)
    WHERE order_id IS NOT NULL;
CREATE INDEX payment_attempts_payment_id_idx ON billing.payment_attempts (merchant_id, payment_id)
    WHERE payment_id IS NOT NULL;
CREATE INDEX payment_attempts_payment_method_id_idx ON billing.payment_attempts (merchant_id, payment_method_id)
    WHERE payment_method_id IS NOT NULL;
CREATE UNIQUE INDEX payment_attempts_provider_intent_id_step_key ON billing.payment_attempts (merchant_id, provider_intent_id, step)
    WHERE provider_intent_id IS NOT NULL;
CREATE UNIQUE INDEX payment_attempts_psp_id_transaction_id_key ON billing.payment_attempts (merchant_id, psp_id, transaction_id)
    WHERE transaction_id IS NOT NULL;
CREATE INDEX payment_attempts_purchase_id_attempted_at_idx ON billing.payment_attempts (merchant_id, purchase_id, attempted_at)
    WHERE purchase_id IS NOT NULL;
CREATE INDEX payment_attempts_subscription_id_attempted_at_idx ON billing.payment_attempts (merchant_id, subscription_id, attempted_at)
    WHERE subscription_id IS NOT NULL;

ALTER TABLE billing.mandates ADD CONSTRAINT mandates_storing_attempt_id_fkey
    FOREIGN KEY (merchant_id, storing_attempt_id) REFERENCES billing.payment_attempts (merchant_id, id) ON DELETE SET NULL (storing_attempt_id);

-- ---------------------------------------------------------------------------
-- PSP imports and history
-- ---------------------------------------------------------------------------

CREATE TABLE billing.psp_imports (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    status text NOT NULL,
    history_since timestamptz,
    history_through timestamptz,
    pulled_at timestamptz,
    applied_at timestamptz,
    error text,
    rules jsonb NOT NULL CHECK (jsonb_typeof(rules) = 'array'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    error_code text,
    apply_requested_by text CHECK (apply_requested_by IS NULL OR apply_requested_by <> ''),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT psp_imports_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT psp_imports_error_check CHECK ((status = 'failed') = (error IS NOT NULL) AND (error IS NULL OR error <> '')),
    CONSTRAINT psp_imports_error_code_check CHECK (
        error_code IS NULL
        OR (status = 'failed' AND error_code IN ('psp_credentials_rejected', 'psp_not_readable', 'pull_roster_implausible'))
    ),
    CHECK (status IN ('pulling', 'pulled', 'applying', 'applied', 'failed'))
);
COMMENT ON TABLE billing.psp_imports IS 'One read-only pull of a PSP''s catalog, customers, subscriptions and transaction history, and the merchant''s rules for linking them to customers. history_through is how far the transaction history has been read. Deleting the import deletes its records.';
COMMENT ON COLUMN billing.psp_imports.error_code IS 'Why a failed import stopped when pulling it again would stop the same way: psp_credentials_rejected (the provider refused the credentials), psp_not_readable (the PSP, its rail''s import or a credential is not declared) or pull_roster_implausible (the NMI roster listed under a tenth of the PSP''s live subscriptions).';
COMMENT ON COLUMN billing.psp_imports.apply_requested_by IS 'The person who last asked to apply the import; its corrections are approved in their name.';

CREATE INDEX psp_imports_created_at_id_idx ON billing.psp_imports (merchant_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX psp_imports_in_flight_key ON billing.psp_imports (merchant_id, psp_id)
    WHERE status IN ('pulling', 'applying');

CREATE TABLE billing.psp_import_mappings (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    mapping text NOT NULL CHECK (mapping ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    key text NOT NULL CHECK (key <> '' AND octet_length(key) <= 256),
    customer_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, mapping, key)
);
COMMENT ON TABLE billing.psp_import_mappings IS 'A merchant''s own identifiers mapped to its customers (a legacy user id, a legacy order id), named by mapping. PSP import rules read a provider fact through one.';

CREATE INDEX psp_import_mappings_customer_idx ON billing.psp_import_mappings (merchant_id, customer_id);

CREATE TABLE billing.psp_import_records (
    merchant_id uuid NOT NULL,
    import_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    kind text NOT NULL CHECK (kind IN ('customer', 'subscription', 'transaction')),
    ref text NOT NULL CHECK (ref <> '' AND octet_length(ref) <= 256),
    customer_ref text,
    subscription_ref text,
    occurred_at timestamptz,
    facts jsonb NOT NULL CHECK (jsonb_typeof(facts) = 'object'),
    data jsonb NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    state text NOT NULL,
    customer_id uuid,
    linked_via text,
    manual_customer_id uuid,
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    manual_price_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT psp_import_records_ref_key UNIQUE (merchant_id, import_id, kind, ref),
    CONSTRAINT psp_import_records_import_id_fkey FOREIGN KEY (merchant_id, import_id) REFERENCES billing.psp_imports (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT psp_import_records_manual_price_id_fkey FOREIGN KEY (merchant_id, manual_price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT psp_import_records_manual_price_check CHECK (manual_price_id IS NULL OR kind = 'subscription'),
    CONSTRAINT psp_import_records_refs_check CHECK ((customer_ref IS NULL OR customer_ref <> '') AND (subscription_ref IS NULL OR subscription_ref <> '')),
    CONSTRAINT psp_import_records_reason_check CHECK ((reason IS NULL OR reason <> '') AND (state NOT IN ('conflict', 'blocked') OR reason IS NOT NULL)),
    CONSTRAINT psp_import_records_state_check CHECK (
        (state IN ('unmatched', 'conflict') AND customer_id IS NULL AND linked_via IS NULL)
        OR (state IN ('linked', 'imported', 'blocked') AND customer_id IS NOT NULL AND linked_via IS NOT NULL AND linked_via <> '')
        OR (state = 'held' AND customer_id IS NOT NULL AND linked_via = 'held')
    )
);
COMMENT ON TABLE billing.psp_import_records IS 'A provider object a PSP import staged: a customer (vault) with its cards, a subscription or a transaction, with the identity facts the provider holds (email, username, order id, vault ids). linked names the customer a rule, a mapping, the object it belongs to or the merchant linked it to; imported once the billing import landed it; blocked says why it did not land. Deleted with its import.';
COMMENT ON COLUMN billing.psp_import_records.manual_customer_id IS 'The customer the merchant linked this object to by hand; it wins over every rule.';
COMMENT ON COLUMN billing.psp_import_records.manual_price_id IS 'The provider price the merchant picked by hand for a subscription whose provider names no plan (CCBill); it wins over matching its rebill.';

CREATE INDEX psp_import_records_kind_idx ON billing.psp_import_records (merchant_id, import_id, kind, id);
CREATE INDEX psp_import_records_state_idx ON billing.psp_import_records (merchant_id, import_id, state, kind, id);

CREATE TABLE billing.psp_import_basis (
    merchant_id uuid NOT NULL,
    import_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    row_version bigint NOT NULL,
    PRIMARY KEY (merchant_id, import_id, subscription_id),
    CONSTRAINT psp_import_basis_import_id_fkey FOREIGN KEY (merchant_id, import_id) REFERENCES billing.psp_imports (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT psp_import_basis_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.psp_import_basis IS 'Each subscription of the PSP an import found before it read the roster, at its row version: the import proposes a state change only for a row still at that version. Deleted with its import.';

CREATE INDEX psp_import_basis_subscription_id_idx ON billing.psp_import_basis (merchant_id, subscription_id);

CREATE TABLE billing.parked_provider_subscriptions (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    subscription_ref text NOT NULL,
    reason text NOT NULL,
    price_ref text,
    customer_ref text,
    import_owned boolean NOT NULL,
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'object'),
    parked_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    released_at timestamptz,
    released_by text,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT parked_provider_subscriptions_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT parked_provider_subscriptions_ref_check CHECK (subscription_ref <> '' AND octet_length(subscription_ref) <= 256),
    CONSTRAINT parked_provider_subscriptions_cause_check CHECK (
        (price_ref IS NULL OR price_ref <> '')
        AND (customer_ref IS NULL OR customer_ref <> '')
        AND (reason <> 'price_unmapped' OR price_ref IS NOT NULL)
    ),
    CHECK (reason IN ('price_unmapped', 'customer_unlinked', 'product_held')),
    CONSTRAINT parked_provider_subscriptions_released_check CHECK (
        (released_at IS NULL) = (released_by IS NULL)
        AND (released_by IS NULL OR released_by IN ('landed', 'converged', 'gone'))
    )
);
COMMENT ON TABLE billing.parked_provider_subscriptions IS 'Live provider subscriptions converge cannot land: its price is unmapped, its customer is linked to no one, or the customer already holds the product. One open park per (PSP, subscription); it mirrors the latest converge. Fixing the cause wakes converge; the park closes when the subscription lands by any path (released_by landed), when converge no longer needs it (converged) or when the provider ended it unlanded (gone). Retention: released parks are deleted 90 days after released_at.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.price_ref IS 'The provider price or plan the subscription bills; set on every park whose provider names one.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.customer_ref IS 'The provider customer the subscription bills: a Stripe customer, an NMI vault.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.import_owned IS 'The PSP''s import owns the subscription (no pull yet, a pull pending, or staged unlanded): the park raises no finding, and the import lands it on Apply.';

CREATE INDEX parked_provider_subscriptions_customer_ref_idx ON billing.parked_provider_subscriptions (merchant_id, psp_id, reason, customer_ref)
    WHERE released_at IS NULL;
CREATE UNIQUE INDEX parked_provider_subscriptions_open_key ON billing.parked_provider_subscriptions (merchant_id, psp_id, subscription_ref)
    WHERE released_at IS NULL;
CREATE INDEX parked_provider_subscriptions_parked_at_id_idx ON billing.parked_provider_subscriptions (merchant_id, parked_at DESC, id DESC);
CREATE INDEX parked_provider_subscriptions_price_ref_idx ON billing.parked_provider_subscriptions (merchant_id, psp_id, reason, price_ref)
    WHERE released_at IS NULL;
CREATE INDEX parked_provider_subscriptions_released_at_idx ON billing.parked_provider_subscriptions (merchant_id, released_at)
    WHERE released_at IS NOT NULL;

-- A bulk verification read (roster and transactions by date range) resumes from
-- its last completed transaction page after a crash.
CREATE TABLE billing.nmi_bulk_checkpoints (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    window_starts_at timestamptz NOT NULL,
    window_ends_at timestamptz NOT NULL,
    next_page integer NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_bulk_checkpoints_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.nmi_bulk_checkpoints IS 'The in-progress bulk verification read per NMI account: its transaction window and the next page to read. Deleted when the pass completes.';

CREATE TABLE billing.nmi_history_months (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    month_at timestamptz NOT NULL CHECK (month_at = date_trunc('month', month_at, 'UTC')),
    kind text NOT NULL CHECK (kind IN ('verification', 'one_off_sale', 'scheduled_rebill')),
    category text NOT NULL,
    reason text NOT NULL,
    authorizations bigint NOT NULL CHECK (authorizations > 0),
    PRIMARY KEY (merchant_id, psp_id, month_at, kind, category, reason),
    CONSTRAINT nmi_history_months_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT nmi_history_months_reason_check CHECK ((category = 'approved') = (reason = ''))
);
COMMENT ON TABLE billing.nmi_history_months IS 'Authorizations NMI answered per PSP, month_at (the month''s first instant, UTC), kind (verification, one_off_sale, scheduled_rebill) and outcome: category approved, or a refusal''s category and reason from the one classifier. A read replaces every month it covers. Retention: rows are deleted 25 months (761 days) after their month.';

CREATE INDEX nmi_history_months_month_at_idx ON billing.nmi_history_months (merchant_id, month_at);

-- One row per NMI PSP whose history was read: the last read that completed.
CREATE TABLE billing.nmi_history_reads (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    read_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_history_reads_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.nmi_history_reads IS 'When each NMI PSP''s history was last read in full. A PSP with none is backfilled 25 months; later reads start the month before this one.';

-- ---------------------------------------------------------------------------
-- Usage, balances and admission
-- ---------------------------------------------------------------------------

CREATE TABLE billing.usage_events (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    invoker_id text NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    resource text,
    event_type text NOT NULL,
    dimensions jsonb NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    source text NOT NULL,
    source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 255),
    ledger_transfer_id uuid,
    metadata jsonb,
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'failed')),
    forgiven_amount bigint NOT NULL,
    charges jsonb NOT NULL CHECK (jsonb_typeof(charges) = 'array'),
    stated_amount bigint CHECK (stated_amount >= 0),
    PRIMARY KEY (merchant_id, id, occurred_at),
    CONSTRAINT usage_events_customer_id_currency_ledger_transfer_id_fkey FOREIGN KEY (merchant_id, customer_id, currency, ledger_transfer_id) REFERENCES billing.ledger_transfers (merchant_id, customer_id, currency, id),
    CONSTRAINT usage_events_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT usage_events_forgiven_amount_check CHECK (forgiven_amount >= 0 AND (outcome = 'failed' OR forgiven_amount = 0))
) PARTITION BY RANGE (occurred_at);
COMMENT ON TABLE billing.usage_events IS 'Append-only multi-dimensional metered usage. Source of truth for usage reporting and usage order lines. Priced when recorded, by the host (amount) or the rate cards (charges); event and ledger postings commit in one tx. The hot admission path never reads this table. Retention: monthly partitions on occurred_at, dropped 24 months after the month''s usage was billed.';
COMMENT ON COLUMN billing.usage_events.invoker_id IS 'Caller-supplied principal string that fired this metered usage event. Opaque to OpenRails; attribution + grouping only, not a FK. Joins use source/source_id.';
COMMENT ON COLUMN billing.usage_events.currency IS 'Native OpenRails currency code; amount uses this currency internal precision.';
COMMENT ON COLUMN billing.usage_events.resource IS 'Caller-supplied free-form string for what was metered (for example, an endpoint or plan slug). Opaque to OpenRails; nullable, not a FK.';
COMMENT ON COLUMN billing.usage_events.occurred_at IS 'When the usage happened; the partition key. Accepted only within the ingest window, so every read and the idempotency lookup name a time range.';
COMMENT ON COLUMN billing.usage_events.outcome IS 'succeeded, or failed: work that cost the platform and did not deliver. A failed event charges only what the customer''s grace leaves.';
COMMENT ON COLUMN billing.usage_events.forgiven_amount IS 'The part of a failed event''s reported cost its grace absorbed; amount + forgiven_amount is what the host reported. Zero for a succeeded event.';
COMMENT ON COLUMN billing.usage_events.charges IS 'What each meter charged the event: [{meter_key, model, cell, quantity, amount}]. Empty when no meter consumes it or the usage failed.';
COMMENT ON COLUMN billing.usage_events.stated_amount IS 'Optional host cost in native currency units. Dynamic meter cards charge it; other cards ignore it. Failed events must state their cost.';

CREATE INDEX usage_events_occurred_at_idx ON billing.usage_events (merchant_id, occurred_at);
CREATE INDEX usage_events_event_type_occurred_at_idx ON billing.usage_events (merchant_id, event_type, occurred_at);
CREATE INDEX usage_events_customer_id_occurred_at_idx ON billing.usage_events (merchant_id, customer_id, occurred_at);
-- A partitioned unique index must carry the partition key, so this one stops
-- only an exact repeat. The idempotency coordinate is claimed under the
-- customer spend lock by a lookup over the ingest window.
CREATE UNIQUE INDEX usage_events_idem_key ON billing.usage_events (merchant_id, customer_id, currency, event_type, source, source_id, occurred_at);
CREATE INDEX usage_events_source_occurred_at_id_idx ON billing.usage_events (merchant_id, source, source_id, occurred_at DESC, id DESC);

-- The migrator and the cleanup job keep the partitions current; this lets the
-- table take rows as soon as it exists.
SELECT billing.ensure_month_partitions('usage_events', now() - interval '35 days', now() + interval '2 months');

-- Holds for every new row; rows that predate it are not checked.
ALTER TABLE billing.usage_events ADD CONSTRAINT usage_events_failed_stated_amount_check
    CHECK (outcome = 'succeeded' OR stated_amount IS NOT NULL) NOT VALID;

CREATE TABLE billing.meter_period_quantities (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    meter_key text NOT NULL CHECK (meter_key <> ''),
    cell text NOT NULL,
    period_starts_at timestamptz NOT NULL,
    quantity bigint NOT NULL CHECK (quantity >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, currency, meter_key, cell, period_starts_at),
    CONSTRAINT meter_period_quantities_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.meter_period_quantities IS 'A customer''s quantity of one meter (and matrix cell) in one billing period, as priced so far: an event is charged price(quantity + its own) - price(quantity), then added. Retention: rows are deleted 100 days after period_starts_at.';
COMMENT ON COLUMN billing.meter_period_quantities.cell IS 'The matrix cell (the event''s value of the matrix dimension) the quantity is priced in; empty for a card without a matrix.';
COMMENT ON COLUMN billing.meter_period_quantities.period_starts_at IS 'Start of the billing period the usage occurred in, under the merchant''s billing_period_boundary.';

CREATE INDEX meter_period_quantities_period_starts_at_idx ON billing.meter_period_quantities (merchant_id, period_starts_at);

CREATE TABLE billing.failed_usage_windows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    invoker text NOT NULL CHECK (octet_length(invoker) <= 255),
    window_key text NOT NULL CHECK (window_key <> '' AND octet_length(window_key) <= 255),
    window_start timestamptz NOT NULL,
    window_end timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount > 0),
    PRIMARY KEY (merchant_id, customer_id, currency, invoker, window_key, window_start),
    CONSTRAINT failed_usage_windows_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT failed_usage_windows_window_check CHECK (window_end > window_start)
);
COMMENT ON TABLE billing.failed_usage_windows IS 'Failed usage counted per customer and configured window: the customer''s grace (invoker empty) or a delegated invoker''s cutoff. Retention: rows are deleted once their window has ended.';
COMMENT ON COLUMN billing.failed_usage_windows.invoker IS 'Empty for the customer''s own grace window; otherwise the delegated invoker the cutoff window meters.';

CREATE INDEX failed_usage_windows_window_end_idx ON billing.failed_usage_windows (merchant_id, window_end);

CREATE TABLE billing.running_charges (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    reference text NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    rate_per_hour bigint NOT NULL,
    overdraft_amount bigint NOT NULL CHECK (overdraft_amount >= 0),
    resource text CHECK (resource <> '' AND octet_length(resource) <= 255),
    started_at timestamptz NOT NULL,
    stopped_at timestamptz,
    posted_through_at timestamptz NOT NULL,
    posted_amount bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT running_charges_reference_key UNIQUE (merchant_id, reference),
    CONSTRAINT running_charges_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE RESTRICT,
    CHECK (reference <> '' AND reference = btrim(reference) AND octet_length(reference) <= 255),
    CONSTRAINT running_charges_posted_check CHECK (
        posted_through_at >= started_at
        AND posted_amount = billing.running_charge_total(rate_per_hour, started_at, posted_through_at)
    ),
    CHECK (rate_per_hour > 0 AND rate_per_hour <= '1000000000000'::bigint),
    CONSTRAINT running_charges_stopped_at_check CHECK (stopped_at IS NULL OR stopped_at = posted_through_at)
);
COMMENT ON TABLE billing.running_charges IS 'One row per running charge: a customer charged rate_per_hour (native units of currency) from started_at until stopped_at. No row changes while it runs; a posting moves what accrued through posted_through_at to the ledger, and a stopped charge is posted through stopped_at. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.running_charges.reference IS 'The host''s name for what it charges for: unique per merchant, and the identity a repeated start or stop is matched on.';
COMMENT ON COLUMN billing.running_charges.overdraft_amount IS 'How far below zero this charge lets a prepaid customer''s capacity reach; the smallest among a customer''s running charges applies.';
COMMENT ON COLUMN billing.running_charges.posted_amount IS 'What the ledger has been posted: running_charge_total through posted_through_at.';

CREATE INDEX running_charges_customer_id_currency_idx ON billing.running_charges (merchant_id, customer_id, currency)
    WHERE stopped_at IS NULL;

CREATE TRIGGER immutable_running_charge_facts BEFORE DELETE OR UPDATE ON billing.running_charges
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('stopped_at', 'posted_through_at', 'posted_amount', 'updated_at');

CREATE TABLE billing.balance_watches (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    next_check_at timestamptz,
    depletes_at timestamptz,
    low_notified_at timestamptz,
    depleted_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    funded_at timestamptz,
    usage_rate_per_hour bigint NOT NULL CHECK (usage_rate_per_hour >= 0),
    rate_sampled_at timestamptz,
    low_line_amount bigint,
    floor_amount bigint,
    look_each_change boolean NOT NULL DEFAULT true,
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT balance_watches_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.balance_watches IS 'One row per prepaid customer balance and per customer and currency with a running charge: when its balance is next looked at (a low line, the run-out, a credit lot lapse or the next 00:00 UTC, whichever is first) and what the last look found, including the lines a change is judged against. Its look job is queued by the change that crosses a line and for next_check_at; the billing pass looks at one whose job failed. Derived from the balance, the running charges and the billing mode; an arrears customer with no running charge has none.';
COMMENT ON COLUMN billing.balance_watches.next_check_at IS 'When the balance is next looked at; to_timestamp(0) once a change marks it due (updated_at is then when), null with nothing scheduled.';
COMMENT ON COLUMN billing.balance_watches.depletes_at IS 'When the customer''s capacity reaches zero at the rate of its running charges and the lapse of its credit lots, as last computed; in the past while it is at zero, null when nothing exact moves it.';
COMMENT ON COLUMN billing.balance_watches.low_notified_at IS 'When balance.low was sent for the current crossing; cleared by a look that finds the balance above its line after a funding.';
COMMENT ON COLUMN billing.balance_watches.depleted_at IS 'When balance.depleted was sent for the current crossing; cleared by a look that finds capacity above zero after a funding.';
COMMENT ON COLUMN billing.balance_watches.funded_at IS 'When the customer was funded (a deposit, a credit grant, a reinstatement or a payment of what it owes) since its last notice; only funding re-arms a notice, and a notice clears it.';
COMMENT ON COLUMN billing.balance_watches.usage_rate_per_hour IS 'Exponentially weighted rate of usage spend, one-hour time constant, in native units per hour: each look adds the usage events dated since rate_sampled_at.';
COMMENT ON COLUMN billing.balance_watches.rate_sampled_at IS 'The instant usage_rate_per_hour was sampled at; usage events dated before it are not read again.';
COMMENT ON COLUMN billing.balance_watches.low_line_amount IS 'The available amount (balance less held) at or under which the balance is low, as the last look found the line; null with no line.';
COMMENT ON COLUMN billing.balance_watches.floor_amount IS 'The available amount at or under which the balance is depleted, as the last look found it; null before a look.';
COMMENT ON COLUMN billing.balance_watches.look_each_change IS 'Every change is looked at: a running charge or a lot that lapses moves the run-out, or no look has stored the lines yet.';

CREATE INDEX balance_watches_depletes_at_idx ON billing.balance_watches (merchant_id, depletes_at);
CREATE INDEX balance_watches_next_check_at_idx ON billing.balance_watches (next_check_at, merchant_id, customer_id, currency)
    WHERE next_check_at IS NOT NULL;

CREATE TABLE billing.customer_low_balance_lines (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    amount bigint CHECK (amount >= 0),
    runway_hours integer CHECK (runway_hours BETWEEN 0 AND 8760),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_low_balance_lines_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_low_balance_lines_set_check CHECK (amount IS NOT NULL OR runway_hours IS NOT NULL)
);
COMMENT ON TABLE billing.customer_low_balance_lines IS 'A customer''s own low-balance line per currency: the amount line and the runway line (hours of spend at its burn rate). A null part inherits the merchant''s settings.balance_notices; a line with no part set has no row.';

CREATE TABLE billing.admission_operations (
    merchant_id uuid NOT NULL,
    request_id text NOT NULL CHECK (octet_length(request_id) BETWEEN 1 AND 255),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    estimated_amount bigint NOT NULL CHECK (estimated_amount >= 0),
    available_amount bigint NOT NULL CHECK (available_amount >= 0),
    terms jsonb NOT NULL,
    requested_expires_at timestamptz,
    expires_at timestamptz,
    admitted_at timestamptz NOT NULL DEFAULT now(),
    window_keys text[] NOT NULL,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'released', 'captured')),
    capture_terms jsonb,
    captured_amount bigint,
    captured_at timestamptz,
    released_at timestamptz,
    estimate_usage jsonb,
    rate_cards jsonb,
    PRIMARY KEY (merchant_id, request_id, admitted_at),
    CONSTRAINT admission_operations_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT admission_operations_requested_expires_at_check CHECK (estimated_amount = 0 OR (requested_expires_at IS NOT NULL AND requested_expires_at > admitted_at)),
    CHECK (jsonb_typeof(terms) = 'object' AND octet_length(terms::text) <= 65536),
    CONSTRAINT admission_operations_expires_at_check CHECK (requested_expires_at IS NULL OR (expires_at IS NOT NULL AND expires_at >= requested_expires_at)),
    CONSTRAINT admission_operations_hold_lifetime_check CHECK (expires_at IS NULL OR expires_at <= (admitted_at + '720:00:00'::interval)),
    CONSTRAINT admission_operations_state_fields_check CHECK (
        (state = 'open' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NULL)
        OR (state = 'released' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NOT NULL)
        OR (state = 'captured' AND capture_terms IS NOT NULL AND captured_amount IS NOT NULL AND captured_amount >= 0 AND captured_at IS NOT NULL)
    ),
    CHECK (jsonb_typeof(capture_terms) = 'object' AND octet_length(capture_terms::text) <= 65536),
    CONSTRAINT admission_operations_estimate_usage_check CHECK (
        (estimate_usage IS NULL) = (rate_cards IS NULL)
        AND (estimate_usage IS NULL OR (jsonb_typeof(estimate_usage) = 'object' AND octet_length(estimate_usage::text) <= 65536))
        AND (rate_cards IS NULL OR (jsonb_typeof(rate_cards) = 'array' AND octet_length(rate_cards::text) <= 262144))
    )
) PARTITION BY RANGE (admitted_at);
COMMENT ON TABLE billing.admission_operations IS 'One row per admitted spend request: its estimated hold until the request is captured or released. Retention: monthly partitions on admitted_at, dropped once older than the longest spend window plus 30 days.';
COMMENT ON COLUMN billing.admission_operations.admitted_at IS 'When the request was admitted; the partition key. request_id is unique per merchant among retained admissions: admission serializes on it, because a partitioned key must carry admitted_at.';
COMMENT ON COLUMN billing.admission_operations.estimate_usage IS 'The estimated usage ({event_type, dimensions, metadata, amount}); dynamic cards charge its optional amount. estimated_amount is its price when admitted. Null for a host-stated hold.';
COMMENT ON COLUMN billing.admission_operations.rate_cards IS 'The rate cards that priced estimate_usage, pinned: the capture is priced by them, whatever the catalog says later.';

CREATE INDEX admission_operations_customer_id_currency_expires_at_idx ON billing.admission_operations (merchant_id, customer_id, currency, expires_at)
    WHERE state = 'open';
CREATE INDEX admission_operations_customer_id_currency_admitted_at_idx ON billing.admission_operations (merchant_id, customer_id, currency, admitted_at)
    WHERE state <> 'released';
CREATE INDEX admission_operations_admitted_at_request_id_idx ON billing.admission_operations (merchant_id, admitted_at DESC, request_id COLLATE "C" DESC);
CREATE INDEX admission_operations_customer_admitted_at_request_id_idx ON billing.admission_operations (merchant_id, customer_id, admitted_at DESC, request_id COLLATE "C" DESC);

CREATE TRIGGER immutable_admission_operation_facts BEFORE DELETE OR UPDATE ON billing.admission_operations
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('expires_at', 'state', 'capture_terms', 'captured_amount', 'captured_at', 'released_at');

-- The migrator and the cleanup job keep the partitions current; this lets the
-- table take rows as soon as it exists.
SELECT billing.ensure_month_partitions('admission_operations', now(), now() + interval '2 months');

CREATE TABLE billing.admission_denials_hourly (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    denial_reason text NOT NULL,
    hour_at timestamptz NOT NULL,
    denials bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, denial_reason, hour_at),
    CONSTRAINT admission_denials_hourly_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT admission_denials_hourly_hour_aligned_check CHECK (hour_at = date_trunc('hour', hour_at)),
    CONSTRAINT admission_denials_hourly_denials_positive_check CHECK (denials > 0)
);
COMMENT ON TABLE billing.admission_denials_hourly IS 'Hourly admission-denial aggregates (merchant x payer x reason), flushed from Redis counters once each hour closes; the hot path never writes PG per-request. Retention: rows are deleted 13 months (397 days) after hour_at.';

CREATE INDEX admission_denials_hourly_hour_at_idx ON billing.admission_denials_hourly (merchant_id, hour_at);

-- ---------------------------------------------------------------------------
-- Notifications and events
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.event_log_data(value jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE key text; prefix text;
BEGIN
    value := COALESCE(value,'{}'::jsonb);
    -- Keep monetary integers lossless for JavaScript consumers, just like the
    -- existing typed billing API. Do not reinterpret arbitrary payload fields.
    FOREACH key IN ARRAY ARRAY['amount','total','overdue_amount','amount_floor',
      'available_amount','owed_amount','accrual_rate_per_hour','usage_rate_per_hour',
      'low_balance_amount','old_amount','new_amount','amount_due'] LOOP
        IF jsonb_typeof(value->key)='number' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(value->>key));
        END IF;
    END LOOP;
    FOR key,prefix IN SELECT * FROM (VALUES
      ('payment_id','pay_'),('product_id','prod_'),('subscription_id','sub_'),
      ('order_id','ord_'),('mandate_id','mdt_'),('price_migration_id','pmig_'),
      ('from_product_id','prod_'),('to_product_id','prod_')) AS ids(key,prefix) LOOP
        IF value->>key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(prefix||(value->>key)));
        END IF;
    END LOOP;
    RETURN value;
END;
$_$;

CREATE FUNCTION billing.event_subject_id(kind text, id uuid) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT CASE WHEN id IS NULL THEN NULL ELSE
      CASE kind WHEN 'payment' THEN 'pay_' WHEN 'product' THEN 'prod_'
      WHEN 'subscription' THEN 'sub_' WHEN 'order' THEN 'ord_'
      WHEN 'mandate' THEN 'mdt_' WHEN 'destructive_hold' THEN 'dsh_' ELSE '' END || id::text END
$$;

CREATE TABLE billing.notification_events (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id),
    id uuid NOT NULL,
    customer_id uuid NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    inserted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT notification_events_merchant_id_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.notification_events IS 'Internal notification emission and deduplication journal; the public feed is event_log. Retention: rows are deleted after at least 90 days from inserted_at and never earlier than the configured event retention, independently of readers.';

CREATE INDEX notification_events_customer_idx ON billing.notification_events (merchant_id, customer_id, created_at, id);
CREATE INDEX notification_events_delivery_idx ON billing.notification_events (inserted_at, merchant_id, id);
CREATE INDEX notification_events_ended_idx ON billing.notification_events (merchant_id, customer_id, created_at)
    WHERE event_type = 'premium_ended';

CREATE FUNCTION billing.capture_notification_event_log() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    PERFORM billing.queue_event_log(billing.notification_event_record(NEW));
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.guard_notification_event() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW) THEN
        RAISE EXCEPTION 'notification events are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER notification_event_immutable BEFORE UPDATE ON billing.notification_events
    FOR EACH ROW EXECUTE FUNCTION billing.guard_notification_event();
CREATE TRIGGER notification_event_log_capture AFTER INSERT ON billing.notification_events
    FOR EACH ROW EXECUTE FUNCTION billing.capture_notification_event_log();

CREATE FUNCTION billing.notification_event_record(n billing.notification_events) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE source_merchant uuid:=n.merchant_id; account uuid:=n.customer_id; authority text;
BEGIN
    SELECT COALESCE(issuer,'') INTO authority FROM billing.customers
      WHERE merchant_id=source_merchant AND id=account;
    RETURN jsonb_build_object('merchant_id',n.merchant_id,'id','ntf_'||n.id,'source','notification',
      'type',n.event_type,'occurred_at',n.created_at,'subject_type','customer',
      'subject_id',n.customer_id::text,'data',billing.event_log_data(n.data||
        jsonb_build_object('customer_id',account,'account_id',account,'account_issuer',authority)));
END;
$$;

CREATE TABLE billing.notification_issue_revisions (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    issue_key text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    withdrawn boolean NOT NULL,
    source_event uuid,
    PRIMARY KEY (merchant_id, customer_id, issue_key),
    CONSTRAINT notification_issue_revisions_merchant_id_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.notification_issue_revisions IS 'Producer ordering for exact account-notification identities. No inbox payload, read state, event cursor, or delivery queue. Retained until customer deletion so delayed publication cannot revive resolved issues.';

CREATE TABLE billing.notification_channel_state (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE CASCADE,
    restore_required boolean NOT NULL DEFAULT false,
    PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.notification_channel_state IS 'Financial archive restore blocks inbox publication until explicit notification ordering-state adoption. No implicit revision reset.';

CREATE TABLE billing.host_outbox (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    event_type text NOT NULL,
    subject_type text NOT NULL,
    payment_id uuid,
    amount bigint,
    subject_id uuid NOT NULL,
    currency text,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    data jsonb NOT NULL DEFAULT '{}',
    dedupe_key text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT host_outbox_payment_id_fkey FOREIGN KEY (merchant_id, payment_id) REFERENCES billing.payments (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT host_outbox_currency_check CHECK (
        (currency IS NULL) = (event_type IN ('product.entitlements_changed', 'customer.network_linked', 'customer.network_unlinked', 'mandate.revoked', 'destructive.hold_opened', 'destructive.hold_cleared'))
        AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$')
    ),
    CONSTRAINT host_outbox_payload_check CHECK (
        (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
        OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared', 'balance.low', 'balance.depleted') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired', 'order.voided') AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
        OR (event_type IN ('customer.network_linked', 'customer.network_unlinked') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type = 'mandate.revoked' AND subject_type = 'mandate' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type IN ('destructive.hold_opened', 'destructive.hold_cleared') AND subject_type = 'destructive_hold' AND payment_id IS NULL AND amount IS NULL AND currency IS NULL AND jsonb_typeof(data) = 'object')
    )
);
COMMENT ON TABLE billing.host_outbox IS 'Internal typed financial emission and deduplication journal; the public feed is event_log. Retention: rows are deleted after at least 90 days from recorded_at and never earlier than the configured event retention, independently of readers.';
COMMENT ON COLUMN billing.host_outbox.currency IS 'The transition''s currency; NULL only for a product''s key change, which has none.';
COMMENT ON COLUMN billing.host_outbox.dedupe_key IS 'Deterministic per transition (delinquency:<customer>:<currency>:<transition_seq>) so a re-run collapses instead of instructing a second shutoff.';

CREATE UNIQUE INDEX host_outbox_dedupe_key_key ON billing.host_outbox (merchant_id, dedupe_key);
CREATE UNIQUE INDEX host_outbox_payment_id_key ON billing.host_outbox (merchant_id, payment_id)
    WHERE payment_id IS NOT NULL;
CREATE INDEX host_outbox_retention_idx ON billing.host_outbox (merchant_id, recorded_at, id);

CREATE FUNCTION billing.capture_host_event_log() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    PERFORM billing.queue_event_log(billing.host_event_record(NEW));
    RETURN NEW;
END;
$$;

CREATE TRIGGER host_event_log_capture AFTER INSERT ON billing.host_outbox
    FOR EACH ROW EXECUTE FUNCTION billing.capture_host_event_log();

CREATE FUNCTION billing.host_event_record(h billing.host_outbox) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE payload jsonb; payment record;
    source_merchant uuid:=h.merchant_id; source_payment uuid:=h.payment_id;
    account uuid; authority text;
BEGIN
    payload:=h.data || jsonb_strip_nulls(jsonb_build_object('currency',NULLIF(h.currency,''),'amount',h.amount::text));
    IF h.subject_type='customer' THEN payload:=payload||jsonb_build_object('customer_id',h.subject_id::text); END IF;
    IF h.subject_type='order' THEN payload:=payload||jsonb_build_object('order_id','ord_'||h.subject_id); END IF;
    IF h.subject_type='product' THEN payload:=payload||jsonb_build_object('product_id','prod_'||h.subject_id); END IF;
    IF h.subject_type='mandate' THEN payload:=payload||jsonb_build_object('mandate_id','mdt_'||h.subject_id); END IF;
    IF h.payment_id IS NOT NULL THEN
        SELECT customer_id,subscription_id,order_id INTO payment
        FROM billing.payments WHERE merchant_id=source_merchant AND id=source_payment;
        payload:=payload||jsonb_strip_nulls(jsonb_build_object('payment_id','pay_'||h.payment_id,
          'customer_id',payment.customer_id,'subscription_id',payment.subscription_id,'order_id',payment.order_id));
    END IF;
    -- A host fact may already name a different account (for example the
    -- network account linked to this merchant's customer). Keep that domain
    -- identity; only fill a missing generic customer-account snapshot.
    IF payload ? 'customer_id' AND NOT payload ? 'account_id' THEN
        account:=(payload->>'customer_id')::uuid;
        SELECT COALESCE(issuer,'') INTO authority FROM billing.customers
          WHERE merchant_id=source_merchant AND id=account;
        payload:=payload||jsonb_build_object('account_id',account,'account_issuer',authority);
    END IF;
    RETURN jsonb_build_object('merchant_id',h.merchant_id,'id','hev_'||h.id,'source','host',
      'type',h.event_type,'occurred_at',h.occurred_at,'subject_type',h.subject_type,
      'subject_id',billing.event_subject_id(h.subject_type,h.subject_id),'data',billing.event_log_data(payload));
END;
$$;

CREATE TABLE billing.eventlog_streams (
    source text NOT NULL,
    scope text NOT NULL,
    epoch text NOT NULL DEFAULT gen_random_uuid()::text,
    position bigint NOT NULL DEFAULT 0,
    head_id text NOT NULL DEFAULT '',
    pruned_through bigint NOT NULL DEFAULT 0,
    pruned_id text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL DEFAULT '-infinity',
    merchant_id uuid GENERATED ALWAYS AS (CASE WHEN source = 'openrails' THEN scope::uuid ELSE NULL::uuid END) STORED REFERENCES billing.merchants (id) ON DELETE CASCADE,
    PRIMARY KEY (source, scope)
);
COMMENT ON TABLE billing.eventlog_streams IS 'Shared committed event positions and retention boundaries; merchant-owned state.';

CREATE UNIQUE INDEX eventlog_merchant_stream ON billing.eventlog_streams (merchant_id)
    WHERE source = 'openrails';

CREATE TABLE billing.eventlog_events (
    source text NOT NULL,
    scope text NOT NULL,
    position bigint NOT NULL,
    id text NOT NULL,
    boundary text NOT NULL,
    event_source text NOT NULL,
    event_type text NOT NULL,
    version integer NOT NULL,
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    subject_type text NOT NULL,
    subject_id text NOT NULL,
    data jsonb NOT NULL,
    merchant_id uuid GENERATED ALWAYS AS (CASE WHEN source = 'openrails' THEN scope::uuid ELSE NULL::uuid END) STORED,
    PRIMARY KEY (source, scope, "position"),
    CONSTRAINT eventlog_events_source_scope_id_key UNIQUE (source, scope, id),
    CONSTRAINT eventlog_events_source_scope_fkey FOREIGN KEY (source, scope) REFERENCES billing.eventlog_streams (source, scope) ON DELETE CASCADE
);
COMMENT ON TABLE billing.eventlog_events IS 'Shared retained merchant facts. Retention: events expire after the configured event retention, independently of reader progress.';

CREATE INDEX eventlog_events_source_scope_event_type_position_idx ON billing.eventlog_events (source, scope, event_type, "position");
CREATE INDEX eventlog_events_source_scope_recorded_at_position_idx ON billing.eventlog_events (source, scope, recorded_at, "position");
CREATE INDEX eventlog_expired_merchants ON billing.eventlog_events (recorded_at, merchant_id, "position")
    WHERE source = 'openrails';
CREATE INDEX eventlog_merchant_position ON billing.eventlog_events (merchant_id, "position");
CREATE INDEX eventlog_merchant_retention ON billing.eventlog_events (merchant_id, recorded_at, "position");
CREATE INDEX eventlog_merchant_type_position ON billing.eventlog_events (merchant_id, event_type, "position");

CREATE FUNCTION billing.eventlog_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'pg_temp' AS $$
BEGIN
 RAISE EXCEPTION 'retained event facts are immutable';
END $$;

CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON billing.eventlog_events
    FOR EACH ROW EXECUTE FUNCTION billing.eventlog_immutable();

CREATE VIEW billing.event_log AS
 SELECT merchant_id,
    "position",
    id,
    event_source AS source,
    event_type,
    version,
    occurred_at,
    recorded_at,
    subject_type,
    subject_id,
    data
   FROM billing.eventlog_events
  WHERE (source = 'openrails'::text);

CREATE VIEW billing.event_log_streams AS
 SELECT merchant_id,
    (epoch)::uuid AS epoch,
    "position",
    head_id,
    pruned_through,
    pruned_id,
    recorded_at
   FROM billing.eventlog_streams
  WHERE (source = 'openrails'::text);

CREATE FUNCTION billing.update_legacy_eventlog_stream() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
 IF NEW.merchant_id IS DISTINCT FROM OLD.merchant_id THEN
  RAISE EXCEPTION 'event scope identity is immutable' USING ERRCODE='23514';
 END IF;
 UPDATE billing.eventlog_streams SET epoch=NEW.epoch::text,position=NEW.position,
 head_id=NEW.head_id,pruned_through=NEW.pruned_through,pruned_id=NEW.pruned_id,
 recorded_at=NEW.recorded_at WHERE source='openrails' AND scope=OLD.merchant_id::text;
 RETURN NEW;
END;
$$;

CREATE TRIGGER update_legacy_eventlog_stream INSTEAD OF UPDATE ON billing.event_log_streams FOR EACH ROW EXECUTE FUNCTION billing.update_legacy_eventlog_stream();

CREATE TABLE billing.eventlog_pending (
    ordinal bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(),
    source text NOT NULL CHECK (source <> ''),
    scope text NOT NULL CHECK (scope <> ''),
    id text NOT NULL CHECK (id <> ''),
    boundary text,
    event_source text NOT NULL,
    event_type text NOT NULL CHECK (event_type <> ''),
    version integer NOT NULL CHECK (version > 0),
    occurred_at timestamptz NOT NULL,
    subject_type text NOT NULL DEFAULT '',
    subject_id text NOT NULL DEFAULT '',
    data jsonb NOT NULL,
    PRIMARY KEY (ordinal)
);
COMMENT ON TABLE billing.eventlog_pending IS 'Shared transaction scratch; empty after commit.';

CREATE INDEX eventlog_pending_transaction_id_idx ON billing.eventlog_pending (transaction_id);

CREATE FUNCTION billing.eventlog_flush() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'pg_temp' AS $$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "billing".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "billing".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "billing".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
  -- Delivered at commit, so a LISTENing consumer reads when its stream has moved (version 2).
  PERFORM pg_notify(TG_TABLE_SCHEMA || '.eventlog', s.source || ' ' || s.scope);
 END LOOP;
 FOR e IN SELECT * FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "billing".eventlog_streams SET position=position+1,head_id=coalesce(e.boundary,e.id),
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "billing".eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,coalesce(e.boundary,e.id),e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id();
 RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON billing.eventlog_pending DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.eventlog_flush();

CREATE FUNCTION billing.queue_event_log(event jsonb) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE pinned text:=NULLIF(current_setting('openrails.merchant_id',true),'');
BEGIN
 IF pinned IS NOT NULL AND pinned IS DISTINCT FROM event->>'merchant_id' THEN
  RAISE EXCEPTION 'event source differs from the pinned merchant' USING ERRCODE='42501';
 END IF;
 INSERT INTO billing.eventlog_pending(source,scope,id,event_source,event_type,version,occurred_at,subject_type,subject_id,data)
 VALUES('openrails',event->>'merchant_id',event->>'id',event->>'source',event->>'type',
  coalesce((event->>'version')::integer,1),(event->>'occurred_at')::timestamptz,
  event->>'subject_type',event->>'subject_id',event->'data');
END;
$$;

CREATE TABLE billing.eventlog_migrations (
    version integer NOT NULL,
    PRIMARY KEY (version)
);
COMMENT ON TABLE billing.eventlog_migrations IS 'Installed shared event-log schema versions.';

INSERT INTO billing.eventlog_migrations (version) VALUES (1), (2);

-- ---------------------------------------------------------------------------
-- Reconciliation
-- ---------------------------------------------------------------------------

CREATE TABLE billing.reconciliation_state (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    source_domain text NOT NULL,
    fully_reconciled boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, source_domain),
    CONSTRAINT reconciliation_state_domain_check CHECK (source_domain IN ('subscriptions', 'payments', 'grants'))
);
COMMENT ON TABLE billing.reconciliation_state IS 'Per-(merchant, source_domain) reconciliation watermark. fully_reconciled gates the confirmed-absence rule: a destructive EXCESS repair is HELD until its source domain (subscriptions|payments|grants) is proven fully reconciled.';

CREATE TABLE billing.reconciliation_findings (
    id uuid NOT NULL DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    finding_type text NOT NULL,
    rail text,
    psp_id uuid,
    subject_key text NOT NULL,
    severity text NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    status text NOT NULL,
    recommended_action text,
    first_seen_run uuid,
    last_seen_run uuid,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution text,
    operator_notes text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    evidence jsonb,
    resolved_by text,
    notified_at timestamptz,
    notified_severity text,
    change jsonb,
    source text,
    evidence_digest text,
    approval_run_id uuid,
    pull_id uuid,
    dismissed_at timestamptz,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT reconciliation_findings_approval_run_id_fkey FOREIGN KEY (merchant_id, approval_run_id) REFERENCES billing.maintenance_runs (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT reconciliation_findings_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT reconciliation_findings_pull_id_fkey FOREIGN KEY (merchant_id, pull_id) REFERENCES billing.psp_imports (merchant_id, id) ON DELETE SET NULL (pull_id),
    CONSTRAINT reconciliation_findings_change_check CHECK (
        change IS NULL
        OR (finding_type ~~ 'pull.%' AND jsonb_typeof(change) = 'object' AND (change ->> 'kind') = ANY (ARRAY['create_subscription', 'record_charge', 'record_refund', 'adopt_subscription', 'cancel_subscription', 'update_card', 'revoke_access', 'refund_charge_after_cancel', 'cancel_provider_schedule', 'collect_skipped_renewal', 'correct_subscription']))
    ),
    CONSTRAINT reconciliation_findings_proposal_resolution_check CHECK (
        resolution IS NULL
        OR resolution NOT IN ('approved', 'dismissed', 'superseded', 'foreign')
        OR finding_type ~~ 'pull.%'
    ),
    CONSTRAINT reconciliation_findings_scope_check CHECK (
        (finding_type IN ('catalog.price.unmapped', 'life.subscription.unattributed') AND rail IS NOT NULL AND psp_id IS NOT NULL)
        OR (finding_type !~~ 'catalog.%' AND finding_type <> 'life.subscription.unattributed' AND ((rail IS NULL AND psp_id IS NULL) OR (finding_type ~~ 'pull.%' AND rail IS NOT NULL AND psp_id IS NOT NULL)))
    ),
    CONSTRAINT reconciliation_findings_source_check CHECK (
        (finding_type ~~ 'pull.%') = (source IS NOT NULL)
        AND (source IS NULL OR source IN ('pull', 'verification'))
    ),
    CONSTRAINT reconciliation_findings_type_check CHECK (finding_type ~ '^(pull|derive|life|consistency|notify|catalog)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'),
    CONSTRAINT reconciliation_findings_resolved_fields_check CHECK (
        (status IN ('auto_fixed', 'fixed', 'ignored') AND resolved_at IS NOT NULL AND resolution IS NOT NULL)
        OR (status IN ('reconcile_required', 'requires_review') AND resolved_at IS NULL AND resolution IS NULL)
    ),
    CHECK (status IN ('auto_fixed', 'reconcile_required', 'requires_review', 'fixed', 'ignored')),
    CONSTRAINT reconciliation_findings_approval_run_check CHECK ((NOT (resolution IS DISTINCT FROM 'approved')) = (approval_run_id IS NOT NULL)),
    CHECK (
        resolution IS NULL
        OR resolution IN ('auto_vanished', 'enforced', 'admin_fixed', 'ignored', 'approved', 'dismissed', 'superseded', 'foreign')
    ),
    CONSTRAINT reconciliation_findings_evidence_digest_check CHECK (
        (change IS NULL) = (evidence_digest IS NULL)
        AND (evidence_digest IS NULL OR evidence_digest ~ '^[0-9a-f]{64}$')
    ),
    CONSTRAINT reconciliation_findings_pull_id_check CHECK (pull_id IS NULL OR source = 'pull')
);
COMMENT ON TABLE billing.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, psp_id, subject_key): pull.*, catalog.price.unmapped and life.subscription.unattributed findings name the PSP whose read raised them. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored. Retention: resolved findings are deleted 12 months (366 days) after they were resolved and last seen.';
COMMENT ON COLUMN billing.reconciliation_findings.psp_id IS 'pull.*, catalog.price.unmapped and life.subscription.unattributed findings: the PSP whose read raised the finding. Part of the identity; absence can be proven only by a complete read of this PSP.';
COMMENT ON COLUMN billing.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): rail subscription id, transaction id, local subscription/payment-method uuid, or customer uuid depending on the check.';
COMMENT ON COLUMN billing.reconciliation_findings.first_seen_run IS 'The reconciliation pass that first saw this finding, an id naming no row; NULL when raised outside a pass (e.g. the intents volume breaker).';
COMMENT ON COLUMN billing.reconciliation_findings.last_seen_run IS 'The reconciliation pass that last saw this finding; a pass resolves the findings it did not see again.';
COMMENT ON COLUMN billing.reconciliation_findings.operator_notes IS 'Operator-entered notes attached when a finding is fixed or ignored manually.';
COMMENT ON COLUMN billing.reconciliation_findings.evidence IS 'Machine-readable finding evidence. Optional nested keys: provider, local, remote, intent, resolution.';
COMMENT ON COLUMN billing.reconciliation_findings.resolved_by IS 'Authenticated admin identity stamped on manual resolution (approve/ignore via the findings queue); NULL for automatic resolutions.';
COMMENT ON COLUMN billing.reconciliation_findings.notified_at IS 'Last time this OPEN finding pushed an operator notification; NULL = not yet notified this open episode. Cleared to NULL on every resolution so a reopened finding notifies again.';
COMMENT ON COLUMN billing.reconciliation_findings.notified_severity IS 'Severity at last notification; a further increase while still open re-fires, re-observation at the same/lower severity does not.';
COMMENT ON COLUMN billing.reconciliation_findings.change IS 'A pull.* proposal''s change: its kind, subject, from and to, effects, the basis it was computed against, when its evidence was observed, the provider evidence and what approving it executes. NULL on findings only dismissing resolves.';
COMMENT ON COLUMN billing.reconciliation_findings.source IS 'What raised a pull.* finding: a PSP read (pull) or a by-id read of an uncertain record (verification). Set exactly on pull.* findings.';
COMMENT ON COLUMN billing.reconciliation_findings.evidence_digest IS 'sha256 of the change''s kind and provider evidence. A dismissed proposal is raised again only when a read yields another digest.';
COMMENT ON COLUMN billing.reconciliation_findings.approval_run_id IS 'The approval run (maintenance_runs kind approval) that applied this proposal; undo-run reverses it. Set exactly when the resolution is approved.';
COMMENT ON COLUMN billing.reconciliation_findings.pull_id IS 'The PSP import whose pull last raised this pull.* proposal; NULL when none did or the import was deleted.';
COMMENT ON COLUMN billing.reconciliation_findings.dismissed_at IS 'When a person last dismissed this proposal, kept when it reopens: an import''s Apply never approves a proposal once dismissed.';

CREATE INDEX reconciliation_findings_approval_run_id_idx ON billing.reconciliation_findings (merchant_id, approval_run_id)
    WHERE approval_run_id IS NOT NULL;
CREATE INDEX reconciliation_findings_finding_type_idx ON billing.reconciliation_findings (finding_type)
    WHERE status IN ('reconcile_required', 'requires_review');
CREATE UNIQUE INDEX reconciliation_findings_finding_type_psp_id_subject_key_key ON billing.reconciliation_findings (merchant_id, finding_type, psp_id, subject_key) NULLS NOT DISTINCT;
CREATE INDEX reconciliation_findings_proposals_idx ON billing.reconciliation_findings (merchant_id, status, source, psp_id)
    WHERE finding_type ~~ 'pull.%';
CREATE INDEX reconciliation_findings_pull_id_idx ON billing.reconciliation_findings (merchant_id, pull_id)
    WHERE pull_id IS NOT NULL;
CREATE INDEX reconciliation_findings_resolved_idx ON billing.reconciliation_findings (merchant_id, GREATEST(resolved_at, last_seen_at))
    WHERE resolved_at IS NOT NULL;
