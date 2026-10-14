-- OpenRails, not the chain program, admits and executes every Solana renewal.
-- Correct the old mirror classification without rewriting settled bill history.
ALTER TABLE billing.subscriptions DISABLE TRIGGER subscriptions_collection_policy_immutable;
UPDATE billing.subscriptions SET collection_policy = 'engine'
WHERE rail = 'solana' AND collection_policy = 'provider';
ALTER TABLE billing.subscriptions ENABLE TRIGGER subscriptions_collection_policy_immutable;
