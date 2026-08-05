-- or#893 × or#795. 0063 made PSP provenance total on the intent plane:
-- rail_intents.psp_id and rail_mutation_logs.psp_id are NOT NULL, and the
-- runner refuses a provider write it cannot attribute. That is right for every
-- intent addressed to a GATEWAY ACCOUNT — which was all of them when 0063 was
-- written.
--
-- or#795's batch account updater is not. `bt_account_updater_batch` is addressed
-- to a CUSTODIAN: the write uploads one token batch to Basis Theory, and a
-- custodian backs MANY PSPs (0053: "one custodian can back many PSPs"). There is
-- no single psp_id to name, and inventing one would claim the batch belongs to a
-- gateway account it was never sent to.
--
-- Same doctrine as 0063's off-rail payments lane: classify the genuinely
-- different class EXPLICITLY rather than weakening the invariant for everyone.
-- An intent must still name the account it will execute against — the CHECK
-- below says exactly that, and now admits the second kind of account. It is
-- "at least one" rather than "exactly one" deliberately: a custodian-proxy sale
-- is addressed to a PSP whose card happens to sit at a custodian, so an intent
-- knowing BOTH is more provenance, not less, and must not be forbidden.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- ------------------------------------------------------------ rail_intents ---

ALTER TABLE openrails.rail_intents ADD COLUMN custodian_id uuid;

COMMENT ON COLUMN openrails.rail_intents.custodian_id IS
    'or#893/or#795: the custodian this outbound write is addressed to, for intents that target a custodian rather than a gateway account (the batch account updater). NULL for the ordinary PSP-addressed intent. Composite FK: an intent can only reference ITS OWN merchant''s custodian.';

-- Composite, exactly like psps_custodian_fk (0053): the reference cannot cross
-- a merchant boundary.
ALTER TABLE ONLY openrails.rail_intents
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_intents_custodian_fk FOREIGN KEY (custodian_id, merchant_id)
    REFERENCES openrails.custodians(id, merchant_id) ON DELETE RESTRICT;

CREATE INDEX idx_rail_intents_custodian ON openrails.rail_intents USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);

-- squawk-ignore ban-drop-not-null
ALTER TABLE openrails.rail_intents ALTER COLUMN psp_id DROP NOT NULL;

-- The invariant 0063 actually meant: an outbound write names the account it
-- will execute against. Dropping NOT NULL above does not weaken it — this
-- restates it over both kinds of account. Every existing row has psp_id NOT
-- NULL (it was the column attribute until two statements ago), so the scan
-- validates rows already known to satisfy it, and NOT VALID buys nothing inside
-- a single-transaction migrator (see 0014).
ALTER TABLE openrails.rail_intents
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT rail_intents_addressed
    CHECK (psp_id IS NOT NULL OR custodian_id IS NOT NULL);

COMMENT ON COLUMN openrails.rail_intents.psp_id IS
    'PSP the outbound intent was enqueued against. Required unless the intent is custodian-addressed (rail_intents_addressed) — or#893/or#795.';

-- ------------------------------------------------------ rail_mutation_logs ---
-- The runner logs one mutation row per attempt for EVERY intent, so the log
-- needs the same two-address vocabulary or a custodian-addressed attempt could
-- not be recorded at all.

ALTER TABLE openrails.rail_mutation_logs ADD COLUMN custodian_id uuid;

COMMENT ON COLUMN openrails.rail_mutation_logs.custodian_id IS
    'or#893/or#795: the custodian the logged mutation was addressed to, for custodian-addressed intents. NULL for the ordinary PSP-addressed mutation.';

ALTER TABLE ONLY openrails.rail_mutation_logs
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_mutation_logs_custodian_fk FOREIGN KEY (custodian_id, merchant_id)
    REFERENCES openrails.custodians(id, merchant_id) ON DELETE CASCADE;

CREATE INDEX idx_rail_mutation_logs_custodian ON openrails.rail_mutation_logs USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);

-- squawk-ignore ban-drop-not-null
ALTER TABLE openrails.rail_mutation_logs ALTER COLUMN psp_id DROP NOT NULL;

ALTER TABLE openrails.rail_mutation_logs
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT rail_mutation_logs_addressed
    CHECK (psp_id IS NOT NULL OR custodian_id IS NOT NULL);

COMMENT ON COLUMN openrails.rail_mutation_logs.psp_id IS
    'PSP the logged mutation was addressed to. Required unless the mutation is custodian-addressed (rail_mutation_logs_addressed) — or#893/or#795.';
