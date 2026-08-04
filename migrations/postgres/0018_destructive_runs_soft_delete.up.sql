-- or#858 / or#859 tier 1: `pull-provider --prune` stops hard-deleting, and the
-- general ledger of reversible destructive runs is born with it.
--
-- A prune acts on ABSENCE — "the provider did not list this row" — the weakest
-- evidence class we act on at all. A hard DELETE turns a bad snapshot (a
-- credential rotated onto a sibling gateway account, an incident returning an
-- empty first page) into permanent data loss with no undo. So a prune now
-- SOFT-deletes, and stamps every row it touches with the run that took it:
--
--   deleted_at          the row is gone as far as every live read is concerned.
--   destructive_run_id  which run took it. Rollback is
--                       `SET deleted_at = NULL WHERE destructive_run_id = $1` —
--                       one statement per table, not row-by-row surgery.
--
-- The run table is deliberately the GENERAL one (or#859 §5.1): kind='prune' is
-- its first user, and converge_enforce / declared_import / plan_migration /
-- catalog_push / merchant_delete adopt the same shape without a migration.
--
-- entitlements already carries deleted_at (ordinary revocation uses it, and its
-- no-overlap GiST exclusion and uniques already exclude deleted rows); it needs
-- only the run stamp. Restoring an entitlement whose window has since been
-- re-granted therefore conflicts on that exclusion and aborts the whole
-- rollback transaction — loudly, which is correct.

CREATE TABLE openrails.destructive_runs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    -- NULL = merchant-wide (a declared import, a catalog edit). --prune is
    -- account-bound and always sets it.
    psp_id uuid,
    kind text NOT NULL,
    actor text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    dry_run boolean DEFAULT false NOT NULL,
    -- The absence proof that authorised it (SnapshotCoverage), verbatim. A
    -- destructive run is only as trustworthy as its proof, so the proof is
    -- stored WITH the run rather than left in a log file nobody goes and finds.
    coverage jsonb,
    -- The operator's typed confirmation.
    expected_rows bigint,
    -- Per-table actual counts.
    affected jsonb,
    reversed_at timestamp with time zone,
    reversed_by text,
    status text DEFAULT 'running' NOT NULL,
    note text,
    CONSTRAINT chk_destructive_runs_status CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text, 'reversed'::text]))),
    CONSTRAINT chk_destructive_runs_expected_rows CHECK ((expected_rows IS NULL OR expected_rows >= 0))
);

COMMENT ON TABLE openrails.destructive_runs IS 'or#858/or#859 tier 1: every destructive operation is an attributable, scoped, stamped unit of damage with a single-command undo. Rows a run destroyed carry its id (destructive_run_id); `openrails prune rollback --run <id>` reverses one. kind=prune is the first user; converge_enforce / declared_import / plan_migration / catalog_push / merchant_delete follow.';
COMMENT ON COLUMN openrails.destructive_runs.coverage IS 'The SnapshotCoverage absence proof that authorised the run, verbatim — the guard that should have stopped an empty-roster mass cancellation, made auditable after the fact rather than only preventive.';
COMMENT ON COLUMN openrails.destructive_runs.expected_rows IS 'The operator''s typed confirmation. A run whose discovered row count differs refuses before writing anything.';
COMMENT ON COLUMN openrails.destructive_runs.status IS 'running = stamped rows may exist but the run did not finish (crash/abort); a rollback still reverses it, which is why rows are stamped before they are written.';

ALTER TABLE ONLY openrails.destructive_runs
    ADD CONSTRAINT destructive_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.destructive_runs
    ADD CONSTRAINT destructive_runs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_destructive_runs_merchant_id ON openrails.destructive_runs USING btree (merchant_id);
CREATE INDEX idx_destructive_runs_merchant_started ON openrails.destructive_runs USING btree (merchant_id, started_at DESC);
CREATE INDEX idx_destructive_runs_merchant_kind_started ON openrails.destructive_runs USING btree (merchant_id, kind, started_at DESC);

ALTER TABLE openrails.destructive_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.destructive_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.destructive_runs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- The ledger of damage is itself append-only-ish: no DELETE, and UPDATE only on
-- the columns a run legitimately advances. Nobody edits away the record of what
-- they did.
GRANT SELECT,INSERT ON TABLE openrails.destructive_runs TO openrails_app;
GRANT UPDATE (finished_at, status, affected, reversed_at, reversed_by, note) ON TABLE openrails.destructive_runs TO openrails_app;

-- --- the soft-delete columns -------------------------------------------------

ALTER TABLE openrails.subscriptions
    ADD COLUMN deleted_at timestamp with time zone,
    ADD COLUMN destructive_run_id uuid;

ALTER TABLE openrails.payments
    ADD COLUMN deleted_at timestamp with time zone,
    ADD COLUMN destructive_run_id uuid;

ALTER TABLE openrails.checkout_sessions
    ADD COLUMN deleted_at timestamp with time zone,
    ADD COLUMN destructive_run_id uuid;

ALTER TABLE openrails.entitlements
    ADD COLUMN destructive_run_id uuid;

