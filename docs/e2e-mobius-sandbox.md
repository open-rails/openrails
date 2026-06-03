# Mobius/NMI Sandbox E2E Runbook (OpenRails)

This runbook exercises the **real** Mobius/NMI sandbox end-to-end:
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
  - Signing secret: **exactly** `PROCESSORS_MOBIUS_WEBHOOK_SECRET` (shared secret/HMAC)

## 1) Configure `.env`

Minimum set (fill in real values):

```bash
# sandbox mode
TEST_MODE=true

# OpenRails must accept tokens minted by the local issuer container (issuer claim is http://issuer:8080)
AUTH_ISSUERS='["http://issuer:8080"]'
AUTH_EXPECTED_AUDIENCE=openrails-app

# AuthKit devserver mint secret (used by scripts/mint_jwt.sh); choose a local-only random value
AUTHKIT_DEV_MINT_SECRET=$(openssl rand -hex 32)

# Mobius/NMI keys
PROCESSORS_MOBIUS_SECURITY_KEY=...
PROCESSORS_MOBIUS_TOKENIZATION_KEY=...          # public (Collect.js)
PROCESSORS_MOBIUS_TOKENIZATION_URL=...          # Collect.js script URL you want to test
PROCESSORS_MOBIUS_WEBHOOK_SECRET=...            # HMAC shared secret for webhooks

# Cloudflared (deterministic webhook hostname)
CLOUDFLARED_TUNNEL_TOKEN=...
CLOUDFLARED_PUBLIC_HOSTNAME=openrails-webhooks.example.com

# Sandbox plan id for local seed
E2E_MOBIUS_PLAN_ID=YOUR_SANDBOX_PLAN_ID
```

Notes:
- Billing uses fixed NMI gateway endpoints for Mobius/NMI direct-post and query calls. Use sandbox/test credentials when `TEST_MODE=true`.
- Collect.js origin restrictions: if your tokenization key is origin-locked, you must load the harness over **HTTPS** via Cloudflared (not `http://localhost`).

## 2) Start the local stack (+ local issuer)

```bash
task docker-up-e2e-sandbox
```

This starts:
- Postgres (billing)
- Garnet/Redis
- ClickHouse (+ bootstrap)
- billing migrations + billing server
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
https://$CLOUDFLARED_PUBLIC_HOSTNAME/debug/mobius/tokenization?mode=real
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
