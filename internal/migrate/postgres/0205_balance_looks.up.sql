-- Repair: none-needed New columns; every existing watch looks at each change until its next look.
-- A balance is looked at by a job (#1320): one queued with the change that crosses a line the
-- last look stored, and one for the instant that look said to look again. The billing pass
-- looks only at a watch whose job failed.
-- A watch written without its lines (a restore) looks at each change until a look stores them.
ALTER TABLE billing.balance_watches
    ADD COLUMN low_line_amount bigint,
    ADD COLUMN floor_amount bigint,
    ADD COLUMN look_each_change boolean NOT NULL DEFAULT true;

COMMENT ON TABLE billing.balance_watches IS 'One row per prepaid customer balance and per customer and currency with a running charge: when its balance is next looked at (a low line, the run-out, a credit lot lapse or the next 00:00 UTC, whichever is first) and what the last look found, including the lines a change is judged against. Its look job is queued by the change that crosses a line and for next_check_at; the billing pass looks at one whose job failed. Derived from the balance, the running charges and the billing mode; an arrears customer with no running charge has none.';
COMMENT ON COLUMN billing.balance_watches.next_check_at IS 'When the balance is next looked at; to_timestamp(0) once a change marks it due (updated_at is then when), null with nothing scheduled.';
COMMENT ON COLUMN billing.balance_watches.low_line_amount IS 'The available amount (balance less held) at or under which the balance is low, as the last look found the line; null with no line.';
COMMENT ON COLUMN billing.balance_watches.floor_amount IS 'The available amount at or under which the balance is depleted, as the last look found it; null before a look.';
COMMENT ON COLUMN billing.balance_watches.look_each_change IS 'Every change is looked at: a running charge or a lot that lapses moves the run-out, or no look has stored the lines yet.';
