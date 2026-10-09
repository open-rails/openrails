-- parent: 15 sha256:ec71d28b334cfc7dfe5aa9721255bcf2dd53c688b37a81cb5b58701d2f1f9b86
-- Repair: none-needed New nullable columns and an unblocked default preserve every existing row.
ALTER TABLE billing.customers ADD COLUMN username text;
ALTER TABLE billing.customers ADD COLUMN blocked boolean DEFAULT false NOT NULL;
ALTER TABLE billing.customers ADD CONSTRAINT customers_username_check CHECK (((username IS NULL) OR ((username = btrim(username)) AND (username <> ''::text) AND (octet_length(username) <= 256))));
COMMENT ON COLUMN billing.customers.username IS 'The customer''s username, as the merchant last declared it: billing email and the CCBill username bridge read it. NULL when none was declared.';
COMMENT ON COLUMN billing.customers.blocked IS 'The merchant declared the customer may not buy (banned or deleted at the host): checkout session actions refuse it. Declared with EnsureCustomer.';
CREATE INDEX customers_username_idx ON billing.customers USING btree (merchant_id, lower(username)) WHERE (username IS NOT NULL);
ALTER TABLE billing.provider_intents ADD COLUMN subject text CHECK (subject <> '');
ALTER TABLE billing.provider_intents ADD COLUMN credential text CHECK (credential <> '');
COMMENT ON COLUMN billing.provider_intents.actor IS 'The invoker that produced a user/admin-origin intent: the party that acted, the subject itself when it acted itself. NULL for system-origin. Keys the anti-credential-compromise rate ceiling (per-invoker + per-merchant rolling-hour count of destructive ops).';
COMMENT ON COLUMN billing.provider_intents.subject IS 'The subject whose authority produced a user/admin-origin intent: the staff member, application or customer. NULL for system-origin.';
COMMENT ON COLUMN billing.provider_intents.credential IS 'How the subject proved itself for a user/admin-origin intent, as kind:id (session:…, api_key:…). NULL for system-origin.';
