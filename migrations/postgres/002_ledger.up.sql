-- =============================================================================
-- #512 double-entry immutable money ledger
--
-- A TigerBeetle-style ledger expressed in Postgres: Accounts belong to ONE
-- ledger (= a (merchant, currency) pair); Transfers are immutable, append-only,
-- and move an amount debit->credit WITHIN one ledger. Balances are DERIVED from
-- transfers (never an overwritten scalar). Two-phase transfers (pending ->
-- post_pending|void_pending) model authorize/capture/release.
--
-- Conserved by construction: every posted transfer adds +amount to one
-- account's credits and +amount to another's debits, so the sum of all account
-- balances in a (merchant, currency) ledger is identically 0.
--
-- Append-only is enforced at the ROLE level: openrails_app is granted only
-- SELECT, INSERT (no UPDATE/DELETE) on both tables.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- ledger_accounts
-- -----------------------------------------------------------------------------
CREATE TABLE openrails.ledger_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    -- NULL for system accounts (one per merchant+currency); set for per-customer
    -- balance accounts.
    customer_id uuid,
    account_type text NOT NULL,
    currency text NOT NULL,
    -- TB-style sign constraints, enforced by the applier (a floor may relax
    -- debits_must_not_exceed_credits up to an arrears credit line).
    debits_must_not_exceed_credits boolean DEFAULT false NOT NULL,
    credits_must_not_exceed_debits boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT ledger_accounts_pkey PRIMARY KEY (id),
    CONSTRAINT ledger_accounts_type_check CHECK ((account_type = ANY (ARRAY[
        'customer_balance'::text,
        'platform_revenue'::text,
        'processor_clearing'::text,
        'arrears_liability'::text,
        'expired_credits'::text,
        'revoked_credits'::text,
        'fx_liquidity'::text,
        'world'::text
    ]))),
    CONSTRAINT ledger_accounts_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.ledger_accounts FORCE ROW LEVEL SECURITY;

-- One system account per (merchant, account_type, currency); one customer
-- account per (merchant, customer, account_type, currency).
CREATE UNIQUE INDEX uq_ledger_accounts_system
    ON openrails.ledger_accounts (merchant_id, account_type, currency)
    WHERE (customer_id IS NULL);
CREATE UNIQUE INDEX uq_ledger_accounts_customer
    ON openrails.ledger_accounts (merchant_id, customer_id, account_type, currency)
    WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_ledger_accounts_merchant_id ON openrails.ledger_accounts USING btree (merchant_id);
CREATE INDEX idx_ledger_accounts_customer ON openrails.ledger_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.ledger_accounts ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.ledger_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- Append-only foundation: accounts are created once, never mutated. The schema
-- default privileges (001) grant openrails_app full DML on every new table, so
-- immutability requires an explicit REVOKE of UPDATE/DELETE.
GRANT SELECT,INSERT ON TABLE openrails.ledger_accounts TO openrails_app;
REVOKE UPDATE, DELETE, TRUNCATE ON TABLE openrails.ledger_accounts FROM openrails_app;

COMMENT ON TABLE openrails.ledger_accounts IS '#512 double-entry ledger accounts. One account belongs to exactly one (merchant, currency) ledger; balance is DERIVED from ledger_transfers, never stored. account_type identifies its role (customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world).';
COMMENT ON COLUMN openrails.ledger_accounts.customer_id IS 'NULL for system accounts (one per merchant+currency); set for per-customer balance accounts.';
COMMENT ON COLUMN openrails.ledger_accounts.debits_must_not_exceed_credits IS 'TB sign flag: balance (credits-debits) may not go below zero (minus an applier-supplied arrears floor). Set on customer_balance.';

-- -----------------------------------------------------------------------------
-- ledger_transfers (immutable, append-only)
-- -----------------------------------------------------------------------------
CREATE TABLE openrails.ledger_transfers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    debit_account_id uuid NOT NULL,
    credit_account_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    transfer_type text NOT NULL,
    -- posted: single-phase, counts in balance.
    -- pending: a two-phase hold; counts in HELD, not balance, until resolved.
    -- post_pending|void_pending: resolves the pending row named by pending_id.
    phase text DEFAULT 'posted'::text NOT NULL,
    pending_id uuid,
    -- opaque control-plane references (ledger purity: NO business logic here).
    source text,
    source_id text,
    -- #514 lot attribution: which credit grant (lot) this transfer draws from /
    -- expires. Distinct from source/source_id (the OPERATION's idempotency
    -- coordinate, e.g. ('window_settle', request_id)) so a credit_spend can be
    -- both idempotent on its caller key AND attributed to a FIFO lot.
    grant_id uuid,
    customer_id uuid,
    invoker_id text,
    resource text,
    invoice_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT ledger_transfers_pkey PRIMARY KEY (id),
    CONSTRAINT ledger_transfers_amount_positive CHECK ((amount > 0)),
    CONSTRAINT ledger_transfers_distinct_accounts CHECK ((debit_account_id <> credit_account_id)),
    CONSTRAINT ledger_transfers_phase_check CHECK ((phase = ANY (ARRAY['posted'::text, 'pending'::text, 'post_pending'::text, 'void_pending'::text]))),
    -- pending_id is set iff this row resolves a pending transfer.
    CONSTRAINT ledger_transfers_pending_link CHECK (((phase = ANY (ARRAY['post_pending'::text, 'void_pending'::text])) = (pending_id IS NOT NULL))),
    CONSTRAINT ledger_transfers_debit_fk FOREIGN KEY (debit_account_id) REFERENCES openrails.ledger_accounts(id),
    CONSTRAINT ledger_transfers_credit_fk FOREIGN KEY (credit_account_id) REFERENCES openrails.ledger_accounts(id),
    CONSTRAINT ledger_transfers_pending_fk FOREIGN KEY (pending_id) REFERENCES openrails.ledger_transfers(id)
);

