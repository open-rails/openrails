# Entitlements and limits

Entitlements and limits answer what a customer may use and how much. A customer
holds a key while a live access window of theirs covers a product that grants the
key; keys are derived when read. Declared limits, the prepaid balance and spend
caps bound consumption over a period: an admission sets a hold aside before work
starts, and the Go SDK's gate decides on views of where the customer stands,
overshooting by at most a few seconds of other gates' traffic. A merchant's backend checks keys and limits;
staff grant and revoke products; the remote Go SDK caches allowed answers. What
usage costs is [usage billing](usage-billing.md).

## Concepts

- **Entitlement key**: a plain string of at most 256 bytes
  (`catalog.MaxEntitlementKeyBytes`), granted by products, at most 10,000 live per
  product (`catalog.MaxProductEntitlements`).
- **Key history**: `product_entitlements` keeps each key of a product for
  `[added_at, removed_at)`; a removal closes the row.
- **Access window**: a customer's `[starts_at, ends_at)` of one product
  (`product_access`) from a `purchase`, `subscription`, `grace` or free `grant`,
  each projected from one access grant in the append-only `grants` ledger. It is
  live when `starts_at <= T < ends_at` (or no end), not revoked and not deleted;
  windows may overlap and reads take their union. A **held key** is one some live
  window's product grants at T.
- **Seats**: a per-seat window's `quantity`; a check answers the most seats held.
- **`valid_until`**: the next scheduled change that could alter a requested
  answer, null when none.
- **Limit**: a declared cap in a named unit or a currency, with a reset and an overrun, `require_estimate` or `admit_while_positive` (`catalog_limits`). A named-unit limit's `counts` add each event's quantity × `weight` ÷ `per`, rounded half up, per meter and optional `filter` (`limits.Units`); a currency limit counts the rated amount. Products give it capacity (`catalog_product_limits`); a customer override replaces their sum (`customer_limit_overrides`).
- **Reset kinds**: `fixed` (tiles of `window_seconds` from the subscription start
  or the first use), `billing_period`, `session` (opens at the first use),
  `rolling` (60 buckets), `rate` (refills continuously), `lifetime`.
- **`balance`**: the built-in limit per currency, prepaid balance plus credit line
  (`catalog.BalanceLimit`), never declared. A billing policy's **spend window** is
  a fixed currency window kept as a limit window keyed `spend:` and a hash.
- **Limit window**: one customer's settled units of one limit in one window
  (`limit_windows`), placed by the server clock.
- **Hold**: capacity set aside for one admitted request in one currency
  (`limit_holds`), one part per limit and window (`limit_hold_parts`). A gate's
  request near a limit is a hold of its lease (`lease_id`), with no deadline.
- **Lease**: a gate's (or a batch job's) usage stream, in-flight report
  (`lease_in_flight`) and request holds (`leases`), one per process. It is live
  for 10 seconds after its last sync; a later sync makes it live again.
- **View**: where one customer stands on every limit its requests may claim, as
  one sync reads it (`billing.View`); it reserves nothing.

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
held the product, and queues `product.entitlements_changed`.

**Reading.** Each `EntitlementService.List` call, and the `include_validity`
check, reads one snapshot (`ReadSnapshot`); a `Check` with prefixes makes one
`List` call per page. Exact keys use `CheckDerivedEntitlements`, prefixes
`ListDerivedEntitlementsPage`, the reverse lookup `ListDerivedEntitlementHolders`;
each requires the window and the product's key both live at the instant, so a
past `at` reads the keys a product had then. A current exact-key check with
`include_validity` runs `CheckDerivedEntitlementsWithBoundary`, which adds
`evaluated_at` and `valid_until`, the earliest start or end among the spans that
grant a requested key.

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

**Lease sync.** A gate settles usage and asks for views on a lease sync
(`POST /v1/app/leases/sync`, `leases.Syncer.Sync`): one transaction, bounded by
`limits.SyncTimeout` (5 s) and a 2 s lock budget. It locks the lease (creating it
on the first sync) and the customers of its usage and releases in id order,
settles new usage ([usage billing](usage-billing.md)), releases the request holds
it names, replaces its in-flight report (changed rows only), answers views,
limit sets and pricing (`views.go`), and stores `settled_through_n` and receipts.
Views read usage only through `leases.UsageReader`.

