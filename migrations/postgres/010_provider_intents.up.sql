-- #358 phase A: provider intent ledger.
--
-- openrails.provider_intents is the durable, effectively-once outbox for ALL
-- outbound provider mutations. Every action OpenRails wants to perform against
-- an external payment provider is recorded here first; a scheduled executor
-- worker claims due intents (SKIP LOCKED lease), gates them on operating mode
-- + origin, executes, and classifies the outcome. Ambiguous outcomes park as
-- unknown_needs_verify and are resolved by a verifier worker via provider
-- READS before any retry. Failure reasons (mode, kill switch, provider down,
-- bad creds) are recorded on the intent — they are reasons an intent stays
-- pending, never errors.
--
-- Status lifecycle:
--   pending              -> due for execution at next_attempt_at
--   in_flight            -> claimed by an executor (lease = claimed_until)
--   succeeded            -> executed effectively-once (result_evidence says how)
--   unknown_needs_verify -> outcome ambiguous; verifier resolves via reads
--   failed_retryable     -> failed cleanly; retried at next_attempt_at (backoff)
--   failed_terminal      -> a handler classified the failure as unretryable
--   superseded           -> no longer applicable (e.g. delete after a resume)
--   expired              -> relevance window (expires_at) elapsed unexecuted
--
-- Effectively-once, per class: deletes/cancels are verify-then-execute
-- (absent = success, blind retry harmless); money-movers park ambiguity for
-- the verifier and are never blind-retried; creates are content-addressed
-- find-or-create. idempotency_key dedupes enqueues per tenant.
--
-- Tenancy mirrors the neighboring billing tables: tenant_id stamped NOT NULL
-- with the single-tenant default, RLS tenant_isolation policy on the
-- app.tenant_id GUC, FORCE ROW LEVEL SECURITY. The executor/verifier workers
-- run on the privileged (no-GUC) pool and sweep cross-tenant, like the other
-- River workers.

CREATE TABLE openrails.provider_intents (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    provider text NOT NULL,
    intent_type text NOT NULL,
    subscription_id uuid,
    payment_id uuid,
    price_id uuid,
    payload jsonb,
    idempotency_key text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    claimed_until timestamp with time zone,
    origin text NOT NULL,
    origin_reason text,
    last_failure_reason text,
    expires_at timestamp with time zone,
    result_evidence jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    executed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT provider_intents_pkey PRIMARY KEY (id),
    CONSTRAINT chk_provider_intents_status CHECK ((status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'succeeded'::text, 'unknown_needs_verify'::text, 'failed_retryable'::text, 'failed_terminal'::text, 'superseded'::text, 'expired'::text]))),
    CONSTRAINT chk_provider_intents_origin CHECK ((origin = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text]))),
    CONSTRAINT chk_provider_intents_executed CHECK ((status <> 'succeeded'::text OR executed_at IS NOT NULL))
);

ALTER TABLE ONLY openrails.provider_intents FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.provider_intents IS 'Durable, effectively-once outbox for outbound provider mutations (#358). One row per logical intent (unique per tenant on idempotency_key); the executor worker drains whatever is currently executable, the verifier resolves ambiguous outcomes via provider reads.';
COMMENT ON COLUMN openrails.provider_intents.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN openrails.provider_intents.provider IS 'Provider/processor key the mutation targets (e.g. ''mobius'' for an NMI-backed processor, ''stripe'').';
COMMENT ON COLUMN openrails.provider_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff): nmi_delete_subscription, and in later phases manual_rebill, refund, plan_archive, ...';
COMMENT ON COLUMN openrails.provider_intents.idempotency_key IS 'Deterministic identity of the logical intent within the tenant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';
COMMENT ON COLUMN openrails.provider_intents.claimed_until IS 'Single-executor lease (SKIP LOCKED claim). An in_flight row whose lease elapsed was orphaned by a crashed executor and becomes claimable again; per-type execute semantics (verify-then-execute, verifier-before-retry) make the reclaim safe.';
COMMENT ON COLUMN openrails.provider_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';
COMMENT ON COLUMN openrails.provider_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';
COMMENT ON COLUMN openrails.provider_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';
COMMENT ON COLUMN openrails.provider_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';

CREATE UNIQUE INDEX uq_provider_intents_tenant_idempotency_key ON openrails.provider_intents USING btree (tenant_id, idempotency_key);

CREATE INDEX idx_provider_intents_tenant_id ON openrails.provider_intents USING btree (tenant_id);

CREATE INDEX idx_provider_intents_due ON openrails.provider_intents USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text]));

CREATE INDEX idx_provider_intents_subscription ON openrails.provider_intents USING btree (subscription_id) WHERE (subscription_id IS NOT NULL);

ALTER TABLE openrails.provider_intents ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON openrails.provider_intents USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_intents TO openrails_app;
