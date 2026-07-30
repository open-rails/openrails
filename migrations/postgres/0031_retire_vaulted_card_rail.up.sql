-- or#879 / or#880 phase 2: `vaulted_card` was never a rail.
--
-- A rail is the gateway KIND that CHARGES the card (nmi | ccbill | stripe |
-- solana | paypal). `vaulted_card` named a CUSTODY arrangement: the processor
-- is NMI, the PAN is held at Basis Theory and detokenized through its proxy.
-- The proof was in the code — the vaulted_card charge path built an NMI client,
-- and the "rail"'s own `gateway_account` setting pointed at an NMI PSP.
--
-- Two orthogonal axes had been collapsed into one enum value, so every
-- rail-dispatching switch had to remember `case 'nmi','mobius','vaulted_card':`
-- and forgetting was silent. Phase 1 (0025) made custody first-class on the
-- instrument (payment_methods.custodian). This migration removes the second,
-- weaker encoding:
--
--   rail = 'vaulted_card'  ->  rail = 'nmi', custodian = 'basis_theory'
--
-- Custody CONFIG moves with it: it now lives on the NMI PSP the charges already
-- landed on, in that PSP's settings (custodian / custodian_account_id /
-- custodian_public_api_key / custodian_network_tokens), with the Basis Theory
-- private application key as that PSP's `custodian_api_key` secret. The old
-- `gateway_account` pointer disappears — the PSP IS the gateway account.
--
-- Nothing here deletes an instrument. Every payment_methods row survives; it
-- is restated, not removed.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- 1. A declared vaulted_card PSP cannot be migrated mechanically: its
-- account_id is a Basis Theory tenant id, not an NMI gateway id, and its
-- credentials belong on the NMI PSP its settings.gateway_account named. No
-- merchant declares one (verified across every manifest and host repo), so the
-- honest handling of a surprise row is to REFUSE, not to guess a fold.
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM openrails.psps WHERE rail = 'vaulted_card';
    IF n > 0 THEN
        RAISE EXCEPTION
            'openrails: % PSP row(s) still declare rail=''vaulted_card'' (or#879)', n
            USING ERRCODE = 'check_violation',
                  HINT = 'vaulted_card is not a rail. Re-declare the arrangement on the NMI PSP that settings.gateway_account already pointed at: settings.custodian=basis_theory, settings.custodian_account_id=<BT tenant id>, settings.custodian_public_api_key=<BT public key>, secret custodian_api_key=<BT private key>. Then drop the vaulted_card PSP row and re-run.';
    END IF;
END;
$$;

-- 2. Instruments: the processor was always NMI; state the custodian.
UPDATE openrails.payment_methods
   SET rail = 'nmi', custodian = 'basis_theory', updated_at = now()
 WHERE rail = 'vaulted_card';

-- 3. Every other mirror row that recorded the arrangement as a rail.
UPDATE openrails.subscriptions           SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.payments                SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.checkout_sessions       SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.invoice_payments        SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.rail_intents            SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.rail_mutation_logs      SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.rail_customer_accounts  SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.imported_dunning_history SET rail = 'nmi' WHERE rail = 'vaulted_card';
UPDATE openrails.probe_verdicts          SET rail = 'nmi' WHERE rail = 'vaulted_card';

-- Webhook health is dimensioned by the SENDER, and Basis Theory sends its own
-- events: the source there is the custodian, not the rail it proxies into.
UPDATE openrails.webhook_health       SET rail = 'basis_theory' WHERE rail = 'vaulted_card';
UPDATE openrails.webhook_health_daily SET rail = 'basis_theory' WHERE rail = 'vaulted_card';

-- 4. The checkout sale intent is named after the custody arrangement now. Its
-- idempotency key carries the same prefix, and an in-flight sale whose key
-- stopped matching would be re-posted as a NEW charge — so both move together.
UPDATE openrails.rail_intents
   SET intent_type = 'custodian_sale'
 WHERE intent_type = 'vaulted_card_sale';

