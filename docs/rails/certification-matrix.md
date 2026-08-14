# Rail certification matrix

What each payment rail actually does, and how well we know it.

A cell's status answers one question: **what evidence do we have that this flow
works on this rail?** Not "did someone write the code" — code is necessary and
never sufficient. Providers' documentation is wrong often enough that an
unverified wire is a guess, so this document distinguishes guesses from
observations and refuses to round the former up.

## Statuses

| Status | Means | Evidence required |
|---|---|---|
| `live-verified` | The wire was exercised against the provider's **real** gateway — production account, real money or real account state. | A dated probe record or a test run naming the account posture. Covers only the exact operation probed. |
| `sandbox-verified` | The wire was exercised against the provider's **sandbox / test-mode / devnet** endpoint by a named automated test. | Test function name + the CI lane that runs it + its cadence. |
| `modeled` | Code is complete and covered by hermetic tests (fake HTTP, recorded fixtures), but **no real provider response has ever confirmed the shape**. | Nothing beyond source. This is the default for anything not verified. |
| `limited` | Works, with a named restriction that changes what a customer can do. The restriction is a fact of the rail or a deliberate design choice — not a code gap. | The caveat must name the restriction. Verification level appears in the evidence line. |
| `unsupported` | The flow does not execute on this rail. Marked **(guarded)** when a request is refused with an error, **(silent)** when the input is accepted and ignored. | — |

`unsupported (silent)` is a defect class, not a design: it means a merchant can
declare something the rail will quietly drop. Those cells are called out below.

### What demotes a status

- **A wire change demotes.** Changing a rail adapter's request or response
  shape drops every affected cell to `modeled` until it is re-verified. This is
  the rule the PR process below enforces.
- **A dead test demotes.** A `sandbox-verified` cell whose test is deleted,
  permanently skipped, or has not run green in 60 days decays to `modeled`.
- **Verification does not generalize.** `live-verified` on one operation says
  nothing about its siblings. A verified `cancelSubscription` does not certify
  `refundTransaction` on the same endpoint.

## Verification lanes

Everything in the `sandbox-verified` and `live-verified` columns comes from one
of these. Nothing else in the repo touches a real provider.

| Lane | Workflow | Cadence | Reaches |
|---|---|---|---|
| NMI sandbox | `.github/workflows/live-gated-integration.yaml` job `nmi-sandbox` | weekly, Mon 06:00 UTC | real NMI sandbox gateway |
| Live invoice collection | same file, job `live-invoice-collection` | weekly | Stripe test mode + NMI sandbox |
| Stripe Model-B upgrade | same file, job `stripe-model-b` | weekly | Stripe test mode |
| Solana devnet | `.github/workflows/solana-devnet-integration.yaml` job `devnet-service-layer` | daily 07:00 UTC | Solana devnet, real USDC |
| Solana sustained rebill | same file, job `devnet-multirebill` | on demand (~3h) | Solana devnet, real USDC |
| Hermetic integration | `.github/workflows/ci.yaml` | every PR | Postgres + Redis testcontainers, fake provider HTTP |

Each live-gated test `t.Skip`s when its credential secret is absent, so a green
run is not by itself proof the lane executed. Several provider-reaching tests
exist outside these lanes (the Stripe catalog `TestLive*` pair, the Solana
devnet tier-change and failure-path tests, `TestSolanaDevnetMoneyMovementProof`)
— no workflow runs them, so they back no cell. The Stripe pair is at least no
longer invisible: or#896 put both behind the `stripelive` build tag (the
convention `internal/modules/catalog`'s live Stripe tests already used), so they
are compiled only when a lane asks for them instead of riding a default
`go test ./...` held back by an env check.

**CCBill has no automated lane.** Every CCBill cell below is either hermetic or
rests on a single dated manual probe.

## Checkout and enrollment

| Flow | NMI | CCBill | Stripe | Solana |
|---|---|---|---|---|
| One-off purchase | `sandbox-verified` | `unsupported` (guarded) — rail is subscription-only | `modeled` | `modeled` |
| Subscription enrollment | `sandbox-verified` | `modeled` | `modeled` | `sandbox-verified` (devnet) |
| Free trial (zero-amount first phase) | `unsupported` (guarded) | `limited` — validated inbound, never originated | `modeled` | `unsupported` (guarded) |
| Paid introductory first phase | `unsupported` (guarded) | `limited` — validated inbound, never originated | `unsupported` (guarded) | `unsupported` (guarded) |

