-- Explicitly set schema to ensure all objects are created in the correct place
-- Set timeouts to prevent hanging migrations
SET lock_timeout = '10s';
SET statement_timeout = '300s';

-- Drop in reverse dependency order
DROP TABLE IF EXISTS billing.idempotency_requests CASCADE;
DROP TABLE IF EXISTS billing.notification_queue CASCADE;
DROP TABLE IF EXISTS billing.payments CASCADE;
DROP TABLE IF EXISTS billing.payment_methods CASCADE;
DROP TABLE IF EXISTS billing.entitlements CASCADE;
DROP TABLE IF EXISTS billing.prices CASCADE;
DROP TABLE IF EXISTS billing.products CASCADE;
DROP TABLE IF EXISTS billing.subscriptions CASCADE;

-- Drop enums created by this migration
DROP TYPE IF EXISTS billing.purchase_status;
DROP TYPE IF EXISTS billing.processor_type;
DROP TYPE IF EXISTS billing.subscription_status;
