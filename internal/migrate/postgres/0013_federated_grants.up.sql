-- parent: 12 sha256:f763fb5269cea5b859a2c06c1594189edc2c4070e0aebe0e875b5341fc6fb4b7
-- Repair: none-needed A new table; no rows exist.
CREATE TABLE billing.federated_grants (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    email text NOT NULL,
    role text NOT NULL,
    issuer text,
    subject text,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federated_grants_email_check CHECK (email <> '' AND email = lower(email)),
    CONSTRAINT federated_grants_role_check CHECK (role IN ('owner', 'support', 'viewer')),
    CONSTRAINT federated_grants_issuer_check CHECK (issuer <> ''),
    CONSTRAINT federated_grants_subject_check CHECK (subject <> ''),
    CONSTRAINT federated_grants_acceptance_check CHECK ((issuer IS NULL) = (subject IS NULL) AND (subject IS NULL) = (accepted_at IS NULL))
);
COMMENT ON TABLE billing.federated_grants IS 'Merchant roles granted by invitation to users of trusted issuers (#1140): pending (email only) until a user of an issuer trusted for the merchant accepts with that verified email, then bound to (issuer, subject). Grants, not accounts; revoking deletes the row.';

ALTER TABLE ONLY billing.federated_grants
    ADD CONSTRAINT federated_grants_pkey PRIMARY KEY (merchant_id, id);
ALTER TABLE ONLY billing.federated_grants
    ADD CONSTRAINT federated_grants_email_key UNIQUE (merchant_id, email);
ALTER TABLE ONLY billing.federated_grants
    ADD CONSTRAINT federated_grants_subject_key UNIQUE (merchant_id, issuer, subject);
ALTER TABLE ONLY billing.federated_grants
    ADD CONSTRAINT federated_grants_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
CREATE INDEX federated_grants_subject_idx ON billing.federated_grants USING btree (issuer, subject);
CREATE INDEX federated_grants_pending_idx ON billing.federated_grants USING btree (email) WHERE subject IS NULL;
