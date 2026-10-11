-- The shop's own accounts, in its own schema beside OpenRails' tables.
-- OpenRails never reads them: the Authenticator in auth.go does.
CREATE SCHEMA IF NOT EXISTS shop;

CREATE TABLE IF NOT EXISTS shop.users (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    email text NOT NULL UNIQUE CHECK (email <> '')
);

-- A role at one merchant, or at every merchant (a root role) when
-- merchant_id is NULL. What each role holds is in auth.go.
CREATE TABLE IF NOT EXISTS shop.roles (
    user_id uuid NOT NULL REFERENCES shop.users (id),
    merchant_id uuid,
    role text NOT NULL CHECK (role IN ('admin', 'support', 'reader')),
    UNIQUE NULLS NOT DISTINCT (user_id, merchant_id, role)
);

-- A sign-in. Its bearer token is random and only its SHA-256 is kept.
-- merchant_id is the merchant the session acts at: the store a shopper
-- signed in at, or the one staff switched to; NULL is none.
CREATE TABLE IF NOT EXISTS shop.sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    token_sha256 bytea NOT NULL UNIQUE,
    user_id uuid NOT NULL REFERENCES shop.users (id),
    merchant_id uuid,
    signed_in_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
