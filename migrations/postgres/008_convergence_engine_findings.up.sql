-- =============================================================================
-- #511 Convergence Engine — findings-ledger foundation (Phase A)
--
-- Reframes the reconciliation findings ledger from the old per-provider `PS-*`
-- taxonomy onto the unified four-plane taxonomy (PULL / DERIVE / LIFE / CON) from
-- docs/consistency-invariants.md, and adds the state the confirmed-absence gate
-- (§3.2) needs to hold destructive EXCESS repairs until a source domain is proven
-- fully reconciled for a merchant.
--
--   - finding_type: replace the PS-1..PS-10 set with qualified slugs
--     (`pull.charge.missing`, `derive.grant.excess`, ...).
--   - status: add `held` (EXCESS blocked by the confirmed-absence gate) and
--     `indeterminate` (authority unreachable / evidence ambiguous — §8).
--   - provider: already free text, so the `self` sentinel for internal
--     (DERIVE/LIFE/CON) findings needs no schema change.
--   - reconciliation_state: per (merchant, source_domain) fully-reconciled flag +
--     last-full-pull watermark backing the confirmed-absence gate.
--
-- Transitional UNION: the new qualified slugs are admitted alongside
-- the legacy PS-1..PS-10 so the existing reconcile engine keeps writing while it
-- is migrated onto the new taxonomy. #511 Phase F drops the PS-* codes from this
-- CHECK once the old engine is fully retired.
-- =============================================================================

-- --- finding_type: unified four-plane taxonomy (+ legacy PS-* during transition) ---
ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT chk_reconciliation_findings_type;
ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_type CHECK (
        finding_type ~ '^(pull|derive|life|consistency)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'
        OR finding_type = ANY (ARRAY[
            -- legacy PS-* (transitional; removed in #511 Phase F)
            'PS-1'::text, 'PS-2'::text, 'PS-3'::text, 'PS-4'::text, 'PS-5'::text,
            'PS-6'::text, 'PS-7'::text, 'PS-8'::text, 'PS-9'::text, 'PS-10'::text
        ])
    );

-- --- status: add held + indeterminate -----------------------------------------
ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT chk_reconciliation_findings_status;
ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_status CHECK (status = ANY (ARRAY[
        'open'::text,
        'auto_fixed'::text,
        'admin_pending'::text,
        'resolved'::text,
        'dismissed'::text,
        'held'::text,          -- EXCESS repair blocked by the confirmed-absence gate (§3.2)
        'indeterminate'::text  -- authority unreachable / evidence ambiguous (§8)
    ]));

COMMENT ON COLUMN openrails.reconciliation_findings.provider IS 'Provider for pull.* findings (nmi|ccbill|stripe|solana); the literal ''self'' for internal derive/life/consistency findings (#511).';
COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, provider, finding_type, subject_key); finding_type is a qualified name such as pull.charge.missing, derive.grant.excess, life.provider_intent.abandoned, or consistency.amount_mismatch.provider_catalog. Legacy PS-* findings remain transitional until #511 Phase F.';

-- --- reconciliation_state: per-domain confirmed-absence gate -------------------
-- One row per (merchant, source_domain). `fully_reconciled` flips true only after
-- a completed authoritative pull/import for that domain; until then every EXCESS
-- repair for that domain is HELD (§3.2) instead of auto-retracting.
CREATE TABLE openrails.reconciliation_state (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    source_domain text NOT NULL,
    fully_reconciled boolean DEFAULT false NOT NULL,
    last_full_pull_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reconciliation_state_pkey PRIMARY KEY (id),
    CONSTRAINT chk_reconciliation_state_domain CHECK (source_domain = ANY (ARRAY[
        'subscriptions'::text, 'payments'::text, 'grants'::text
    ]))
);

CREATE UNIQUE INDEX uq_reconciliation_state_identity
    ON openrails.reconciliation_state (merchant_id, source_domain);

ALTER TABLE ONLY openrails.reconciliation_state FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.reconciliation_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.reconciliation_state
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

COMMENT ON TABLE openrails.reconciliation_state IS '#511 per-(merchant, source_domain) reconciliation watermark. fully_reconciled gates the confirmed-absence rule: a destructive EXCESS repair is HELD until its source domain (subscriptions|payments|grants) is proven fully reconciled.';
