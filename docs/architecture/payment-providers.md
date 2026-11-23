# Payment providers

How OpenRails writes to NMI, Stripe, CCBill and Solana and stays in agreement
with them. Every provider write is a durable operation recorded before it is
sent; every callback is verified, deduplicated and, where the provider can be
read, treated as a reason to read it. Checkout, dunning and refunds decide what
to charge; this system decides whether a write may reach the provider and what
the provider's answer proves.

## Concepts

- **Rail**: a gateway kind, `billing.Rail`; `rails.Descriptor` (`internal/modules/payments/rails/registry.go`) declares what each does.
- **PSP**: one merchant account on a rail, a `psps` row; its identity is `(rail, live, account_id)`.
- **Operation** (intent): one `provider_intents` row: frozen terms, an idempotency key, a status and a lease.
- **Unknown**: `unknown_needs_verify`, an operation that may have reached the provider; only provider evidence settles it.
- **Write posture**: `full`, `limited` (no system-origin writes) or `readonly`.
- **Provider posture**: a PSP credential's proven environment; any other verdict disarms it.
- **Wake job**: a debounced River job that reads one provider object after a callback.
- **Finding**: a `reconciliation_findings` row; a `pull.*` finding carries a proposed change (a proposal).
- **Park**: a live provider subscription converge cannot land (`parked_provider_subscriptions`).

## How it works

### Writes: the provider-write ledger

Each write type is a `Handler` (`Execute`, `Verify`, `CheckRelevance`,
`Backoff`) in `internal/intents`.

1. `Store.Enqueue` (or `Runner.EnqueueAndExecute`, which runs the first attempt
   inline) records the row: frozen `payload`, its PSP (`psp_id`, a composite key
   to `psps` with `rail`), origin, and an `idempotency_key` unique per merchant.
   Its `openrails.provider_operation` wake commits in the same transaction.
2. The `Runner` claims it (`in_flight`, a lease, `attempts` bumped as a fencing
   token) and checks the handler, `CheckRelevance`, `GateExecution`, the
   destructive switch and the `VolumeBreaker`. A refusal parks it without
   spending an attempt, retried after `ParkRetryInterval` (5 minutes).
3. It appends an `attempting` row to `provider_mutation_logs` and calls
   `Execute`. A charge first calls `Store.RequireClaim` on its own connection:
   the claim must still hold a lease beyond `ProviderCallHold` (30 s).
4. The outcome is written on a detached context (`LedgerWriteContext`):
   `succeeded`, `failed_retryable` (provably not executed), `unknown_needs_verify`,
   `failed_terminal`, or parked. A lapsed lease becomes unknown, never a
   re-execution (`Store.PrepareDispatch`). Unknown work runs `Verify`, reads only.
5. What reads cannot settle waits for `openrails intents resolve`: `--receipt`
   is read back and matched to the frozen terms; `--not-executed` needs the
   type's own non-execution proof.

Per rail ([provider uncertainty](../provider-uncertainty.md) has the full rules):

- **NMI** has no idempotency key. Local refusals return `nmi.ErrNotDispatched`;
  any error after sending is ambiguous. A charge settles only from the exact
  receipt under its order id, which every attempt for one period shares; an
  empty search proves nothing, and NMI is never resent
  (`Store.ArmLostSubmissionResend` refuses any rail but Stripe).
- **Stripe** requests carry an `Idempotency-Key` rooted in the operation id
  (`stripeapi.SetIdempotencyKey`). A lost renewal may replay it after
  `LostSubmissionSettle` (5 minutes), at most `MaxLostSubmissionResends` (2)
  times, inside Stripe's 24-hour key retention less margins.
- **CCBill** writes are DataLink cancels (`ccbill_cancel_subscription`), status
  read first; refunds are refused (`refund_unsupported`).
- **Solana** pulls (`solana_pull`) record the signature and its blockhash's last
  valid height before sending, and are re-signed only once the chain proves the
  pull never landed.

Money-moving types include `nmi_sale`, `subscription_collection`,
`manual_rebill`, `order_collection`, `stripe_tier_change`, `nmi_refund` and
`solana_pull`. Provider-run renewals are orders with `collection_owner`
`nmi_schedule` or `provider`, matched to accepted schedule versions
([provider-managed orders](../provider-managed-orders.md)).

