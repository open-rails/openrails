-- #591: platform identity anchors. These are additive, portable AuthKit links:
-- opaque permission-group ids only, no FK into AuthKit/profiles.
CREATE TABLE openrails.customer_anchors (
    permission_group_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT customer_anchors_pkey PRIMARY KEY (permission_group_id),
    CONSTRAINT customer_anchors_permission_group_nonempty CHECK (btrim(permission_group_id) <> '')
);

COMMENT ON TABLE openrails.customer_anchors IS '#591 customer permission-group anchor. The id is the AuthKit customer permission-group id, stored as opaque text with no FK into AuthKit.';
COMMENT ON COLUMN openrails.customer_anchors.permission_group_id IS 'Opaque AuthKit customer permission-group id. Bills by group identity, never by user_id.';

CREATE TABLE openrails.merchant_anchors (
    permission_group_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT merchant_anchors_pkey PRIMARY KEY (permission_group_id),
    CONSTRAINT merchant_anchors_permission_group_nonempty CHECK (btrim(permission_group_id) <> '')
);

COMMENT ON TABLE openrails.merchant_anchors IS '#591 merchant permission-group anchor. The id is the AuthKit merchant permission-group id, stored as opaque text with no FK into AuthKit.';
COMMENT ON COLUMN openrails.merchant_anchors.permission_group_id IS 'Opaque AuthKit merchant permission-group id. Mirrors openrails.merchants.permission_group_id without re-keying the existing merchant directory.';

-- Safe merchant_external_account merge floor: provider_accounts already has the
-- merchant/provider/env/account/secret/role shape. Add only owner now; a table
-- rename would churn query code and existing FKs for no behavioral gain.
ALTER TABLE openrails.provider_accounts
    ADD COLUMN owner text DEFAULT 'merchant'::text NOT NULL,
    ADD CONSTRAINT provider_accounts_owner_check CHECK (owner = ANY (ARRAY['merchant'::text, 'platform'::text]));

COMMENT ON TABLE openrails.provider_accounts IS 'Merchant external account registry (#591; historical table name provider_accounts). A row is one merchant x provider account; owner distinguishes merchant-owned rails from future platform-owned rails.';
COMMENT ON COLUMN openrails.provider_accounts.owner IS '#591 external-account owner: merchant = the merchant owns/vaults through this rail account; platform = OpenRails platform account.';
