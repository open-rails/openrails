# Metering and usage limits

Metering bills usage in arrears and limits how much of a resource a customer may
consume over a period. OpenRails prices reported usage on totals and posts each
customer's day to the ledger once; with automatic usage billing, usage orders
bill what is owed. Limits, the prepaid balance and spend caps are enforced by
holds set aside before work starts, by an admission or by the Go SDK's gate.

## Concepts

- **Meter**: what a usage item counts, `count` or `sum` of a `value_property`, for
  one event type, optionally in matrix cells (`catalog_meters`).
- **Rate card**: a meter's price in one currency: `per_unit`, graduated `tiered`,
  `package` or `dynamic`, with an optional allowance (`catalog_rate_cards`, which
  also holds flat, unmetered fees).
- **Limit**: a declared cap in a named unit or a currency, with a reset and an
  overrun, `require_estimate` or `admit_while_positive` (`catalog_limits`).
  Products give it capacity (`catalog_product_limits`); a customer override
  replaces their sum (`customer_limit_overrides`).
- **Reset kinds**: `fixed` (tiles of `window_seconds` from the subscription start
  or the first use), `billing_period`, `session` (opens at the first use),
  `rolling` (60 buckets), `rate` (refills continuously), `lifetime`.
- **`balance`**: the built-in limit per currency, prepaid balance plus credit line
  (`catalog.BalanceLimit`), never declared. A billing policy's **spend window** is
  a fixed currency window kept as a limit window keyed `spend:` and a hash.
- **Window**: one customer's settled units of one limit in one window
  (`limit_windows`), placed by the server clock.
- **Hold**: capacity set aside for one customer in one currency (`limit_holds`),
  one part per limit and window (`limit_hold_parts`). An admission's hold has a
  `request_id`; a gate's hold belongs to a lease and one limit set.
- **Lease**: a gate's claim on its holds (`leases`), one per process or per group
  of processes sharing Redis. It dies 10 seconds after its last sync.
- **Accrual**: what a customer's usage charged in one currency on one UTC day of
  one pricing period (`usage_accruals`). A **usage order** (`origin: usage`) bills
  a period's owed usage; a **running charge** bills a rate per hour (`running_charges`).

## How it works

**Intake.** Usage arrives on a lease sync (`POST /v1/app/leases/sync`,
`leases.Syncer.Sync`) or an admission's capture (`MoneyService.CaptureAdmission`).
A sync is one transaction, bounded by `limits.SyncTimeout` (5 s) and a 2 s lock
budget. It locks the lease (creating it on the first sync) and the
customers it names in id order, settles new usage, releases what the gate
returned or confirmed shrinking, answers wants, renews up to `maxRenewals` (64)
other holds and stores `settled_through_n` and receipts. Items are numbered per
lease: a resend answers from the receipts, a changed item is
`idempotency_key_reused`, a gap `usage_out_of_order`. A sync settles a bounded
prefix (100 customers, 1.5 s); the gate sends the rest again.

**Pricing.** `MoneyService.PriceUsageTx` groups items by customer, currency,
pricing period, meter cell and rate card (`cardPrint`). With Q the period's
quantity so far (`meter_period_quantities`), a group of q is charged
`R(Q+q) − R(Q)` (`marginalPrice`), and `share` splits it to the items exactly; a
`dynamic` card charges each item its stated amount.
Usage decided against a hold is priced by the cards the hold pinned. A failed
item is never rated: its grace windows (`failed_usage_windows`) forgive it or
charge it with its own ledger legs. Then `SettleUsageTx`: `settleLimitsTx` adds
units to their windows in full, never clamped, and takes them from the live hold;
`accrueTx` adds amounts to `usage_accruals`; `rollUpTx` to `usage_daily` and
`usage_minutes`. No usage event is stored.

**Posting.** An accrual posts once its UTC day ends (`UsageAccrualWorker`, the
first scan of the hourly billing pass), or before any other movement of that
balance (`grants.PostUsageAccruals`): balance first, one `credit_spend` per lot,
the rest one `owed_accrual` dated on its day (`owedAt`), so the usage order of
that period bills it. Until then the unposted amount counts as held.

**Holds.** Every writer of holds and windows locks the customer
(`LockCustomerForSpend`) and takes window rows in one order (`limits.Holder.Take`).
A claim fits by its overrun (`limits.Fits`). A request is checked against
`balance`, its spend windows (`spendgate.SpendClaims`), then its declared limits.
One that fits only if other leases' holds came back is `capacity_held`, and those
holds are marked (`MarkLeaseHoldsContended`) to shrink at their next sync.
`admission.Admitter` adds delinquency, the accrual-rate cap (`AccrualRateMeter`,
summing `usage_minutes`) and the delegated-invoker cutoff.

