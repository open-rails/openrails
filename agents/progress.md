<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 688

---

# #686: decompose the subscriptions module along the #669 registry seam

**Completed:** no
**Status:** IN_PROGRESS (2026-07-01, approved by Paul) — rail-specific lifecycle behavior moves out of giant
per-rail branches inside internal/modules/subscriptions and into the #669 rail descriptor registry (function
fields), leaving lifecycle orchestration rail-agnostic.

## Metadata
- Category: architecture
- Status: in_progress
- Passes: false

## Problem
internal/modules/subscriptions (~9k lines) carries rail-conditional behavior inline: lifecycle
create/renew/fail/cancel paths branch per rail (NMI remote-delete scheduling, CCBill
provider-auto-billed/no-retry semantics, Stripe-specific sources), which means every new rail edits the
module's core files and every reader must hold all four rails in their head at once. #669 built the registry
(predicates + metadata already collapsed); behavior hooks are the remaining half.

## Inventory (2026-07-01, every rail-conditional branch in internal/modules/subscriptions, non-test)
(a) = existing/new registry consult, (b) = new descriptor field, (c) = genuinely inline (justification given).
1. cancel_mode.go:89-104 CancelModeFor rail switch (stripe reversible / ccbill external-portal / nmi
   state-conditional via nmiDeletePending / default destructive) → (b) descriptor func field `CancelMode`
   (CancelMode type + constants move to rails; subscriptions keeps type/const ALIASES so the exported surface —
   used by handlers/subscription_lifecycle.go:162 + pkg/service/service_user.go:1058 — is signature-stable).
2. cancel_mode.go:177-183 CancelPortalURL (hardcoded CCBill support URL) → (b) descriptor metadata field
   `CancelPortalURL string` ("" = none).
3. lifecycle_service.go:1574 ResolveCancelledRemoteAlive → `rails.IsNMI && RailSubscriptionID != ""` gates the
   deferred NMI delete queue → (b) descriptor bool `RemoteDeleteOnTerminalCancel` (nmi true, all others false).
   deferDelete!=nil gating + #679 queue-always + marker/intent atomicity UNTOUCHED — only the rail-name test
   moves to the registry.
4. lifecycle_service.go:1932 FailMembership terminal cancel, same `rails.IsNMI` gate → same (b) predicate.
5. user_service.go:479 CCBill guard in CancelUserSubscription (portal error) → (a) consult the new registry
   cancel mode (external_portal ⇒ CCBillCancelError, URL from descriptor); message text preserved.
6. user_service.go:487-530 NMI-only user-cancel executor (deferred/immediate rail-side delete, NMI clients) →
   (c) single-rail machinery needing injected NMIClients; a hook would have exactly one implementer and drag
   client deps into the dependency-free descriptor.
7. admin_service.go:180 cancelWithNMI guard → (c) NMI-only helper's own early return, not orchestration.
8. admin_service.go:218-235 admin cancel executor switch (NMI client delete / StripeService cancel / others
   rejected) → (c) per-rail EXECUTORS over injected clients; #669 deliberately left executors out of the
   descriptor (dependency-free metadata), no second dependency-free implementer exists.
