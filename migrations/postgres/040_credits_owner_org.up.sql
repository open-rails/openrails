-- =============================================================================
-- 040 — Org-owned API usage credits (issue #221)
--
-- Makes the credit tables OWNER-ORG-owned (the payer / billing owner) while
-- keeping user_id for ACTOR attribution (who caused usage). This folds in the
-- tenant-scoping of the credit MUTATION paths that #223 (migration 039) left
-- for later, so the credit tables are rewritten only once.
--
-- THE THREE-WAY IDENTITY SPLIT (see pkg/identity):
--   * operator org  — admin authority (#224), NOT stored on credit rows;
--   * owner_id       — the org that OWNS the balance / is billed (this migration);
--   * user_id        — the ACTOR that caused usage (kept, unchanged).
--
-- This migration is intentionally ADDITIVE and FORWARD-ONLY-SAFE:
--   * Adds a NULLABLE owner_id (owner org) column to the three credit tables.
--   * Backfills owner_id DETERMINISTICALLY by mapping each existing user_id to
--     that user's STAND-IN personal-org id (see DERIVATION RULE below).
--   * Keeps user_id for actor attribution (NOT dropped, NOT renamed).
--   * Adds NEW owner-scoped idempotency / unique indexes ALONGSIDE the existing
--     user-scoped ones (additive). The old user-scoped uniques are intentionally
--     left in place during the rollout; see RETIREMENT NOTE below.
--
-- It does NOT (deferred to a follow-up once all writers stamp owner_id and the
-- AuthKit personal-org reconciliation has run):
--   * Enforce NOT NULL on owner_id.
--   * Drop or rewrite the existing user-scoped unique indexes.
--   * Rename user_id -> actor_user_id (kept as user_id to avoid breaking direct
--     SQL readers; the model exposes it as ActorUserID at the Go layer).
--
-- DERIVATION RULE for the backfill (MUST match pkg/identity.PersonalOrgID
-- byte-for-byte):
--
--   owner_id = UUIDv5(namespace, 'openrails:personal-org:' || user_id)
--   namespace = 6f1c9b3e-2a44-5d7c-8e10-9a2b3c4d5e6f
--
-- UUIDv5 = RFC 4122 name-based SHA-1 UUID. It is deterministic and collision-
-- free per distinct user_id, and reproducible here in pure SQL via pgcrypto's
-- digest(). This owner_id is a STAND-IN, not the real AuthKit personal-org id
-- (those are random uuidv7 minted by AuthKit and are not yet guaranteed to
-- exist for legacy users). A later AuthKit-side reconciliation must map each
-- stand-in owner_id to the user's real personal-org id; until then balances are
-- correctly org-scoped to a stable per-user owner id and NO credits are created
-- or destroyed (every existing balance maps 1:1 to exactly one owner_id).
--
-- RETIREMENT NOTE: once every writer stamps owner_id and the AuthKit
-- reconciliation has run, a follow-up migration should (a) backfill owner_id
-- from the reconciled real personal-org ids, (b) make owner_id NOT NULL, and
-- (c) DROP the old user-scoped uniques (uniq_credit_hold_idem,
-- uniq_credit_deposit_idem, uniq_credit_withdrawal_idem, and the
-- user_credit_balances UNIQUE(user_id, credit_type_id)) in favour of the
-- owner-scoped ones added here.
--
-- CROSS-REPO COUPLING NOTE: doujins / hentai0 read billing.entitlements (not the
-- credit tables) via direct SQL; the credit tables are accessed only through the
-- OpenRails service/facade. Adding a nullable column + extra indexes here is
-- fully backward compatible regardless.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- pgcrypto provides digest() (SHA-1), needed for the UUIDv5 derivation below.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- -----------------------------------------------------------------------------
-- Deterministic UUIDv5 derivation, identical to Go's uuid.NewSHA1:
--   v5(namespace, name) = first 16 bytes of SHA1(namespace_bytes || name_bytes),
--   with the version nibble forced to 5 and the variant bits forced to 10xx.
--
-- Defined as a temporary helper in this migration's transaction scope only, so
-- it does not leak into the runtime schema. We inline it as an expression via a
-- DO block + UPDATE rather than a permanent function.
-- -----------------------------------------------------------------------------

DO $$
DECLARE
    -- namespace 6f1c9b3e-2a44-5d7c-8e10-9a2b3c4d5e6f as raw bytes
    ns_bytes CONSTANT BYTEA := decode('6f1c9b3e2a445d7c8e109a2b3c4d5e6f', 'hex');
    prefix   CONSTANT TEXT  := 'openrails:personal-org:';
    t        TEXT;
    credit_tables CONSTANT TEXT[] := ARRAY[
        'user_credit_balances',
        'credit_transactions',
        'credit_blocks'
    ];
BEGIN
    FOREACH t IN ARRAY credit_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'billing' AND table_name = t
        ) THEN
            -- 1. Add the nullable owner_id column.
            EXECUTE format(
                'ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS owner_id UUID',
                t
            );

            -- 2. Backfill owner_id = UUIDv5(ns, prefix || user_id) for every row
            --    whose owner_id is still NULL. This is the deterministic
            --    personal-org mapping; it preserves balances exactly because it
            --    is a pure function of user_id (1 user -> 1 owner, no merge, no
            --    split). The SHA-1 digest is taken over ns_bytes || utf8(name);
            --    we then force the version (byte 6 high nibble -> 5) and variant
            --    (byte 8 high bits -> 10).
            EXECUTE format($q$
                UPDATE billing.%I AS tbl SET owner_id = (
                    WITH h AS (
                        SELECT substring(
                            public.digest(%L::bytea || convert_to(%L || tbl.user_id, 'UTF8'), 'sha1')
                            FROM 1 FOR 16
                        ) AS b
                    )
                    SELECT encode(
                        set_byte(
                            set_byte(
                                h.b,
                                6, (get_byte(h.b, 6) & 15) | 80     -- version 5: (b6 & 0x0F) | 0x50
                            ),
                            8, (get_byte(h.b, 8) & 63) | 128         -- variant 10xx: (b8 & 0x3F) | 0x80
                        ),
                        'hex'
                    )::uuid
                    FROM h
                )
                WHERE tbl.owner_id IS NULL
            $q$, t, ns_bytes, prefix);

            EXECUTE format(
                $cmt$COMMENT ON COLUMN billing.%I.owner_id IS 'Owner org that OWNS this balance / is billed (issue #221, payer/billing owner). NULLABLE during the additive rollout; backfilled to a deterministic STAND-IN personal-org id = UUIDv5(ns, ''openrails:personal-org:'' || user_id). Reconciled to the real AuthKit personal-org id by a later migration, then made NOT NULL. user_id is kept for ACTOR attribution.'$cmt$,
                t
            );
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- Fold in #223's tenant scoping for the credit MUTATION paths: ensure every
-- credit row also carries tenant_id. Migration 039 already added tenant_id +
-- backfilled it to the default tenant for these tables; this is a defensive
-- re-backfill so any rows written between 039 and 040 without a tenant are
-- normalised to the default tenant. (Idempotent; no-op if already set.)
-- -----------------------------------------------------------------------------

DO $$
DECLARE
    default_tenant CONSTANT UUID := '00000000-0000-0000-0000-000000000001';
    t TEXT;
    credit_tables CONSTANT TEXT[] := ARRAY[
        'user_credit_balances',
        'credit_transactions',
        'credit_blocks'
    ];
BEGIN
    FOREACH t IN ARRAY credit_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'tenant_id'
        ) THEN
            EXECUTE format(
                'UPDATE billing.%I SET tenant_id = %L WHERE tenant_id IS NULL',
                t, default_tenant
            );
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- NEW owner-scoped indexes + idempotency uniques, ALONGSIDE the existing
-- user-scoped ones. Idempotency is now unique per (tenant, owner, credit_type,
-- source, source_id) per transaction kind, matching the org-owned model.
--
-- We additionally scope by tenant_id so the same owner/source/source_id in two
-- tenants cannot collide (defence-in-depth for the multi-tenant case).
--
-- These do NOT replace the old user-scoped uniques yet (see RETIREMENT NOTE in
-- the header); both sets coexist during the rollout.
-- -----------------------------------------------------------------------------

