# Entitlement Timeline Semantics

OpenRails models each entitlement (a plain string, e.g. `"premium"`) as a **timeline of
windows per (customer, entitlement)**. The timeline is the single source of truth for
"does user X have entitlement Y at time T?" — host apps read it for every access decision.

## The row model

`billing.entitlements`: `entitlement`, `customer_id`, `merchant_id`, `start_at`,
`end_at` (NULL = indefinite), `source_type` + `source_id`, `grant_id`, `revoked_at` +
`revoke_reason`, `deleted_at`. Windows are half-open `[start_at, end_at)`; finite
windows satisfy `start_at < end_at`. On the wire the bounds are `starts_at` and
`ends_at` (`EntitlementRecord`).

A window is **active at T** iff:

```sql
start_at <= T AND (end_at IS NULL OR end_at > T)
AND revoked_at IS NULL AND deleted_at IS NULL
```

Revoked or soft-deleted windows are inactive and ignored by every active check.

## Querying access

Merchant API (API key or service token carrying `merchant:customer-settings:read`;
prefix `/v1` standalone, `/billing/v1` embedded):

- `POST /v1/merchant/entitlements/lookup` body `{"customer_ids": [...], "at": "RFC3339"}` —
  the primary host read (max 500 customers); `{"customers": {id: [EntitlementRecord]}}`,
  a customer with none maps to `[]`. Omitted `at` = now.
- `POST /v1/merchant/customers/{customer_id}/entitlements/check` body `{"entitlements": [...], "at"}` —
  `{"entitlements": {key: bool}}` for up to 100 keys (Go: `HasEntitlement`).
- `GET /v1/merchant/entitlements/{entitlement}/customers?at=&cursor=&limit=` — reverse lookup:
  one page of customer ids holding an active window (`{data, next_cursor}`).
- `GET /v1/me/entitlements?at=` — the signed-in customer's own active windows.

Embedded hosts sharing the DB may run the SQL predicate above directly
(add `customer_id = $1 AND entitlement = $2`); it is exactly what the API executes.

## Writes

A timeline changes in two ways only: a window is appended at its tail, or active
windows are revoked (and future scheduled ones removed). A window's end is immutable:
a renewal appends a new window, never edits one.

Every window derives from a grant. A merchant grants one by hand with
`POST /v1/merchant/customers/{customer_id}/entitlements` (`Client.CreateEntitlement`)
and revokes it with `DELETE /v1/merchant/customers/{customer_id}/entitlements/{id}`
(`Client.DeleteEntitlement`), which revokes the grant behind it.

## Grants vs entitlements

The **grant ledger** (`billing.grants`) is the append-only access-domain sibling of
the money ledger. Derive-1 appends immutable events (grant / revoke / expire / supersede —
a revoke is a NEW event referencing the original); derive-2 (`MaterializeGrant`) folds the
log into projections: **entitlement windows** (rows carry the producing `grant_id`),
credit lots and product ownership. Grants are provenance and replayable
truth; entitlement rows are the projection you query. The Convergence Engine's `derive.*`
pass repairs any drift between the two.

## Sources

`source_type` + `source_id` on each window: `subscription` (paid access from a subscription),
`purchase` (a one-time purchase), `admin` (granted by the merchant; the source is the grant
itself), and `grace` (see below).

## Standing access — auto-renew subscriptions have no end date

An auto-renew subscription's entitlement window is **standing**: open-ended, closed only
by a proven event (a confirmed cancellation, a terminal decline, exhausted dunning — never
by the clock alone). A lost webhook, a provider billing on its own day boundary, or a dead
webhook pipe therefore cannot gate a paying user: access simply continues while
reconciliation converges the subscription against provider truth. `grace` remains in the
source vocabulary as a pacing marker in convergence; no code appends grace windows for
provider cohorts. Deliberate cancellation still ends access at the period
end the user expects.

Engine memberships (OpenRails collects them itself) are the exception: access is the paid
period plus a bounded renewal allowance (`grace`, min(24h, max(5m, period/10))) that holds access across the
boundary until the engine's own renewal decides. The renewal supersedes it; a decline or
cancellation revokes it. A renewal with no outcome past the allowance is held (collection is
stopped): by default access continues until it is attempted; `access_while_renewal_held: suspend`
ends access when the allowance lapses.

Date-only CCBill values (`YYYY-MM-DD`) are read as end of that UTC day (`23:59:59Z`) to
avoid access gaps from ambiguity.

## Never infer access from subscription rows

Subscription `status` is provider-lifecycle state, not an access decision: `past_due` and
`unverified` still project standing access (providers like NMI retry indefinitely and forgive
gaps; stale data parks as `unverified` rather than losing entitlements to a malfunction).
Cancellation is last-resort and evidence-driven. All of that doctrine is already folded
into the timeline — subscriptions *produce* windows; the windows are the answer.
