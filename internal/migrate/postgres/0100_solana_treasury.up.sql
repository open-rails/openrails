-- Repair: none-needed Every existing Solana subscription paid its plan's owner, so its treasury is that owner's address; the pending-signer column held only approvals the deployment cranker no longer needs.
-- A Solana PSP is its treasury, and one deployment cranker owns every plan:
-- a subscription records the treasury its pulls land in. A PSP no longer
-- derives its account from a signer, so no signer change awaits approval.
ALTER TABLE billing.solana_subscriptions ADD COLUMN treasury text;
UPDATE billing.solana_subscriptions SET treasury = merchant_address;
ALTER TABLE billing.solana_subscriptions ALTER COLUMN treasury SET NOT NULL;
COMMENT ON COLUMN billing.solana_subscriptions.merchant_address IS 'The plan''s owner: the deployment cranker that publishes it and pulls each renewal.';
COMMENT ON COLUMN billing.solana_subscriptions.treasury IS 'The merchant''s treasury: the plan''s destination, where every pull lands.';
ALTER TABLE billing.psps DROP COLUMN pending_signer_public_key;
