# Mobius/NMI Sandbox E2E Runbook (OpenRails)

This runbook exercises the **real** Mobius/NMI sandbox end-to-end. Prefer the
self-verifying harness first; use the manual steps below only when debugging a
specific portal/tunnel/browser problem.

```bash
task e2e-mobius-live
```

The harness starts the compose E2E stack, seeds the Mobius catalog, mints a JWT,
uses real Collect.js tokenization, saves a card in OpenRails, charges the saved
vault through one-off checkout, creates a saved-card subscription checkout,
verifies local DB state, queries NMI remotely, verifies signed webhook
ingestion/idempotent replay, and cancels the subscription.

Required environment:

- `MOBIUS_PRODUCTION_KEY`
- `MOBIUS_TOKENIZATION_KEY`
- `MOBIUS_WEBHOOK_SIGNING_SECRET`

Optional environment:

- `E2E_MOBIUS_PLAN_ID`; when omitted, the harness creates a per-run 1-day
  sandbox recurring plan through the NMI direct-post API.
- `E2E_MOBIUS_ONE_OFF_AMOUNT` defaults to a per-run value from 500 to 899 cents
  so repeated sandbox runs do not trip NMI duplicate-transaction checks.
- `E2E_MOBIUS_RECURRING_AMOUNT` defaults to the existing-plan amount `999`
  cents when `E2E_MOBIUS_PLAN_ID` is supplied; otherwise it defaults to a
  per-run amount from 1000 to 1399 cents for the auto-created sandbox plan.
- `OPENRAILS_HOST_PORT` defaults to `2053`; use another free port when a
  different local E2E stack already owns `2053`.
- `MOBIUS_E2E_BASE_URL` defaults to `http://localhost:$OPENRAILS_HOST_PORT`.
- `MOBIUS_E2E_TOKENIZATION_BASE_URL` defaults to `MOBIUS_E2E_BASE_URL`.
- `MOBIUS_E2E_START_COMPOSE` defaults to `true`.
- `MOBIUS_E2E_BUILD` defaults to `true`; set to `false` for faster reruns
  against an already-current compose image.
- `MOBIUS_E2E_START_TUNNEL` defaults to `false`; set it to `true` when your
  Collect.js tokenization key requires the Cloudflared HTTPS origin.
- `AUTHKIT_DEV_MINT_SECRET` defaults to the compose issuer's local dev secret.

The manual flow below covers the same surfaces:
- browser-side tokenization (Collect.js) → `payment_token`
- create vault/payment method in billing
- create a subscription purchase (NMI recurring)
- receive + verify webhooks over a deterministic Cloudflared hostname
- confirm remote state via NMI Query API
- cancel subscription and verify transitions

## 0) Prereqs

### Local tools
- `docker` + `docker compose`
- `cloudflared` (for deterministic webhooks)
- `curl`

### Mobius/NMI portal setup (one-time)
- Create a sandbox recurring plan (recommended: **1 day** cadence to test rebills quickly).
- Register billing webhook endpoint:
  - URL: `https://$CLOUDFLARED_PUBLIC_HOSTNAME/v1/webhooks/mobius`
  - Signing secret: **exactly** the merchant secret value you seed as
    `webhook_signing_secret` (the helper scripts read
    `MOBIUS_WEBHOOK_SIGNING_SECRET`)

## 1) Configure `.env`

Minimum set (fill in real values):

```bash
# sandbox mode
TEST_MODE=true

# AuthKit devserver mint secret (used by scripts/mint_jwt.sh); choose a local-only random value
AUTHKIT_DEV_MINT_SECRET=$(openssl rand -hex 32)

# Mobius/NMI values used by the bootstrap manifest and helper scripts.
MOBIUS_PRODUCTION_KEY=...
MOBIUS_TOKENIZATION_KEY=...
MOBIUS_WEBHOOK_SIGNING_SECRET=...               # HMAC shared secret for webhooks

# Cloudflared (deterministic webhook hostname)
CLOUDFLARED_TUNNEL_TOKEN=...
CLOUDFLARED_PUBLIC_HOSTNAME=openrails-webhooks.example.com

# Optional sandbox plan id for local seed. If omitted, task e2e-mobius-live
# creates a per-run 1-day plan automatically.
E2E_MOBIUS_PLAN_ID=YOUR_SANDBOX_PLAN_ID
```

Notes:
- Billing uses fixed NMI gateway endpoints for Mobius/NMI direct-post and query calls. Use sandbox/test credentials when `TEST_MODE=true`.
- OpenRails does not read PROCESSORS_* from `.env`. Seed merchant-scoped
  provider credentials through `openrails push-merchant-config`:

