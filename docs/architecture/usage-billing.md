# Usage billing

Usage billing decides what usage costs and bills it in arrears. Usage lives in the event store (ClickHouse); OpenRails prices each period on its total and posts it to [Books](books.md) at each month's end and before balance movements; with automatic usage billing, usage orders bill what is owed, and running charges bill a rate per hour. How much a customer may use, and the holds and gate that enforce it, are [entitlements and limits](entitlements.md).

## Concepts

- **Meter**: what a usage item counts, `count` or `sum` of a `value_property`, for
  one event type, optionally in matrix cells (`catalog_meters`).
- **Rate card**: a meter's price in one currency: `per_unit`, `tiered` (graduated,
  or volume as a discount), `package` or `dynamic`, with an optional allowance (`catalog_rate_cards`, which
  also holds flat, unmetered fees).
- **Usage event**: one usage item in the event store's `usage_events`, with its
  priced meter cells (card print and pricing period) and the limit windows that
  count it; rollups keep limit windows, period quantities, minutes and
  15-minute UTC history buckets (`usage_history`).
- **Posting**: what the ledger took of a period's usage (`usage_postings`);
  `usage_unposted` names who has usage it has not taken.
- **Usage order** (`origin: usage`): bills a period's owed usage.
- **Running charge**: bills a rate per hour (`running_charges`).

## How it works

**Intake.** Usage arrives on a lease sync (`POST /v1/app/leases/sync`) or an
admission's capture (`MoneyService.CaptureAdmission`); both are
[entitlements and limits](entitlements.md). Items are numbered per lease: a resend
answers from the sync's receipts, a changed item is `idempotency_key_reused`, a
gap `usage_out_of_order`. A sync settles a bounded prefix (100 customers, 1.5 s);
the gate sends the rest again. While ClickHouse is out of reach a sync answers 503
and settles nothing (OR-J24-S48).

**Pricing.** `MoneyService.PrepareUsageTx` validates, prices and locates under the
customers' locks: each group sharing a period, meter cell and card print
(`cardPrint`) is priced provisionally at the period position the store holds,
`R(Q+q) − R(Q)` never below zero (`marginalPrice`), shared by quantity; a
`dynamic` card charges each item its stated amount. A capture of a request hold
is priced by the cards it pinned. Limits are located at `admitted_at`
(`locateItems`); a failed item's grace is read from its windows' `limit_usage`
and what it leaves is charged with its own ledger legs. `CommitUsageTx` skips
events an earlier attempt inserted (`ExistingUsage`), inserts the rest and waits,
then writes `usage_unposted`, the card prints, opened session and first-use
windows and captures, in the caller's transaction.

**Posting.** `MoneyService.postUsageTx` is the one choke point: under the
customer lock, per pricing period from `usage_unposted.since_at`, the price of
the period's deduplicated usage before the cut (`PostingQuantities`, `FINAL`),
card by card in print order (`usagePrints.cellPrice`), less what earlier
postings took (`postingDelta`): balance first, one `credit_spend` per lot live
before the cut, the rest one `owed_accrual` split by meter and dated before the
period's end and the cut, never before the last usage order's end. It runs at
each month's end (`UsageCloseWorker`, the first scan of the hourly billing pass)
and before any other movement of that balance (`db.UsageLedger`). A period whose
price fell (a volume tier) is carried and credited (`usage_reversed`) once its
late window closes. Until then the balance holds what is unposted
(`unpostedUsage`).

