-- Invoice profiles and their customers must belong to the same merchant.
-- Remove legacy mismatches before enforcing the invariant at the DB boundary.
-- Lock in application order so legacy writers cannot race the cleanup.
LOCK TABLE openrails.customers IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE openrails.customer_invoice_profiles IN SHARE ROW EXCLUSIVE MODE;

DELETE FROM openrails.customer_invoice_profiles AS profile
USING openrails.customers AS customer
WHERE profile.customer_id = customer.id
  AND profile.merchant_id <> customer.merchant_id;

ALTER TABLE ONLY openrails.customers
    ADD CONSTRAINT customers_merchant_id_id_key UNIQUE (merchant_id, id);

ALTER TABLE ONLY openrails.customer_invoice_profiles
    DROP CONSTRAINT customer_invoice_profiles_customer_fk;

ALTER TABLE ONLY openrails.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_customer_fk
    FOREIGN KEY (merchant_id, customer_id)
    REFERENCES openrails.customers(merchant_id, id)
    ON DELETE CASCADE;