**Lease holds.** `internal/modules/leases/holds.go` sizes a hold from traffic:
demand until the next sync, at most 1/`HeadroomShare` of what is free, doubled
after running dry, shrunk toward recent use. Growth applies at once. A smaller
size carries a new `version` and is released once the gate reports it applied
(`applied_version`), never below what open tickets `committed`. Readers count a
lease's holds `LeaseGrace` (5 s) past its life (`limits.LeaseLiveSince`); no hold
counts past 30 days (`limits.HeldSince`).

**The gate.** `openrails.NewGate` keeps one lease and one hold per customer and
limit set, in memory (`localGate`) or Redis (`sharedGate`). A warm decision prices
the request with the hold's pinned cards at their `PriceBound`s
(`holdPricing.cost`) and takes it from every part of the hold or none, with no
I/O; a cold customer waits for one sync. Captured usage goes on the next sync
(once a second), stored first by `GateDeps.Outbox`, or with Redis in a stream
deduplicated by write key for 15 minutes.

**Billing.** With `usage_billing_enabled`, the usage pass (`UsageOrderWorker`)
bills each account whose `money_settings.usage_due_at` has come (its bill date, or
a threshold crossing) as a usage order (`BillDueUsageAccount`), then queues due
collections as `order_collection` operations. A running charge's accrual is
computed on read and posts at its stop, at 00:00 UTC and before other balance
movements (`postRunningChargesThrough`).

## Interfaces