### Callbacks

- One route, `POST /v1/webhooks/{rail}/{account_id}` (`handlers.Webhook`); the
  path's account must equal the payload's. `routeSignedWebhook` accepts a
  delivery only when exactly one PSP on that account verifies it (Stripe's
  `livemode` breaks a live/test tie).
- NMI and Stripe are HMAC-signed with a 5-minute window
  (`NMISignatureTolerance`); a thin Stripe event is read back from Stripe first.
  CCBill signs nothing: its source address (`webhookauth.CCBillIPAllowed`) and
  the payload's account authenticate it.
- A rotated secret verifies for `webhook_secret_overlap` (default 24 h);
  `retire_webhook_overlap` ends it at once. A delivery is claimed in
  `idempotency_keys` and recorded in `webhook_events`, in the handler's
  transaction where possible (`MarkWebhookProcessedInTx`).
- Stripe and NMI subscription events only queue `openrails.subscription_converge`,
  which fetches the subscription; Stripe catalog events queue
  `openrails.provider_object_observe`. CCBill has no read API, so its payload
  goes through the lifecycle machine, one event at a time on the locked row.

### Staying in sync

- **Wake jobs** (`wakejobs.Enqueue`): unique per object until complete, debounced
  5 s; a wake landing on a running job chains a follow-up, since that job may
  have read before the cause.
- **Convergence**: `reconcile.Decide` is the one pure decision table. Webhook
  fetches, probes, the rebill watch, pulls and imports each give it an
  `EvidenceBundle`; only `ApplyDecision` changes a subscription.
- **Missed callbacks**: `openrails.rebill_watch` reads each NMI schedule past
  `NMIRebillDeadline` (24 h) with no attempt; a found charge is recorded, a
  proven skip collected, a reversed one never recharged (`life.rebill.missed`).
  A period lapsing without evidence makes a subscription `unverified`, access
  kept; `trg_subscriptions_track_unverified` queues it in
  `subscription_verifications`, and the `Verifier` reads queued ids, 50 per
  request. `openrails.charge_attributions`, `openrails.nmi_history` and
  `openrails provider-refresh` (from `psp_refresh_watermarks`) read the rest;
  `life.webhooks.silent` fires when webhooks stop but pulls find charges.
- **Proposals**: only `proposals.Executor` applies a `pull.*` finding's `change`,
  one approval run per approve call, reversed by `openrails undo-run`.
- **Parking**: an unmapped price, an unlinked customer or a customer already
  holding the product parks the subscription (`price_unmapped`,
  `customer_unlinked`, `product_held`) with a finding. Fixing the cause wakes
  converge in the same transaction (`parking.Wake`); a trigger closes the park
  when the subscription lands by any path. A pulling or staging import owns it.
- **Customer convergence**: `openrails.customer_converge` commits with the
  mutation that changed the customer (`internal/convergejob`).

### Posture

- **Write posture** (`writeposture.View`): the lower of `provider_write_mode`
  and `merchant_write_posture`; `readonly` in a database whose book was never
  armed (`openrails book arm`). `GateExecution` runs user, admin and provider
  origins under `limited`, system origin only under `full`, nothing under
  `readonly`. The transports refuse writes again in `readonly`
  (`ErrProviderReadOnly` in `stripeapi` and the NMI client).
- **Provider posture** (`providerposture.Registry`): a verdict keyed by rail,
  merchant, PSP, account, endpoint and credential fingerprint. Only `Live` or
  `Simulated`, as the environment expects, arms; it is proven again at the first
  write after `VerdictTTL` (one hour). Otherwise writes fail with `ErrDisarmed`
  before any bytes are sent; reads work. A sandbox CCBill PSP never arms.
- A second live PSP with another's key (`psps.credential_fingerprint`) stays
  disarmed (`consistency.duplicate_gateway_account`). Objects OpenRails creates
  at NMI and Stripe carry the book id (`internal/booktag`).

