-- th-045: provider-neutral billing observations are append-only evidence for
-- one operation authorization. OpenRails owns qualification and settlement;
-- provider adapters only supply exact normalized facts and bounded raw bytes.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.provider_billing_qualifications (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    provider text NOT NULL,
    provider_resource_id text NOT NULL,
    provider_lifetime_start timestamptz NOT NULL,
    provider_lifetime_end timestamptz NOT NULL,
    provider_absent_at timestamptz NOT NULL,
    provider_absence_reference text NOT NULL,
    billing_stop_reference text NOT NULL,
    windows_closed_at timestamptz NOT NULL,
    windows_closed_reference text NOT NULL,
    lifecycle_evidence_bytes bytea NOT NULL,
    lifecycle_evidence_digest bytea NOT NULL,
    quiescence_seconds bigint NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    reason text NOT NULL DEFAULT 'awaiting_equal_observation',
    baseline_observation_id text,
    qualified_observation_id text,
    qualified_provider_cost_usd_micros bigint,
    qualified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT provider_billing_qualifications_pkey
        PRIMARY KEY (merchant_id, operation_id),
    CONSTRAINT provider_billing_qualification_operation_fk
        FOREIGN KEY (merchant_id, operation_id)
        REFERENCES openrails.operation_authorizations(merchant_id, operation_id)
        ON DELETE RESTRICT,
    CONSTRAINT provider_billing_qualification_provider_shape CHECK (
        provider <> '' AND provider = btrim(provider)
        AND octet_length(provider) <= 255
        AND provider_resource_id <> '' AND provider_resource_id = btrim(provider_resource_id)
        AND octet_length(provider_resource_id) <= 255
    ),
    CONSTRAINT provider_billing_qualification_lifetime_shape CHECK (
        provider_lifetime_end >= provider_lifetime_start
        AND provider_absent_at >= provider_lifetime_end
        AND windows_closed_at >= provider_lifetime_end
    ),
    CONSTRAINT provider_billing_qualification_reference_shape CHECK (
        provider_absence_reference <> ''
        AND provider_absence_reference = btrim(provider_absence_reference)
        AND octet_length(provider_absence_reference) <= 1024
        AND billing_stop_reference <> ''
        AND billing_stop_reference = btrim(billing_stop_reference)
        AND octet_length(billing_stop_reference) <= 1024
        AND windows_closed_reference <> ''
        AND windows_closed_reference = btrim(windows_closed_reference)
        AND octet_length(windows_closed_reference) <= 1024
    ),
    CONSTRAINT provider_billing_qualification_evidence_shape CHECK (
        octet_length(lifecycle_evidence_bytes) BETWEEN 1 AND 65536
        AND octet_length(lifecycle_evidence_digest) = 32
        AND lifecycle_evidence_digest = public.digest(lifecycle_evidence_bytes, 'sha256')
    ),
    CONSTRAINT provider_billing_qualification_policy_shape CHECK (
        quiescence_seconds > 0
    ),
    CONSTRAINT provider_billing_qualification_state_shape CHECK (
        state IN ('pending', 'refused', 'eligible')
        AND reason IN (
            'awaiting_equal_observation',
            'awaiting_quiescence',
            'coverage_incomplete',
            'observation_changed',
            'provider_evidence_refused',
            'negative_or_corrective_record',
            'decreasing_provider_cost',
            'eligible'
        )
        AND (baseline_observation_id IS NULL OR (
            baseline_observation_id <> ''
            AND baseline_observation_id = btrim(baseline_observation_id)
            AND octet_length(baseline_observation_id) <= 255
        ))
        AND (qualified_observation_id IS NULL OR (
            qualified_observation_id <> ''
            AND qualified_observation_id = btrim(qualified_observation_id)
            AND octet_length(qualified_observation_id) <= 255
        ))
        AND (
            (state = 'pending'
             AND reason IN ('awaiting_equal_observation', 'awaiting_quiescence', 'coverage_incomplete', 'observation_changed')
             AND qualified_observation_id IS NULL
             AND qualified_provider_cost_usd_micros IS NULL
             AND qualified_at IS NULL)
            OR
            (state = 'refused'
             AND reason IN ('provider_evidence_refused', 'negative_or_corrective_record', 'decreasing_provider_cost')
             AND qualified_observation_id IS NULL
             AND qualified_provider_cost_usd_micros IS NULL
             AND qualified_at IS NULL)
            OR
            (state = 'eligible'
             AND reason = 'eligible'
             AND baseline_observation_id IS NOT NULL
             AND qualified_observation_id IS NOT NULL
             AND qualified_provider_cost_usd_micros IS NOT NULL
             AND qualified_provider_cost_usd_micros >= 0
             AND qualified_at IS NOT NULL)
        )
    )
);