**Holds.** Every writer of holds and windows locks the customer (`LockCustomerForSpend`, `FOR NO KEY UPDATE`, so a row naming the customer is written without waiting for it) and takes window rows in one order (`limits.Holder.Take`). A claim fits by its overrun (`limits.Fits`). A request is checked against `balance`, its spend windows (`spendgate.SpendClaims`), then its declared limits; held is request holds plus what live leases report in flight. `admission.Admitter` adds delinquency, the accrual-rate cap (`AccrualRateMeter`, summing `usage_minutes`) and the delegated-invoker cutoff. Settlement (`settleLimitsTx`) adds units to their windows in full, never clamped: a request hold's windows, or those current when the usage was admitted (`admitted_at`), and the spend windows of the invoker attributes it reports (none when its policy cannot be resolved, such as during an FX outage). A lease's report and holds count while it is live (`limits.LeaseLiveSince`), and a lease's hold also for a lease lifetime after it is made; no hold counts past 30 days (`limits.HeldSince`). A view request a sync cannot answer is refused alone (`refused_views`).

**The gate.** `openrails.NewGate` keeps one lease for its life and a view of each customer it serves (`gate_view.go`). A warm decision prices the request (named units by the view's counts, money at its `PriceBound`s) and takes it from every limit of its set or none, against the view less what the gate has in flight, with no I/O. Within a margin of four sync intervals of what the views show others adding it is decided on the server as a request hold of the lease; refused locally when it cannot fit. Tickets open across a sync, and usage a sync left unsettled, are reported in flight. A view older than two syncs decides one request until a sync answers; with OpenRails unreachable a view answered when the outage began keeps deciding within the gate's share (remaining under the request's policy, less running charges since, ÷ live leases). A view with anything in flight is never dropped. An idle gate stops syncing. Captured usage goes on the next sync, stored first by `GateDeps.Outbox`.

## Interfaces

- **Check**: `Client.CheckEntitlements`, `CheckEntitlementsLive`
  (`POST /v1/app/entitlements/check`, `merchant:entitlements:read`): up to 100
  keys (`billing.MaxEntitlementChecks`) and 10 prefixes at one instant. Cache:
  `EntitlementCheckCacheOptions{MaxAge, Disabled, Redis}` on `RemoteOptions`,
  `DefaultEntitlementCheckMaxAge` (2 minutes); see [backend](../client.md#caching).
- **Gate**: `openrails.NewGate(client, GateDeps{Outbox})`; `Gate.Admit`,
  `Consume`, `Record`, `ListCustomerLimits`, `Flush`, `Close`; `Ticket.Capture`, `Release`,
  `Refusal`; `UsageOutbox`; `ErrUnavailable` ([the gate](../client.md#limits-admissions-and-the-gate)).
- **Limits and admissions** (`merchant:usage:manage`, reads `merchant:usage:read`):
  `SyncLeases` (`POST /v1/app/leases/sync`); `Admit` (`POST /v1/app/admissions`),
  `CaptureAdmissions`, `ReleaseAdmissions`, `ExtendAdmissions`, `GetAdmission`,
  `ListAdmissions`; `ListCustomerLimits` (`GET /v1/app/limits`); in a host
  transaction (`LocalClient`) `AdmitTx`, `CaptureAdmissionsTx`, `ReleaseAdmissionsTx`.
- **Staff** (`merchant:customer:read`, `merchant:customer:manage`):
  `ListEntitlements` (`GET /v1/admin/entitlements`), `ListProductAccess`,
  `CreateProductAccess` (`GET`/`POST /v1/admin/product-access`),
  `RevokeProductAccess` (`POST /v1/admin/product-access/{id}/revoke`),
  `GetAccessTenure` (`GET /v1/admin/customers/{customer_id}/access-tenure`);
  `UpdateCustomer` (`limits`, `credit_limits`, `billing_policy`);
  `AdjustCustomerLimit` (`POST /v1/admin/customers/{customer_id}/limit-adjustments`).
- **Customer**: `GET /v1/me/entitlements`, `/v1/me/access-tenure`, `/v1/me/limits`.
- **Catalog, metrics and events**: a product's `entitlements`, an application's
  `entitlement_replacements`; `ListLimits`, `GetLimit`, `SetLimit`
  (`PUT /v1/catalog/limits/{key}`); `limits_reached` (`POST /v1/metrics/query`);
  `product.entitlements_changed` (`billing.ProductEntitlementsChangedEvent`),
  `balance.low`, `balance.depleted`. Guide:
  [entitlements follow the product](../client.md#entitlements-and-access).
- **Configuration**: merchant `billing_policies`
  ([spend limits](../client.md#limits-admissions-and-the-gate)).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: entitlements and limits call no provider; the payments and subscriptions that open or close windows are made by [orders and checkout](orders.md), [subscriptions](subscriptions.md) and [refunds](refunds.md).

### 2. Agreement with providers
Limits, leases and holds come from the merchant's backend, not a provider. Access
follows what OpenRails recorded of a provider, never a guess. A refund that
revokes access does so in the transaction that records the refund; canceling a
subscription's recurrence never shortens a paid period; an uncertain or
`unverified` subscription keeps its access. Repair rechecks recorded evidence and
never reopens access a recorded revocation ended.
Checks:
- `internal/intents/refund.go` calls `RevokeRefundedPaymentAccess` with the
  refund's transaction (`txDB`).
- `lifecycle.EndAccess` revokes only renewal grace unless the effect sets
  `Revoke` (`internal/modules/subscriptions/effects.go`).
- OR-J27-S05, OR-J27-S07, OR-J27-S08 and OR-J27-S09 cover repair, a late
  chargeback and uncertain state.

### 3. Exact money
Access windows and keys carry no amounts; credit lots (`credit_grants`) belong to
[prepaid credits](prepaid-credits.md). Holds and limit windows are `int64` native
units of one currency each. The one FX use converts a spend window's cap declared
in another currency to the request's (`SpendgatePolicyLoader.convert`): a cap,
never a ledger amount.
Checks:
- No `fx` import in `internal/modules/leases` or `internal/modules/limits`.

### 4. Access control and tenant isolation
The check route needs `merchant:entitlements:read` (or `root:entitlements:read`);
lease, admission and limit routes the `merchant:usage:manage` or
`merchant:usage:read` pair; adjustments `merchant:customer:manage`. Both obey the
credential's customer scope (`commerceCustomer`, `Syncer.Allows`), refusing
`service_credential_customer_scope_denied`; a sync refuses that item alone and
never locks the customer. `/v1/me` routes act as the signed-in customer. Every
query names `merchant_id`; `product_access` references its customer by
`(merchant_id, customer_id)`, and a lease id is per merchant
(`PRIMARY KEY (merchant_id, id)`). The SDK cache's scope hashes the base URL,
merchant, API key, customer and `MaxAge`, so another key, merchant or stricter
`MaxAge` never reuses an entry, and token credentials never cache.
Checks:
- `CheckEntitlements` in `internal/http/routes/entitlements.go` declares
  `Permission: needEntitlements`; the handler calls `commerceCustomer`.
- Each `/v1/app` route in `internal/http/routes/limits.go` and each admission route
  in `metering.go` has a `Permission` pair; `AdjustCustomerLimit` is
  `Group: Admin, Level: LevelUpdate`.
- `syncRun.usageItem` and `syncRun.lockCustomers` apply `Syncer.Allows`.
- `entitlementCacheScopeOf` (`entitlement_check_cache.go`) and `newRemoteHandle`
  (`remote.go`), which enables the cache only for an `APIKey`.
- Every query in `internal/db/queries/entitlement_checks.sql`, `product_access.sql`, `limits.sql`, `leases.sql` and `limit_holds.sql`, advisory locks aside, filters on `merchant_id`.
- OR-J27-S13: a key without `merchant:entitlements:read` is refused, and another
  merchant's key never sees a cached answer. OR-J23-S11 and OR-J24-S03 keep limit
  reads, adjustments and refusals in their merchant.
- `TestEntitlementCheckCacheRequiresMetadataAndScope`: an answer without
  `evaluated_at` is not cached, and one naming no merchant is refused.

### 5. Contracts match the code
Their types are in `api/go.txt`, the routes in the route catalog and
`api/openapi.json`, the tables in `api/schema.txt`. `include_validity` was added
to the request; a server older than the SDK refuses it with `unknown_field`, so
the server is deployed first. The in-process `Client` has no entitlement cache
and always answers live; a remote client answers alike except that a revocation
or seat change can show up to `MaxAge` late, unless `CheckEntitlementsLive` is
used. `NewGate` takes any `*Client`: the in-process client reaches the same
handlers through an in-process transport, and the gate prices named units with
the server's own `limits` code. Before v1.0.0 a release may still break them:
`0003_usage_limits` dropped `admission_operations`.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `cacheableEntitlementKeys` sends checks with `at`, prefixes or invalid keys to
  the server, so validation answers alike.
- OR-J23-S19: remote and embedded gates decide alike, warm without I/O.
- `pricing` (`gate_cost.go`) builds on `limits.Meter` and `limits.Definition`.
- `LocalClient`'s only admission methods in `api/go.txt` are the three `…Tx` forms
  above.

### 6. Data lifecycle
`product_entitlements` and `grants` are permanent; `product_access` is state and keeps revoked and removed windows. `limit_holds` (`created_at`) and `limit_hold_parts` (`hold_created_at`) are monthly partitions, dropped past the 61-day replay horizon, parts first since they have no foreign key to holds; `limit_windows`, `leases` and `limit_adjustments` are rows (61 days); the limit catalog and override tables are state. Every partitioned read bounds its key by the hold lifetime (`limits.HeldSince`), the replay horizon or the range a list asks for. An archive refuses open request holds and live leases.
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.
- Outside the gated merchant purge (`PurgeMerchantRowsProductAccess`), no query
  deletes `product_access` rows: revocation sets `revoked_at` or `deleted_at`.
- Each query on `limit_holds` or `limit_hold_parts` in `limit_holds.sql`,
  `leases.sql`, `money_accounts.sql` and `operational_reads.sql`, inserts aside,
  bounds `created_at` or `hold_created_at`.
- `retention.Partitioned` drops parts before holds (`AdmissionsDropBefore`).
- OR-J31-S14 moves windows and adjustments in archives; OR-J29-S08 ages out leases.

### 7. Work scales with activity
No job projects or expires keys or holds, and no table caches keys: they are derived on every read from indexes (`product_entitlements_entitlement_idx`, `product_access_customer_idx`, `product_access_product_idx`), a window ends by its own `ends_at`, and holds and reports expire by time (`LeaseLiveSince`, `HeldSince`, `expires_at`). An exact check probes the products granting each key, then the customer's windows of each; a prefix read walks the customer's live windows once, seats included (`product_access_customer_idx`), then each held product's keys (`product_entitlements_product_idx`). Derived reads use custom plans (`plan_cache_mode`), so a customer with three products and one with thousands are each planned for their size. A staff grant or revocation queues one customer's convergence; a view carries each window's next, so a window that closes for every customer sends no request to the server.
Checks:
- `internal/river` has no job that reads `product_access` or
  `product_entitlements` for expiry, and none but retention scans `limit_windows`,
  `limit_holds` or `leases` across customers; a balance look reads one
  customer's holds.
- OR-J27-S16: for a customer holding 10,000 of 40,000 products, exact checks start from `product_entitlements_entitlement_idx`, a prefix page touches at most 4 blocks per held product, and no read scans a table.
- OR-J24-S34 bounds a sync's settlement.

### 8. Abuse protection
Limits are themselves the consumption limit: holds and views bound spend and units before work starts; a delegated invoker cannot spend (`delegated_spend_not_allowed`) and is cut off past its wasted-spend limit (`failure_rate_limited`). The check, sync and admission routes take application credentials and bound each request (100 keys, 10 prefixes and `prefix_limit` up to 10,000; `billing.MaxSyncUsageItems`, `MaxSyncViews`, `MaxAdmissionBatchItems`). `middleware.AdminOperationGrant` meters each invoker by item: one full batch (`billing.MaxBatchItems`, 100 grants) a minute, shared by `UpdateCustomer`, `AdjustCustomerLimit`, `CreateCreditGrants` and `CreateProductAccess`; going over locks the invoker out for an hour. An application acting as itself is not metered. `/v1/me/entitlements` and `/v1/me/limits` take no OpenRails rate limit; the proxy owns a generic ceiling ([limits](../standalone.md#abuse-limits)). SDK Redis calls wait at most 25 ms (lookups and fills) or 250 ms (a live denial's eviction), at most 64 outstanding each, so a slow Redis falls back to OpenRails.
Checks:
- `ValidateCheck` (`internal/modules/entitlements/resource_checks.go`) refuses an
  oversize or malformed check before any read; `validate`
  (`internal/modules/leases/sync.go`) and `Client.SyncLeases` an oversize sync
  before any I/O.
- The `CreateProductAccess` and `AdjustCustomerLimit` routes set
  `Limit: middleware.AdminOperationGrant`.
- `TestEntitlementRedisBudgetLimitsOutstandingWork` bounds outstanding Redis work.
- OR-J23-S17 refuses a delegated invoker and holds nothing.

### 9. Destructive-action safeguards
Revoking a window is a `revoke_access` destructive command
(`productaccess.Service.RevokeProductAccessAsOf`, `destructive.RunLocal`), run
under the customer's lock; the window keeps its row and reason. A free grant
records its actor and reason (`grants_access_grant_reason_check`). Changing or
replacing a product's keys, or changing an existing limit (`limitEditPlan` for
`SetLimit`, `applicationAdmissionPlan` for a document), is a `catalog_change`
command; key history is closed, never deleted, and a new limit is additive.
`AdjustCustomerLimit` changes the current window only, once per `source_id`, but
is not a destructive command, and `limit_adjustments` records no actor: its
attribution is the limiter's log event, which an application acting as itself
does not emit.
Checks:
- `RevokeProductAccessAsOf` builds its command with `destructive.RevokeAccess`.
- `substantiveCatalogField` (`internal/service/catalog_admission.go`) treats every
  product field except `display_name` and `description` as substantive.
- `limitEditPlan` (`internal/service/catalog_limits.go`) names a changed limit as
  a target of the catalog admission.
- OR-J27-S02: revoking one source keeps another.

## Tests

Critical journeys, over real PostgreSQL, Redis and Vault:
- **J27**, entitlements: OR-J27-S01 to S04 (window union, revocation, boundaries, archive and refund), S05 to S09 (subscription closure and repair), S10 to S12 (imported grants), S13 and S14 (the SDK cache: scoped allowed keys, cutoff, Redis sharing, live denial, a purchase seen at once), S16 (read plans at 10,000 held products).
- **J23**, admissions, limits and the gate: OR-J23-S01 to S05 (admissions), S07 to S18 (declarations, capacity, windows, overruns, spend caps, racing replicas), S19 to S22, S24, S26, S27, S37 to S41 (the gate: lost replies, outbox, closing, in-flight reports, the bound, offline shares, near zero, idle stop, window resets), S33, S34 (a window and an accrual-rate cap in the policy's `policy_currency`), S35 (a limit counting two meters by weight, admitted and through a gate), S36 (a limit's `rate_cards` refused with their rename to `counts`); OR-J25-S05 (running charges under `accrual_rate_cap`).
- **J24**, leases and views: OR-J24-S03, S14, S16, S18 to S20, S23 to S29, S31 to S34, S37, S38, S42 to S46 (lease lifetime, views, limit sets, admission windows, bounded syncs, view refusals, lock order).
- Also OR-J22-S06 and OR-J22-S50 (lapsed credit), OR-J29-S08, OR-J31-S14, OR-J31-S16 (stored limit rate cards migrate to counts).

Other tests: `entitlement_check_cache_test.go` drives the cache against a test
server (defaults, allowed keys only, live eviction, an in-flight fill refused
after a live denial, relative lifetime, Redis budgets); `gate_test.go` drives the
gate against a scripted server.

Gaps:
- No scenario covers a product key edit or `entitlement_replacements` reaching current holders.
- No scenario reads keys through `ListEntitlements` or `GET /v1/me/entitlements`;
  OR-J01-S02 only checks that they refuse a missing credential.
- `CheckEntitlementsLive` with `at` or prefixes leaves cached entries as they were.
- No test reaches an `outstanding_cap` cap, or declares a spend window in another currency than its policy's.
- Nothing checks that partitioned reads name a time range; it is review-only. `TestTablesMatchSchema` holds `retention.Tables` to the schema.
