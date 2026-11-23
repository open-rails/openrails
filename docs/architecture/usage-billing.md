# Usage billing

Usage billing decides what usage costs and bills it in arrears. OpenRails prices
reported usage on totals and posts each customer's day to the ledger once; with
automatic usage billing, usage orders bill what is owed, and running charges
bill a rate per hour. How much a customer may use, and the holds and gate that
enforce it, are [entitlements and limits](entitlements.md).

## Concepts

- **Meter**: what a usage item counts, `count` or `sum` of a `value_property`, for
  one event type, optionally in matrix cells (`catalog_meters`).
- **Rate card**: a meter's price in one currency: `per_unit`, graduated `tiered`,
  `package` or `dynamic`, with an optional allowance (`catalog_rate_cards`, which
  also holds flat, unmetered fees).
- **Accrual**: what a customer's usage charged in one currency on one UTC day of
  one pricing period (`usage_accruals`).
- **Usage order** (`origin: usage`): bills a period's owed usage.
- **Running charge**: bills a rate per hour (`running_charges`).

## How it works

**Intake.** Usage arrives on a lease sync (`POST /v1/app/leases/sync`) or an
admission's capture (`MoneyService.CaptureAdmission`); both are
[entitlements and limits](entitlements.md). Items are numbered per lease: a resend
answers from the sync's receipts, a changed item is `idempotency_key_reused`, a
gap `usage_out_of_order`. A sync settles a bounded prefix (100 customers, 1.5 s);
the gate sends the rest again.

**Pricing.** `MoneyService.PriceUsageTx` groups items by customer, currency,
pricing period, meter cell and rate card (`cardPrint`). With Q the period's
quantity so far (`meter_period_quantities`), a group of q is charged
`R(Q+q) − R(Q)` (`marginalPrice`), and `share` splits it to the items exactly; a
`dynamic` card charges each item its stated amount.
Usage decided against a hold is priced by the cards the hold pinned. A failed
item is never rated: its grace windows (`failed_usage_windows`) forgive it or
charge it with its own ledger legs. Then `SettleUsageTx`: `settleLimitsTx` adds
units to their limit windows; `accrueTx` adds amounts to `usage_accruals`;
`rollUpTx` to `usage_daily` and `usage_minutes`. No usage event is stored.

**Posting.** An accrual posts once its UTC day ends (`UsageAccrualWorker`, the
first scan of the hourly billing pass), or before any other movement of that
balance (`grants.PostUsageAccruals`): balance first, one `credit_spend` per lot,
the rest one `owed_accrual` dated on its day (`owedAt`), so the usage order of
that period bills it. Until then the unposted amount counts as held.

**Billing.** With `usage_billing_enabled`, the usage pass (`UsageOrderWorker`)
bills each account whose `money_settings.usage_due_at` has come (its bill date, or
a threshold crossing) as a usage order (`BillDueUsageAccount`), then queues due
collections as `order_collection` operations. A running charge's accrual is
computed on read and posts at its stop, at 00:00 UTC and before other balance
movements (`postRunningChargesThrough`).

## Interfaces

- **Intake**: `SyncLeases` and `CaptureAdmissions`
  ([entitlements and limits](entitlements.md)).