CREATE TABLE openrails.provider_billing_observations (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    observation_id text NOT NULL,
    normalized_query text NOT NULL,
    query_start timestamptz NOT NULL,
    query_end timestamptz NOT NULL,
    raw_body_available boolean NOT NULL,
    raw_body_bytes bytea NOT NULL,
    raw_body_digest bytea NOT NULL,
    normalized_records_bytes bytea,
    normalized_records_digest bytea,
    provider_cost_usd_micros bigint,
    has_negative_record boolean NOT NULL DEFAULT false,
    refusal_kind text,
    covers_lifetime boolean NOT NULL,
    qualification_reason text NOT NULL,
    observed_at timestamptz NOT NULL,
    CONSTRAINT provider_billing_observations_pkey
        PRIMARY KEY (merchant_id, operation_id, observation_id),
    CONSTRAINT provider_billing_observation_qualification_fk
        FOREIGN KEY (merchant_id, operation_id)
        REFERENCES openrails.provider_billing_qualifications(merchant_id, operation_id)
        ON DELETE RESTRICT,
    CONSTRAINT provider_billing_observation_id_shape CHECK (
        observation_id <> '' AND observation_id = btrim(observation_id)
        AND octet_length(observation_id) <= 255
    ),
    CONSTRAINT provider_billing_observation_query_shape CHECK (
        normalized_query <> '' AND normalized_query = btrim(normalized_query)
        AND octet_length(normalized_query) <= 8192
        AND query_end > query_start
    ),
    CONSTRAINT provider_billing_observation_raw_shape CHECK (
        octet_length(raw_body_bytes) <= 16777216
        AND octet_length(raw_body_digest) = 32
        AND raw_body_digest = public.digest(raw_body_bytes, 'sha256')
        AND (raw_body_available OR octet_length(raw_body_bytes) = 0)
    ),
    CONSTRAINT provider_billing_observation_normalized_shape CHECK (
        (
            refusal_kind IS NULL
            AND raw_body_available
            AND octet_length(raw_body_bytes) > 0
            AND normalized_records_bytes IS NOT NULL
            AND octet_length(normalized_records_bytes) > 0
            AND octet_length(normalized_records_bytes) <= 16777216
            AND normalized_records_digest IS NOT NULL
            AND octet_length(normalized_records_digest) = 32
            AND normalized_records_digest = public.digest(normalized_records_bytes, 'sha256')
            AND provider_cost_usd_micros IS NOT NULL
        )
        OR
        (
            refusal_kind IS NOT NULL
            AND refusal_kind <> ''
            AND refusal_kind = btrim(refusal_kind)
            AND octet_length(refusal_kind) <= 255
            AND normalized_records_bytes IS NULL
            AND normalized_records_digest IS NULL
            AND provider_cost_usd_micros IS NULL
            AND NOT has_negative_record
            AND NOT covers_lifetime
            AND qualification_reason = 'provider_evidence_refused'
        )
    ),
    CONSTRAINT provider_billing_observation_reason_shape CHECK (
        qualification_reason IN (
            'awaiting_equal_observation',
            'awaiting_quiescence',
            'coverage_incomplete',
            'observation_changed',
            'provider_evidence_refused',
            'negative_or_corrective_record',
            'decreasing_provider_cost',
            'eligible'
        )
    )
);

CREATE INDEX idx_provider_billing_observations_operation_time
    ON openrails.provider_billing_observations (merchant_id, operation_id, observed_at DESC);

ALTER TABLE openrails.provider_billing_qualifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE openrails.provider_billing_qualifications FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.provider_billing_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE openrails.provider_billing_observations FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.provider_billing_qualifications
    USING (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid);

CREATE POLICY merchant_isolation ON openrails.provider_billing_observations
    USING (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid);

COMMENT ON TABLE openrails.provider_billing_qualifications IS
    'OpenRails-owned th-045 post-absence qualification state for one operation authorization. Eligible is an operator quiescence policy fact, never provider-attested finality.';

COMMENT ON TABLE openrails.provider_billing_observations IS
    'Append-only provider-neutral billing reads. Exact bounded raw bodies and OpenRails-canonical normalized records remain evidence; no row is a ledger movement.';

GRANT SELECT, INSERT ON TABLE openrails.provider_billing_qualifications TO openrails_app;
GRANT UPDATE (
    state,
    reason,
    baseline_observation_id,
    qualified_observation_id,
    qualified_provider_cost_usd_micros,
    qualified_at,
    updated_at
) ON TABLE openrails.provider_billing_qualifications TO openrails_app;

GRANT SELECT, INSERT ON TABLE openrails.provider_billing_observations TO openrails_app;