COMMENT ON COLUMN openrails.subscriptions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `prune rollback` clears it.';
COMMENT ON COLUMN openrails.payments.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `prune rollback` clears it.';
COMMENT ON COLUMN openrails.checkout_sessions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `prune rollback` clears it.';

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_destructive_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_destructive_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_destructive_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_destructive_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;

-- Rollback is keyed on the run stamp, and only a vanishing fraction of rows
-- ever carry one — partial indexes, so the cost is proportional to prunes, not
-- to records on file.
CREATE INDEX idx_subscriptions_destructive_run ON openrails.subscriptions USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_payments_destructive_run ON openrails.payments USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_checkout_sessions_destructive_run ON openrails.checkout_sessions USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_entitlements_destructive_run ON openrails.entitlements USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);

-- --- uniqueness must ignore soft-deleted rows --------------------------------
--
-- A hard DELETE freed the unique key; a soft delete does not. Without this, a
-- subscription pruned by a bad snapshot and then re-listed by the provider
-- would fail to re-insert with a duplicate-key error naming a row nobody can
-- see. Every unique index on a soft-deletable table is therefore partial on
-- `deleted_at IS NULL`. (entitlements' uniques and its no-overlap exclusion
-- constraint already carry the predicate.)

DROP INDEX IF EXISTS openrails.uq_subscriptions_customer_product_lifecycle;
CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle
    ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id)
    WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status, 'past_due'::openrails.subscription_status])) AND (deleted_at IS NULL));

DROP INDEX IF EXISTS openrails.uq_subscriptions_customer_tier_group_active;
CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active
    ON openrails.subscriptions USING btree (customer_id, tier_group)
    WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status])) AND (tier_group IS NOT NULL) AND (deleted_at IS NULL));

DROP INDEX IF EXISTS openrails.uq_subscriptions_merchant_psp_subscription_id;
CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id
    ON openrails.subscriptions USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        rail_subscription_id
    )
    WHERE ((rail_subscription_id <> ''::text) AND (deleted_at IS NULL));

DROP INDEX IF EXISTS openrails.uq_payments_merchant_psp_transaction;
CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction
    ON openrails.payments USING btree (
        merchant_id,
        rail,
        COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid),
        transaction_id
    )
    WHERE (deleted_at IS NULL);

DROP INDEX IF EXISTS openrails.checkout_sessions_rail_reference_idx;
CREATE UNIQUE INDEX checkout_sessions_rail_reference_idx
    ON openrails.checkout_sessions USING btree (rail, reference)
    WHERE ((reference IS NOT NULL) AND (deleted_at IS NULL));

DROP INDEX IF EXISTS openrails.checkout_sessions_rail_transaction_id_idx;
CREATE UNIQUE INDEX checkout_sessions_rail_transaction_id_idx
    ON openrails.checkout_sessions USING btree (rail, transaction_id)
    WHERE ((transaction_id IS NOT NULL) AND (deleted_at IS NULL));

-- --- the analytics views read these tables too --------------------------------
--
-- freeloader_episodes / orphaned_episodes are ordinary live reads dressed as
-- views: they drive the #690 gauges. Redefined here so a pruned subscription or
-- payment stops contributing coverage — a soft delete that leaks into a gauge
-- is a silently wrong number, which is the failure mode this whole change
-- exists to prevent.

