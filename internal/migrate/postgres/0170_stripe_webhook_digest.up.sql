-- Repair: none-needed A new nullable column: a PSP with none is reconciled on
-- the next boot.
-- A Stripe PSP's webhook endpoint is reconciled at boot only when the
-- configuration it was last settled at changed.
ALTER TABLE billing.psps
    ADD COLUMN webhook_digest text,
    ADD CONSTRAINT psps_webhook_digest_check CHECK (webhook_digest ~ '^[0-9a-f]{64}$');
COMMENT ON COLUMN billing.psps.webhook_digest IS 'SHA-256 of the pinned Stripe API version, the handled event types and the webhook URL the PSP''s managed endpoint was last settled at; NULL until one is.';
