-- Migration 003: Add unique indexes for data integrity

-- Ensure a user cannot have duplicate vault entries for the same payment method
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_user_vault
    ON billing.payment_methods(user_id, vault_id);
