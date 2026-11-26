# Payment providers

How OpenRails writes to NMI, Stripe, CCBill and Solana and stays in agreement
with them. Every provider write is a durable operation recorded before it is
sent; every callback is verified and deduplicated, and a subscription or charge
event is treated as a reason to read the provider. Checkout, dunning and refunds
decide what to charge; this system decides whether a write may reach the
provider and what the provider's answer proves.

## Concepts

- **Rail**: a gateway kind, `billing.Rail`; `rails.Descriptor` (`internal/modules/payments/rails/registry.go`) declares what each does.
- **PSP**: one merchant account on a rail, a `psps` row; its identity is `(rail, live, account_id)`.
- **Operation** (intent): one `provider_intents` row: frozen terms, an idempotency key, a status and a lease.
- **Unknown**: `unknown_needs_verify`, an operation that may have reached the provider; provider evidence settles it, with an attested or timed empty-order release for NMI checkout sales.
- **Merchant controls**: `writes` (`full`, `limited`: no system-origin writes, or `readonly`), the rebill brake on system-origin charges, and destructive arming (`internal/controls`).
- **Provider posture**: a PSP credential's proven environment; any other verdict disarms it.
- **Wake job**: a debounced River job that reads one provider object after a callback.
- **Finding**: a `reconciliation_findings` row; a `pull.*` finding carries a proposed change (a proposal); the queue is [findings and alerts](findings.md).
- **Park**: a live provider subscription converge cannot land (`parked_provider_subscriptions`).

## How it works

### Writes: the provider-write ledger

Each write type is a `Handler` (`Execute`, `Verify`, `CheckRelevance`,
`Backoff`) in `internal/intents`.

1. `Store.Enqueue` (or `Runner.EnqueueAndExecute`, which runs the first attempt
   inline) records the row: frozen `payload`, its PSP (`psp_id`, a composite key
   to `psps` with `rail`), origin, and an `idempotency_key` unique per merchant.
   Its `openrails.provider_operation` wake commits in the same transaction.
2. The `Runner` claims it (`in_flight`, a lease, `attempts` bumped as a fencing token) and checks the handler, `CheckRelevance`, `GateExecution`, the merchant's destructive arming and the `VolumeBreaker`; a charge also checks `RebillHold`. A refusal parks it, refunding the attempt, for `ParkRetryInterval` (5 minutes); one that must wait (`WaitUntil`) stays pending until then with no attempt and no `operation_log` entry; one no longer relevant is superseded, and a collection held by write posture goes unknown and is read back.
3. A destructive type's breaker admission sets `provider_intents.sent_attempt` under its lock, for the claim's own attempt only; a park clears it. The `attempting` entry goes to `operation_log` ([history](history.md)) ahead, on its own statement, and `Execute` runs; its result is written ahead too, and a hold commits with the intent's transition. An entry over the log's bounds keeps the start of its reason and evidence and says `truncated`. A charge first calls `Store.RequireClaim` on its own connection: the claim must still hold a lease beyond `ProviderCallHold` (30 s).
4. The outcome is written on a detached context (`LedgerWriteContext`):
   `succeeded`, `failed_retryable` (provably not executed), `unknown_needs_verify`,
   `failed_terminal`, or parked. A lapsed lease becomes unknown, never a
   re-execution (`Store.PrepareDispatch`). Unknown work runs `Verify`, reads only.
5. What reads cannot settle waits for `openrails intents resolve`: `--receipt`
   is read back and matched to the frozen terms; `--not-executed` needs the
   type's own non-execution proof.

