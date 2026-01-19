-- Rollback Migration 003: Remove unique indexes

DROP INDEX IF EXISTS billing.uq_payment_methods_user_vault;
