# Usage billing

Usage billing decides what usage costs and bills it in arrears. Usage lives in the event store (ClickHouse); what intake settled is in PostgreSQL's running totals, whose provisional price is pending in the customer's balance at once, and OpenRails posts the exact price of each period's total to [Books](books.md) once a day, on one receipt with what running charges accrued that day; usage invoices bill what is owed, and running charges bill a rate per hour. How much a customer may use, and the gate that enforces it, are [entitlements and limits](entitlements.md).

## Concepts

- **Meter**: what a usage item counts, `count` or `sum` of a `value_property`, for
  one event type, optionally in matrix cells (`catalog_meters`).
- **Rate card**: a meter's price in one currency: `per_unit`, `tiered` (graduated,
  or volume as a discount), `package` or `dynamic`, with an optional allowance (`catalog_rate_cards`, which
  also holds flat, unmetered fees).
- **Usage event**: one usage item in the event store's `usage_events`: what was reported, its identity and its priced meter cells (card print and pricing period). Intake inserts into `usage_intake`, which stores nothing; its views write the event and the rollups: limit windows (from the windows that count the item) and history.
- **Usage history** (`usage_history`, kept for good): one row per UTC quarter
  hour, customer, currency, event type and meter cell. A row with no meter
  counts the bucket's events (`requests`; `failed_requests`, `failed_amount`
  and `forgiven` of the failed ones); a meter cell's row what they took of it
  (`requests`, `quantity`, `stated`, `amount` at intake's price and
  `discount`, a volume tier's fall), so amounts less discounts sum to what
  posts. Reads group the quarter hours into any zone's hours, days, weeks and
  months, exactly. The invoker is only in raw events (90 days).
- **Running totals** (`usage_totals`): what intake settled, per customer, currency, pricing period, meter cell, card print and lapse segment: one row per lease until a posting takes it, then one posted row per period, meter cell and print. They are the next item's tier position and what a posting prices.
- **Pending usage**: the unposted totals' provisional prices; `usage_pending` is the posting queue of every customer with usage or a running charge not posted. A balance is its ledger account less its pending usage and running charges' accrual.
- **Posting**: the daily (or inline) event that moves usage and running charges into the ledger. It writes one **usage receipt** (`usage_receipts`) per customer and currency: its `lines` per pricing period, meter, cell and card print (a period whose price fell has negative lines), its `charges` per running charge (from its last posting to the posting's instant or its stop), and ledger transfers (one per credit lot drawn, plus owed, and a lot for a fall), in one transaction.
- **Usage invoice** (`origin: usage`): bills a period's owed usage.
- **Running charge**: bills a rate per hour (`running_charges`); a stop records `stopped_at` and posts nothing.

## How it works

**Intake.** Usage arrives on a lease sync (`POST /v1/app/leases/sync`, [entitlements and limits](entitlements.md)). Items are numbered per lease: a resend answers from the sync's receipts, a changed item is `idempotency_key_reused`, a gap `usage_out_of_order`. A sync settles the new items it carries in order, up to a gap, in chunks of at most `leases.UsageChunkCustomers` customers (and `leases.UsageChunkItems` items), and starts no chunk after the first once `leases.UsageBudget` of it has passed: `through_n` is the last item it settled, and the gate sends the rest again at once (OR-J24-S34, OR-J24-S70). Each lease number settles once, in the transaction that advances `through_n`, which adds its item to its lease's running totals; a resend at or below `through_n` settles nothing again, its receipt kept or not (OR-J24-S72, OR-J24-S75). Only once that transaction commits does the sync insert what it settled into the event store, in one insert tried up to three times, and answer. A sync that fails before its commit inserted nothing: the resend settles and inserts its items once, priced as if the failed sync never ran (OR-J24-S73, OR-J24-S74). A resend of what committed inserts nothing, so no rollup counts an item twice. What the store never takes, after a crash between the commit and the insert or a store that refuses every try, is charged and missing for good from raw usage, history and limit usage; the sync logs it at error level with the lease and its numbers (OR-J24-S47, OR-J24-S86). A gate never reports the balance in flight: a sync refuses an `in_flight` line naming it. Intake reads and writes a chunk's customers together: their rows, balance accounts, posting queue rows, period anchors, lot lapses, trust levels and billing policies. While ClickHouse is out of reach a sync answers 503 and settles nothing: it pings the store before it commits (OR-J24-S48). An item's metadata is at most `billing.MaxUsageMetadataBytes` (1,024) of compact JSON; a longer one is refused alone (OR-J24-S68).

**Pricing.** `MoneyService.PrepareUsageTx` validates, prices and locates, taking a customer's lock only to open a session or first-use window or to charge a failed item (and locating that customer's items again under it): each group sharing a period, meter cell and card print (`cardPrint`) is priced provisionally at the period position the running totals hold (`ListUsagePositions`: every lease's settled usage and what posted), `R(Q+q) − R(Q)` never below zero (`marginalPrice`), shared by quantity; where a volume tier's price falls, the fall is the group's `discount`, in history only: pending never nets it, so two gates pricing from one position cannot both take it off. A `dynamic` card charges each item its stated amount. Each item counts in the lapse segment it occurred in (`lapseSegment`): the latest lapse at or before it of the customer's lots that hold units (`ListLotLapses`), else its period's start. Limits are located at `admitted_at` (`locateItems`); a failed item's grace is read from its windows' `limit_usage`, plus what the sync's earlier chunks took of them, which the store does not hold yet (OR-J24-S87), and what it leaves is charged with its own ledger legs. `CommitUsageTx`, in the caller's transaction, holds the customers' `usage_pending` rows by key share (`ShareUsagePending`; it writes a row only to move `due_at` earlier), records the card prints, adds each item to its lease's running totals (`AddUsageTotals`), opens session and first-use windows and adds the events to the sync's `money.UsageSettlement`, which the store takes once the sync commits; the insert waits until they read back, so a view answered after the sync counts them. A posting locks the queue row for update, so it waits for every intake that may have settled usage, and later intakes settle after it commits; intakes never wait on each other, since only one lease's syncs write its totals. Concurrent intakes of one customer may price from the same period position: postings price the period's total, so only provisional amounts differ.

**Posting.** `MoneyService.postUsageTx` is the one choke point: under the customer lock and its queue row's, per pricing period it takes usage of, the price of the period's settled quantity (its posted row and the rows taken), card by card in print order (`usagePrints.cellPrice`), less the price of its posted row alone, which is what the period's earlier receipts took (`postingDelta`); and each running charge from its last posting through the posting's instant, no later than its stop, whose row moves first so the balance no longer counts it (`takeRunningCharge`). It writes one receipt of both (`postReceipt`) and settles its falls first (`settle`): the periods whose price fell (a volume tier) are credited at the receipt's coordinate, as a lot (`usage_reversed`), so a period's receipts sum to the price of its quantity after every posting; then the periods whose price rose and the running charges are spent together, the lots first (that lot included), one `credit_spend` per lot, the rest one `owed_accrual`, whose owed items are split by period and meter, each dated before its period's end and the cut, and by running charges, dated before the instant they were posted through, never before the last usage invoice's end; the general ledger's `usage_accrued` posts once per date (OR-J24-S89, OR-J24-S90). A posting takes every settled usage: its cut is now plus `retention.UsageClockSkew`, and it draws the lots live at now; one before a lot's lapse takes the lapse segments before it, exactly the usage before the lapse, and the running charges through the lapse (OR-J24-S22, OR-J24-S61). It folds what it took into the posted rows (`FoldUsageTotals`), which ends their pending price, and leaves the queue row due at the end of the accounting day each running charge reached, or deletes it with nothing left. It runs (`usage_receipts.reason`): `daily`, once the accounting-zone day of a customer's oldest pending usage or running charge ended, its running charges through that zone's last midnight (`UsagePostingWorker`, the first scan of the hourly billing pass); `billing`, before a usage invoice bills what is owed, through the window's end; `credit`, before a credit lot's expiry, revocation or refund and before an invoice is paid from the balance, so lots are spent in time order (`db.UsageLedger`: an expiry posts through the lapse, expires, then posts the usage since); `purge`, before a merchant purge. Lots that lapsed expire first, each after what came before its lapse posted (`postEverything`). A running charge's start queues its customer at the end of the accounting day it started in; a stop posts nothing, and its final accrual is pending until the next posting (OR-J25-S45, OR-J25-S46). Deposits post nothing: they draw no lot, and spends are gated by the available amount, which already deducts pending usage. No posting reads the event store, so a store that lost usage (a restore, another store attached) changes nothing that posts, and a store out of reach stops none (OR-J24-S62, OR-J48-S20). A merchant's own unit is never owed: in a posting in one, a period whose price fell first takes back what its earlier receipts forgave and credits the rest; the rises take what the lots and that credit hold, and the rest is forgiven, shared across the periods whose price rose by their amounts and each period's across its positive lines, by largest remainder (`usage_receipts.lines` `forgiven`; [credits](credits.md)).

**Billing.** The usage pass (`UsageInvoiceWorker`), on every deployment, with or
without an event store, bills each account whose `money_settings.usage_due_at` has come (its bill date, or
a threshold crossing) as a usage invoice (`BillDueUsageAccount`), then queues due
collections as `order_collection` operations. An invoice bills the pending owed
items of its window; its period starts where their charges began (a posting's
pricing period, a running charge's last posting) and ends at its close. The threshold is its currency's
`usage_billing` `collection_threshold`, else its credit limit; a billing policy's
outstanding cap and `collection_threshold_amount` bind only an account in the
policy's `policy_currency`, as they do in a gate's views (OR-J26-S61). A running charge's accrual is
computed on read from its row and posts with the usage postings above.

**Staleness.** A gate decides on views that lag settled usage by the syncs in between, about four sync intervals (`limits.SyncInterval`, one second): other gates' new usage counts once their sync inserts it, just after it commits, and the balance is never reported in flight. Spending past zero in that window posts as owed, or is forgiven in a merchant's unit (OR-J24-S71). Pending usage is exact as of its intake. A failed item's grace is read from the store as well, so two syncs of one customer a moment apart may each miss the other's failures and forgive up to that much more.

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
  `MarkInvoiceUncollectible`, `RecordExternalPayment`; `GetUsage`,
  `GetUsageSeries` (the customer's reads).
- **Customer**: `GET /v1/me/usage` (totals on any zone's days) and `GET /v1/me/usage/series` (by hour, day, week or month in any zone; at most `billing.MaxUsageBuckets` buckets), both from history by event type or meter (`UsageHistory`), by invoker from raw events within 90 days; `/v1/me/balance/transactions`.
- **Metrics and events**: `usage_units` and `usage_charged` by `charge` (charged, forgiven; forgiveness by rate card, with no invoker) (`POST /v1/metrics/query`); `delinquency.*`; customer facts `invoice_due`, `invoice_overdue`, `invoice_collection_stopped`.
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
Amounts are `int64` native units of their currency end to end. A period is priced on its total, so its receipts sum to R of its quantity however the usage arrived, and two gates that priced from the same position are trued up by the next posting with no lock between them (OR-J24-S84); one receipt may hold a period that rose and one that fell (OR-J24-S88). History keeps each charge at the price intake took it at, which never falls below zero, and where a volume tier's price fell its `discount`, so a period's amounts less discounts telescope to R as well (OR-J24-S81); intakes of one customer that price from the same position at once differ, as their pending amounts do. History is kept by UTC quarter hour; reads group the quarter hours into a zone's dates (`toDate` in the zone) and Go places each at its first instant, so a 23- or 25-hour day holds its hours, a half- or quarter-hour offset is exact and a zone change regroups all of it (OR-J24-S57, OR-J24-S67, OR-J24-S76, OR-J24-S77). Buckets of a series come from `calendarutil.Buckets`, from each day's first instant. Each item is one event (a UUIDv7 derived from merchant, lease and number at the lease's time), sorted by customer and event id; its lease number settles it once in PostgreSQL, and the store takes it only after that commits, so a resend is never charged or counted twice (OR-J24-S47). Each receipt has its own ledger key (`OpUsagePosting`, `usage_receipt:<id>`, `1`); a running charge's total through its posting is `running_charges_posted_check`'s. Postings are per currency; the one FX use, a spend window's cap, is in [entitlements and limits](entitlements.md).
Checks:
- `usage_price_test.go` prices a period's total, a card revision and a volume crossing; `usage_posting_test.go` a period's total across leases less what posted, a receipt holding both signs and forgiveness by line; OR-J24-S49 posts them, OR-J24-S57 reads them by local day and OR-J24-S81 shows a volume fall as the discount.
- `postReceipt` fails if the ledger already holds the receipt.
- No `fx` import in `internal/modules/money/usage_intake.go`, `usage_price.go` or `usage_posting.go`.
- Resends post once: OR-J24-S01, OR-J24-S06 to S08, OR-J24-S47, OR-J24-S73, OR-J24-S74, OR-J23-S21.

### 4. Access control and tenant isolation
Running-charge routes declare the `merchant:usage:manage` or `merchant:usage:read`
pair; `/v1/me/usage` and `/v1/me/usage/series` read the signed-in customer, and
their staff twins (`GetUsage`, `GetUsageSeries`) need `merchant:customer:read`
(OR-J24-S80). The merchant comes from the
credential (`merchant.Require`) and every table is keyed by `merchant_id`. The
cross-merchant due scans run each row in its own merchant scope.
Checks:
- Each running-charge route in `internal/http/routes/metering.go` has a
  `Permission` pair.
- Every tenant query in `internal/db/queries/usage_posting.sql`, `running_charges.sql`, `usage_billing.sql`, `usage_invoices.sql` and `ListLotLapses` filters on `merchant_id`; due scans are marked `CROSS-MERCHANT`. Every event store query names the context's
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
Classes, in each table comment and `internal/retention/tables.go`: permanent `usage_receipts`, `usage_card_prints` and `running_charges`; rows `usage_totals`, deleted by the cleanup job a day after their period's late usage window closes, once none of its rows is unposted (`DeleteClosedUsageTotals`); state `usage_pending` and the meter and rate-card tables. The event store's tables expire by TTL (`retention.EventStoreTTLs`): raw events 90 days (the owner's choice), limit windows a day after they end (usage settled later counts toward no window that ended, OR-J24-S83). Usage history has no TTL: it is kept, names no invoker, and a merchant purge deletes its rows (OR-J24-S78). A merchant archive carries every table as the store holds it (OR-J24-S54). A SCIM deletion clears a customer's invoker and metadata in its raw events under its lock and leaves history as it is; a run stopped part way converges when run again (`EraseCustomer`, OR-J24-S53, OR-J24-S63). A merchant purge deletes the merchant's usage there in a job queued with it (`openrails.merchant_usage_purge`, OR-J32-S07).
Checks:
- The table comments and `internal/retention/tables.go` agree on each class (`TestTablesMatchSchema`).

### 7. Work scales with activity
The daily posting reads only customers due (`usage_pending_due_at_idx`), in keyset pages of 200: about one posting per active customer a day, each reading that customer's running totals (its open periods' posted rows and what its leases settled since) and its unposted running charges (`running_charges_unposted_idx`); a period whose price fell in a merchant's unit also reads the receipts cut since its start (`usage_receipts_customer_idx`). The usage pass reads due accounts (`money_settings_usage_due_at_idx`) and due collections (`next_collection_attempt_at`). A sync reads its customers' period positions, lot lapses and its views' limit rows in one query each; a balance and a posting never read the store's rollups (OR-J25-S43, OR-J24-S52). Running totals stay bounded by open periods and the leases since the last posting: a posting folds what it took into one row per period, meter cell and print.
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
- **J24**, intake and posting: OR-J24-S01, S02, S04 to S11, S21, S22, S36, S47 to S57, S61 to S63, S71 to S75, S84 to S88 (replay, pricing on totals, a posting equal to pending, daily posting, owed past the balance, owed dating, lots and their lapses, the store out of reach, a start without the store, erasure, archive, tenancy, recovery from failed syncs, a store that refuses inserts or lost usage, grace counted once, a receipt holding a fall). Its lease scenarios are in [entitlements and limits](entitlements.md).
- **J25**, running charges and balance looks: OR-J25-S01 to S05, S37, S40 to S42, S45 (a charge posting at the accounting zone's midnight with the day's usage), S46 (a stop posts nothing until the daily posting), S47 to S49 (low amounts, exact crossings).
- **J48**, merchant units: OR-J48-S20 (a posting forgives with the event store out of reach).
- **J26**, usage billing and delinquency: OR-J26-S01 to S04, S30, S40, S50 to S58, S65.
- **J40**, the usage receivable: OR-J40-S20 to S22, S27 to S29.

Other tests: `internal/modules/money/rating_test.go` prices every accepted model; `usage_price_test.go` a period's total and a zone's days; `usage_posting_test.go` a period's total across leases less what posted, a receipt of both signs and the lapse segment an item counts in; `internal/modules/limits/window_test.go` a rate level from buckets.

Gaps:
- No scenario forces two gates to price from the same position at once; OR-J24-S84 shows the posting pricing the total across gates.