Per rail ([provider uncertainty](../standalone.md#provider-intents) has the full rules):

- **NMI** has no idempotency key. Local refusals and dial failures before a
  connection is obtained return `nmi.ErrNotDispatched`. Lost answers and
  non-200 responses stay ambiguous. Checkout and engine renewals recognize
  `response=3` refusal codes 300, 410, 411, 440, 441, 460 and 461 through
  `nmi.ErrSaleRefused`; engine renewals take the normal decline/dunning path.
  A charge settles only from the exact
  receipt under its invoice id, which every attempt for one period shares; an
  empty search proves nothing. Checkout sales release after 24 hours with an empty
  order search, or earlier on operator attestation, raising a high finding for
  reconciliation if money later appears. Engine renewals accept operator resolution
  from an exact receipt, a definitive decline, or an attested empty order; they
  never release automatically. An empty-order release admits the next attempt
  under the same obligation order, read before submission to catch late money.
  An unresolved NMI submission is never resent
  (`Store.ArmLostSubmissionResend` refuses any rail but Stripe).
- **Stripe** requests carry an `Idempotency-Key` rooted in the operation id
  (`stripeapi.SetIdempotencyKey`). A lost renewal may replay it after
  `LostSubmissionSettle` (5 minutes), at most `MaxLostSubmissionResends` (2)
  times, inside Stripe's 24-hour key retention less margins.
- **CCBill** writes are DataLink cancels (`ccbill_cancel_subscription`), status
  read first; refunds are refused (`refund_unsupported`).
- **Solana** pulls (`solana_pull`) record the signature and its blockhash's last
  valid height before sending, and are re-signed only once the chain proves the
  pull never landed: the finalized block height is past that last valid height
  and a history-searching status read does not know the signature
  (`RPCClient.NeverLanded`). A signature the chain reports at any commitment,
  confirmed included, counts as landed.
- **Solana subscription receipts settle at finalized.** A first payment, a plan
  change and a renewal pull are booked, and access extended, only from a
  transaction read at finalized commitment (`ChainReader.GetFinalizedTransaction`),
  and a pull's revert is classified only from a finalized read. A transaction
  that is only confirmed is landed with its settlement pending: its operation
  waits for the verifier to read again, and is never failed, re-signed or
  canceled for it. One-off
  purchases read references at finalized too.

Money-moving types include `nmi_sale`, `subscription_collection`,
`manual_rebill`, `order_collection`, `stripe_tier_change`, `nmi_refund` and
`solana_pull`. Provider-run renewals are invoices with `collection_owner`
`nmi_schedule` or `provider`, matched to accepted schedule versions
([provider-managed invoices](../client.md#invoices-and-checkout)).

### Callbacks

- One handler, `handlers.Webhook`, on `POST /v1/webhooks/{rail}/{account_id}`
  and, for CCBill only, `POST /v1/webhooks/{rail}/{account_id}/{token}` (any
  other rail answers `404 route_not_found`); the path's account must equal the
  payload's. `routeSignedWebhook` accepts a
  delivery only when exactly one PSP on that account verifies it (Stripe's
  `livemode` breaks a live/test tie).
- NMI and Stripe are HMAC-signed. Stripe's signed `t` is a time, checked within
  5 minutes (a literal in the Stripe path of `handlers/webhook.go`); NMI's is a
  nonce, so an NMI replay is answered by the event's `event_id` alone. A thin
  Stripe event is read back first. CCBill signs nothing: its source address
  (`webhookauth.CCBillIPAllowed`), the PSP's webhook token in the URL
  (`checkCCBillWebhook`, compared in constant time with the current token, then
  the previous one inside its overlap) and the payload's account authenticate
  it; a delivery must pass both checks. A PSP that declares `webhook_token`
  refuses a missing or wrong token (`401 authentication_required`, "Invalid
  webhook token"), unless its `require_webhook_token` is false, which still
  admits a tokenless delivery and logs a warning; a wrong token is always
  refused. A PSP without one refuses the token URL. A tokenless delivery that
  one candidate PSP of the account refuses for want of its token is refused
  for all of them (`signedWebhook.veto`), never handed to a sibling without a
  token; an undeclared PSP, or one whose account another PSP holds, admits
  nothing. The source address is the
  one `trusted_proxies` resolves, so a trusted proxy can name any sender; the
  token is what a process inside that network does not hold. The request log
  writes the token segment as `{token}` (`middleware.loggedPath`); it is never
  stored with the event.
- A rotated secret or CCBill webhook token verifies for 24 hours;
  `retire_webhook_overlap` ends it at once. A delivery is claimed in
  `idempotency_keys` and recorded in `webhook_events`, in the handler's
  transaction where possible (`MarkWebhookProcessedInTx`).
- Stripe and NMI subscription events only queue `openrails.subscription_converge`,
  which fetches the subscription; Stripe catalog events queue
  `openrails.provider_object_observe`. CCBill callbacks are not read back (its
  DataLink status read serves cancels and probes), so its payload goes through
  the lifecycle machine, one event at a time on the locked row.
- Because a CCBill callback is unsigned and applied as sent, the provider
  refresh checks it afterwards and only reports; it never changes access or
  money. Every CCBill DataLink read (the full read and each paced day) records
  the `REBILL` rows it lists in `provider_listed_rebills`: transaction id and
  subscription id without leading zeros, and the amount. After its reads each
  refresh run lists the rebills recorded from CCBill callbacks (a payment
  whose attempt is `observed_via` `webhook`; positive `rail` charges, no
  reversals) that the server received (`payments.created_at`, never the
  callback's own timestamp) between 30 days and 36 hours ago, and before the
  read reached 36 hours past them. One no read has listed with both its
  transaction id and its subscription raises `pull.charge.unconfirmed` (high,
  `requires_review`, one per PSP and transaction, with the payment, customer,
  subscription and its CCBill id, amount, currency and any listing of the
  transaction under another subscription as evidence); a read that later
  lists it resolves it. The amount is evidence, never a condition. The check
  makes no DataLink request of its own.
- It is high, not critical, until DataLink's dating of rebills is confirmed
  against a live account: reads do not overlap, so a genuine rebill DataLink
  exports late into a window already read can raise it today. Initial sales
  (the fetcher reads no `NEW` export) and upgrades are not checked, the
  latter until DataLink's listing of them is confirmed.
- When the ACTIVEMEMBERS roster lists a member OpenRails has as a CCBill lapse
  (`cancel_type` `expired`), `pull.subscription.mismatch` is critical if the
  lapse cut paid access short (ended before `current_period_ends_at`, as a
  `failedRB` cancel does) or the roster still lists the member 48 hours after
  access ended; an ordinary lapse keeps its severity. Nothing is restored.

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
  when the subscription lands by any path. While the PSP's import is pulling or
  staging the subscription, the park raises no finding.
- **Customer convergence**: `openrails.customer_converge` commits with the
  mutation that changed the customer (`internal/convergejob`).

### Posture

- **Write posture** (`controls.View`): the merchant's `merchant_controls.writes` (`full` without a row); `readonly` in a database whose book was never armed (`openrails book arm`), and when the controls cannot be read. Each wire choke (the Stripe transport, the NMI, CCBill, Basis Theory, HyperSwitch and Solana clients), provider operations, Solana plan publishing, Stripe webhook registration and usage collection read the posture of the merchant they write for through it; nothing else reads the table.
- **Rebill brake** (`controls.View.RebillHold`, `intents.RebillHold`): while `rebills_held`, a system-origin charge (engine renewal or retry, NMI recurring recovery, scheduled usage collection, Solana renewal pull) is not selected by the due pass, is refused at admission (`controls.ErrRebillsHeld`) and parks unsent at execution. A user-origin charge, the customer's own, never waits. `GateExecution` runs user, admin and provider
  origins under `limited`, system origin only under `full`, nothing under
  `readonly`. The transports refuse writes again in `readonly`
  (`ErrProviderReadOnly` in `stripeapi` and the NMI client).
- **Provider posture** (`providerposture.Registry`): a verdict keyed by rail, merchant, PSP, account, endpoint and credential fingerprint. Only `Live` or `Simulated`, as the environment expects, arms. NMI accounts and Solana networks are proven at load and again at the first write after `VerdictTTL` (one hour). A Stripe key is proven once per merchant, PSP, declared account and key: `GET /v1/account` must name the declared account (another is `Mismatched`), a test key's `GET /v1/balance` must report test mode, and a live key's mode is its prefix; a key Stripe refuses, or one lacking a read's permission, is `Refused` with Stripe's reason, and asked again at most every `RefusedRecheck` (five minutes). Loading a merchant's configuration, in a server or a one-off tool (`merchants.RegisterStripePosture`), makes each Stripe PSP document's key, archived ones too, that document's registration (`Registry.Register`): a changed document replaces it, a removed one drops it. `stripeapi`'s `guardTransport` holds each write to an armed registration of its merchant for its key (`Registry.RequireCredential`), so a document declaring the key with a wrong account disarms only itself: a key the merchant declares nowhere is refused at once with `ErrPending`, an `ErrDisarmed` that parks an intent for a later try, a first check still running is waited for, a 401 Stripe answers to a write refuses the key until it is accepted again (not dispatched), and a live key never goes to a fake provider. An unarmed PSP's writes fail with `ErrDisarmed` before any bytes are sent; reads work. A sandbox CCBill PSP never arms.
- A second live PSP with another's key (`psps.credential_fingerprint`) stays
  disarmed (`consistency.duplicate_gateway_account`). Objects OpenRails creates
  at NMI and Stripe carry the book id (`internal/booktag`).

Other tables: `provider_bills`, `provider_charge_attributions`, `provider_listed_rebills`, `webhook_health`,
`webhook_health_daily`, `nmi_history_months`, `nmi_bulk_checkpoints`,
`solana_subscriptions`, `solana_pay_references`, `solana_pay_receipts`.

## Interfaces

- **Go** (`*Client`): `ListPSPs`, `GetPSP`, `CreatePSP`, `UpdatePSP`,
  `PreviewPSPRouting`, `ListFindings`, `GetFinding`, `ResolveFinding`,
  `ListProposals`, `ApproveProposals`, `DismissProposals`, `ListParkedSubscriptions`.
- **HTTP**: one `/v1/merchant` route per method above (under `psps`, `findings`,
  `proposals`, `parked-subscriptions`) and `POST /v1/webhooks/{rail}/{account_id}`.
- **CLI**: `openrails intents resolve`, `openrails provider-refresh`,
  `openrails book arm`, `openrails merchant controls|writes|rebills|destructive`, `openrails undo-run`,
  `openrails solana-pay resolve`.
- **Configuration**: `solana.crankers`, each PSP's `environment`; test seam `Deps.ProviderEndpoints` (`CCBillWebhookSources`); probes `openrails_psp_posture`, `openrails_solana_crankers`. Docs: [rails](../client.md#merchant-configuration-and-psps), [PSP environments](../client.md#merchant-configuration-and-psps).

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

Stripe and NMI subscription and charge events only wake a fetch, so a late,
repeated or reordered one cannot write stale state; refund, void, dispute and
chargeback notices are applied from the verified payload, once per event id.
Missing callbacks are caught by reads driven by evidence (the rebill watch,
verification, attributions, refreshes). A subscription ends only on the
provider's own terminal answer (Stripe `resource_missing`, NMI `E_NOT_FOUND`);
no evidence keeps it `unverified` with access, and what converge cannot land
parks for a person.

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
- OR-J28-S02: a missing, wrong, tampered or foreign-account signature changes no invoice and wakes no `openrails.provider_operation`.
- OR-J28-S06, S07: another merchant's claim of an account is refused; live and test PSPs on one account get only events their own secret verifies.
- OR-J15-S06: an unverified subscription is read with its own PSP, and a sibling account's absence cancels nothing.

### 5. Contracts match the code

Routes are declared once (`internal/http/routes/psps.go`, `ops.go`,
`merchant.go`) and snapshotted in `api/openapi.json`, `Client` methods in
`api/go.txt`, tables in `api/schema.txt`; `Engine.Merchant` and `Remote.Merchant`
return the same `*Client`. `stripeapi.APIVersion` pins the outbound
`Stripe-Version` and the registered endpoint alike.

Checks:
- `go run ./scripts/contracts` reports no change under `api/`; `intent_type` is free text, so a new operation type is additive.
- `stripeapi.guardTransport.RoundTrip` sets `Stripe-Version` on every request lacking it.

### 6. Data lifecycle

In `internal/retention` (`tables.go`; `OutboxIntentTypes` in `retention.go`), instruction-only operations and `operation_log` entries go after 25 months; `webhook_events`, released parks and `provider_listed_rebills` after 90 days; resolved findings after 12 months; money-moving operations and provider bills, schedules and receipts are permanent. None is partitioned.

Checks:
- Each table comment's `Retention:` sentence matches its `tables.go` rule (`State` tables carry none).
- `provider_intents_updated_at_idx`, `DeleteFinishedOutboxIntentsBefore` and `OutboxIntentTypes` list the same types; nothing checks it.
- OR-J29-S06: retention and export refuse to discard unresolved money; `immutable_operation_log` blocks log edits.
- OR-J43-S08: an operation's `attempting` entry is written before its provider call and outlives a crash; OR-J43-S11: the breaker counts an in-flight attempt only once `sent_attempt` names it.

### 7. Work scales with activity

An operation's wake is queued by the write that leaves it live, at its due time; `openrails.operation_wakes` only requeues operations an hour overdue, 500 at a time. Wake jobs coalesce per object. The `Verifier` reads queued ids; the rebill watch reads only overdue NMI schedules without an attempt (`ListOverdueRebills`). Provider posture is proven per credential set at load and at the first write after an hour (a Stripe key's once), never by sweep.

Checks:
- `OperationWakes.Pass` uses `ListOverdueProviderOperations` past `OperationWakeGrace`; OR-J04-S60 requeues a lost wake.
- OR-J15-S07: a verification burst reads only flagged subscriptions; OR-J15-S72: a complete refresh queues no due work.

### 8. Abuse protection

Webhooks share the `webhook` rate-limit bucket (1200 a minute) and per-rail body caps (CCBill 16 KiB, NMI 64 KiB, Stripe 256 KiB) before parsing. Unsigned, mis-signed and stale deliveries are refused and counted in `webhook_health_daily`. CCBill is accepted from its published ranges, or from `Deps.ProviderEndpoints.CCBillWebhookSources`, a test seam, only for an account proven non-live. Customer declines from charge operations feed the card-testing ledger (`Runner.recordInvoiceCardDecline`).

Checks:
- `readLimitedWebhookBody` wraps every body in `http.MaxBytesReader` and answers 413.
- Stripe's public path checks its signed time within 5 minutes; only the queued re-verification skips it. NMI's signed `t` is a nonce (`sigverify.VerifyNMI`): no window, and a replay is its `event_id`, applied once (`DeduplicationService.ProcessWebhook`).
- `webhookauth.CCBillIPAllowed` refuses an allowlisted source when the account's money class is unknown; `internal/config` drops a `/0` entry and reports it.

### 9. Destructive-action safeguards

Destructive types (`IsDestructiveIntentType`: NMI subscription deletes, CCBill
cancels, card deletes) pass the merchant's destructive arming (`destructive.Gate`
on `merchant_controls.destructive_armed`; it starts disarmed, so nothing
destructive runs and pulls stay advisory until an operator arms it), the producer
`RateCeiling` (`PerActorHourlyCeiling` 5; per merchant `PerMerchantHourlyCeiling`
15 for user and admin origins, `PerMerchantSystemHourlyCeiling` 50 for system)
and the `VolumeBreaker`; a held one parks unsent. Every convergence cancel
passes `GateCancelPass` (the arming, then the cancel budget); a held cancel
leaves the row `unverified` with access. Operations record `actor`, `subject`
and `credential`; uncertainty never triggers a compensating cancel or refund.

Checks:
- `Runner.executeOne` checks `AllowDestructive` and `Breaker.Check` before `Execute`.
- OR-J34-S06, S30: a held webhook cancel keeps access; disarming mid-pull holds its retractions.
- `PlanUndoRun` refuses `NeverRollbackableTables` (including `operation_log` and `webhook_events`); OR-J38-S71 spends the actor allowance once per subscription.

## Tests

Critical scenarios:

- J06: OR-J06-S01 to S05, S09, S30, S31, S60 to S64 (`TestPSPControls`, `TestStripeWebhookDueWork`), S07 (`TestBackgroundJobs`), S08 (`TestPSPPostureExpiry`), S65 (`TestDueWork`).
- J10: OR-J10-S01, S03, S04 (`TestPaymentReplay`), S02 (`TestCheckoutBoundaries`), S05 (`TestInvoicePaymentReceipts`).
- J11: OR-J11-S01, S02 (`TestProviderRecovery`), S04 (`TestUnresolvedRouting`).
- J15: OR-J15-S01 to S04 (`TestNMIRecovery`), S06, S07 (`TestVerificationBinding`), S08 to S21 (`TestProviderTrailing`), S30 (`TestBackgroundJobs`), S70, S72 (`TestDueWork`), S71 (`TestNMIRoster`).
- J28: OR-J28-S01 to S05, S08 (`TestCallbackContracts`), S06, S07 (`TestWebhookAccountClasses`), S17 to S23 (`TestCCBillWebhookToken`).
- J29: OR-J29-S01, S04 (`TestProcessRecovery`), S02, S05, S06 (`TestUnresolvedMoneyContracts`), S07, S08, S40, S41 (`TestBackgroundJobs`), S37, S70 to S73 (`TestDueWork`).
- J38: OR-J38-S86 to S94 (`TestCCBillCallbackEvidence`): unconfirmed CCBill callback rebills and doubtful lapses.
- J37: OR-J37-S01 to S05, S63, S73 (`TestSolanaContracts`), S07, S09, S60 to S62, S72, S74 (`TestSolanaPulls`), S64 to S68 (`TestSolanaPlanChanges`), S06 (`TestBackgroundJobs`), S10 (`TestFXRates`), S70 (`TestInvoiceExpiryClock`).
- Elsewhere: OR-J09-S05, S06 (lost and partial NMI sales), OR-J16-S04, OR-J20-S03, S70, OR-J26-S55, OR-J31-S03, S04 (copied book, export with open work), OR-J34-S05, OR-J04-S43.

Provider fakes: `nmimock`, `stripemock`, `ccbillmock`, `solanafake`. The only package tests here are `internal/reconcile/solana_discovery_test.go` and the fake's own `openrailstest/nmimock/nmimock_test.go`.

Gaps:

- CI runs against deterministic fakes, not providers; a wire change is
  unverified until exercised against the provider ([evidence](../../.claude/CLAUDE.md#rails-and-psps)).
- `reconcile.Decide` is pure but has no table test of its own.
- CCBill's own source ranges are never exercised: the CCBill tests allowlist
  loopback. OR-J28-S17 refuses a forwarded non-CCBill address.
