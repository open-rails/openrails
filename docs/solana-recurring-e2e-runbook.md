# Solana recurring — end-to-end validation runbook (#263)

How to validate the full recurring-Solana flow against the doujins docker-compose
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

The doujins stack (`~/doujins/docker-compose.yaml`) pins the **published** image
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
Create `~/doujins/docker-compose.override.yaml`:
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

### 3. Provision the tenant cranker key + a USDC recurring price
- Seed the tenant secret `solana/private_key` (the cranker wallet) — DB-backed
  store (self-hosted) or Vault. The cranker pays gas + signs `transfer_subscription`.
- Publish a plan: `POST /v1/admin/solana/recurring/plans`
  `{plan_id, token_symbol:"USDC", amount_base_units, period_hours, price_id}` →
  attaches the plan handle to the price's Solana config.

### 4. Drive the subscribe → confirm → cancel flow
Browser (doujins/hentai0) or API directly:
1. `POST /v1/self/checkout` `{price_id, mode:"subscription", payment:{processor:"solana", wallet}}`
   → `next_action: solana_sign_transactions [base64...]`.
2. Wallet signs + sends each tx (first-timer: init then subscribe).
3. `POST /v1/self/checkout/:id/confirm` `{payment:{processor:"solana", wallet, signature}}`
   → verifies the on-chain subscription, first crank, creates membership.
4. Cancel: `POST /v1/self/subscriptions/:id/cancel` (soft cancel → cranker stops).

### Prerequisites / blockers for the live run
- **Devnet USDC** — the service layer enforces the USDC/USD1 allowlist (unlike the
  raw-mechanics tests, which mint a self-controlled token). The subscriber wallet
  needs **devnet USDC** from a faucet (e.g. faucet.circle.com) — a self-mint will
  not resolve through `ResolveRecurringMint`.
- **A reachable billing Postgres** for `EnrollService` (CreateMembership) — the
  stack's `:5446` or a dedicated test DB (`OPENRAILS_TEST_DB_URL`).
- **A wallet that can sign** in the browser for the true UI e2e (or sign the
  returned base64 txns programmatically with a test keypair for an API-level e2e).

## Status
On-chain mechanics, the confirm path, and the service-unit logic are validated.
The full-stack run is gated on: a green tree (for the image build), devnet USDC,
and a stable billing DB — none code blockers, all environment provisioning.
