# Recurring revenue (MRR)

OpenRails records monthly recurring revenue as subscriptions change: one movement per customer and currency whose MRR changed, each cancellation at period end as a movement dated at its end, and the MRR at risk for a subscription's status. Every measure is computed from these facts when it is read. Subscriptions themselves are [subscriptions](subscriptions.md); the measures, reports and dashboard that read MRR are [reporting](reporting.md). Integration guide: [subscriptions](../client.md#subscriptions).

## Concepts

- **MRR**: what a live subscription adds to monthly recurring revenue, in its price's currency (`subscription_mrr`, served as `mrr`).
- **Movement** (`mrr_movements`, with `mrr_movement_lines` per subscription): one change of a customer's MRR in one currency, of a kind: `new`, `reactivation`, `expansion`, `contraction`, `churn` or `correction`.
- **Scheduled movement** (`mrr_movements.scheduled`): a cancellation at period end, written when it is decided and dated at its end; it counts once that instant passes. A scheduled price or seat change (`scheduled_changes`) is priced when read.
- **At-risk case** (`mrr_risk_cases`, with an `mrr_risk_episodes` row per reason and one of reason `any`): a subscription adding MRR while its status puts it at risk, closed `recovered`, `churned` or `superseded`.

## How it works

**Amount.** A subscription's monthly recurring revenue is its price's list amount per seat normalised to a month (`billing.price_mrr`): divided by its calendar months or by a named cadence's (720, 2160, 4320 and 8760 hours are 1, 3, 6 and 12), times 52/12 for a week, or for any other window times 730.485 hours, the average Gregorian month, over its hours; rounded to a native unit once, times its seats, in the price's currency, and held at 10^15 native units with a warning. Provider discounts are not seen. The metrics `mrr` measure and the fleet view sum the same function. Seats are at most 10,000. It counts while the subscription is live, or canceled with its end ahead, and is 0 while pending, ended, or in a trial: a price with a trial phase counts from the subscription's first renewal. A Stripe `trialing` subscription is mirrored as `active` and counts at list price, since no trial is recorded for it.

