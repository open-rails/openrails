# NMI Sandbox E2E Runbook (OpenRails)

This runbook exercises the **real** NMI sandbox end-to-end through the
configured `mobius` provider slug. Prefer the self-verifying harness first; use
the manual steps below only when debugging a specific portal/tunnel/browser
problem.

```bash
task e2e-nmi-live
```

The harness starts the compose E2E stack, seeds the NMI catalog, mints a JWT,
loads Collect.js in a browser, enters an NMI sandbox test card, receives a real
opaque `payment_token`, saves a card in OpenRails, charges the saved vault
through one-off checkout, creates a saved-card subscription checkout, verifies
local DB state, queries NMI remotely, verifies signed webhook
ingestion/idempotent replay, and cancels the subscription. It provisions the
local `local-stack` merchant and mints a short-lived delegated JWT with a
throwaway test key for the run.

Required environment:

- `NMI_SANDBOX_SECURITY_KEY`
- `NMI_TOKENIZATION_KEY` for the same sandbox NMI account
- `NMI_WEBHOOK_SIGNING_SECRET`

Optional environment:

- `E2E_NMI_PLAN_ID` defaults to `openrails_e2e_nmi_daily_999`. The harness
  attempts to create that 1-day sandbox recurring plan every run and uses it
  when NMI reports that it already exists.
- `E2E_NMI_ONE_OFF_AMOUNT` defaults to a per-run value from 500 to 899 cents
  so repeated sandbox runs do not trip NMI duplicate-transaction checks.
- `E2E_NMI_RECURRING_AMOUNT` defaults to a per-run value from 1000 to 1399
  cents for the same reason; the plan id stays stable.
- `OPENRAILS_HOST_PORT` defaults to `3053`; use another free port when a
  different local E2E stack already owns `3053`.
- `NMI_E2E_BASE_URL` defaults to `http://localhost:$OPENRAILS_HOST_PORT`.
- `NMI_E2E_START_COMPOSE` defaults to `true`.
- `NMI_E2E_BUILD` defaults to `true`; set to `false` for faster reruns
  against an already-current compose image.
- `NMI_E2E_START_TUNNEL` defaults to `false`; set it to `true` when NMI
  webhooks need the Cloudflared HTTPS origin during the run.
- `NMI_COLLECT_JS_URL` defaults to `https://secure.networkmerchants.com/token/Collect.js`.
- `NMI_E2E_CARD_NUMBER`, `NMI_E2E_CARD_EXPIRY`, and `NMI_E2E_CARD_CVV` default
  to sandbox test-card values.
- `E2E_NMI_MERCHANT_SLUG` defaults to `local-stack`.

The manual flow below covers the same surfaces:
- browser-side tokenization with NMI Collect.js → `payment_token`
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

### NMI sandbox account setup (one-time)
- Register billing webhook endpoint:
  - URL: `https://$CLOUDFLARED_PUBLIC_HOSTNAME/v1/merchants/local-stack/webhooks/mobius`
  - Signing secret: **exactly** the merchant secret value you seed as
    `webhook_signing_secret` (the helper scripts read
    `NMI_WEBHOOK_SIGNING_SECRET`)

## 1) Configure `.env`

Minimum set (fill in real values):

```bash
# sandbox mode
TEST_ENV=true

# NMI sandbox values used by the bootstrap manifest and helper scripts.
NMI_SANDBOX_SECURITY_KEY=...
NMI_TOKENIZATION_KEY=...                     # public Collect.js tokenization key for the sandbox account
NMI_WEBHOOK_SIGNING_SECRET=...               # HMAC shared secret for webhooks

# Cloudflared (deterministic webhook hostname)
CLOUDFLARED_TUNNEL_TOKEN=...
CLOUDFLARED_PUBLIC_HOSTNAME=openrails-webhooks.example.com

# Optional sandbox plan id for local seed. If omitted, task e2e-nmi-live
# creates or reuses openrails_e2e_nmi_daily_999.
E2E_NMI_PLAN_ID=openrails_e2e_nmi_daily_999
```

Notes:
- Billing uses fixed NMI gateway endpoints for direct-post and query calls. Use
  sandbox/test credentials when `TEST_ENV=true`.
- OpenRails does not read PROCESSORS_* from `.env`. Seed merchant-scoped
  provider credentials through `openrails push-merchant-config`:

```yaml
version: 1
merchants:
  - slug: local-stack
    display_name: Local Stack
    provider_accounts:
      - provider_type: nmi
        environment: live
        account_id: mobius-profile-id
        mode: primary
        secrets:
          security_key: {env: NMI_SANDBOX_SECURITY_KEY}
          webhook_signing_secret: {env: NMI_WEBHOOK_SIGNING_SECRET}
```

- Collect.js origin restrictions belong to the browser origin that loads
  Collect.js. OpenRails should only receive the opaque `payment_token`.

## 2) Start the local stack

```bash
task docker-up
```

This starts:
- Postgres (OpenRails)
- Garnet/Redis
- ClickHouse (+ bootstrap)
- OpenRails migrations + OpenRails server

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
task e2e-nmi-sandbox
```

## 4) Seed a minimal local catalog

```bash
task seed-e2e-nmi
```

This creates:
- `billing.products.slug = e2e_mobius`
- one active `billing.prices` row with `processors.mobius.plan_id = $E2E_NMI_PLAN_ID`

## 5) Mint a JWT for API calls

```bash
task mint-jwt
```

This prints:
- `E2E_RUN_ID`
- `E2E_USER_ID`
- `E2E_JWT`

Keep those exported for the remaining steps.

## 6) Run the live NMI E2E

The script opens an ephemeral local browser page, loads NMI Collect.js with
`NMI_TOKENIZATION_KEY`, fills the sandbox card fields, captures the
`payment_token`, and then posts only that opaque token to OpenRails:

```bash
task e2e-nmi-live
```

The raw test card only enters NMI-hosted Collect.js fields in the browser; it is
never posted to OpenRails.

## 7) Create a subscription checkout session

Use:
- `X-E2E-Run-ID: $E2E_RUN_ID`
- `X-Idempotency-Key: e2e_${E2E_RUN_ID}_checkout`

Example (replace `price_PRICE_UUID` + `pm_PAYMENT_METHOD_UUID`):

```bash
curl -fsS "https://$CLOUDFLARED_PUBLIC_HOSTNAME/v1/me/checkout" \
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
- confirm the NMI portal shows cancellation (and/or Query API)
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
