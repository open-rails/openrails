# Entitlements and limits

Entitlements and limits answer what a customer may use and how much. A customer
holds a key while a live access window of theirs covers a product that grants the
key; keys are derived when read. Declared limits, the prepaid balance and spend
caps bound consumption over a period: the Go SDK's gate decides each request on
views of where the customer stands, nothing held on the server, overshooting by
at most a few seconds of other gates' traffic. A merchant's backend checks keys and limits;
staff grant and revoke products; the remote Go SDK caches allowed answers. What
usage costs is [usage billing](usage-billing.md).

## Concepts

- **Entitlement key**: a plain string of at most 256 bytes (`syntax.MaxEntitlementKeyBytes`, in `internal/catalogrules/syntax`), granted by products, at most 10,000 live per product (`syntax.MaxProductEntitlements`).
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
- **`balance`**: the built-in limit per currency, prepaid balance plus credit line (`catalog.BalanceLimit`), never declared: the ledger balance less pending usage and running-charge accrual, never held for in-flight work. The credit line is the customer's credit limit (its own, else its billing policy's `outstanding_cap_amount`, else the merchant's usage terms') less what it owes (`MoneyService.usageAdmissionCapacity`). A billing policy's **spend window** is a fixed currency window kept as a limit window keyed `spend:` and a hash.
- **Trust tier**: picks a customer's billing policy binding in one currency, for its credit line, collection and delinquency terms and spend windows: its explicit level (`money_settings.tier`), else the highest of the merchant's `trust_tiers` whose requirement its paid totals meet, else `free`; a request's own trust level picks only its spend windows. Computed where it is read (`billing.trust_tier` over `customer_paid_totals`, which the payments trigger keeps in the transaction of each settlement, refund and chargeback), never stored (OR-J23-S43 to S50).
- **Limit window**: one customer's settled units of one limit in one window
  (`limit_windows`), placed by the server clock.
- **Lease**: a gate's (or a batch job's) usage stream and in-flight report of
  its declared limits and spend windows (`leases`, `lease_in_flight`; never the
  balance), one per process. It is live for 10 seconds
  after its last sync; a later sync makes it live again.
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

**SDK cache.** A remote handle with a static `APIKey` caches `CheckEntitlements` by default (`entitlementCheckCache`): a bounded 8 MiB in-process LRU, or Redis when `EntitlementCheckCacheOptions.Redis` is set. Only current exact-key checks use it. It stores each allowed key's answer, never a denial, and answers from the cache only when every requested key is allowed there; otherwise it asks OpenRails for the whole set, with `include_validity`. An entry lives `min(MaxAge, valid_until − evaluated_at)`, timed from before the request was sent; a response naming no merchant (`OpenRails-Bound-Merchant`) is refused, never stored. A check through an `Uncached` handle always asks; its answer replaces allowed entries, drops denied ones and, when it denies a key, advances the scope's generation, so a fill begun before it cannot restore a dropped entry.

**Lease sync.** A gate settles usage and asks for views on a lease sync (`POST /v1/app/leases/sync`, `leases.Syncer.Sync`): one transaction, bounded by `limits.SyncTimeout` (5 s) and a 2 s lock budget, that takes no customer's lock. One that cannot take a lock in time, or is chosen to break a deadlock, commits nothing and answers 503. It locks the lease (creating it on the first sync), settles new usage in chunks while within its usage budget ([usage billing](usage-billing.md)), replaces its in-flight report (changed rows of its own), and stores `settled_through_n` and receipts. Once it committed, it inserts the usage into the event store, watches the balances its usage moved (`MoneyService.WatchUsage`) and answers views, limit sets and pricing (`views.go`) in one read-only snapshot, reading every customer's money, recent spend and limit usage in one batch each; money is PostgreSQL's alone (a balance and its pending usage). Limit reads take PostgreSQL before the event store, so another gate's view counts an item twice at worst (its in-flight line and its events), and misses it only between its sync's commit and its insert, or for good when the store never takes it. The view covers every item the receipts name that the store took. Views read usage only through `leases.UsageReader`.

