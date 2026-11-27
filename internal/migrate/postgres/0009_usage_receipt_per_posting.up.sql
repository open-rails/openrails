-- A posting writes one usage receipt per customer and currency, listing all it
-- took: usage lines, each naming its pricing period and what it forgave, and
-- the running charges it posted, through the same accounting-zone midnight.
-- A stop posts nothing: what a stopped charge accrued stays in the balance
-- until the next posting takes it.

ALTER TABLE billing.usage_receipts DROP CONSTRAINT usage_receipts_amount_check;
ALTER TABLE billing.usage_receipts DROP CONSTRAINT usage_receipts_forgiven_check;
ALTER TABLE billing.usage_receipts DROP CONSTRAINT usage_receipts_forgiven_lines_check;
ALTER TABLE billing.usage_receipts DROP CONSTRAINT usage_receipts_period_ends_at_check;
ALTER TABLE billing.usage_receipts DROP CONSTRAINT usage_receipts_reason_check;
DROP INDEX billing.usage_receipts_forgiven_idx;
DROP INDEX billing.usage_receipts_period_idx;

-- Each line takes its receipt's period and a share of what it forgave: the
-- lines of the forgiveness's sign in order, each up to its amount. A reversal
-- receipt did not record what its posting ran for: daily.
UPDATE billing.usage_receipts r
SET reason = CASE r.reason WHEN 'reversal' THEN 'daily' ELSE r.reason END,
    lines = (
        SELECT COALESCE(jsonb_agg(e.line || jsonb_build_object(
                   'period_starts_at', to_char(r.period_starts_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                   'period_ends_at', to_char(r.period_ends_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                   'forgiven', sign(r.forgiven)::bigint * LEAST(e.size, GREATEST(abs(r.forgiven) - (e.running - e.size), 0)))
                   ORDER BY e.ord), '[]'::jsonb)
        FROM (
            SELECT s.line, s.ord, s.size, (sum(s.size) OVER (ORDER BY s.ord))::bigint AS running
            FROM (
                SELECT l.line, l.ord,
                       CASE WHEN r.forgiven <> 0 AND ((l.line ->> 'amount')::bigint > 0) = (r.forgiven > 0)
                            THEN abs((l.line ->> 'amount')::bigint) ELSE 0 END AS size
                FROM jsonb_array_elements(r.lines) WITH ORDINALITY AS l(line, ord)
            ) s
        ) e
    );

ALTER TABLE billing.usage_receipts ADD COLUMN charges jsonb NOT NULL DEFAULT '[]';
ALTER TABLE billing.usage_receipts ALTER COLUMN charges DROP DEFAULT;
ALTER TABLE billing.usage_receipts DROP COLUMN period_starts_at;
ALTER TABLE billing.usage_receipts DROP COLUMN period_ends_at;
ALTER TABLE billing.usage_receipts DROP COLUMN forgiven_lines;

ALTER TABLE billing.usage_receipts
    ADD CONSTRAINT usage_receipts_reason_check CHECK (reason IN ('daily', 'billing', 'credit', 'purge')),
    ADD CONSTRAINT usage_receipts_charges_check CHECK (jsonb_typeof(charges) = 'array'),
    -- A receipt takes something; its lines may net to zero.
    ADD CONSTRAINT usage_receipts_taken_check CHECK (jsonb_array_length(lines) + jsonb_array_length(charges) > 0),
    -- Only a merchant's unit forgives: it is never owed.
    ADD CONSTRAINT usage_receipts_forgiven_check CHECK (forgiven = 0
        OR currency NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));

-- A customer's receipts in time: where a period's forgiveness is read.
CREATE INDEX usage_receipts_customer_idx ON billing.usage_receipts (merchant_id, customer_id, currency, cut_at);
-- Receipts with a line that forgave or took forgiveness back, however they net.
CREATE INDEX usage_receipts_forgiven_idx ON billing.usage_receipts (merchant_id, cut_at)
    WHERE jsonb_path_exists(lines, '$[*] ? (@.forgiven != 0)');

COMMENT ON TABLE billing.usage_receipts IS 'What one posting took of a customer''s metered spend in a currency, never changed once written: per pricing period, meter cell and card print the price of the period''s settled quantity less what its earlier receipts took, and what each running charge accrued since its last posting. The posting draws the lots first, one credit_spend each, and leaves the rest owed in one owed_accrual, at (usage_posting, usage_receipt:<id>, 1); a period whose price fell is credited at the same coordinate as a lot, usage_reversed. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.usage_receipts.cut_at IS 'The receipt took the settled usage that occurred before it: a credit lot''s lapse, else all of it (the posting''s instant plus the usage clock skew).';
COMMENT ON COLUMN billing.usage_receipts.reason IS 'daily: once the accounting-zone day of the oldest pending usage or running charge ended; billing: before a usage invoice is billed; credit: before a credit lot''s expiry, revocation or refund, or an invoice paid from the balance; purge: before a merchant purge.';
COMMENT ON COLUMN billing.usage_receipts.amount IS 'The sum of the lines and charges: what the posting took less what it credited.';
COMMENT ON COLUMN billing.usage_receipts.lines IS 'Per pricing period, meter, cell and card print, what the posting added: [{period_starts_at, period_ends_at, meter_key, cell, print, requests, quantity, stated, amount, forgiven}]. A period''s receipts sum to the price of its settled usage; a period whose price fell has negative lines. forgiven is the line''s share of what a merchant''s unit could not take (positive) or took back (negative).';
COMMENT ON COLUMN billing.usage_receipts.forgiven IS 'The sum of the lines'' forgiven: of amount, what the ledger did not take. Zero in a registry currency.';
COMMENT ON COLUMN billing.usage_receipts.charges IS 'Each running charge the posting took, from its last posting to its end here: [{running_charge_id, starts_at, ends_at, amount}].';

-- A stop posts nothing: a stopped charge is posted through its stop later.
ALTER TABLE billing.running_charges DROP CONSTRAINT running_charges_stopped_at_check;
ALTER TABLE billing.running_charges ADD CONSTRAINT running_charges_stopped_at_check CHECK (stopped_at IS NULL OR posted_through_at <= stopped_at);
CREATE INDEX running_charges_unposted_idx ON billing.running_charges (merchant_id, customer_id, currency)
    WHERE stopped_at IS DISTINCT FROM posted_through_at;
COMMENT ON TABLE billing.running_charges IS 'One row per running charge: a customer charged rate_per_hour (native units of currency) from started_at until stopped_at. No row changes while it runs but at its postings, which move what accrued through posted_through_at to the ledger; a stop posts nothing, and a stopped charge is posted through stopped_at by a later posting. Retention: permanent, never pruned.';

-- The posting queue holds every customer with a charge not yet posted.
INSERT INTO billing.usage_pending (merchant_id, customer_id, currency, due_at, updated_at)
SELECT DISTINCT merchant_id, customer_id, currency, now(), now()
FROM billing.running_charges
WHERE stopped_at IS DISTINCT FROM posted_through_at
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET due_at = LEAST(usage_pending.due_at, EXCLUDED.due_at);
COMMENT ON TABLE billing.usage_pending IS 'A customer with usage or a running charge in a currency no posting has taken: the posting queue. Intake holds the row FOR KEY SHARE from before it writes its usage until it commits, and writes it only to move due_at earlier; a running charge''s start writes it under the customer lock; a posting locks it FOR UPDATE, so it waits for every intake of the customer, and deletes it once nothing is unposted.';
COMMENT ON COLUMN billing.usage_pending.due_at IS 'When the daily posting takes it: the end of the merchant''s accounting-zone day its oldest unposted usage occurred in, or a running charge was last posted through.';
