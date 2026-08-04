DROP INDEX IF EXISTS openrails.idx_customers_subject;
DROP FUNCTION IF EXISTS openrails.customer_merchant_ids_for_subject(text);
DROP FUNCTION IF EXISTS openrails.psp_owner_by_identity(text, text, text);
DROP FUNCTION IF EXISTS openrails.assert_cross_merchant_reader();
DROP FUNCTION IF EXISTS openrails.current_merchant_id();