Other tables: `provider_bills`, `provider_charge_attributions`, `webhook_health`,
`webhook_health_daily`, `nmi_history_months`, `nmi_bulk_checkpoints`,
`solana_subscriptions`, `solana_pay_references`, `solana_pay_receipts`.

## Interfaces

- **Go** (`*Client`): `ListPSPs`, `GetPSP`, `CreatePSP`, `UpdatePSP`,
  `PreviewPSPRouting`, `ListFindings`, `GetFinding`, `ResolveFinding`,
  `ListProposals`, `ApproveProposals`, `DismissProposals`, `ListParkedSubscriptions`.
- **HTTP**: `/v1/merchant/psps`, `/v1/merchant/psps/{id}`,
  `POST /v1/merchant/psps/routing-preview`, `/v1/merchant/findings`,
  `/v1/merchant/findings/{id}`, `POST /v1/merchant/findings/{id}/resolve`,
  `GET /v1/merchant/proposals`, `POST /v1/merchant/proposals/approve`,
  `POST /v1/merchant/proposals/dismiss`, `GET /v1/merchant/parked-subscriptions`,
  `POST /v1/webhooks/{rail}/{account_id}`.
- **CLI**: `openrails intents resolve`, `openrails provider-refresh`,
  `openrails book arm`, `openrails merchant hold`, `openrails undo-run`,
  `openrails solana-pay resolve`.
- **Configuration**: `provider_write_mode`, `webhook_secret_overlap`,
  `ccbill_webhook_ip_allowlist`, `solana.crankers`, each PSP's `environment`;
  probes `openrails_psp_posture`, `openrails_solana_crankers`. Docs:
  [rails](../rails/README.md), [PSP environments](../psp-environments.md).

## Guarantees

### 1. Exactly-once provider calls

This system is where the guarantee lives. A write is a `provider_intents` row,
unique on its idempotency key, committed with its wake. Every claim bumps
`attempts`, so a superseded executor's claim never matches, and
`Store.RequireClaim` re-reads it just before a charge. A lost lease becomes
unknown, and unknown work only reads. Partial unique indexes allow one open
operation per obligation (`provider_intents_obligation_unreleased_key`,
`provider_intents_subscription_id_tier_change_subscription_key`).

Checks:
- `Runner.executeOne` logs `attempting` before `Execute`; `Runner.record` writes the outcome on `LedgerWriteContext`.
- `lostSubmission` (`internal/modules/money/lost_submission.go`) answers an unseen NMI charge with `life.submission.unresolved`, never a resend.
- OR-J11-S01 loses an accepted debit's answer and asserts one debit after recovery (`p.effects`); OR-J11-S04 refuses a replacement charge or another PSP meanwhile.
- OR-J29-S01 (crash before submission) and OR-J29-S04 (two workers, expired lease) count `Submitted("/v1/payment_intents")`.

### 2. Agreement with providers

Stripe and NMI callbacks only wake a fetch, so a late, repeated or reordered
callback cannot write stale state. Missing callbacks are caught by reads driven
by evidence (the rebill watch, verification, attributions, refreshes). A
subscription ends only on the provider's own terminal answer (Stripe
`resource_missing`, NMI `E_NOT_FOUND`); no evidence keeps it `unverified` with
access, and what converge cannot land parks for a person.

Checks:
- `markSubscriptionDirty` (`internal/modules/webhooks/converge.go`) only enqueues; state changes go through `ApplyDecision`.
- OR-J28-S04 (duplicate, out-of-order) and OR-J28-S05 (callback during a fetch).
- OR-J15-S01, S03, S04: a missing callback repaired from the Query API, a skipped cycle collected only after proven absence, a reversed charge never recollected.
- OR-J15-S18, S19: any other 404 fails the converge, which retries with access intact.

### 3. Exact money

Amounts are `int64` micros, converted at one boundary each way:
`moneyutil.NativeToRailMinor` out (rounding up, so a charge never under-covers),
`DecimalToRailMinor` and `RailMinorToNative` in. NMI gets a major-unit decimal
from `nmi.WireAmount`, no floats (4 JPY units are `4.00`); Stripe gets integer
minor units; CCBill gets no amount (the FlexForm holds it), and its decimals are
read in the event's own currency (`ccbill.CurrencyCode`, no default). Solana
quotes fiat into token base units and refuses a cross-token payment
(`ValidateQuoteCurrency`).

