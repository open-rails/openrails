# Solana Recurring Subscriptions — Implementation Plan

**Status:** draft / proposed
**Author:** Paul Fidika
**Date:** 2026-06-02

## 1. Goal

Make Solana a true peer to Stripe as a payment processor by adding **recurring
subscriptions** on top of the existing one-off purchase flow. Today Solana is
one-off only — `SolanaPayService.GeneratePayment` explicitly rejects tier
changes (`internal/modules/solana/pay.go:154`: *"solana does not support
subscription tier changes"*). The new on-chain
[Subscriptions Delegation Program](https://solana.com/docs/payments/subscriptions/overview)
(`De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44`) removes that limitation, so a
user can authorize recurring pulls once and we bill them every cycle — the same
shape as NMI/CCBill/Stripe.

## 2. Locked design decisions

These were decided up front and constrain everything below:

| Decision | Choice | Consequence |
|---|---|---|
| **Pricing** | **Stablecoin-fixed** | A Solana-subscribable price is denominated in a stablecoin so a fixed base-unit amount stays ~constant in USD for the whole plan life. On-chain plan `amount`/`period` are **immutable**, so no FX re-quote per cycle. Non-stablecoin recurring is out of scope. |
| **Recurring stablecoin allowlist** | **USDC confirmed; PYUSD pending verification — never USDT** | USDC is a plain SPL mint and is cleanly supported. **PYUSD is at risk** (see below). USDT is excluded for regulatory/counterparty-freeze risk. Allowlist is **enforced at plan-create**. |

> ✅ **CONFIRMED on devnet (2026-06-02): PYUSD is rejected by the Subscriptions program** — `create_plan(PYUSD)` fails with `custom program error 0x79` (121 = `mintHasPermanentDelegate`; it would also hit 122 `mintHasTransferFee`). Token-2022 itself IS supported — the program has explicit per-extension reject codes (118-124) and PYUSD carries two of them. The rejection is on the immutable mint, so no implementation change bypasses it. Recurring is **USDC-only**; PYUSD is permanently excluded (mint extensions are immutable). Original analysis below.
>
> ⚠️ **PYUSD may be incompatible with the Subscriptions program.** PYUSD's
> Token-2022 mint has **PermanentDelegate** and **TransferFee** (0%) extensions
> *initialized*, and the program rejects mints carrying either
> (`PermanentDelegate`, `TransferFee` are both on its reject list, alongside
> `ConfidentialTransfer` which PYUSD also plans to enable). This very likely
> means `create_plan`/`subscribe` reject the PYUSD mint outright. **Action:
> verify on devnet against the actual program before committing to PYUSD.** If
> rejected, recurring is **USDC-only** at launch; one-off PYUSD purchases remain
> unaffected (they don't touch the delegation program). Plan accordingly:
> treat USDC as the guaranteed path and PYUSD as a verify-then-add.
| **Billing engine** | **New OpenRails pull worker + merchant hot wallet** | OpenRails holds a funded Solana keypair, signs `transfer_subscription` each cycle from a scheduled River worker, and pays SOL gas. This is the big new capability — today the backend is read-only. |

### Stablecoin recurring-eligibility (verified by on-chain mint inspection)

Eligibility is a function of the mint's token-program + extension set (the program
rejects ConfidentialTransfer / NonTransferable / PermanentDelegate / TransferHook /
TransferFee / MintCloseAuthority / Pausable). Checked live on mainnet, with USDC
also confirmed via `create_plan` on devnet:

| Stablecoin | Mint program | Blocking extensions | Recurring |
|---|---|---|---|
| **USDC** | SPL Token | none | ✅ eligible (devnet `create_plan` accepted) |
| **USD1** | SPL Token | none | ✅ eligible (World Liberty Financial USD; **mainnet-only** mint) |
| **PYUSD** | Token-2022 | PermanentDelegate, TransferFee | ❌ (devnet error 121 `mintHasPermanentDelegate`) |
| **USDG** | Token-2022 | PermanentDelegate, TransferFee, ConfidentialTransfer, TransferHook, MintCloseAuthority | ❌ |

(USD1 owner program confirmed `TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA` =
classic SPL Token, not Token-2022.) BUIDL was evaluated and **dropped** — it's a
permissioned security (transferHook gates transfers to Securitize-whitelisted
holders), too complex, not used at all.

Mint extensions are immutable, so a rejected token can never become eligible.
`RecurringStablecoins = {USDC, USD1}`. New stablecoins are added by re-running the
mint-inspection check.

**Frontend default:** **USDC is the exclusive preferred stablecoin** presented in
the UI for Solana purchase options. The supported-tokens API marks it
(`config.PreferredStablecoin`, `TokenInfo.Preferred`) and flags each token's
`RecurringEligible`.

**One-off vs. recurring:** PYUSD and USDG are supported for **one-off** purchases
(in the token registry) but **never used for recurring** — they can't be rebilled,
so we don't prefer them or attach them to recurring prices.

**Stablecoin pricing ($1 peg + depeg failsafe):** stablecoins are quoted at a flat
**$1.00** — no live price check, no sub-penny noise. A rarely-used failsafe guards
against a real depeg: for stablecoins with a price feed (USDC/PYUSD), if the feed
shows divergence **> 1%** from $1, the live price is used so the charge compensates
(e.g. USDC at $0.95 → ~5% more tokens). Feedless stablecoins (USD1/USDG) have no
feed and always use the $1.00 peg. Tolerance: `stablecoinPegTolerance = 0.01`.

### One-off vs. recurring token asymmetry

These are deliberately different and must be enforced separately:

| | Accepted tokens | Pricing mechanism |
|---|---|---|
| **One-off purchase** (today) | **Any supported token** — SOL, USDC, PYUSD | FX-quoted to the fiat price **at purchase time** (`CalculateTokenQuote`). Volatility is fine because the quote is instantaneous. |
| **Recurring subscription** (new) | **USDC or PYUSD only** | Fixed stablecoin base-unit amount, locked at `create_plan`, immutable. SOL and any volatile/other token are **rejected** when publishing a recurring price. |

The reason for the asymmetry: a one-off quote is a single snapshot, so a floating token price is harmless. A recurring plan amount is frozen on-chain forever, so a floating token would silently drift the real charge ("$10" becomes $7 or $14). Only a stablecoin keeps a fixed base-unit amount ≈ a fixed USD amount across every cycle.

## 3. The architectural inversion (why this is non-trivial)

The existing one-off flow is **push + observe**: the *user's wallet pushes* a
single SPL transfer tagged with a random reference, and `SolanaPayPoller` polls
the chain (`GetSignaturesForAddress`) to detect it. The backend never signs or
spends SOL.

Recurring is **authorize-once + merchant-pulls**:

| Capability | Today (one-off) | Needed (recurring) |
|---|---|---|
| Signing | none (observe only) | merchant **hot wallet** signs `create_plan` + every `transfer_subscription` |
| Gas (SOL) | none | merchant pays SOL per pull; fee wallet must stay funded + monitored |
| Billing trigger | user pushes | **OpenRails drives** the pull on a schedule (we *are* the rebill engine, not a webhook receiver) |
| Client flow | Solana Pay URL | user wallet signs `initialize_subscription_authority` + `subscribe` via the TS SDK |
| Failure mode | tx never arrives → expire | pull fails (insufficient balance / revoked delegation) → dunning |

Three genuinely new subsystems: **(A)** a signing/fee wallet + tx builder,
**(B)** a client-side enroll flow, **(C)** a scheduled pull worker that feeds the
existing subscription lifecycle.

## 4. On-chain program model (recap)

- **Subscription Authority PDA** — `["subscription_authority", subscriber]`; set as the single `u64::MAX` delegate on the subscriber's USDC token account. Can only move funds when a delegation authorizes it.
- **Plan PDA** — `["plan", merchant, plan_id]`; merchant-published terms: mint, `amount` (USDC base units), `period_hours`, optional `destinations[4]` + `pullers[4]` whitelists, `metadata_uri`, `end_ts`. Immutable core terms; `created_at` is a unique fingerprint.
- **Subscription PDA** — `["subscription", plan_pda, subscriber]`; snapshots plan terms, tracks `current_period_start_ts` + `amount_pulled_in_period` + `expires_at_ts`.
- **Key instructions:** `create_plan` (merchant), `initialize_subscription_authority` + `subscribe` (user), `transfer_subscription` (merchant/puller, each cycle), `cancel_subscription` / `resume_subscription` (user), `revoke_delegation` (cleanup).
- **Ghost-plan safety:** if a plan is deleted and recreated at the same PDA, `created_at` differs and pulls fail `PlanTermsMismatch` — we must surface this as a forced re-enroll.

## 5. Architecture / components

```
                       ┌─────────────────────────────────────────────┐
                       │  config: solana processor                   │
                       │   + merchant signing keypair (hot wallet)   │
                       │   + fee wallet balance monitor              │
                       └─────────────────────────────────────────────┘
 create_plan (once per price)          │
   ┌───────────────────────────────────┘
   ▼
 internal/integrations/solana/
   plan.go        — build/sign create_plan, update_plan, delete_plan
   subscribe.go   — build (unsigned) initialize_authority + subscribe ixns for client
   transfer_sub.go— build/sign transfer_subscription (the pull)
   signer.go      — Signer interface, resolved PER TENANT (hot keypair now; KMS later)

 internal/modules/solana/
   tenant_credentials.go  — resolve a tenant's Solana keypair via the EXISTING tenancy.TenantSecretStore (DB/Vault); never a bespoke store
   plan_service.go        — map Price ⇄ on-chain Plan PDA (per-tenant; derived from the tenant's merchant address)
   subscription_service.go— enroll/confirm a subscriber; persist row in billing.solana_subscriptions
   pull_service.go        — execute one cycle's pull (using the tenant's signer), then call lifecycle.RenewMembership

 internal/river/
   jobs_solana_rebill.go  — scheduled worker: find due Solana subs, group by tenant, enqueue per-sub pull jobs
   (models jobs_dunning.go; reuses SubscriptionLifecycleService)
```

### Multi-tenant model (decided)

OpenRails is tenant-aware at the data layer today (migration `039_tenant_aware_core`:
`billing.tenants` control-plane table, `tenant_id` on tenant-owned tables,
`pkg/tenant` context, `middleware.ResolveTenant`), but **processor credentials are
still a single global config** (`cfg.GetSolanaProcessor()` → one `RecipientWallet`,
one `HeliusAPIKey`). Per-tenant billing requires generalizing that:

- **Each tenant brings its own provider connection.** Stripe = its own API key +
  account; Solana = **its own keypair + on-chain merchant address**. The global
  config-file processor becomes the **`default` tenant's** credentials, so
  single-tenant self-hosted installs keep working unchanged.
- **Credentials use the EXISTING `tenancy.TenantSecretStore`** (issues #225/#227),
  not a new store. The Solana keypair is the secret `solana/private_key`, resolved
  per request via `tenant.FromContext(ctx)`. Backend is DB+envelope (self-hosted)
  or Vault (managed) — same addressing either way. See §8 for the Vault design.
- **Plans are inherently per-tenant.** A Plan PDA is `["plan", tenant_merchant,
  plan_id]` — derived from *that tenant's* merchant address — so two tenants
  selling "$10/mo" get distinct on-chain plans automatically. The `Price` row is
  already `tenant_id`-scoped, so its stored plan handle is too.
- **The pull worker is tenant-fanned:** it groups due subscriptions by `tenant_id`,
  loads each tenant's signer once, and pulls that tenant's subs with it. A failure
  to load one tenant's credentials must not block other tenants.

### Reuse, don't rebuild
- **Subscription lifecycle:** `SubscriptionLifecycleService.CreateMembership` on first successful pull, `RenewMembership` on each subsequent pull, `FailMembership` (dunning) on failed pull, `CancelMembership` on cancel. The NMI dunning worker already calls these — Solana is just another `Processor` (`models.ProcessorSolana` already exists).
- **Payments + entitlements + credits:** `RegisterPurchase` / lifecycle already record `models.Payment`, grant entitlement windows, and snapshot credits. Each Solana pull's tx signature is the `TransactionID` (idempotency key, same as today).
- **Price catalog:** store the on-chain plan handle in `Price.Processors["solana"]` (e.g. `plan_pda`, `plan_id`, `mint`, `created_at`) via the existing `SetProcessorConfig` helper, exactly like `SetStripeConfig` stores a Stripe price ID.

## 6. Data model changes

- **`Price.Processors["solana"]`** gains keys: `plan_pda`, `plan_id`, `mint`, `mint_symbol` (`USDC`|`PYUSD`), `amount_base_units`, `period_hours`, `created_at`. Created when an admin "publishes" a recurring Solana price (calls `create_plan` on-chain). Recurring Solana prices must have `BillingCycleDays` consistent with on-chain `period_hours`.
- **Recurring stablecoin allowlist** — a small constant set, **`{USDC}` at launch**, with `PYUSD` gated behind devnet verification (see §2 warning; its mint extensions likely disqualify it). Resolved to mainnet/devnet mints via `config.TokensForNetwork`. `create_plan` / publish-recurring-price **rejects any mint not in this set** (notably USDT and SOL). One-off purchase paths keep using the full `DefaultSupportedTokens()` set and are unaffected.
- **`subscriptions` table:** reuse `Processor=solana`, `ProcessorSubscriptionID` = the **Subscription PDA** address (natural unique key for `GetByProcessorSubscriptionID`, which lifecycle renewal already keys on). No new columns on this table.
- **New table `billing.solana_subscriptions`** (decided — a dedicated table, **not** subscription metadata; on-chain state is load-bearing and the due-worker queries it, so it must be first-class and indexable). Tenant-scoped (`tenant_id`), with FK to `subscriptions.id`. Columns: `subscriber_wallet`, `authority_pda`, `subscription_pda` (unique), `plan_pda`, `mint`, `last_pulled_period_start`, `last_signature`, `plan_created_at_fingerprint` (detects ghost-plan recreation), `next_pull_at`, timestamps. Indexes on `(tenant_id, next_pull_at)` for the due-query and `subscription_pda` for idempotent upserts.
- **Tenant credentials reuse the EXISTING `TenantSecretStore`** (`internal/tenancy/secrets.go`, issues #225/#227) — **do NOT build a bespoke `tenant_solana_credentials` table.** That abstraction already provides `(tenant_id, name)`-addressed, per-tenant-isolated, envelope-encrypted secrets with a DB backend (self-hosted) and a Vault backend (managed) behind one interface. Add canonical Solana secret names alongside the existing `stripe/*` ones:
  - `solana/private_key` — the tenant's signing keypair (the sensitive bit; ideally never extracted — see §8 Transit).
  - `solana/merchant_address`, `solana/fee_wallet_address` — non-secret but stored together for cohesion (or keep addresses in a small non-secret tenant-config row; they're public on-chain).
  - `solana/rpc_endpoint`, `solana/helius_api_key` — per-tenant RPC config.
  The global config `GetSolanaProcessor()` seeds the **`default` tenant's** secrets so existing single-tenant installs are unchanged. **Only the non-secret on-chain merchant address needs to be queryable** for plan-PDA derivation — keep it in a tiny non-secret `billing.tenant_solana_config` row (or on `billing.solana_subscriptions`), never the private key.
- **Pending-enroll record:** mirror the existing Redis pending-payment pattern for the `subscribe` confirmation (detect the user's on-chain `subscribe` tx before activating).

## 7. Flows

### 7.1 Publish a recurring Solana price (admin, one-time per price)
1. Admin marks a stablecoin price as Solana-recurring; backend **validates the mint is in the allowlist** (rejects USDT/SOL/others) and loads the **tenant's** Solana credentials.
2. Backend signs (with the **tenant's** keypair) + submits `create_plan(plan_id, mint, amount, period_hours, pullers=[tenant_merchant], end_ts=0)`. The Plan PDA is `["plan", tenant_merchant, plan_id]`, so it's tenant-unique by construction.
3. Persist `plan_pda` + `mint_symbol` + `created_at` into `Price.Processors["solana"]` (the price row is already tenant-scoped).

### 7.2 Enroll (user, checkout)
1. Checkout session for a Solana-recurring price → backend returns the instructions (or SDK params) for the user to sign: `initialize_subscription_authority` (if absent) + `subscribe(plan_pda)`.
2. User signs in-wallet (TS SDK fetches live plan terms during `subscribe`).
3. Backend detects the `subscribe` tx (poller, same machinery as one-off), creates the local subscription as `pending`, then performs the **first pull immediately** → `CreateMembership` (grants access + records first payment). Mirrors how a first charge activates an NMI/CCBill sub.

### 7.3 Recurring pull (OpenRails, each cycle)

> **Cadence (decided):** run the worker **hourly** (cron; configurable 15–60 min) and let the **due-query filter** to only subscriptions whose `next_pull_at <= now`. Worker frequency is decoupled from billing frequency (monthly) — exactly like `jobs_dunning.go`. `next_pull_at` aligns to the **on-chain period boundary** (the program's `amount_pulled_in_period` guard rejects/no-ops an early pull, so we never pull before the period rolls over). A run is **N individual `transfer_subscription` pulls** (one per due subscriber, optionally a few instructions batched per tx) — *not* a single fan-in sweep.

1. River cron (`jobs_solana_rebill.go`) runs hourly; queries `billing.solana_subscriptions` due rows (`next_pull_at <= now`), **grouped by `tenant_id`**.
2. Per tenant, load the tenant's signer once; per sub, enqueue a pull job: build + sign + submit `transfer_subscription(plan_pda, subscription_pda)` from **that tenant's** hot wallet. A credential-load failure for one tenant is logged and skipped without blocking others.
3. On confirmed tx → `RenewMembership(processor=solana, processor_subscription_id=subscription_pda, transaction_id=signature, amount, currency=usd-equiv)` extends the period, pushes the next entitlement window, records the payment. Idempotent on signature. Advance `last_pulled_period_start` / `next_pull_at` in `billing.solana_subscriptions`.
4. On failure → classify (see 7.5).

### 7.4 Cancel (user or admin)
- **User-initiated:** user signs `cancel_subscription` on-chain (sets `expires_at_ts`), backend stops scheduling pulls and calls `CancelMembership` (period-end). `resume_subscription` before expiry maps to reactivation.
- **Merchant/admin-initiated:** stop pulling + `CancelMembership`. Optionally call `update_plan` to sunset the plan for new subscribers. We cannot force-cancel a user's on-chain authorization, but not pulling is sufficient.

### 7.5 Failure / dunning
- **Insufficient USDC balance** → `transfer_subscription` fails → `FailMembership` → **the same dunning state machine as NMI** (`lifecycle_service.go`: `past_due`, retried on the cadence-relative `DunningRetryOffsets` schedule (#359) — for a monthly price: retries at D+2/5/9/13, terminal on the 5th failure, then cancel). The pull worker is the Solana analog of `DunningWorker`. **No new dunning logic — Solana is just another processor on the existing path.**
  - **Never partial-pull (decided):** the worker always requests the **full** plan amount; it never pulls `min(balance, cap)`. Combined with Solana tx atomicity, an underfunded pull **reverts entirely — zero USDC moves** (we pay only the failed-tx SOL fee). "Take nothing, not $5" is therefore a property of the chain, not a policy to enforce.
  - **On-chain period cap (Solana-only nuance):** the program enforces `amount_pulled_in_period ≤ plan_amount` and **resets the allowance each period**. So (a) dunning retries must land **within the on-chain period window** — fine, since the monthly schedule's last retry is D+13 (window 14d), inside a 30d monthly period, and we cancel before the period rolls; and (b) a **fully-missed period cannot be clawed back later** (can't exceed the cap next period — unlike NMI arrears). #257 must keep `NextRetryAt` inside the period and treat exhausted dunning as cancel (already the existing behavior).
  - **Dunning grace (decided): Solana gets the SAME paid-through grace as NMI.** The grace-entitlement-append at `lifecycle_service.go:1375` is currently gated on `processors.IsNMIBackedProcessor`. Generalize that gate to a predicate like `processorDrivesDunning(processor)` (true for NMI-backed **and** Solana — both are processors where *OpenRails* controls retry timing, unlike Stripe where the processor does). So during a Solana subscriber's retry window the user keeps access via grace entitlement windows, exactly like NMI; access is revoked only on dunning exhaustion → cancel.
- **Delegation revoked** (user revoked the SA approval) → terminal; `CancelMembership`.
- **`PlanTermsMismatch` (ghost plan)** → terminal for that subscription; notify user to re-enroll.
- **Period already pulled** → on-chain `amount_pulled_in_period` guard makes double-pull a no-op; treat as idempotent success.

## 8. Signing / secret infrastructure (build on what exists)

> **Concrete sketch:** [`solana-vault-signing.md`](./solana-vault-signing.md) —
> the `Signer` interface + tx-builder, both impls (`keypairSigner` everywhere,
> `transitSigner` for prod), the live `VaultKV` KV-v2 adapter, Vault auth/renewal,
> the Transit adapter, wiring, and the Ed25519/Solana gotchas. This section is the
> summary; that doc is the buildable detail.
>
> **Decision:** Vault **Transit** (non-extractable Ed25519 key, sign-as-a-service)
> is the recommended production custody for the money-moving Solana key — OpenRails
> requests signatures and never holds the key. The KV-fetch impl remains the simple
> path that works everywhere (incl. self-hosted DB+envelope). Both behind one
> interface, selected per deployment.

**OpenRails already has the tenant-secret abstraction this needs** — issues
#225/#227 shipped `tenancy.TenantSecretStore` (`(tenant_id, name)`-addressed,
per-tenant isolated), three backends (`memSecretStore`, `dbSecretStore` →
`billing.tenant_secrets`, `vaultSecretStore` → tenant-scoped Vault KV path), an
`encryptedSecretStore` envelope-encryption decorator (`internal/crypto`,
master-key-wraps-per-tenant-DEK), and the `server.go` wiring that selects them.
Stripe per-tenant keys already flow through it (`stripe/secret_key`). **Solana
plugs into the same pipe; we do not invent a parallel one.**

- **`Signer` interface** in `integrations/solana/signer.go` — `Sign(ctx, tx)`, **resolved per tenant** via `tenant.FromContext(ctx)`. It loads `solana/private_key` from the injected `TenantSecretStore` (whatever backend is wired). **No process-global signer** — every signing call is tenant-scoped, mirroring how Stripe credentials resolve.
- **Two `Signer` impls (the §11-Q4 decision), both behind one interface:**
  1. **KV-fetch-then-sign-locally** — `store.Get(tenant, "solana/private_key")` → decrypt → sign in-process. Works with *any* backend (DB+envelope self-hosted, or Vault KV managed). Simplest; the key briefly lives in container memory.
  2. **Vault Transit remote-sign (recommended for production)** — the private key is a non-extractable Ed25519 key inside Vault's **Transit** engine; OpenRails sends the tx message to `transit/sign/<tenant-key>` and gets back a signature. **The key never leaves Vault / never enters the container.** This is the right custody level for a key that moves money, and it's strictly stronger than KV-fetch. Add it as a third `TenantSecretStore`-sibling or a dedicated `RemoteSigner` — it's a "sign this," not a "give me the secret," operation, so it's a separate method, not `Get`.
- **How Vault fetch works in a single-container multi-tenant prod (the question):**
  - **App-level Vault auth, not per-tenant.** The OpenRails container authenticates to Vault *once as itself* (AppRole `role_id`/`secret_id`, or Kubernetes auth via its service-account JWT), receives a Vault token, and **renews it on a schedule**. Tenant isolation is enforced in code by the `(tenant_id, name)` addressing — the app is the trusted broker. Per-tenant Vault *policies* only matter if tenant operators get direct Vault access (BYO-key self-service), which can come later.
  - **Addressing already defined:** `vaultSecretStore` maps `(tenant, name)` → `secret/openrails/tenants/<tenant-id>/<name>` (KV-v2). Per-tenant path isolation is built in.
  - **Wire a live `VaultKV`:** the only missing piece is a real adapter implementing the existing `VaultKV` interface (`ReadSecret/WriteSecret/DeleteSecret/ListSecrets`) over `hashicorp/vault/api`, plus config to select `vaultSecretStore` and the auth method. Everything above it is unchanged.
  - **Cache on the hot path.** Fetching from Vault per request/per-pull adds latency and makes Vault a hard hot-path dependency. Cache resolved secrets in-process with a **60s TTL (decided; configurable)** keyed by `(tenant, name)`, invalidated early on a `Secret.Version` bump (rotation). 60s keeps revocation lag small while the per-run dedupe already removes most round-trips. The pull worker loads each tenant's signer **once per run**, not per subscription. (Transit signing still round-trips Vault per signature — acceptable, but batch per tenant.)
  - **Fail closed, distinguish outage from missing.** `vaultSecretStore` already fails closed (`ErrVaultNotConfigured` / propagated errors). For the pull worker, *Vault unreachable* = operational → **retry, do not cancel**; *secret genuinely absent / tenant deprovisioned* = terminal for that tenant. Never treat a fetch failure as "no charge needed."
- **Key custody & rotation:** prefer Transit so leakage-driven rotation is rarely needed. When rotation *is* needed, note it's expensive for Solana: a new keypair = new merchant address = **re-publish that tenant's plans + re-enroll subscribers** (plan PDA derives from the merchant address). Runbook this.
- **Two-wallet model (decided):** separate the roles per tenant.
  - **Cranking / puller wallet (hot):** the per-tenant key in Vault Transit. It is the plan owner (or a whitelisted `puller`), **signs every `transfer_subscription`, and pays SOL gas** — but never holds funds.
  - **Receiving wallet (cold/treasury):** **public key only** in OpenRails; set as the plan's whitelisted `destination`. **Receives the USDC**, never signs.
  - **Containment:** with `destinations = [cold_receiving_wallet]`, the program rejects any pull to another address (`UnauthorizedDestination`). So a fully compromised hot cranking key **cannot redirect subscriber funds to an attacker** — it can only trigger already-authorized pulls into your cold treasury, capped at each subscriber's per-period amount. This is the main reason to split the wallets.
- **Fee (SOL) management:** monitor **each tenant's** cranking-wallet SOL balance and **alert** when low (NO auto-top-up, NO gasless relayer / fee-payer delegation — too complex). Each pull costs ~5,000 lamports base + priority fee; N due subs = N txns/cycle. A pull that fails for lack of SOL is *operational* (retry), not subscriber dunning — distinguish from insufficient *USDC*. Per-tenant fee wallets isolate one tenant running dry.
- **Rate / RPC:** reuse `RPCClient` + per-tenant Helius config; batch/throttle pulls (the dunning worker's lease + backoff patterns apply).

## 9. Risks & edge cases

- **FX drift is excluded by decision** (USDC-fixed), but reporting still records a fiat-equivalent `amount` — decide whether that's pinned at enroll or marked-to-market for analytics only.
- **Immutable plan terms:** a price change requires sunset + new plan + re-enroll. Need admin UX + user re-enroll notification. Document that "editing" a Solana recurring price is really "replace."
- **Partial/late pulls:** period boundaries are on-chain; our `CurrentPeriodEndsAt` must track on-chain `current_period_start_ts + period` to avoid drift between DB and chain. Source of truth for "paid through" should be the confirmed pull, not wall-clock.
- **Hot wallet compromise** = ability to pull that tenant's subscribers up to their per-period caps. Per-tenant key isolation bounds the blast radius to one tenant; per-period caps bound it further; KMS path mitigates more. A single global key (rejected) would have exposed *all* tenants — another reason for per-tenant signing.
- **Tenant credential lifecycle** — provisioning, encryption-key rotation, and revoking/rotating a leaked tenant keypair (which forces re-publishing that tenant's plans under a new merchant address, hence re-enroll). Must be an explicit operational runbook, not an afterthought.
- **Reconciliation:** extend the existing reconcile workers to cross-check on-chain `transfer_subscription` events against recorded payments (the program emits events for indexers).
- **Token-2022 extension rejections** (TransferHook/Fee/PermanentDelegate/etc.) — validate the mint at plan-create time. USDC is plain SPL Token (safe). **PYUSD is Token-2022 with PermanentDelegate + TransferFee initialized, both on the program's reject list** — almost certainly disqualifying. Confirm on devnet; if rejected, ship recurring as USDC-only and revisit PYUSD only if PayPal changes the mint (which they cannot — extensions can't be removed after mint creation) or if the program relaxes its checks. Practically: **PYUSD recurring is unlikely to ever be possible** given mint extensions are immutable.

## 10. Phased delivery (suggested issues — next_id 251 in agents/progress.json)

0. **PYUSD compatibility spike** — on devnet, attempt `create_plan` + `subscribe` against the PYUSD mint and confirm whether the program rejects it (PermanentDelegate/TransferFee). Outcome decides whether the launch allowlist is `{USDC}` or `{USDC, PYUSD}`. *(cheap, do first)*
1. **Tenant Solana signer over the existing secret store** — add `solana/*` secret names; `Signer` (KV-fetch impl) resolving `solana/private_key` from `tenancy.TenantSecretStore`; seed the `default` tenant from existing global config; in-process cache with TTL. **No new credentials table** — reuse #225/#227. *(foundation; everything else depends on it)*
2. **Signer + tx-builder foundation** — build/sign helpers, devnet smoke test using a tenant signer. *(no user-facing change)*
   - *Managed-prod track (parallel, optional):* implement a live `VaultKV` adapter (`hashicorp/vault/api`) + Vault auth (AppRole/K8s) + select `vaultSecretStore`; and/or a Vault **Transit** `RemoteSigner` so the key never leaves Vault.
3. **Plan publishing** — `create_plan`/`update_plan`/`delete_plan` signed by the tenant key; admin path to mark a USDC price Solana-recurring; persist plan handle in `Price.Processors["solana"]`.
4. **Enroll flow** — `billing.solana_subscriptions` migration; checkout returns subscribe instructions; poller confirms `subscribe`; first pull → `CreateMembership`.
5. **Recurring pull worker** — `jobs_solana_rebill.go`; tenant-grouped due-query; per-sub pull with the tenant signer; `RenewMembership`; idempotency on signature.
6. **Cancel / resume / dunning** — `cancel_subscription`/`resume_subscription` wiring; `FailMembership` classification; ghost-plan + revoked-delegation handling.
7. **Per-tenant fee-wallet monitoring + reconciliation** — per-tenant SOL balance alerts; on-chain event ↔ payment reconcile; operational dashboards + credential-rotation runbook.

## 11. Open questions

**Resolved**
- ✅ **Multi-tenant signing** — **per-tenant** keypair + on-chain merchant address, loaded from a new `billing.tenant_solana_credentials` store (the global config seeds the `default` tenant). Generalizes the Stripe "per-tenant API key + account" model. *(§5, §6, §8)*
- ✅ **On-chain subscription state** — **dedicated table** `billing.solana_subscriptions`, not subscription metadata. *(§6)*

**Still open**
1. **Fiat-equivalent reporting** — pin USD value at enroll, or mark-to-market for analytics only?
2. **First charge timing** — pull immediately on enroll (recommended, matches card flows) vs. at first period boundary?
3. **Whitelist destinations** on plans, or rely on `pullers` only?
4. ✅ **RESOLVED — secret storage** — reuse the existing `tenancy.TenantSecretStore` (#225/#227): DB+envelope self-hosted, Vault KV managed; key `solana/private_key`. *Remaining sub-choice:* KV-fetch-then-sign vs. **Vault Transit** remote-sign (recommended for the money-moving key). See §8.
5. **Per-tenant fee-wallet funding model** — tenant funds their own SOL, or platform fronts gas and bills it back?
```

## 12. Testing strategy — real devnet integration tests (decided)

On-chain behavior is verified with **live devnet integration tests, not mocks**.
Each test generates a keypair, funds it, and submits **real transactions** against
the deployed program on devnet — so we test what actually works, including the
DEVNET-VERIFY items (PDA derivation, account orderings, `__event_authority` seed,
Token-2022 mint rejection) that no unit test can confirm.

- **Build-tagged + opt-in:** files carry `//go:build devnet` and run only via
  `go test -tags devnet -run Devnet ./internal/integrations/solana/...`. They are
  slow and need network, so they never run in the default unit suite.
- **Funded payer:** set `SOLANA_DEVNET_PAYER_KEY` to a base58 devnet key funded
  once at https://faucet.solana.com (the public RPC `requestAirdrop` is IP-rate-
  limited, so a persistent funded payer is the reliable path). Tests fall back to
  RPC airdrop and **skip** (not fail) when the faucet is dry.
- **Per-flow coverage (built as each flow lands):** `create_plan` (USDC accepted /
  PYUSD rejected → #252), `init_subscription_authority` + `subscribe`,
  `transfer_subscription` (the pull, incl. insufficient-USDC revert), and
  `cancel`/`resume`. Each asserts the real on-chain outcome.
- **Fast structural unit tests remain** for pure logic (PDA determinism, arg byte
  layout, allowlist, signer crypto) — they are not mocks of on-chain behavior, just
  cheap guards; the devnet tests are the source of truth for program interaction.

The first integration test (`create_plan`) is committed; it ran live against
devnet and is currently faucet-blocked pending a funded payer key.
