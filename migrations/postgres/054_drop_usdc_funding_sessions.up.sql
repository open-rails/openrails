-- #666: delete the half-implemented USDC funding (Coinbase onramp) subsystem — sessions could
-- never be created (no route/caller reached Service.Create), so the table is empty everywhere.
-- Also removed from the 001 baseline; this drop covers DBs migrated before the removal.
-- Future re-introduction (Robinhood/Coinbase funding into user Solana wallets) is tracked in future.md.
DROP TABLE IF EXISTS openrails.usdc_funding_sessions;