Checks:
- OR-J09-S03: a 500000-micro USD sale reaches NMI as `0.50`; OR-J05-S01: 29.97 USD settles as 2997 Stripe cents.
- OR-J38-S42: NMI `1000.00` reads as 1000 JPY and `12.50` as 12.50 EUR; an inexact value is refused.
- OR-J20-S05 books each CCBill partial refund once; OR-J37-S01 refuses an underpaid Solana transfer.
- `WireAmount` calls `moneyutil.RequireFiatCurrency`: an unknown currency is refused, never USD.

### 4. Access control and tenant isolation

`Store.Enqueue` refuses an operation whose merchant is not the request's. The
ledger, log, dedup and park tables key on `merchant_id` and reach `psps` by
composite key. A webhook takes its merchant only from the PSP whose secret
verified it, then pins that merchant's connection (`pinWebhookMerchantConn`).
`nmi.NewAccountClient`, the only NMI constructor, binds merchant, PSP, account
and environment. Staff routes are `MerchantConfig` with `LevelRead` or
`LevelUpdate`; approving and dismissing proposals is `Person: true`.

Checks:
- OR-J28-S02: a missing, wrong, tampered or foreign-account signature changes no order and wakes no `openrails.provider_operation`.
- OR-J28-S06, S07: another merchant's claim of an account is refused; live and test PSPs on one account get only events their own secret verifies.
- OR-J15-S06: an unverified subscription is read with its own PSP, and a sibling account's absence cancels nothing.

### 5. Stable contracts

Routes are declared once (`internal/http/routes/psps.go`, `ops.go`) and
snapshotted in `api/openapi.json`, `Client` methods in `api/go.txt`, tables in
`api/schema.txt`; `Engine.Merchant` and `NewRemoteMerchant` return the same
`*Client`. `stripeapi.APIVersion` pins the outbound `Stripe-Version` and the
registered endpoint alike.

Checks:
- `go run ./scripts/contracts` reports no change under `api/`; `intent_type` is free text, so a new operation type is additive.
- `stripeapi.guardTransport.RoundTrip` sets `Stripe-Version` on every request lacking it.

### 6. Migrations and retention

The tables are in `0001_schema.up.sql`; a change is a new numbered migration.
In `internal/retention/tables.go`, instruction-only operations
(`OutboxIntentTypes`) and `provider_mutation_logs` go after 25 months;
`webhook_events` and released parks after 90 days; resolved findings after 12
months; money-moving operations and provider bills, schedules and receipts are
permanent. None is partitioned.

Checks:
- Each table comment's `Retention:` sentence matches its `tables.go` rule (`State` tables carry none).
- `provider_intents_updated_at_idx`, `DeleteFinishedOutboxIntentsBefore` and `OutboxIntentTypes` list the same types; nothing checks it.
- OR-J29-S06: retention and export refuse to discard unresolved money; `immutable_provider_mutation_log_content` blocks log edits.

### 7. Work scales with activity

An operation's wake is queued by the write that leaves it live, at its due
time; `openrails.operation_wakes` only requeues operations an hour overdue, 500
at a time. Wake jobs coalesce per object. The `Verifier` reads queued ids; the
rebill watch reads only overdue NMI schedules without an attempt
(`ListOverdueRebills`). Provider posture is proven per credential set and at
the first write after an hour, never by sweep. Daily reads are per PSP.

Checks:
- `OperationWakes.Pass` uses `ListOverdueProviderOperations` past `OperationWakeGrace`; OR-J04-S60 requeues a lost wake.
- OR-J15-S07: a verification burst reads only flagged subscriptions; OR-J15-S72: a complete refresh queues no due work.

### 8. Abuse protection

Webhooks share the `webhook` rate-limit bucket (1200 a minute) and per-rail
body caps (CCBill 16 KiB, NMI 64 KiB, Stripe 256 KiB) before parsing. Unsigned,
mis-signed and stale deliveries are refused and counted in
`webhook_health_daily`. CCBill is accepted from its published ranges, or from
`ccbill_webhook_ip_allowlist` only for an account proven non-live. Customer
declines from charge operations feed the card-testing ledger
(`Runner.recordOrderCardDecline`).

