-- #813 (implements #778's sketch): cross-product plan migration as the
-- generalization of #773's reprice engine — ONE scheduled-change workflow.
--
-- subscription_reprices grows a kind ('reprice' = #773 same-product price
-- move; 'plan_change' = cross-product migration with entitlement cutover at
-- the same boundary) and a 'blocked' terminal status for rows the engine
-- cannot auto-migrate (rail requires user action / missing rail config), so
-- a bulk migration's per-subscription ledger is complete — nothing silently
-- escapes the cohort.

ALTER TABLE openrails.subscription_reprices
    ADD COLUMN kind text DEFAULT 'reprice' NOT NULL,
    ADD COLUMN blocked_reason text DEFAULT '' NOT NULL;

ALTER TABLE openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_kind_chk
        CHECK (kind = ANY (ARRAY['reprice'::text, 'plan_change'::text]));

ALTER TABLE openrails.subscription_reprices
    DROP CONSTRAINT subscription_reprices_status_chk;
ALTER TABLE openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_status_chk
        CHECK (status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text, 'blocked'::text]));

ALTER TABLE openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_blocked_has_reason
        CHECK (status <> 'blocked' OR blocked_reason <> '');

COMMENT ON COLUMN openrails.subscription_reprices.kind IS '#813: ''reprice'' = #773 same-product price move; ''plan_change'' = cross-product migration — the renewal-boundary pickup also moves product_id and cuts entitlement/credit snapshots over.';
COMMENT ON COLUMN openrails.subscription_reprices.blocked_reason IS '#813: why this row could not be auto-scheduled (rail_requires_user_action, missing rail config, rail push failure). Only set when status=blocked.';

-- reprice_batches grows the plan-migration header fields: what was migrated
-- FROM, the operator's fallback for rails that cannot be auto-migrated, and a
-- blocked counter so batch totals reconcile (matched = scheduled + skipped +
-- blocked).
ALTER TABLE openrails.reprice_batches
    ADD COLUMN kind text DEFAULT 'reprice' NOT NULL,
    ADD COLUMN source_price_id uuid,
    ADD COLUMN fallback_policy text DEFAULT '' NOT NULL,
    ADD COLUMN subscriptions_blocked integer DEFAULT 0 NOT NULL;

ALTER TABLE openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_kind_chk
        CHECK (kind = ANY (ARRAY['reprice'::text, 'plan_change'::text]));

ALTER TABLE openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_fallback_chk
        CHECK (fallback_policy = ANY (ARRAY[''::text, 'keep_grandfathered'::text, 'cancel_at_period_end'::text]));

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_source_price_fk FOREIGN KEY (source_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

COMMENT ON COLUMN openrails.reprice_batches.source_price_id IS '#813: the retired plan''s price for a plan_change batch (the cohort selector); NULL for #773 price-key batches.';
COMMENT ON COLUMN openrails.reprice_batches.fallback_policy IS '#813: operator''s choice for subscriptions on rails that cannot be auto-migrated (ccbill/solana): keep_grandfathered leaves them billing the archived source; cancel_at_period_end schedules their cancellation.';