```yaml
version: 1
merchants:
  - slug: local-stack
    name: Local Stack
    provider_accounts:
      - provider_type: nmi
        provider_key: mobius
        account_id: mobius-profile-id
        role: primary
        secrets:
          production_key: {env: MOBIUS_PRODUCTION_KEY}
          tokenization_key: {env: MOBIUS_TOKENIZATION_KEY}
          webhook_signing_secret: {env: MOBIUS_WEBHOOK_SIGNING_SECRET}
```

- Collect.js origin restrictions: if your tokenization key is origin-locked, you must load the harness over **HTTPS** via Cloudflared (not `http://localhost`).

## 2) Start the local stack (+ local issuer)

```bash
task docker-up-e2e-sandbox
```

This starts:
- Postgres (OpenRails)
- Garnet/Redis
- ClickHouse (+ bootstrap)
- OpenRails migrations + OpenRails server
- AuthKit devserver issuer (`issuer:8080`, exposed on `http://localhost:8080`)

## 3) Start a deterministic webhook hostname

In a second terminal:

```bash
task tunnel-webhooks
```

Verify routing:

```bash
task verify-webhook-tunnel
```

Optional: send a signed test webhook through the public hostname:

```bash
task e2e-mobius-sandbox
```

## 4) Seed a minimal local catalog

```bash
task seed-e2e-mobius
```

This creates:
- `billing.products.slug = e2e_mobius`
- one active `billing.prices` row with `processors.mobius.plan_id = $E2E_MOBIUS_PLAN_ID`

## 5) Mint a JWT for API calls

```bash
task mint-jwt
```

This prints:
- `E2E_RUN_ID`
- `E2E_USER_ID`
- `E2E_JWT`

Keep those exported for the remaining steps.

## 6) Tokenize in the browser (Collect.js)

Open (over Cloudflared HTTPS):

```
https://$CLOUDFLARED_PUBLIC_HOSTNAME/debug/nmi/tokenization?mode=real&provider=mobius
```

1. Generate `payment_token`.
2. Paste `E2E_JWT` (and optionally `E2E_RUN_ID`) and click **Create payment method**.
3. Copy the created payment method ID from the response (`pm_...`).

## 7) Create a subscription checkout session

Use:
- `X-E2E-Run-ID: $E2E_RUN_ID`
- `X-Idempotency-Key: e2e_${E2E_RUN_ID}_checkout`

Example (replace `price_PRICE_UUID` + `pm_PAYMENT_METHOD_UUID`):

```bash
curl -fsS "https://$CLOUDFLARED_PUBLIC_HOSTNAME/v1/checkout" \
  -H "Authorization: Bearer $E2E_JWT" \
  -H "Content-Type: application/json" \
  -H "X-E2E-Run-ID: $E2E_RUN_ID" \
  -H "X-Idempotency-Key: e2e_${E2E_RUN_ID}_checkout" \
  --data '{
    "price_id": "price_PRICE_UUID",
    "mode": "subscription",
    "metadata": {"e2e_run_id":"'"$E2E_RUN_ID"'"},
    "payment": {
      "processor": "mobius",
      "payment_method_id": "pm_PAYMENT_METHOD_UUID"
    }
  }'
```

## 8) Verify (local + remote)

Local DB dump:

```bash
task e2e-dump-local
```

Remote query (by processor transaction/subscription IDs):

```bash
task nmi-query TXN_ID="TRANSACTION_ID_FROM_BILLING"
# or:
task nmi-query SUB_ID="SUBSCRIPTION_ID_FROM_NMI"
```

Webhook verification:
- billing logs should show signature verification succeeded and the expected lifecycle transitions.

## 9) Cancel + verify

```bash
curl -fsS "https://$CLOUDFLARED_PUBLIC_HOSTNAME/v1/me/subscriptions/subscription_SUB_UUID/cancel" \
  -H "Authorization: Bearer $E2E_JWT" \
  -H "Content-Type: application/json" \
  -H "X-E2E-Run-ID: $E2E_RUN_ID" \
  --data '{"feedback":"e2e cancel"}'
```

Then:
- confirm Mobius/NMI portal shows cancellation (and/or Query API)
- confirm billing receives the cancellation webhook and updates local state

## 10) Rebill testing

Sandboxes typically do **not** support “advance time”.
Recommended approach:
- Create a **1-day** plan in the sandbox, subscribe to it, then wait for rebill, or
- Use any portal/manual trigger functionality (if available).

## Optional cleanup

Local cleanup (wipe DB volumes):

```bash
docker compose -f docker-compose.yaml down -v
```

Remote cleanup:
- Prefer creating a fresh sandbox plan/user per run if the portal makes “delete” operations difficult.
- If the portal supports deleting test subscriptions/transactions, you can do so, but the harness is designed to be runnable without remote wipes.
