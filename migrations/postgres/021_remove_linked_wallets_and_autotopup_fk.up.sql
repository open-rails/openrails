DROP TABLE IF EXISTS openrails.linked_wallets;

ALTER TABLE openrails.money_settings
    DROP CONSTRAINT IF EXISTS money_settings_auto_topup_payment_method_fk;

ALTER TABLE openrails.money_settings
    ADD CONSTRAINT money_settings_auto_topup_payment_method_fk
    FOREIGN KEY (auto_topup_payment_method_id)
    REFERENCES openrails.payment_methods(id)
    ON DELETE SET NULL;
