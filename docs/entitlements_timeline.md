# Entitlement Timeline Semantics

OpenRails models each entitlement (e.g. `"premium"`) as a **timeline per user**. The host application should treat the timeline as the source of truth for “does user X have entitlement Y at time T?”.

## Write API (stack-like)

Entitlement windows are written via exactly two operations:

- `PushNewEntitlement`: append a new window at the tail of the timeline (can be finite or indefinite).
- `RevokeExistingEntitlement`: immediately remove access by revoking any currently-active window(s) and soft-deleting any future scheduled windows.

`end_at` is treated as immutable after creation: renewals or grace extensions create new windows; they do not modify existing windows.

## Invariants (per `tenant_subject_id` + `entitlement`)

For active (not revoked, not soft-deleted) windows:

- Windows are ordered by `start_at` ascending.
- Finite windows satisfy `start_at < end_at`.
- When OpenRails appends access, it does so at the **tail** of the timeline to avoid overlaps.

Revoked (`revoked_at` set) or soft-deleted (`deleted_at` set) windows are treated as **inactive** and are ignored by active checks.

## Sources

Each entitlement window has a `source_type` + `source_id`:

- `subscription`: paid access, sourced from a subscription row
- `grace`: processor-driven dunning grace windows (e.g., CCBill retry windows)
- `one_off`: one-time purchase sourced from a payment row
- `admin`: admin-granted access sourced from an admin_grants row

## Grace (CCBill)

CCBill retry/dunning “grace” is modeled as **separate `grace` windows** appended to the timeline:

- The paid subscription window still ends at `current_period_ends_at`.
- If CCBill tells us the next retry is after the paid term end (`nextRetryDate`), OpenRails appends `grace` windows up to that retry time.
- On renewal success, OpenRails revokes any currently-active grace windows and deletes any future grace windows for that subscription (so paid windows can proceed cleanly).

### Date-only policy

CCBill provides `nextRetryDate` / `nextRenewalDate` as `YYYY-MM-DD` with no time-of-day. OpenRails interprets these as the end of the given UTC day (`23:59:59Z`) to avoid accidental access gaps due to ambiguity.
