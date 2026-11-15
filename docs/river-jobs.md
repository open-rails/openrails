# OpenRails background jobs and events audit

Source snapshot: `origin/master` at `88a70db5ac000a8d27802bbe806ab7ca0eec9cda`
(v0.279.0), inspected 2026-11-15. This is a source inventory, not a report of
jobs currently present in any deployed database.

This audit covers core River workers, periodic billing scans, scheduled wake-ups,
provider operations, business events, and jobs contributed by the standalone
server's pinned dependencies. Triggers, effects, configuration gates, and source
references are being traced in this draft.
