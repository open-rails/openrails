-- The usage burn rate reads the event store's usage history by quarter hour
-- (the per-minute rollup is gone), and a limit window is over a day after
-- it ends: nothing reads it then but the limits_reached metric, which keeps
-- one that reached its capacity.

COMMENT ON COLUMN billing.balance_watches.usage_rate_per_hour IS 'Exponentially weighted rate of usage spend, one-hour time constant, in native units per hour: each look adds what the usage history''s quarter hours that ended since rate_sampled_at cost.';
COMMENT ON COLUMN billing.balance_watches.rate_sampled_at IS 'The instant usage_rate_per_hour was sampled at; quarter hours ended before it are not read again.';

COMMENT ON TABLE billing.limit_windows IS 'One customer''s window of one limit, identified by the server clock: a session or first-use window a use opened, a window staff adjusted, or one whose usage reached capacity; a rolling limit''s bucket, a rate or lifetime limit''s one row, or a billing policy''s spend window (spend:<identity>). What usage took of it is in the event store (limit_usage). Usage settled after a window ended counts toward it no more. Retention: rows are deleted a day after window_ends_at, 61 days after it once reached_at is set; a row without an end stays while its customer does.';
