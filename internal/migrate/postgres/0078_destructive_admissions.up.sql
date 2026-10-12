-- parent: 77 sha256:4fb973340dd814d572e6cee45cec5e31cee9380669951fc02918633f0b083638
-- Repair: none-needed New durable destructive command admissions and incident episodes; no historical operation is reconstructed.
CREATE TABLE billing.destructive_actions (
 merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
 id uuid NOT NULL DEFAULT uuidv7(),
 admission_sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
 kind text NOT NULL CHECK (kind IN ('refund','cancel_subscription','delete_payment_method','revoke_credit','revoke_access','waive_order','mark_uncollectible','catalog_change','configuration_change','account_deletion')),
 target text NOT NULL CHECK (btrim(target) <> '' AND octet_length(target)<=2048),
 action_key text NOT NULL CHECK (btrim(action_key) <> '' AND octet_length(action_key)<=2048),
 request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
 origin text NOT NULL CHECK (origin IN ('staff','host')),
 actor_issuer text NOT NULL CHECK (btrim(actor_issuer) <> ''),
 actor_id text NOT NULL CHECK (btrim(actor_id) <> ''),
 actor_key text NOT NULL CHECK (actor_key ~ '^[0-9a-f]{64}$'),
 credential text CHECK (credential <> ''),
 units bigint NOT NULL CHECK (units>0 AND units<=1000000000),
 admitted_at timestamptz NOT NULL,
 state text NOT NULL CHECK (state IN ('reserved','started','succeeded','no_effect','uncertain')),
 started_at timestamptz,
 finished_at timestamptz,
 result text CHECK (result <> '' AND octet_length(result)<=2048),
 PRIMARY KEY(merchant_id,id),
 CONSTRAINT destructive_actions_lifecycle_check CHECK (
   (state='reserved' AND started_at IS NULL AND finished_at IS NULL)
   OR (state IN ('started','uncertain') AND started_at IS NOT NULL AND finished_at IS NULL)
   OR (state='succeeded' AND started_at IS NOT NULL AND finished_at IS NOT NULL)
   OR (state='no_effect' AND finished_at IS NOT NULL))
);
CREATE UNIQUE INDEX destructive_actions_live_key ON billing.destructive_actions(merchant_id,action_key) WHERE state<>'no_effect';
CREATE INDEX destructive_actions_key_history_idx ON billing.destructive_actions(merchant_id,action_key,admitted_at DESC,id DESC);
CREATE INDEX destructive_actions_actor_recent_idx ON billing.destructive_actions(actor_key,admitted_at) INCLUDE (units) WHERE state='succeeded';
CREATE INDEX destructive_actions_merchant_recent_idx ON billing.destructive_actions(merchant_id,admitted_at) INCLUDE (units) WHERE state='succeeded';
CREATE INDEX destructive_actions_recent_idx ON billing.destructive_actions(admitted_at) INCLUDE (units) WHERE state='succeeded';
CREATE INDEX destructive_actions_pending_idx ON billing.destructive_actions(merchant_id,actor_key) INCLUDE (units) WHERE state IN ('reserved','started','uncertain');
CREATE TRIGGER destructive_actions_immutable BEFORE UPDATE ON billing.destructive_actions
 FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('state','started_at','finished_at','result');
CREATE FUNCTION billing.guard_destructive_action_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.state IN ('succeeded','no_effect') AND NEW IS DISTINCT FROM OLD THEN
   RAISE EXCEPTION 'destructive action is final' USING ERRCODE='23514';
 END IF;
 IF OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at THEN
   RAISE EXCEPTION 'destructive action start is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state <> 'reserved' AND NEW.state='reserved' THEN
   RAISE EXCEPTION 'destructive action cannot be unstarted' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER destructive_actions_state BEFORE UPDATE ON billing.destructive_actions
 FOR EACH ROW EXECUTE FUNCTION billing.guard_destructive_action_state();
CREATE TRIGGER destructive_actions_no_delete BEFORE DELETE ON billing.destructive_actions
 FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
COMMENT ON TABLE billing.destructive_actions IS 'Admitted destructive command audit, immutable authority and admission facts with explicit outcome; not a counter or request-body store. Proven no-effect retries create another immutable attempt. Retention: permanent, never pruned.';