- **One-off, NMI** — `internal/integrations/nmi/payments.go:59` (v5 `payments/sale`). last-verified: weekly / env: sandbox / how: `TestNMILiveLifecycleE2E` step 5.
- **One-off, CCBill** — refused at `internal/modules/checkout/service.go:485`. Subscription-only rail.
- **One-off, Stripe** — hosted Checkout `mode=payment`, `internal/modules/checkout/service.go:1264`. env: code-read + hermetic / how: `TestCheckoutSessionStripeRedirect` (fake Stripe transport). The redirect leg is covered; the `checkout.session.completed` activation leg is deliberately not, per the test's own header note.
- **One-off, Solana** — Solana Pay, `transfer_request` and `transaction_request`. A one-time devnet observation exists (2026-06-29, `TestSolanaDevnetMoneyMovementProof`, confirmed 1 USDC transfer), but **no lane re-runs it**: the test is gated on `OPENRAILS_TEST_SOLANA_PRIVATE_KEY` and its on-chain leg is non-fatal, so it is not standing evidence. Stays `modeled`.
- **Enrollment, NMI** — classic `recurring=add_subscription`, atomic first-charge + enroll. last-verified: weekly / env: sandbox / how: `TestNMILiveLifecycleE2E` steps 6-7.
- **Enrollment, CCBill** — FlexForm redirect; the subscription exists only once `NewSaleSuccess` arrives. No CCBill lane exists, so the outbound URL and the inbound payload are both pinned against hand-authored fixtures.
- **Enrollment, Stripe** — hosted Checkout `mode=subscription`; membership is created by fetch-and-converge, not from the webhook payload.
- **Enrollment, Solana** — `init_subscription_authority` + `subscribe`, period 1 pulled atomically. last-verified: daily / env: devnet / how: `TestDevnetLifecycle/FastPlan`.
- **Trials** — the catalog's first-phase (`Price.GetTrial`) has exactly two readers: Stripe checkout and CCBill webhook amount validation. NMI and Solana enrolment never read it, so a trial declared against either is now **refused at catalog push** (or#896): `resolveProviders` consults the rail registry's `SupportsCatalogTrial` and fails the price create with an error naming the limitation, so the price never reaches the DB (`pkg/service/catalog_providers.go`; `TestCatalogPublishRefusesTrialOnRailsWithoutFirstPhase`). It used to be accepted and dropped, and the subscriber was charged the full amount immediately. Stripe refuses a *paid* intro explicitly (`service.go:1211`); a zero-amount trial becomes `subscription_data[trial_end]`. CCBill trial terms live in the FlexForm — OpenRails only validates the billed amount against the catalog trial.

## Billing lifecycle

| Flow | NMI | CCBill | Stripe | Solana |
|---|---|---|---|---|
| Rebill / recurring charge | `modeled` | `modeled` | `limited` — provider-driven, ingest only | `modeled` |
| Dunning / retry | `modeled` | `limited` — provider owns cadence | `unsupported` (by design) | `modeled` |
| Cancel — user | `sandbox-verified` | `live-verified` | `modeled` | `limited` — dedicated endpoints only (guarded elsewhere) |
| Cancel — merchant/admin | `modeled` — durable intent, verify-not-decline | `live-verified` (wire, 2026-07-03) — same intent as the user cancel | `modeled` | `unsupported` (guarded) |
| Cancel — provider-initiated | `modeled` | `modeled` | `modeled` | `modeled` |
| Tier change — upgrade | `modeled` | `modeled` | `modeled` | `modeled` |
| Tier change — downgrade | `modeled` | `unsupported` (guarded) | `modeled` | `modeled` |
| Bulk plan migration (reprice) | `modeled` | `unsupported` — needs user action | `modeled` | `unsupported` — needs user action |

