-- A merchant invoice is a bill: it stays open until paid or canceled and never
-- expires. Only a customer's own purchase expires, a day after it is made; its
-- pay URL lasts as long. A merchant invoice's pay URL expires on its own.

-- The old checks require a merchant invoice's expiry: they go before it does.
ALTER TABLE billing.invoices DROP CONSTRAINT invoices_period_check, DROP CONSTRAINT invoices_pay_url_check;

UPDATE billing.invoices SET expires_at = NULL, updated_at = now()
WHERE origin = 'merchant' AND expires_at IS NOT NULL;

ALTER TABLE billing.invoices ADD CONSTRAINT invoices_period_check CHECK (
    (origin IN ('subscription', 'usage')) = (period_starts_at IS NOT NULL AND period_ends_at IS NOT NULL AND due_at IS NOT NULL)
    AND (origin = 'subscription') = (subscription_id IS NOT NULL)
    AND (collection_owner <> 'engine' OR (origin = 'customer') = (expires_at IS NOT NULL))
    AND (period_ends_at IS NULL OR period_ends_at > period_starts_at)
);

ALTER TABLE billing.invoices ADD CONSTRAINT invoices_pay_url_check CHECK (
    (pay_url_secret_hash IS NULL) = (pay_url_expires_at IS NULL)
    AND (pay_url_secret_hash IS NULL) = (saved_payment_methods IS NULL)
    AND (pay_url_secret_hash IS NULL) = (embedded IS NULL)
    AND (success_url IS NULL OR pay_url_secret_hash IS NOT NULL)
    AND (cancel_url IS NULL OR pay_url_secret_hash IS NOT NULL)
    AND (pay_url_secret_hash IS NULL OR (octet_length(pay_url_secret_hash) = 32 AND (
        (origin = 'merchant' AND success_url IS NOT NULL)
        OR (origin = 'customer' AND pay_url_expires_at <= expires_at AND success_url IS NULL AND cancel_url IS NULL AND embedded))))
);

COMMENT ON COLUMN billing.invoices.expires_at IS 'When a customer''s unpaid purchase (origin customer) expires; no other invoice expires.';
COMMENT ON COLUMN billing.invoices.pay_url_expires_at IS 'When the pay URL stops working: a customer purchase''s with the purchase, a merchant invoice''s on its own while the invoice stays open.';
