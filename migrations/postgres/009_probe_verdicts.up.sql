-- #348 probe-cooldown cache: persist the NMI test-mode boot probe verdict so a
-- restart loop does not pay one declined authorization (and risk card-testing
-- flags at the gateway) on every boot.
--
-- openrails.probe_verdicts is keyed by (provider, key_hash) where key_hash is
-- the sha256 of the security key: rotating the credential always re-probes
-- (different hash = no row). Verdicts are consulted by build_runtime under
-- test_env:
--   * a 'live' verdict younger than the cooldown refuses to boot FROM CACHE
--     without re-probing (a crash-looping supervisor stops costing declines);
--   * a 'simulated' verdict younger than the cooldown skips the probe;
--   * anything older (or any read/write failure) degrades to probing as before.
--
-- DELIBERATELY RLS-EXEMPT (global): processor credentials are instance-level
-- runtime configuration, not tenant data — the probe runs at boot, before any
-- tenant-pinned connection exists, and the verdict describes the gateway
-- account, not a tenant. No tenant_id column, no RLS policy; access control is
-- the ordinary role grant, mirroring other non-tenant runtime state.

CREATE TABLE openrails.probe_verdicts (
    provider text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT probe_verdicts_pkey PRIMARY KEY (provider, key_hash),
    CONSTRAINT chk_probe_verdicts_verdict CHECK ((verdict = ANY (ARRAY['live'::text, 'simulated'::text])))
);

COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts (#348): one row per (provider, sha256(security_key)). Fresh ''live'' refuses boot from cache, fresh ''simulated'' skips the probe, stale/missing re-probes. RLS-exempt by design: instance-level credential state, not tenant data.';
COMMENT ON COLUMN openrails.probe_verdicts.key_hash IS 'sha256 hex of the provider security key. A rotated key hashes differently, so the cache never answers for a credential it has not seen.';

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.probe_verdicts TO openrails_app;