- **Rebill, NMI** — classic `recurring=rebill_subscription`, driven by our dunning worker. Heavily covered by integration tests, but **never live-verified**: an NMI sandbox cannot advance time, so no lane has ever observed a real rebill.
- **Rebill, CCBill** — CCBill owns the schedule; OpenRails follows `RenewalSuccess`/`RenewalFailure`. Retry pacing is read from CCBill's own `nextRetryDate`, capped at 72h.
- **Rebill, Stripe** — Stripe rebills and dunns itself; the dunning worker never processes Stripe cohorts (`OpenRailsDrivenDunning=false`). OpenRails ingests `invoice.paid` / `invoice.payment_failed` as wake-ups and converges from a fetched invoice.
- **Rebill, Solana** — `transfer_subscription` pull, driven by an hourly cranker. The daily devnet lane exercises the instruction only as part of the atomic subscribe bundle (period 1), so it proves the on-chain instruction lands — **not** that the cranker bills a second period. Recurrence across a real period rollover is covered only by `TestDevnetSustainedRebill` and `TestDevnetFailurePaths`, and **no scheduled lane runs either** (see findings). Missed periods are never back-billed by design; a delegate revocation is terminal and skips dunning.
- **Cancel (user), NMI** — deferred delete with an undo window, else immediate `DELETE /v5/subscriptions/{id}`. last-verified: weekly / env: sandbox / how: `TestNMILiveLifecycleE2E` step 9.
- **Cancel (user), CCBill** — DataLink `cancelSubscription`. last-verified: 2026-07-03 / env: **live production account** / how: manual probe, success envelope `<results>1</results>` confirmed on a long-dead subscription; pinned by `TestCancelSubscription_SuccessShapes`.
- **Cancel (user), Solana** — `limited`: the on-chain `cancel_subscription` path works and is covered by the daily devnet lane (`TestDevnetLifecycle/FastPlan`), but it is reachable **only** through the dedicated `solana-cancel-tx` / `solana-cancel` endpoints — a cancel is a transaction the subscriber's wallet signs. The rail-agnostic `POST /subscriptions/:id/cancel` used to queue a job that fell through the worker's default branch and failed permanently; since or#896 it is **refused synchronously (400) naming those two endpoints**, the service refuses it for any other producer, and an already-queued job is cancelled rather than retried forever (`TestCancelSubscriptionSolanaNamesDedicatedEndpoints`).
- **Cancel (merchant), NMI** — the admin path now rides the SAME `nmi_delete_subscription` intent as the user path (or#896, admin-origin): the local cancellation and the intent commit in one transaction, the `deletion_scheduled_at` marker holds while the rail side is unconfirmed, and an ambiguous provider outcome parks as `unknown_needs_verify` for the verifier instead of reporting a delete that may not have happened. It used to call the gateway synchronously with no intent and no verify leg, and an unresolvable PSP logged a warning and returned nil — the local row went terminal while NMI kept rebilling. `modeled`: hermetic fake gateway (`TestMerchantCancelNMIRidesDurableIntent`, `TestMerchantCancelNMIAmbiguousOutcomeParksForVerification`); the delete WIRE itself is the sandbox-verified one from the user path.
- **Cancel (merchant), CCBill** — the split-brain is closed (or#896): the admin subscriptions API used to refuse CCBill while the findings-queue `cancel_and_refund` action cancelled it. Both surfaces now queue the same admin-origin `ccbill_cancel_subscription` intent, drained through the DataLink SMS choke point. The cancel wire is the one confirmed live on 2026-07-03 (see *Cancel (user), CCBill*); the admin ENTRY POINT is pinned hermetically by `TestMerchantCancelCCBillDrivesTheLiveVerifiedCancel` alongside `TestFindingsQueueApproveCCBillCancelAndRefund`.
- **Cancel (merchant), Solana** — refused; a cancel is a transaction the subscriber's wallet must sign.
- **Cancel (provider-initiated)** — all rails converge from *fetched* provider truth rather than trusting the payload. NMI and Stripe treat a 404 as provider-confirmed-gone; CCBill consumes `Cancellation`/`Expiration`; Solana reads the chain.
- **Tier change** — NMI: immediate proration charge + new subscription, downgrade deferred to period end. CCBill: upgrade is an `originalSubscriptionId` FlexForm redirect, downgrade refused (`service.go:2229`). Stripe: Model-B anchor reset with `always_invoice`; **carries an explicit `TODO(#268)` saying the live invoice amount must be verified on a real Stripe test upgrade** — the `stripe-model-b` lane exists for exactly this. Solana: a single atomic on-chain transaction via the dedicated `solana-tier-change` endpoints.
- **Bulk plan migration** — CCBill and Solana are classified `capabilityUserAction` (`plan_migration.go:183`): the rail cannot be repriced server-side, so rows land `blocked` and are surfaced rather than automated.

## Money reversal

| Flow | NMI | CCBill | Stripe | Solana |
|---|---|---|---|---|
| Refund — full | `modeled` | `modeled` — wire provisional | `modeled` | `unsupported` (guarded) |
| Refund — partial | `modeled` | `modeled` — amount encoding is a guess | `modeled` | `unsupported` (guarded) |
| Chargeback / dispute ingestion | `limited` — webhook-only, unrecoverable if missed | `modeled` | `modeled` | `unsupported` — impossible on-chain |

- **Refund, CCBill** — the single weakest cell in this document, and deliberately marked so in the source. `internal/integrations/ccbill/subscription_management.go:280` carries a `WIRE PROVISIONAL` block: the request shape is modeled on the cancel pattern, never confirmed by a successful round-trip. Three specific unknowns remain — the success/partial/already-refunded result codes, whether `amount` is decimal dollars (assumed) or cents, and whether `transactionId` actually narrows the refund to that charge. Even the subaccount parameter name (`clientSubacc` vs `usingSubacc`) is unconfirmed. The one live datum is a 2026-07-03 safe fail-probe that returned `-7` against a 12-year-old transaction: it proves the request reaches CCBill and that the failure path moves no money, and nothing more. Every refund test asserts against hand-authored strings; the only *captured* production golden on this endpoint is `ViewSubscriptionStatus`. **Do not read this cell as "refunds work on CCBill."**
- **Refund verification, CCBill** — CCBill exposes no per-transaction refund read, only per-subscription counters, so an ambiguous refund can never be attributed to a specific charge. Non-zero counters stay `Ambiguous` for operator resolution.
- **Refund, Solana** — refused at `internal/http/handlers/admin_payments.go:284`. A pull-based on-chain rail has no reverse authorization; a refund is a fresh outbound transfer, which OpenRails does not model.
- **Chargeback, NMI** — `limited` for two reasons: it is the only NMI handler that trusts the webhook payload rather than re-fetching, and NMI's read APIs do not expose chargebacks at all (`internal/reconcile/nmi.go:60` declares `Chargebacks: false`). **A missed `chargeback.batch.complete` delivery is unrecoverable** — no backfill path exists. An unparseable batch body is logged and swallowed.
- **Chargeback, CCBill** — ingested from webhooks *and* recoverable from the DataLink chargeback export, so unlike NMI a missed delivery is repairable.

## Payment instruments

| Flow | NMI | CCBill | Stripe | Solana |
|---|---|---|---|---|
| Add / vault a payment method | `sandbox-verified` | `unsupported` (guarded) — provider owns the vault | `unsupported` (guarded) — portal-delegated | `unsupported` (guarded) — wallet, not an instrument |
| Update / delete a payment method | `modeled` | `unsupported` (guarded) | `unsupported` (guarded) — portal-delegated | n/a |
| Swap a subscription's payment source | `modeled` | `unsupported` (guarded) | `unsupported` | n/a |
| Account Updater | `unsupported` — events logged, no action | `unsupported` — invisible to us | `unsupported` — nothing consumed | n/a |
| Charge a saved method (arrears settlement, auto top-up) | `sandbox-verified` | `unsupported` — no adapter | `sandbox-verified` | `unsupported` — no adapter |
| Credits bundled with a purchase or renewal | `modeled` | `modeled` | `modeled` | `modeled` |
| Standalone credit-purchase price | `unsupported` — priced, never checkout-able | `unsupported` | `unsupported` | `unsupported` |

- **Vaulting, NMI** — Collect.js tokenization → customer vault. last-verified: weekly / env: sandbox / how: `TestLiveCollectJSTokenVaultCreate`, `TestLiveSandboxStoredCredentialCITThenMIT`. Note the lifecycle E2E vaults **directly at NMI**, so the OpenRails payment-method API surface is not itself live-exercised.
- **Payment methods, Stripe** — `unsupported` (guarded): there is no first-party CRUD, and since or#896 the refusal is honest. `RailPaymentMethodService` is NMI-only (`rails.SupportsPaymentMethodCRUD`), and a Stripe/CCBill/Solana request now fails with *"payment methods are not managed by OpenRails on this rail"* plus where the instrument actually lives, instead of the old **`PSP 'stripe' is not configured`** — which read as a misconfiguration. Mutation is delegated to Stripe's Billing Portal (`stripe_portal.go:23`), and `payment_method.attached` is recorded passively. Pinned by `TestCreatePaymentMethodUnsupportedRailIsHonest`.
- **Account Updater** — no rail consumes it. NMI receives `acu.summary.*` and logs them without touching the vault or the local card record (`internal/modules/webhooks/nmi.go:383`). Stripe's card updater is not subscribed to at all. The only working Account Updater in the repo belongs to the Basis Theory custodian, which is a different rail — do not read it as coverage here.
- **Charge saved method** — gated by `SupportsChargeSavedMethod` in the rail registry: NMI and Stripe only. last-verified: weekly / env: sandbox + Stripe test mode / how: `TestChargeOutstanding_NMISandbox_CollectsRealCharge`, `TestLiveNMIInvoiceCollectionAgainstSandbox`, `TestLiveStripeInvoiceCollectionAgainstTestAccount`. CCBill and Solana have no collection adapter, so arrears settlement and auto top-up do not exist on those rails.
- **Credits** — three different things, often conflated. (1) Credits declared on a product's `credits_spec` are granted rail-agnostically when a purchase or renewal registers, so they follow whatever checkout works on the rail. (2) Auto top-up charges a saved method and is therefore NMI/Stripe only. (3) A standalone **catalog credit-purchase price** can be declared and quoted, but nothing can BUY one: a top-up product's offers are rate-priced rows in `catalog_credit_purchase_prices`, never `prices` rows, so no checkout session can name one. or#896 deleted the unreachable `DepositCatalogCreditPurchase` wrapper (a second money-writer duplicating `MoneyService.Deposit`, which `POST /v1/service/credits/deposit` already drives live); the quote remains and a host delivers the quoted lot through that live deposit path. That is a gap on every rail, not a per-rail limitation.

## Catalog and reconciliation

| Flow | NMI | CCBill | Stripe | Solana |
|---|---|---|---|---|
| Product push | `unsupported` — no product concept | `unsupported` — manual | `modeled` | n/a |
| Price / plan push | `modeled` | `unsupported` — manual link only | `modeled` | `sandbox-verified` (devnet) |
| Catalog update propagation | `unsupported` — no-op | `unsupported` — no-op | `modeled` | `unsupported` — plan is immutable |
| Catalog drift detection (pull) | `modeled` | `unsupported` — structurally impossible | `modeled` | `modeled` |
| Inbound event ingestion | `sandbox-verified` | `modeled` | `modeled` | `modeled` — polling, not webhooks |
| Provider reconciliation pull | `modeled` | `modeled` — column mapping unverified | `modeled` | `modeled` |
| Settlement / payout ingestion | `unsupported` | `unsupported` | `unsupported` | n/a |

- **Price push, NMI** — recurring plans only; a non-recurring price returns `errPendingManualLink`. The lifecycle E2E creates its plan **out of band** via raw `recurring=add_plan`, so the adapter's own push path has never run against the sandbox.
- **Price push, Stripe** — find-or-create by content-addressed `lookup_key`. Two tests do reach Stripe test mode (`TestLiveStripeCatalogAutoCreateReusesContentKeys`, `TestLiveStripeExtrasListing`) but **no workflow runs either**, so they are still not standing evidence. Since or#896 they carry the `stripelive` build tag (`go test -tags=stripelive ./pkg/service/ -run TestLiveStripe`), so they are absent from the default build rather than silently skipped in it.
- **Price push, CCBill** — `AutoCreate` always returns `errPendingManualLink`; the operator creates the FlexForm in CCBill's admin and links `flex_id` + `form_name`. This surfaces as `pending_manual_actions` on the price rather than an error, which is the intended behavior.
- **Catalog update, NMI** — `Update` is a deliberate no-op: amount and frequency are immutable post-create and `is_active` is not representable.
- **Drift detection, CCBill** — impossible, not missing: FlexForms are write-only redirect URLs and DataLink exports members, not catalog objects. There is no catalog-list endpoint to diff against, so CCBill is excluded from the reconciliation job by design.
- **Webhooks, CCBill** — the rail has **no HMAC**; source IP is the only transport authentication. Inbound fixtures are hand-authored, not captured deliveries.
- **Event ingestion, Solana** — the rail emits nothing. Confirmation arrives by polling: a Solana Pay reference poller, slot-gated reads (`ReadUntilConsistent` / `*AtSlot`) so a read after a confirm cannot observe stale state, and a reconcile fetcher over locally-known PDAs. Note the reconcile fetcher's own reads deliberately skip the slot gate (`internal/reconcile/solana.go:158`).
- **Reconciliation pull, CCBill** — the DataLink transaction-export **column mapping has never been validated against a live account** (`internal/integrations/ccbill/datalink_export.go:23`); the typed accessors are best-effort and the raw fields are authoritative.
- **Settlement ingestion** — no rail has it. The `payment_settlement_events` feed is an OpenRails-internal pending/ack queue, not provider payout reconciliation. There is no Stripe balance-transaction or payout call site.

## Maintenance

Three rules, and they are the whole process:

1. A PR that changes a rail adapter's **wire behavior** — request fields, response
   parsing, endpoints, signing — must update the affected cells in the same PR.
2. If the change was not re-verified against the provider, the new status is
   `modeled`. Do not carry a stale `live-verified` across a wire change.
3. When a live or sandbox verification happens, record the date, the environment,
   and the command or test name. Never the credentials, account ids, or amounts.
