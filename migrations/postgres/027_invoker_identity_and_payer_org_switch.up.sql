--
-- #491 (open slices): the PAYER↔INVOKER identity split, finished at the data model.
--
-- THREE things happen here, all idempotent + guarded so this is a no-op on a fresh
-- 001 baseline that already declares the final shape and re-runnable on an old DB:
--
-- 1. customers re-grows a NATURAL KEY behind its uuidv7 surrogate pk (REVERSES
--    slice-1's "UUID-only, no lookup column"). Resolution now switches on the
--    issuer's org-binding (authkit#80 nullable org_id):
--      org-bound issuer  -> ONE payer per org      : UNIQUE(merchant_id, org_id)
--      org-less / native -> per (issuer, subject)   : UNIQUE(merchant_id, issuer, subject)
--    Both resolved via INSERT ... ON CONFLICT (<natural key>) DO UPDATE RETURNING id,
--    so the deterministic uuidv5 FederatedCustomerID is GONE (id is uuidv7, minted
--    once, looked up by the natural key). Columns are NULLABLE: the native/embedded
--    path keeps materializing the subject UUID directly as the id (no natural key).
--
-- 2. invoker_id: the INVOKER (end-user) becomes a first-class FK'd identity on the
--    spend-ATTRIBUTION tables — money_transactions / usage_events / money_spend_limits
--    — pointing CROSS-SCHEMA at authkit profiles.delegated_users(id). NULLABLE +
--    ADDITIVE (we do NOT drop the opaque `actor` label): the FEDERATED/standalone
--    delegated path (which carries an issuer) resolves it via TouchDelegatedUser and
--    stamps it; the EMBEDDED/service-proxy path has NO issuer in context (the host
--    asserts identity — see #491 EMBEDDED SECURITY MODEL), so it leaves invoker_id
--    NULL and the opaque `actor` text stays the attribution/abuse key there. The
--    polymorphic budget scope-key columns (budget_reservations/budget_window_state
--    .actor, which hold subject|role:<uuid>|<invoker> per budgets/scopes.go) are NOT
--    a pure invoker and are intentionally left UNCHANGED — see progress.md Outcome.
--
-- CROSS-SCHEMA FK ORDERING: profiles.delegated_users (authkit#81) must pre-exist.
-- In standalone (internal/migrate) authkit migrations run BEFORE openrails ones, so
-- it does. Embedded hosts own their own authkit migration ordering, so the FK is
-- added only when the table is present; otherwise the column lands without the FK
-- (a host that migrates authkit later can add it, or re-run this no-op once present).
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- ===========================================================================
-- 1. customers natural key (re-added behind the uuidv7 surrogate pk).
-- ===========================================================================
ALTER TABLE openrails.customers ADD COLUMN IF NOT EXISTS org_id  text;
ALTER TABLE openrails.customers ADD COLUMN IF NOT EXISTS issuer  text;
ALTER TABLE openrails.customers ADD COLUMN IF NOT EXISTS subject text;

-- org-bound payer: ONE customer per (merchant, org).
CREATE UNIQUE INDEX IF NOT EXISTS uq_customers_merchant_org
  ON openrails.customers (merchant_id, org_id)
  WHERE org_id IS NOT NULL;

-- org-less / standalone-federated payer: per (merchant, issuer, subject).
CREATE UNIQUE INDEX IF NOT EXISTS uq_customers_merchant_issuer_subject
  ON openrails.customers (merchant_id, issuer, subject)
  WHERE org_id IS NULL AND issuer IS NOT NULL AND subject IS NOT NULL;

COMMENT ON COLUMN openrails.customers.org_id IS
  '#491 org-bound payer natural key: authkit org id (issuer is org-bound). One customer per (merchant, org). NULL for org-less/native payers.';
COMMENT ON COLUMN openrails.customers.issuer IS
  '#491 org-less payer natural key part: the validated issuer. NULL for org-bound and for native (subject==id) payers.';
COMMENT ON COLUMN openrails.customers.subject IS
  '#491 org-less payer natural key part: the merchant-supplied STABLE subject uuid. NULL for org-bound and native payers.';

-- ===========================================================================
-- 2. invoker_id on the spend-attribution tables (nullable, cross-schema FK).
-- ===========================================================================
ALTER TABLE openrails.money_transactions ADD COLUMN IF NOT EXISTS invoker_id uuid;
ALTER TABLE openrails.usage_events       ADD COLUMN IF NOT EXISTS invoker_id uuid;
ALTER TABLE openrails.money_spend_limits ADD COLUMN IF NOT EXISTS invoker_id uuid;

CREATE INDEX IF NOT EXISTS idx_money_transactions_invoker ON openrails.money_transactions (merchant_id, invoker_id, created_at DESC) WHERE invoker_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usage_events_invoker       ON openrails.usage_events       (merchant_id, invoker_id, occurred_at DESC) WHERE invoker_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_money_spend_limits_invoker ON openrails.money_spend_limits (merchant_id, invoker_id)               WHERE invoker_id IS NOT NULL;

COMMENT ON COLUMN openrails.money_transactions.invoker_id IS '#491 the end-user (invoker) that triggered this charge -> authkit profiles.delegated_users(id), cross-schema. NULL on the embedded/service path (no issuer to resolve); `actor` is the opaque attribution label there.';
COMMENT ON COLUMN openrails.usage_events.invoker_id       IS '#491 the end-user (invoker) that fired this usage -> authkit profiles.delegated_users(id), cross-schema. NULL on the embedded/service path.';
COMMENT ON COLUMN openrails.money_spend_limits.invoker_id IS '#491 the end-user (invoker) this per-invoker cap applies to -> authkit profiles.delegated_users(id), cross-schema. NULL when keyed by the opaque actor label.';

-- Cross-schema FK -> profiles.delegated_users(id). Guarded on the table existing
-- (embedded ordering) AND on the constraint not already existing (re-runnable).
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = 'profiles' AND table_name = 'delegated_users'
  ) THEN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'money_transactions_invoker_fk') THEN
      ALTER TABLE openrails.money_transactions
        ADD CONSTRAINT money_transactions_invoker_fk
        FOREIGN KEY (invoker_id) REFERENCES profiles.delegated_users(id) ON DELETE SET NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'usage_events_invoker_fk') THEN
      ALTER TABLE openrails.usage_events
        ADD CONSTRAINT usage_events_invoker_fk
        FOREIGN KEY (invoker_id) REFERENCES profiles.delegated_users(id) ON DELETE SET NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'money_spend_limits_invoker_fk') THEN
      ALTER TABLE openrails.money_spend_limits
        ADD CONSTRAINT money_spend_limits_invoker_fk
        FOREIGN KEY (invoker_id) REFERENCES profiles.delegated_users(id) ON DELETE SET NULL;
    END IF;
  ELSE
    RAISE NOTICE '#491: profiles.delegated_users absent at migration time; invoker_id columns added WITHOUT the cross-schema FK (embedded host must add it after its authkit migrations, or re-run this).';
  END IF;
END $$;

-- ===========================================================================
-- 3. Fold pre-existing per-(issuer,subject) balances into the org payer
--    (ORG-BOUND issuers ONLY). Structural-only when there is no real data:
--    standalone customers carry no org/issuer natural key yet (slice-1 dropped
--    them), so this matches zero rows on a fresh system. Documented in
--    progress.md: no real federated balances exist to fold at this time.
-- ===========================================================================
-- (No-op move: there are no org-bound customer rows yet — the natural-key columns
--  were just added empty. Left as a documented structural step; a real fold would
--  reparent money_*/usage rows from the per-subject customer to the org customer.)
