-- Repair: none-needed No writer ever stored the kind account_deletion (an account deletion is admitted as a cancellation); a stored one refuses the migration.
DO $$ DECLARE retired bigint; BEGIN
    SELECT count(*) INTO retired FROM billing.destructive_actions WHERE kind='account_deletion';
    IF retired > 0 THEN
        RAISE EXCEPTION '% destructive actions hold the retired kind account_deletion', retired
          USING HINT='No OpenRails version writes this kind. Find what wrote these rows in billing.destructive_actions; the audit is immutable, so resolve them with the operator before retrying.';
    END IF;
END $$;
ALTER TABLE billing.destructive_actions DROP CONSTRAINT destructive_actions_kind_check;
ALTER TABLE billing.destructive_actions ADD CONSTRAINT destructive_actions_kind_check CHECK (kind IN ('refund','cancel_subscription','delete_payment_method','revoke_credit','revoke_access','waive_order','mark_uncollectible','catalog_change','configuration_change'));
