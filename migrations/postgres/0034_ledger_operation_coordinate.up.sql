-- or#894: put the OPERATION KIND in the ledger's idempotency coordinate.
--
-- The spend coordinate was (merchant, customer, currency, source, source_id)
-- with no discriminator for which kind of money write posted the leg, so two
-- different operations sharing one (source, source_id) aliased at the ledger
-- while staying distinct upstream. Measured: a wasted-spend overage charge
-- (which posts through RecordUsage at event_type='wasted_spend') and the
-- CAPTURE of the same rendered request both land at ("invoke", request_id) —
-- the capture moved 0 micros and returned the waste transfer, so a delivered
-- 900,000-micro service went uncharged with no error. The reciprocal direction
-- is a double charge: usage_events dedupes on event_type (uq_usage_events_idem)
-- and the ledger did not, so two genuinely different usage events at one
-- request id collided on the ledger leg.
--
-- `operation` is ENGINE-COMPOSED, never caller-supplied: 'capture', 'spend',
-- 'withdraw', 'usage:<event_type>', 'deposit', ... A caller therefore cannot
-- present a colliding namespace, and cannot claim another operation's key.
--
-- Rows that predate this column keep '' — legacy dev data grouped exactly as it
-- was before. Every producer now supplies a non-blank operation (ledger.Apply
-- refuses a blank one), which is why the DEFAULT is dropped immediately.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.ledger_transfers ADD COLUMN operation text NOT NULL DEFAULT '';
ALTER TABLE openrails.ledger_transfers ALTER COLUMN operation DROP DEFAULT;

COMMENT ON COLUMN openrails.ledger_transfers.operation IS 'or#894 engine-composed money-operation kind (capture / spend / withdraw / usage:<event_type> / deposit / ...). Part of the idempotency coordinate together with (source, source_id): two different operations sharing a caller key must not alias.';

DROP INDEX openrails.idx_ledger_transfers_credit_spend_once;

CREATE UNIQUE INDEX idx_ledger_transfers_credit_spend_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, operation, source, source_id, grant_id)
    WHERE transfer_type = 'credit_spend';

DROP INDEX openrails.idx_ledger_transfers_owed_accrual_once;

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, operation, source, source_id)
    WHERE transfer_type = 'owed_accrual';
