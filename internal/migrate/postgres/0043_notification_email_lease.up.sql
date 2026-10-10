-- parent: 42 sha256:63f66bbc2deaf3a8e4d9915340a24cfab8b1c819c1ba7ca1d19318de05057ee0
-- Repair: none-needed No email is being sent while the schema migrates.
-- One sender at a time per notification email: replicas delivering the same
-- notification at once send it once.
ALTER TABLE billing.notifications ADD COLUMN email_lease_expires_at timestamp with time zone;
COMMENT ON COLUMN billing.notifications.email_lease_expires_at IS 'Until when one sender holds the undelivered email; another sends it only after this passes, so a sender that died is replaced.';
