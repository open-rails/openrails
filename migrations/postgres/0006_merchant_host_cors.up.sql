-- #734: per-merchant API host + browser CORS allow-list, engine-owned.
--
-- Both columns live on the GLOBAL merchant directory (same table
-- merchantForIssuer's authority chain already reads), never AuthKit's
-- remote_application (CORS is browser transport policy, not federation
-- identity) and never a boot-time engine-config list (a merchant registered
-- or reconfigured on any node must resolve immediately on every other node
-- sharing this database).
--
-- api_host is the canonical Host-header value ("api.example.com", no
-- scheme/port) a request resolves this merchant from — the ONE mechanism
-- shared by per-merchant CORS preflight and the Host-routed webhook mount
-- (pkg/embedded). NULL = this merchant does not resolve from any Host (the
-- default; single-merchant self-hosters see no behavior change without
-- opting in). Globally unique when set.
ALTER TABLE openrails.merchants
    ADD COLUMN api_host text,
    ADD COLUMN allowed_origins text[] DEFAULT ARRAY[]::text[] NOT NULL;

COMMENT ON COLUMN openrails.merchants.api_host IS 'Canonical Host-header value this merchant resolves from (#734), e.g. "api.acme.example". NULL = no Host resolution for this merchant. Lowercase, no scheme/port.';

COMMENT ON COLUMN openrails.merchants.allowed_origins IS 'Browser CORS allow-list answered ONLY for preflight against this merchant''s resolved Host (#734). Empty = no browser CORS for this merchant, even if api_host is set.';

CREATE UNIQUE INDEX uq_merchants_api_host ON openrails.merchants (api_host) WHERE api_host IS NOT NULL AND deleted_at IS NULL;