**Limits.** Every opener of a window locks the customer (`LockCustomerForSpend`, `FOR NO KEY UPDATE`, so a row naming the customer is written without waiting for it); intake, which only appends usage, takes it only to open a window or charge a failure. A request is checked against `balance`, its spend windows (`spendgate.SpendClaims`), then its declared limits, each by its overrun; a declared limit's or spend window's held is what live leases report in flight, the balance's its pending usage and accrual. `admission.Admitter` resolves the set a view carries and adds delinquency and the delegated-invoker cutoff. Intake (`MoneyService.PrepareUsageTx`) counts units in their windows in full, never clamped, in the event store's `limit_usage`: the windows current when the usage was admitted (`admitted_at`), and the spend windows of the invoker attributes it reports (none when its policy cannot be resolved, such as during an FX outage); reads take a window's units there, a rolling limit's buckets and a rate limit's level from its buckets (`limits.RateLevelOf`). A lease's report counts while it is live (`limits.LeaseLiveSince`). A view request a sync cannot answer is refused alone (`refused_views`).

**The gate.** `openrails.NewGate` keeps one lease for its life and a view of each customer it serves (`gate_view.go`). A warm decision prices the request (named units by the view's counts, money at its `PriceBound`s) and takes it from every limit of its set or none, against the view less what the gate has in flight, with no I/O. Nothing is decided or held on the server: near a limit, what other gates admit before the next views may take it past its line, by at most what they admit in about four sync intervals plus one request per gate; for the balance, the views see other gates' usage once it settles, and spending past zero posts as owed. Tickets open across a sync, and usage it does not carry, are reported in flight for declared limits and spend windows; what a sync carried but had no time to settle, the next carries at once. A sync answering requests that wait for their first view carries no usage unless a full batch is queued, leaving it to the next, so a first decision does not wait on the event store's insert. A view older than two syncs decides one request until a sync answers; with OpenRails unreachable a view answered when the outage began keeps deciding within the gate's share (remaining under the request's policy, less running charges since, ÷ live leases). A view with anything in flight is never dropped. An idle gate stops syncing. Captured usage goes on the next sync, stored first by `GateDeps.Outbox`.

## Interfaces

- **Check**: `Client.CheckEntitlements`, past the cache through `Client.Uncached` (`POST /v1/app/entitlements/check`, `merchant:entitlements:read`): up to 100 keys (`billing.MaxEntitlementChecks`) and 10 prefixes at one instant. Cache: `EntitlementCheckCacheOptions{MaxAge, Disabled, Redis}` on `RemoteOptions`, `DefaultEntitlementCheckMaxAge` (2 minutes); see [backend](../client.md#caching).
- **Gate**: `openrails.NewGate(client, GateDeps{Outbox})`; `Gate.Admit`, `Consume`, `Record`, `ListCustomerLimits`, `Flush`, `Close`; `Ticket.Capture`, `Release`, `Refusal`; `UsageOutbox`; `ErrUnavailable` ([the gate](../client.md#limits-and-the-gate)).
- **Limits** (`merchant:usage:manage`, reads `merchant:usage:read`): `SyncLeases` (`POST /v1/app/leases/sync`); `ListCustomerLimits` (`GET /v1/app/limits`).
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
  ([spend limits](../client.md#limits-and-the-gate)).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: entitlements and limits call no provider; the payments and subscriptions that open or close windows are made by [invoices and checkout](invoices.md), [subscriptions](subscriptions.md) and [refunds](refunds.md).

### 2. Agreement with providers
Limits and leases come from the merchant's backend, not a provider. Access
follows what OpenRails recorded of a provider, never a guess. A refund that
revokes access does so in the transaction that records the refund; canceling a
subscription's recurrence never shortens a paid period; an uncertain or
`unverified` subscription keeps its access. Repair rechecks recorded evidence and
never reopens access a recorded revocation ended. A live window never covers more
than its grant: convergence cuts a wider one back to the grant's dates, once the
merchant's destructive switch is armed. A window wholly outside its grant goes to
an operator unchanged.
Checks:
- `internal/intents/refund.go` calls `RevokeRefundedPaymentAccess` with the
  refund's transaction (`txDB`).
- `lifecycle.EndAccess` revokes only renewal grace unless the effect sets
  `Revoke` (`internal/modules/subscriptions/effects.go`).
- OR-J27-S05, OR-J27-S07, OR-J27-S08 and OR-J27-S09 cover repair, a late
  chargeback and uncertain state; OR-J27-S17 a window wider than its grant.

### 3. Exact money
Access windows and keys carry no amounts; credit lots (`credit_grants`) belong to [Credits](credits.md). In-flight reports and limit windows are `int64` native units of one currency each. The one FX use converts a spend window's cap declared in another currency to the request's (`SpendgatePolicyLoader.convert`): a cap, never a ledger amount.
Checks:
- No `fx` import in `internal/modules/leases` or `internal/modules/limits`.

### 4. Access control and tenant isolation
The check route needs `merchant:entitlements:read` (or `root:entitlements:read`);
lease and limit routes the `merchant:usage:manage` or
`merchant:usage:read` pair; adjustments `merchant:customer:manage`. Lease syncs
and views obey the credential's customer scope (`Syncer.Allows`), refusing
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
- Each `/v1/app` route in `internal/http/routes/limits.go` and `metering.go` has a `Permission` pair; `AdjustCustomerLimit` is `Group: Admin, Level: LevelUpdate`.
- `syncRun.checkItem`, `checkView` and `report` apply `Syncer.Allows`.
- `entitlementCacheScopeOf` (`entitlement_check_cache.go`) and `newRemoteHandle`
  (`remote.go`), which enables the cache only for an `APIKey`.
- Every query in `internal/db/queries/entitlement_checks.sql`, `product_access.sql`, `limits.sql` and `leases.sql`, advisory locks aside, filters on `merchant_id`.
- OR-J27-S13: a key without `merchant:entitlements:read` is refused, and another
  merchant's key never sees a cached answer. OR-J23-S11 and OR-J24-S03 keep limit
  reads, adjustments and refusals in their merchant.
- `TestEntitlementCheckCacheRequiresMetadataAndScope`: an answer without
  `evaluated_at` is not cached, and one naming no merchant is refused.

### 5. Contracts match the code
Their types are in `api/go.txt`, the routes in the route catalog and `api/openapi.json`, the tables in `api/schema.txt`. `include_validity` was added to the request; a server older than the SDK refuses it with `unknown_field`, so the server is deployed first. The in-process `Client` has no entitlement cache and always answers live; a remote client answers alike except that a revocation or seat change can show up to `MaxAge` late, unless the check goes through an `Uncached` handle. `NewGate` takes a remote or in-process `*Client`, never one made with `ActingFor`: the in-process client reaches the same handlers through an in-process transport, and the gate prices named units with the server's own `limits` code. Before v1.0.0 a release may still break them: `0003_usage_limits` dropped `admission_operations`, and `0044_drop_request_holds` the request-hold API with `limit_holds` and `limit_hold_parts`.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `cacheableEntitlementKeys` sends checks with `at`, prefixes or invalid keys to
  the server, so validation answers alike.
- OR-J23-S19: remote and embedded gates decide alike, warm without I/O.
- `pricing` (`gate_cost.go`) builds on `limits.Meter` and `limits.Definition`.
- `LocalClient` has no admission method in `api/go.txt`.

### 6. Data lifecycle
`product_entitlements` and `grants` are permanent; `product_access` is state and keeps revoked and removed windows. `limit_windows` are rows kept a day past their end (61 days once one reached its capacity, for `limits_reached`; usage settled after a window ended counts toward none, OR-J24-S83), `leases` and `limit_adjustments` rows kept 61 days; the limit catalog and override tables are state. An archive refuses live leases.
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.
- Outside the gated merchant purge (`PurgeMerchantRowsProductAccess`), no query
  deletes `product_access` rows: revocation sets `revoked_at` or `deleted_at`.
- No table is partitioned (`TestTablesMatchSchema`).
- OR-J31-S14 moves windows and adjustments in archives; OR-J29-S08 ages out leases.

### 7. Work scales with activity
No job projects or expires keys or reports, and no table caches keys: they are derived on every read from indexes (`product_entitlements_entitlement_idx`, `product_access_customer_idx`, `product_access_product_idx`), a window ends by its own `ends_at`, and in-flight reports expire by time (`LeaseLiveSince`). An exact check probes the products granting each key, then the customer's windows of each; a prefix read walks the customer's live windows once, seats included (`product_access_customer_idx`), then each held product's keys (`product_entitlements_product_idx`). Derived reads use custom plans (`plan_cache_mode`), so a customer with three products and one with thousands are each planned for their size. A staff grant or revocation queues one customer's convergence; a view carries each window's next, so a window that closes for every customer sends no request to the server.
Checks:
- `internal/river` has no job that reads `product_access` or
  `product_entitlements` for expiry, and none but retention scans `limit_windows`
  or `leases` across customers; a balance look reads no in-flight report.
- OR-J27-S16: for a customer holding 10,000 of 40,000 products, exact checks start from `product_entitlements_entitlement_idx`, a prefix page touches at most 4 blocks per held product, and no read scans a table.
- OR-J24-S34, OR-J24-S70: a sync settles what its time allows, at least a chunk, so a backlog of a thousand customers' usage settles across syncs, every item once.

### 8. Abuse protection
Limits are themselves the consumption limit: views bound spend and units before work starts; a delegated invoker cannot spend (`delegated_spend_not_allowed`) and is cut off past its wasted-spend limit (`failure_rate_limited`). The check and sync routes take application credentials and bound each request (100 keys, 10 prefixes and `prefix_limit` up to 10,000; `billing.MaxSyncUsageItems`, `MaxSyncViews`). `middleware.AdminOperationGrant` meters each invoker by item: one full batch (`billing.MaxBatchItems`, 100 grants) a minute, shared by `UpdateCustomer`, `AdjustCustomerLimit`, `CreateCreditGrants` and `CreateProductAccess`; going over locks the invoker out for an hour. An application acting as itself is not metered. `/v1/me/entitlements` and `/v1/me/limits` take no OpenRails rate limit; the proxy owns a generic ceiling ([limits](../standalone.md#abuse-limits)). SDK Redis calls wait at most 25 ms (lookups and fills) or 250 ms (a live denial's eviction), at most 64 outstanding each, so a slow Redis falls back to OpenRails.
Checks:
- `ValidateCheck` (`internal/modules/entitlements/resource_checks.go`) refuses an oversize or malformed check before any read; `validate` (`internal/modules/leases/sync.go`) an oversize sync before any I/O.
- The `CreateProductAccess` and `AdjustCustomerLimit` routes set
  `Limit: middleware.AdminOperationGrant`.
- `TestEntitlementRedisBudgetLimitsOutstandingWork` bounds outstanding Redis work.
- OR-J23-S17 refuses a delegated invoker, who takes nothing.

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
- **J27**, entitlements: OR-J27-S01 to S04 (window union, revocation, boundaries, archive and refund), S05 to S09 (subscription closure and repair), S10 to S12 (imported grants), S13 and S14 (the SDK cache: scoped allowed keys, cutoff, Redis sharing, live denial, a purchase seen at once), S16 (read plans at 10,000 held products), S17 (a window wider than its grant).
- **J23**, limits and the gate: OR-J23-S04, S05 (a host transaction), S07 to S18 (declarations, capacity, windows, overruns, spend caps), S19 to S22, S24, S27, S37, S38, S40, S41 (the gate: lost replies, outbox, closing, in-flight reports, the bound, offline shares and resends, idle stop, window resets), S33 (a window in the policy's `policy_currency`), S35 (a limit counting two meters by weight), S36 (a field a limit does not take refused), S42 (one policy's cap on what is owed and its spend window, and a customer's own credit limit over the cap).
- **J24**, leases and views: OR-J24-S03, S14, S16, S18 to S20, S23 to S27, S29, S31 to S34, S37, S38, S42, S43, S45 (lease lifetime, views, limit sets, admission windows, bounded syncs, view refusals, lock order).
- Also OR-J22-S06 and OR-J22-S50 (lapsed credit), OR-J29-S08, OR-J31-S14.

Other tests: `entitlement_check_cache_test.go` drives the cache against a test
server (defaults, allowed keys only, live eviction, an in-flight fill refused
after a live denial, relative lifetime, Redis budgets); `gate_test.go` drives the
gate against a scripted server.

Gaps:
- No scenario covers a product key edit or `entitlement_replacements` reaching current holders.
- No scenario reads keys through `ListEntitlements` or `GET /v1/me/entitlements`;
  OR-J01-S02 only checks that they refuse a missing credential.
- An `Uncached` check with `at` or prefixes leaves cached entries as they were.
- No test declares a spend window in another currency than its policy's.
- `TestTablesMatchSchema` holds `retention.Tables` to the schema.
