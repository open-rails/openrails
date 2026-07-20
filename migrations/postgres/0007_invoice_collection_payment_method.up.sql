ALTER TABLE openrails.money_settings
    ADD COLUMN collection_payment_method_id uuid
        REFERENCES openrails.payment_methods(id) ON DELETE SET NULL;

UPDATE openrails.money_settings
SET collection_payment_method_id = auto_topup_payment_method_id
WHERE auto_topup_payment_method_id IS NOT NULL;

ALTER TABLE openrails.invoice_payments
    ADD COLUMN payment_method_id uuid
        REFERENCES openrails.payment_methods(id) ON DELETE SET NULL,
    ADD COLUMN idempotency_key text;

CREATE UNIQUE INDEX ux_invoice_payments_attempt_key
    ON openrails.invoice_payments (merchant_id, invoice_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
