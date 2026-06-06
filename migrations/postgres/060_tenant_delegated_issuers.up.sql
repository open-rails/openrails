-- =============================================================================
-- 051 — Tenant delegated-token issuer registry (issue #259)
--
-- Switches the browser-direct delegated-token tier from OpenRails-sole-signer
-- (#222: OpenRails mints + verifies with its OWN key) to a FEDERATED /
-- TRUSTED-ISSUER model: each tenant host backend signs
-- its OWN aud=openrails delegated access tokens with its own keypair and
-- publishes a JWKS; OpenRails verifies tenant-signed tokens against the tenant's
-- registered JWKS.
--
-- This table is the trust anchor. It maps each registered ISSUER to the
-- OpenRails tenant it speaks for and the JWKS URL OpenRails fetches its public
-- keys from.
--
-- MANY ISSUERS -> ONE TENANT (required wrinkle): multiple host apps are
-- SEPARATE deployed services with DISTINCT signing keys / JWKS, but they are the
-- SAME OpenRails billing tenant and SHARE one user set. So:
--   * `issuer` is GLOBALLY UNIQUE (a UNIQUE constraint) -> it maps to exactly
--     one tenant. This preserves no-cross-tenant-forgery: an issuer can only
--     ever assert ITS tenant.
--   * a tenant may register MULTIPLE issuer rows (multiple host apps).
-- Because the user set is shared, the host MUST present the tenant's CANONICAL
-- user id as `delegated_sub` so tokens from either issuer resolve to the SAME
-- OpenRails billing account (enforced/documented in the resolver, not here).
--
-- GLOBAL control-plane table (it IS the issuer->tenant directory), like
-- billing.tenants / billing.tenant_secrets / billing.tenant_deks — so it is NOT
-- given RLS / a tenant policy: it is read at startup and on change to build the
-- multi-issuer verifier, never on a tenant-scoped request path.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.tenant_delegated_issuers (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- The OpenRails tenant this issuer speaks for. Many issuers may point at one
    -- tenant; an issuer points at exactly one tenant (issuer is globally unique).
    tenant_id   UUID        NOT NULL
                    REFERENCES billing.tenants (id) ON DELETE CASCADE,

    -- The token `iss` value. GLOBALLY UNIQUE so a given issuer maps to exactly
    -- one tenant (no-cross-tenant-forgery), even though a tenant trusts many.
    issuer      TEXT        NOT NULL,

    -- Where OpenRails fetches this issuer's public keys (JWKS). This is the ONLY
    -- URL ever fetched for the issuer — a token-supplied jwks_uri/iss URL is
    -- never honored (SSRF guard lives in the registration route + verifier).
    jwks_uri    TEXT        NOT NULL,

    -- Per-issuer kill-switch. Disabling evicts the issuer from the live verifier
    -- + JWKS cache WITHOUT affecting the tenant's other issuers. The verifier
    -- only loads enabled rows.
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    CONSTRAINT uq_tenant_delegated_issuers_issuer UNIQUE (issuer)
);

CREATE INDEX IF NOT EXISTS idx_tenant_delegated_issuers_tenant
    ON billing.tenant_delegated_issuers (tenant_id);

-- Hot path at startup / reload: load all enabled issuers.
CREATE INDEX IF NOT EXISTS idx_tenant_delegated_issuers_enabled
    ON billing.tenant_delegated_issuers (enabled)
    WHERE enabled;

COMMENT ON TABLE billing.tenant_delegated_issuers IS
    'Federated delegated-token issuer registry (issue #259). Maps a globally-unique token issuer (iss) to the OpenRails tenant it speaks for and the JWKS URL its public keys are fetched from. MANY issuers -> ONE tenant (multiple host apps = distinct keys, one tenant, shared users). GLOBAL control-plane table, not tenant-scoped.';
COMMENT ON COLUMN billing.tenant_delegated_issuers.issuer IS
    'Token iss value. GLOBALLY UNIQUE -> maps to exactly one tenant (no cross-tenant forgery).';
COMMENT ON COLUMN billing.tenant_delegated_issuers.jwks_uri IS
    'The ONLY URL OpenRails fetches this issuer''s keys from (allowlist; token-supplied URLs never honored).';
COMMENT ON COLUMN billing.tenant_delegated_issuers.enabled IS
    'Per-issuer kill-switch. Only enabled rows are loaded into the live verifier; disabling evicts without affecting sibling issuers of the same tenant.';

-- Application role needs DML (it manages issuers via the service-token-gated route and
-- reloads the verifier). The role + default privileges are created in 050.
GRANT SELECT, INSERT, UPDATE, DELETE ON billing.tenant_delegated_issuers TO openrails_app;
