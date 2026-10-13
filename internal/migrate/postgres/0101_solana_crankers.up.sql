-- Repair: none-needed A new, empty table: each listed cranker is pinned on its next use.
-- Each listed Solana cranker is pinned to the public key it first had, so a
-- Vault Transit rotation never silently turns it into another cranker.
CREATE TABLE billing.solana_crankers (
    name text PRIMARY KEY CHECK (btrim(name) <> ''),
    public_key text NOT NULL CHECK (btrim(public_key) <> ''),
    pinned_at timestamp with time zone NOT NULL DEFAULT now()
);
COMMENT ON TABLE billing.solana_crankers IS 'Global by design: the deployment''s Solana crankers, each pinned to the public key it first had. A cranker whose key now differs is refused, so a Vault Transit rotation never silently becomes another cranker. A row is never rewritten.';
COMMENT ON COLUMN billing.solana_crankers.name IS 'The listed cranker without its secret: vault_transit:<key> or local_keypair:<public key>.';
COMMENT ON COLUMN billing.solana_crankers.public_key IS 'The public key the cranker had on its first use.';
