ALTER TABLE ONLY openrails.customer_invoice_profiles
    DROP CONSTRAINT customer_invoice_profiles_customer_fk;

ALTER TABLE ONLY openrails.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_customer_fk
    FOREIGN KEY (customer_id)
    REFERENCES openrails.customers(id)
    ON DELETE CASCADE;

ALTER TABLE ONLY openrails.customers
    DROP CONSTRAINT customers_merchant_id_id_key;