**Movements.** `trg_subscriptions_track_mrr` keeps `subscription_mrr` (each subscription's level, its end and its status reasons) for every writer. At commit, `billing.settle_mrr` settles each customer whose rows changed (`settle_customer_mrr`, the customers taking turns on an advisory lock no other writer takes): one `mrr_movements` row per currency whose MRR changed, dated at the latest change and never before a movement already recorded, with one `mrr_movement_lines` row per subscription carrying the movement's instant and currency. A subscription moving to another product or currency leaves one and enters the other; at an equal MRR the lines stand alone. The kind is the customer's, from its MRR at the change: the sum of its movements dated by then, whatever order they were written in: `new` (first MRR), `reactivation` (MRR again after none), `expansion`, `contraction`, `churn` (to none; `voluntary` for a customer's or merchant's cancel) or `correction` (a prune or an undo). `customer_logo_delta` counts the customer's first currency gained and last lost. A cancel and the subscription replacing it in one transaction are one movement, caused `upgrade` when one is; an undo names itself with the `openrails.mrr_cause` setting. The first dirty row's event settles the merchant; later events find their rows recorded, so a commit's cost grows with what it changed. The functions plan each read for the table as it is (`plan_cache_mode`), with sequential scans off. It is the second step of the one commit-time sequence (`billing.settle_commit`), after the ledger's rollups and before the event streams ([books](books.md#how-it-works)).

**Imported history.** An import is not an event in the business: nothing is booked at its instant. The subscriptions a billing import lands (a host's book, or a PSP import's) add MRR from their start at their own dates (`billing.record_mrr_history`, in the import's transaction): each to its end when that came by the import's `as_of`, priced by the bills it paid (a change of price or seats dated at the bill's period start), its last price what its `subscription_mrr` row holds at `as_of`; a subscription that paid for nothing adds none. A customer's imported movements (`imported`) are written again with each import of the customer, in time order, each classified by the customer's MRR then as a settlement classifies, live movements included and left as written; its ends ahead of `as_of` are scheduled movements. The rows are recorded, so the commit settles nothing of them. Migration 0015 turned each `opening` into the same history: its subscriptions from their start to the opening, at what it counted.

**Cancellations at period end.** Every settlement of a customer deletes its scheduled movements dated after the change and writes them again from its subscriptions' ends, in time order: churn, or contraction when other MRR stays. A resume removes it, a seat or price change while canceled reprices it, an add-on bought before the end turns its churn into contraction, and a prune removes it (an undo writes it again). Nothing runs at the end: a read is bounded by its own instant, so the movement counts from its date, and a subscription's MRR is its level while its `ends_at` is ahead. Once the end has come, the row keeps its level and end until the subscription adds MRR again, so a settlement dated before the end writes its churn again. `trg_subscriptions_track_cancellations` keeps each cancel decision in `subscription_cancellations` and the resume that reverses it.

**Reads.** The metrics read the facts at query time ([reporting](reporting.md)): movements by `(merchant_id, occurred_at)` and `(merchant_id, cohort_at)`, lines by `(merchant_id, occurred_at)`, cases by `closed_at` and episodes by `entered_at` and `left_at`, each an index-only range scan. A snapshot sums the rows before each edge in one scan; nothing is summed across currencies. Results are cached 60 seconds by merchant and plan, in Redis when configured. A merchant past about a million movements is the point for a monthly close; there is none.

**At risk.** A subscription adding MRR is at risk while `past_due`, `needs_payment_method` (`awaiting_method`) or `unverified`, from its own status (`subscription_mrr.risk`; canceled at its period end it keeps them until its MRR ends), and, read from state alone, `card_expiring` (the card OpenRails charges expires by its period end: an engine subscription's own card or the customer's default; an NMI schedule's only when the subscription names it) or `delinquent` (its customer, in its currency). `billing.mrr_risk_now` gives all five at an instant: the subscription's `mrr_risk`, the list's `at_risk` and `risk_reason`, and `mrr_at_risk` from the query's instant on read it. The status reasons keep history: at commit `billing.settle_mrr` opens a case with the first reason, an episode per reason and one of reason `any`, and closes it `recovered` (healthy again while adding MRR), `churned` (its MRR ended; `voluntary` for a customer's or merchant's cancel) or `superseded` (a prune, an upgrade, a change of currency). A change of the subscription's MRR while at risk ends its open episodes at the old amount and begins them at the new. A cancellation at period end closes the case `churned` at the end, written then; a resume before it reopens the case. `card_expiring` and `delinquent` keep no history: their cases ended `superseded` when this design shipped.

## Interfaces

- HTTP and Go: Merchant routes serve `mrr` on each subscription, with its scheduled change, and `mrr_risk`; `scheduled_change_before` lists the subscriptions whose MRR is scheduled to change by a date (`subscription_mrr.ends_at`, and `billing.mrr_scheduled_changes` from `scheduled_changes`). The subscription routes and their `Client` methods are in [subscriptions](subscriptions.md#interfaces).
- Metrics: the `mrr` family and the MRR movement, retention and at-risk measures ([reporting](reporting.md)).
- Configuration: the merchant's `accounting.timezone`.
- Jobs: none.

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
MRR is served on the merchant's subscription routes and read through its metrics routes, each with their permissions. Every query in `mrr.sql` names `merchant_id`; the cache keys by schema and merchant.

### 5. Contracts match the code
`billing.SubscriptionMRR`, `MRRRisk` and the subscription list's `scheduled_change_before`, `at_risk` and `risk_reason` are in `api/go.txt` and `api/openapi.json`; the tables are in `api/schema.txt`.

Checks:
- `go run ./scripts/contracts` shows no removed or changed line here.

### 6. Data lifecycle
State: `subscription_mrr`. Permanent: `mrr_movements`, `mrr_movement_lines`, `mrr_risk_cases`, `mrr_risk_episodes`; a scheduled movement is deleted only by a later settlement of its customer, while its date is ahead. None is partitioned. A merchant archive carries the MRR tables; a restore skips their triggers, and nothing derived from them needs proving.

Checks:
- Table comments carry the `Retention:` rule in `internal/retention/tables.go`.
- Each table is in `internal/merchantarchive/owned_tables.go`.

### 7. Work scales with activity
MRR is recorded by triggers as subscriptions change and settled at commit; nothing runs at a period's end. A read is one range scan of the facts up to its instant: a merchant's whole history, cached for a minute.

Checks:
- OR-J42-S30: every MRR measure reads its facts by index-only range scans bounded by their instant, once per statement; OR-J42-S09: a bulk import reads `subscription_mrr` in proportion to the book.

### 8. Abuse protection
Does not apply: MRR has no entry point of its own.

### 9. Destructive-action safeguards
Does not apply: MRR records what the subscriptions did; a prune or an undo moves it as a `correction`, and supersedes what it scheduled.

### 10. Consistent time
Movements are dated by the writer's `updated_at`, or the end a cancellation runs to, in UTC. A read groups them into the merchant's local days and months in its `accounting.timezone` (IANA, default UTC): month ends and daylight saving follow the zone, a change of zone needs no rebuild, and no SQL that writes MRR depends on a zone. A price on a calendar cadence divides by its months, never by days; any other window uses the average Gregorian month (730.485 hours).

Checks:
- OR-J42-S06 reads a past local day across a daylight-saving change; OR-J42-S30 every MRR measure in UTC and in a non-UTC zone.

## Tests

- OR-J42-S01–S09: MRR movements once per change and normalised per currency; a cancellation at period end shown and cleared; scheduled contraction applied or reversed; a Stripe portal cancel and resume; a CCBill upgrade and an import; MRR at a past day; an engine upgrade; a prune and its undo; a bulk import.
- OR-J42-S38–S41: an imported subscription new at its start and churned at its end; two rails imported in two passes as one timeline; imported months equal to the same history booked live; an upgrade turning openings into history. OR-J38-S14–S18: the PSP imports' ended subscriptions, never-paid ones and inferred NMI schedules.
- OR-J42-S11–S15, S17–S19, S21, S23: at-risk MRR as past due (recovered, then churned), needing a payment method then past due, a card expiring and delinquency read from state, unverified, a case closed churned at a cancellation's end and reopened by a resume, a new default card, an upgrade, a restore, and a card cleared while canceled.
- OR-J42-S30: every MRR measure reads its facts on indexes. OR-J42-S32, S33: MRR leaving a product, a currency and a customer; a refund that revokes access.
- OR-J42-S34–S36: a cancellation at period end counts at its end with no job and a resume removes it; an add-on turns the churn into contraction; at-risk MRR follows a price change during a case.
- OR-J42-S37: migration 0013 keeps every MRR measure of a book the rollups recorded, including a product move and an equal-MRR upgrade; a write after it classifies from summed movements written out of date order, and writes again an end that came before a change dated earlier.
