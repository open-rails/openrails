-- #795 vaulted_card rail (Basis Theory neutral vault) + #796 token_type.

-- Instrument columns for the neutral-vault rail. rail='vaulted_card' rows use
-- rail_method_ref = BT card-token id (rotates on Account Updater new_token).
ALTER TABLE openrails.payment_methods
    ADD COLUMN vault_provider text DEFAULT ''::text NOT NULL,
    ADD COLUMN vault_fingerprint text DEFAULT ''::text NOT NULL,
    ADD COLUMN network_token_id text DEFAULT ''::text NOT NULL,
    ADD COLUMN network_token_status text DEFAULT ''::text NOT NULL,
    ADD COLUMN network_token_par text DEFAULT ''::text NOT NULL,
    ADD COLUMN charge_via text DEFAULT 'pan_proxy'::text NOT NULL,
    ADD COLUMN park_reason text DEFAULT ''::text NOT NULL,
    ADD COLUMN parked_at timestamp with time zone,
    ADD CONSTRAINT payment_methods_charge_via_check CHECK ((charge_via = ANY (ARRAY['pan_proxy'::text, 'network_token'::text])));

COMMENT ON COLUMN openrails.payment_methods.vault_provider IS '#795 neutral card vault holding this instrument (''basis_theory'' on vaulted_card rows; '''' elsewhere).';
COMMENT ON COLUMN openrails.payment_methods.vault_fingerprint IS '#795 vault card fingerprint (BT default expression over the PAN) for dedup/lookup.';
COMMENT ON COLUMN openrails.payment_methods.network_token_id IS '#795 BT network-token uuid; '''' = not provisioned.';
COMMENT ON COLUMN openrails.payment_methods.network_token_status IS '#795 NT lifecycle status: ''''|active|inactive|suspended|deleted (webhook-folded; never touches PAN-side expiry).';
COMMENT ON COLUMN openrails.payment_methods.network_token_par IS '#795 payment account reference from NT provisioning.';
COMMENT ON COLUMN openrails.payment_methods.charge_via IS '#795 per-instrument charge routing: pan_proxy (detokenized FPAN through the vault proxy) | network_token (DPAN; gated off on NMI gateways).';
COMMENT ON COLUMN openrails.payment_methods.park_reason IS '#795 instrument park marker (cancellation-last-resort): non-empty = vault-side problem (token deleted/expired, closed account); charges fail loudly, operator notified, subscriptions NEVER terminally cancelled by this.';
COMMENT ON COLUMN openrails.payment_methods.parked_at IS '#795 when the instrument was parked; NULL = not parked.';

CREATE INDEX payment_methods_vault_fingerprint_idx
    ON openrails.payment_methods USING btree (merchant_id, vault_fingerprint) WHERE (vault_fingerprint <> ''::text);

-- #796: credential form presented at charge time — the approval_rate
-- dimension that makes the network-token uplift measurable.
ALTER TABLE openrails.payments
    ADD COLUMN token_type text,
    ADD CONSTRAINT chk_payments_token_type CHECK (((token_type IS NULL) OR (token_type = ANY (ARRAY['network_token'::text, 'pan_via_vault'::text, 'provider_vault'::text]))));

COMMENT ON COLUMN openrails.payments.token_type IS '#796 credential form presented to the network: network_token | pan_via_vault | provider_vault. NULL = unknown/legacy; excluded from token_type-dimensioned metrics.';
