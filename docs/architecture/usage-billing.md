# Usage billing

Usage billing decides what usage costs and bills it in arrears. Usage lives in the event store (ClickHouse); its provisional price is pending in the customer's balance at once, in PostgreSQL, and OpenRails posts the exact price of each period's total to [Books](books.md) once a day; usage invoices bill what is owed, and running charges bill a rate per hour. How much a customer may use, and the gate that enforces it, are [entitlements and limits](entitlements.md).

## Concepts

- **Meter**: what a usage item counts, `count` or `sum` of a `value_property`, for
  one event type, optionally in matrix cells (`catalog_meters`).
- **Rate card**: a meter's price in one currency: `per_unit`, `tiered` (graduated,
  or volume as a discount), `package` or `dynamic`, with an optional allowance (`catalog_rate_cards`, which
  also holds flat, unmetered fees).
- **Usage event**: one usage item in the event store's `usage_events`: what was
  reported, its identity and its priced meter cells (card print and pricing
  period). Intake inserts into `usage_intake`, which stores nothing; its views
  write the event and the rollups: limit windows (from the windows that count
  the item), period quantities, minutes (from its provisional amount) and
  history (`usage_history`, by UTC hour, grouped into the merchant's
  accounting-zone days as it is read).
- **Pending usage**: what usage the ledger has not taken costs at its
  provisional prices, per customer, currency and lease
  (`usage_pending_amounts`); `usage_pending` is the posting queue. A balance
  is its ledger account less its pending usage and running charges' accrual.
- **Posting**: what the ledger took of a period's usage (`usage_postings`).
- **Usage invoice** (`origin: usage`): bills a period's owed usage.
- **Running charge**: bills a rate per hour (`running_charges`).

## How it works

**Intake.** Usage arrives on a lease sync (`POST /v1/app/leases/sync`,
[entitlements and limits](entitlements.md)). Items are numbered per lease: a resend
answers from the sync's receipts, a changed item is `idempotency_key_reused`, a
gap `usage_out_of_order`. A sync settles the new items it carries in order, up to a
gap, in chunks of at most `leases.UsageChunkCustomers` customers (and 250 items), each one insert, and
starts no chunk after the first once `leases.UsageBudget` of it has passed: `through_n`
is the last item it settled, and the gate sends the rest again at once (OR-J24-S34,
OR-J24-S70). A gate never reports the balance in flight: a sync refuses an
`in_flight` line naming it. Intake reads and writes a
chunk's customers together: their rows, balance accounts, posting queue rows, period
anchors, trust levels and billing policies. While ClickHouse is out of reach a sync
answers 503 and settles nothing (OR-J24-S48). An item's metadata is at most
`billing.MaxUsageMetadataBytes` (1,024) of compact JSON; a longer one is refused
alone (OR-J24-S68).

