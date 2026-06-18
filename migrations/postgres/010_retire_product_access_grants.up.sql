-- #511 ownership-on-grants: product ownership is now a kind=ownership row in the
-- append-only grant ledger (openrails.grants) — the single source of truth for
-- all grant kinds. The legacy product_access_grants table is retired; its repo
-- (internal/db/repo/product_access_grant.go) reads/writes the grant ledger now.
DROP TABLE IF EXISTS openrails.product_access_grants CASCADE;
