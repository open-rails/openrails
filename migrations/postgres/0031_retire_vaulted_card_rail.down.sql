-- Reverse of 0031. The rail value cannot be recovered from `rail` alone — it is
-- reconstructed from the custody axis, which is exactly the point of or#879:
-- (rail=nmi, custodian=basis_theory) carries strictly more information than
-- rail='vaulted_card' did.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION IF EXISTS openrails.psp_owner_by_custodian_identity(text, text, text);
DROP INDEX IF EXISTS openrails.uq_psps_custodian_identity;

ALTER TABLE openrails.payments DROP CONSTRAINT chk_payments_token_type;
UPDATE openrails.payments SET token_type = 'provider_vault' WHERE token_type = 'psp_token';
UPDATE openrails.payments SET token_type = 'pan_via_vault'  WHERE token_type = 'pan_via_proxy';
ALTER TABLE openrails.payments
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT chk_payments_token_type
    CHECK (((token_type IS NULL) OR (token_type = ANY (ARRAY['network_token'::text, 'pan_via_vault'::text, 'provider_vault'::text]))));
COMMENT ON COLUMN openrails.payments.token_type IS '#796 credential form presented to the network: network_token | pan_via_vault | provider_vault. NULL = unknown/legacy; excluded from token_type-dimensioned metrics.';

ALTER INDEX openrails.payment_methods_fingerprint_idx RENAME TO payment_methods_vault_fingerprint_idx;
ALTER TABLE openrails.payment_methods RENAME COLUMN fingerprint TO vault_fingerprint;

UPDATE openrails.rail_intents
   SET intent_type = 'vaulted_card_sale'
 WHERE intent_type = 'custodian_sale';

UPDATE openrails.webhook_health       SET rail = 'vaulted_card' WHERE rail = 'basis_theory';
UPDATE openrails.webhook_health_daily SET rail = 'vaulted_card' WHERE rail = 'basis_theory';

UPDATE openrails.payment_methods
   SET rail = 'vaulted_card', updated_at = now()
 WHERE rail = 'nmi' AND custodian = 'basis_theory';

UPDATE openrails.subscriptions s SET rail = 'vaulted_card'
 WHERE s.rail = 'nmi'
   AND EXISTS (SELECT 1 FROM openrails.payment_methods pm
                WHERE pm.id = s.payment_method_id AND pm.rail = 'vaulted_card');

UPDATE openrails.payments p SET rail = 'vaulted_card'
 WHERE p.rail = 'nmi' AND p.token_type = 'pan_via_vault';