**Pricing.** `MoneyService.PrepareUsageTx` validates, prices and locates, taking a
customer's lock only to open a session or first-use window or to charge a failed
item (and locating that customer's items again under it): each group sharing a
period, meter cell and card print
(`cardPrint`) is priced provisionally at the period position the store holds,
`R(Q+q) − R(Q)` never below zero (`marginalPrice`), shared by quantity; a
`dynamic` card charges each item its stated amount. Limits are located at `admitted_at`
(`locateItems`); a failed item's grace is read from its windows' `limit_usage`
and what it leaves is charged with its own ledger legs. `CommitUsageTx` skips
events an earlier attempt inserted (`ExistingUsage`, by customer and event id at
any `occurred_at`), and, in the caller's transaction, holds the customers'
`usage_pending` rows by key share before it inserts the rest (`ShareUsagePending`;
it writes a row only to move `since_at` or `due_at` earlier), waits for them,
adds their provisional prices to its lease's `usage_pending_amounts` and writes
the card prints and opened session and first-use windows. A posting locks the
queue row for update, so it waits for every intake that may have inserted
usage, and later intakes insert after it commits; intakes never wait on each
other, since only one lease's syncs write its amounts. Concurrent intakes of one
customer may price from the same period position: postings price the period's
total, so only provisional amounts differ. An event's identity never depends on a clock read per attempt: an item an earlier attempt
inserted keeps that attempt's `occurred_at`, `admitted_at` and pricing period
(so its session and first-use windows), and an insert whose caller gave up still
finishes before the intake lets go of its locks (OR-J24-S58 to S60).

**Posting.** `MoneyService.postUsageTx` is the one choke point: under the
customer lock and its queue row's, per pricing period from `usage_pending.since_at`, the price of
the period's deduplicated usage before the cut (`PostingQuantities`, `FINAL`),
card by card in print order (`usagePrints.cellPrice`), less what earlier
postings took (`postingDelta`): balance first, one `credit_spend` per lot live
before the cut, the rest one `owed_accrual` split by meter and dated before the
period's end and the cut, never before the last usage invoice's end. A posting
takes everything the store holds: its cut is now plus `retention.UsageClockSkew`,
and it draws the lots live at now; every usage it read had its provisional price
in `usage_pending_amounts` and nothing else did, so it deletes them. It runs
(`usage_postings.reason`): `daily`, once the accounting-zone day of a customer's
oldest pending usage ended (`UsagePostingWorker`, the first scan of the hourly
billing pass; a month's last day's posting is its close); `billing`, before a
usage invoice bills what is owed; `credit`, before a credit lot's expiry,
revocation or refund and before an invoice is paid from the balance, so lots
are spent in time order (`db.UsageLedger`: an expiry posts through the lapse,
expires, then posts the rest); `purge`, before
a merchant purge. Lots that lapsed expire first, each after what came before
its lapse posted (`postEverything`). Deposits post nothing: they draw no lot, and
spends are gated by the available amount, which already deducts pending usage. Each posting
records how far it read the store (`usage_postings.ingested_through`); a later one
takes, of the usage that occurred before an earlier cut and was ingested by then,
no less than what posted, and prices what came since on top (`takenQuantities`).
So a period whose price fell (a volume tier) is carried and credited
(`usage_reversed`) once its late window closes, while usage the store lost (a
restore, another store attached, a purge) is never credited back (OR-J24-S62).
Usage under a card print its intake never recorded is priced by the card in force
that names it, else left out (OR-J24-S64). A merchant's own unit is never owed: a posting in
one takes what the lots hold and forgives the rest (`usage_postings.forgiven`),
and a reversal takes forgiveness back before it credits
([credits](credits.md)).

**Billing.** The usage pass (`UsageInvoiceWorker`), on every deployment, with or
without an event store, bills each account whose `money_settings.usage_due_at` has come (its bill date, or
a threshold crossing) as a usage invoice (`BillDueUsageAccount`), then queues due
collections as `order_collection` operations. An invoice bills the pending owed
items of its window; its period starts where their charges began (a posting's
pricing period, a running charge's last posting) and ends at its close. The threshold is its currency's
`usage_billing` `collection_threshold`, else its credit limit; a billing policy's
outstanding cap and `collection_threshold_amount` bind only an account in the
policy's `policy_currency`, as they do in a gate's views (OR-J26-S61). A running charge's accrual is
computed on read from its row and posts at its stop, at 00:00 UTC and with the
usage postings above (`postRunningChargesThrough`).

**Staleness.** A gate decides on views that lag settled usage by the syncs in
between, about four sync intervals (`limits.SyncInterval`, one second): other
gates' new usage counts once it settles, and the balance is never reported in
flight. Spending past zero in that window posts as owed, or is forgiven in a
merchant's unit (OR-J24-S71). Pending usage is exact as of its intake: usage
whose sync inserted it and then failed is posted when found, and its gate's
resend adds its price to pending again, so pending can stay high by it until
the next posting.

**Books.** Every owed accrual is usage revenue receivable in the general ledger as it posts, and every way the units book settles owed settles the receivable: a collection by card, remittance or observed provider payment, a repayment from credit, a waiver. An uncollectible usage invoice carries an allowance for doubtful accounts while it stays uncollectible; a chargeback of a usage invoice's payment owes again what it paid, billed at once on a usage invoice whatever its minimum or usage terms (in a currency whose terms are gone, that invoice bills all its pending owed, since nothing else would), and a won dispute's cash beyond what is still owed and the chargeback's loss is the customer's credit. See [books](books.md).

## Interfaces

- **Intake**: `SyncLeases` ([entitlements and limits](entitlements.md)).
- **Running charges** (`merchant:usage:manage`, reads `merchant:usage:read`):
  `StartRunningCharge` (`POST /v1/app/running-charges`), `StopRunningCharge`,
  `GetRunningCharge`, `ListRunningCharges`; `GetBalance`
  ([running charges](../client.md#usage-and-running-charges)); in a host transaction
  (`LocalClient`) `StartRunningChargeTx`, `StopRunningChargeTx`.
- **Catalog**: `ListMeters`, `GetMeter`, `SetMeter`, or a catalog document.
- **Staff**: `UpdateCustomer` (`usage_billing`); `WaiveInvoiceBalance`,
  `MarkInvoiceUncollectible`, `RecordExternalPayment`.
- **Customer**: `GET /v1/me/usage` (read by day of the merchant's accounting zone and priced per day, `UsageHistory`), `/v1/me/balance/transactions`.
- **Metrics and events**: `usage_units` and `usage_charged` by `charge` (charged, forgiven) (`POST /v1/metrics/query`); `delinquency.*`; customer facts `invoice_due`, `invoice_overdue`, `invoice_collection_stopped`.
- **Configuration**: merchant `usage_billing` (its terms turn usage billing on), `billing_period_boundary`
  ([usage billing](../client.md#usage-and-running-charges)).

## Guarantees

### 1. Exactly-once provider calls
Usage billing calls no provider. A usage invoice's card charge is an
`order_collection` provider intent frozen with the invoice's attempt
(`enqueueUsageCollection`) and run by its own job, as
[collection and dunning](dunning.md) describes.
Checks:
- `enqueueUsageCollection` (`internal/modules/money/usage_collection.go`) writes
  the intent and the invoice's attempt in one transaction under the customer and
  invoice locks; a repeated operation key replays the first intent.
- The usage pass only queues and never waits on a gateway (OR-J26-S53).
- A collection refused before dispatch keeps its proof and pays once (OR-J26-S55).

### 2. Agreement with providers
Does not apply: usage comes from the merchant's backend, not a provider; a usage invoice's payment reconciles in [collection and dunning](dunning.md).

### 3. Exact money
Amounts are `int64` native units of their currency end to end. A period is
priced on its total, so its postings sum to R of its quantity however the usage
arrived, and history is priced per day of the accounting zone the same way,
so the days sum to what posts. History is kept by UTC hour; ClickHouse groups
the hours into the zone's days as they are read (`toStartOfDay` in the zone),
so a 23- or 25-hour day holds its hours and a zone change regroups all of it
(OR-J24-S57, OR-J24-S67). A zone whose offset is not whole hours (Kolkata,
Kathmandu) has its days cut at the hour: an hour counts on the day it starts
in.
Each item is one event (a UUIDv7 derived from
merchant, lease and number at the lease's time, or from the request id), sorted by customer and event
id; a resend inserts nothing, and postings read `FINAL`, so a duplicate copy never
counts. Each posting has its own
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
- Resends post once: OR-J24-S01, OR-J24-S06 to S08, OR-J24-S47, OR-J24-S58 to S60, OR-J23-S21.

### 4. Access control and tenant isolation
Running-charge routes declare the `merchant:usage:manage` or `merchant:usage:read`
pair; `/v1/me/usage` reads the signed-in customer. The merchant comes from the
credential (`merchant.Require`) and every table is keyed by `merchant_id`. The
cross-merchant due scans run each row in its own merchant scope.
Checks:
- Each running-charge route in `internal/http/routes/metering.go` has a
  `Permission` pair.
- Every tenant query in `internal/db/queries/usage_postings.sql`,
  `usage_billing.sql` and `usage_invoices.sql` filters on `merchant_id`; due scans
  are marked `CROSS-MERCHANT`. Every event store query names the context's
  merchant (`internal/eventstore`); OR-J24-S55 keeps equal ids apart.

### 5. Contracts match the code
The running-charge types are in `api/go.txt`, the routes in the route catalog and
`api/openapi.json`, the tables in `api/schema.txt` and `api/clickhouse.txt`. Before
v1.0.0 a release may still break them.
Checks:
- `go run ./scripts/contracts` leaves the snapshots unchanged.
- `LocalClient`'s only running-charge methods in `api/go.txt` are the two `…Tx`
  forms above.

### 6. Data lifecycle
Classes, in each table comment and `internal/retention/tables.go`: permanent
`usage_postings`, `usage_card_prints` and `running_charges`; state
`usage_pending`, `usage_pending_amounts` and the meter and rate-card tables. The event store's tables
expire by TTL (`retention.EventStoreTTLs`): raw events 90 days (the owner's
choice; a period's last posting reads them up to 66 days on), period quantities
100 days, minutes 2 days, limit windows 61 days after they end. Usage history has
no TTL: it is kept, and a merchant purge deletes its rows. A merchant archive carries every table as the store holds
it (OR-J24-S54). A SCIM deletion clears a customer's invoker and metadata there
under its lock; a run stopped part way converges when run again (`EraseCustomer`,
OR-J24-S53, OR-J24-S63). A merchant purge deletes the merchant's usage there in a
job queued with it (`openrails.merchant_usage_purge`, OR-J32-S07).
Checks:
- The table comments and `internal/retention/tables.go` agree on each class.

### 7. Work scales with activity
The daily posting reads only customers due (`usage_pending_due_at_idx`), in keyset
pages of 200: about one posting per active customer and period a day. The usage pass reads due accounts
(`money_settings_usage_due_at_idx`) and due collections
(`next_collection_attempt_at`). A sync reads one customer's period position and
its views' limit rows in one query each; a balance never reads the store, and a
balance look and a lot's expiry read it only for a customer with pending usage
(OR-J25-S43).
Checks:
- `UsagePostingWorker.Pass` pages `ListDueUsagePending`; an idle pass reads no row.
- OR-J26-S40 and OR-J26-S52 bound the usage pass.

### 8. Abuse protection
Does not apply: usage enters through syncs, bounded in [entitlements and limits](entitlements.md); `/v1/me/usage` takes no OpenRails rate limit, and the proxy owns a generic ceiling ([limits](../standalone.md#abuse-limits)).

### 9. Destructive-action safeguards
Waiving or writing off a usage invoice is a `waive_order` or `mark_uncollectible`
command (`MoneyService.destructiveUsageInvoice`). Settled usage is never reversed,
and settling it is never refused for money.
Checks:
- `destructiveUsageInvoice` (`internal/modules/money/usage_invoice.go`) uses
  `destructive.WaiveInvoice` and `destructive.MarkUncollectible`.
- OR-J26-S03: paid, waived or written-off debt is not charged again; OR-J26-S50:
  each clears delinquency in the same change.

## Tests

Critical journeys, over real PostgreSQL, Redis and Vault:
- **J24**, intake and posting: OR-J24-S01, S02, S04 to S11, S21, S22, S36, S47 to S64, S71 (replay, pricing on totals, daily posting, owed past the balance, owed dating, lots, the store out of reach, start refusals, erasure, archive, tenancy, recovery from lost commits and a store that lost usage). Its lease scenarios are in [entitlements and limits](entitlements.md).
- **J25**, running charges and balance looks: OR-J25-S01 to S05, S37, S40 to S42.
- **J26**, usage billing and delinquency: OR-J26-S01 to S04, S30, S40, S50 to S58.
- **J40**, the usage receivable: OR-J40-S20 to S22, S27 to S29.

Other tests: `internal/modules/money/rating_test.go` prices every accepted model; `usage_price_test.go` a period's total and history by bucket and zone; `usage_posting_test.go` the posting queue and what a posting takes after the store lost usage; `internal/modules/limits/window_test.go` a rate level from buckets.

Gaps:
- Nothing checks that `retention.Tables` matches the schema; it is review-only.
