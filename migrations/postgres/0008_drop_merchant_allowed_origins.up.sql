-- #765: drop the per-merchant browser CORS allow-list. Ruling: browser-tier
-- requests are authorized by bearer JWTs, never ambient cookies, so an origin
-- allowlist protected nothing (a stolen token replays fine from curl, where
-- CORS doesn't exist) — it was pure settings-surface overhead. The static
-- replacement policy (checkout/self-service = `*`, everything else = no CORS
-- headers) is engine code (internal/http/middleware.PermissiveCORSHTTP), not
-- per-merchant config.
--
-- api_host (added alongside allowed_origins in 0006, #734) is UNRELATED and
-- stays: Host->merchant resolution and the Host-routed webhook mount still
-- depend on it.
ALTER TABLE openrails.merchants
    DROP COLUMN allowed_origins;
