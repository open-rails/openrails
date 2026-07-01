<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 681

---

# #679: destructive provider-intent circuit breaker — mass NMI deletes must halt and ask

**Completed:** no
**Status:** PLANNED (2026-07-01) — from Paul's #664 post-mortem question: the 1,672 wrongful cancellations queued
ZERO nmi_delete intents (lucky, see Findings), but the legitimate delete path has no volume guard. NMI has no
read-only keys, so a bug that mass-cancels through the REAL path (FailMembership) would mass-delete real
production subscriptions.

## Metadata
- Category: safety
- Status: planned
- Passes: false

## Findings (why the incident queued no provider intents)

Converge repairs are structurally provider-side-effect-free: the old `grace_exhausted` repair called
`ApplyLocalCancellation`, which contains NO delete scheduling; only `FailMembership` writes
`DeletionScheduledAt` + the `nmi_delete_subscription` intent, gated on an injected `deferDelete` scheduler that
`NewConvergeEngine`'s lifecycle instance never receives. The converge pass comment even deferred the
provider-cancel remediation ("lands with the provider-action wiring") — never wired. So the incident corrupted
local state only; NMI was untouched. Post-#664 converge cannot even cancel locally.

## Existing safeguard stack (verified, keep)

1. `test_mode=true` + NMI boot probe: production NMI credentials REFUSE BOOT in a test environment (cached
   verdict in `openrails.probe_verdicts`) — the "prod keys in a test env" scenario is already fail-closed.
2. `provider_write_mode=readonly`: transport-level block in the NMI client (every direct-post mutation returns
   ErrProviderReadOnly) and the stripeapi choke point; REQUIRED outside development.
3. `provider_write_mode=limited`: no system-initiated provider writes (FailMembership skips delete scheduling).
4. All remote subscription deletes funnel through the durable provider-intent ledger + executor — one chokepoint.

## Gap + design (recommended)

