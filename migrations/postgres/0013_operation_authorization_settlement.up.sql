-- th-005 one-shot final settlement for a durable operation authorization.
-- Money still moves only through the existing double-entry ledger. RunPod
-- billing aggregates are mutable before final evidence, so v1 deliberately
-- exposes no partial-settlement state.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.operation_authorizations
    ADD COLUMN settlement_rated_usd_micros bigint,
    ADD COLUMN settlement_body_bytes bytea,
    ADD COLUMN settlement_body_digest bytea,
    ADD CONSTRAINT operation_authorizations_settlement_shape CHECK (
        (
            state <> 'settled'
            AND settlement_rated_usd_micros IS NULL
            AND settlement_body_bytes IS NULL
            AND settlement_body_digest IS NULL
        )
        OR
        (
            state = 'settled'
            AND settlement_rated_usd_micros IS NOT NULL
            AND settlement_rated_usd_micros >= 0
            AND settlement_body_bytes IS NOT NULL
            AND octet_length(settlement_body_bytes) BETWEEN 1 AND 65536
            AND settlement_body_digest IS NOT NULL
            AND octet_length(settlement_body_digest) = 32
            AND settlement_body_digest = public.digest(settlement_body_bytes, 'sha256')
            AND terminal_reference = 'sha256:' || encode(settlement_body_digest, 'hex')
        )
    );

COMMENT ON COLUMN openrails.operation_authorizations.settlement_rated_usd_micros IS
    'One host-rated final customer settlement. It may exceed authorized_usd_micros and must never be clamped.';

COMMENT ON COLUMN openrails.operation_authorizations.settlement_body_bytes IS
    'Exact canonical final-settlement bytes authored by the embedding host. OpenRails binds but does not interpret them.';

COMMENT ON COLUMN openrails.operation_authorizations.settlement_body_digest IS
    'OpenRails-derived SHA-256 of settlement_body_bytes, also rechecked by the database and used as the canonical terminal reference.';

GRANT UPDATE (settlement_rated_usd_micros, settlement_body_bytes, settlement_body_digest)
    ON TABLE openrails.operation_authorizations TO openrails_app;
