-- Configuration history: every change to a merchant's catalog and
-- configuration, one entry per changed object.

CREATE TABLE billing.change_log (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL CHECK (octet_length(actor) <= 4200 AND (actor ~ '^(staff|customer):.' OR actor ~ '^provider:[a-z][a-z0-9_]{0,31}$' OR actor IN ('system', 'file', 'vault'))),
    source text NOT NULL CHECK (source IN ('api', 'customer', 'console', 'catalog_apply', 'cli', 'reload', 'job', 'intent', 'webhook', 'provider_read')),
    object_type text NOT NULL CHECK (object_type IN ('product', 'price', 'meter', 'limit', 'merchant', 'psp', 'custodian')),
    object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 512),
    action text NOT NULL CHECK (action IN ('create', 'update', 'archive', 'unarchive', 'delete', 'key_add', 'key_remove', 'key_rename')),
    changes jsonb NOT NULL CHECK (jsonb_typeof(changes) = 'object' AND jsonb_typeof(changes->'version') = 'number' AND octet_length(changes::text) <= 262144),
    reason text CHECK (octet_length(reason) BETWEEN 1 AND 1024),
    request_id text CHECK (octet_length(request_id) BETWEEN 1 AND 128),
    operation_id uuid,
    PRIMARY KEY (merchant_id, id)
);
COMMENT ON TABLE billing.change_log IS 'Configuration history: each change to a catalog object (product, price, meter, limit) or a configuration document (merchant, psp, custodian), who made it and how. A catalog entry commits with its change; a configuration entry is written when OpenRails writes the document, or when a load finds a change made outside it. Credentials are never recorded. Nothing on a billing path reads it. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.change_log.actor IS 'staff:<invoker>, customer:<invoker>, provider:<rail>, or system, file or vault where no person acted.';
COMMENT ON COLUMN billing.change_log.object_id IS 'The object''s key: product:price for a price, the merchant id for the merchant document.';
COMMENT ON COLUMN billing.change_log.changes IS 'The changed fields'' values before and after, versioned per action; a credential reads "changed".';
COMMENT ON COLUMN billing.change_log.operation_id IS 'The operation the change belongs to, when one names it.';

CREATE INDEX change_log_object_idx ON billing.change_log (merchant_id, object_type, object_id, occurred_at);
CREATE INDEX change_log_occurred_at_idx ON billing.change_log (merchant_id, occurred_at);

-- Append-only. A merchant purge keeps it, like the ledger and the grant log.
-- It is not in the billing restore guard: a destination records its own
-- configuration's history as soon as that configuration loads, and could
-- never delete it, so an archive's entries join it.
CREATE TRIGGER immutable_change_log BEFORE DELETE OR UPDATE ON billing.change_log
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_change_log_truncate BEFORE TRUNCATE ON billing.change_log
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
