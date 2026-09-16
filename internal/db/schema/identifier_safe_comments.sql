-- sqlc-only comment overlays for the immutable, previously applied baseline.
-- Runtime migrations do not load internal/db/schema; this keeps generated Go
-- documentation identifier-safe without changing a migration ledger digest.

COMMENT ON COLUMN openrails.usage_events.resource IS
    'Caller-supplied free-form string for what was metered (for example, an endpoint or plan slug). Opaque to OpenRails; nullable, not a FK.';
