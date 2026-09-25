-- parent: 18 sha256:9f32b074269c32f1eceb47ba77157e1037342ee23b0701749e5b4e63dd57718f
-- Validates the collection policy constraints 0018 added NOT VALID.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions VALIDATE CONSTRAINT subscriptions_collection_policy_check;
ALTER TABLE openrails.subscriptions VALIDATE CONSTRAINT subscriptions_nmi_schedule_rail_check;
