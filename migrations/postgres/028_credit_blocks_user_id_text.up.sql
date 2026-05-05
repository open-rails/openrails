SET lock_timeout = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.credit_blocks
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;