**Billing.** With `usage_billing_enabled`, the usage pass (`UsageOrderWorker`)
bills each account whose `money_settings.usage_due_at` has come (its bill date, or
a threshold crossing) as a usage order (`BillDueUsageAccount`), then queues due
collections as `order_collection` operations. The threshold is its currency's
`usage_billing` `collection_threshold`, else its credit limit; a billing policy's
outstanding cap and `collection_threshold_amount` bind only an account in the
policy's `policy_currency`, as they do in admission (OR-J26-S61). A running charge's accrual is
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
- **Customer**: `GET /v1/me/usage` (priced per 15-minute UTC bucket and folded into the merchant's days, `UsageHistory`), `/v1/me/balance/transactions`.
- **Metrics and events**: `usage_units` and `usage_charged` by `charge` (charged, forgiven) (`POST /v1/metrics/query`); `delinquency.*`; customer facts `order_due`, `order_overdue`, `order_collection_stopped`.
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
Amounts are `int64` native units of their currency end to end. A period is
priced on its total, so its postings sum to R of its quantity however the usage
arrived, and history's 15-minute buckets, so any zone's days, sum to what posts.
Each item is one event (UUIDv5 of
merchant, lease and number, or of the request id); a resend inserts nothing, and
postings read `FINAL`, so a duplicate copy never counts. Each posting has its own
ledger key (`OpUsagePosting`, `usage_posting:<id>`, `1`). Postings are per
currency; the one FX use, a spend window's cap, is in
[entitlements and limits](entitlements.md).
Checks:
- `usage_price_test.go` prices a period's total, a card revision and a volume
  crossing, and history by bucket folded into two zones' days; OR-J24-S49
  posts them and OR-J24-S57 reads them by local day.
- `postPeriod` fails if the ledger already holds the posting.
- No `fx` import in `internal/modules/money/usage_intake.go`, `usage_price.go` or
  `usage_posting.go`.
- Resends post once: OR-J24-S01, OR-J24-S06 to S08, OR-J24-S47, OR-J23-S21.

### 4. Access control and tenant isolation
Running-charge routes declare the `merchant:usage:manage` or `merchant:usage:read`
pair; `/v1/me/usage` reads the signed-in customer. The merchant comes from the
credential (`merchant.Require`) and every table is keyed by `merchant_id`. The
cross-merchant due scans run each row in its own merchant scope.
Checks:
- Each running-charge route in `internal/http/routes/metering.go` has a
  `Permission` pair.
- Every tenant query in `internal/db/queries/usage_postings.sql`,
  `usage_billing.sql` and `usage_orders.sql` filters on `merchant_id`; due scans
  are marked `CROSS-MERCHANT`. Every event store query names the context's
  merchant (`internal/eventstore`); OR-J24-S55 keeps equal ids apart.

### 5. Contracts match the code
The running-charge types are in `api/go.txt`, the routes in the route catalog and
`api/openapi.json`, the tables in `api/schema.txt` and `api/clickhouse.txt`. Before
v1.0.0 a release may still break them: `0025_usage_on_event_store` dropped the
usage accruals and rollups.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `LocalClient`'s only running-charge methods in `api/go.txt` are the two `…Tx`
  forms above.

### 6. Data lifecycle
Classes, in each table comment and `internal/retention/tables.go`: permanent
`usage_postings`, `usage_card_prints` and `running_charges`; state
`usage_unposted` and the meter and rate-card tables. The event store's tables
expire by TTL (`retention.EventStoreTTLs`): raw events 100 days, period
quantities 100 days, minutes 2 days, history buckets 25 months, limit windows 61 days
after they end. A SCIM deletion clears a customer's invoker and metadata there
(`EraseCustomer`, OR-J24-S53).
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.

### 7. Work scales with activity
The close reads only customers due (`usage_unposted_due_at_idx`), in keyset
pages of 200. The usage pass reads due accounts
(`money_settings_usage_due_at_idx`) and due collections
(`next_collection_attempt_at`). A sync reads one customer's period position and
its views' limit rows in one query each; a balance reads the store only for a
customer with unposted usage.
Checks:
- `UsageCloseWorker.Pass` pages `ListDueUsageUnposted`; an idle pass reads no row.
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
- **J24**, intake and posting: OR-J24-S01, S02, S04 to S11, S21, S22, S36, S47 to S55 (replay, pricing on totals, month-end posting, owed dating, lots, the store out of reach, start refusals, erasure, archive, tenancy). Its lease scenarios are in [entitlements and limits](entitlements.md).
- **J25**, running charges and balance looks: OR-J25-S01 to S05, S37, S40 to S42.
- **J26**, usage billing and delinquency: OR-J26-S01 to S04, S30, S40, S50 to S58.

Other tests: `internal/modules/money/rating_test.go` prices every accepted model; `usage_price_test.go` a period's total and history by bucket and zone; `internal/modules/limits/window_test.go` a rate level from buckets.

Gaps:
- Nothing checks that `retention.Tables` matches the schema; it is review-only.
