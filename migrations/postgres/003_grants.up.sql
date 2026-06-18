-- =============================================================================
-- #514 append-only grant ledger
--
-- The access-domain sibling of the #512 money ledger. A grant is an IMMUTABLE
-- event ("source S grants customer C product P for [start,end), spec X").
-- revoke / expire / supersede / adjust are NEW events that reference the
-- original via supersedes_id — a grant row is never updated.
--
-- The live entitlement windows (openrails.entitlements), product ownership (the
-- grant rows themselves), and credit lots (the #512 ledger) are DERIVED
-- projections folded from this log. Append-only is enforced at the role level:
-- openrails_app is granted SELECT,INSERT only (UPDATE/DELETE revoked over the
-- schema default grant).
--
-- Single-entry by nature: access is not conserved, so a grant is a one-sided
-- issuance — NOT double-entry. The one cross-over is that a credit grant also
-- emits a double-entry deposit transfer into the #512 ledger.
-- =============================================================================

CREATE TABLE openrails.grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    product_id uuid,
    kind text NOT NULL,
    source_type text NOT NULL,
    source_id text DEFAULT ''::text NOT NULL,
    payment_id uuid,
    event text DEFAULT 'grant'::text NOT NULL,
    supersedes_id uuid,
    spec_snapshot jsonb,
    starts_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    ends_at timestamp with time zone,
    amount bigint,
    currency text,
    reason text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT grants_pkey PRIMARY KEY (id),
    CONSTRAINT grants_kind_check CHECK ((kind = ANY (ARRAY['entitlement'::text, 'ownership'::text, 'credit'::text]))),
    CONSTRAINT grants_source_type_check CHECK ((source_type = ANY (ARRAY['purchase'::text, 'subscription'::text, 'admin'::text, 'grace'::text]))),
    CONSTRAINT grants_event_check CHECK ((event = ANY (ARRAY['grant'::text, 'revoke'::text, 'expire'::text, 'supersede'::text, 'adjust'::text]))),
    -- A 'grant' event roots a grant (supersedes_id NULL); every other event
    -- references the grant it acts on.
    CONSTRAINT grants_event_supersedes CHECK (((event = 'grant'::text) = (supersedes_id IS NULL))),
    -- Credit grants carry the lot's amount + currency (the grant IS the lot).
    CONSTRAINT grants_credit_amount CHECK (((kind <> 'credit'::text) OR ((amount IS NOT NULL) AND (currency IS NOT NULL)))),
    CONSTRAINT grants_amount_positive CHECK (((amount IS NULL) OR (amount > 0))),
    CONSTRAINT grants_valid_window CHECK (((ends_at IS NULL) OR (starts_at < ends_at))),
    CONSTRAINT grants_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id),
    CONSTRAINT grants_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id),
    CONSTRAINT grants_payment_fk FOREIGN KEY (payment_id) REFERENCES openrails.payments(id),
    CONSTRAINT grants_supersedes_fk FOREIGN KEY (supersedes_id) REFERENCES openrails.grants(id)
);

ALTER TABLE ONLY openrails.grants FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_grants_merchant_id ON openrails.grants USING btree (merchant_id);
CREATE INDEX idx_grants_customer ON openrails.grants USING btree (merchant_id, customer_id);
CREATE INDEX idx_grants_customer_kind ON openrails.grants USING btree (merchant_id, customer_id, kind) WHERE (event = 'grant'::text);
CREATE INDEX idx_grants_supersedes ON openrails.grants USING btree (supersedes_id) WHERE (supersedes_id IS NOT NULL);
CREATE INDEX idx_grants_source ON openrails.grants USING btree (merchant_id, source_type, source_id) WHERE (source_id <> ''::text);
-- A grant is terminated (revoke/expire/supersede) at most once.
CREATE UNIQUE INDEX uq_grants_termination ON openrails.grants USING btree (supersedes_id)
    WHERE ((supersedes_id IS NOT NULL) AND (event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])));

ALTER TABLE openrails.grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.grants USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- Append-only: the schema default privileges (001) grant openrails_app full DML
-- on every new table, so immutability requires an explicit REVOKE.
GRANT SELECT,INSERT ON TABLE openrails.grants TO openrails_app;
REVOKE UPDATE, DELETE, TRUNCATE ON TABLE openrails.grants FROM openrails_app;

COMMENT ON TABLE openrails.grants IS '#514 append-only grant ledger: the access-domain sibling of the #512 money ledger. Immutable events (grant/revoke/expire/supersede/adjust); the live entitlement windows, product ownership, and credit lots are DERIVED projections folded from this log. A credit grant carries the lot amount+currency and IS the FIFO credit lot (subsumes the old money_blocks role); derive-2 emits its #512 deposit transfer tagged source=grant.';
COMMENT ON COLUMN openrails.grants.event IS 'grant roots a grant; revoke/expire/supersede/adjust are new rows referencing it via supersedes_id. The grant row is never updated.';
COMMENT ON COLUMN openrails.grants.spec_snapshot IS 'Product entitlements/credits spec captured at issuance so derive-2 (grant->projection) is a pure function and replay is exact + historical.';

-- -----------------------------------------------------------------------------
-- Entitlements become a projection of the grant ledger: allow source_type='grant'.
-- (Hard cut: the older source_types remain valid only until their writers are
-- cut over; new entitlement windows are grant-sourced.)
-- -----------------------------------------------------------------------------
ALTER TABLE openrails.entitlements DROP CONSTRAINT chk_entitlements_source_type;
ALTER TABLE openrails.entitlements ADD CONSTRAINT chk_entitlements_source_type
    CHECK ((source_type = ANY (ARRAY['subscription'::text, 'one_off'::text, 'admin'::text, 'grace'::text, 'grant'::text])));