CREATE TABLE billing.destructive_holds (
 merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
 id uuid NOT NULL DEFAULT uuidv7(),
 scope text NOT NULL CHECK (scope IN ('actor','merchant','deployment')),
 scope_key text NOT NULL CHECK (btrim(scope_key)<>''),
 actor_issuer text NOT NULL CHECK (btrim(actor_issuer)<>''),
 actor_id text NOT NULL CHECK (btrim(actor_id)<>''),
 credential text CHECK (credential<>''),
 action_kind text NOT NULL,
 target text NOT NULL CHECK (btrim(target)<>''),
 window_name text NOT NULL CHECK (window_name IN ('hour','day')),
 observed bigint NOT NULL CHECK (observed>=0),
 requested bigint NOT NULL CHECK (requested>0),
 ceiling bigint NOT NULL CHECK (ceiling>0),
 opened_at timestamptz NOT NULL,
 cleared_at timestamptz,
 clear_through_sequence bigint,
 cleared_by_issuer text,
 cleared_by_id text,
 clear_reason text,
 PRIMARY KEY(merchant_id,id),
 CONSTRAINT destructive_holds_clear_check CHECK (
  (cleared_at IS NULL AND clear_through_sequence IS NULL AND cleared_by_issuer IS NULL AND cleared_by_id IS NULL AND clear_reason IS NULL)
  OR (cleared_at IS NOT NULL AND clear_through_sequence IS NOT NULL AND clear_through_sequence>=0 AND cleared_by_issuer IS NOT NULL AND btrim(cleared_by_issuer)<>'' AND cleared_by_id IS NOT NULL AND btrim(cleared_by_id)<>'' AND clear_reason IS NOT NULL AND btrim(clear_reason)<>''
      AND (cleared_by_issuer,cleared_by_id) IS DISTINCT FROM (actor_issuer,actor_id)))
);
CREATE UNIQUE INDEX destructive_holds_open_scope_key ON billing.destructive_holds(scope_key) WHERE cleared_at IS NULL;
CREATE INDEX destructive_holds_epoch_idx ON billing.destructive_holds(scope_key,cleared_at DESC) WHERE cleared_at IS NOT NULL;
CREATE INDEX destructive_holds_merchant_idx ON billing.destructive_holds(merchant_id,id);
CREATE TRIGGER destructive_holds_immutable BEFORE UPDATE ON billing.destructive_holds
 FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('cleared_at','clear_through_sequence','cleared_by_issuer','cleared_by_id','clear_reason');
CREATE FUNCTION billing.guard_destructive_hold_clear() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.cleared_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
   RAISE EXCEPTION 'destructive hold clear is final' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER destructive_holds_clear BEFORE UPDATE ON billing.destructive_holds
 FOR EACH ROW EXECUTE FUNCTION billing.guard_destructive_hold_clear();
CREATE TRIGGER destructive_holds_no_delete BEFORE DELETE ON billing.destructive_holds
 FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
COMMENT ON TABLE billing.destructive_holds IS 'Deployment-authoritative destructive incident episodes. Merchant_id identifies only the triggering tenant; actor/deployment holds span tenants. An audited clear ends one episode, never an ordinary finding resolution. Retention: permanent, never pruned.';

ALTER TABLE billing.provider_intents ADD COLUMN destructive_action_id uuid;
ALTER TABLE billing.provider_intents ADD CONSTRAINT provider_intents_destructive_action_fkey
 FOREIGN KEY(merchant_id,destructive_action_id) REFERENCES billing.destructive_actions(merchant_id,id) ON DELETE RESTRICT;
CREATE INDEX provider_intents_destructive_action_idx ON billing.provider_intents(merchant_id,destructive_action_id) WHERE destructive_action_id IS NOT NULL;
COMMENT ON COLUMN billing.provider_intents.destructive_action_id IS 'Original admitted command; recovery and necessary consequences reuse its authority and never spend another unit.';

CREATE INDEX subscriptions_destructive_scale_idx ON billing.subscriptions(merchant_id,customer_id,price_id) WHERE status IN ('active','past_due') AND deleted_at IS NULL;
CREATE INDEX payments_destructive_scale_idx ON billing.payments(purchased_at,merchant_id,customer_id) WHERE status='succeeded' AND amount>0 AND refunded_payment_id IS NULL AND deleted_at IS NULL;

ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_currency_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_currency_check CHECK ((currency IS NULL) = (event_type IN ('product.entitlements_changed', 'customer.network_linked', 'customer.network_unlinked', 'mandate.revoked', 'destructive.hold_opened', 'destructive.hold_cleared')) AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'));
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared', 'balance.low', 'balance.depleted')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired', 'order.voided')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'mandate.revoked' AND subject_type = 'mandate' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('destructive.hold_opened','destructive.hold_cleared') AND subject_type='destructive_hold' AND payment_id IS NULL AND amount IS NULL AND currency IS NULL AND jsonb_typeof(data)='object')
);