UPDATE openrails.rail_intents
   SET idempotency_key = 'custodian_sale:' || substring(idempotency_key from 19)
 WHERE idempotency_key LIKE 'vaulted_card_sale:%';

-- 5. or#871: `vault` is reserved for HashiCorp Vault in this codebase, and this
-- column is on payment_methods — Stripe's own word for it is `fingerprint`.
-- squawk-ignore renaming-column
ALTER TABLE openrails.payment_methods RENAME COLUMN vault_fingerprint TO fingerprint;
ALTER INDEX openrails.payment_methods_vault_fingerprint_idx RENAME TO payment_methods_fingerprint_idx;
COMMENT ON COLUMN openrails.payment_methods.fingerprint IS
    'Custodian-issued stable fingerprint of the underlying PAN (Basis Theory''s default fingerprint expression), for dedup/lookup. '''' = the custodian issues none.';

-- 6. token_type: same reserved word, same fix. `provider_vault` also carried
-- the retired `provider` vocabulary (a merchant's account on a rail is a PSP).
--   provider_vault -> psp_token     (the PSP holds the card; we charge its token)
--   pan_via_vault  -> pan_via_proxy (the custodian detokenizes the PAN into the gateway)
ALTER TABLE openrails.payments DROP CONSTRAINT chk_payments_token_type;

UPDATE openrails.payments SET token_type = 'psp_token'     WHERE token_type = 'provider_vault';
UPDATE openrails.payments SET token_type = 'pan_via_proxy' WHERE token_type = 'pan_via_vault';

ALTER TABLE openrails.payments
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT chk_payments_token_type
    CHECK (((token_type IS NULL) OR (token_type = ANY (ARRAY['network_token'::text, 'pan_via_proxy'::text, 'psp_token'::text]))));

COMMENT ON COLUMN openrails.payments.token_type IS
    '#796 credential form presented to the network: network_token | pan_via_proxy | psp_token. NULL = unknown/legacy; excluded from token_type-dimensioned metrics.';

-- 7. Custodian routing. An inbound Basis Theory webhook carries a TENANT id and
-- no merchant context, exactly like an account-routed Stripe/CCBill webhook —
-- so it needs the same indexed, cross-merchant directory lookup. The custodian
-- identity lives in the PSP's settings, so index the expression.
CREATE UNIQUE INDEX uq_psps_custodian_identity
    ON openrails.psps ((evidence -> 'settings' ->> 'custodian'),
                       environment,
                       (evidence -> 'settings' ->> 'custodian_account_id'))
 WHERE (evidence -> 'settings' ->> 'custodian_account_id') IS NOT NULL;

COMMENT ON INDEX openrails.uq_psps_custodian_identity IS
    'or#879: (custodian, environment, custodian_account_id) is a GLOBAL natural key, same shape as uq_psps_identity for rails. One BT tenant belongs to one PSP.';

CREATE FUNCTION openrails.psp_owner_by_custodian_identity(p_custodian text, p_environment text, p_account_id text)
    RETURNS TABLE (id uuid, merchant_id uuid, rail text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT p.id, p.merchant_id, p.rail, p.environment, p.account_id
      FROM openrails.psps p
     WHERE (p.evidence -> 'settings' ->> 'custodian') = lower(p_custodian)
       AND p.environment = p_environment
       AND (p.evidence -> 'settings' ->> 'custodian_account_id') = p_account_id
     LIMIT 1;
END;
$$;

COMMENT ON FUNCTION openrails.psp_owner_by_custodian_identity(text, text, text) IS
    'Cross-merchant PSP ownership lookup by the GLOBAL (custodian, environment, custodian_account_id) natural key — the custody sibling of psp_owner_by_identity, for inbound custodian webhooks (Basis Theory) that carry a tenant id and no merchant context. Returns the routing tuple only.';

REVOKE ALL ON FUNCTION openrails.psp_owner_by_custodian_identity(text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.psp_owner_by_custodian_identity(text, text, text) TO openrails_app;
