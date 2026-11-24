# Subscriptions

Recurring agreements between a customer and one price: start, renewal, cancel and resume, plan and seat changes, and applying a price migration's move at renewal. Retries are in [dunning.md](dunning.md); the starting order is in [orders.md](orders.md); price migrations themselves are in [catalog.md](catalog.md#moving-subscribers-to-other-prices-and-products).

## Concepts

- **Owner** (`subscriptions.OwnerOf`): `engine` (OpenRails charges and duns),
  `nmi_schedule` (NMI charges, OpenRails duns), `provider` (Stripe Billing or
  CCBill charge and retry; OpenRails mirrors).
- **Status** (`lifecycle.Status`): `pending`, `active`, `past_due`,
  `awaiting_method`, `unverified`, `canceled`; the middle four are live.
- **Paid agreement**: the paid terms of the current period; a renewal's only
  source of price, seats and cadence.
- **Scheduled change** (`scheduled_changes`): the one change awaiting renewal: a plan or seat change, or a price migration's move (`source` `migration`).
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
at the first payment ([billing intervals](../client.md#catalog)); trial
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

**Cancel and resume.** Customers cancel at period end with a 4–500 character reason (`Service.CancelSubscriptionForCustomer`); staff use `AdminSubscriptionService.CancelSubscription` (`revoke_access`, `account_deletion`). Access runs to the boundary unless immediate or a chargeback; any provider stop is an operation. An NMI delete waits until 48 hours before the rebill (`NMIDeferredDeleteAt`), a system cancel 24 hours, never later than 48 hours before the rebill (`SystemDeleteCoolingOff`); one during an unresolved renewal is refused (`payment_in_progress`). `Resumable` allows undoing a non-merchant cancel (engine, Solana: the customer's own; never a provider's or an abandoned start) on a reversible rail in the paid period; a Stripe resume is a `stripe_resume_subscription` operation committed with it.

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

**Applying a price migration's move.** A [price migration](catalog.md#moving-subscribers-to-other-prices-and-products) leaves each subscriber a scheduled move, which applies by owner:

- Engine: once due (`effective_at` reached, `IsDue`), `PrepareRenewalTerms` bills the target's amount, seats and cadence from the renewal's boundary, and `applyRenewalTerms` marks the move applied (`ApplyChange`) as the renewal commits.
- Stripe: Stripe switches the price at period end; converge adopts it (`AdoptProviderPrice`), and `ProviderMovedPrice` applies the move.
- NMI schedule: the push lands in the last period before `effective_at`, and the subscription moves at once (`applyNow`), since NMI bills the new amount from its next rebill.
- A move to another product grants that product's access from the next renewal; a Stripe move swaps it as it is adopted.
- While an accepted renewal owns the terms, a move can be neither scheduled nor canceled (`RefuseOwnedRebillTerms`, `rebill_terms_committed`), so that renewal bills what it admitted. Each resolution drops the move's waiting push and settles its migration if it was the last (`resolved`).

## Interfaces

- Go: one `Client` method per staff route below (`ListSubscriptions`, `ChangeSubscription`, …).
- `/v1/admin/subscriptions`: `GET`, `GET /{id}`, `GET /{id}/history`,
  `GET /{id}/change/options`, `PUT /{id}/payment-method`, `POST` on
  `/{id}/cancel`, `/{id}/resume`, `/{id}/change`, `/{id}/change/preview`.
- `/v1/me/subscriptions`: `GET`, `GET /{id}`, `GET /{id}/change/options`,
  `PUT /{id}/payment-method`, `POST` on `/{id}/cancel`, `/{id}/resume`,
  `/{id}/change`, `/{id}/confirm`, `/{id}/change/preview`, `/{id}/retry-now`.
- Events ([events](../client.md#events)): `premium_started`, `premium_renewed`, `premium_ended`, `subscription_canceled`, `subscription_changed`, `payment_method_failed`, `payment_method_update_required`.
- Config: `dunning_policy`, `engine_admission_hold`, `user_action_rate_limit`, `destructive_action_limits`.

## Guarantees

### 1. Exactly-once provider calls

Each provider write is a `provider_intents` row committed with its job before it is sent: `subscription_collection`, `initial_membership`, `nmi_upgrade`, `stripe_tier_change`, a cancel's NMI delete or Stripe or CCBill cancel, and a Stripe resume (`stripe_resume_subscription`, committed with the local resume); a migration's push is the [catalog's](catalog.md#1-exactly-once-provider-calls). Keys are deterministic; an ambiguous outcome is verified, never resent.

Checks:
- `provider_intents_open_subscription_collection_key` and
  `provider_intents_subscription_collection_slot_key` in `0001_schema.up.sql`.
- `DecodeSubscriptionCollectionPayload` refuses contradicting terms.
- OR-J13-S02, OR-J14-S02: one charge; OR-J14-S05: survives death before ack.

### 2. Agreement with providers

Webhooks only wake a converge that reads the provider. A decline for a paid or canceled period is ignored, a payment on a row canceled by decision goes to refund review (`lifecycle.ErrTerminal`), one on a lapsed (`expired`) row restores it, and missing facts lead only to `unverified`. An end the provider confirmed is stored as `provider` and one whose first payment never completed as `abandoned`, so read back they stay decided: neither a payment nor a resume reopens them. CCBill's own cancel and expiration are lapses, whether its webhook or a pull reports them; its reactivation or next renewal restores them. A Stripe price move settles from Stripe's record (`ProviderMovedPrice`).

Checks:
- `lifecycle.Apply` gives no effects for a stale `RenewalPaid` or for
  `RenewalDeclined` on a canceled row.
- `RenewalOverdue` and `DunningStale` reach only `unverified`.
- OR-J17-S04 (late callbacks); OR-J15-S18, OR-J15-S19 (ends only on not-found); OR-J38-S75 (a provider-confirmed end is not reopened by a later renewal); OR-J38-S76, S77 (CCBill's cancels are lapses).

### 3. Exact money

Terms are integer micros; `RenewalTerms.Validate` requires an exact rail-minor amount (`NativeToRailMinorExact`), and `DecodeSubscriptionCollectionPayload` requires `AmountMinor` to equal it; `SeatAmount` refuses overflow. Proration rounds once to a rail minor unit for the customer, in one currency.

Checks:
- `prorateCredit` and `QuoteSeatIncrease` in `proration.go`.
- `critical/renewal_test.go` pins a $10 renewal as Stripe `amount` 1000;
  `critical/subscription_change_contract_test.go` pins 1500 and NMI `"20.00"`.
- OR-J14-S04, OR-J19-S04: a later price edit leaves an admitted charge as is.

### 4. Access control and tenant isolation

Staff routes need `merchant:customer:read` or `merchant:customer:manage` by `Level`, or a `root:` twin. Customer routes refuse another customer's row (`ownSubscription`). Queries name `merchant_id`; the due pass uses `RunInMerchantScope`.

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
- Each staff route has one generated `Client` method (`client_gen.go`); `/v1/me` routes have none.

### 6. Data lifecycle

State: `subscriptions`, `subscription_verifications`. Permanent: `scheduled_changes`, `mandates`, `provider_schedules`. Rows (25 months): `subscription_status_transitions`, `subscription_billing_changes`, `rebill_cycles`. Subscriptions soft-delete (`deleted_at`); no partitioned table is read.

Checks:
- Table comments carry the `Retention:` rule in `internal/retention/tables.go`.
- Each table is in `internal/merchantarchive/owned_tables.go`.

### 7. Work scales with activity

The due pass reads partial indexes on due instants ([dunning](dunning.md)), pages 500 merchants and defers an unchanged row (`due_recheck_at`). Operations wake at `next_attempt_at`; webhooks coalesce. A due move rides the renewal that applies it.

Checks:
- `ListDueDunningMerchants`, `ListDueDunningSubscriptions` read due instants.
- OR-J14-S01: none before the boundary; OR-J14-S70: misses read without a sweep.

### 8. Abuse protection

`/v1/me/subscriptions` writes share the `subscribe` bucket (20 per minute, 64
KiB, captcha when configured; [limits](../standalone.md#abuse-limits)). Customer cancels spend
the merchant-wide budget (`destructive.NewUserActionLimiter`). `retry-now` passes
the card-testing ledger; `AdminOperationOffChannel` caps staff changes.

Checks:
- `ClassifyBucket` maps POST/PUT/DELETE `/v1/me/subscriptions*` there.
- `CancelSubscriptionForCustomer` calls `Reserve` before any change.

### 9. Destructive-action safeguards

Staff cancels run in `destructive.RunLocal` (`CancellationCommand`, keyed by `lifecycle_rev`). A cancel needing a provider stop while destructive actions are disarmed is refused `provider_cancel_held`; an account deletion cancels locally and waits. `ExpireAwaitingMethod` holds under `mode=limited` or a closed gate. A migration's admission is the [catalog's](catalog.md#9-destructive-action-safeguards).

Checks:
- `RequireProviderCancelArmed` refuses a disarmed provider stop with `provider_cancel_held`, except an account deletion.
- OR-J17-S03: a held provider delete survives process death; OR-J34-S06: a held
  webhook cancel keeps access.

## Tests

Journeys:
- OR-J13-S01–S04: enrollment, concurrent signup, imported terms, trials.
- OR-J14-S01–S07, OR-J14-S70: boundary, concurrency, restart, frozen terms,
  process death, one agreement, calendar anchor, missed renewal.
- OR-J17-S01–S07: cancel and resume (a Stripe resume a `stripe_resume_subscription` operation), renewal custody, held delete, callbacks.
- OR-J18-S01–S06: preview, stale preview, customer action, fences, seats.
- OR-J19-S01, OR-J19-S04: a move applies at the paid boundary, never before; an admitted renewal keeps its terms (the rest of J19 is the [catalog's](catalog.md#tests)).
- Related: OR-J04-S42, OR-J04-S70, OR-J34-S05/S06, OR-J27-S08, OR-J37-S64–S68.

Other tests: none; `internal/lifecycle`, `internal/modules/subscriptions` and
`internal/modules/payment` have no package tests.

Gaps:
- No table test of `lifecycle.Apply`; rare transitions run only in scenarios.
- Nothing checks the `Retention:` table comments against `internal/retention/tables.go`.
- OR-J18-S03 checks a seat increase's payment and card, not its amount.
