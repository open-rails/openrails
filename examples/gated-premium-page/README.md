# Gated premium page — OpenRails standalone demo

A ~300-line Go webserver showing the whole standalone integration:

- `/` — public page
- `/premium` — gated on an OpenRails **entitlement** (`ListActiveEntitlements`
  via the root SDK); no entitlement → redirect to `/buy`
- `/buy` — browser-direct checkout against OpenRails' `/v1/me/*` surface:
  delegated token from `/api/token`, NMI Collect.js tokenized-vault flow,
  sandbox test card
- `/api/token` — the ONE backend endpoint the frontend needs: swaps the (fake,
  signed-cookie) session for a short-lived delegated JWT

Payment configuration is **discovered, not configured**: the buy page reads
OpenRails' public `GET /v1/checkout-config` for the merchant's armed PSPs and
their public tokenization values, so this app holds no rail, PSP or Collect.js
settings of its own.

It is the living companion to
[docs/standalone-integration.md](../../docs/standalone-integration.md),
[docs/frontend-integration.md](../../docs/frontend-integration.md) and
[docs/rails/nmi.md](../../docs/rails/nmi.md).

## Setup

> **Shortcut / proven path**: `bash e2e/provision.sh` provisions everything
> below against the local compose stack (and `e2e/run-e2e.sh` proves the whole
> demo headlessly against the real NMI sandbox). [E2E.md](E2E.md) documents
> the walkthrough and the engine gaps the script routes around — read it if a
> step below misbehaves.

You need a running OpenRails with a merchant, an NMI sandbox PSP, a catalog,
and an API key. The deployment must run with `PROVIDER_WRITE_MODE=full`
(unset fail-closes to readonly and every charge is refused). Locally:

1. **Stack**: `task docker-up` in the repo root, then
   `curl localhost:3053/health/ready`.
2. **Issuer key**: run the app once (`go run .`) — it generates
   `issuer_key.pem` + `issuer_key.pem.pub` and exits (no API key yet). Register
   the public key on your merchant in `merchants.yaml`:

   ```yaml
   merchants:
     demo:
       display_name: Demo
       remote_application:
         issuer: https://gated-premium-page.example
         public_keys:
           - kid: demo-1
             public_key_pem: |
               <contents of issuer_key.pem.pub>
       psps:
         mobius-sandbox:
           nmi:
             environment: test
             account_id: "<dashboard Gateway ID>"
             settings:
               tokenization_key: <public Collect.js key>
             secrets:
               security_key: <sandbox security key>
               webhook_signing_secret: <sandbox webhook secret>
   ```

   Sandbox credentials come from your ISO — see
   [docs/rails/nmi.md](../../docs/rails/nmi.md). Push it (and the auth
   bootstrap) per the "First run" section of
   [docs/standalone-integration.md](../../docs/standalone-integration.md).
3. **Catalog** (`push-merchant-catalog --insert --overwrite`):

   ```yaml
   catalogs:
     - merchant: demo
       products:
         - key: premium
           display_name: Premium
           entitlements: [premium]
           prices:
             - currency: usd
               unit_amount: 5_000_000   # $5.00 — micros everywhere
               duration: 30d
               auto_renew: true
               psps: [mobius-sandbox]
               # NMI is link-only: a recurring price sells ONLY once it is
               # linked to an existing NMI plan (create one with the direct-post
               # `recurring=add_plan` API — same amount + day_frequency):
               psp_links:
                 mobius-sandbox:
                   plan_id: openrails_demo_premium_500_d30
   ```

   Also set the merchant's `api_host` (Host→merchant resolution for the
   public catalog `GET /v1/products` the buy page fetches) — locally
   `api_host='localhost'`; see E2E.md for the how (no API surface yet).
4. **API key**: `POST /v1/merchant/api-keys {"name":"demo","role":"owner"}`,
   authenticated per [docs/merchant-provisioning.md](../../docs/merchant-provisioning.md)
   (delegated JWT from the issuer you just registered, an operator session, or
   the admin console). Export the returned `openrails_st_…` secret.
5. **Run**:

   ```bash
   OPENRAILS_API_KEY=openrails_st_... go run .
   ```

   Nothing payment-shaped is configured here. The buy page reads
   `GET /v1/checkout-config` — a public, unauthenticated, per-merchant
   document listing the merchant's armed PSPs and, for each, how a browser
   drives it (`flow`) plus the public values it needs (for NMI, the Collect.js
   `tokenization_key` + `tokenization_url`). The PSP key it returns is the
   `payment.rail` wire value and the catalog `psps:` name.

Renewals (not the first charge — that is synchronous) need the NMI webhook
pointed at OpenRails; for local tunnels see
[docs/dev/local-webhooks.md](../../docs/dev/local-webhooks.md).

## The six-step demo

1. Open http://localhost:8080 — the public page.
2. Click **Log in** — a signed cookie holding the demo user id (this is the
   stand-in for your app's real auth).
3. Visit **/premium** — the backend asks OpenRails for the user's active
   entitlements, finds none, and redirects to **/buy**.
4. Pick the plan and pay with the sandbox test card
   `4111 1111 1111 1111`, expiry `10/29`, any CVV. The card goes browser →
   NMI (Collect.js); OpenRails only ever sees the one-time token.
5. Checkout succeeds synchronously; the page redirects to **/premium** —
   the entitlement is active, the page unlocks.
6. Verify from the outside: `GET /v1/me/status` with a delegated token from
   `/api/token`, or the merchant admin console.

## Environment

| Var | Default | Meaning |
|---|---|---|
| `OPENRAILS_BASE_URL` | `http://localhost:3053` | OpenRails origin |
| `OPENRAILS_API_KEY` | — (required) | merchant API key (`openrails_st_…`) |
| `MERCHANT_SLUG` | `demo` | merchant this demo belongs to |
| `DEMO_USER_ID` | fixed UUID | the fake logged-in user (must be a UUID) |
| `ENTITLEMENT` | `premium` | entitlement string gating `/premium` |
| `DEMO_ISSUER` | `https://gated-premium-page.example` | registered `remote_application.issuer` |
| `ISSUER_KEY_FILE` | `issuer_key.pem` | RS256 signing key (generated on first run) |
| `SESSION_SECRET` | random per boot | HMAC key for the demo session cookie |
| `PORT` | `8080` | listen port |

## Local dev against the parent checkout

`go.mod` already carries `replace github.com/open-rails/openrails => ../..`,
so the example always builds against the repo checkout — no `go.work` needed.