Checks:
- `readLimitedWebhookBody` wraps every body in `http.MaxBytesReader` and answers 413.
- The public path passes `NMISignatureTolerance`; only the queued re-verification skips the window.
- `webhookauth.CCBillIPAllowed` refuses an allowlisted source when the account's money class is unknown; `internal/config` refuses a `/0` entry.

### 9. Destructive-action safeguards

Destructive types (`IsDestructiveIntentType`: subscription deletes, CCBill
cancels, card deletes) pass the kill switch (`destructive.Gate`, off by
default), the producer `RateCeiling` (`PerActorHourlyCeiling` 5,
`PerMerchantHourlyCeiling` 15) and the `VolumeBreaker`; a held one parks
unsent. Every convergence cancel passes `GateCancelPass` (the switch, then the
cancel budget); a held cancel leaves the row `unverified` with access.
Operations record `actor`, `subject` and `credential`; uncertainty never
triggers a compensating cancel or refund.

Checks:
- `Runner.executeOne` checks `AllowDestructive` and `Breaker.Check` before `Execute`.
- OR-J34-S06, S30: a held webhook cancel keeps access; the switch turned off mid-pull holds its retractions.
- `PlanUndoRun` refuses `NeverRollbackableTables` (including `provider_mutation_logs` and `webhook_events`); OR-J38-S71 spends the actor allowance once per subscription.

## Tests

Critical scenarios:

- J06: OR-J06-S01 to S05, S30, S31, S60 to S64 (`TestPSPControls`, `TestStripeWebhookDueWork`), S07 (`TestBackgroundJobs`), S08 (`TestPSPPostureExpiry`), S65 (`TestDueWork`).
- J10: OR-J10-S01, S03, S04 (`TestPaymentReplay`), S02 (`TestCheckoutBoundaries`), S05 (`TestOrderPaymentReceipts`).
- J11: OR-J11-S01, S02 (`TestProviderRecovery`), S04 (`TestUnresolvedRouting`).
- J15: OR-J15-S01 to S04 (`TestNMIRecovery`), S06, S07 (`TestVerificationBinding`), S08 to S21 (`TestProviderTrailing`), S30 (`TestBackgroundJobs`), S70, S72 (`TestDueWork`), S71 (`TestNMIRoster`).
- J28: OR-J28-S01 to S05, S08 (`TestCallbackContracts`), S06, S07 (`TestWebhookAccountClasses`).
- J29: OR-J29-S01, S04 (`TestProcessRecovery`), S02, S05, S06 (`TestUnresolvedMoneyContracts`), S07, S08, S40, S41 (`TestBackgroundJobs`), S37, S70 to S73 (`TestDueWork`).
- J37: OR-J37-S01 to S05, S63 (`TestSolanaContracts`), S07, S09, S60 to S62 (`TestSolanaPulls`), S64 to S68 (`TestSolanaPlanChanges`), S06 (`TestBackgroundJobs`), S10 (`TestFXRates`), S70 (`TestOrderExpiryClock`).
- Elsewhere: OR-J09-S05, S06 (lost and partial NMI sales), OR-J16-S04, OR-J20-S03, S70, OR-J26-S55, OR-J31-S03, S04 (copied book, export with open work), OR-J34-S05, OR-J04-S43.

Provider fakes: `nmimock`, `stripemock`, `ccbillmock`, `solanafake`. The only
package test here is `internal/reconcile/solana_discovery_test.go`.

Gaps:

- CI runs against deterministic fakes, not providers; a wire change is
  unverified until exercised against the provider ([evidence](../rails/README.md#evidence)).
- `reconcile.Decide` is pure but has no table test of its own.
- No scenario refuses a CCBill delivery from an unlisted address; the CCBill
  tests allowlist loopback. J28 covers Stripe and NMI; CCBill callbacks only
  appear in `TestCCBillIdentity` and `TestCCBillRefunds`.
