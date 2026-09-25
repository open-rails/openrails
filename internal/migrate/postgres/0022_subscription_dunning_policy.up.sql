-- parent: 21 sha256:dad015168557ed4f2adbfec9f5f8658612f459356a3a8a78de821fea9bfef2be
-- #1102: a dunning case keeps the policy it opened under. The merchant's
-- policy at the first decline is recorded on the subscription and read for the
-- rest of the case, so a policy edit never changes a case already in flight.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions ADD COLUMN dunning_policy jsonb;
