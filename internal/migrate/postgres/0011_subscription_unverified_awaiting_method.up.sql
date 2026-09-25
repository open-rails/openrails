-- parent: 10 sha256:daa124994f8ce3f9a8885db0c7dbd8720785122a4d2956d7f9991fbaeb52aa8c
-- #1091 lifecycle states: `unknown` becomes `unverified` (resolved from the
-- provider, never a dead end), and `awaiting_method` is the declined
-- subscription waiting for the customer's new card. The new label is used by
-- indexes and functions in the next migration (a new enum value cannot be
-- used in the transaction that adds it).
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TYPE openrails.subscription_status RENAME VALUE 'unknown' TO 'unverified';
ALTER TYPE openrails.subscription_status ADD VALUE 'awaiting_method' AFTER 'past_due';