The unguarded case: a #664-class bug that wrongly routes subs through the LEGITIMATE dunning path — real retries
fail (cards can't pay for the wrong reason), FailMembership terminally cancels, and thousands of nmi_delete
intents execute automatically under `provider_write_mode=full`.

Recommend a VOLUME CIRCUIT BREAKER at the intent executor (not blanket approval): destructive intent types
(`nmi_delete_subscription`) get a per-(merchant, rail) execution budget per rolling window — e.g.
`max(K, small % of active subs)` per 24h. Over budget → executor HALTS destructive types for that merchant,
emits an OPERATOR finding, and resumes only on explicit operator ack. Routine churn (a handful/day) stays
automatic — holding EVERY delete behind approval would let NMI keep billing customers whose subs we cancelled
(customer harm) and turns normal operation into an ops queue. Mass deletion is always an incident; that is what
asks permission.

## Tasks

- [ ] Budget check in the provider-intent executor for destructive types, per (merchant, rail), rolling window;
      threshold `max(K, pct of active subs)` — constants, not config knobs, until someone needs tuning.
- [ ] Breach → halt destructive execution for that merchant + OPERATOR finding (`life.provider_intent.held_bulk`
      or similar); non-destructive intents unaffected; explicit ack resumes.
- [ ] Intents held by the breaker stay pending (never expire into `abandoned` while held).
- [ ] Tests: N deletes under budget execute; N+1 halts + finding; ack resumes; other merchants unaffected.

## Out of scope

- Per-delete operator approval (rejected: harms customers via continued NMI billing, and normal churn would
  drown an approval queue).
- New read-only NMI credentials (NMI doesn't offer them; transport read-only mode already covers it).

Acceptance: no single run/window can mass-delete provider subscriptions; a bulk anomaly halts destructive
execution and requires explicit operator acknowledgment; routine single deletes remain automatic; the existing
boot-probe/readonly/limited stack is unchanged.

---

# #671: CRITICAL — micros passed where cents/dollars expected: real charges at 10,000×

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit. Headline claims (1a, 1b, worker wiring)
re-verified firsthand in source before filing. One root cause: the micros migration flipped the happy-path
checkout but missed tier-change/upgrade, Solana quote, and admin-refund paths. `nmi.SaleParams.Amount` is CENTS
(`FormatCentsDecimal`, internal/integrations/nmi/payments.go:47,104); `price.Amount` is MICROS (the happy path
converts via `MicrosToCentsExact`, internal/modules/checkout/nmi_sale_service.go:156).

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Findings
- **1a. NMI upgrade proration sale**: `internal/modules/checkout/service.go:1690` computes `prorationAmount` in
  micros (`CalculateModelBUpgradeCharge(oldPrice.Amount, newPrice.Amount, …)`, unit-preserving :2119), then
  :1805-1807 charges it RAW: `client.RunSale(nmi.SaleParams{Amount: prorationAmount})`. Preview shows $31.34,
  execution charges $313,400. VERIFIED.
- **1b. NMI upgrade recurring amount**: service.go:1762 `Amount: float64(newPrice.Amount) / 100.0` where
  `RecurringPaymentData.Amount` is float DOLLARS (create path :706 correctly uses `MicrosToMajorUnits` ÷1e6).
  Every rebill of the upgraded sub at 10,000×. VERIFIED.
- **1c. Solana one-time quotes**: `CalculateTokenQuote(…, amountCents…)` divides by 10^minorUnits
  (internal/modules/solana/support.go:123-138,199-207); all three prod callers pass MICROS:
  solana/pay.go:217, checkout/session_service.go:716, handlers/solana_supported_tokens.go:305→361.
  $19.99 → wallet asked for 199,900 USDC. Also label bug: solana/transaction.go:117 FormatCentsDecimal(micros).
- **1d. Solana upgrade first pull**: micros fed into `FiatCentsToStablecoinBaseUnits` (cents contract,
  support.go:74-86) at handlers/solana_recurring.go:385-394 + checkout/session_service.go:1305-1312. The correct
  `FiatMicrosToStablecoinBaseUnits` (support.go:53) exists, tested, ZERO production callers.
- **1e. Admin refunds**: `req.Amount` validated in micros (internal/modules/payments/payment.go:267) then shipped
  as `RefundPayload.AmountCents` (handlers/admin_payments.go:353), executed as cents (intents/refund.go:226 NMI
  FormatCentsDecimal; :400-402 Stripe). Full refunds always fail (> charge), but a partial refund ≤1/10,000 of
  the charge SUCCEEDS at 10,000×: refunding 5,000 micros ($0.005) of a $60 payment refunds $50 real money.
  Bonus: verifier compares cents vs micros (refund.go:317-318) so verified-executed refunds never match.
- **1f. Reporting-only siblings**: Stripe failed-payment rows store `inv.AmountDue` CENTS in the micros column
  (webhooks/stripe.go:1151,1166-1178; success path :484 converts). Analytics `float64(x)/100.0` family:
  stripe.go:1234, nmi.go:386,2085, ccbill.go:1581,2527,2766,2894,2975, analytics/event_log_service.go:1063,1096.
- **1g. Contradictory internal→rail-minor converters; JPY 100× exposure**: arrears hardcodes
  `(snapshot+9_999)/10_000` ceil (money/arrears.go:262) vs auto-topup's registry-scale
  `nativeAmountToRailMinor` truncating + assuming 10⁻² (money_in.go:221-236). JPY (scale 4, currency.go:26):
  the two paths send 100×-different values to the same Charger; Stripe JPY is zero-decimal. Also topup
  truncation credits more micros than charged (≤9,999/topup); arrears ceil collects ≤9,999 micros unledgered.

## Tasks
- [ ] Fix 1a/1b/1d with the existing exact converters (`MicrosToCentsExact`, `MicrosToMajorUnits`,
      `FiatMicrosToStablecoinBaseUnits`); fix 1c callers or retype CalculateTokenQuote to micros.
- [ ] Fix 1e end to end (micros → provider-minor at the intents boundary; fix the verifier comparison).
- [ ] Fix 1f storage row (stripe.go:1151) exactly; analytics display family opportunistically.
- [ ] 1g: ONE scale-aware converter with an explicit per-adapter unit contract; both arrears + topup use it.
- [ ] SYSTEMIC: introduce `Micros`/`Cents` defined types at the integration boundaries (nmi.SaleParams,
      RefundPayload, solana helpers) so this bug shape is a compile error; sweep remaining `/100` and `/10_000`
      literals.
- [ ] TEST WALL (Paul 2026-07-01: mandatory — a units mixup must never reach production again; unit-test EVERY
      place that matters). Two layers:
      1. Wire-pinning unit tests at every provider money boundary — feed a known micros amount, assert the EXACT
         wire value the provider sees (not "a" conversion — the literal string/int on the wire):
         - NMI (internal/integrations/nmi): RunSale, capture, refund, void, AddRecurringSubscription /
           update-recurring — 19_990_000 micros ⇒ wire "19.99"; sub-cent input ⇒ error, never rounding.
         - Stripe (stripeapi + adapters): every outbound amount (charge, refund, price/plan push) micros⇒cents;
           inbound webhook cents⇒micros for BOTH success and failed-payment rows.
         - CCBill: inbound amount parsing + the ±2% validation boundary.
         - Solana: CalculateTokenQuote per currency, FiatMicrosToStablecoinBaseUnits, and the transfer
           instruction base-unit amount ($19.99 ⇒ 19.99 USDC base units, never 199,900).
         - Rail-minor converters (arrears + topup): USD, JPY/zero-decimal, scale-4 — both paths produce the
           SAME provider amount for the same micros.
      2. Cross-path invariant tests: tier-change preview amount == executed charge amount; refund request
         (micros) == provider-executed amount (minor units) == what the verifier compares; deposit credited
         micros == charged amount converted back (no truncation gain).
      Convention going forward: any NEW provider boundary ships with its wire-pinning test in the same PR —
      typed Micros/Cents makes new mixups uncompilable, the tests pin the conversions that remain.
- [ ] Stale unit docs while in there: pkg/catalog/manifest.go:159 example, solana/recurring/confirm_tier_change.go:73,
      models/payment.go:28.

## Acceptance
No production call passes micros into a cents/dollars parameter (typed units make it uncompilable); upgrade,
Solana, and refund paths charge exactly what the preview/request says; JPY topup and arrears collect the same
provider amount for the same micros. EVERY provider money boundary has a wire-pinning unit test (known micros
in ⇒ exact wire amount out) — a reintroduced units mixup fails unit tests, not production.

---

# #672: CRITICAL — metered usage re-accrued (billed N×) across overlapping invoice-close windows

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit.

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Problem
`FinalizeInvoice` sweeps catalog metered usage keyed by the exact window: `meteredPeriodSourceID` =
`period:<from>-<to>` (internal/modules/money/metered_rating.go:574-576; sweep at money/invoice.go:78). Threshold
closes anchor `from` at the current period start (invoice.go:751-758), so a June-10 threshold close accrues
[Jun1,Jun10), a June-20 close accrues [Jun1,Jun20) — different source_id → NOT deduped by
`uq_invoice_items_source` → the full period-to-date usage accrues AGAIN; the daily `FinalizePreviousMonth` job
then accrues [Jun1,Jul1) a third time. Each accrual is a real `owed_accrual` transfer + pending invoice item
collected by `ChargeOutstanding` → the customer's card is charged N× for the same usage, and each sweep re-trips
the threshold (feedback loop).

## Tasks
- [ ] Rated-through watermark per meter (or accrue only the delta vs the already-accrued prefix); source_id keyed
      by the watermark advance, not the full window.
- [ ] Backstop invariant: sum of accruals for a meter over a period == rating of that period's usage exactly once
      (integration test with a threshold close mid-period + month-end finalize).
- [ ] Audit existing accruals for double-billing exposure once fixed (conservation query).

## Acceptance
Threshold close + later threshold close + month-end finalize over the same period accrues each unit of usage
exactly once; the feedback loop is structurally impossible (watermark monotone).

---

# #673: CRITICAL — invoice collection + auto-top-up workers run with no merchant context: wedged since birth

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit; wiring re-verified firsthand
(jobs_credit_money_in.go has zero merchant-context references; InvoiceSettings → merchantconfig Get →
merchant.Require → ErrNoMerchant).

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Problem
`InvoiceWorker.Work` → `w.Money.InvoiceSettings(ctx)` (internal/river/jobs_credit_money_in.go:135) →
`merchantconfig.Store.Get` → `merchant.Require(ctx)` → ErrNoMerchant; `AutoTopupWorker` → `RunAutoTopups` →
`belowThresholdAccounts` → same. River job contexts carry no merchant, and unlike every sibling money worker
(jobs_dunning.go:189, jobs_converge_sweep.go:66 loop merchants under RunInMerchantConn) these two never inject
it. Every hourly `InvoiceArgs{Collect:true}`, monthly floor sweep, daily FinalizePreviousMonth, and 15-min
auto-top-up tick (internal/app/river_register.go:478-541) errors and retries forever: arrears are never
collected, top-ups never happen, silently.

Related while in there: `LowBalanceAlertWorker` is registered without an `Alerter` → permanent no-op
(river_register.go:170-174; nil guard jobs_credit_money_in.go:40-43).

## Tasks
- [ ] Mirror ConvergeSweepWorker: enumerate active merchants, run each under `RunInMerchantConn(merchant.WithID(…))`.
- [ ] Integration test that runs InvoiceWorker + AutoTopupWorker against a seeded merchant and asserts actual
      collection/top-up effects (this class of wedge = a worker whose every run errors — consider a
      worker-error-rate alert so "retries forever" is never silent again).
- [ ] Wire or delete the LowBalanceAlertWorker registration.
- [ ] NOTE: fixing this ACTIVATES arrears collection + metered sweeps — land #672 (N× accrual) and the #674
      arrears-CAS fix first or together, or the newly-unwedged workers start collecting doubled debt.

## Acceptance
Hourly/monthly/daily invoice jobs and auto-top-up demonstrably move money for a seeded merchant in integration
tests; no worker in river_register runs with a context its own call graph rejects.

---

# #674: universal write-ahead provider-intents log — every external write durable-first, effectively-once

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit. One theme, six sites: a real provider
charge can happen with no durable local record (→ re-charge on retry/crash), and transport-ambiguous errors are
treated as clean declines. The provider-intents outbox (dunning/manual-rebill/refunds) already does ALL of this
right — unique (merchant, idempotency_key), SKIP LOCKED lease, attempts>1 ⇒ verify-at-provider before resend —
these paths predate/bypass it.

## Design decision (Paul, 2026-07-01): generalize, don't patch

ALL writes into a payment provider / external system post to the provider-intents queue FIRST, then execute
IMMEDIATELY in the same request (write-through: the happy path pays one local INSERT + complete, never a worker
round-trip — this must not slow writes down). The intent log is the durable write-ahead record of every
external write we ever attempt; semantics are effectively-once into the external system: durable intent +
provider idempotency key/order-id DERIVED FROM THE INTENT + on any ambiguity or restart, verify-at-provider
before resend. If the provider is misconfigured or offline, the intent stays pending and the existing outbox
lease/sweeper retries it later — nothing is ever lost to a down or misbehaving provider. Read-only provider
calls are exempt. The six findings below become migrations onto this one primitive, not six bespoke fixes.

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Findings
- **Arrears collection CAS drop**: `chargeOneOpenInvoice` (internal/modules/money/arrears.go:246-345) charges
  the card (key `invoice:<id>:<snapshot>`, :255-266) BEFORE the recording tx; inside,
  `ApplyInvoicePaymentSnapshot` is a snapshot CAS and on n==0 the code `return nil` (:288-290) — provider
  charged, no PayOwed transfer, no invoice_payments row, no refund; next run re-charges the remainder. Key is a
  MUTABLE snapshot so Stripe's dedup drifts too; NMI never dedupes (key only order_id, nmi_collection.go:56-62).
- **NMI sync sale/subscription ambiguity**: RunSale/AddRecurringSubscription return one error for decline AND
  timeout-after-send (25s, nmi/client.go:45). nmi_sale_service.go:169-176: any error → MarkFailed + DeleteVault
  + idempotency Fail; retry is told to use a NEW key (:119-127) → new orderid → double charge. Subscription
  create (checkout/service.go:716-736): timeout after NMI created the sub = live remote subscription billing
  every cycle with no local row. Upgrade proration (service.go:1812-1819) rolls back without verifying the sale.
  `FindSuccessfulSaleByOrderID` verify-on-ambiguity exists (intents) and is unused here. (Adjacent to #330 but
  distinct.)
- **Auto-top-up**: topUpAccount (money/money_in.go:166-218): episode = now.Truncate(max(cooldown,1m)); charge →
  Deposit → stamp. Crash between charge and deposit: same-bucket retry re-charges on NMI; cross-bucket retry
  double-charges even on Stripe; first charge recorded nowhere.
- **NMI sale wedged on crash between provider success and CompleteProviderAttempt**
  (nmi_sale_service.go:139-182): attempt row stays `pending` forever ("already pending, please wait"); no
  sweeper resolves stale attempts via FindSuccessfulSaleByOrderID; new-key retry double-charges.
- **ReserveProviderAttempt hides "already existed"** (modules/payments/payment.go:187-218): ON CONFLICT DO
  NOTHING then returns the existing row with no created/found signal; caller proceeds to RunSale regardless —
  two replicas can double-charge when Redis idempotency is per-process.
- **Solana crank invisible renewal**: jobs_solana_crank.go:187,285-304 submits on-chain pull then records; crash
  between = re-run hits the AlreadyPaid branch (:208-217) which only advances next_pull_at — no RenewMembership,
  no payment row; SolanaReconcileWorker cross-checks only last_signature (jobs_solana_reconcile.go:83-96) which
  never advanced. Subscriber paid on-chain, gets nothing, zero drift alert.
- Minor: the only guard on `nmi_upgrade` replays is the 5-min default idempotency TTL (no durable key-addressed
  record).

## Tasks
- [ ] Extend `internal/intents` with a synchronous execute-through mode: `PostAndExecute(intent)` = insert
      intent (in the caller's local tx where one exists) → run the provider call inline → complete/fail the
      intent with the outcome. Ambiguous outcome (transport error after send) marks the intent
      `pending_verify`, never failed. The EXISTING lease/sweeper machinery picks up pending/pending_verify
      intents: verify-at-provider (FindSuccessfulSaleByOrderID / Stripe fetch by idempotency key / chain read
      by signature) then resolve or resend. New intent kinds as needed: sale, recurring-create/update/delete,
      vault-delete, void, topup-charge, solana-pull.
- [ ] Provider idempotency keys/order-ids are derived from the intent id — one intent, one external effect,
      regardless of how many times execution is attempted.
- [ ] NMI client: classify transport-ambiguous vs provider-declined errors (prerequisite for the
      pending_verify state); ambiguity ⇒ verify-by-order-id, never MarkFailed/DeleteVault/new-key.
- [ ] Migrate the six sites onto the primitive: sync NMI sale + subscription create + upgrade proration;
      arrears collection (key by invoice+attempt intent, not mutable snapshot; on CAS n==0 after a successful
      charge, record the payment unapplied or void — never `return nil`); auto-top-up (episode from persisted
      state); Solana crank (intent carries the signature before submit; AlreadyPaid branch resolves via the
      intent and runs the renewal repair); retire/absorb `ReserveProviderAttempt` + `nmi_sale_attempt` rows
      into intents (or give Reserve a created-bool and a stale-pending verifier if kept).
- [ ] Enforcement: no provider write bypasses intents — integration-client choke points (stripeapi, nmi client
      mutating methods, solana submit) accept an intent ref or are only callable from the intents executor;
      plus a lint/deps-test-style guard so a bypass fails CI, not review.
- [ ] Crash-injection integration tests per site: die before-execute / after-execute-before-complete / on
      timeout ⇒ re-run yields exactly one external effect and one local record; provider-offline test: intent
      parks, provider returns, sweeper lands the write with the original idempotency key.

## Acceptance
Every external WRITE (charge, refund, void, recurring create/update/delete, vault delete, on-chain submit) has
a durable intent row written before execution and completed with the outcome; the happy path executes inline
with no added round-trips; a crash, timeout, or offline/misconfigured provider at ANY point leads to
verify-then-resolve from the intent log — effectively-once into the external system, never a blind re-charge,
never a charged-but-unrecorded outcome, never a lost write. No code path can reach a provider write without an
intent (enforced by CI guard).

---

# #675: webhook state correctness — clobbering writes, dropped renewals, swallowed reversals

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit. Enqueue/dedup/signature layers verified
sound; these are the apply-layer holes.

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Findings
- **No row lock / ordering guard; full-row clobber**: `SubscriptionRepo.UpdateAt` writes EVERY column from an
  in-memory image, no version guard (internal/db/repo/subscription.go:138+, "nil values CLEAR fields"); no
  FOR UPDATE on subscriptions anywhere. `handleSubscriptionUpdated` (webhooks/stripe.go:899-975) is a
  non-transactional read-modify-write and ignores evt.Created. Renewal emits invoice.paid +
  subscription.updated near-simultaneously (concurrent per-request processing, handlers/webhook.go:574-605):
  the stale full-row write reverts RenewMembership's committed changes; a delayed older
  subscription.updated(active) after invoice.payment_failed flips past_due→active without payment (only
  `cancelled` is terminal-guarded :904-911). The 2-min pending-lease takeover (deduplication.go:136-154) adds
  same-event concurrency.
- **Terminal-blocked renewal charge silently dropped**: stripe.go:521-527 IsTerminalTransitionBlocked → Warn →
  return nil → deduped as success; nmi.go:962-970 identical. Customer charged, no payment row, no ledger trace,
  never retried. CCBill does it right (ccbill.go:2421-2443 writes a terminal_blocked_renewal_success repair
  alert) — copy it.
- **Void/chargeback before sale materializes is ACKed**: CCBill handleVoid (ccbill.go:1912-1955) +
  handleChargeback (:2096-2144) + NMI handleVoidSuccess (nmi.go:2272-2277): subscription not found → return nil
  (deduped). Provider fraud-voids seconds after approval; if Void wins the race, the later NewSaleSuccess
  creates entitlements+credits for an already-reversed charge.
- **Subscription credit grants warn-only at all six call sites**: stripe.go:548-559, ccbill.go:631-639,
  2464-2472, nmi.go:915-928,974-987 — transient GrantSubscriptionCredits failure → event marked success → that
  period's credit lot lost forever. The deposit is idempotent per (subscription,label,period_end) under the
  customer lock, so returning the error for retry is safe.
- **NMI one-off refunds ACKed with zero effect**: the reversal block is gated on `subscription != nil`
  (nmi.go:2024); dashboard refund of a one-time purchase keeps payment/entitlements/credits. Refund amount
  parse failure downgraded to 0 (nmi.go:1977-1981). Stripe's one-off path is correct (stripe.go:1360-1371) —
  mirror it.
- Minor: CCBill amount/validation mismatches ACK with log-only logBillingError (ccbill.go:358-390) — a >2%
  drifted real charge deserves a durable repair alert.

## Tasks
- [ ] Subscription apply layer: one tx + FOR UPDATE (or updated_at CAS) around read-modify-write; persist and
      compare provider event created-timestamps; make UpdateAt full-row semantics explicit or retire it for
      webhook applies.
- [ ] Terminal-blocked renewal: record the payment or a CCBill-style repair alert before return nil (Stripe+NMI).
- [ ] Void/chargeback/refund not-found: retryable error (as nmi.go:2019 refunds do) or deferred-reversal repair
      alert — never plain ACK.
- [ ] Propagate GrantSubscriptionCredits errors (all six sites).
- [ ] NMI one-off refund branch (resolve by transaction id); parse failure = error.
- [ ] Out-of-order integration tests: invoice.paid ∥ subscription.updated; payment_failed then stale
      updated(active); void-then-sale; renewal-after-cancel.

## Acceptance
Out-of-order or concurrent webhook delivery cannot revert committed state or grant access without payment; no
money-bearing event is ACKed with zero durable effect (payment row, retry, or repair alert — always one of the
three).

---

# #676: spendgate holds — volatile capture pointer, leaking held gauge, phantom sweep

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit.

## Metadata
- Category: admission
- Status: planned
- Passes: false

## Findings
- **Capture depends on a volatile Redis pointer**: capture wire carries only request_id (remote.go:177-192);
  `Service.CaptureHold` (pkg/service/service.go:239-244) hard-fails "hold not found" when the pointer is gone —
  it's TTL'd to the hold (default 1h) and written best-effort AFTER the admit EVAL (spendgate/gate.go:239-240,
  `_ = g.rdb.Set(…)`). Redis restart/failover, a failed Set, or execution outliving the TTL ⇒ service rendered,
  charge unrecoverable (payer coords lost).
- **`held` gauge leaks permanently; the referenced self-heal doesn't exist**: admit INCRBYs held with no TTL;
  only the per-request hold record expires (gate.go:91,106). The comment at gate.go:152-157 promises a
  reconciliation sweep; grep confirms nothing recomputes sg:*:held. Every abandoned admit permanently shrinks
  the payer's capacity toward 100% denial; the only remedy (flush) swings to over-admission.
- **Captures hard-fail up to 1h after a credit lot lapses**: spendBalanceThenOwedTx (money/unified_spend.go:199-211)
  computes fromBalance from the raw counter (includes the unswept lapsed remainder) but CreditSpend only draws
  spendable lots → ErrInsufficientCredits on every retry until the hourly sweep — the "served but can't charge"
  failure #513 decision 8 forbids.
- Minor: releaseScript recreates expired window buckets as permanent negative keys (gate.go:138-146).

## Tasks
- [ ] Durable admit-coordinate record (or caller-supplied payer coords as capture fallback); pointer Set failure
      = admit failure, not best-effort.
- [ ] Implement the held-recompute sweep the comment promises (or rolling-TTL holds + recompute-on-expiry);
      fix releaseScript bucket recreation.
- [ ] Derive the balance/owed split from spendable lots (or fall through to owed on shortfall) so lapsed lots
      can't block capture.
- [ ] Chaos test: admit → Redis flush → capture still lands (degraded path); abandoned admits don't shrink
      capacity permanently.

## Acceptance
A rendered service is always chargeable (no capture path depends on volatile state); abandoned admits release
capacity within one sweep interval; lapsed lots never produce ErrInsufficientCredits on capture.

---

# #677: customer-lock bypasses — credit expiry, converge repairs, AccrueOwed race

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit (lock discipline verified firsthand:
LockCustomerForSpend via lockBalance, money_service.go:664-681; overdraft trigger enforces only the ACCOUNT
total, not per-lot).

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Findings
- **Credit expiry bypasses the lock**: jobs_credit_expiry.go:74-79 runs ExpireLapsed in a bare RunInTx;
  ExpireLapsed (grants/credit_spend.go:81-110) reads derived lot.Remaining then applies credit_expire. A spend
  at T−ε (lock held) and expiry at T+ε both read remaining=X and both commit when other lots cover the account
  floor → lot over-consumed, X of the customer's OTHER credits moved to expired_credits — customer money loss.
- **Converge repairs on a plain conn**: converge.go:245-252 runs Repair without tx/lock;
  clawbackRevokedCredit (grants.go:448-473) — overlapping converge runs can double-claw/double-deposit (no
  unique index on ledger_transfers.grant_id; idx_ledger_transfers_source is NON-unique, migration :2665 — all
  transfer idempotency is app-side).
- **AccrueOwed check-then-insert** (money/arrears.go:57-86): no lock, no constraint; concurrent calls both post
  owed_accrual — invoice item dedupes but the ledger carries 2× debt; arrears_liability never returns to zero.
- Latent: RevokeGrant refund leg reads `clawed` before the clawback re-reads remaining (grants.go:493-519) —
  over-refund race; no non-test callers today (surface being deleted in #666 — keep whichever survives locked).

## Tasks
- [ ] Take LockCustomerForSpend inside ExpireLapsed (or in the expiry job per customer).
- [ ] Wrap converge money repairs in MerchantTx + customer lock; consider a partial unique index on
      ledger_transfers(grant_id, kind) for the clawback/deposit pairs as a DB backstop.
- [ ] AccrueOwed: lockBalance first, or a partial unique index on owed_accrual coordinates.
- [ ] Concurrency tests: spend ∥ expiry on the same lot; two converge repairs on the same grant; two AccrueOwed.

## Acceptance
Every write that consumes or reverses lot value runs under the same per-customer serialization as spends; the
ledger cannot record the same accrual/clawback twice (app lock + DB backstop).

---

# #678: webhook dedup is Redis-only, fail-open, non-transactional

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 money-path audit. Lower urgency than #675 (payment rows
have DB unique backstops; #675 findings are the residual non-replay-safe holes) but this is the posture that
makes those holes reachable.

## Metadata
- Category: reliability
- Status: planned
- Passes: false

## Problem
Webhook dedup lives only in Redis (90d TTL, build_runtime.go:799); `cfg.Redis == nil` silently falls back to a
PER-PROCESS memStore (idempotency/service.go:105-117) — multi-replica standalone loses cross-replica dedup
entirely, and Redis flush/eviction loses history. `Complete` is written after (not with) the effects, so every
handler must be individually replay-safe (that's #675's job). The 2-min pending-lease takeover
(deduplication.go:136-154) permits concurrent same-event processing for slow handlers. `IsDuplicate`
(deduplication.go:70-98) is dead code with a mismatched op key.

## Tasks
- [ ] Refuse to start webhook processing without Redis when running multi-instance (config posture check, like
      EnforceRLSPosture) — silent memStore fallback only in dev/single-process.
- [ ] Consider moving dedup marks into Postgres inside the effect tx (then Redis is a fast-path, not the truth).
- [ ] Revisit the 2-min takeover: lease renewal for slow handlers instead of concurrent takeover.
- [ ] Delete dead IsDuplicate (fold into #666 if it lands first).

## Acceptance
Two replicas processing the same delivery cannot both apply effects; losing Redis degrades to slower processing,
never to replayed money effects.

---

# #663: hard-cut the NMI rail to the modern v5 JSON API (classic `transact.php` dies; `query.php` stays for transaction search only)

**Completed:** no — ACTIVE (promoted from future.md 2026-07-01). Paul's call: switch now, delete the old
API entirely, keep the old API only where genuinely necessary.

## Decision (2026-07-01): hard cut, not incremental
The future.md plan was incremental behind a per-account `api_version` toggle with classic retained as a
fallback. SUPERSEDED: delete the classic Direct Post (`transact.php`) path outright — every mutation and
every read moves to v5 JSON, EXCEPT transaction search, which has no modern merchant-credential equivalent
(see below) — `query.php` stays for exactly that. No toggle, no coexistence. Cutover gate is the live
sandbox E2E, not a runtime fallback.

## Auth — RESOLVED: same key, new transport, NO new secret
NMI's Classic API Migration Playbook, verbatim: "No need to generate new API keys or alter permissions,
both are inherited by the new endpoints." So the portal's plain "API key" (the classic security key — our
existing `security_key` secret) auths v5 too. Transport changes: v5 wants it as the ENTIRE `Authorization`
header value — "do not provide `Bearer`, `ApiKey`, or any other scheme". The portal's other keys are not
ours to use here: the **v4 key** auths only `/api/v4/*` (partner/merchant-management surface we don't
touch), the **checkout key** is the PUBLIC client-side key (Payment Component / hosted checkout).
`query.php` keeps taking `security_key` in the request body as today.

## Endpoint map (verified 2026-07-01 vs docs.nmi.com llms.txt index + v5 reference pages)
| openrails op (classic) | v5 |
|---|---|
| sale / auth / validate / credit (new-money ops) | `POST /api/v5/payments/{sale,auth,validate,credit}` (top-level) |
| capture / refund / void (ops ON a payment) | `POST /api/v5/payments/{id}/{capture,refund,void}` (id in PATH) |
| get one transaction | `GET /api/v5/payments/{id}` |
| Customer Vault add / update / delete | `POST` / `PATCH` / `DELETE /api/v5/customers[/{id}]` (+ billing/shipping sub-resources) |
| vault roster (reconcile `report_type=customer_vault`) | `GET /api/v5/customers` (cursor pagination) |
| recurring add / update / delete | `POST` / `PATCH` / `DELETE /api/v5/subscriptions` ("custom subscription" supported — keep our no-plan model; `/api/v5/plans` exists if ever wanted) |
| recurring roster (reconcile `report_type=recurring`) | `GET /api/v5/subscriptions` |
| recurring liveness by id (`GetRecurringLiveness`) | `GET /api/v5/subscriptions/{id}` |
| Collect.js token in sale | `payment_details.payment_token` in JSON body (was flat `payment_token` field); client flow unchanged |
| v5 refund vs credit | distinct ops, same as classic: refund = settled txn back to original method; credit = standalone |

## `query.php` survives for transaction SEARCH only (researched, not assumed)
- v5 payments has NO list/search — the complete v5 payments surface is the 7 ops + `GET /payments/{id}`
  (get by KNOWN id). Every other v5 resource (customers, subscriptions, plans, invoices, products,
  devices) has a List endpoint; payments does not (raw llms.txt grep, not a summarizer).
- v4 HAS `POST /api/v4/transactions/reports` but it is PARTNER-key-only ("Your v4 API key that was
  generated in the Partner portal", "transactions by merchants under your partner account") — that's the
  reseller's (MobiusPay's) credential class, not our merchant one.
- The two jobs that therefore stay on `query.php`:
  1. reconcile's date-ranged bulk transaction pull, declines included (`internal/reconcile/nmi.go`
     `report_type=transaction`);
  2. order-id evidence probes (`internal/integrations/nmi/liveness.go` `ProbeSalesByOrderID` /
     `FindSuccessfulSaleByOrderID` — #664/#665 machinery; get-by-id is useless when the tx id is the unknown).
- Re-check for a v5 payments-list/search endpoint at each future NMI touch — if it appears, `query.php` dies.

## Tasks
- [ ] `internal/integrations/nmi/client.go`: v5 JSON transport — base URL const beside the query URL,
      bare-security-key `Authorization` header, JSON encode/decode, error mapping (modern error shape ≠
      classic `response`/`responsetext` — map at the client boundary so callers keep their semantics),
      preserve timeout + read-only guards. DELETE the Direct Post `transact.php` path and form encoding.
- [ ] Port mutations: sale (vaulted-customer charge + Collect.js `payment_details.payment_token`),
      auth/capture, refund/void/credit, validate; Customer Vault add/update/delete → `/customers`;
      recurring add/delete → `/subscriptions` (custom subscription, no-plan model preserved).
- [ ] Reconcile fetcher (`internal/reconcile/nmi.go`): recurring + customer_vault reports → v5 lists with
      cursor pagination; transaction pull stays `query.php`; re-map parsed fields (v5 JSON names ≠
      query.php XML names) and assert parity against sandbox data.
- [ ] `liveness.go`: `GetRecurringLiveness` → `GET /api/v5/subscriptions/{id}`; order-id probes stay.
- [ ] Webhooks: expected unchanged (portal-configured, payload independent of which API created the txn) —
      verify on sandbox; touch `internal/modules/webhooks/nmi.go` only if that's wrong.
- [ ] Tests: rewrite NMI fixtures to JSON shapes; full live sandbox Go E2E green end-to-end.

## Out of scope
- The entire v4 surface (partner/onboarding/processor/fee config).
- Client-side Collect.js / Payment Component changes (none needed).

Acceptance: zero `transact.php` references remain; the NMI rail runs sale/vault/recurring/liveness/reconcile
on v5 JSON with the existing `security_key`, `query.php` remains ONLY for the two transaction-search jobs,
and the live sandbox E2E passes end-to-end.

---

# #666: dead-code purge — ~7k lines of unreachable features and orphaned surface

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 whole-repo audit (two independent sweeps agreed on the
big items; every claim below was deadcode- AND grep-verified, including `//go:build integration` files).

## Metadata
- Category: cleanup
- Status: planned
- Passes: false

## Problem

Several whole subsystems are wired-looking but unreachable, and a long tail of exported surface has zero
non-test callers. Ranked, biggest first:

1. **USDC funding (Coinbase onramp) is dead-in-the-water** (~1,400 lines): `funding.Service.Create/Get` have NO
   route/export callers, so sessions can never be created — the wired Coinbase webhook branch
   (`internal/http/handlers/webhook.go:650`) can only ever hit ErrSessionNotFound. Cut module + repo +
   usdc_funding_sessions queries/models/DDL + `USDCFundingConfig` + `ufs_` id helpers. Supersedes/parks #328
   (keep the design notes there; the checkout `usdc_funding` error metadata is separate and stays).
2. **Abuse blocklist + velocity guard never consulted** (~550): `IsBlocked`/`VelocityGuard.*` have zero callers;
   blocklist.go's own doc says "deny wiring intentionally NOT built here". Cut blocklist.go, velocity.go,
   payment_blocklist queries/table. (CardAbuseGuard + WastedSpendGuard ARE wired — keep.)
3. **11 unregistered `Service*` handlers** (~650): `ServiceAdmit` (single), `ServiceGetBudget`,
   `ServiceAbuseUsage`, `ServiceSetPayerSpendLimits`, `ServiceSetTierSchedule`,
   `ServiceGet/SetMerchantConfiguration`, `ServiceSet/GetInvokerSpendLimits`,
   `ServiceSet/GetCreditAccountSettings`, `ServiceListCustomerCreditTransactions`, `ServiceWithdrawCredits`,
   `ServiceLookupCreditTransaction` — none in any route table (`internal/http/routes/routes.go:184-231` is the
   full service surface); embed/client.go has its own copies.
4. **`internal/modules/webhooks/replay`** (~800 incl. tests): zero importers outside its own test.
5. **6 unregistered Admin handlers** (~450): AdminReconcileProduct/Price, AdminListCatalogOrphans,
   AdminListStripeOrphans, GetAdminManualRebillAttempts, DeleteAdminUserPaymentMethod.
6. **Health circuit-breaker gates nothing** (~300): nothing calls `IsAvailable`; only `/ready` reads
   `GetServiceHealth`. Replace `internal/services/health` with ~25 lines of on-demand pg/redis Ping in the
   readiness handler.
7. **Dead ledger surface** (~270): `grants.Ledger.{Expire,Materialize,RevokeGrant,LiveGrants,RevokeBySource,
   SpendableLots}`, `money/ledger.Ledger.{Conservation,Spend,Expire}`. NOTE deadcode misses embed roots:
   `GrantAdmin`/`AdminGrantExists` ARE live via `pkg/embedded/admin_grants.go:94` — keep.
8. **Legacy embedded-mux path in gin Server** (~360): `Server.NewHTTPHandler`/`newHTTPHandlerMux`/
   `HTTPHandlerOptions` duplicate `embedhttp.Assembler`; kept alive only by embedded_mux_test.go.
9. **`RegisterHostWebhookRoutes` + `HostWebhook`** (~120): no caller; embedded hosts use
   `RegisterMerchantWebhookRoutes`.
10. **Dead CCBillVersionedPayload interface + 13 Get* method blocks** (~100, `internal/modules/webhooks/types.go:368`).
11. **CrossMerchant analytics** (~100): five `*CrossMerchant` wrappers with zero callers ("TODO(#232)");
    deleting them collapses the `crossMerchant bool` threaded through merchantFilter + 10 query helpers.
12. **Misc verified-dead tail** (~600): `fx.NoOpProvider`; `solana/subscriptions.BuildResumeSubscription`;
    solana `ValidateRecurringMint`/`IsTerminalFailure`/`GetTokenBySymbol`/`IsValidToken`/
    `FiatMicrosToStablecoinBaseUnits`/bare `NewPlanService`; `moneyutil.MicrosToCentsCeil`/`ParseDecimalToMicros`;
    webhookutil delegation re-exports (`ParseStripeSignatureHeader`/`ParseNMISignatureHeader` — call sigverify
    directly); `cache.CacheMiddleware`/`GenerateKey`; `controlplane.Catalog`/`CatalogNames`/
    `MerchantOwnerRolePermissions`; `RateLimitStore.Reset`; `MoneyService.snapshotTx`; `money.FormatAmount`;
    `webhooks.IsExpired`; `spendgate.Gate.SetClock`; `checkout.SetSolanaLifecycleForTest`; dead ginmw
    (`CORS`, most of service_credential.go ~193, `UserSessionAdminPrincipalRequired`, `RateLimit`,
    `InternalIPWhitelist`, `WebhookIPWhitelist` — ~350, or fold into #670); most of
    `internal/bootstrap/merchant_env.go` (keep `MerchantBillingEnvKey`); sqlc `SetSubscriptionUnknown`
    (the ONLY orphan among 419 gen methods — but check #664/#665 don't claim it first);
    `internal/http/response` wrapper pkg (~40 — every func is one `api.SimpleErrorResponse` call; the
    "two error envelopes" unification is already de-facto done, this is the residue);
    `pkg/authprovider` re-export shim (8 importers, s/authprovider/billingauth/);
    `derefStr` defined identically in 3 files; `PaymentsIdempotencyAdapter` + checkout's duplicate
    IdempotencyStatus/IdempotencyRecord types (~60 — have checkout consume idempotency's types).

Also (yagni, do opportunistically while in the files): single-impl single-injection interfaces in
subscriptions (`user_admin_support.go` NotificationStore/NotificationEmailSender/AdminCancellationLogger/
LifecycleEventLogger, `merchantConfigurationReader`, StripeLivenessProber, StripeSubscriptionLister/
StripeChargeLister) — take the concrete types. And the entitlements repo facade is 20-40 pure one-line
forwards; the de-facto house convention (money/grants, the newest audited code) is module→gen directly —
collapse pure-forwarding facades where they add nothing.

## Tasks
- [x] Item 1 DONE 2026-07-01: USDC funding subsystem deleted (module, repo/models/queries/gen, webhook branch +
      Hook0 verifier, config + env mapping + example yaml, ufs_ ids, RLS-test refs; table dropped via migration
      054 and removed from 001 baseline). #328 closed to completed.md; intent re-filed as future.md #680.
- [ ] Item 2: delete the abuse blocklist/velocity subsystem + its tables/queries (integration tests go with it).
- [ ] Items 3-5, 8-9: delete unrouted handlers + dead mux/mount paths.
- [ ] Item 6: replace health manager with on-demand pings in `/ready`.
- [ ] Items 7, 10-12: dead-surface tail sweep.
- [ ] Re-run deadcode + full integration suite green after each batch.

## Acceptance
`deadcode ./cmd/openrails` (cross-checked against integration-tagged files and pkg/embedded roots) reports no
production-dead symbols in the listed areas; no route-table or behavior change; net ≈ -7k lines.

---

# #667: production gate: payment-provider secrets must not persist plaintext at rest

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 security audit (MEDIUM; highest-value security fix found —
the rest of the audit came back clean: sig-verify, RLS/GUC, merchant binding, IDOR, crypto, SQL all verified).

## Metadata
- Category: security
- Status: planned
- Passes: false

## Problem
`buildDBSecretStore` (`internal/merchantsecrets/store.go:158-185`): when `ENCRYPTION_MASTER_KEY` is unset,
`crypto.NewEncryptor` returns a disabled encryptor and the DB secret store persists PLAINTEXT. Only Solana
private keys are write-restricted; Stripe secret keys, NMI security keys, CCBill credentials, and webhook
signing secrets all write cleartext into `openrails.merchant_secrets`. A DB dump/backup/replica leak exposes
every merchant's live provider credentials. There is a prod fail-closed gate for RLS posture
(`internal/db/rls.go:59` refuses BYPASSRLS) but no equivalent for encryption — the Solana-only restriction
shows the asymmetry is an oversight, not a decision.

Posture (Paul, 2026-07-01): production normally uses Vault as the merchant-secret store; the DB-backed store
is a FALLBACK. Scope accordingly — no new encryption machinery, just: whenever the DB store is the one storing
secrets, the master key must be set. Vault-backed deployments are unaffected.

## Tasks
- [ ] Fail closed in non-dev: DB-backed secret store selected + disabled encryptor ⇒ refuse boot (mirror the
      EnforceRLSPosture pattern). Vault-backed store: no change.
- [ ] Keep dev/test ergonomics: explicit dev-mode escape hatch, loudly logged.
- [ ] Integration test: prod-ish config with DB store and no master key refuses boot / cannot write a
      Stripe/NMI/CCBill credential.

## Acceptance
A production deployment cannot silently store any payment-provider credential or webhook secret in cleartext;
dev keeps working with an explicit opt-out.

---

# #668: CCBill webhook auth bypassed by global test_mode (+ webhook trust-flag hygiene)

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 security audit (MEDIUM + two LOW hardening nits).

## Metadata
- Category: security
- Status: planned
- Passes: false

## Problem
CCBill has no HMAC — the source-IP allowlist is its ONLY authentication. `internal/http/handlers/webhook.go:106-116`
(also :184, :302) skips the IP check whenever global `isTestMode` is true. TestMode is orthogonal to
live-credential Mode (#355), so a deployment with REAL CCBill credentials + `test_mode=true` accepts forged
`NewSaleSuccess`/`RenewalSuccess` from ANY IP — entitlement/credit fabrication. (Replays of a seen
transactionId are deduped; forged NEW transactions are not. Client IP correctly uses RemoteAddr, not XFF.)

Nits from the same audit:
- CCBill events are stamped `SignatureValid=true` (webhook.go:479,487) despite having no signature — nothing
  gates on it today (NMI/Stripe fail closed at dispatcher.go:135,179) but a future refactor could over-trust it.
- `internal/db/db_pgx.go:210-218`: session-GUC reset error silently dropped on pool release. Self-healing
  (GUC re-set before every use; RLS fails closed without GUC) but a failed reset should at least log.

## Tasks
- [ ] Bind the CCBill IP-check bypass to the CCBill account's sandbox posture (or refuse test_mode when a live
      CCBill account is configured) — never the global test_mode flag.
- [ ] Stamp CCBill events `SignatureValid=false` (or a distinct `ip_allowlist` verification kind).
- [ ] Log GUC-reset failure in `release()` (or use tx-scoped set_config).
- [ ] Test: live-mode CCBill + test_mode=true still rejects webhooks from non-CCBill IPs.

## Acceptance
No configuration accepts CCBill webhooks from arbitrary IPs while live credentials are configured; the
SignatureValid flag never claims verification that didn't happen.

---

# #669: rail capability descriptor registry — collapse the 36-point dispatch scatter

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 architecture audit. The single biggest extensibility tax
found; everything else structural came back healthy (standalone/embedded share routes+services; sqlc fully
consumed; suspected-dead dirs all alive).

## Metadata
- Category: architecture
- Status: planned
- Passes: false

## Problem
The four rails have four shapes (stripeapi choke-point pkg, NMIClient struct, CCBillClient struct, loose Solana
parts) and the only shared interface is reconcile's read-only `RailFetcher` (`internal/reconcile/reconcile.go:199`).
Everything else is hardcoded switch/if-chains: webhook ingress routing (3 layers: webhook.go:99,:175 +
service_webhooks.go:42), `railHasRemoteCustomer` (rail_customer_service.go:72), credential-key switches
(payment_provider_config.go:365,374 + secrets.go:134), ChargeSavedMethod blacklist (money/collection.go:75),
`subscriptionProviderAutoBilled` (jobs_dunning.go:660), checkout confirm (session_service.go:499),
unknown_orchestration.go:58,68, display names (email_service.go:582), build_runtime wiring. Adding rail #5 =
~25 files, 36+ dispatch points, zero compile-time help finding them.

## Target
NOT a fat uniform PaymentAdapter (rails genuinely differ — vault ownership asymmetry is real). A per-rail
capability DESCRIPTOR registry: one struct per rail declaring HasRemoteCustomer, AutoBilled,
SupportsChargeSavedMethod, DisplayName, CredentialKeys, sandbox-posture (feeds #668), plus function fields for
webhook enqueue/handle. Collapses ~80% of the switches (all boolean predicates + credential keys + display
names + webhook routing) into one file per rail; "did I miss a switch?" becomes "fill in the struct".
Overlaps #291 (processor-capability-metadata) — this IS #291's capability model, scoped to the dispatch
points that exist today; fold #291's checkout-validation/routing ambitions in later, don't build them here.

## Tasks
- [ ] Inventory pass: enumerate every rail switch (list above is the audit's; re-verify).
- [ ] Define the descriptor struct + registry; port the boolean predicates first (remote-customer, auto-billed,
      collection exclusion), then credential keys + display names, then the 3-layer webhook dispatch.
- [ ] Compile-time completeness: constructing the registry requires every field per rail.
- [ ] Leave RailFetcher as-is (it already has the right shape); reconcile switches route through the registry.

## Acceptance
Adding a rail touches: the enum, its integrations package, ONE descriptor file, build_runtime wiring. The
boolean-predicate/credential/display/webhook-routing switches are gone; grep for `case "stripe"`-style rail
switches outside the registry returns ~zero.

---

# #670: single HTTP stack — drop the gin layer, serve the neutral mux everywhere

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 audit. Do AFTER #666 (which already deletes ~350 lines of
production-dead ginmw).

## Metadata
- Category: refactor
- Status: planned
- Passes: false

## Problem
Two complete HTTP stacks: standalone serves gin (`cmd/openrails/main.go:204` → `pkg/embedded/gin` →
`internal/http/server.go`), embedded serves net/http (`internal/http/embedhttp`). Route registration is ALREADY
framework-neutral (`internal/http/routes/routes.go` via `router.Router`, #282) and even the gin engine is
wrapped by the NEUTRAL middleware (`pkg/embedded/gin/self.go:98` calls ChainHTTP/SecurityHeadersHTTP/CORSHTTP/
RateLimitHTTP). The gin residue is pure duplication that is already rotting (several ginmw middlewares are
production-dead — see #666): ginmw (1,113), ginreq, ginrouter, ginroutes, ginboot, controlplane/ginroutes, gin
half of request.go, gin Server (~300 of server.go), pkg/embedded/gin (433), response.go. ~1,800 prod +
~1,300 test lines, plus gin (+ deps) out of go.mod.

## Tasks
- [ ] Standalone serves the embedhttp/neutral mux; gin hosts keep working via `gin.WrapH` (pkg/embedded/gin
      shrinks to a WrapH shim or dies — cozy-art is the embedder to check).
- [ ] Port the few live gin-only middlewares to the neutral chain; delete the gin packages.
- [ ] Remove gin from go.mod (root deps_test.go already forbids it in the client package).
- [ ] Full integration suite + one embedded-host smoke (cozy-art) green.

## Acceptance
One HTTP stack; gin absent from go.mod (or vestigially present only in a WrapH shim if an embedder demands it);
no route/middleware behavior change on either deployment mode.

---

# #665: consolidate the lapsed-subscription machinery — one probe, one decider, one freshness signal

**Completed:** no
**Status:** PLANNED (2026-07-01) — architecture follow-up to #664, from the 2026-07-01 review of the whole
reconcile/converge system. #664 (evidence-gated lifecycle) ships FIRST and stands alone; this issue deletes the
accumulated duplication afterward.

## Metadata
- Category: reconciliation
- Status: planned
- Passes: false

## Problem

Four mechanisms from three eras all answer "this subscription's period lapsed with no signal — now what?", and
none was deleted when its successor landed:

1. **Subscription liveness** (#367, `jobs_subscription_liveness.go`) — per-sub provider probe with its own outcome
   machine (repaired / failed / adopted / cancelled / skipped), run as a lane inside provider refresh.
2. **Legacy #107 diff engine** (`engine.go` + `diff.go`, ~2,000 lines + fetchers) — full-snapshot pull, findings,
   appliers, mutation policy, circuit breaker. Also emits `derive.*` and `consistency.*` finding types
   (`findings.go`), violating the spec's single-writer-per-invariant rule (consistency-invariants.md §7).
3. **LIFE `period_overdue` → `grace_exhausted`** (#511, `converge_passes.go`) — rail-heuristic cohort split, acts
   without evidence (the #664 bug).
4. **`needs_verification` → `unknown` → `unknown_reconcile`** (#632/#633) — park + targeted pull, with a SECOND
   per-sub verification outcome machine duplicating (1).

Cohort exclusivity hangs on WHERE-clause complements spread across three SQL files and is NOT actually exclusive:
a vaulted NMI row with a lapsed period is in BOTH `ListSilentLapsedSubscriptions` AND
`ListPeriodOverdueSubscriptions`; which mechanism wins is a scheduling race (liveness runs inside provider
refresh, but converge sweep + inline converge run the LIFE plane with no freshness precondition). On doujins the
evidence-fetching lane never ran while the guessing lane did — that race IS the #664 incident.

Spec/code drift, same symptom: §8's `held`/`indeterminate` finding states don't exist in code (converge writes
`reconcile_required`/`requires_review`); the §3.2 confirmed-absence gate's `reconciliation_state` flag is
merchant-global, manual, and has exactly one writer (`pkg/embedded/pull_provider.go` CLI) — nothing operational
sets it.

## Target — finish the consolidation the spec already demands (§7/§10)

- **One per-subscription provider-verification primitive.** Extract the liveness worker's probe + outcome machine
  ("entitlements extend only through a real charge"; "period adoption alone never grants access" — that doctrine
  is correct and becomes THE implementation); port `unknown_reconcile` onto it. One outcome set.
- **One lifecycle decider.** Post-#664, LIFE routes every lapsed row (evidence → dunning, none → `unknown`), so
  the silent-lapsed cohort IS the unknown cohort: delete `ListSilentLapsedSubscriptions` + the liveness lane.
- **One freshness signal.** `provider_refresh_watermarks` (per merchant+provider+account, never advances on
  failure) drives both the #664 evidence predicate AND the confirmed-absence gate; retire the manual
  `reconciliation_state` flag or scope it strictly to bulk-import mode.
- **Strip the legacy engine to a mirror-writer** (fetch snapshot → upsert mirror rows). It keeps: read-only
  fetchers, the coverage contract, the circuit breaker, mutation policy — as guards on MIRROR writes. It loses:
  emission of `derive.*`/`consistency.*` findings (converge passes own those invariants) and appliers that
  duplicate converge repairs. `pull.*` findings remain its only vocabulary.
- **Reconcile spec ↔ code on finding states** (`held`/`indeterminate` vs `reconcile_required`/`requires_review`)
  — pick one, update the other.

## Tasks

- [ ] Extract the verify-subscription primitive from `jobs_subscription_liveness.go`; port
      `unknown_reconcile.go` onto it (one probe, one outcome set).
- [ ] Delete the silent-lapsed lane (`ListSilentLapsedSubscriptions` + the liveness cohort scan) once #664 routes
      the cohort through LIFE.
- [ ] Single writer per invariant: stop emitting `derive.*`/`consistency.*` from the pull engine; move any check
      not already covered by a converge pass into one.
- [ ] Mirror-writer refactor of `engine.go`/`diff.go`; `pull.*` findings only.
- [ ] Watermark-derived confirmed-absence gate; retire or import-scope `reconciliation_state`.
- [ ] Spec/code finding-state reconciliation (docs/consistency-invariants.md §8).

## Acceptance

Exactly ONE implementation each of: (a) per-subscription provider verification, (b) lapsed-cohort routing,
(c) mirror-freshness attestation. No plane emits another plane's finding types. Scheduler ordering cannot change
outcomes — an evidence-less pass can only park (the #664 property, now structural). Net code deletion in
`internal/reconcile` + the river lanes, not addition.

---

# #662: deterministic natural-key UUIDs for products, prices, provider accounts (replace random NewV7)

**Completed:** no
**Status:** PLANNED (2026-07-01) — not started. Design settled: derive each entity's uuid PK deterministically
(uuidv5) from its IMMUTABLE natural key instead of `uuidutil.NewV7()`. Keep the uuid column type — this is
"encode the natural key as a uuid", NOT a text-PK conversion (that was evaluated and rejected, see Out of scope).

## Metadata
- Category: catalog
- Status: planned
- Passes: false

## Problem
Product, price, and provider-account primary keys are random UUIDv7 (`uuidutil.NewV7()`), disconnected from each
entity's natural key. Consequences:
- The same logical product/price/account gets a DIFFERENT uuid in every environment and every fresh DB rebuild.
- Seeding is idempotent only WITHIN one DB (via get-or-create by the natural key), never reproducible across DBs.
- The id can't be computed without a DB lookup — e.g. doujins legacy-migrate must look up the shared premium price
  id (`resolveSharedLegacyPremiumPrice`) rather than compute it and stamp it on 40k subscriptions.
- The uuid is a second, arbitrary identity when a deterministic one derivable from the natural key is strictly
  better and costs nothing extra.

All three entities ALREADY have an immutable natural key with a unique constraint:
- products: `UNIQUE (merchant_id, key)`; `key` immutable by contract (changing it = a new product).
- prices: `unique_prices_product_amount_window (product_id, amount, currency, access_duration_hours, auto_renew,
  trial_unit_amount, trial_duration_hours)`; prices are immutable on financial fields — a reprice creates a new row
  and archives the old (`PriceService.Update` errors "prices are immutable"; converge re-mints via
  `matchPrice`/`PriceCreate`/`PriceArchive`).
- payment_provider_accounts: natural key `(rail, environment, account_id)`, global.

## Target design
Add `uuidutil.DeterministicID(namespace uuid.UUID, parts ...string) uuid.UUID` = uuidv5 over a canonical,
injective encoding of `parts` (length-prefixed join, so no delimiter-collision — `account_id` contains `/`), under
ONE fixed package-level namespace constant that must never change (changing it re-mints every id).

Mint catalog/provider ids from the immutable natural key:
- `product.id  = DeterministicID(ns, merchant_id, key)`
- `price.id    = DeterministicID(ns, product_id, amount, currency, access_duration_hours, auto_renew,
  trial_unit_amount, trial_duration_hours)` — exactly the `unique_prices_product_amount_window` columns.
- `provider.id = DeterministicID(ns, rail, environment, account_id)`

EXCLUDE every mutable field from the derivation — `rails` (rotated in place by `UpdateRails`), `status`,
`display_name`, timestamps. Deriving from a mutable field would orphan FK references when it changes. The uuid
becomes a content-hash of the entity's frozen identity: same key → same id everywhere; a change to identity (a
reprice) correctly hashes to a NEW id while the old row (still referenced by subscriptions/payments) is untouched;
collisions are impossible because the unique constraint already forbids two rows with the same tuple.

## Tasks
- [ ] Add `uuidutil.DeterministicID(namespace, parts...)` (uuidv5; canonical length-prefixed encoding; document that
      the namespace constant is permanent). Unit-test injectivity + stability.
- [ ] Product create path: replace `NewV7` with `DeterministicID(ns, merchant_id, key)`. Now that the id is
      computable, the create can be `ON CONFLICT (id) DO NOTHING/UPDATE` (simpler than the GetProductByKey-then-Create
      dance) — confirm re-seed stays idempotent.
- [ ] Price create path (`pkg/service/service_definition_catalog.go:490`): replace `NewV7` with `DeterministicID`
      over the frozen tuple. Canonicalize each field the SAME way the unique key / `matchPrice` compares them
      (amount as int64, currency lowercased, nullable trial fields → a stable sentinel) so equal prices always hash
      equal and never violate `unique_prices_product_amount_window`.
- [ ] Provider-account create/upsert path: replace `NewV7` with `DeterministicID(ns, rail, environment, account_id)`,
      canonicalized to match the `(rail, environment, account_id)` unique index (`lower(rail)` etc.).
- [ ] Structurally enforce price money-immutability: split the raw sqlc `UpdatePrice` (`internal/db/queries/prices.sql`)
      into status-only + rails-only queries so the money/identity columns are not settable at the DB layer (today they
      are immutable only by caller convention — `PriceService.Update` blocks it, but the query text can still SET them).
- [ ] Tests: (a) same natural key → same id across two fresh seeds; (b) reprice → new id, old row archived/untouched;
      (c) provider-account re-import → same id; (d) product re-seed → same id; (e) mutating a mutable field
      (`rails`/`status`/`display_name`) does NOT change the id.
- [ ] Rollout note: NO schema change (uuid type unchanged; FKs/RLS/sqlc untouched). Existing DBs keep their random
      ids — get-or-create won't re-mint — so adoption is via fresh seed (doujins is pre-cutover / re-seeding, so it
      picks up deterministic ids with no backfill). A prod backfill to deterministic ids is a SEPARATE, explicitly
      scoped migration (rewrite FKs) and is NOT in this issue.

## Out of scope (named and rejected)
- Converting any PK to a text or composite natural key. Natural keys here are composite (products `(merchant_id,
  key)`; provider `(rail, environment, account_id)`; price 7-col) — a text/composite PK propagates into ~25 child FK
  columns + every index + RLS join + sqlc type, `account_id` carries a `/`, and it breaks openrails' uniform
  single-column-uuid keying. A deterministic uuid gives the SAME identity semantics at ~zero blast radius.
- Adding a price `lookup_key`. Unnecessary: prices are immutable, so the frozen attribute tuple is already the
  natural key.
- Including `rails` (or any mutable field) in the derivation — would orphan references when a provider link rotates.
- Backfilling deterministic ids onto existing prod rows (separate migration if ever wanted).

Acceptance: product/price/provider-account ids are a pure, stable function of each entity's immutable natural key —
identical across environments and fresh rebuilds, computable without a DB read; a reprice mints a new id (old row
archived, untouched); rotating a provider link (`rails`) or flipping `status` leaves the id unchanged; no
FK/RLS/sqlc/type churn; the raw `UpdatePrice` can no longer SET money/identity columns.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---


# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, non-archived provider-account eligibility, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, archived-account exclusion, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

---

