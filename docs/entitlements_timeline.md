# Entitlement Timeline Semantics

OpenRails models each entitlement (a plain string, e.g. `"premium"`) as a **timeline of
windows per (customer, entitlement)**. The timeline is the single source of truth for
"does user X have entitlement Y at time T?" — host apps read it for every access decision.

## The row model

`openrails.entitlements`: `entitlement`, `customer_id`, `merchant_id`, `start_at`,
`end_at` (NULL = indefinite), `source_type` + `source_id`, `grant_id`, `revoked_at` +
`revoke_reason`, `deleted_at`. Windows are half-open `[start_at, end_at)`; a generated
`period` tstzrange plus an exclusion constraint forbids overlap among active windows of
one (merchant, customer, entitlement). Finite windows satisfy `start_at < end_at`.

A window is **active at T** iff:

```sql
start_at <= T AND (end_at IS NULL OR end_at > T)
AND revoked_at IS NULL AND deleted_at IS NULL
```

Revoked or soft-deleted windows are inactive and ignored by every active check.

## Querying access

Merchant API (API key or service token carrying `merchant:customer-settings:read`;
prefix `/v1` standalone, `/billing/v1` embedded):

- `POST /v1/merchant/customers/entitlements:batch` body `{"subjects": [...], "at": "RFC3339"}` —
  the primary host check. Always batch (max 500 subjects); response is keyed by subject,
  unknown subjects get `[]`, never an error. Omitted `at` = now.
- `GET /v1/merchant/customers/{customer_id}/entitlements?at=` — active windows for one customer.
- `GET /v1/merchant/entitlements/{entitlement}/customers?at=&cursor=&limit=` — reverse lookup:
  customer ids holding an active window, keyset-paginated (`next_cursor`/`has_more`).
- `GET /v1/me/entitlements/active?at=` — the delegated end-user token reads its own subject.

Embedded hosts sharing the DB may run the SQL predicate above directly
(add `customer_id = $1 AND entitlement = $2`); it is exactly what the API executes.

## Write API (stack-like)

Exactly two operations mutate a timeline (serialized per timeline by an advisory lock):

- `PushNewEntitlement` — append a window at the **tail** (finite or indefinite).
- `RevokeExistingEntitlement` — revoke currently-active window(s) and soft-delete future
  scheduled ones.

`end_at` is immutable after creation: renewals and grace extensions append new windows,
never edit existing ones.

## Grants vs entitlements

The **grant ledger** (`openrails.grants`, #514) is the append-only access-domain sibling of
the money ledger. Derive-1 appends immutable events (grant / revoke / expire / supersede —
a revoke is a NEW event referencing the original); derive-2 (`MaterializeGrant`) folds the
log into projections: **entitlement windows** (rows carry the producing `grant_id`), credit
lots, and derived ownership for bundle includes. Grants are provenance and replayable
truth; entitlement rows are the projection you query. The Convergence Engine's `derive.*`
pass repairs any drift between the two.

## Sources

`source_type` + `source_id` on each window: `subscription` (paid access from a subscription),
`one_off` (a one-time purchase), `admin` (admin-granted; source is the grant itself), and
`grace` (historical — see below).

## Standing access — auto-renew subscriptions have no end date

An auto-renew subscription's entitlement window is **standing**: open-ended, closed only
by a proven event (a confirmed cancellation, a terminal decline, exhausted dunning — never
by the clock alone). A lost webhook, a provider billing on its own day boundary, or a dead
webhook pipe therefore cannot gate a paying user: access simply continues while
reconciliation converges the subscription against provider truth. This replaced the older
appended-grace-window mechanism (#368, deleted by #691) — `grace` remains in the source
vocabulary for historical rows and as a pacing marker in convergence, but no code appends
grace windows today. Deliberate cancellation still ends access at the period end the user
expects.

Date-only CCBill values (`YYYY-MM-DD`) are read as end of that UTC day (`23:59:59Z`) to
avoid access gaps from ambiguity.

## Never infer access from subscription rows

Subscription `status` is provider-lifecycle state, not an access decision: `past_due` and
`unknown` still project standing access (providers like NMI retry indefinitely and forgive
gaps; stale data parks as `unknown` rather than losing entitlements to a malfunction).
Cancellation is last-resort and evidence-driven. All of that doctrine is already folded
into the timeline — subscriptions *produce* windows; the windows are the answer.