9. lifecycle_service.go:1402 Solana cancel cascade inside ApplyLocalCancellation (#264 cranker stop) → (c)
   single-rail machinery with one implementer requiring db/repo handles; a hook inverts layering (rails would
   import internal/db/repo) for zero orchestration gain.
10. email_service.go:244 `isSolana` one-off receipt copy (keyed on a free-string payment method, not
    models.Rail) → (c) content templating for one rail; no orchestration.
11. dunning.go ClassifyNMIDecline + hardDeclineNMICodes → (c) NMI decline taxonomy consumed only by the NMI
    dunning worker (river/jobs_dunning.go:479); no other rail has NMI response codes.
12. dunning.go:275 RenewalGraceEligibleRail → already (a), registry delegate since #669.
13. email_service.go:576 railDisplayName → already (a), registry delegate since #669.
14. lifecycle_service.go:157 + :1888 (RenewalGraceEligibleRail / rails.OpenRailsDrivenDunning consults) →
    already (a) registry-backed.
15. subscription.go:72-73 RailCCBill/RailStripe string consts → not branches; consumed by
    internal/http/handlers; left.
16. provider_account_clients.go:31 rails.SameRail → normalization, not per-rail dispatch; left.
Candidates from the brief verified ABSENT in this module: payment-method-update remote propagation (no such
code here); dunning/retry participation + renewal grace semantics already registry predicates.
Webhooks-surface check: internal/modules/webhooks consumes subscriptions.{Notification,Subscription,
SubscriptionLifecycle}Service, New*Service ctors, {Create,Renew,Cancel,Fail,Reactivate}MembershipParams,
IsTerminalTransitionBlocked, RemoveCancelledSubscriptionsForActivation, PremiumEndReason*, DunningInterval,
TerminalCancelReason; internal/app consumes DeferredDeleteScheduler + SetDeferredDeleteScheduler. NONE of these
change shape (CancelMode aliasing is additive); no webhooks-side edits required, nothing deferred.

## Target
- Registry descriptors gain lifecycle FUNCTION FIELDS for genuinely rail-divergent behavior (e.g. terminal-
  cancel remote cleanup, dunning/retry policy shape, renewal semantics) — behavior lives with the rail.
- lifecycle_service.go and friends become rail-agnostic orchestration: consult the descriptor, never switch on
  the rail string.
- Doctrine preserved EXACTLY: cancellation-last-resort / evidence-gating (#664), NMI remote delete only on the
  certainty path (converge side-effect deps stay nil), #679 circuit-breaker semantics untouched.

## Tasks
- [x] Inventory rail-conditional branches in internal/modules/subscriptions (grep rail switches/IsNMI/case
      strings); classify each: registry predicate (exists), new behavior hook, or genuinely-inline.
      → DONE, see Inventory above (16 sites: 3 new descriptor fields, 2 registry-consult ports, 6 justified
      inline, rest already registry-backed or not branches).
- [x] Add the behavior hooks to the #669 descriptors; port branches; keep exported call surfaces used by the
      webhooks module STABLE (concurrent work in webhooks).
      → Descriptor gains `RemoteDeleteOnTerminalCancel bool` (nmi only), `CancelMode func(sub, now) CancelMode`
      (type + reversible/destructive/external_portal constants moved to rails; NMI's is the issue-216
      state-conditional nmiCancelMode), `CancelPortalURL string` (ccbill support portal). subscriptions keeps
      CancelMode/constants as ALIASES + CancelModeFor/CancelPortalURL as registry delegates — handlers,
      pkg/service, webhooks untouched. lifecycle_service.go:1574/:1932 deferred-delete gates and
      user_service.go:479 external-portal guard consult the registry; nmiDeletePending deleted (lives in the
      nmi descriptor func). #664/#679 doctrine untouched: deferDelete nil-gating, queue-always,
      marker+intent same-tx atomicity all byte-identical; converge still constructs lifecycle with nil deps.
- [x] Registry completeness test extended to the new function fields.
      → TestRegistryCompleteness forces CancelMode non-nil per rail + portal-URL-implies-external-portal;
      TestRegistryPinnedFacts pins per-rail RemoteDeleteOnTerminalCancel + active-sub cancel mode + ccbill
      portal URL + NMI's pending/executed/lapsed cancel-mode window; TestLookupNormalizes pins unknown-rail
      destructive default + nil-sub destructive + empty portal URL.
- [ ] Existing subscriptions + webhooks + tests/ suites green as the behavior-preservation net.

## Acceptance
No rail-string switches remain in subscription lifecycle orchestration; adding a rail's lifecycle behavior =
filling descriptor fields; all existing suites green with no behavior change.

---

# #685: unify embedded and remote modes — one client, pluggable transport

**Completed:** no
**Status:** PLANNED (2026-07-01) — direction set by Paul: embedded mode is a KEPT product feature (removal
considered in the 2026-07-01 design review and REJECTED); the fix for dual-mode cost is further consolidation,
not removal. Successor in spirit to the deferred #338/#468 unified-SDK work; unblocked by #670 (one neutral
HTTP stack).

## Metadata
- Category: architecture
- Status: planned
- Passes: false

## Problem
The last real "two of everything" is `embed/client.go`: it hand-transcribes handler logic per method, while
`remote.go` implements the same SDK interface over HTTP. Parity is maintained socially (comments citing
"transcribes handlers.X") and by the conformance suite catching drift after the fact. A bug can exist in one
transport and not the other; every new endpoint is written twice.

## Target design
One client implementation, transport pluggable:
- `remote.go` stays THE client. Embedded mode becomes a custom `http.RoundTripper` that dispatches directly
  into the in-process neutral mux (#670's handler — the same one embedded hosts already mount), no socket.
- Two constructors: `Dial(url)` (network) and `Embed(app)` (in-process transport). Same types, errors,
  validation, auth, envelopes — by construction.
- Delete `embed/client.go`'s transcriptions; the conformance suite shrinks to a transport smoke test.
- `pkg/service`'s "dual-transport parity contract" role dissolves — handlers are the single surface; keep
  pkg/service only where it's a real facade, not a parity mirror.
- Cost accepted: one JSON round-trip per in-process call (noise for billing ops; buys production-identical
  request path for embedded hosts).

## What stays legitimately dual
Boot/infra ownership (`BootstrapOptions` injected pool; posture gates keyed on it describe a REAL difference).
cozy-art's browser surface is already unified (frontend → embedded HTTP routes with normal AuthKit bearers).

## Tasks
- [ ] In-process RoundTripper over the neutral handler (context propagation: merchant pinning + auth principal
      must traverse it identically to a real request).
- [ ] `Embed(app)` constructor on the root SDK; wire pkg/embedded to hand out the unified client.
- [ ] Migrate embed/client.go method-by-method onto it; delete transcriptions as they fall.
- [ ] Collapse conformance suite to transport smoke tests once both constructors share one implementation.
- [ ] Audit pkg/service for facade-vs-parity-mirror roles; retire the mirror half.
- [ ] Host adoption notes (cozy-art, doujins): constructor swap, no behavior change expected.

## Acceptance
One SDK implementation serves both modes; adding an endpoint touches handler + route + (generated/typed) client
once; embedded and remote cannot drift because there is nothing to drift between. Boot remains dual only where
infra ownership genuinely differs.

---

# #684: webhooks as wake-up signals — fetch-and-converge for fetchable rails (Stripe, NMI)

**Completed:** no
**Status:** PLANNED (2026-07-01) — from the 2026-07-01 design review; sibling of the rescoped #665 (they feed
the same decider — do them in sight of each other, #665's decider seam first or together).

## Metadata
- Category: architecture
- Status: planned
- Passes: false

## Problem
Webhook handlers APPLY the event payload to local state. Every ordering defense we built in #675 (row locks,
`stripe_last_event_created` watermarks, stale-event rejection, terminal-transition guards) exists because two
events about one subscription are two competing snapshots whose write order matters. That entire bug class is
structural to payload-apply and cannot be closed, only patched.

## Target design
For rails with a read API (Stripe; NMI v5), a verified webhook means only: "this object is dirty — fetch
current provider truth and converge to it." The event keeps four jobs: signature verification, dedup key,
identifying the dirty object(s), carrying the event timestamp. It stops being a state source.

- Ordering becomes meaningless: N events about one subscription (any order, duplicated, delayed) collapse to
  "fetch, converge" — idempotent by construction; replay = redundant fetch = no-op.
- Dirty-flag coalescing: burst of events about one sub ⇒ one fetch (debounce window), FEWER provider calls
  under dunning storms than per-event processing.
- Degrades safely: provider API down ⇒ row stays dirty, retry later, access intact (#664 posture). Fetch 404
  IS evidence (provider-confirmed dead) feeding the certainty ladder.
- Historical facts (decline codes, charge amounts) come from fetchable records (Stripe invoices/charges, NMI
  v5 payment queries), not event payloads — the payload only hints where to look.
- Stripe thin-event hydration already does fetch-on-event; classic snapshot events are the legacy shape. This
  issue makes fetch-first the ONLY shape for fetchable rails.
- CCBill stays payload-apply (nothing to fetch) — the documented exception; the #675 ordering machinery
  remains earning its keep there only.

## Read-after-write lag
A provider read API can briefly trail its own event. Converging to slightly-old truth is self-healing (next
event/pull converges again) and never corrupting; if a specific NMI read is known-laggy, gate on the event
timestamp (fetch until object updated_at >= event created, bounded retries) — same pattern as Solana
ReadUntilConsistent.

## Tasks
- [ ] Dirty-mark + coalesced fetch worker (River; per-subscription debounce; UniqueOpts on the sub id).
- [ ] Stripe: reduce snapshot-event handlers to dirty-marks; converge from fetched objects (reuse the
      thin-event hydration path + #665 decider seam). Keep payment-record writes fetch-sourced.
- [ ] NMI: same, via the v5 read surface (#663) + `unknown_probe.go` sources.
- [ ] Delete the then-dead payload-apply machinery for those rails (the #675 watermark/lock code retires where
      fetch-first makes it unreachable; keep for CCBill).
- [ ] Out-of-order/burst integration tests become trivial-by-construction — port the #675 ordering tests to
      assert convergence instead.

## Acceptance
For Stripe and NMI: no webhook handler writes subscription/payment state from an event payload; all state
writes flow through fetch → decider (#665). The #675 ordering tests pass with the ordering machinery deleted
for those rails. CCBill behavior unchanged.

---

# #681: scoped provider-credential lookups hardcode environment="live" — sandbox deployments can't resolve NMI/Stripe/Solana credentials

**Completed:** no
**Status:** PLANNED (2026-07-01) — surfaced by the #668 test-posture work. The CCBill instance of this bug was
FIXED (internal/modules/checkout/merchant_rail_secrets.go now derives via
`config.ExpectedProviderEnvironment(IsTestMode())`); the remaining rails still hardcode.

## Metadata
- Category: bug
- Status: planned
- Passes: false

## Problem
Under #355/#668 semantics, `test_mode=true` ⇒ provider-account catalog rows get `environment='test'`
(ValidateRailSet enforces consistency). But scoped credential lookups hardcode `environment="live"`:
- internal/http/handlers/checkout_session.go:143
- internal/modules/checkout/merchant_rail_secrets.go resolveNMIClient (NMI leg; CCBill leg fixed)
- internal/modules/vault/vault_service.go:268
- internal/merchants/credentials.go — LoadStripeCredentials / LoadNMIWebhookSigningSecret / LoadNMITokenizationConfig
- internal/modules/solana/recurring/wiring.go

A true sandbox deployment (test rows) can never resolve those rails' scoped credentials. The tests/ suite
masks it by seeding NMI as 'live'.

## Tasks
- [ ] Sweep all sites to derive environment from config (`ExpectedProviderEnvironment(IsTestMode())`) — some
      `merchants.Service` methods carry no config today, so signatures change; plumb, don't global.
- [ ] Un-mask the test fixtures: seed NMI/Stripe as 'test' under test_mode posture and keep the suite green.
- [ ] One regression test per rail: sandbox posture resolves scoped credentials.

## Acceptance
A test_mode deployment resolves every configured rail's scoped credentials from its 'test' catalog rows; no
lookup hardcodes an environment literal.

---

# #679: destructive provider-intent circuit breaker — mass NMI deletes must halt and ask

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01, uncommitted) — was PLANNED, from Paul's #664 post-mortem question: the
1,672 wrongful cancellations queued ZERO nmi_delete intents (lucky, see Findings), but the legitimate delete
path had no volume guard. NMI has no read-only keys, so a bug that mass-cancels through the REAL path
(FailMembership) would mass-delete real production subscriptions.

## Metadata
- Category: safety
- Status: implemented
- Passes: true

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

- [x] Queue-always for terminal cancels (Paul's model, 2026-07-01: queue the delete intent even without
      credentials/mode to execute — execution waits, the decision is durable): (a) `FailMembership` under
      `provider_write_mode=limited` currently SKIPS queuing the delete entirely (inconsistent — the dunning
      CHARGE path under limited materializes its intent "for the executor to drain when the mode allows");
      (b) unknown-resolution's stale-decline → cancelled path queues nothing though the remote may be alive.
      Both should durably record the desired delete unconditionally; mode/credentials/breaker govern execution
      only. Aligns with #674 (all external writes post a durable intent first).
- [x] Budget check in the provider-intent executor for destructive types, per (merchant, rail), rolling window;
      threshold `max(K, pct of active subs)` — constants, not config knobs, until someone needs tuning.
      (Implemented per-merchant across all destructive types — one budget + ONE standing finding per merchant;
      the only destructive type today is nmi_delete_subscription, so per-(merchant,rail) is currently identical.)
- [x] Breach → halt destructive execution for that merchant + OPERATOR finding (`life.provider_intent.held_bulk`
      or similar); non-destructive intents unaffected; explicit ack resumes.
- [x] Intents held by the breaker stay pending (never expire into `abandoned` while held).
- [x] Tests: N deletes under budget execute; N+1 halts + finding; ack resumes; other merchants unaffected.

## Progress

- 2026-07-01 IMPLEMENTED (uncommitted). What was built:
  - Queue-always: `FailMembership` no longer skips delete scheduling under `provider_write_mode=limited` —
    marker + nmi_delete intent are queued unconditionally (atomic in the cancellation tx); if no scheduler is
    wired it WARNs loudly. Execution was ALREADY executor-gated (`intents.GateExecution` parks system-origin
    under limited/readonly) — verified by test.
  - Unknown-resolution stale-decline gap: `UnknownVerdict.RemoteGone` (false only for the stale-decline cancel;
    true for roster cancelled/expired or absent-from-exhaustive-roster). Apply path maps Cancelled+!RemoteGone
    to new `ResolveCancelledRemoteAlive`, which cancels locally AND queues the deferred NMI delete
    (DeletionScheduledAt + intent, FailMembership shape). `jobs_provider_refresh.runUnknownReconcile` now
    injects the DeferredDeleteScheduler (embedded uses the same Runtime registration — no separate wiring).
    The production scheduler moved from internal/app to exported `intents.NMIDeleteScheduler` (app keeps a
    shim) so the reconcile path/tests construct the real thing.
  - Volume breaker: `intents.VolumeBreaker` in the executor, gating a destructive-types set
    (`nmi_delete_subscription`). Budget `max(25, 1% of merchant's active subs)` per rolling 24h (package
    consts). Executed count from the provider_intents ledger (executed_at, plus unresolved attempt statuses;
    in_flight excluded — batch claims would self-count). Over budget → intent PARKS (stays pending) + ONE
    `life.provider_intent.held_bulk` requires_review finding per merchant (stable subject_key
    `destructive_volume`, evidence: executed_count/budget/window_hours/active_subscriptions). Open finding =
    halted; operator ACK (fixed) resumes with the count window restarted at resolved_at; DISMISS (ignored)
    permanently silences the breaker for that merchant (findings upsert keeps ignored ignored — documented).
    `ExpireOverdueProviderIntents` now refuses to expire breaker-held destructive intents while the finding is
    open. New sqlc queries live in the intents query file (now rail_intents.sql after the concurrent
    provider_intents→rail_intents rename; NOT reconciliation.sql). Non-destructive intents untouched; the
    verifier (read-only) is not gated. NOTE: implemented concurrently with the rail-identity rename sweep
    (provider_intents→rail_intents etc.) — that agent adapted this code in place; #679 semantics unchanged.
  - Tests: verdict RemoteGone table cases (unit, green); breaker budget math (unit, green); integration —
    limited-mode FailMembership queues + executor parks under limited + drains under full
    (TestFailMembershipLimitedModeQueuesDeleteIntent), breaker end-to-end with dedicated merchants: 25 execute,
    26th halts + finding, open finding keeps halting, held intent survives ExpireOverdue, other merchant
    unaffected, ack resumes (TestBreakerHaltsBulkDestructiveExecution), stale-decline queues the delete /
    roster-gone queues nothing (TestReconcileUnknownCohort_StaleDeclineQueuesDeferredDelete). Full
    internal/intents integration package green; `go build ./...` + `go test ./...` green; targeted river
    integration (dunning materialize + provider refresh) green. Incidental rename-completion fixes made while
    validating (rail-identity sweep leftovers in files #679 owns): jobs_provider_refresh.go watermark upserts'
    `ON CONFLICT ON CONSTRAINT` now uses rail_refresh_watermarks_identity_key, and
    unknown_orchestration_integration_test.go finished the #682 half-edit (account_id column; NMI vault now
    asserts NO rail_customer_accounts row, matching the test's own #682 tail assertion).

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
**Status:** IN_PROGRESS (2026-07-01) — 1a/1b/1c/1d/1e/1g + the analytics siblings of 1f FIXED, with
wire-pinning unit tests; `go build ./...` + all touched-package tests green. DEFERRED: 1f storage row
(webhooks/stripe.go — #675 owns that file) and the NMI/Stripe/CCBill provider-boundary wire tests + boundary
struct retyping (collide with the in-flight #663 nmi-v5 rewrite; post-#663 task added below). Notable:
CalculateModelBUpgradeCharge output was generally NOT whole-cent micros (integer proration), so the fix
quantizes the unused credit UP to a whole cent inside the shared helper (customer-favored; preview==charge on
every rail) and the NMI RunSale seam converts via MicrosToCentsExact (errors on sub-cent, never rounds).
RefundPayload.AmountCents now really is cents (converted at prepare, 400 on sub-cent micros), which also makes
the verifier's cents-vs-cents comparison consistent by construction — no separate verifier edit was needed.
Original filing: from the 2026-07-01 money-path audit. Headline claims (1a, 1b, worker wiring)
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
- [x] Fix 1a/1b/1d with the existing exact converters (`MicrosToCentsExact`, `MicrosToMajorUnits`,
      `FiatMicrosToStablecoinBaseUnits`); fix 1c callers or retype CalculateTokenQuote to micros.
      → DONE: CalculateTokenQuote retyped to `moneyutil.Micros` (÷1e6, currency-independent; dead
      `currencyMinorUnits` deleted); `FiatCentsToStablecoinBaseUnits` deleted (zero production callers after
      the switch); transaction.go label bug → FormatMicrosDecimal.
- [x] Fix 1e end to end (micros → provider-minor at the intents boundary; fix the verifier comparison).
      → DONE: `refundAmountCents` (MicrosToCentsExact, 400 on sub-cent) at prepareAdminRefund; cents carried on
      adminRefundPrepared into RefundIntentFor + RefundPayload.AmountCents. Verifier compare is cents==cents
      once the payload is truly cents — no refund.go logic change needed (unit doc added).
- [x] Fix 1f storage row (stripe.go:1151) exactly — DONE (in #675 agent — stripe.go failed-payment row now
      stores CentsToMicros(inv.AmountDue)). Analytics display family: event_log_service.go /100.0 pair fixed
      (MicrosToMajorUnits) by #671; the stripe.go/nmi.go/ccbill.go /100.0(+/100) siblings in
      internal/modules/webhooks (8 sites) also converted to MicrosToMajorUnits by the #675 agent.
- [x] 1g: ONE scale-aware converter with an explicit per-adapter unit contract; both arrears + topup use it.
      → DONE: `money.NativeToRailMinor` (registry-driven: new `MinorDecimals` on the Currency table; ceil,
      errors on unknown currency); arrears chargeOneOpenInvoice + topUpAccount both use it;
      `nativeAmountToRailMinor` deleted; ChargeRequest.AmountCents unit contract documented.
- [ ] SYSTEMIC: introduce `Micros`/`Cents` defined types at the integration boundaries (nmi.SaleParams,
      RefundPayload, solana helpers) so this bug shape is a compile error; sweep remaining `/100` and `/10_000`
      literals.
      → PARTIAL: `moneyutil.Micros`/`moneyutil.Cents` defined; applied to CalculateTokenQuote +
      FiatMicrosToStablecoinBaseUnits params. Remaining sweep hits are inside internal/modules/webhooks (#675)
      and internal/integrations/nmi (#663).
- [x] POST-#663: retype the provider boundary structs (`nmi.SaleParams.Amount`/`RefundParams.Amount` →
      `moneyutil.Cents`, `RecurringPaymentData.Amount` dollars → consider a defined type,
      `intents.RefundPayload.AmountCents`, `subscriptions.RefundParams.Amount`, `money.ChargeRequest.AmountCents`)
      once the nmi-v5 rewrite lands — deliberately NOT done now to avoid colliding with #663's uncommitted tree.
      Ship the deferred provider-boundary wire tests (NMI RunSale/refund/recurring, Stripe outbound+webhook,
      CCBill parsing) in the same pass.
      → DONE 2026-07-01 (with the #669 agent — same boundary files). Retyped: nmi.SaleParams.Amount +
      nmi.RefundParams.Amount + AddRecurringPlan/EditRecurringPlan params + centsJSONAmount/centsToDollarString
      → moneyutil.Cents; RecurringPaymentData.Amount → NEW `moneyutil.MajorUnits` (float64 decimal dollars);
      intents.RefundPayload.AmountCents + RefundIntentFor param + adminRefundPrepared.amountCents →
      moneyutil.Cents; subscriptions.RefundParams.Amount + StripeInvoiceCollectionParams.AmountCents →
      moneyutil.Cents; money.ChargeRequest.AmountCents → moneyutil.Cents (rail minor; NativeToRailMinor now
      RETURNS Cents so both arrears+topup flow typed end-to-end). All existing wire-pinning assertions pass
      with UNCHANGED values (only mechanical type conversions at a few test call sites). NEW NMI wire tests:
      TestRunSale_WirePinsCentsAmount (Cents(1999) ⇒ body "19.99"), TestRefund_WirePinsCentsAmount ("5.00";
      0 ⇒ amount key omitted), TestAddRecurringSubscription_WirePinsDollarsAmount (MajorUnits(19.99) ⇒
      form amount=19.99) — joining the pre-existing TestCentsAmountWirePinning + plan-shape tests. The
      Stripe outbound+webhook and CCBill parsing wire-test battery remains under the TEST WALL bullet.
- [ ] TEST WALL (Paul 2026-07-01: mandatory — a units mixup must never reach production again; unit-test EVERY
      place that matters). PARTIAL — landed now: TestUpgradeWirePinning + TestCalculateModelBUpgradeCharge
      (checkout: preview micros == 3133-cent "31.33" NMI wire, recurring dollars shared converter, sub-cent ⇒
      error), TestCalculateTokenQuote_MicrosWirePin + TestFiatMicrosToStablecoinBaseUnits (solana: $19.99 ⇒
      19_990_000 base units, never 199,900), TestRefundAmountCents (handlers: 5_000 micros ⇒ ERROR, 60_000_000
      ⇒ 6000 cents), TestNativeToRailMinor(+LegacyArrearsCeil) (money: USD/EUR/JPY-zero-decimal, ceil, unknown
      currency errors), TestSolanaUpgradeReducedFirstCharge rewritten to micros. REMAINING (post-#663): the
      provider-package boundaries listed below. Two layers:
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
- [x] Stale unit docs while in there: pkg/catalog/manifest.go:159 example, solana/recurring/confirm_tier_change.go:73,
      models/payment.go:28. → DONE (plus micros notes on manifest UnitAmount fields and solana types.go Amounts).

## Acceptance
No production call passes micros into a cents/dollars parameter (typed units make it uncompilable); upgrade,
Solana, and refund paths charge exactly what the preview/request says; JPY topup and arrears collect the same
provider amount for the same micros. EVERY provider money boundary has a wire-pinning unit test (known micros
in ⇒ exact wire amount out) — a reintroduced units mixup fails unit tests, not production.

---

# #672: CRITICAL — metered usage re-accrued (billed N×) across overlapping invoice-close windows

**Completed:** yes
**Status:** FIXED (2026-07-01) — delta-vs-accrued-prefix design (chosen over segment rating, which would
re-grant allowances and drift rounding per segment): durable per-period watermark table
`openrails.metered_rating_watermarks` (migration 056; PK merchant/customer/currency/source[+dim]/period_from;
columns accrued_amount + rated_through). Every metered sweep now goes through `accrueMeteredPrefix`
(metered_rating.go): rate the FULL prefix [period_from, cutoff) once, upsert-LOCK the watermark row, accrue only
`rated − accrued_amount` — ledger transfer + pending item + watermark advance in ONE tx. Monotone: repeated or
stale closes compute delta ≤ 0 and accrue nothing. Both sweep paths converted (RateMeteredUsageFromEvents +
sweepCatalogRateCardUsage); the explicit per-report AccrueCatalogMeteredAggregate (spend.go) keeps caller
source_id idempotency (not window-keyed, unaffected). Integration test
`TestMeteredUsage_OverlappingCloses_RatedExactlyOnce` GREEN; pre-existing bridge/ratecard/re-finalize tests GREEN.

## Metadata
- Category: bug
- Status: done
- Passes: true

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
- [x] Rated-through watermark per meter (delta-vs-accrued-prefix; source_id = `period:<from>-<cutoff>` of the
      advancing close, amount = only the unbilled delta).
- [x] Backstop invariant: integration test with threshold close mid-period + longer-prefix close + month-end
      finalize + stale re-close ⇒ total accrued == rating the period exactly once
      (metered_watermark_integration_test.go).
- [x] Audit existing accruals for double-billing exposure: MOOT — the only producers of overlapping closes
      (InvoiceWorker threshold/month-end jobs) were wedged since birth by #673 (every run died in
      merchant.Require), so no overlapping close ever executed; direct FinalizeInvoice callers use a single
      window per period. No historical double-accruals to repair.

## Acceptance
Threshold close + later threshold close + month-end finalize over the same period accrues each unit of usage
exactly once; the feedback loop is structurally impossible (watermark monotone).

---

# #673: CRITICAL — invoice collection + auto-top-up workers run with no merchant context: wedged since birth

**Completed:** yes
**Status:** FIXED (2026-07-01) — `forEachActiveMerchant` helper (jobs_credit_money_in.go) mirrors
ConvergeSweepWorker: privileged ListActiveMerchantIDs, then each merchant runs under
`RunInMerchantConn(merchant.WithID(…))`; one merchant's failure is logged + skipped, the joined error is
returned so a failing run is visible in River (never silently "succeeds"). InvoiceWorker + AutoTopupWorker got a
`DB` field (wired in river_register.go). LowBalanceAlertWorker registration + its hourly periodic job DELETED —
no money.Alerter implementation exists anywhere in the runtime, so registration was a permanent no-op; re-add
with the notification wiring (worker type kept). Landed deliberately AFTER #672 + the #674 minimal arrears
hardening, since unwedging these workers activates arrears collection + metered sweeps. Integration tests
(worker_merchant_ctx_integration_test.go) run both workers exactly as River does — bare context.Background(), no
merchant — and assert real effects: invoice collected to paid, balance topped up. GREEN.

## Metadata
- Category: bug
- Status: done
- Passes: true

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
- [x] Mirror ConvergeSweepWorker: enumerate active merchants, run each under `RunInMerchantConn(merchant.WithID(…))`.
- [x] Integration test that runs InvoiceWorker + AutoTopupWorker against a seeded merchant and asserts actual
      collection/top-up effects. Per-merchant failures are Error-logged AND joined into the worker's return
      error, so River's job state shows the failure (a full worker-error-rate alert channel remains a good
      follow-up).
- [x] Wire or delete the LowBalanceAlertWorker registration — DELETED (registration + hourly periodic job): no
      Alerter implementation exists in the runtime.
- [x] NOTE honored: #672 and the #674 minimal arrears hardening landed in the same pass, BEFORE this unwedge.

## Acceptance
Hourly/monthly/daily invoice jobs and auto-top-up demonstrably move money for a seeded merchant in integration
tests; no worker in river_register runs with a context its own call graph rejects.

---

# #674: universal write-ahead provider-intents log — every external write durable-first, effectively-once

**Completed:** no
**Status:** IMPLEMENTED IN-TREE (2026-07-01) — primitive + NMI ambiguity classification + four site migrations
(sync NMI sale, NMI subscription create, auto-top-up, Solana crank) + upgrade-proration verify-hardening + CI
guard + crash-injection integration tests; unit + integration suites for intents/checkout/money/river green.
No new migration needed (provider_intents already carries everything; intent kinds are strings). Deferred
tails listed below. Originally PLANNED from the 2026-07-01 money-path audit. One theme, six sites: a real provider
charge can happen with no durable local record (→ re-charge on retry/crash), and transport-ambiguous errors are
treated as clean declines. The provider-intents outbox (dunning/manual-rebill/refunds) already does ALL of this
right — unique (merchant, idempotency_key), SKIP LOCKED lease, attempts>1 ⇒ verify-at-provider before resend —
these paths predate/bypass it.

## Implementation notes (2026-07-01)
- **Primitive**: `Runner.EnqueueAndExecute` (pre-existing, #358/#607) IS the write-through primitive — insert
  intent (idempotent) → ClaimByID → execute inline through the identical gate/execute/classify pipeline →
  return the canonical post-execution row. No new runner machinery was needed; this issue added intent KINDS +
  producers + the ambiguity classification underneath. Producers branch on row.Status; non-final statuses
  surface as `checkout.ErrCheckoutProcessing` ("retry with the SAME idempotency key"), never a decline.
- **NMI ambiguity (new)**: `nmi.TransportAmbiguousError` / `nmi.IsTransportAmbiguous` — wrapped at the
  transports: direct-post PostForm/non-200/body-read/unparseable-200, v5 non-GET send/read/5xx/undecodable-2xx.
  Parsed declines (CustomerVaultError), parsed 4xx envelopes, read-only guard, pre-send validation stay CLEAN.
  Reads (GET) never wrapped. Conservative: connection-refused is ambiguous (verify resolves to
  "not executed" → clean retry).
- **New intent kinds**: `nmi_sale` + `nmi_subscription_create` (handlers in internal/modules/checkout — they
  need per-merchant NMI client resolution + the registration services; intents pkg can't import checkout),
  `topup_charge` (internal/intents/topup_charge.go), `solana_pull` (internal/river/jobs_solana_pull_intent.go).
  All provider order-ids/idempotency keys derive from the INTENT ID (`intent.ID`, `topup:<id>`), ≤ 50 chars;
  intent identity derives from the client checkout key / persisted episode anchors.
- **Sale (a)**: CheckoutNMISaleService.Process = redis idempotency shell (kept for response caching) +
  EnqueueAndExecute. ReserveProviderAttempt/nmi_sale_attempt rows ABSORBED into the intent (removed from the
  flow; ValidateRefund keeps the historic `nmi_sale_attempt:`/`nmi_sub_attempt:` prefix guards for old rows).
  finalize = idempotent RegisterPurchase; charged-but-unrecorded ⇒ unknown_needs_verify, verifier repairs via
  FindSuccessfulSaleByOrderID. The "retry with a new idempotency key" message now only follows a VERIFIED-clean
  decline.
- **Subscription create (b)**: same shape; verify leg = order-id sale search (immediate starts) + v5
  subscription ROSTER scan matched on (customer_vault_id, plan.id), accepting matches unknown locally (the
  orphan) or whose local row carries this intent's order_id (finalize crashed midway). finalize reuses
  completeNMISubscriptionRegistration (idempotent). recoverNMISubscriptionAttempt + the attempt-row reservation
  removed.
- **Upgrade proration (b, partial)**: NOT a full intent (compensation saga too entangled) — hardened in place:
  redis Failed now falls through + pre-charge verify by the stable content-derived order id; RunSale ambiguity
  ⇒ verify-then-adopt txn, else rollback successor + ErrCheckoutProcessing (retry re-verifies same order id).
  Never rolls back/declines on a lost response anymore.
- **Auto-top-up (c)**: episode anchor = persisted last_topup_at (unix or "genesis"), NEVER wall clock;
  MoneyService.RunAutoTopups/topUpAccount replaced by ListDueAutoTopups + HasAutoTopupDeposit (credit-grant
  source_id lookup — the old GetTransactionBySource("deposit",...) check never matched the #514 grant-ledger
  shape, i.e. the old dedupe was dead) + FinalizeAutoTopup + StampAutoTopupAttempt. Deposit source_id = wire
  ref = `topup:<intent id>` (also fixes the >50-char NMI order-id breakage of the old `autotopup:<uuid>:<unix>`
  key under the #663 length check). Verify: local deposit first, then NMI order-id search; stripe-family rails
  re-execute under the same Stripe Idempotency-Key (replay-safe) instead of a read.
- **Solana crank (d)**: signature WRITE-AHEAD — BuildSignSubmit*Presubmit persists the signed tx signature onto
  the intent (Store.RecordProgress, evidence key transaction_id) BEFORE submission; crankOne refactored to a
  typed crankOutcome (+presubmit hook; state-machine unit tests intact). Handler: recorded-sig ⇒ chain read
  (GetTransaction) BEFORE any re-pull — landed ⇒ renewal repair (RenewMembership+AdvanceAfterPull via
  finalizePull; the "subscriber paid, gets nothing" bug is dead), read-failed ⇒ refuse to re-pull blind
  (ambiguous), verified-not-landed ⇒ re-crank (program period guard Custom:400 backstops). AlreadyPaid with no
  recorded sig keeps the legacy advance+reconcile-repair (out-of-band payment). Worker keeps a legacy direct
  path when Intents is nil (unit-test harnesses).
- **CI guard**: internal/intents/enforcement_guard_test.go (no build tag) — textual scan of internal/+pkg/
  (excluding internal/integrations/**, which IS the client layer) for the choke tokens (.RunSale(,
  .AddRecurringSubscription(, .AttemptManualRebill(, nmi.RefundParams{, .Void(, .DeleteRecurringSubscription(,
  .UpdateSubscriptionPaymentSource(, .DeleteCustomerVault(, solanaint.BuildSignSubmit) against a per-token
  file allowlist with justifications. LIMITS (documented in the test): token-textual (wrappers/method values
  invisible), allowlisted files trusted wholesale, Stripe not scanned (architecturally choked through
  stripeapi's readonly transport, the stronger guard).
- **Tests** (integration tag, testcontainers): per site — happy-path/decline/ambiguous-timeout-verify/
  charged-but-unrecorded-repair/provider-offline-original-key (checkout sale:
  nmi_sale_intent_integration_test.go), orphaned-remote-create repair (nmi_subscription_intent_integration_test.go),
  topup episode/decline-cooldown/ambiguous-verify/charge-crash-deposit/offline-retry
  (money_in_integration_test.go), solana crash-after-submit signature repair + not-landed re-crank
  (jobs_solana_pull_intent_integration_test.go), NMI ambiguity classification unit tests (ambiguity_test.go).

## Deferred (follow-ups, not in this pass)
- **Arrears collection**: stays on the Wave-1 hardening (attempt-count keys + CAS-miss recording, #672/#673).
  Migrating chargeOneOpenInvoice onto a `topup_charge`-style intent is NOT a small mechanical step (invoice
  snapshot CAS + PayOwed transfer live inside the recording tx) — do it as its own slice; nmi_collection.go is
  allowlisted in the guard with this note.
- **Upgrade as a full intent saga** (successor create + proration + swap + compensation): only the proration
  leg was verify-hardened; an ambiguous successor CREATE still rolls back pessimistically (delete is
  verify-then-execute, so safe, but a lost-create-response leaves a remote sub until the delete lands).
- **Vault delete / payment-method swap / reactive cancels**: still direct calls through their service chokes
  (VaultService.DeleteVault, UpdateSubscriptionPaymentSource, DeleteRecurringSubscription in user/admin
  cancels) — allowlisted in the guard; migrate opportunistically.
- **CCBill**: no writes migrated (its mutations are hosted-page/DataLink shaped); unchanged.

NOTE (2026-07-01): the MINIMAL arrears-collection hardening landed with #672/#673 — chargeOneOpenInvoice now
keys the charge `invoice:<id>:attempt:<n>` (n = durable count of recorded invoice_payments rows, NOT the mutable
amount snapshot), and on ApplyInvoicePaymentSnapshot n==0 after a successful provider charge it records the
settled invoice_payments row + PayOwed transfer (unapplied) and Error-alerts instead of `return nil` (tests:
arrears_cas_integration_test.go). The full intents-outbox migration of this site remains THIS issue.

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
- Status: in_progress
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
- [x] Extend `internal/intents` with a synchronous execute-through mode — DONE via the pre-existing
      `Runner.EnqueueAndExecute` (no duplication needed): insert intent → inline execute through the identical
      gate/execute/classify pipeline → complete/fail. Ambiguity marks `unknown_needs_verify`, never failed; the
      existing lease/sweeper drains pending/unknown. New kinds shipped: nmi_sale, nmi_subscription_create,
      topup_charge, solana_pull. (recurring-update/vault-delete/void kinds deferred — see Deferred.)
- [x] Provider idempotency keys/order-ids derived from the intent id (sale/sub-create order id = intent.ID
      [+e2e suffix], topup wire ref = `topup:<intent id>`, solana signature recorded onto the intent).
- [x] NMI client: transport-ambiguous vs provider-declined classification
      (nmi.TransportAmbiguousError/IsTransportAmbiguous at both transports); ambiguity ⇒ verify-by-order-id,
      never MarkFailed/DeleteVault/new-key.
- [x] Migrated sites: sync NMI sale (a), NMI subscription create (b), upgrade proration verify-hardening
      (b, partial — full saga deferred), auto-top-up with persisted-state episode anchor (c), Solana crank with
      pre-submit signature + recorded-sig renewal repair (d); ReserveProviderAttempt/nmi_sale_attempt +
      nmi_sub_attempt reservations ABSORBED into intents. Arrears deliberately left on Wave-1 hardening (not a
      small mechanical step — see Deferred).
- [x] Enforcement: grep-based allowlist guard (enforcement_guard_test.go, runs untagged in CI) over the NMI
      mutating methods + solana sign-submit; Stripe covered architecturally by the stripeapi choke. Limits
      documented in the test + Implementation notes.
- [x] Crash-injection integration tests per migrated site (ambiguous-timeout ⇒ verify resolves; charged-but-
      unrecorded ⇒ verifier repairs off the original key; provider-offline ⇒ intent parks then lands under the
      ORIGINAL derived key; decline ⇒ terminal, no effect; replay ⇒ durable answer, zero provider calls).

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
**Status:** IMPLEMENTED (2026-07-01) — all findings fixed in-tree; unit + full webhooks/repo integration suites
green. Awaiting review/commit. Enqueue/dedup/signature layers were verified sound; these were the apply-layer
holes. See Implementation notes below.

## Metadata
- Category: bug
- Status: implemented
- Passes: true

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
- [x] Subscription apply layer: one tx + FOR UPDATE (or updated_at CAS) around read-modify-write; persist and
      compare provider event created-timestamps; make UpdateAt full-row semantics explicit or retire it for
      webhook applies.
- [x] Terminal-blocked renewal: record the payment or a CCBill-style repair alert before return nil (Stripe+NMI).
- [x] Void/chargeback/refund not-found: retryable error (as nmi.go:2019 refunds do) or deferred-reversal repair
      alert — never plain ACK.
- [x] Propagate GrantSubscriptionCredits errors (re-grep: 5 sites in webhooks — stripe ×1, ccbill ×2, nmi ×2;
      NMI's warn-only pre-grant subscription reloads now propagate too).
- [x] NMI one-off refund branch (resolve by transaction id); parse failure = error.
- [x] Out-of-order integration tests: invoice.paid ∥ subscription.updated; payment_failed then stale
      updated(active); void-then-sale; renewal-after-cancel.
- [x] Minor: CCBill >2% amount mismatch now writes a durable `amount_mismatch` repair alert (both
      validateCCBillBilledAmount callers + the upgrade-success inline check; upgrade alert written after the
      tx rolls back so it survives).

## Implementation notes (2026-07-01)
- Ordering guard: new sqlc query `GetSubscriptionByRailSubIDForUpdate` + repo method; stripe
  `handleSubscriptionUpdated` and `handleInvoicePaymentFailed` run one MerchantTx with FOR UPDATE, and a
  `stripe_last_event_created` watermark in subscription metadata rejects strictly-older events;
  `handleInvoicePaid` bumps the watermark (row-locked, never rewinds) after CreateMembership/RenewMembership
  so a delayed older updated(active) can't revert a renewal or a past_due flip. evt.Created plumbed through
  handleEvent. UpdateAt's full-row semantics documented at the definition.
- Void/chargeback race: chose RETRYABLE ERROR at all three not-found sites (CCBill handleVoid/handleChargeback,
  NMI handleVoidSuccess — the NMI one now errors regardless of subscription presence): all three rails
  redeliver on non-2xx, matching the pattern NMI refunds already used. The not-found-branch ClickHouse "audit"
  logging was dropped (it would duplicate per retry; durable trace = failed idempotency record + the applied
  reversal on redelivery).
- NMI refunds: new `handleNMIOneOffRefund` mirrors Stripe's one-off path (resolve by refund txn id, then
  original txn id → Refund row → full refund revokes entitlements/product-access, or cancels the payment's
  subscription); amount parse failure = durable repair alert + non-retryable (redelivery resends the same
  bytes). Refund/one-off-before-sale → retryable error.
- OTHER UpdateAt callers left unguarded (deliberately out of scope): lifecycle_service.go
  RenewMembership/CancelMembership read-modify-write inside MerchantTx but WITHOUT FOR UPDATE; NMI
  handleUpdateSubscription/ACU and CCBill customer-data/billing-date handlers Update() from non-locked reads.
  Follow-up: switch those reads to the ForUpdate variant.
- Sweep note: `sqlc generate` (needed for the new query) also regenerated gen for ANOTHER agent's in-flight
  uncommitted removal of the payment_blocklist queries (admission_support.sql edit + untracked migration
  055_drop_payment_blocklist) — gen/admission_support.sql.go + gen/models.go lost that dead code; zero Go
  references, build green.
- Tests: stripe_ordering_test.go (watermark unit tests) + webhooks_apply_integration_test.go (8 tests:
  payment_failed then stale updated(active) does NOT reactivate; invoice.paid ∥ stale updated in BOTH orders
  keeps the renewed period; terminal-blocked renewal writes the repair alert; CCBill void + chargeback
  before-sale retryable; NMI void before-sale retryable; NMI one-off refund reverses + idempotent redelivery;
  NMI refund-before-sale retryable; CCBill credit-grant failure propagates retryable). Full webhooks + repo
  integration suites green via testcontainers.

## Acceptance
Out-of-order or concurrent webhook delivery cannot revert committed state or grant access without payment; no
money-bearing event is ACKed with zero durable effect (payment row, retry, or repair alert — always one of the
three).

---

# #676: spendgate holds — volatile capture pointer, leaking held gauge, phantom sweep

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01) — all four findings fixed + tested (see Resolution); awaiting commit.

## Metadata
- Category: admission
- Status: implemented
- Passes: true

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
- [x] Durable admit-coordinate record (or caller-supplied payer coords as capture fallback); pointer Set failure
      = admit failure, not best-effort.
- [x] Implement the held-recompute sweep the comment promises (or rolling-TTL holds + recompute-on-expiry);
      fix releaseScript bucket recreation.
- [x] Derive the balance/owed split from spendable lots (or fall through to owed on shortfall) so lapsed lots
      can't block capture.
- [x] Chaos test: admit → Redis flush → capture still lands (degraded path); abandoned admits don't shrink
      capacity permanently.

## Resolution (2026-07-01)
- **Capture pointer**: admit's request→payer pointer Set is now REQUIRED (Set failure ⇒ admit error, fail
  closed; the leaked reservation self-heals via hold TTL + recompute). Durable fallback = caller-supplied payer
  coords on capture, ADDITIVE wire fields `customer_id`/`currency`/`invoker`/`admit_source` on
  POST /v1/merchant/admissions/:id/capture (handler + pkg/service.CaptureHoldRequest + SDK CaptureUsage +
  remote/embed transports). `admit_source` must echo the admit's source (default "admit") so retries dedupe on
  the same (source, source_id) ledger coordinates. Chosen over a per-admit Postgres record: no hot-path DB
  write, no migration (avoids the live #582/#630 db/gen churn); hosts opt in by echoing coords they already
  hold. Fallback customer_id passes requireServiceCustomerScope in the handler.
- **held gauge**: chose lazy recompute-on-deny inside admitScript over a River sweep: when the affordability
  gate would deny, the (atomic) script recomputes held = Σ live hold records via a new "<base>:holds" index SET
  (SADD on reserve, SREM on capture/release + stale-member GC) and re-checks — abandoned admits release
  capacity exactly when it's needed, no scan job, trivially race-free. capture/release clamp held at 0.
- **releaseScript**: window decrements now gated on EXISTS — an expired bucket is never recreated as a
  permanent negative key.
- **Lapsed lots**: spendBalanceThenOwedTx caps the balance leg at the SPENDABLE-lot total
  (ListSpendableCreditLots AsOf now) instead of the raw ledger balance; the shortfall follows the existing
  gating contract (capture → owed unconditionally; immediate spend → credit-line gate). Lapsed lots can no
  longer surface ErrInsufficientCredits on capture.
- Tests (all green): spendgate `TestGate_AbandonedHoldRecomputeRestoresCapacity`,
  `TestGate_ReleaseAfterBucketExpiryNoNegativeWindow`; money
  `TestCaptureAuthorized_LapsedLotNeverBlocksCapture` (new file lapsed_lot_capture_integration_test.go);
  service `TestCaptureHold_RedisFlushFallback`, `TestCaptureHold_FallbackRetryIsIdempotent`
  (new file capture_fallback_integration_test.go).
- Known follow-up: gauge introspection (HeldAmount/BudgetStatus) can read a stale (inflated) held for an idle
  payer until the next denied admit — display-only, enforcement is exact.

## Acceptance
A rendered service is always chargeable (no capture path depends on volatile state); abandoned admits release
capacity within one sweep interval; lapsed lots never produce ErrInsufficientCredits on capture.

---

# #677: customer-lock bypasses — credit expiry, converge repairs, AccrueOwed race

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. All three bypasses now take the SAME
customers-row FOR UPDATE as spends (new `grants.Ledger.LockCustomer`, delegating to LockCustomerForSpend):
ExpireLapsed locks at entry (the job's tx wraps it); converge MaterializeGrant repairs run via
`lockedMaterialize` (MerchantTx + LockCustomer, converge_passes.go); AccrueOwed locks before its
check-then-insert. DB backstop = migration 057: partial unique `idx_ledger_transfers_lot_once`
(merchant_id, grant_id, transfer_type) WHERE type IN (deposit, credit_expire, credit_revoke) — a lot
deposits/expires/claws at most once — and `idx_ledger_transfers_owed_accrual_once`
(merchant_id, customer_id, currency, source, source_id) WHERE type='owed_accrual' AND source_id <> ''
(empty excluded: SpendCredits permits keyless spends whose owed spill legitimately repeats). Concurrency
tests green: spend∥expiry (TestGrants_SpendExpiryConcurrency_LockSerializes), overlapping clawbacks at the
grants seam AND through two full concurrent Converge runs, two concurrent AccrueOwed. The latent RevokeGrant
over-refund race is gone with RevokeGrant itself (#666 item 7, done alongside).

## Metadata
- Category: bug
- Status: done
- Passes: true

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
- [x] Take LockCustomerForSpend inside ExpireLapsed (or in the expiry job per customer). (Inside ExpireLapsed —
      every caller inherits it; grants_customer_fk guarantees the customers row exists, no ensure needed.)
- [x] Wrap converge money repairs in MerchantTx + customer lock; consider a partial unique index on
      ledger_transfers(grant_id, kind) for the clawback/deposit pairs as a DB backstop. (Both MaterializeGrant
      repair closures wrapped in lockedMaterialize; index landed as 057 idx_ledger_transfers_lot_once, also
      covering credit_expire. Entitlement-only repairs — DeriveSubscriptionGrant/DeriveWalletGrant — stay on
      the engine conn: no money legs, and derive-1 relies on incremental per-feature commits.)
- [x] AccrueOwed: lockBalance first, or a partial unique index on owed_accrual coordinates. (Both: the lock
      primitive without the balance derivation — ensureCustomer + LockCustomerForSpend — plus 057
      idx_ledger_transfers_owed_accrual_once; index excludes source_id='' for keyless SpendCredits spills.)
- [x] Concurrency tests: spend ∥ expiry on the same lot; two converge repairs on the same grant; two AccrueOwed.
      (True-parallel goroutine tests against the shared pg, made deterministic via channel + lock-hold; green.)

## Acceptance
Every write that consumes or reverses lot value runs under the same per-customer serialization as spends; the
ledger cannot record the same accrual/clawback twice (app lock + DB backstop).

---

# #678: webhook dedup is Redis-only, fail-open, non-transactional

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01) — ALL tasks in-tree, including the Postgres-truth follow-up Paul
approved 2026-07-01: dedup TRUTH moved to `openrails.webhook_events` (migration 061), Redis demoted to a
fast-path cache + lease-coordination layer (flushable with zero correctness consequences), posture gate
downgraded to a warning. Unit + integration (webhooks, idempotency, river, app, db, querytest) green.
Awaiting review/commit. (Originally from the 2026-07-01 money-path audit; #675's row locks/watermarks are
the apply-layer half.)

## Metadata
- Category: reliability
- Status: implemented
- Passes: true

## Problem
(line refs = audit-time, pre-fix) Webhook dedup lives only in Redis (90d TTL, build_runtime.go:799); `cfg.Redis == nil` silently falls back to a
PER-PROCESS memStore (idempotency/service.go:105-117) — multi-replica standalone loses cross-replica dedup
entirely, and Redis flush/eviction loses history. `Complete` is written after (not with) the effects, so every
handler must be individually replay-safe (that's #675's job). The 2-min pending-lease takeover
(deduplication.go:136-154) permits concurrent same-event processing for slow handlers. `IsDuplicate`
(deduplication.go:70-98) is dead code with a mismatched op key.

## Tasks
- [x] Refuse to start webhook processing without Redis when running multi-instance (config posture check, like
      EnforceRLSPosture) — silent memStore fallback only in dev/single-process.
- [x] Consider moving dedup marks into Postgres inside the effect tx (then Redis is a fast-path, not the truth)
      — DONE 2026-07-01 (was design-only); see "Implementation — Postgres truth" below.
- [x] Revisit the 2-min takeover: lease renewal for slow handlers instead of concurrent takeover.
- [x] Delete dead IsDuplicate (fold into #666 if it lands first) — deleted here (zero callers, mismatched
      `webhook.<rail>.event` op key; verified incl. integration files).

## Implementation notes (2026-07-01)
- **Posture gate** (SUPERSEDED same day by the Postgres-truth section below: the startup error is now a
  warning) (`enforceWebhookDedupPosture` + pure `webhookDedupPostureError`, build_runtime.go, called
  right after Redis resolution in `buildRuntimeWithOverrides`): no Redis + non-dev (`!cfg.IsDev()`, same axis
  as `RequiresRLS`) + standalone ⇒ startup error. **Standalone signal = no host-injected DB pool**
  (`overrides.DB == nil`) — the exact signal that scopes `createDatabase`'s RLS gate to the config-built path:
  embedded hosts inject a pgx pool (documented BootstrapOptions contract) and are single-process by contract;
  the standalone server AND worker binaries build their DB from config and can run N replicas. Dev/embedded
  without Redis keep the memStore fallback behind a loud `log.Warn` (per-process, no cross-replica protection).
- **Lease heartbeat**: `IdempotencyService.RenewPending` (thin `TryTakeoverPending(…, 0)` delegate — pending-only,
  cannot resurrect a completed record) + `DeduplicationService.startPendingHeartbeat` renews the pending
  record's CreatedAt every lease/4 while the owning handler runs. Takeover-by-timeout kept unchanged as the
  dead-holder path: staleness now genuinely means the holder died (4 consecutive missed heartbeats), not that
  it was slow. `pendingLease` is a test-only override field (default still 2 min). #675 row locks backstop the
  residual wedged-process >2min pause window — deliberately not over-built.
- Tests: posture unit matrix (internal/app/webhook_dedup_posture_test.go); slow-handler-stays-exclusive +
  dead-holder-takeover + renewal semantics (webhooks + idempotency unit, -race -count=3); Redis-backed
  renew/takeover integration (service_redis_integration_test.go, testcontainers);
  TestStripeInvoicePaymentAlreadyRecorded integration re-run green. Full `go build ./...` blocked by another
  agent's in-flight internal/http/handlers edit (not this change); all touched packages build/vet clean.

## Implementation — Postgres truth (2026-07-01, supersedes the deferred design note)
- **Schema**: migration `061_webhook_events.up.sql` — `openrails.webhook_events(merchant_id, rail, op,
  event_id, status DEFAULT/CHECK 'completed', created_at, completed_at, PK (merchant_id, op, event_id))`
  + `idx_webhook_events_completed_at` (retention scan) + FORCE/ENABLE RLS + merchant_isolation policy +
  `GRANT SELECT,INSERT,DELETE TO openrails_app` + merchant FK. Key mirrors the Redis derivation exactly
  (`op = webhook.<rail>.<event_type>`, event_id = rail event/transaction id); merchant_id joins the PK
  because the table is RLS-scoped. Only COMPLETED marks live here — pending/lease stays in Redis
  (coordination, not truth), hence the CHECK. sqlc queries in internal/db/queries/webhook_events.sql
  (WebhookEventCompleted / MarkWebhookEventCompleted / DeleteCompletedWebhookEventsBefore), gen regenerated.
- **ProcessWebhook order** (deduplication.go): Redis Begin (fast-path skip + pending lease, unchanged) →
  on cache miss, Postgres truth SELECT under MerchantTx (hit ⇒ backfill Redis Complete + skip) → handler
  (heartbeat unchanged) → on success/non-retryable, verify-or-write the truth row (INSERT … ON CONFLICT DO
  NOTHING under MerchantTx) → Redis Complete is now CACHE-ONLY (failure = warn, not retry — the old
  "Complete failed ⇒ retry ⇒ re-run effects" loop is gone whenever the truth row exists). Truth-row write
  failure stays retryable (#675 replay-safety converges the redelivery). No merchant on ctx or nil DB ⇒
  loud warn + legacy Redis/memory-only behavior (handlers' own MerchantTx calls would fail anyway).
- **In-tx seam**: `MarkWebhookProcessedInTx(ctx, tx)` — ProcessWebhook stashes the mark identity on ctx;
  a handler calls it inside its final MerchantTx so mark+effects commit atomically; the wrapper's
  verify-or-write then no-ops on conflict, so the call is an atomicity upgrade, never a requirement.
- **Per-rail mark modes**: CCBill `handleBillingDateChange` + `handleCustomerDataUpdate` = IN-TX (their
  whole effect is one MerchantTx). Everything else = WRITE-AFTER: every Stripe handler, every NMI handler
  and the CCBill money paths (NewSale/Renewal/Refund/Void/Chargeback/Upgrade/Cancel/Expiration) are
  sequences of service calls each committing their own txs — no single enclosing tx exists to mark in, and
  restructuring them is exactly the refactor this change was scoped NOT to do (#684's fetch-and-converge
  will collapse Stripe/NMI anyway; CCBill stays payload-apply and is why the Postgres backstop matters).
- **Posture gate decision**: DOWNGRADED to a loud warning (`webhookDedupPostureError` →
  `webhookDedupPostureWarning`, always-nil `enforceWebhookDedupPosture`) — the boot refusal existed because
  the memStore was per-process TRUTH; with truth in Postgres a Redis-less multi-replica boot is safe
  (never replays), merely wasteful (no cross-replica lease coordination, no fast path), and the standalone
  warning says exactly that.
- **Retention**: new leg in the existing `CleanupExpiredDataWorker` (house periodic cleanup):
  `CleanupConfig.WebhookEventRetention` default 90d = `webhooks.WebhookIdempotencyTTL` (the Redis cache
  TTL), zero/unset falls back to the default (never "delete everything"). RLS-correct: walks
  `ListActiveMerchantIDs` (control-plane read) and deletes per merchant under MerchantTx — the converge-
  sweep pattern, NOT the notifications legs' bare cross-merchant DELETE (see pre-existing-issue note).
- **idempotency memStore**: kept as-is (checkout et al. still use IdempotencyService as their sole store);
  type doc now states that for webhooks it is coordination+cache only.
- Tests (integration, testcontainers PG+Redis, all green): `TestWebhookDedupSurvivesRedisFlush` (headline:
  FLUSHALL between delivery and redelivery ⇒ no reapply + cache backfilled),
  `TestWebhookDedupTwoReplicasNoSharedRedis` (two service instances, same PG ⇒ exactly one apply),
  `TestWebhookDedupCrashBeforeMarkConverges` (effects-committed-no-mark ⇒ redelivery re-runs replay-safe
  handler, converges, mark lands), `TestWebhookDedupInTxMarkAtomicity` (tx rollback takes the mark with it;
  committed in-tx mark makes verify a no-op), `TestWebhookDedupNoMerchantFallsBackToRedisOnly`,
  `TestWebhookDedupNonRetryableWritesTruth`, `TestCleanupWebhookEventsRetention` (old deleted, recent
  kept, zero-retention safe; pending never exists in PG by construction), posture unit matrix rewritten.
- PRE-EXISTING issue observed, NOT fixed here (different files): the notifications/checkout cleanup legs
  and the River `WebhookProcessWorker` path run without merchant ctx — under a real openrails_app + FORCE
  RLS connection the bare cross-merchant DELETEs see nothing and worker-path MerchantTx would fail
  merchant.Require; tests pass because dbtest connects privileged. Worth its own issue.

## Design note — Postgres as dedup truth (SHIPPED above; kept for the record)
Original sketch: `Complete` written INSIDE the effect tx so Redis is a fast-path, not the truth —
`openrails.webhook_events` + RLS; Redis Begin → handler's own tx INSERT … ON CONFLICT DO NOTHING → Redis
Complete as cache; crash between commit and Redis write converges instead of double-applying. Implemented
2026-07-01 with one deviation: the wrapper verifies-or-writes after handler return, so the in-tx call is
optional per handler rather than a contract change to processingFunc. No backfill of the 90d Redis history
was done — existing completed keys keep serving from the cache until TTL; a pre-truth event redelivered
after BOTH its cache entry expired AND a flush would re-run the handler, which #675 replay-safety absorbs.

## Acceptance
Two replicas processing the same delivery cannot both apply effects; losing Redis degrades to slower processing,
never to replayed money effects.

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
- [x] Item 2 DONE 2026-07-01: blocklist.go/velocity.go + their integration tests deleted (CardAbuseGuard/
      WastedSpendGuard kept); payment_blocklist model + the 3 queries cut from admission_support.sql, sqlc
      regenerated; table dropped via migration 055 and removed from the 001 baseline (+ the two schema-test
      table lists).
- [x] Items 3-5, 8-9: delete unrouted handlers + dead mux/mount paths.
      (Item 4 DONE 2026-07-01: webhooks/replay subpackage deleted. Audit missed one importer —
      tests/nmi_webhook_test.go — which existed to exercise replay itself; trimmed to the one non-replay
      regression test (Stringish subscription-id normalization, now os.ReadFile on the fixture).
      testdata/webhooks fixtures KEPT: used by ccbill tests + seed_data.go.)
      (Item 3 DONE 2026-07-01 phase 2: all 14 unregistered Service* handlers deleted from
      service_admission.go (-370) + service_credits.go (~-290) plus the types/helpers that died with them
      (trustTierQuery, spendLimitWindow/spendLimitWindowInputs, serviceQueryCustomer, serviceWithdrawRequest,
      serviceAccountSettingsRequest, serviceMerchantConfigurationRequest, servicePayerSpendLimitRequest,
      serviceTierScheduleRequest, serviceInvokerSpendLimitRequest). embed/client.go references were comments
      only. serviceTxnResponse KEPT (live via self_account.go); merchantProfileResponse +
      serviceMerchantConfigWindows KEPT (live via ServiceGet/SetMerchantSettings).
      Item 5 DONE: AdminReconcileProduct/Price, AdminListCatalogOrphans + AdminListStripeOrphans + listOrphans,
      GetAdminManualRebillAttempts + manualRebillStatusFilters, DeleteAdminUserPaymentMethod +
      adminPaymentMethodPath deleted (~-265).
      Item 8 DONE: Server.NewHTTPHandler/newHTTPHandlerMux + http_handler_options.go + embedded_mux_test.go
      deleted (~-330). Audit missed a SECOND live caller — tests/http_handler_options_test.go — its two
      legacy-path tests were PORTED onto embedhttp.FromApp(suite.App) so route-set coverage survives on the
      real embed path (Server.Handler() standalone assertions kept).
      Item 9 DONE: RegisterHostWebhookRoutes + handlers.HostWebhook deleted; the /host-webhooks leg of
      routes_merchant_webhook_integration_test.go trimmed — it existed only to exercise the dead mount.)
- [x] Item 6 DONE 2026-07-01: internal/services/health deleted; /ready does on-demand pg pool.Ping + redis
      Ping (2s timeout) in routes_public.go; HealthManager field + Start/Stop + createHealthManager wiring
      removed from internal/app. Response keeps status/service/auth + verbose dependencies
      {available, last_error|reason}; tests only assert status codes.
- [ ] Items 7, 10-12: dead-surface tail sweep.
      (Item 7 DONE 2026-07-01 by #677: every listed method grep-verified dead (incl. integration-tagged files
      + pkg/embedded roots) and deleted — grants.Ledger.{Expire,Materialize,RevokeGrant,LiveGrants,
      RevokeBySource,SpendableLots} (~95 lines, grants.go + credit_spend.go) and
      money/ledger.Ledger.{Conservation,Spend,Expire} (~42 lines) + the now-orphaned LedgerLedgerNet sqlc
      query (regenerated). RevokeBySourceAsOf/MaterializeGrant/CreditSpend/ExpireLapsed KEPT (live) and every
      surviving lot-consuming/reversing path now runs under the customer lock (#677). Test-only consumers
      rewritten onto the surviving surface / gen queries / raw SQL; the RevokeGrant refund-leg test died with
      RevokeGrant (credit_refund transfers no longer have a writer). GrantAdmin/AdminGrantExists kept as noted.
      deadcode ./cmd/openrails now reports NOTHING in grants/money-ledger except the documented embed-root
      pair.)
      (Item 11 DONE 2026-07-01: five *CrossMerchant methods deleted, crossMerchant bool collapsed out of
      merchantFilter + the 5 query helpers; fixture-only tests removed, isolation integration test keeps its
      per-merchant assertions. Item 12 PARTIAL 2026-07-01: pkg/authprovider shim deleted, 8 importers
      migrated to billingauth (ginauth subpackage KEPT — live from ginmw/server/ginreq);
      cache.CacheMiddleware/NewCacheMiddleware/GenerateKey deleted (Cache interface kept, live);
      controlplane.Catalog/CatalogNames/MerchantOwnerRolePermissions + Permission type + catalogEntries
      deleted — never seeded, Groups()+Perm* consts are the live surface; fixture-guard tests removed,
      live-behavior tests (apex-bypass, admission gate, bootstrap owner walk-up) reworked onto inline perm
      lists. sqlc SetSubscriptionUnknown: already deleted by #664, nothing to do. SKIPPED:
      internal/bootstrap/merchant_env.go — audit claim stale, every func in the file is a live private
      helper of MerchantBillingEnvKey (called from merchant_manifest.go's env.Provider loader).)
      (Item 10 DONE 2026-07-01 phase 2: CCBillVersionedPayload interface + 12 Get* method blocks +
      CCBillWebhookVersion type + the 5 version consts (only ever fed the dead interface) + webhooks.IsExpired
      deleted from webhooks/types.go (-128).
      Item 12 phase-2 slice DONE: solana ValidateRecurringMint (+its allowlist_test block),
      IsTerminalFailure + terminalSignatures + failure_terminal_test.go (ClassifyCrankError's on-chain-code
      path superseded it), tokens.GetTokenBySymbol/IsValidToken, bare recurring.NewPlanService (6 test callers
      migrated to NewPlanServiceWithReader(sub, nil, ...)); moneyutil.ParseDecimalToMicros + its test
      (parseDecimalScaled/roundHalfAwayFromZero KEPT — live via ParseDecimalToCents, whose rounding coverage
      lives in webhooks/nmi_test.go); PaymentsIdempotencyAdapter + its test deleted, checkout's duplicate
      IdempotencyStatus/IdempotencyRecord replaced with type aliases onto the idempotency module, the two
      checkout store interfaces' Complete narrowed to json.RawMessage (nmi_sale_service marshals its struct
      at the one call site), build_runtime passes idempotencyService directly (never-nil ctor);
      dead ginmw: CORS (2 tests ported to CORSFromSource), bare RateLimit + classifyBucket (bucket test
      retargeted at the live neutral middleware.ClassifyBucket), InternalIPWhitelist, WebhookIPWhitelist,
      UserSessionAdminPrincipalRequired + userSessionPrincipal (+their tests), and most of
      service_credential.go — ServiceCredentialRequired/resolveServiceCredential/ServiceCredentialFromGin/
      RequireServiceCredentialCustomerScope + the 3 resolver interfaces + OwnerGroupRef context key +
      service_credential_test.go (~-680 across ginmw); ServiceCredentialContextKey + bearerToken KEPT (live).
      SKIPPED with reasons: MicrosToCentsCeil — LIVE (checkout/service.go:2164, audit claim stale);
      FiatMicrosToStablecoinBaseUnits — LIVE post-#671 (handlers + checkout), as re-verified;
      spendgate.Gate.SetClock — LIVE test seam (gate_integration_test.go:38 injects a fake clock);
      derefStr triplication — all 3 copies are LIVE in their packages (not dead code); money copy is
      #677-owned and normalize.FromPtr trims (behavior change), so 2-of-3 dedupe was skipped → #670/later.
      NOT TOUCHED in phase 2 (outside the phase-2 slice, still present in-tree — remaining item-12 tail):
      fx.NoOpProvider, solana/subscriptions.BuildResumeSubscription, RateLimitStore.Reset,
      MoneyService.snapshotTx (#677 area), checkout.SetSolanaLifecycleForTest, internal/http/response
      wrapper pkg, webhookutil ParseStripeSignatureHeader/ParseNMISignatureHeader re-exports.)
      (TAIL CLEARED by #670 2026-07-01: fx.NoOpProvider, BuildResumeSubscription, RateLimitStore.Reset,
      snapshotTx(+accountSnapshot), internal/http/response (pkg deleted, helpers inlined into ginauth),
      webhookutil re-exports — all deleted. Still open: checkout.SetSolanaLifecycleForTest (grep-dead but
      session_service.go was #674-hot; take in a later pass).)
      (Yagni interface collapse DONE 2026-07-01 phase 2: subscriptions NotificationStore/
      NotificationEmailSender → *NotificationService, AdminCancellationLogger/LifecycleEventLogger →
      *analytics.EventLogService (river workers already held the concrete type; reconcile/converge callers
      pass nil — no reconcile edits), merchantConfigurationReader → *merchantconfig.Store;
      user_admin_support.go now holds only GetNotificationsFilters. SKIPPED: StripeLivenessProber — live
      test seam (fakeStripeProber in jobs_subscription_liveness_integration_test.go); StripeSubscriptionLister/
      StripeChargeLister — collapse forces edits in internal/modules/reconcile + pkg/service/reconcile.go
      (reconcile owned/in-rewrite during phase 2) → #670/later. Entitlements repo-facade collapse NOT done
      (bigger refactor, not dead code) → future.)
- [ ] Re-run deadcode + full integration suite green after each batch.
      (Phase 2 verification 2026-07-01: go build ./... green, go build -tags integration ./... green,
      go vet (+integration tag) green on every touched package, unit tests green: handlers, routes, http,
      middleware+ginmw, webhooks, checkout, idempotency, subscriptions, river, app, solana/*, moneyutil.
      Full integration suite not re-run here — grants/money/spendgate/nmi were churning concurrently
      (#676/#677/#678 + the NMI rewrite session); run it once those land.)

**Status update (2026-07-01): PHASE 2 COMPLETE.** All phase-2 items are DONE or explicitly SKIPPED-with-reason
above. Phase-2 net ≈ -2,600 lines. What remains → #670 (or a final tail pass): the rest of the gin layer
(CredentialType consts now mostly unreferenced, remaining ginmw surface), the deferred interface collapses
(Stripe listers, derefStr dedupe), the untouched item-12 tail listed above (fx.NoOpProvider,
BuildResumeSubscription, RateLimitStore.Reset, snapshotTx, SetSolanaLifecycleForTest, internal/http/response,
webhookutil re-exports), and the entitlements facade question.

## Acceptance
`deadcode ./cmd/openrails` (cross-checked against integration-tagged files and pkg/embedded roots) reports no
production-dead symbols in the listed areas; no route-table or behavior change; net ≈ -7k lines.

---

# #667: production gate: payment-provider secrets must not persist plaintext at rest

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Fail-closed boot gate on the DB-backed
(fallback) secret store: `enforceEncryptionPosture` in `internal/merchantsecrets/store.go` (buildDBSecretStore),
gated by new `config.RequiresSecretEncryption()` — the exact same `!IsDev()` env signal as `RequiresRLS()`
(#227). Non-dev + secret_backend=db + no ENCRYPTION_MASTER_KEY ⇒ boot refused with an actionable error (names
ENCRYPTION_MASTER_KEY and secret_backend=vault); dev boots with one loud PLAINTEXT warning. All four boot
paths (standalone http server, bootstrap manifest, bootstrap dump, embed/provision.go) funnel through
merchantsecrets.Build — verified the only production construction site of the DB store. Vault-backed store
unaffected (returns before the gate). Solana WriteRestrictedSecretStore KEPT: now dev-only-reachable, but
Solana private keys are self-custody funds so plaintext is refused even where dev allows it for the rest.
Tests: store_test.go (posture matrix + env-signal pin vs RequiresRLS) and store_integration_test.go
(Build-level: prod+db+nokey refused / prod+vault+nokey boots via fake Vault / dev+db+nokey warns /
prod+db+key round-trips ciphertext-at-rest) — all green.

## Metadata
- Category: security
- Status: done
- Passes: true

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
- [x] Fail closed in non-dev: DB-backed secret store selected + disabled encryptor ⇒ refuse boot (mirror the
      EnforceRLSPosture pattern). Vault-backed store: no change.
- [x] Keep dev/test ergonomics: explicit dev-mode escape hatch, loudly logged.
- [x] Integration test: prod-ish config with DB store and no master key refuses boot / cannot write a
      Stripe/NMI/CCBill credential.

## Acceptance
A production deployment cannot silently store any payment-provider credential or webhook secret in cleartext;
dev keeps working with an explicit opt-out.

---

# #668: CCBill webhook auth bypassed by global test_mode (+ webhook trust-flag hygiene)

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Bypass now gated by `ccbillWebhookIPAllowed`
(one helper, all three ingestion sites): requires test_mode AND no environment=live CCBill row in
`openrails.payment_provider_accounts` (new `merchants.Service.HasLiveProviderAccounts`, cross-merchant probe on
the same trust boundary as ResolvePaymentProviderAccountByIdentity); probe failure / nil config / nil merchants
fail closed to the allowlist. Rationale: CCBill creds carry no intrinsic test/live marker and ValidateRailSet
forces config-declared environment to equal test_mode, so the persisted catalog rows are the only per-account
signal that survives a live deployment flipping test_mode on. CCBill direct-dispatch events no longer stamp
SignatureValid=true (nil now, matching the River path; re-verified only NMI/Stripe Apply gate on it). GUC-reset
failure in db_pgx.go release() now warns AND closes the conn so the pool destroys it rather than reusing a conn
that may still carry a merchant GUC. Tests: handlers/webhook_ccbill_test.go (4 tests, green).
FOLLOW-UP FIX (2026-07-01, tests/ suite): checkout's scoped CCBill lookups hardcoded environment="live"
(merchant_rail_secrets.go), so a sandbox deployment (test_mode ⇒ environment=test rows per ValidateRailSet ⇒
the exact posture this issue requires) could never resolve its CCBill provider account — now derived via
config.ExpectedProviderEnvironment(IsTestMode()). NOTE: the NMI/Stripe/solana scoped lookups still hardcode
"live" (checkout_session.go:143, merchant_rail_secrets.go resolveNMIClient, vault_service.go:268,
merchants/credentials.go LoadStripeCredentials/LoadNMIWebhookSigningSecret/LoadNMITokenizationConfig,
solana recurring/wiring.go) — same latent sandbox-posture mismatch, needs its own sweep.

## Metadata
- Category: security
- Status: done
- Passes: true

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
- [x] Bind the CCBill IP-check bypass to the CCBill account's sandbox posture (or refuse test_mode when a live
      CCBill account is configured) — never the global test_mode flag.
- [x] Stamp CCBill events `SignatureValid=false` (or a distinct `ip_allowlist` verification kind). (Left nil —
      never claimed — matching the River path's unverified Prepared.QueueArgs.)
- [x] Log GUC-reset failure in `release()` (or use tx-scoped set_config). (Warn + discard the conn; tx-scoped
      set_config doesn't fit — the lazy conn's GUC spans multiple statements outside any tx.)
- [x] Test: live-mode CCBill + test_mode=true still rejects webhooks from non-CCBill IPs.

## Acceptance
No configuration accepts CCBill webhooks from arbitrary IPs while live credentials are configured; the
SignatureValid flag never claims verification that didn't happen.

---

# #669: rail capability descriptor registry — collapse the 36-point dispatch scatter

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Registry lives in the existing rail-identity
home `internal/modules/payments/rails` (registry.go): one `Descriptor` per rail (nmi, ccbill, stripe, solana,
paypal) with fields Rail, DisplayName, HasProviderAccounts, HasRemoteCustomer, SupportsChargeSavedMethod,
OpenRailsDrivenDunning, RenewalGraceEligible, AutoBilled (func field — NMI's answer depends on vault presence),
CredentialKeys ([]{Name, MerchantWritable}; solana private_key is MerchantWritable=false). Compile-time
completeness via UNKEYED in-package struct literals (adding a Descriptor field fails to compile until every
rail declares it); registry_test.go forces every enum rail, pins every per-rail fact the old switches encoded,
and pins Lookup normalization (mobius does NOT resolve — post-#630 it's a provider-account name).
Collapsed all 12 descriptor-portable switch sites from the inventory below. Webhook ingress got the routing-
TABLE port only: globalWebhookIngress (handlers/webhook.go) + serviceWebhookIngress (service_webhooks.go)
replace the two top-level rail switches; posture gates/credential loading stayed put; the apply layer was
already registry-driven (#296/#675); processResolvedMerchantWebhook/processProviderAccountWebhook if-chains
deliberately LEFT (per-surface credential/#641-account/#668-posture logic, not routing). reconcile now iterates
`rails.All()` filtered on HasProviderAccounts (same order as the old hardcoded loop); RailFetcher untouched.
Legacy names (railHasRemoteCustomer, subscriptionProviderAutoBilled, RenewalGraceEligibleRail,
OpenRailsDrivenDunning) kept as thin registry delegates so their existing pinning tests prove behavior
preservation unchanged. Verified: go build+vet (both tags), repo-wide unit suite, integration suites for
webhooks/checkout/money/river/reconcile(+converge)/subscriptions/intents/merchants/handlers/pkg-service +
tests/ — green. (One-off parallel-package "tuple concurrently updated" role-provision flake on the shared DB
reproduced pre-change semantics; every package green when run normally.)

## Metadata
- Category: architecture
- Status: done
- Passes: true

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

## Inventory (2026-07-01, re-verified in source before refactor)
DESCRIPTOR-PORTABLE (facts/predicates — collapse into registry):
1. rail_customer_service.go:72 `railHasRemoteCustomer` → HasRemoteCustomer
2. reconcile/unknown_orchestration.go:58 `railExposesRemoteCustomer` (same fact, duplicated) → HasRemoteCustomer
3. reconcile/unknown_orchestration.go:68 `railToProvider` + :96 hardcoded 4-rail loop → registry iteration
4. money/collection.go:75 ChargeSavedMethod exclusion (ccbill/solana) → SupportsChargeSavedMethod
5. river/jobs_dunning.go:660 `subscriptionProviderAutoBilled` (ccbill always; nmi vault-less) → AutoBilled func field
6. subscriptions/email_service.go:574 `railDisplayName` → DisplayName
7. merchants/payment_provider_config.go:365 `supportedPaymentProvider` → HasProviderAccounts
8. merchants/payment_provider_config.go:374 `paymentProviderCredentialKeys` → CredentialKeys (merchant-writable subset)
9. merchants/secrets.go:134 `NormalizeProviderAccountSecretKey` key sets → CredentialKeys (full set)
10. merchants/secrets.go:125 `SecretWritable` solana/private_key carve-out → CredentialKey.MerchantWritable=false
11. rails/nmi.go:27 `OpenRailsDrivenDunning` (nmi+solana) → descriptor field
12. subscriptions/dunning.go:283 `RenewalGraceEligibleRail` (nmi+stripe) → descriptor field

WEBHOOK INGRESS (3 layers): apply layer is ALREADY registry-driven (webhooks/dispatcher.go
WebhookHandlerRegistry, #296/#675 — no work needed). Top-level rail switches in handlers/webhook.go:92-117
(global surface) + pkg/service/service_webhooks.go:38-53 → routing-TABLE port only.
processResolvedMerchantWebhook (:142) + processProviderAccountWebhook (:246) if-chains LEFT: each arm is
per-rail credential loading / #641 account-identity extraction / #668 posture gates, not simple routing.

LEFT AS-IS (behavior dispatch or ownership elsewhere): checkout service.go:392,417,2324 +
session_service.go:499,554,630 (per-rail executors/validators — deliberate per the #521 drift comment);
config/config.go:484,950 (config-shape validation; config is imported BY rails — cycle);
subscriptions/cancel_mode.go:94 CancelModeFor (rail-fact candidate but drags nmiDeletePending along; future);
admin_service.go:218 + admin_payments.go:246,335 + jobs_subscription_manage.go:105 (per-rail cancel/refund
executors); the ~20 `rails.IsNMI` guard sites around NMI-only machinery (vault, deferred delete, liveness);
build_runtime.go client construction (acceptance keeps it); webhookutil.CanonicalRail (legacy mobius alias);
reconcile RailFetcher (kept by design).

INCONSISTENCIES FOUND (old switches disagreeing — not silently resolved):
- A. Stripe display name hit railDisplayName's generic fallback → "STRIPE" (email_service.go:586); every
  other rail is curated. Descriptor sets "Stripe" — deliberate display-only fix, noted here not silent.
- B. subscriptionProviderAutoBilled("stripe")=false although Stripe rebills itself (OpenRailsDrivenDunning
  excludes stripe for exactly that reason). Harmless: the dunning worker only processes OpenRails-driven
  cohorts. PRESERVED as-is in the descriptor (AutoBilled=never for stripe), documented on the field.
- C. money.ChargeRequest.AmountCents is RAIL MINOR UNITS (whole yen for JPY) but the NMI collection adapter
  feeds it to nmi.SaleParams.Amount whose wire format is two-decimal cents — divergent for zero-decimal
  currencies on NMI. USD unaffected; PRESERVED (sibling of #671-1g; no NMI merchant bills JPY).
- D. paymentProviderCredentialKeys("solana")=nil vs NormalizeProviderAccountSecretKey accepting
  solana/private_key: deliberate (operator-only secret hidden from merchant credential-status view) — now
  expressed as MerchantWritable=false rather than two disagreeing switches.
- E. money/collection.go: known-but-unsupported rails (paypal) previously fell through to "no adapter
  configured"; now get the explicit "does not support invoice collection" error. Unknown rail strings keep
  the adapter-not-found path.

## Tasks
- [x] Inventory pass: enumerate every rail switch (list above is the audit's; re-verify). → DONE, see Inventory.
- [x] Define the descriptor struct + registry; port the boolean predicates first (remote-customer, auto-billed,
      collection exclusion), then credential keys + display names, then the 3-layer webhook dispatch.
      → DONE in that order; webhook dispatch = routing-TABLE port only (see Status — the merchant/provider-
      account surface if-chains are credential/posture logic, deliberately left).
- [x] Compile-time completeness: constructing the registry requires every field per rail. → unkeyed in-package
      struct literals (a new Descriptor field breaks every rail's literal at compile time) + registry_test.go
      forces every enum RAIL and non-zero required fields.
- [x] Leave RailFetcher as-is (it already has the right shape); reconcile switches route through the registry.
      → railToProvider + railExposesRemoteCustomer + the hardcoded 4-rail loop all registry-backed.

## Acceptance
Adding a rail touches: the enum, its integrations package, ONE descriptor file, build_runtime wiring. The
boolean-predicate/credential/display/webhook-routing switches are gone; grep for `case "stripe"`-style rail
switches outside the registry returns ~zero.

---

# #670: single HTTP stack — drop the gin layer, serve the neutral mux everywhere

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Standalone now serves the SAME neutral
net/http stack as embedded; every gin internal deleted.

## Metadata
- Category: refactor
- Status: done
- Passes: true

## What shipped (2026-07-01)
- **Flip:** cmd/openrails → `embedded.StandaloneHandler` (new, gin-free, pkg/embedded/standalone.go) →
  `internal/http` server.New, which now assembles an http.ServeMux + the neutral chain
  (RecoverHTTP → RequestLogHTTP → SecurityHeadersHTTP → CORSFromSourceHTTP(dynamic control-plane origins,
  NEW) → BodyLimitHTTP → ResolveMerchantHTTP → billingauth.Optional → RateLimitHTTP) — same order the gin
  engine ran. `internal/bootstrap/ginboot` → `internal/bootstrap/serverboot` (gin-free composition root;
  integrationharness + tests suite use it).
- **Ports to neutral (were gin-only):** self-service + customer-treasury registrations →
  `internal/http/routes/self.go` (RegisterSelfServiceRoutes/RegisterCustomerTreasuryRoutes on router.Router);
  delegated middlewares + Principal/keys → `internal/http/middleware/delegated_neutral.go`
  (DelegatedSelfRequired/DelegatedPrincipalRequired/CustomerScopeRequired/EnsureCustomerPermissionGroup/
  RequirePermission; DelegatedContextKey etc. — handlers retargeted); control-plane /auth mount = direct
  ServeMux registration of RouteSpecs (they are native ServeMux patterns; the gin `{}`→`:` translator died);
  meta/health/captcha endpoints → net/http (captcha handlers shared via embedhttp.CaptchaStatusHandler/
  CaptchaClientScriptHandler); embedhttp.NewSelfHandler (neutral self surface used by the shim). Root banner
  registered as "GET /{$}" so ServeMux keeps gin's exact-root behavior.
- **Route parity PROVEN:** dumped the gin engine's 206-route table BEFORE the cut (gin engine.Routes(), real
  boot); golden checked in at internal/integrationharness/testdata/standalone_route_surface.txt (129 billing
  routes; the 77 /auth routes are AuthKit-owned and asserted dynamically). Permanent test
  `TestStandaloneRouteSurface` (integrationharness) boots the real standalone server and requires
  RouteTable() == golden AND /auth == controlplane.RouteSpecs() verbatim (Server records every registered
  pattern via router.NewMuxRecorded / Server.handle).
- **pkg/embedded/gin shim (cozy-art compat, public API unchanged):** StandaloneHandler/Handler delegate to
  embedded.StandaloneHandler; SelfHandler delegates to embedhttp.NewSelfHandler (gin-free);
  MountHandler/RegisterAPI/MountOptions/ProviderRoutes/With* unchanged — gin now appears ONLY in
  api.go's RegisterAPI (`gin.WrapH` + group.Handle) and pkg/authprovider/ginauth (public, kept).
- **Deleted:** internal/http/middleware/ginmw (whole pkg), internal/http/router/ginrouter,
  internal/http/request/ginreq, internal/http/routes/ginroutes (+tests, re-ported neutrally),
  internal/controlplane/ginroutes, internal/http/response (helpers inlined: ginauth writes
  api.SimpleErrorResponse directly), gin half of internal/http (newPublicEngine + gin routes_*.go rewritten
  neutral), ginboot. Net across the cut ≈ -2,900 gin-coupled prod+test lines, ~+1,900 neutral lines
  (ports + re-ported tests + parity test).
- **Deliberate behavior deltas (audited):** (1) neutral rate-limit/captcha error bodies now emit the
  canonical pkg/api envelope on BOTH deployments (gin already did; embedded previously used ad-hoc
  `{"error":...}` maps) — envelope unification, standalone byte-compatible; (2) ServeMux answers
  wrong-method with 405+Allow where gin returned 404, and gin's 301 trailing-slash redirect is gone
  (matches the embedded surface's existing behavior); (3) global BodyLimit now exempts webhook paths on
  standalone (neutral matcher; webhook handlers keep their tighter per-rail caps); (4) embedded self
  handler now honors probed RouteCapabilities for provider routes (was rails-only), matching standalone.
- **gin linkage:** `go list -deps ./cmd/openrails | grep gin-gonic` ⇒ EMPTY — the standalone binary no
  longer links gin. gin remains in go.mod ONLY for the pkg/embedded/gin shim (RegisterAPI/WrapH) +
  pkg/authprovider/ginauth; full go.mod removal happens when cozy-art migrates off the gin shim.
- **Tests:** ginroutes self-surface tests re-ported to internal/http/routes (neutral); ginmw
  delegated/ratelimit/security coverage re-ported to internal/http/middleware
  (delegated_neutral_test.go, ratelimit_http_test.go additions incl. ClassifyBucket, http_base_extra_test.go);
  internal/http captcha/meta/self tests rewritten on the mux; tests/ + integrationharness ad-hoc gin engines →
  router.NewMux + middleware.ChainHTTP. Dropped without port: ginmw principal_test's user-session
  principal cases (its subject, UserSessionAdminPrincipalRequired, was already deleted in #666).
- **#666 tail deletions done here:** fx.NoOpProvider, solana/subscriptions.BuildResumeSubscription,
  RateLimitStore.Reset, MoneyService.snapshotTx(+accountSnapshot), webhookutil
  ParseStripeSignatureHeader/ParseNMISignatureHeader re-exports. SKIPPED:
  checkout.SetSolanaLifecycleForTest (grep-dead, but session_service.go was hot under the concurrent #674
  agent — delete in a later pass).

## Tasks
- [x] Standalone serves the embedhttp/neutral mux; gin hosts keep working via `gin.WrapH` (pkg/embedded/gin
      is now a thin delegating shim — cozy-art public API preserved).
- [x] Port the few live gin-only middlewares to the neutral chain; delete the gin packages.
- [ ] Remove gin from go.mod — deferred until cozy-art drops pkg/embedded/gin (shim + ginauth are the only
      remaining importers; the standalone binary already links zero gin-gonic).
- [x] Route-surface parity test green (TestStandaloneRouteSurface: neutral table == pre-cut gin golden) +
      integration suites (see verification note).

## Verification (2026-07-01)
- go build ./... + -tags integration green; go test ./... (repo-wide unit) fully green.
- Integration: TestStandaloneRouteSurface GREEN (129/129 billing routes == gin golden; 77 /auth ==
  RouteSpecs). pkg/embedded (+/gin) integration green. internal/integrationharness green except
  TestNativeCatalogBundleIncludesHTTP ("membership tier requires a recurring price" — concurrent catalog
  validation churn, not transport). tests/ suite: green except concurrent-agent areas — ccbill IP-gate
  hardening (webhook.go, theirs), refund whole-cents validation (intents/refund.go, theirs), subscription
  dunning lifecycle expectations (modules/subscriptions churn), TestGetProductsEndpoint interval "720h" vs
  "30d" (billing-cycle-hours formatting drift, not transport).
- REAL BUG the flip surfaced + fixed: the neutral query binder (request.go decodeTaggedValues) did not
  recurse into nested filter structs (query.QueryOptions[T].Filters), silently dropping e.g. the
  /v1/merchant/payments?user_id= filter, and lacked time.Time(time_format)/TextUnmarshaler(uuid) support —
  gin recursed. Fixed to gin parity + pinned by TestHTTPBindQueryNestedFiltersTimeAndUUID; the admin
  payments list-filter integration tests now pass. Also fixed two stale tests: http_handler_options_test
  (#666 migration forgot Assembler.Authenticator → panic aborted the whole tests/ package) and
  embedded_http_surface_test (pre-#650 expectation that the global /v1/webhooks surface is absent).

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
**Status:** RE-OPENED + RESCOPED 2026-07-01 (Paul): parts A-E of the original scope LANDED (see Progress — one
probe primitive, silent-lapsed lane deleted, single writer per invariant, coverage-derived confirmed-absence
gate, spec/states reconciled). What remains is (1) the one original open task — the mirror-writer refactor of
`engine.go`/`diff.go` — and (2) the EXTENDED end state below (from the 2026-07-01 design review): one
subscription state machine, everything else reduced to inputs. Webhook fetch-and-converge is the sibling issue
#684 — do them in sight of each other; the decider they feed is the same.

## Extended target (rescope 2026-07-01)

The original issue consolidated the *lapsed-cohort* machinery. The end state goes further: ONE subscription
state-machine decider whose transitions are evidence-gated (#664 doctrine), where every remaining plane is an
INPUT, not a writer:

- **Inputs (produce evidence, never move state):** bulk pull snapshots, per-sub probes
  (`unknown_probe.go`), webhook-triggered fetches (#684), first-party billing outcomes (dunning results,
  intents verify legs), the coverage/confirmed-absence gate.
- **One decider (moves state):** the LIFE/`ResolveUnknownFromSnapshot` core, generalized: given (current row,
  evidence bundle), emit the transition — active/renew, past_due+dunning, unknown-park, adopt-period-end,
  cancel-with-certainty. Scheduler ordering structurally cannot change outcomes (any plane running first only
  adds evidence or parks).
- **Appliers demoted:** the legacy engine's remaining appliers/selective-apply become mirror writes + decider
  invocations — the mirror-writer refactor IS this demotion; no plane applies domain state directly.

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

- [x] Extract the verify-subscription primitive from `jobs_subscription_liveness.go`; port
      `unknown_reconcile.go` onto it (one probe, one outcome set). (Landed inverted per the target:
      `ResolveUnknownFromSnapshot` stayed THE decision core; liveness's probing became SNAPSHOT SOURCES —
      `internal/reconcile/unknown_probe.go` — feeding it. One outcome set: UnknownOutcome, + Adopted.)
- [x] Delete the silent-lapsed lane (`ListSilentLapsedSubscriptions` + the liveness cohort scan) once #664 routes
      the cohort through LIFE.
- [x] Single writer per invariant: stop emitting `derive.*`/`consistency.*` from the pull engine; move any check
      not already covered by a converge pass into one.
- [ ] Mirror-writer refactor of `engine.go`/`diff.go`; `pull.*` findings only. (The "pull.* findings only" half
      landed with the single-writer move; the appliers/selective-apply refactor itself is still open.)
- [ ] RESCOPE: extract the decider seam — one function (row, evidence) → transition, called by LIFE sweep,
      unknown-resolution, and (later) #684 webhook-triggered fetches; planes stop writing subscription state
      directly.
- [ ] RESCOPE: evidence bundle type unifying what the planes produce today (pull snapshot / probe result /
      charge evidence / coverage proof) so the decider's inputs are explicit and testable as data.
- [ ] RESCOPE: property test — for a fixed evidence bundle, ANY interleaving/ordering of plane execution yields
      the same terminal state (the #664 acceptance, generalized to the whole machine).
- [x] Watermark-derived confirmed-absence gate; retire or import-scope `reconciliation_state`. (Implemented
      COVERAGE-derived, not watermark-derived — absence proofs need exhaustive pulls, not event-window freshness.)
- [x] Spec/code finding-state reconciliation (docs/consistency-invariants.md §8).

## Progress

- 2026-07-01 — Parts A/B/C IMPLEMENTED, uncommitted (probe-unification + silent-lapsed-lane deletion still open,
  sequenced after other in-flight work).
  - **A (single writer):** the pull engine now emits `pull.*` only. PS-9 (`derive.grant_effect.mismatch`) moved
    into the DERIVE pass as two set queries (`ListActiveSubsMissingEntitlementProjection` — repair = derive-1 for
    exactly the missing features; `ListDeadSubsWithLiveEntitlements` — repair terminates grants + retracts
    windows; `unknown` excluded, #664). PS-10 (`life.provider_intent.stuck`) moved into the LIFE pass (sweep
    scope; mode-parked = informational; recovered intents auto-resolve SUBJECT-FIRST via
    `AutoResolveRecoveredStuckIntentFindings` — converge has no run-driven vanish sweep, a pre-existing gap for
    its other surface-only findings). PS-8 is snapshot-dependent so it stays PULL:
    `consistency.duplicate.subscription` → `pull.subscription.duplicate` (migration 060 renames ledger rows +
    rekeys legacy PS-9 subjects). Deleted: diffEntitlements, stuck_intents.go, entitlement local-state loading,
    standalone Grant/Revoke apply actions, AutoResolveVanishedAllProviders. Net deletion in internal/reconcile.
  - **B (automatic gate):** `reconcile.MarkReconciledSourceDomains` (coverage.go) flips `reconciliation_state`
    from PROOFS: completed provider sections' `SnapshotCoverage` (exhaustive, never event-window watermarks),
    only when EVERY declared rail account is covered (multi-account rails / empty catalog prove nothing; ratchet,
    never unset). Wired into provider-refresh (per merchant, before the converge pass) and pull-provider
    (replaces the blind full-head flip; --insert --overwrite full-head pull = the manual bulk-import setter).
    `payments` needs full-history coverage so only full-head pulls prove it; `grants` is never pull-provable.
  - **C (spec/states):** §8 rewritten to the implemented vocabulary (reconcile_required / requires_review /
    auto_fixed / fixed / ignored + resolutions); held = reconcile_required with `source_domain` in evidence,
    indeterminate = failed-run/requires_review/`unknown` park. NO new DB state needed — no expressiveness gap
    found. §3.2/§5/§10 updated for the gate + rename.
  - Tests: engine_test reworked (PS-9/PS-10 gone, PullProofs unit test), stuck-intent integration test ported to
    converge (dedicated merchant), new converge tests for both mismatch directions + the gate
    (held EXCESS proceeds after an exhaustive-pull flip; non-exhaustive/multi-account never flips). Full unit
    suite + internal/reconcile, converge, river provider-refresh integration suites green; `task sqlc` clean.

- 2026-07-01 — Parts D/E IMPLEMENTED, uncommitted (verify-subscription primitive + silent-lapsed lane deletion).
  - **One decider:** `ResolveUnknownFromSnapshot` stays the decision core; the #367 liveness worker's
    capabilities were ported INTO it as snapshot sources + doctrine, then the worker was deleted.
    New `internal/reconcile/unknown_probe.go`: `SubscriptionProber` (NMI = query.php sales-by-order-ref +
    v5 recurring GET; Stripe = the existing `subscriptions.StripeLivenessProber`, kept) producing per-sub
    `RemoteSnapshot`s; the orchestration probes ONLY rows the bulk pull left unreachable (NULL period end,
    evidence outside window, non-exhaustive roster; never fanned out when the bulk fetch failed).
  - **Doctrine ported into the core:** new `UnknownOutcomeAdopted` + lifecycle `ResolveAdopted` — roster
    alive w/ future next billing and NO charge re-anchors the period END only (start untouched → DERIVE
    projects nothing): renewed now requires a VERIFIED charge (was: roster-active ⇒ Renewed, which reset
    period start and let DERIVE grant access without a charge). Roster past_due (stalled NMI record /
    Stripe past_due) → dunning within the window, cancel + deferred delete beyond (#679 kept green).
    24h `renewalAlignmentSlack` on charge classification + backfill (NMI day-boundary billing).
  - **Deleted:** `jobs_subscription_liveness.go` (+ integration test), the `runSubscriptionLiveness` lane,
    the legacy-worker River registration (queued `openrails.subscription_liveness` jobs from old deploys
    would now error — none scheduled, kind retired), `ListSilentLapsedSubscriptions` (+ repo
    `ListSilentLapsed`), `LivenessProbeSlack`. `nmi.SaleProbeResult` extended (dates/amounts/decline txn id)
    so probe evidence is verbatim. `ListUnknownSubscriptions` → `ORDER BY current_period_ends_at NULLS FIRST`
    (NULL-period legacy rows must not starve under the LIMIT; they're only resolvable here).
  - Deliberately dropped: charged-repair for rows with NO rail_subscription_id (liveness could order-ref-probe
    them; unreachable in practice — signup stamps both ids) and probe-driven Stripe `incomplete`/`paused` →
    past_due (probe uses the bulk `normalizeStripeStatus`, those park unknown until Stripe self-resolves).
  - Tests: liveness scenarios ported (renewed+backfill exactly-once, decline→past_due, roster-gone→cancel
    +revoke NO intent, stale-decline→cancel WITH intent, adopt-without-access, NULL-period via probe,
    unreachable stays unknown); probe→snapshot mapping unit-tested against a fake NMI HTTP server
    (direct-post counter stays 0). `go build ./...` + full unit suite + reconcile/converge/river
    integration (`-tags integration -count=1`) green; net ≈ −450 lines.

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

NOTE (2026-07-01): for the CROSS-MERCHANT wallet, the SaaS strategy chose Stripe-Connect saved-PM reuse
(Rank 1, ~/openrails-saas/payments-multi-merchant-wallet-and-payfac.md) — Hyperswitch is NOT that path;
it stays a per-merchant vault-tech option, and any shared use falls under openrails-saas #23
(vault/state inseparability).

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

