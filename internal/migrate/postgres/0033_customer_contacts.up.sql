-- parent: 32 sha256:d658673718a6d8cd3c4b839579c2a15e0e4c0fcd753afd1cf7f19690166450bf
-- Repair: none-needed New tables have no rows; the dropped columns leave with their values.
-- Who a customer is now comes from the host's directory: in process when
-- embedded, else the copy its directory pushes (SCIM) or a verified token's
-- claims record. OpenRails no longer keeps declared contact facts, nor a
-- blocked flag: the host's auth decides who may sign in.
DROP INDEX billing.customers_username_idx;
ALTER TABLE billing.customers DROP COLUMN email;
ALTER TABLE billing.customers DROP COLUMN username;
ALTER TABLE billing.customers DROP COLUMN blocked;

CREATE TABLE billing.customer_contacts (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    email text,
    display_name text,
    user_name text,
    active boolean,
    provisioned_at timestamp with time zone,
    directory_updated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_contacts_email_check CHECK (((email IS NULL) OR ((email = btrim(email)) AND (email <> ''::text) AND (octet_length(email) <= 320)))),
    CONSTRAINT customer_contacts_display_name_check CHECK (((display_name IS NULL) OR ((display_name = btrim(display_name)) AND (display_name <> ''::text) AND (octet_length(display_name) <= 256)))),
    CONSTRAINT customer_contacts_user_name_check CHECK (((user_name IS NULL) OR ((user_name = btrim(user_name)) AND (user_name <> ''::text) AND (octet_length(user_name) <= 256))))
);
COMMENT ON TABLE billing.customer_contacts IS 'A customer''s contact as the merchant''s directory last reported it, when OpenRails is not embedded beside that directory: pushed over SCIM, or recorded from a verified access token''s claims. Newest wins by directory_updated_at. Erasure keeps the row without its values, so an older report cannot restore them.';
COMMENT ON COLUMN billing.customer_contacts.active IS 'SCIM active, shown and never enforced: the host''s auth decides who signs in. NULL when no SCIM client reported it.';
COMMENT ON COLUMN billing.customer_contacts.provisioned_at IS 'When a SCIM client created this User (its meta.created); NULL when no SCIM client holds it: claims alone recorded it, or it was deleted.';
COMMENT ON COLUMN billing.customer_contacts.directory_updated_at IS 'When the directory last changed these values, as it reported (a SCIM resource''s meta.lastModified, a token''s updated_at claim), else when OpenRails received a SCIM write. NULL when claims without updated_at recorded it: any dated report replaces it. An older report is ignored.';
COMMENT ON COLUMN billing.customer_contacts.updated_at IS 'When OpenRails last wrote the contact: its SCIM meta.lastModified, the customer read''s synced_at.';

ALTER TABLE ONLY billing.customer_contacts
    ADD CONSTRAINT customer_contacts_pkey PRIMARY KEY (merchant_id, customer_id);
ALTER TABLE ONLY billing.customer_contacts
    ADD CONSTRAINT customer_contacts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE;
CREATE UNIQUE INDEX customer_contacts_user_name_key ON billing.customer_contacts USING btree (merchant_id, lower(user_name)) WHERE (provisioned_at IS NOT NULL);
CREATE INDEX customer_contacts_email_idx ON billing.customer_contacts USING btree (merchant_id, lower(email)) WHERE (email IS NOT NULL);
CREATE INDEX customer_contacts_provisioned_idx ON billing.customer_contacts USING btree (merchant_id, provisioned_at, customer_id) WHERE (provisioned_at IS NOT NULL);

CREATE TABLE billing.provisioning_tokens (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    name text NOT NULL,
    declared boolean NOT NULL,
    token_sha256 bytea NOT NULL,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT provisioning_tokens_name_check CHECK (((name = btrim(name)) AND (name <> ''::text) AND (octet_length(name) <= 128))),
    CONSTRAINT provisioning_tokens_token_sha256_check CHECK ((octet_length(token_sha256) = 32))
);
COMMENT ON TABLE billing.provisioning_tokens IS 'Bearer tokens a merchant''s directory presents at /scim/v2, stored as their SHA-256. Revoking deletes the row.';
COMMENT ON COLUMN billing.provisioning_tokens.declared IS 'The merchant declaration''s secrets.scim_token: at most one per merchant, replaced when the declaration changes it.';

ALTER TABLE ONLY billing.provisioning_tokens
    ADD CONSTRAINT provisioning_tokens_pkey PRIMARY KEY (merchant_id, id);
ALTER TABLE ONLY billing.provisioning_tokens
    ADD CONSTRAINT provisioning_tokens_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX provisioning_tokens_token_sha256_key ON billing.provisioning_tokens USING btree (token_sha256);
CREATE UNIQUE INDEX provisioning_tokens_declared_key ON billing.provisioning_tokens USING btree (merchant_id) WHERE declared;
