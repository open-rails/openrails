# Entitlements

Entitlements answer who has access to what, and until when. A customer holds a
key while a live window of theirs covers a product that grants the key; keys are
derived when read. A merchant's backend checks keys before serving content; staff
grant and revoke products; the remote Go SDK caches allowed answers.

## Concepts

- **Entitlement key**: a plain string of at most 256 bytes
  (`catalog.MaxEntitlementKeyBytes`), granted by products, at most 10,000 live per
  product (`catalog.MaxProductEntitlements`).
- **Key history**: `product_entitlements` keeps each key of a product for
  `[added_at, removed_at)`; a removal closes the row.
- **Access window**: a customer's `[starts_at, ends_at)` of one product
  (`product_access`) from a `purchase`, `subscription`, `grace` or free `grant`,
  each projected from one access grant in the append-only `grants` ledger.
- **Held key**: a key some live window's product grants at T. A window is live
  when `starts_at <= T < ends_at` (or no end), not revoked and not deleted;
  windows may overlap and reads take their union.
- **Seats**: a per-seat window's `quantity`; a check answers the most seats held.
- **`valid_until`**: the next scheduled change that could alter a requested
  answer, null when none.
- **Heavy buyer**: a customer holding at least 500 products
  (`entitlements.HeavyBuyerProducts`), whose keys OpenRails caches.

## How it works

**Writing access.** Checkout, subscriptions and free grants open windows through
`EntitlementService.PushAccess`, under `LockAccessTimeline` (the customer's lock,
then an advisory lock per customer and product). A duration purchase stacks
after the customer's live windows of the product; a subscription period and
renewal grace keep their own interval; a replay consults only its own source.
Free grants are `productaccess.Service.GrantProducts`, all or none, once per
idempotency key. Revocation ends a started window (`revoked_at`) and removes a
future one (`deleted_at`): `RevokeGrantedAccess`, `RevokeRefundedPaymentAccess`,
`RevokeSourcesForSubscriptionAsOf`, `BoundSubscriptionAccess`. Staff grants and
revocations queue the customer's convergence in the same transaction
(`convergejob.Queue`).

**Product keys.** `UpdateProduct` and catalog applications set a product's keys;
an application's `entitlement_replacements` moves a key across every product in
one edit. Each edit reports the keys added and removed and how many customers
held the product, and queues `product.entitlements_changed`
(`billing.ProductEntitlementsChangedEvent`).

**Reading.** Each `EntitlementService.List` call, and the `include_validity`
check, reads one snapshot (`ReadSnapshot`); a `Check` with prefixes makes one
`List` call per page. Exact keys use `CheckDerivedEntitlements`, prefixes
`ListDerivedEntitlementsPage`, the reverse lookup `ListDerivedEntitlementHolders`;
each requires the window and the product's key both live at the instant, so a
past `at` reads the keys a product had then. A current exact-key check with
`include_validity` runs `CheckDerivedEntitlementsWithBoundary`, which adds
`evaluated_at` and `valid_until`, the earliest start or end among the spans that
grant a requested key.

**Heavy-buyer cache.** For a heavy buyer, `List` reads `customer_entitlement_cache`
while its stamps row (`customer_entitlement_cache_stamps`) still matches the
merchant's `entitlement_generation` and the customer's `access_version`, and the
instant falls in `[valid_from, valid_until)`. Statement triggers step those
counters on any change to `product_entitlements` or `product_access`. A current
read that derived a heavy buyer's keys live rebuilds the cache in the background
(`RebuildEntitlementCache`), from one snapshot; a concurrent write fails the
rebuild, which starts over or leaves keys derived live.

**SDK cache.** A remote handle with a static `APIKey` caches `CheckEntitlements`
by default (`entitlementCheckCache`): a bounded 8 MiB in-process LRU, or Redis
when `EntitlementCheckCacheOptions.Redis` is set. Only current exact-key checks
use it; an `Uncached` handle always asks. It stores each allowed key's answer,
never a denial, and answers from the cache only when every requested key is
allowed there; otherwise it asks OpenRails for the whole set, with `include_validity`.
An entry lives `min(MaxAge, valid_until − evaluated_at)`, timed from before the
request was sent; a response naming no merchant (`OpenRails-Bound-Merchant`) is
refused, never stored. `CheckEntitlementsLive` always asks; its answer replaces
allowed entries, drops denied ones and, when it denies a key, advances the
scope's generation, so a fill begun before it cannot restore a dropped entry.

