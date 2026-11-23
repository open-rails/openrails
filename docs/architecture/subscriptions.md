# Subscriptions

Recurring agreements between a customer and one price: start, renewal, cancel
and resume, plan and seat changes, and price migrations. Retries are in
[dunning.md](dunning.md); the starting order is in [checkout.md](checkout.md).

## Concepts

- **Owner** (`subscriptions.OwnerOf`): `engine` (OpenRails charges and duns),
  `nmi_schedule` (NMI charges, OpenRails duns), `provider` (Stripe Billing or
  CCBill charge and retry; OpenRails mirrors).
- **Status** (`lifecycle.Status`): `pending`, `active`, `past_due`,
  `awaiting_method`, `unverified`, `canceled`; the middle four are live.
- **Paid agreement**: the paid terms of the current period; a renewal's only
  source of price, seats and cadence.
- **Scheduled change** (`scheduled_changes`): the one change awaiting renewal.
- **Price migration** (`price_migrations`): moves a price's subscribers to
  another price at each one's first renewal on or after `effective_at`.
- **Engine renewal grace** (`EngineRenewalGrace`): access past the boundary
  while the renewal decides, `min(24h, max(2h, period/10))`.

## How it works

**State machine.** `lifecycle.Apply(snapshot, event)` is pure: next snapshot and
effects, no clock, database or provider. Callers lock the row
(`GetSubscriptionByIDForUpdate`), call `subscriptions.Transition`, run
`ApplyEffects` (grant, end access, dunning, provider cancel, notify) and persist
in one transaction. Only a payment extends the period; only a decline or a
proven NMI skip opens dunning; a clock alone reaches only `unverified`. Creation
sets the first state directly (`createMembershipCore`); a trigger logs each
status change to `subscription_status_transitions`.

**Enrollment.** A customer buys a recurring price as an order line
(`POST /v1/me/orders`); once paid, `orders.Service.fulfil` calls
`CreateMembershipTx` with frozen `InitialMembershipTerms` and records a
recurring `mandates` row; it is engine-owned unless the provider enrolled it
(`charge.Enrolled`). `subscriptions_customer_id_tier_group_key` and its product
twin allow one live membership per tier group. A calendar price fixes its anchor
at the first payment ([billing intervals](../billing-intervals.md)); trial
prices are refused (`order_line_unavailable`).

**Engine renewal.** The hourly billing pass's `openrails.dunning` scan
(`ListDueDunningMerchants`, then `ListDueDunningSubscriptions` in each
`RunInMerchantScope`) calls `MoneyService.AdmitDueSubscriptionCollection`. Under
customer and subscription locks it replays an unresolved
`subscription_collection`; refuses during an upgrade
(`ErrRenewalHeldByUpgrade`), under `engine_admission_hold`, before
`EngineCollectionDue` or for a wholly elapsed period
(`SelectEngineRenewalPeriod`); takes terms from the one paid agreement at the
boundary (`PrepareEngineRenewalTerms`), never the catalog, with any due
scheduled change; opens the renewal order (`orders.OpenRenewal`); and enqueues
the operation keyed by `SubscriptionCollectionKey`. The
`openrails.provider_operation` job charges; payment, order and `RenewMembership`
commit with its terminal outcome (`CommitsTerminalOutcome`). An unchargeable
card leaves the order to the customer (`awaiting_method`). A renewal past grace
with no outcome is held, access kept by default (`access_while_renewal_held`),
and reported as `life.renewal.held`.

**Provider-owned renewal.** Webhooks wake `openrails.subscription_converge`,
which applies the provider's state. `openrails.rebill_watch` reads NMI schedules
past `NMIRebillDeadline` and collects a proven skip (`CollectSkippedRenewal`).
The due pass moves a provider-billed renewal still unpaid 48 hours
(`reconcile.PeriodGrace`) past its period to `unverified` and reads it by id
(`AskProviderOverdueRenewal`). Solana renewals are the cranker's pulls
([payment providers](payment-providers.md)).

