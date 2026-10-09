-- parent: 29 sha256:4061d333f16c182fd4ea7fb88079493a88124e58fdcdf4060d13c42738bdde09
-- Repair: none-needed Each new CHECK only widens the old one's values, so every stored row satisfies it.
-- A card an account updater reissued under another brand is recorded as such:
-- it voids the card's stored-credential agreements. Stripe's card updater is a
-- source.
ALTER TABLE billing.payment_method_updates DROP CONSTRAINT payment_method_updates_source_check;
ALTER TABLE billing.payment_method_updates ADD CONSTRAINT payment_method_updates_source_check
    CHECK (source IN ('nmi_acu', 'bt_account_updater', 'stripe_card_updater', 'customer'));
ALTER TABLE billing.payment_method_updates DROP CONSTRAINT payment_method_updates_kind_check;
ALTER TABLE billing.payment_method_updates ADD CONSTRAINT payment_method_updates_kind_check
    CHECK (kind IN ('updated', 'brand_changed', 'closed_account', 'contact_customer'));
COMMENT ON TABLE billing.payment_method_updates IS 'Changes to a stored card''s standing, by source (nmi_acu, bt_account_updater, stripe_card_updater, customer) and kind; brand_changed voided the card''s stored-credential agreements. event_ref makes a redelivered notice a no-op. Retention: permanent, never pruned.';