## Interfaces

- **Check**: `Client.CheckEntitlements`, `CheckEntitlementsLive`
  (`POST /v1/app/entitlements/check`, `merchant:entitlements:read`): up to 100
  keys (`billing.MaxEntitlementChecks`) and 10 prefixes at one instant.
- **Cache**: `EntitlementCheckCacheOptions{MaxAge, Disabled, Redis}` on
  `RemoteOptions`; `DefaultEntitlementCheckMaxAge`
  (2 minutes). See [backend](../client.md#caching).
- **Staff** (`merchant:customer:read`, `merchant:customer:manage`):
  `ListEntitlements` (`GET /v1/admin/entitlements`), `ListProductAccess`
  (`GET /v1/admin/product-access`), `CreateProductAccess`
  (`POST /v1/admin/product-access`), `RevokeProductAccess`
  (`POST /v1/admin/product-access/{id}/revoke`), `GetAccessTenure`
  (`GET /v1/admin/customers/{customer_id}/access-tenure`).
- **Customer**: `GET /v1/me/entitlements`, `GET /v1/me/access-tenure`.
- **Catalog and events**: a product's `entitlements`, an application's
  `entitlement_replacements`; `product.entitlements_changed`. Guide:
  [entitlements follow the product](../client.md#entitlements-and-access).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: entitlements call no provider; the payments and subscriptions that open or close windows are made by [checkout](checkout.md), [subscriptions](subscriptions.md) and [refunds](refunds.md).

### 2. Agreement with providers
Access follows what OpenRails recorded of a provider, never a guess. A refund
that revokes access does so in the transaction that records the refund;
canceling a subscription's recurrence never shortens a paid period; an uncertain
or `unverified` subscription keeps its access. Repair rechecks recorded evidence
and never reopens access a recorded revocation ended.
Checks:
- `internal/intents/refund.go` calls `RevokeRefundedPaymentAccess` with the
  refund's transaction (`txDB`).
- `lifecycle.EndAccess` revokes only renewal grace unless the effect sets
  `Revoke` (`internal/modules/subscriptions/effects.go`).
- OR-J27-S05, OR-J27-S07, OR-J27-S08 and OR-J27-S09 cover repair, a late
  chargeback and uncertain state.

### 3. Exact money
Does not apply: windows and keys carry no amounts; credit lots in `grants` belong to [prepaid credits](prepaid-credits.md).

### 4. Access control and tenant isolation
The check route needs `merchant:entitlements:read` (or `root:entitlements:read`)
and the credential's customer scope (`commerceCustomer`, refusing
`service_credential_customer_scope_denied`). Staff routes are admin reads and
writes; `/v1/me/entitlements` and `/v1/me/access-tenure` act as the signed-in
customer. Every query names `merchant_id`, and `product_access` references its
customer by `(merchant_id, customer_id)`. The SDK cache's scope hashes the base
URL, merchant, API key, customer and `MaxAge`, so another key, merchant or stricter
`MaxAge` never reuses an entry, and token credentials never cache.
Checks:
- `CheckEntitlements` in `internal/http/routes/entitlements.go` declares
  `Permission: needEntitlements`; the handler calls `commerceCustomer`.
- `entitlementCacheScopeOf` (`entitlement_check_cache.go`) and `newRemoteHandle`
  (`remote.go`), which enables the cache only for an `APIKey`.
- Every query in `internal/db/queries/entitlement_checks.sql`,
  `entitlement_cache.sql` and `product_access.sql`, advisory locks aside, filters
  on `merchant_id`.
- OR-J27-S13: a key without `merchant:entitlements:read` is refused, and another
  merchant's key never sees a cached answer.
- `TestEntitlementCheckCacheRequiresMetadataAndScope`: an answer without
  `evaluated_at` is not cached, and one naming no merchant is refused.

### 5. Stable contracts
The check, list, product-access and cache types are in `api/go.txt`, the routes
in the route catalog and `api/openapi.json`. `include_validity` was added to the
request; a server older than the SDK refuses it with `unknown_field`, so the
server is deployed first. The in-process `Client` has no cache and always answers
live; a remote client answers alike except that a revocation or seat change can
show up to `MaxAge` late, unless `CheckEntitlementsLive` is used.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `cacheableEntitlementKeys` sends checks with `at`, prefixes or invalid keys to
  the server, so validation answers alike.

### 6. Migrations and retention
All tables are in the baseline. `product_entitlements` and `grants` are permanent;
`product_access` is state and keeps revoked and removed windows; the two cache
tables are derived state, left out of merchant archives. None is partitioned.
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.
- `internal/merchantarchive/checks.go` lists both cache tables as derived.
- Outside the gated merchant purge (`PurgeMerchantRowsProductAccess`), no query
  deletes `product_access` rows: revocation sets `revoked_at` or `deleted_at`.

### 7. Work scales with activity
No job projects or expires keys: they are derived at read time from indexes
(`product_entitlements_entitlement_idx`, `product_access_customer_idx`,
`product_access_product_idx`), and a window ends by its own `ends_at`. Derived
reads use custom plans (`plan_cache_mode`), so a customer with three products
and one with thousands are each planned for their size. The heavy-buyer cache is
rebuilt only after a read misses, and goes stale by counters that triggers step,
not by a sweep. A staff grant or revocation queues one customer's convergence.
Checks:
- `internal/river` has no job that reads `product_access` or
  `product_entitlements` for expiry.
- `rebuildAfterMiss` (`internal/modules/entitlements/cache.go`) runs one rebuild
  per customer and process.
- `step_entitlement_generation` and `step_access_version` are statement
  triggers on `product_entitlements` and `product_access`.

### 8. Abuse protection
The check route takes application credentials and bounds each request (100
keys, 10 prefixes, `prefix_limit` up to 10,000). `middleware.AdminOperationGrant`
meters each invoker by item: 10 a minute, shared by `UpdateCustomer`,
`AdjustCustomerLimit`, `CreateCreditGrants` and `CreateProductAccess` (at most
100 grants a call, `billing.MaxBatchItems`); going over locks the invoker out for
an hour. An application acting as itself is not metered. `/v1/me/entitlements`
takes no OpenRails rate limit; the proxy owns a generic ceiling. SDK Redis calls
wait at most 25 ms (lookups and fills) or 250 ms (a live denial's eviction), at
most 64 outstanding each, so a slow Redis falls back to OpenRails.
Checks:
- `ValidateCheck` (`internal/modules/entitlements/resource_checks.go`) refuses an
  oversize or malformed check before any read.
- The `CreateProductAccess` route sets `Limit: middleware.AdminOperationGrant`.
- `TestEntitlementRedisBudgetLimitsOutstandingWork` bounds outstanding Redis work.

### 9. Destructive-action safeguards
Revoking a window is a `revoke_access` destructive command
(`productaccess.Service.RevokeProductAccessAsOf`, `destructive.RunLocal`), run
under the customer's lock; the window keeps its row and reason. A free grant
records its actor and reason (`grants_access_grant_reason_check`). Changing or
replacing a product's keys is a `catalog_change` command that reports the
holders it affects, and key history is closed, never deleted.
Checks:
- `RevokeProductAccessAsOf` builds its command with `destructive.RevokeAccess`.
- `substantiveCatalogField` (`internal/service/catalog_admission.go`) treats every
  product field except `display_name` and `description` as substantive.
- OR-J27-S02: revoking one source keeps another.

## Tests

Critical journeys: OR-J27-S01 to S04 (window union, revocation, boundaries,
archive and refund), S05 to S09 (subscription closure and repair), S10 to S12
(imported grants), S13 and S14 (the SDK cache: scoped allowed keys, cutoff,
Redis sharing, live denial, a purchase seen at once).

Other tests: `entitlement_check_cache_test.go` drives the cache against a test
server: defaults, allowed keys only, live eviction, an in-flight fill refused
after a live denial, relative lifetime, Redis budgets.

Gaps:
- No scenario covers the heavy-buyer cache, or a product key edit and
  `entitlement_replacements` reaching current holders.
- No scenario reads keys through `ListEntitlements` or `GET /v1/me/entitlements`;
  OR-J01-S02 only checks that they refuse a missing credential.
- `CheckEntitlementsLive` with `at` or prefixes leaves cached entries as they were.
