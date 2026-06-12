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
- `grace`: bounded, revocable generosity windows (renewal grace, dunning/retry grace)
- `one_off`: one-time purchase sourced from a payment row
- `admin`: admin-granted access sourced from an admin_grants row

## Grace

“Grace” is the general primitive for **bounded, revocable generosity**: paid
windows stay truthful (`end_at` = period end, immutable), and any access
beyond what was paid for is a **separate `grace` window** appended to the
timeline (`source_type='grace'`, `source_id` = subscription id). Grace ends
the moment truth arrives: the resolving path revokes any currently-active
grace windows and soft-deletes any future scheduled ones; with no resolution,
grace simply lapses by its own `end_at` — fail-closed eventually. The
timeline’s period ranges are half-open (`[)`), so a grace window starting
exactly at the paid end never overlaps it.

Three producers use the primitive:

- **Renewal grace (#368, NMI-backed + Stripe).** Activation and every renewal
  pre-append a trailing grace window `[period_end, period_end + slack)` where
  slack = half the billing cycle capped at 48h (daily cycles get 12h; not a
  config knob — see `GraceSlack`). Pre-appended (never granted lazily) so
  silence at period end — a lost success webhook, a provider billing on its
  own day boundary, a late webhook — never gates a paying user, regardless of
  worker cadence. Resolution: renewal success revokes the old grace and
  appends the next paid + grace pair; terminal failure / remote-confirmed
  death revokes grace with the cancellation; a **deliberate cancel deletes
  the scheduled grace at cancel time** (access ends at the period end the
  user expects — no generosity for explicit cancellation); still-silence past
  the slack lapses fail-closed while the #367 liveness sync keeps probing and
  can still repair later. Months-stale period ends (imported subscriptions)
  get no resurrection grace — the push is skipped once `period_end + slack`
  is already in the past.
- **CCBill retry grace.** The paid subscription window still ends at
  `current_period_ends_at`; if CCBill reports the next retry after the paid
  term end (`nextRetryDate`), OpenRails appends `grace` windows up to that
  retry time. (CCBill does NOT get the pre-appended renewal grace — its own
  retry cadence drives its grace.)
- **NMI/Solana dunning grace.** When the dunning retry schedule extends past
  the paid term end, the failure path models that access as grace windows up
  to the next retry.

On renewal success (any producer), OpenRails revokes active grace windows and
deletes future ones for that subscription before pushing the next paid window
(so the grace tail never delays or swallows the paid push).

### Date-only policy

CCBill provides `nextRetryDate` / `nextRenewalDate` as `YYYY-MM-DD` with no time-of-day. OpenRails interprets these as the end of the given UTC day (`23:59:59Z`) to avoid accidental access gaps due to ambiguity.