-- Owner-scoped balance lookup (the GetBalance / lockBalance hot path).
CREATE INDEX IF NOT EXISTS idx_user_credit_balances_owner
    ON billing.user_credit_balances (tenant_id, owner_id, credit_type_id);

-- Owner-scoped balance uniqueness (the eventual replacement for
-- UNIQUE(user_id, credit_type_id)). Partial on owner_id IS NOT NULL so it is
-- safe to create during the additive rollout (some rows may transiently lack an
-- owner if written by an un-upgraded writer; backfill above covers existing).
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_credit_balances_owner_type
    ON billing.user_credit_balances (tenant_id, owner_id, credit_type_id)
    WHERE owner_id IS NOT NULL;

-- Owner-scoped transaction history.
CREATE INDEX IF NOT EXISTS idx_credit_transactions_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, created_at DESC);

-- Owner-scoped idempotency uniques per transaction kind.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_hold_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'hold' AND owner_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_deposit_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'deposit' AND source_id IS NOT NULL AND owner_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'withdrawal' AND source_id IS NOT NULL AND owner_id IS NOT NULL;

-- Owner-scoped FIFO block consumption.
CREATE INDEX IF NOT EXISTS idx_credit_blocks_owner
    ON billing.credit_blocks (tenant_id, owner_id, credit_type_id, expires_at ASC);