- **Running charges** (`merchant:usage:manage`, reads `merchant:usage:read`):
  `StartRunningCharge` (`POST /v1/app/running-charges`), `StopRunningCharge`,
  `GetRunningCharge`, `ListRunningCharges`; `GetBalance`
  ([running charges](../client.md#usage-and-running-charges)); in a host transaction
  (`LocalClient`) `StartRunningChargeTx`, `StopRunningChargeTx`.
- **Catalog**: `ListMeters`, `GetMeter`, `SetMeter`, or a catalog document.
- **Staff**: `UpdateCustomer` (`usage_billing`); `WaiveOrderBalance`,
  `MarkOrderUncollectible`, `RecordExternalPayment`.
- **Customer**: `GET /v1/me/usage`, `/v1/me/balance/transactions`.
- **Metrics and events**: `usage_units`, `usage_revenue`, `forgiven_usage`
  (`POST /v1/metrics/query`); `delinquency.*`; customer facts `order_due`,
  `order_overdue`, `order_collection_stopped`.
- **Configuration**: `usage_billing_enabled` (`Config.UsageBillingEnabled`);
  merchant `usage_billing`, `billing_period_boundary`
  ([usage billing](../client.md#usage-and-running-charges)).

## Guarantees

### 1. Exactly-once provider calls
Usage billing calls no provider. A usage order's card charge is an
`order_collection` provider intent frozen with the order's attempt
(`enqueueUsageCollection`) and run by its own job, as
[collection and dunning](dunning.md) describes.
Checks:
- `enqueueUsageCollection` (`internal/modules/money/usage_collection.go`) writes
  the intent and the order's attempt in one transaction under the customer and
  order locks; a repeated operation key replays the first intent.
- The usage pass only queues and never waits on a gateway (OR-J26-S53).
- A collection refused before dispatch keeps its proof and pays once (OR-J26-S55).

### 2. Agreement with providers
Does not apply: usage comes from the merchant's backend, not a provider; a usage order's payment reconciles in [collection and dunning](dunning.md).

### 3. Exact money
Amounts are `int64` native units of their currency end to end. A group is charged
on its total, so a period's charges sum to R of its quantity however the usage
was batched, and `share` leaves no remainder. Each item posts once: receipts
answer a resend, and each accrual posting has its own ledger key
(`OpUsageAccrual`, `usage_accrual:<id>`, posting number). Accruals are per
currency; the one FX use, a spend window's cap, is in
[entitlements and limits](entitlements.md).
Checks:
- `marginalPrice` refuses a falling price (no test reaches it); OR-J24-S09 prices
  a thousand items on their total, OR-J24-S10 prices periods and card revisions apart.
- `usage_accruals_day_at_key` allows one accrual per customer, currency, day and
  period; `postUsageAccrualsTx` fails if the ledger already holds the posting.
- No `fx` import in `internal/modules/money/settlement.go`, `rating.go` or
  `accruals.go`.
- Resends post once: OR-J24-S01, OR-J24-S06 to S08, OR-J23-S21.

### 4. Access control and tenant isolation
Running-charge routes declare the `merchant:usage:manage` or `merchant:usage:read`
pair; `/v1/me/usage` reads the signed-in customer. The merchant comes from the
credential (`merchant.Require`) and every table is keyed by `merchant_id`. The
cross-merchant due scans run each row in its own merchant scope.
Checks:
- Each running-charge route in `internal/http/routes/metering.go` has a
  `Permission` pair.
- Every tenant query in `internal/db/queries/usage_settlement.sql`,
  `usage_billing.sql` and `usage_orders.sql` filters on `merchant_id`; due scans
  are marked `CROSS-MERCHANT`.

### 5. Contracts match the code
The running-charge types are in `api/go.txt`, the routes in the route catalog and
`api/openapi.json`, the tables in `api/schema.txt`. Before v1.0.0 a release may
still break them: `0003_usage_limits` dropped `usage_events`.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `LocalClient`'s only running-charge methods in `api/go.txt` are the two `…Tx`
  forms above.

### 6. Data lifecycle
Classes, in each table comment and `internal/retention/tables.go`: permanent
`usage_accruals` and `running_charges`; rows for `meter_period_quantities`
(100 days), `usage_daily` (24 months after billing), `usage_minutes` (25 hours),
`failed_usage_windows` (window end); state for the meter and rate-card tables.
None is partitioned.
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.

### 7. Work scales with activity
The accrual scan reads only unposted accruals of ended days
(`usage_accruals_due_idx`), in keyset pages of 200. The usage pass reads due
accounts (`money_settings_usage_due_at_idx`) and due collections
(`next_collection_attempt_at`). Rollups grow with customers and days, not
requests; retention deletes by indexed cutoffs in bounded batches.
Checks:
- `UsageAccrualWorker.Pass` pages `ListDueUsageAccruals`; an idle pass reads no row.
- OR-J26-S40 and OR-J26-S52 bound the usage pass.

### 8. Abuse protection
Does not apply: usage enters through syncs and admissions, bounded in [entitlements and limits](entitlements.md); `/v1/me/usage` takes no OpenRails rate limit, and the proxy owns a generic ceiling ([limits](../standalone.md#abuse-limits)).

### 9. Destructive-action safeguards
Waiving or writing off a usage order is a `waive_order` or `mark_uncollectible`
command (`MoneyService.destructiveUsageOrder`). Settled usage is never reversed,
and settling it is never refused for money.
Checks:
- `destructiveUsageOrder` (`internal/modules/money/usage_order.go`) uses
  `destructive.WaiveOrder` and `destructive.MarkUncollectible`.
- OR-J26-S03: paid, waived or written-off debt is not charged again; OR-J26-S50:
  each clears delinquency in the same change.

## Tests

Critical journeys, over real PostgreSQL, Redis and Vault:
- **J24**, settlement: OR-J24-S01, S02, S04 to S11, S21, S22 (replay, pricing on
  totals, daily posting, owed dating, lots). Its lease scenarios are in
  [entitlements and limits](entitlements.md).
- **J25**, running charges and balance looks: OR-J25-S01 to S04, S37, S40 to S42.
- **J26**, usage billing and delinquency: OR-J26-S01 to S04, S30, S40, S50 to S58.

Other tests: `internal/modules/money/rating_test.go` prices every accepted model.

Gaps:
- Nothing checks that `retention.Tables` matches the schema; it is review-only.
- A meter correction locks all of `usage_daily`
  (`LockUsageDailyForMeterCorrection`), every merchant's settlement included,
  until its catalog transaction commits.
