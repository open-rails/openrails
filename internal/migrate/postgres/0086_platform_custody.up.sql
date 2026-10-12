-- Repair: none-needed Widened vocabularies and a new nullable column: every stored row already satisfies them.
-- The deployment's platform vault holds a merchant's cards in the merchant's
-- partition. The engine records one identity per merchant for it (kind
-- platform, key platform, account <tenant>/<merchant_id>), so platform cards
-- keep the custodian foreign key and the holder checks.
ALTER TABLE billing.custodians DROP CONSTRAINT custodians_kind_check;
ALTER TABLE billing.custodians ADD CONSTRAINT custodians_kind_check CHECK (kind = ANY (ARRAY['basis_theory'::text, 'hyperswitch'::text, 'platform'::text]));
COMMENT ON TABLE billing.custodians IS 'Custodian identities. A row is one merchant-owned account with a third-party card custodian, the target of the cards it holds, whose configuration (settings, credentials, archived) is the custodian document in a file or Vault; or, kind platform, the merchant''s partition of the deployment''s platform vault, which the engine records and no document configures. Custody is orthogonal to the rail: this says who holds the card, psps says who charges it.';
COMMENT ON COLUMN billing.custodians.kind IS 'The custodian: basis_theory or hyperswitch (a merchant''s vendor account), or platform (the deployment''s platform vault). Same vocabulary as payment_methods.custodian, minus ''psp'' (which is the absence of a third-party custodian, not an account).';
COMMENT ON COLUMN billing.custodians.account_id IS 'The custodian-native identity: the vendor''s tenant id, operator-declared (there is no runtime whoami); for platform, <tenant id>/<merchant id>.';

ALTER TABLE billing.payment_methods DROP CONSTRAINT payment_methods_custodian_check;
ALTER TABLE billing.payment_methods ADD CONSTRAINT payment_methods_custodian_check CHECK (custodian = ANY (ARRAY['psp'::text, 'basis_theory'::text, 'hyperswitch'::text, 'platform'::text]));
COMMENT ON COLUMN billing.payment_methods.custodian IS 'Who holds the instrument: psp (the processor itself), basis_theory or hyperswitch (the merchant''s third-party vault), or platform (the deployment''s platform vault, in the merchant''s partition); a vault proxies the card to the processor at charge time.';

-- A merchant's copy of a customer's wallet card names the card it came from;
-- the copy is its own credential.
ALTER TABLE billing.payment_methods ADD COLUMN wallet_card_id uuid;
COMMENT ON COLUMN billing.payment_methods.wallet_card_id IS 'The wallet card this merchant copy was made from (lineage); NULL for a card given to the merchant.';
