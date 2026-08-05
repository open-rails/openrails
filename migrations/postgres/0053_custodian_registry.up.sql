-- or#880 phase 3: custodians get a config home of their own.
--
-- Phase 2 (0031) killed the fake `vaulted_card` rail and parked the custody
-- arrangement in the charging PSP's settings blob: custodian /
-- custodian_account_id / custodian_public_api_key / custodian_network_tokens,
-- plus a `custodian_api_key` secret scoped to that PSP. That was correct about
-- WHERE custody hangs (on the rail that charges) and wrong about WHOSE
-- credentials those are. A custodian is an account a merchant holds with a
-- third party, exactly like a PSP is an account it holds with a gateway — and
-- one custodian can back SEVERAL PSPs (a live gateway and a sandbox gateway,
-- two acquirers fronting the same vault). Copied per PSP, the tenant id and
-- the private application key drift, and the "same" custodian silently becomes
-- two.
--
--   custodians.<key>.<kind>   is to a custodian
--   psps.<key>.<rail>         what it is to a PSP
--
-- and a PSP REFERENCES its custodian by key. Declared once, armed everywhere.
--
-- Nothing here touches an instrument. payment_methods.custodian keeps naming
-- the KIND that holds the card (or#870: a stored method is never destroyed).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- 1. A PSP that still carries an inline custody block cannot be folded
-- mechanically: its custodian would need a key nobody has chosen, and the
-- private application key is stored under a PSP-scoped secret name that only
-- the operator can re-address. No merchant declares one (the shipped example
-- is a commented sample, and or#879 verified the row count is zero), so the
-- honest handling of a surprise row is to REFUSE, not to guess.
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n
      FROM openrails.psps
     WHERE COALESCE(evidence -> 'settings' ->> 'custodian', 'psp') NOT IN ('', 'psp');
    IF n > 0 THEN
        RAISE EXCEPTION
            'openrails: % PSP row(s) still declare custody inline in settings (or#880 phase 3)', n
            USING ERRCODE = 'check_violation',
                  HINT = 'Custody credentials belong to a custodian, not to each PSP that charges through it. Declare merchants.<slug>.custodians.<key>.<kind> with account_id + settings.public_api_key + secret api_key, point the PSP at it with psps.<key>.<rail>.custodian: <key>, and drop the custodian_* keys from the PSP settings. Then re-run.';
    END IF;
END;
$$;

-- 2. The registry. One row is one merchant-owned account with one custodian.
CREATE TABLE openrails.custodians (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    kind text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    settings jsonb DEFAULT '{}'::jsonb NOT NULL,
    credential_versions jsonb DEFAULT '{}'::jsonb NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT custodians_kind_check CHECK ((kind = ANY (ARRAY['basis_theory'::text]))),
    CONSTRAINT custodians_environment_check CHECK ((environment = ANY (ARRAY['live'::text, 'test'::text]))),
    CONSTRAINT custodians_nonempty CHECK (((btrim(key) <> ''::text) AND (btrim(account_id) <> ''::text))),
    -- The target of the psps composite FK below: it is what makes a
    -- cross-merchant reference impossible at the DB level, not merely at the
    -- validator.
    CONSTRAINT uq_custodians_id_merchant UNIQUE (id, merchant_id)
);

ALTER TABLE ONLY openrails.custodians FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.custodians IS
    'or#880: merchant custodian registry. A row is one merchant-owned account with a third-party card custodian (Basis Theory today). Custody is orthogonal to the rail: this says WHO HOLDS the card, openrails.psps says who charges it. Referenced by psps.custodian_id — one custodian can back many PSPs.';
COMMENT ON COLUMN openrails.custodians.key IS
    'The custodian''s manifest key (merchants.<slug>.custodians.<key>) — the name a PSP entry references.';
COMMENT ON COLUMN openrails.custodians.kind IS
    'The custodian VENDOR: basis_theory today. Same vocabulary as payment_methods.custodian, minus ''psp'' (which is the absence of a third-party custodian, not an account).';
COMMENT ON COLUMN openrails.custodians.account_id IS
    'The custodian-native tenant identity (Basis Theory: the tenant id). Operator-declared — there is no runtime whoami (#592).';
COMMENT ON COLUMN openrails.custodians.settings IS
    'Declared NON-secret knobs, validated against the kind''s registry (internal/custodians): public_api_key, network_tokens. Credentials are merchant secrets under custodians/<kind>/<environment>/<account_id>/<key>.';
COMMENT ON COLUMN openrails.custodians.credential_versions IS
    'or#812 rotation watermarks, per credential key: the Secret.Version each credential reached at its last rotation. A reader holding an older cached version must go back to the backend, so a rotation on one node is effective on every node the instant it commits. Absent/zero = no floor.';
COMMENT ON COLUMN openrails.custodians.archived IS
    'Drain-only lifecycle flag, matching psps.archived: true keeps the custodian addressable for instruments it already holds but excludes it from new arrangements.';

ALTER TABLE ONLY openrails.custodians
    ADD CONSTRAINT custodians_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

-- The referencing key: unique per merchant, so `custodian: bt` resolves to one row.
CREATE UNIQUE INDEX uq_custodians_key ON openrails.custodians USING btree (merchant_id, lower(key));

-- The GLOBAL natural key, same shape as uq_psps_identity: one Basis Theory
-- tenant belongs to one merchant. This is what an inbound custodian webhook
-- routes by. It replaces uq_psps_custodian_identity (dropped below).
CREATE UNIQUE INDEX uq_custodians_identity ON openrails.custodians USING btree (kind, environment, account_id);

CREATE INDEX idx_custodians_merchant ON openrails.custodians USING btree (merchant_id);

ALTER TABLE openrails.custodians ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.custodians
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.custodians TO openrails_app;

-- 3. The reference. NULL = this PSP holds its own instruments, which is the
-- overwhelmingly common case and needs no configuration at all.
ALTER TABLE openrails.psps ADD COLUMN custodian_id uuid;

COMMENT ON COLUMN openrails.psps.custodian_id IS
    'or#880: the custodian holding the instruments charged through this PSP. NULL = the PSP holds its own (Stripe pm_, NMI customer vault). Composite FK: a PSP can only reference ITS OWN merchant''s custodian.';

-- Two-step, per the repo's lock-safety doctrine (0032/0043): NOT VALID here,
-- VALIDATE in 0054's own transaction. NOT VALID already enforces the reference
-- on every new and updated row — which is all of them, since the column was
-- added in the statement above and every existing row is NULL.
ALTER TABLE ONLY openrails.psps
    ADD CONSTRAINT psps_custodian_fk FOREIGN KEY (custodian_id, merchant_id)
    REFERENCES openrails.custodians(id, merchant_id) ON DELETE RESTRICT NOT VALID;

CREATE INDEX idx_psps_custodian ON openrails.psps USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);

-- 4. Custody identity moves off the PSP settings expression index onto the
-- custodian's own columns.
DROP INDEX IF EXISTS openrails.uq_psps_custodian_identity;
DROP FUNCTION IF EXISTS openrails.psp_owner_by_custodian_identity(text, text, text);

CREATE FUNCTION openrails.custodian_owner_by_identity(p_kind text, p_environment text, p_account_id text)
    RETURNS TABLE (id uuid, merchant_id uuid, key text, kind text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT c.id, c.merchant_id, c.key, c.kind, c.environment, c.account_id
      FROM openrails.custodians c
     WHERE c.kind = lower(p_kind)
       AND c.environment = p_environment
       AND c.account_id = p_account_id
     LIMIT 1;
END;
$$;

COMMENT ON FUNCTION openrails.custodian_owner_by_identity(text, text, text) IS
    'or#880: cross-merchant custodian ownership lookup by the GLOBAL (kind, environment, account_id) natural key — the custody sibling of psp_owner_by_identity, for inbound custodian webhooks (Basis Theory) that carry a tenant id and no merchant context. A custodian may back several PSPs, so this deliberately resolves the CUSTODIAN, never "the" PSP.';

REVOKE ALL ON FUNCTION openrails.custodian_owner_by_identity(text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.custodian_owner_by_identity(text, text, text) TO openrails_app;
