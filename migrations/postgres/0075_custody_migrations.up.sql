-- or#297 Phase C: the custody-migration record.
--
-- The deplatforming scenario: a processor drops the merchant. Cards vaulted AT
-- that processor are hostage — the PAN was never ours, and the gateway that
-- holds it is the one that just terminated us. Cards held by a neutral
-- custodian (or#880) are portable, because the custodian will export them to
-- any PCI-AoC destination.
--
-- Phase C makes the PSP-vaulted book RECOVERABLE. Once the merchant obtains a
-- vault export and the custodian ingests it (an operational, vendor-mediated
-- act — OpenRails never touches a PAN), each instrument gets a custodian token
-- standing for the SAME card. Flipping payment_methods.custodian from 'psp' to
-- that custodian, on the SAME payment_method_id, is all it takes: subscriptions
-- point at the instrument, not at the vault, and or#879/or#880 already made the
-- charge transport a function of the instrument's custody.
--
-- This table is the flip's memory. It exists for three reasons:
--   1. The old PSP vault handle must never be lost. It is the only evidence of
--      where the card used to live, and the only way to explain a charge that
--      settled before the flip.
--   2. Idempotency. A re-run of the same export must be a no-op, not a second
--      flip.
--   3. Reversibility IN RECORD. Every field needed to re-point the instrument
--      back at its PSP vault is here. That is NOT reversibility in CUSTODY: the
--      processor may have deleted the vault entry, or terminated the merchant
--      outright — which is the very event this mechanism exists for. Restoring
--      the record does not restore the card.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.custody_migrations (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    -- One operator run. Every row an import produced shares it, so a report is
    -- a single indexed read and a run is auditable as a unit.
    batch_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    rail text NOT NULL,
    from_custodian text NOT NULL,
    from_custodian_id uuid,
    from_rail_customer_ref text DEFAULT ''::text NOT NULL,
    from_rail_method_ref text DEFAULT ''::text NOT NULL,
    from_psp_id uuid,
    to_custodian text NOT NULL,
    to_custodian_id uuid NOT NULL,
    to_rail_method_ref text NOT NULL,
    to_psp_id uuid,
    -- When the custodian's ingest of the vault export was true. Declared, like
    -- billingimport's AsOf — the horizon the tokens describe.
    exported_at timestamp with time zone,
    outcome text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_custody_migrations_outcome
        CHECK ((outcome = ANY (ARRAY['remapped'::text, 'created'::text]))),
    CONSTRAINT chk_custody_migrations_target
        CHECK ((btrim(to_rail_method_ref) <> ''::text) AND (btrim(to_custodian) <> ''::text))
);

ALTER TABLE ONLY openrails.custody_migrations FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.custody_migrations IS
    'or#297 Phase C: one row per instrument whose CUSTODY changed — the durable memory of a vault-export remap. Records where the card used to live (the PSP vault handle the processor holds) and where it lives now (the custodian token), on an unchanged payment_method_id so subscriptions never move. Reversible in RECORD, never in custody: the fields to re-point an instrument back are all here, but a processor that deleted the vault entry or terminated the merchant cannot be undone by a row.';
COMMENT ON COLUMN openrails.custody_migrations.batch_id IS
    'The operator run that produced this row. A dry-run plan writes nothing; an applied run stamps every flip with one batch id so the report and the audit agree.';
COMMENT ON COLUMN openrails.custody_migrations.from_rail_customer_ref IS
    'The PSP-scope vault handle the instrument had BEFORE the flip (NMI customer_vault_id). Retained on the payment_methods row too — this is the copy that survives a later re-remap.';
COMMENT ON COLUMN openrails.custody_migrations.from_rail_method_ref IS
    'The instrument-scope handle before the flip (NMI billing_id; empty for the one-vault-per-card default, #682).';
COMMENT ON COLUMN openrails.custody_migrations.to_rail_method_ref IS
    'The custodian token id the instrument now charges through — payment_methods.rail_method_ref after the flip.';
COMMENT ON COLUMN openrails.custody_migrations.outcome IS
    'remapped = an existing instrument changed custody (same payment_method_id, subscriptions untouched); created = the export carried a card with no local instrument and the operator declared its customer.';
COMMENT ON COLUMN openrails.custody_migrations.exported_at IS
    'The declared horizon of the custodian''s ingest of the vault export — when the token set was true.';

ALTER TABLE ONLY openrails.custody_migrations
    ADD CONSTRAINT custody_migrations_merchant_fk FOREIGN KEY (merchant_id)
    REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.custody_migrations
    ADD CONSTRAINT custody_migrations_payment_method_fk FOREIGN KEY (payment_method_id)
    REFERENCES openrails.payment_methods(id) ON DELETE CASCADE;

-- The idempotency key of the flip itself: one instrument reaches one custodian
-- token once. A re-run of the same export conflicts here rather than writing a
-- second history for the same move.
CREATE UNIQUE INDEX uq_custody_migrations_target
    ON openrails.custody_migrations USING btree (merchant_id, payment_method_id, to_rail_method_ref);

CREATE INDEX idx_custody_migrations_batch
    ON openrails.custody_migrations USING btree (merchant_id, batch_id, created_at);

ALTER TABLE openrails.custody_migrations ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.custody_migrations
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.custody_migrations TO openrails_app;
