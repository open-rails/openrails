-- #511 entitlements-on-grants: manually granted entitlements are now `admin`-
-- sourced rows in the append-only grant ledger (openrails.grants) — the single
-- source of truth. The legacy entitlement_grants provenance table is retired.
-- (Per Paul 2026-06-18, the GrantedBy/which-admin audit column is dropped, not
-- migrated; manual grants record source='admin' with no separate actor field.)
DROP TABLE IF EXISTS openrails.entitlement_grants CASCADE;