ALTER TABLE ONLY openrails.ledger_transfers FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_ledger_transfers_merchant_id ON openrails.ledger_transfers USING btree (merchant_id);
CREATE INDEX idx_ledger_transfers_debit ON openrails.ledger_transfers USING btree (debit_account_id);
CREATE INDEX idx_ledger_transfers_credit ON openrails.ledger_transfers USING btree (credit_account_id);
CREATE INDEX idx_ledger_transfers_source ON openrails.ledger_transfers USING btree (merchant_id, source, source_id) WHERE (source IS NOT NULL);
-- Lot-attribution lookups (derive a credit lot's spent/expired remainder).
CREATE INDEX idx_ledger_transfers_grant ON openrails.ledger_transfers USING btree (merchant_id, grant_id) WHERE (grant_id IS NOT NULL);
-- Per-customer transaction history (GetTransactions) + per-coordinate idempotency.
CREATE INDEX idx_ledger_transfers_customer ON openrails.ledger_transfers USING btree (merchant_id, customer_id, currency, created_at DESC) WHERE (customer_id IS NOT NULL);
-- A pending may be resolved at most once; partial unique index over resolvers.
CREATE UNIQUE INDEX uq_ledger_transfers_pending_resolution ON openrails.ledger_transfers USING btree (pending_id) WHERE (pending_id IS NOT NULL);

ALTER TABLE openrails.ledger_transfers ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.ledger_transfers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- IMMUTABLE / append-only: only SELECT + INSERT. The schema default privileges
-- (001) grant full DML to openrails_app on every new table, so we must REVOKE
-- UPDATE/DELETE explicitly — this is what makes the transfer log truly append-only.
GRANT SELECT,INSERT ON TABLE openrails.ledger_transfers TO openrails_app;
REVOKE UPDATE, DELETE, TRUNCATE ON TABLE openrails.ledger_transfers FROM openrails_app;

COMMENT ON TABLE openrails.ledger_transfers IS '#512 immutable double-entry transfers. Append-only (role granted SELECT,INSERT only). A transfer moves amount debit->credit within ONE (merchant, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. Balances + held are derived from this table.';
COMMENT ON COLUMN openrails.ledger_transfers.phase IS 'posted = single-phase (counts in balance); pending = two-phase hold (counts in held until resolved); post_pending/void_pending = resolves the pending named by pending_id.';
COMMENT ON COLUMN openrails.ledger_transfers.source IS 'Opaque origin key (e.g. ''grant''/grant_id, ''payment''/transaction_id). Ledger purity: business joins live in control-plane tables.';

-- -----------------------------------------------------------------------------
-- currency guard: a transfer must stay within one ledger (both accounts share
-- the transfer's currency). Enforced as a trigger because it spans rows.
-- -----------------------------------------------------------------------------
CREATE FUNCTION openrails.ledger_transfers_currency_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    dcur text;
    ccur text;
BEGIN
    SELECT currency INTO dcur FROM openrails.ledger_accounts WHERE id = NEW.debit_account_id;
    SELECT currency INTO ccur FROM openrails.ledger_accounts WHERE id = NEW.credit_account_id;
    IF dcur IS NULL OR ccur IS NULL THEN
        RAISE EXCEPTION 'ledger_transfers: debit/credit account not found';
    END IF;
    IF dcur <> NEW.currency OR ccur <> NEW.currency THEN
        RAISE EXCEPTION 'ledger_transfers: cross-currency transfer (debit=%, credit=%, transfer=%) — a transfer never crosses ledgers', dcur, ccur, NEW.currency;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_ledger_transfers_currency_guard
    BEFORE INSERT ON openrails.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION openrails.ledger_transfers_currency_guard();
