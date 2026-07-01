# Solana recurring — end-to-end validation runbook (#263)

How to validate the full recurring-Solana flow against the host app docker-compose
stack on devnet, plus what is already validated and what each step needs.

## What is already validated on devnet (no stack required)

Run with a funded devnet payer (see `.env.devnet`):

```
SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
  go test -tags devnet -run 'Lifecycle|PartialPull|CancelVsRevoke|SubmitAndConfirm' \
  -v -timeout 580s ./internal/integrations/solana/...
```

- **Full lifecycle** — create_plan → init_subscription_authority → subscribe →
  transfer_subscription (crank moves tokens) → cancel → resume.
- **Partial pulls + per-period cap** — a pull < plan amount is accepted;
  cumulative pulls are capped at the plan amount; over-cap is rejected with
  `Custom:400`. (Basis for Model-B proration, #267.)
- **Cancel vs revoke** — `cancel_subscription` does **not** stop pulls (soft
  cancel = stop cranking is the real stop); the subscriber revoking the SPL token
  delegate (`token.Revoke`) **does** stop pulls → next crank rejected with token
  OwnerMismatch `Custom:4` (the terminal signal, #265/#266/#270).
- **WatchTransaction / SubmitAndConfirm** — the crank now confirms a pull landed
  before treating it as success.

These cover the on-chain mechanics + the new confirm path. The service layer
(`PlanService`, `PrepareSubscribeService`, `EnrollService`, cranker) is unit-tested.

## Full-stack e2e (the remaining #263 work) — procedure + prerequisites

The host stack (`~/openrails-host/docker-compose.yaml`) pins the **published** image
`openrails/openrails:v0.10.2`, so testing local changes needs a locally-built
image overridden in.

### 1. Build the local image
```
cd ~/openrails && docker build -t openrails:local .
```
Prerequisite: a green `go build ./...`. **Blocker today:** concurrent in-flight
work (e.g. `internal/controlplane/catalog_provider_solana.go` referencing
not-yet-added symbols) breaks the full build; wait until the tree compiles.

### 2. Override the stack onto the local image + devnet
Create `~/openrails-host/docker-compose.override.yaml`:
```yaml
services:
  openrails:
    image: openrails:local
    environment:
      SOLANA_NETWORK: devnet
      SOLANA_RPC_URL: https://devnet.helius-rpc.com/?api-key=${HELIUS_API_KEY}
  openrails-migrate:        # if the stack has a migrate service, same image
    image: openrails:local
```
Then `docker compose up -d postgres openrails` (the `:5446` Postgres in the stack
is the billing DB; the integration notes flag it as flaky — recreate the volume
if connections reset).

### 3. Provision the merchant cranker key + a USDC recurring price
- Configure a Solana provider account with `signer.mode: local_keypair` plus scoped
  `secrets.private_key`, or `signer.mode: vault_transit`. The signing wallet pays
  gas + signs `transfer_subscription`.
- Obsolete: the old `POST /v1/admin/solana/recurring/plans` route was removed
  with the admin-surface hard cut. Do not use this runbook as the source of
  truth for publishing Solana recurring plans until the flow is redesigned.

### 4. Drive the subscribe → confirm → cancel flow
Browser (host apps) or API directly:
1. `POST /v1/me/checkout` `{price_id, mode:"subscription", payment:{rail:"solana", wallet}}`
   → `next_action: solana_sign_transactions [base64...]`.
2. Wallet signs + sends each tx (first-timer: init then subscribe).
3. `POST /v1/me/checkout/:id/confirm` `{payment:{rail:"solana", wallet, signature}}`
   → verifies the on-chain subscription, first crank, creates membership.
4. Cancel: `POST /v1/me/subscriptions/:id/cancel` (soft cancel → cranker stops).

### Prerequisites / blockers for the live run
- **Devnet USDC** — the service layer enforces the USDC/USD1 allowlist (unlike the
  raw-mechanics tests, which mint a self-controlled token). The subscriber wallet
  needs **devnet USDC** from a faucet (e.g. faucet.circle.com) — a self-mint will
  not resolve through `ResolveRecurringMint`.
- **A reachable billing Postgres** for `EnrollService` (CreateMembership) — the
  stack's `:5446` or a dedicated test DB (`OPENRAILS_TEST_DB_URL`).
- **A wallet that can sign** in the browser for the true UI e2e (or sign the
  returned base64 txns programmatically with a test keypair for an API-level e2e).

## Browser wallet-signing e2e (#275) — runbook

The API-level e2e above can sign the returned base64 txns with a test keypair.
The *true* browser e2e instead drives the **host React frontend** and has a
**real wallet extension approve the transactions** — the one thing the headless
service-layer tests can't prove (the wallet adapter -> `signAllTransactions` ->
`POST .../confirm` round trip through the UI). It is **manual** by nature: a
human (or a pre-unlocked extension) must click "Approve" in the wallet popup,
which cannot be fully scripted without a mock-wallet shim (see "Automation
limits" below).

### 0. One-time wallet prep (devnet)
- Install a browser wallet (Phantom/Backpack) and switch it to **Devnet**.
- Import the **subscriber** keypair (`SOLANA_DEVNET_SUBSCRIBER_KEY`, same one CI
  uses) so the wallet holds the funded devnet USDC + SOL gas. Fund its USDC ATA
  at https://faucet.circle.com (Solana devnet, USDC) and top up SOL via
  `solana airdrop 1 <addr> --url devnet` if needed.

### 1–3. Bring up the stack + plan
Identical to the full-stack steps above:
1. `cd ~/openrails && docker build -t openrails:local .` (needs a green `go build ./...`).
2. `~/openrails-host/docker-compose.override.yaml` pinning `image: openrails:local` +
   `SOLANA_NETWORK: devnet` / `SOLANA_RPC_URL: https://devnet.helius-rpc.com/?api-key=${HELIUS_API_KEY}`,
   then `docker compose up -d postgres openrails`.
3. Configure the merchant Solana provider-account signer. The old
   `POST /v1/admin/solana/recurring/plans` publishing step is obsolete until
   the Solana recurring plan flow is redesigned.
4. Bring up the host-app frontend pointed at the local OpenRails
   (`cd ~/openrails-host/frontend && pnpm dev`, default `http://localhost:13000`).

### 4. Walk the subscribe → sign → confirm → cancel flow in the host UI
1. Log into host-app as a test user; open the premium/subscribe entry point for
   the price tied to the published plan.
2. Pick **Solana / wallet** as the payment method and connect the devnet wallet
   (the subscriber keypair from step 0).
3. Click **Subscribe**. The frontend calls
   `POST /v1/me/checkout {mode:"subscription", payment:{rail:"solana"}}`
   and receives `next_action: solana_sign_transactions [base64...]` (first-timer:
   init-authority **then** subscribe).
4. The wallet adapter pops up one approval **per transaction** — **Approve each**.
   Watch the popup show the program + USDC token accounts.
5. The UI submits the signed txns and calls
   `POST /v1/me/checkout/:id/confirm {payment:{rail:"solana", wallet, signature}}`.
   This verifies the on-chain subscription, runs the first crank (pulls USDC),
   and creates the membership.
6. **Cancel:** open the subscription in the account UI and click **Cancel**
   (`POST /v1/me/subscriptions/:id/cancel` — soft cancel = cranker stops).

### What to assert (manual)
- **UI:** subscribe shows a wallet popup per tx; after confirm the UI flips to an
  active/subscribed state; the account page lists the subscription; cancel flips
  it to canceled/ending.
- **On-chain (devnet explorer / RPC):** the subscriber's USDC ATA balance drops
  by the plan amount on the first crank; the subscription + subscription-authority
  PDAs exist; the `transfer_subscription` signature returned by confirm is present
  and succeeded.
- **OpenRails side:** `solana_subscriptions` row created with the right PDAs; a
  membership/entitlement row created on confirm; after cancel the cranker no
  longer schedules a `next_pull_at` (soft cancel ≠ on-chain revoke — pulls only
  truly stop on SPL `Revoke`, per the on-chain runbook above).
- **Logs:** the OpenRails container logs the checkout → confirm → first-crank
  path without allowlist/mint-resolution errors (proves real USDC resolved).

### Automation limits + the skeleton
A fully-automated browser e2e is **best-effort, not the deliverable**: a real
wallet extension's approval popup runs in the extension's own context and can't
be reliably driven by Playwright without one of:
- a **mock-wallet adapter** injected into the frontend (a `window.solana`-style
  stub that auto-signs with the subscriber keypair and skips the popup), or
- a pre-unlocked extension + `@synthetixio/synpress`-style extension automation.

A Playwright **skeleton** that drives the host-app frontend up to the
wallet-approval boundary lives at
`~/openrails-host/frontend/e2e/premium/solana-subscribe.skeleton.spec.ts` (separate
repo — not committed by the OpenRails workflow). It loads the subscribe entry
point, selects Solana, and asserts the checkout call returns
`solana_sign_transactions`; it then **stops at the wallet popup** and is
`test.fixme()`-skipped by default. To make it run end-to-end, wire in a
mock-wallet adapter (auto-sign with the subscriber key) so the popup is bypassed
— at which point it becomes a genuine UI regression test for everything except
the human approval click itself.

## Status
On-chain mechanics, the confirm path, and the service-unit logic are validated.
The fast real-USDC service-layer test (`TestDevnetServiceLayerUSDC`) now runs on
a **daily schedule** in CI (`.github/workflows/solana-devnet-integration.yml`),
and the multi-hour real-rebill test (`TestDevnetMultiRebillHourly`) is a manual
`workflow_dispatch` job there. The full-stack/browser run is gated on: a green
tree (for the image build), devnet USDC, a stable billing DB, and a wallet that
can sign in-browser — none code blockers, all environment provisioning.
