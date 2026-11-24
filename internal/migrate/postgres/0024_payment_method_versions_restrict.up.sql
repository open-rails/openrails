-- A card's versions are permanent: a payment method they describe is never
-- deleted beneath them.
ALTER TABLE billing.payment_method_versions
    DROP CONSTRAINT payment_method_versions_payment_method_id_fkey,
    ADD CONSTRAINT payment_method_versions_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id)
        REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE RESTRICT;
