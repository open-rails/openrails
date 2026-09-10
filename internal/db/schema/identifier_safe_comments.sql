-- sqlc-only comment overlays for the immutable, previously applied baseline.
-- Runtime migrations do not load internal/db/schema; this keeps generated Go
-- documentation identifier-safe without changing a migration ledger digest.

COMMENT ON COLUMN openrails.usage_events.resource IS
    'Caller-supplied free-form string for what was metered (for example, an endpoint or plan slug). Opaque to OpenRails; nullable, not a FK.';

COMMENT ON TABLE openrails.imported_dunning_history IS
    'Append-only imported legacy dunning history (#735 import target). Display/forensics evidence only.';

COMMENT ON COLUMN openrails.imported_dunning_history.source IS
    'Legacy origin of the imported row, for example a users log or provider scheduler.';