**Cancel and resume.** Customers cancel at period end with a 4–500 character
reason (`Service.CancelSubscriptionForCustomer`); staff use
`AdminSubscriptionService.CancelSubscription` (`revoke_access`,
`account_deletion`). Access runs to the boundary unless immediate or a
chargeback; any provider stop is an operation. An NMI delete waits until 48
hours before the rebill (`NMIDeferredDeleteAt`), a system cancel 24 hours, never
later than 48 hours before the rebill (`SystemDeleteCoolingOff`); one during an
unresolved renewal is refused (`payment_in_progress`). `Resumable` allows
undoing a non-merchant cancel (engine, Solana: the customer's own) on a
reversible rail in the paid period.

**Plan and seat changes.** `PurchaseService.ChangeSubscription` routes by owner:

- Engine upgrade, now: new price × seats − unused credit (`QuoteModelBUpgrade`),
  an `initial_membership` successor with a new period.
- Engine seat increase, now: added seats to period end (`QuoteSeatIncrease`).
- Engine seat decrease, downgrade or staff change: a `scheduled_changes` row the
  renewal applies, charging and refunding nothing.
- Other rails: Stripe (`stripe_tier_change`) and NMI schedules (`nmi_upgrade`),
  with no staff upgrade; CCBill and Solana change only by the customer's own step.
- Seats exist only on engine NMI and Stripe rows; calendar prices refuse
  changes. One unresolved change owns a subscription: its `Idempotency-Key`
  replays it; an unresolved provider outcome answers 202 `processing`, and one
  awaiting the customer `requires_action` (`next_action`), which `/confirm` finishes.

**Price migrations.** `PriceMigrationService.Create` reads the cohort
(`ListMigratableSubscriptionsByPriceIDs`), classifies it, admits the command and
records one `scheduled_changes` move each. A move is skipped (change pending,
other currency, a rise inside `reprice_notice_window_days` without
`acknowledge_short_notice`) or blocked: CCBill, Solana or an unreachable NMI
schedule (`rail_requires_user_action`), an engine calendar price, an NMI cycle or
sub-cent mismatch, or an unmapped Stripe price. Engine moves apply at renewal;
Stripe and NMI moves push (`price_migration_push`). Cancel lists Stripe
schedules to release (`RailReleaseRequired`); an admitted renewal keeps its terms.

## Interfaces

- Go: one `Client` method per staff and migration route below
  (`ListSubscriptions`, `ChangeSubscription`, `CreatePriceMigration`, …).
- `/v1/admin/subscriptions`: `GET`, `GET /{id}`, `GET /{id}/history`,
  `GET /{id}/change/options`, `PUT /{id}/payment-method`, `POST` on
  `/{id}/cancel`, `/{id}/resume`, `/{id}/change`, `/{id}/change/preview`.
- `/v1/catalog/price-migrations`: `POST`, `POST /preview`, `GET`, `GET /{id}`,
  `POST /{id}/cancel`.
- `/v1/me/subscriptions`: `GET`, `GET /{id}`, `GET /{id}/change/options`,
  `PUT /{id}/payment-method`, `POST` on `/{id}/cancel`, `/{id}/resume`,
  `/{id}/change`, `/{id}/confirm`, `/{id}/change/preview`, `/{id}/retry-now`.
- Events ([events](../events.md)): `premium_started`, `premium_renewed`,
  `premium_ended`, `subscription_canceled`, `subscription_changed`,
  `subscription_reprice_scheduled`, `subscription_plan_change_scheduled`,
  `payment_method_failed`, `payment_method_update_required`.
- Config: `dunning_policy`, `engine_admission_hold`, `user_action_rate_limit`,
  `destructive_action_limits`, `reprice_notice_window_days`.

## Guarantees

### 1. Exactly-once provider calls

Each provider write is a `provider_intents` row committed with its job before it
is sent: `subscription_collection`, `initial_membership`, `nmi_upgrade`,
`stripe_tier_change`, `price_migration_push`, a cancel's NMI delete or Stripe or
CCBill cancel. Keys are deterministic; an ambiguous outcome is verified, never
resent. Exception: the Stripe resume is a direct, idempotent write.

Checks:
- `provider_intents_open_subscription_collection_key` and
  `provider_intents_subscription_collection_slot_key` in `0001_schema.up.sql`.
- `DecodeSubscriptionCollectionPayload` refuses contradicting terms.
- OR-J13-S02, OR-J14-S02: one charge; OR-J14-S05: survives death before ack.

### 2. Agreement with providers

Webhooks only wake a converge that reads the provider. A decline for a paid or
canceled period is ignored, a payment on a row canceled by decision goes to
refund review (`lifecycle.ErrTerminal`), one on a lapsed (`expired`) row restores
it, and missing facts lead only to `unverified`. A Stripe
price move settles from Stripe's record (`ProviderMovedPrice`).

Checks:
- `lifecycle.Apply` gives no effects for a stale `RenewalPaid` or for
  `RenewalDeclined` on a canceled row.
- `RenewalOverdue` and `DunningStale` reach only `unverified`.
- OR-J17-S04 (late callbacks); OR-J15-S18, OR-J15-S19 (ends only on not-found).

### 3. Exact money

Terms are integer micros; `RenewalTerms.Validate` requires an exact rail-minor
amount (`NativeToRailMinorExact`), and `DecodeSubscriptionCollectionPayload`
requires `AmountMinor` to equal it; `SeatAmount` refuses overflow. Proration
rounds once to a rail minor unit for the customer, in one currency; migrations
skip other currencies and block NMI amounts finer than cents.

Checks:
- `prorateCredit` and `QuoteSeatIncrease` in `proration.go`.
- `critical/renewal_test.go` pins a $10 renewal as Stripe `amount` 1000;
  `critical/subscription_change_contract_test.go` pins 1500 and NMI `"20.00"`.
- OR-J14-S04, OR-J19-S04: a later price edit leaves an admitted charge as is.

### 4. Access control and tenant isolation

Staff routes need `merchant:customer:read` or `merchant:customer:manage` by
`Level`, migrations `merchant:catalog:read` or `merchant:catalog:manage`, or a
`root:` twin. Customer routes refuse another customer's row (`ownSubscription`).
Queries name `merchant_id`; the due pass uses `RunInMerchantScope`.

Checks:
- `Route.Needs` in `internal/http/routes/permissions.go`.
- `ownSubscription` answers `subscription_not_found` for another's id.
- OR-J16-S05: a failing pass holds back no other merchant's renewal.

### 5. Contracts match the code

Routes are declared once (`internal/http/routes/subscriptions.go`); the
in-process `Client` reaches the same handlers, so both answer alike. Snapshots:
`api/go.txt`, `api/openapi.json`, `api/schema.txt`; statuses and cancel types
are CHECK constraints.

Checks:
- `go run ./scripts/contracts` shows no removed or changed line here.
- Each staff and migration route has one `Client` method (`remote_recovery.go`,
  `price_migrations.go`); `/v1/me` routes have none.

### 6. Migrations and retention

State: `subscriptions`, `subscription_verifications`. Permanent:
`scheduled_changes`, `price_migrations`, `mandates`, `provider_schedules`. Rows
(25 months): `subscription_status_transitions`, `subscription_billing_changes`,
`rebill_cycles`. `0002_calendar_billing_intervals` changes the calendar cadence
check on `prices` and adds it to `order_lines`; subscriptions soft-delete
(`deleted_at`); no partitioned table is read.

Checks:
- Table comments carry the `Retention:` rule in `internal/retention/tables.go`.
- Each table is in `internal/merchantarchive/owned_tables.go`.

### 7. Work scales with activity

The due pass reads partial indexes on due instants ([dunning](dunning.md)),
pages 500 merchants and defers an unchanged row (`due_recheck_at`). Operations
wake at `next_attempt_at`; webhooks coalesce. A migration's cohort is one
unpaged read, bounded over HTTP by the destructive ceiling.

Checks:
- `ListDueDunningMerchants`, `ListDueDunningSubscriptions` read due instants.
- OR-J14-S01: none before the boundary; OR-J14-S70: misses read without a sweep.

### 8. Abuse protection

`/v1/me/subscriptions` writes share the `subscribe` bucket (20 per minute, 64
KiB, captcha when configured; [limits](../limits.md)). Customer cancels spend
the merchant-wide budget (`destructive.NewUserActionLimiter`). `retry-now` passes
the card-testing ledger; `AdminOperationOffChannel` caps staff changes.

Checks:
- `ClassifyBucket` maps POST/PUT/DELETE `/v1/me/subscriptions*` there.
- `CancelSubscriptionForCustomer` calls `Reserve` before any change.

### 9. Destructive-action safeguards

Staff cancels run in `destructive.RunLocal` (`CancellationCommand`, keyed by
`lifecycle_rev`). A migration is a `destructive.CatalogChange`, a unit per
subscription and archived source; canceling it is free. A cancel needing a
provider stop while destructive actions are disarmed is refused
`provider_cancel_held`; an account deletion cancels locally and waits.
`ExpireAwaitingMethod` holds under `mode=limited` or a closed gate.

Checks:
- `RequireProviderCancelArmed` refuses a disarmed provider stop with
  `provider_cancel_held`, except an account deletion; `admitMigration` admits one
  unit per moved subscription and archived source.
- OR-J17-S03: a held provider delete survives process death; OR-J34-S06: a held
  webhook cancel keeps access.

## Tests

Journeys:
- OR-J13-S01–S04: enrollment, concurrent signup, imported terms, trials.
- OR-J14-S01–S07, OR-J14-S70: boundary, concurrency, restart, frozen terms,
  process death, one agreement, calendar anchor, missed renewal.
- OR-J17-S01–S06: cancel and resume, renewal custody, held delete, callbacks.
- OR-J18-S01–S06: preview, stale preview, customer action, fences, seats.
- OR-J19-S01–S07: eligibility, push retry, cancel, frozen terms, notice, seats.
- Related: OR-J04-S42, OR-J04-S70, OR-J34-S05/S06, OR-J27-S08, OR-J37-S64–S68.

Other tests: none; `internal/lifecycle`, `internal/modules/subscriptions` and
`internal/modules/payment` have no package tests.

Gaps:
- No table test of `lifecycle.Apply`; rare transitions run only in scenarios.
- Nothing checks the `Retention:` table comments against `internal/retention/tables.go`.
- OR-J18-S03 checks a seat increase's payment and card, not its amount.
- No scenario migrates a cohort large enough to show the unpaged read.
- No scenario covers a local failure after Stripe accepted a resume.
