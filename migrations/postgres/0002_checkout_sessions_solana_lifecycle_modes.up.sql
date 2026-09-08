-- checkout_sessions.mode: admit the Solana lifecycle modes.
--
-- The engine models a wallet-driven cancel and tier change as checkout sessions
-- (models.CheckoutSessionModeSolanaCancel / ...SolanaTierChange, #529) so the
-- Solana Pay reference poller can confirm the landed on-chain action exactly
-- like a purchase. The baseline's CHECK still listed only the purchase modes,
-- so every such session was refused at INSERT (23514) and the flows could not
-- start. Widen the allowed set; nothing else about the column changes.

SET statement_timeout = '300s';
SET lock_timeout = '10s';

ALTER TABLE openrails.checkout_sessions
    DROP CONSTRAINT checkout_sessions_mode_check;

ALTER TABLE openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_mode_check
    CHECK (mode = ANY (ARRAY['one_off'::text, 'subscription'::text, 'solana_cancel'::text, 'solana_tier_change'::text]));