- **Gate**: `openrails.NewGate(client, GateDeps{Redis, Outbox})`; `Gate.Admit`,
  `Consume`, `Record`, `Limits`, `Flush`, `Close`; `Ticket.Capture`, `Release`,
  `Refusal`; `UsageOutbox`; `ErrUnavailable` ([the gate](../client.md#limits-admissions-and-the-gate)).
- **Usage** (`merchant:usage:manage`, reads `merchant:usage:read`): `SyncLeases`
  (`POST /v1/app/leases/sync`); `Admit` (`POST /v1/app/admissions`),
  `CaptureAdmissions`, `ReleaseAdmissions`, `ExtendAdmissions`, `GetAdmission`,
  `ListAdmissions`; `ListCustomerLimits` (`GET /v1/app/limits`).
- **Running charges**: `StartRunningCharge` (`POST /v1/app/running-charges`),
  `StopRunningCharge`, `GetRunningCharge`, `ListRunningCharges`; `GetBalance`
  ([running charges](../client.md#usage-and-running-charges)).
- **In a host transaction** (`LocalClient`): `AdmitTx`, `CaptureAdmissionsTx`,
  `ReleaseAdmissionsTx`, `StartRunningChargeTx`, `StopRunningChargeTx`.
- **Catalog**: `ListLimits`, `GetLimit`, `SetLimit` (`PUT /v1/catalog/limits/{key}`),
  `ListMeters`, `GetMeter`, `SetMeter`, or a catalog document.
- **Staff**: `UpdateCustomer` (`limits`, `credit_limits`, `usage_billing`,
  `billing_policy`); `AdjustCustomerLimit`
  (`POST /v1/admin/customers/{customer_id}/limit-adjustments`);
  `WaiveOrderBalance`, `MarkOrderUncollectible`, `RecordExternalPayment`.
- **Customer**: `GET /v1/me/limits`, `/v1/me/usage`, `/v1/me/balance/transactions`.
- **Metrics and events**: `usage_units`, `usage_revenue`, `forgiven_usage`,
  `limits_reached` (`POST /v1/metrics/query`); `balance.low`, `balance.depleted`,
  `delinquency.*`; customer facts `order_due`, `order_overdue`,
  `order_collection_stopped`.
- **Configuration**: `usage_billing_enabled` (`Config.UsageBillingEnabled`);
  merchant `usage_billing`, `billing_period_boundary`, `billing_policies`,
  `usage_gate_safe_spend` ([usage billing](../client.md#usage-and-running-charges),
  [spend limits](../client.md#limits-admissions-and-the-gate)).

## Guarantees

### 1. Exactly-once provider calls
Metering calls no provider. A usage order's card charge is an `order_collection`
provider intent frozen with the order's attempt (`enqueueUsageCollection`) and
run by its own job, as [collection and dunning](dunning.md) describes.
Checks:
- `enqueueUsageCollection` (`internal/modules/money/usage_collection.go`) writes
  the intent and the order's attempt in one transaction under the customer and
  order locks; a repeated operation key replays the first intent.
- The usage pass only queues and never waits on a gateway (OR-J26-S53).
- A collection refused before dispatch keeps its proof and pays once (OR-J26-S55).

### 2. Agreement with providers
Does not apply: usage, limits and holds come from the merchant's backend, not a provider; a usage order's payment reconciles in [collection and dunning](dunning.md).

### 3. Exact money
Amounts are `int64` native units of their currency end to end. A group is charged
on its total, so a period's charges sum to R of its quantity however the usage
was batched, and `share` leaves no remainder. Each item posts once: receipts
answer a resend, and each accrual posting has its own ledger key
(`OpUsageAccrual`, `usage_accrual:<id>`, posting number). Accruals, windows and
holds are per currency. The one FX use converts a spend window's cap declared in
another currency to the request's (`SpendgatePolicyLoader.convert`): a cap, never
a ledger amount.
Checks:
- `marginalPrice` refuses a falling price (no test reaches it); OR-J24-S09 prices
  a thousand items on their total, OR-J24-S10 prices periods and card revisions apart.
- `usage_accruals_day_at_key` allows one accrual per customer, currency, day and
  period; `postUsageAccrualsTx` fails if the ledger already holds the posting.
- No `fx` import in `internal/modules/money/settlement.go`, `rating.go`,
  `accruals.go`, `internal/modules/leases` or `internal/modules/limits`.
- Resends post once: OR-J24-S01, OR-J24-S06 to S08, OR-J23-S21.

### 4. Access control and tenant isolation
`/v1/app` routes declare the `merchant:usage:manage` or `merchant:usage:read` pair;
adjustments need `merchant:customer:manage`; `/v1/me/limits` and `/v1/me/usage`
read the signed-in customer. The merchant comes from the credential
(`merchant.Require`), every table is keyed by `merchant_id`, and a lease id is per
merchant (`PRIMARY KEY (merchant_id, id)`). A sync obeys the credential's customer
scope: another customer's item is refused alone with
`service_credential_customer_scope_denied` and that customer is never locked. The
cross-merchant due scans run each row in its own merchant scope.
Checks:
- Each `/v1/app` route in `internal/http/routes/limits.go` and `metering.go` has a
  `Permission` pair; `AdjustCustomerLimit` is `Group: Admin, Level: LevelUpdate`.
- `syncRun.usageItem` and `syncRun.lockCustomers` apply `Syncer.Allows`.
- Every tenant query in `internal/db/queries/limits.sql`, `leases.sql`,
  `limit_holds.sql`, `usage_settlement.sql`, `usage_billing.sql` and
  `usage_orders.sql` filters on `merchant_id`; due scans are marked `CROSS-MERCHANT`.
- OR-J23-S11 and OR-J24-S03 keep reads, adjustments and refusals in their merchant.

### 5. Stable contracts
The gate, lease, limit, admission and running-charge types are in `api/go.txt`,
the routes in the route catalog and `api/openapi.json`, the tables in
`api/schema.txt`. `NewGate` takes any `*Client`: the in-process client reaches the
same handlers through an in-process transport, and the gate prices named units
with the server's own `limits` code. Before v1.0.0 a release may still break
them: `0003_usage_limits` dropped `usage_events` and `admission_operations`.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- OR-J23-S19: remote and embedded gates decide alike, warm without I/O.
- `holdPricing` (`gate_cost.go`) builds on `limits.Meter` and `limits.Definition`.
- `LocalClient`'s only metering methods in `api/go.txt` are the five `…Tx` forms above.

### 6. Migrations and retention
The baseline holds meters, rate cards, running charges, `meter_period_quantities`
and `failed_usage_windows`; `0003_usage_limits` added limits, windows, leases,
holds, adjustments, accruals and rollups. Classes, in each table comment and
`internal/retention/tables.go`: permanent `usage_accruals` and `running_charges`;
monthly partitions for `limit_holds` (`created_at`) and `limit_hold_parts`
(`hold_created_at`), dropped past the 61-day replay horizon, parts first since
they have no foreign key to holds; rows for `limit_windows`, `leases` and
`limit_adjustments` (61 days), `meter_period_quantities` (100 days), `usage_daily`
(24 months after billing), `usage_minutes` (25 hours), `failed_usage_windows`
(window end); state for the catalog and override tables. Every partitioned read
bounds its key by the hold lifetime (`limits.HeldSince`), the replay horizon or
the range a list asks for. An archive refuses open request holds and live leases.
Checks:
- Each query on `limit_holds` or `limit_hold_parts` in `limit_holds.sql`,
  `leases.sql`, `money_accounts.sql` and `operational_reads.sql`, inserts aside,
  bounds `created_at` or `hold_created_at`.
- `retention.Partitioned` drops parts before holds (`AdmissionsDropBefore`).
- OR-J31-S14 moves windows and adjustments in archives; OR-J29-S08 ages out leases.

### 7. Work scales with activity
The accrual scan reads only unposted accruals of ended days
(`usage_accruals_due_idx`), in keyset pages of 200. The usage pass reads due
accounts (`money_settings_usage_due_at_idx`) and due collections
(`next_collection_attempt_at`). Holds expire by time, read through
`LeaseLiveSince`, `HeldSince` and `expires_at`, so no worker expires them. A sync
works on the customers it names plus at most 64 renewals, so a window that closes
for every customer spreads over syncs. Rollups grow with customers and days, not
requests; retention deletes by indexed cutoffs in bounded batches.
Checks:
- `UsageAccrualWorker.Pass` pages `ListDueUsageAccruals`; an idle pass reads no row.
- No job in `internal/river` scans `limit_windows`, `limit_holds` or `leases`
  across customers but retention; a balance look reads one customer's holds.
- OR-J24-S30 and OR-J24-S34 bound a sync's renewals and settlement; OR-J26-S40 and
  OR-J26-S52 bound the usage pass.

### 8. Abuse protection
Metering is itself the consumption limit: holds bound spend and units before work
starts; a delegated invoker cannot spend (`delegated_spend_not_allowed`) and is
cut off past its wasted-spend limit (`failure_rate_limited`). `/v1/app` routes take
application credentials and bound their size (`billing.MaxSyncUsageItems`,
`MaxSyncWants`, `MaxAdmissionBatchItems`). `middleware.AdminOperationGrant`
meters each invoker by item: 10 a minute, shared by `UpdateCustomer`,
`AdjustCustomerLimit`, `CreateCreditGrants` and `CreateProductAccess`; going over
locks the invoker out for an hour. An application acting as itself is not
metered. `/v1/me/limits` and `/v1/me/usage` take no OpenRails rate limit; the
proxy owns a generic ceiling ([limits](../standalone.md#abuse-limits)).
Checks:
- `validate` (`internal/modules/leases/sync.go`) and `Client.SyncLeases` refuse an
  oversize sync before any I/O.
- `AdjustCustomerLimit`'s route sets `Limit: middleware.AdminOperationGrant`.
- OR-J23-S17 refuses a delegated invoker and holds nothing.

### 9. Destructive-action safeguards
Changing an existing limit is a `catalog_change` command (`limitEditPlan` for
`SetLimit`, `applicationAdmissionPlan` for a document); a new limit is additive.
Waiving or writing off a usage order is a `waive_order` or `mark_uncollectible`
command (`MoneyService.destructiveUsageOrder`). Settled usage is never reversed,
and settling it is never refused for money. `AdjustCustomerLimit` changes the
current window only, once per `source_id`, but is not a destructive command, and
`limit_adjustments` records no actor: its attribution is the limiter's log event,
which an application acting as itself does not emit.
Checks:
- `limitEditPlan` (`internal/service/catalog_limits.go`) names a changed limit as
  a target of the catalog admission.
- `destructiveUsageOrder` (`internal/modules/money/usage_order.go`) uses
  `destructive.WaiveOrder` and `destructive.MarkUncollectible`.
- OR-J26-S03: paid, waived or written-off debt is not charged again; OR-J26-S50:
  each clears delinquency in the same change.

## Tests

Critical journeys, over real PostgreSQL, Redis and Vault:
- **J23**, admissions, limits and the gate: OR-J23-S01 to S05 (admissions), S07 to
  S18 (declarations, capacity, windows, overruns, spend caps, racing replicas),
  S19 to S31 (the gate in process and over Redis, lost replies, outbox, closing).
- **J24**, settlement and leases: OR-J24-S01 to S35 (replay, pricing on totals,
  daily posting, lots, lease lifetime, hold versions, sizing, bounded syncs).
- **J25**, running charges and balance looks: OR-J25-S01 to S04, S37, S40 to S42.
- **J26**, usage billing and delinquency: OR-J26-S01 to S04, S30, S40, S50 to S58.
- Also OR-J22-S06 and OR-J22-S50 (lapsed credit), OR-J29-S08, OR-J31-S14.

Other tests: `gate_test.go` drives the gate against a scripted server;
`internal/modules/money/rating_test.go` prices every accepted model.

Gaps:
- No test reaches an `outstanding_cap` cap or uses `accrual_rate_cap`, or
  declares a spend window in another currency.
- Nothing checks that partitioned reads name a time range, or that
  `retention.Tables` matches the schema; both are review-only.
- The accrual-rate measure reads `usage_minutes` only, so running charges never
  count toward an `accrual_rate_cap`; no test pins either way.
- A meter correction locks all of `usage_daily`
  (`LockUsageDailyForMeterCorrection`), every merchant's settlement included,
  until its catalog transaction commits.
