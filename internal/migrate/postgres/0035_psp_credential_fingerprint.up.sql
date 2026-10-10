-- parent: 34 sha256:d230349587eb4c95bdc6688e05abbefdbf98e7784e7418b138c80e421f9f8930
-- The same gateway account declared twice under different labels is billed
-- by both PSPs. The credential that names the account identifies it: a PSP
-- whose credential another live PSP on its rail already holds is a duplicate,
-- and its credentials are not read until it is archived or given its own.
ALTER TABLE billing.psps ADD COLUMN credential_fingerprint text;
ALTER TABLE billing.psps ADD COLUMN credential_duplicate_at timestamp with time zone;
ALTER TABLE billing.psps ADD CONSTRAINT psps_credential_fingerprint_check CHECK (credential_fingerprint ~ '^[0-9a-f]{64}$');
COMMENT ON COLUMN billing.psps.credential_fingerprint IS 'HMAC-SHA256, under a key derived from encryption.master_key, of the credential naming the gateway account (NMI security_key, Stripe secret_key). Never the credential; NULL without a master key or before the credential is published.';
COMMENT ON COLUMN billing.psps.credential_duplicate_at IS 'When this PSP was found to declare a gateway account another live PSP on its rail already declares. Its credentials are not read, so it stays disarmed, until it is archived or given its own.';
CREATE UNIQUE INDEX psps_live_credential_fingerprint_key ON billing.psps USING btree (rail, environment, credential_fingerprint)
    WHERE NOT archived AND credential_duplicate_at IS NULL AND credential_fingerprint IS NOT NULL;
