# Entitlement Timeline Semantics

OpenRails models each entitlement (a plain string, e.g. `"premium"`) as a **timeline of
windows per (customer, entitlement)**. The timeline is the single source of truth for
"does user X have entitlement Y at time T?" — host apps read it for every access decision.

## The row model

`billing.entitlements`: `entitlement`, `customer_id`, `merchant_id`, `starts_at`,
`ends_at` (NULL = indefinite), `source_type` + `source_id`, `grant_id`, `revoked_at` +
`revoke_reason`, `deleted_at`. Windows are half-open `[starts_at, ends_at)`; finite
windows satisfy `starts_at < ends_at`. The wire uses the same names
(`EntitlementRecord`).

A window is **active at T** iff:

```sql
starts_at <= T AND (ends_at IS NULL OR ends_at > T)
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

## Access duration and billing cadence

A recurring price has a positive `billing_interval_hours`, which schedules its
next payment. Each payment independently grants `access_duration_hours` from that
paid phase's start. A finite duration creates a bounded window; null creates a
window with no scheduled expiry. Overlapping paid windows keep their own immutable
boundaries. Renewal appends a grant; it does not stretch earlier paid windows.

Cancellation stops future billing and removes renewal grace, while purchased
access remains until its own expiry. Refunds and explicit access revocations can
remove paid access. A future plan change affects the next grant without shortening
access already purchased on the previous terms.

Engine memberships whose access and billing windows match may receive the
existing bounded renewal allowance (`grace`, min(24h, max(5m, period/10))). An
intentionally shorter access window is not extended through the billing gap;
longer or indefinite paid access is not capped at the billing boundary. Dunning
and renewal-held policy govern grace separately from paid grants.

Date-only CCBill values (`YYYY-MM-DD`) are read as end of that UTC day
(`23:59:59Z`).

## Never infer access from subscription rows

Subscription `status` describes billing lifecycle, not current access. An active
subscription can have a deliberately expired short access window; a canceled
subscription can retain long or indefinite purchased access. Read the entitlement
timeline for access decisions. Grants produce those windows; the windows are the
answer.