CREATE OR REPLACE VIEW openrails.freeloader_episodes WITH (security_invoker='true') AS
 WITH win AS (
         SELECT e.merchant_id,
            e.customer_id,
            e.id AS entitlement_id,
            e.entitlement,
            e.source_type,
            e.source_id,
            e.start_at,
            LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone)) AS window_end,
            s.status AS sub_status,
            s.next_retry_at,
            GREATEST(s.current_period_ends_at, s.ended_at) AS paid_through,
            p.id AS payment_id,
            p.status AS payment_status,
            COALESCE(( SELECT max(r.purchased_at) AS max
                   FROM openrails.payments r
                  WHERE ((r.merchant_id = e.merchant_id) AND (r.refunded_payment_id = p.id) AND (r.deleted_at IS NULL))), p.purchased_at) AS refund_effective_at,
            ( SELECT max(COALESCE(g.ends_at, 'infinity'::timestamp with time zone)) AS max
                   FROM openrails.grants g
                  WHERE ((g.merchant_id = e.merchant_id) AND (g.customer_id = e.customer_id) AND (g.event = 'grant'::text) AND (g.kind = 'entitlement'::text) AND (g.starts_at <= now()) AND ((g.id = e.grant_id) OR ((g.source_id = (e.source_id)::text) AND (((e.source_type = 'subscription'::text) AND (g.source_type = 'subscription'::text)) OR ((e.source_type = 'one_off'::text) AND (g.source_type = 'purchase'::text))))) AND (NOT (EXISTS ( SELECT 1
                           FROM openrails.grants t
                          WHERE ((t.merchant_id = g.merchant_id) AND (t.supersedes_id = g.id) AND (t.event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])))))))) AS grant_covered_until
           FROM ((openrails.entitlements e
             LEFT JOIN openrails.subscriptions s ON (((e.source_type = 'subscription'::text) AND (s.id = e.source_id) AND (s.merchant_id = e.merchant_id) AND (s.deleted_at IS NULL))))
             LEFT JOIN openrails.payments p ON (((e.source_type = 'one_off'::text) AND (p.id = e.source_id) AND (p.merchant_id = e.merchant_id) AND (p.deleted_at IS NULL))))
          WHERE (e.source_type = ANY (ARRAY['subscription'::text, 'one_off'::text]))
        ), spans AS (
         SELECT w.merchant_id,
            w.customer_id,
            w.entitlement_id,
            w.entitlement,
            w.source_type,
            w.source_id,
            w.start_at,
            w.window_end,
            w.sub_status,
            w.next_retry_at,
            w.paid_through,
            w.payment_id,
            w.payment_status,
            w.refund_effective_at,
            w.grant_covered_until,
            GREATEST(w.start_at,
                CASE
                    WHEN (w.source_type = 'subscription'::text) THEN COALESCE(w.paid_through, '-infinity'::timestamp with time zone)
                    WHEN (w.payment_status = 'completed'::openrails.payment_status) THEN 'infinity'::timestamp with time zone
                    WHEN (w.payment_status = 'refunded'::openrails.payment_status) THEN w.refund_effective_at
                    ELSE '-infinity'::timestamp with time zone
                END, COALESCE(w.grant_covered_until, '-infinity'::timestamp with time zone)) AS unpaid_from,
            LEAST(w.window_end, now()) AS unpaid_until
           FROM win w
        )
 SELECT merchant_id,
    customer_id,
    entitlement_id,
    entitlement,
    source_type,
    source_id,
        CASE
            WHEN ((sub_status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL)) THEN 'sanctioned_dunning'::text
            WHEN (sub_status = 'unknown'::openrails.subscription_status) THEN 'awaiting_verification'::text
            ELSE 'unsanctioned'::text
        END AS cause,
    unpaid_from AS started_at,
    unpaid_until AS ended_at,
    (window_end > now()) AS open,
    ((EXTRACT(epoch FROM (unpaid_until - unpaid_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (unpaid_from < unpaid_until);

CREATE OR REPLACE VIEW openrails.orphaned_episodes WITH (security_invoker='true') AS
 WITH cov AS (
         SELECT s.merchant_id,
            s.customer_id,
            'subscription'::text AS source_type,
            s.id AS source_id,
            s.product_id,
            COALESCE(s.current_period_starts_at, s.started_at) AS cov_start,
            GREATEST(s.current_period_ends_at, s.ended_at) AS cov_end
           FROM (openrails.subscriptions s
             JOIN openrails.products pd ON (((pd.id = s.product_id) AND (pd.merchant_id = s.merchant_id))))
          WHERE ((s.deleted_at IS NULL) AND (s.status <> 'pending'::openrails.subscription_status) AND (GREATEST(s.current_period_ends_at, s.ended_at) IS NOT NULL) AND (((pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb)) OR ((s.entitlements_spec_snapshot IS NOT NULL) AND (s.entitlements_spec_snapshot <> '{}'::jsonb))))
        UNION ALL
         SELECT p.merchant_id,
            p.customer_id,
            'one_off'::text AS text,
            p.id,
            pr.product_id,
            p.purchased_at,
            (p.purchased_at + make_interval(hours => pr.access_duration_hours))
           FROM ((openrails.payments p
             JOIN openrails.prices pr ON (((pr.id = p.price_id) AND (pr.merchant_id = p.merchant_id))))
             JOIN openrails.products pd ON (((pd.id = pr.product_id) AND (pd.merchant_id = p.merchant_id))))
          WHERE ((p.deleted_at IS NULL) AND (p.status = 'completed'::openrails.payment_status) AND (p.amount > 0) AND (p.subscription_id IS NULL) AND (pr.access_duration_hours IS NOT NULL) AND (pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb))
        ), spans AS (
         SELECT c.merchant_id,
            c.customer_id,
            c.source_type,
            c.source_id,
            c.product_id,
            c.cov_start,
            c.cov_end,
            GREATEST(c.cov_start, COALESCE(( SELECT max(LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone))) AS max
                   FROM openrails.entitlements e
                  WHERE ((e.merchant_id = c.merchant_id) AND (e.customer_id = c.customer_id) AND (e.source_type = c.source_type) AND (e.source_id = c.source_id) AND (e.start_at <= now()))), '-infinity'::timestamp with time zone)) AS uncovered_from,
            LEAST(c.cov_end, now()) AS uncovered_until
           FROM cov c
        )
 SELECT merchant_id,
    customer_id,
    source_type,
    source_id,
    product_id,
    uncovered_from AS started_at,
    uncovered_until AS ended_at,
    (cov_end > now()) AS open,
    ((EXTRACT(epoch FROM (uncovered_until - uncovered_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (uncovered_from < uncovered_until);
