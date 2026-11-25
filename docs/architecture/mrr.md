# Recurring revenue (MRR)

OpenRails records monthly recurring revenue as subscriptions change: one movement per customer and currency whose MRR changed, scheduled churn and contraction before they take effect, and the MRR at risk and why. Subscriptions themselves are [subscriptions](subscriptions.md); the measures, reports and dashboard that read MRR are [reporting](reporting.md). Integration guide: [subscriptions](../client.md#subscriptions).

## Concepts

- **MRR**: what a live subscription adds to monthly recurring revenue, in its price's currency (`subscription_mrr`, served as `mrr`).
- **Movement** (`mrr_movements`, with `mrr_movement_lines` per subscription): one change of a customer's MRR in one currency, of a kind: `new`, `reactivation`, `expansion`, `contraction`, `churn`, `opening` or `correction`.
- **Scheduled change** (`mrr_scheduled`): churn, contraction or expansion decided now and effective later.
- **At-risk case** (`mrr_risk_cases`, with an `mrr_risk_episodes` row per reason): a subscription adding MRR while a reason puts it at risk, closed `recovered`, `churned` or `superseded`.

## How it works

**Amount.** A subscription's monthly recurring revenue is its price's list amount per seat normalised to a month (`billing.price_mrr`): divided by its calendar months or by a named cadence's (720, 2160, 4320 and 8760 hours are 1, 3, 6 and 12), times 52/12 for a week, or for any other window times 730.485 hours, the average Gregorian month, over its hours; rounded to a native unit once, times its seats, in the price's currency, and held at 10^15 native units with a warning. Provider discounts are not seen. The metrics `mrr` measure and the fleet view sum the same function. Seats are at most 10,000. It counts while the subscription is live, or canceled with its end ahead, and is 0 while pending, ended, or in a trial: a price with a trial phase counts from the subscription's first renewal. A Stripe `trialing` subscription is mirrored as `active` and counts at list price, since no trial is recorded for it.

**Movements.** `trg_subscriptions_track_mrr` keeps `subscription_mrr` for every writer. At commit, `billing.settle_mrr` writes one `mrr_movements` row per customer and currency whose MRR changed, with one `mrr_movement_lines` row per subscription, dated by the writer's `updated_at`. The kind is the customer's: `new` (first MRR), `reactivation` (MRR again after none), `expansion`, `contraction`, `churn` (to none; `voluntary` for a customer's or merchant's cancel), `opening` (an import adopting subscriptions) or `correction` (a prune or an undo). A cancel and the subscription replacing it in one transaction are one movement, caused `upgrade` when one is; an import names itself with the `openrails.mrr_cause` setting. The first dirty row's event settles the merchant; later events find their rows recorded, so a commit's cost grows with what it changed. The functions that keep and settle `subscription_mrr` plan each read for the table as it is (`plan_cache_mode`), with sequential scans off: every read is keyed, and a plan a session cached while the table was small would otherwise read it whole for each row of a bulk write. It is the second step of the one commit-time sequence (`billing.settle_commit`), after the ledger's rollups and before the event streams, whichever a transaction changed first ([books](books.md#how-it-works)); the at-risk rollups slot in after `mrr_totals`.

**Scheduled changes.** A cancellation at period end is scheduled churn (`mrr_scheduled`): MRR is unchanged until the end, a resume reverses it, and at the end the hourly pass's `openrails.mrr_due` scan, which reads `subscription_mrr.next_change_at` by index, records the churn dated at the end. A seat or price change while canceled moves the scheduled churn with it; a prune supersedes it. A scheduled change to a lower or higher price or seat count is scheduled contraction or expansion from the renewal it applies at; canceling the change reverses it, and a later cancellation supersedes it.

**Rollups.** `mrr_totals` (MRR, customers and subscribers now per currency) and, per 15-minute UTC bucket (`billing.mrr_bucket`) of each movement, `mrr_flows` (movements by kind, with the churn of a customer's or merchant's cancel again as `churn_voluntary` and of a chargeback as `churn_chargeback`), `mrr_product_flows` (with `mrr_product_totals`, each product's now), `mrr_cohort_flows`, `mrr_streak_flows` (expansion, contraction, churn and its parts, and corrections, with their logo change, by the bucket in which the customer's run of MRR began, its latest movement from none) and `mrr_customer_flows` (customers gaining MRR in a first currency less those losing it in their last, with `mrr_customer_totals`), all written at commit. `trg_subscriptions_track_cancellations` keeps each cancel decision in `subscription_cancellations`, dated by the writer's `updated_at`, and the resume that reverses it. Scheduled churn, contraction and expansion added, reversed, effected or superseded in a period are read from `mrr_scheduled` by its `scheduled_at` and `resolved_at` indexes. Every UTC offset is a whole number of quarter hours, so a read groups buckets into the merchant's local days and months with `AT TIME ZONE`, the zone passed from its `accounting.timezone` (IANA, default UTC); a change of zone needs no rebuild, and no SQL that writes MRR depends on a zone. MRR at an instant is `mrr_totals` less the movements of the buckets from it on. Nothing is summed across currencies.

**At risk.** A live subscription adding MRR is at risk while `past_due`, `needs_payment_method` (`awaiting_method`, or a provider's past due whose latest decline's action is `fix_payment_method`), `unverified`, `card_expiring` (the card OpenRails charges expires by its period end: an engine subscription's own card or the customer's default; an NMI schedule's only when the subscription names it) or `delinquent` (its customer, in its currency: `customer_delinquency`). `subscription_mrr.risk` holds the reasons; at commit `billing.settle_mrr` opens a case (`mrr_risk_cases`) with the first reason, chains an episode per reason (`mrr_risk_episodes`; a delinquent one keeps the overdue amount), and closes it `recovered` (healthy again while adding MRR), `churned` (its MRR ended while the case was open; `voluntary` for a customer's or merchant's cancel) or `superseded` (a prune, an upgrade, a change of currency). A cancellation at period end keeps the case open until the MRR ends; a resume continues it. Meanwhile the reasons its status gave stay as they were at the cancel, and `card_expiring` and `delinquent` are taken anew, so their episodes close when they clear. `mrr_risk_totals` and 15-minute `mrr_risk_flows` keep at-risk MRR per reason (`any` counts a subscription once) and the closed cases by entry reason. A change of a card, a default card, a provider's decline or delinquency appends a row to `mrr_risk_due`, so no writer waits on another; the row's notification wakes a drain at commit (`openrails.mrr_risk` every 30 seconds is the backstop), which claims rows no other drain holds (`FOR UPDATE SKIP LOCKED`), locks each customer's subscriptions in id order, re-evaluates them (passing over a customer without MRR) and dates the change at the event. A change committed after a claim stays for the next. When a batch fails, the drain takes its customers one at a time; one that still fails is set aside (`held_until`, an hour) with an `mrr.risk.evaluation_failed` finding, and the rest of the merchant drains. A later evaluation of the customer takes the changes set aside and closes the finding. Merchant routes serve `mrr_risk`; `at_risk` and `risk_reason` list the subscriptions at risk from `subscription_mrr`'s index of them.

## Interfaces

- HTTP and Go: Merchant routes serve `mrr` on each subscription, with its scheduled change; `scheduled_change_before` lists the subscriptions whose MRR is scheduled to change by a date, read from `mrr_scheduled`'s index of open entries. The subscription routes and their `Client` methods are in [subscriptions](subscriptions.md#interfaces).
- Metrics: the `mrr` family and the MRR movement, retention and at-risk measures ([reporting](reporting.md)).
- Configuration: the merchant's `accounting.timezone`.
- Jobs: the `openrails.mrr_due` scan of the hourly billing pass; `openrails.mrr_risk` ([River jobs](../river-jobs.md)).

## Guarantees

### 1. Exactly-once provider calls
Does not apply: MRR calls no payment provider.

### 2. Agreement with providers
MRR follows the subscriptions as they mirror their providers: a provider-run subscription's MRR moves when converge records its change, a Stripe portal cancel and resume included. Provider discounts are not seen, and a Stripe `trialing` subscription counts at list price.

Checks:
- OR-J42-S01–S10 include a Stripe portal cancel and resume and a CCBill upgrade.

### 3. Exact money
MRR is an integer in the price's native units, rounded once (`billing.price_mrr`), held at 10^15 native units, and never summed across currencies; a customer's movement is per currency.

Checks:
- OR-J42-S01–S10: movements normalised per currency.

### 4. Access control and tenant isolation
MRR is served on the merchant's subscription routes and read through its metrics routes, each with their permissions. Every query in `mrr.sql` names `merchant_id` but the deployment-wide due reads `ListDueSubscriptionMRR` and `ListMRRRiskDueMerchants` (marked `CROSS-MERCHANT`).

### 5. Contracts match the code
`billing.SubscriptionMRR`, `MRRRisk` and the subscription list's `scheduled_change_before`, `at_risk` and `risk_reason` are in `api/go.txt` and `api/openapi.json`; the tables are in `api/schema.txt`.

Checks:
- `go run ./scripts/contracts` shows no removed or changed line here.

### 6. Data lifecycle
State: `subscription_mrr`, `customer_mrr`, `mrr_risk_due` and the rollups. Permanent: `mrr_movements`, `mrr_movement_lines`, `mrr_scheduled`, `mrr_risk_cases`, `mrr_risk_episodes`. None is partitioned; reads group the 15-minute rollups. A merchant archive carries the MRR tables; a restore skips their triggers, so `billing.check_billing_restore_mrr` proves at commit that subscriptions, customers, movements, totals and open risk episodes agree, and an archive from before MRR opens it (`billing.seed_mrr`), dating a backfilled case when its delinquency or unverified state began. A backfilled case's at-risk amounts enter `mrr_risk_flows` at the backfill (the migration or the restore), as MRR's opening does, so at-risk MRR before then reads zero even where the case began earlier. The source's queued risk changes stay at the source: the restore queues every customer with MRR for re-evaluation.

Checks:
- Table comments carry the `Retention:` rule in `internal/retention/tables.go`.
- Each table is in `internal/merchantarchive/owned_tables.go`.

### 7. Work scales with activity
MRR is recorded by triggers as subscriptions change and settled at commit; `openrails.mrr_due` reads only subscriptions whose end has come, the risk drain only the changes queued, and a past day's MRR and at-risk MRR read the bucketed rollups, never the movements or cases.

Checks:
- `ListDueSubscriptionMRR` reads `subscription_mrr.next_change_at` by index.
- OR-J42-S06: a past day's MRR, and an index for each report read and the due read; OR-J42-S09 and S10: a bulk import and the backfill read `subscription_mrr` in proportion to the book.

### 8. Abuse protection
Does not apply: MRR has no entry point of its own.

### 9. Destructive-action safeguards
Does not apply: MRR records what the subscriptions did; a prune or an undo moves it as a `correction`, and supersedes what it scheduled.

### 10. Consistent time
Movements are dated by the writer's `updated_at`, in UTC, and rolled up into 15-minute UTC buckets (`billing.mrr_bucket`). Every UTC offset is a whole number of quarter hours, so a read groups buckets into the merchant's local days and months in its `accounting.timezone` (IANA, default UTC): month ends and daylight saving follow the zone, a change of zone needs no rebuild, and no SQL that writes MRR depends on a zone. A price on a calendar cadence divides by its months, never by days; any other window uses the average Gregorian month (730.485 hours).

Checks:
- OR-J42-S30 reads every MRR measure in UTC and in a non-UTC zone.
- `billing.mrr_bucket` truncates to the quarter hour in UTC.

## Tests

- OR-J42-S01–S10: MRR movements once per change and normalised per currency; scheduled churn, resume and end; scheduled contraction applied or reversed; a Stripe portal cancel and resume; a CCBill upgrade and an import; MRR at a past day; an engine upgrade; a prune and its undo; a bulk import; the backfill.
- OR-J42-S11–S15: at-risk MRR as past due (recovered, then churned), needing a payment method then past due, a card expiring before its renewal, unverified, and usage delinquency.
- OR-J42-S16–S19, S21: the risk drain and a decline for one customer both commit, the decline queued after the claim re-evaluated; a cancel at period end keeps the case open, a resume continues it, the end churns it once; a new default card puts a subscription following it at risk, never an NMI schedule; an upgrade supersedes the case; a restore carries the open case and re-evaluates every customer with MRR.
- OR-J42-S22, S23: a customer whose evaluation fails alone is set aside with a finding while the rest drain, and a later evaluation takes its changes and closes the finding; canceled at period end, a card update clears `card_expiring` and recovers the case.
- OR-J42-S30: every MRR measure, alone, by each dimension and filtered, in UTC and in another zone, plans on indexes.
- OR-J42-S32: MRR leaving a product, a currency and a customer settles, at once and at a period's end, the totals falling to none.
