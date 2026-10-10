-- parent: 50 sha256:ff3bf04ba9290d69970a5b7951d651762ee4e55b0f6e45bcb7ed862e22c1e5bd
-- A customer is (merchant_id, issuer, sub) (OIDC Core §5.7): its id is the
-- subject, and issuer pins whose subject it is. NULL is the host's own users.
ALTER TABLE billing.customers
    ADD CONSTRAINT customers_issuer_check CHECK ((issuer IS NULL) OR ((issuer = btrim(issuer)) AND (issuer <> ''::text)));
COMMENT ON TABLE billing.customers IS 'OpenRails payable identity: (merchant_id, issuer, id), id being the issuer''s stable UUID subject. A credential of another issuer never acts on the row.';
COMMENT ON COLUMN billing.customers.issuer IS 'The trusted issuer whose subject id is, fixed when the row is created; NULL is the host''s own users (its native issuer).';
-- Roles a trusted issuer's users hold in a merchant are AuthKit's
-- (group_remote_user_roles).
DROP TABLE billing.federated_grants;
