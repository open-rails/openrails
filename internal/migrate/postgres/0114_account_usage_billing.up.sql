-- Repair: none-needed. Every existing row retains its explicit credit value,
-- including zero. New rows may inherit a declared merchant usage-billing limit.
ALTER TABLE billing.money_settings
    ADD COLUMN credit_limit_override boolean NOT NULL DEFAULT true,
    ADD COLUMN usage_billing_anchor timestamptz,
    ADD COLUMN usage_next_bill_at timestamptz,
    ADD COLUMN usage_minimum_invoice_amount bigint CHECK (usage_minimum_invoice_amount >= 0),
    ADD COLUMN usage_collection_due boolean NOT NULL DEFAULT false;
ALTER TABLE billing.money_settings ALTER COLUMN credit_limit_override SET DEFAULT false;
CREATE INDEX money_settings_usage_due ON billing.money_settings (merchant_id, usage_next_bill_at)
    WHERE usage_next_bill_at IS NOT NULL OR usage_collection_due;
COMMENT ON COLUMN billing.money_settings.credit_limit_override IS 'False inherits the merchant usage-billing limit; true honors credit_limit_amount including zero.';
COMMENT ON COLUMN billing.money_settings.usage_billing_anchor IS 'Fixed monthly UTC anchor used for usage pricing and invoicing; threshold payments never alter it.';
COMMENT ON COLUMN billing.money_settings.usage_next_bill_at IS 'Next monthly usage billing check. A below-minimum balance advances the check while its unbilled accruals carry forward.';
