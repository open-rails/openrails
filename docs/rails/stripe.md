# Stripe

Connect a Stripe account to OpenRails as a PSP on the `stripe` rail. Stripe is a
reserved gateway: the PSP name is `stripe` (a sandbox account is conventionally
`stripe-sandbox`). Checkout uses Stripe-hosted pages — the browser is redirected to
Stripe, enters card details there, and Stripe redirects back to your frontend;
fulfillment is driven by webhooks.

### API keys

OpenRails needs one server-side key per Stripe account: either a standard secret key
(`sk_live_…` / `sk_test_…`) or a restricted key (`rk_live_…` / `rk_test_…`). Both are
accepted; the live/test prefix is enforced against your deployment posture:

- `test_mode: sandbox` + a live key (`sk_live_`/`rk_live_`) → **refuses to boot**.
- Live mode + a test key → refuses to boot outside development (in development the
  key is warned about and the rail disabled).

Key health is validated with a read-only `GET /v1/balance` — no charge is made.

### PSP manifest entry

Declare the account under `merchants.<slug>.psps.<key>.stripe`:

```yaml
merchants:
  acme:
    psps:
      stripe:                    # PSP name (reserved gateway name)
        stripe:                  # rail block
          environment: live      # assertion, cross-checked against test_mode (test|live)
          account_id: acct_XXXXXXXXXXXXXXXX
          secrets:
            secret_key: sk_live_...                # or rk_live_...
            webhook_signing_secret: whsec_...      # required in manifest mode; see Webhooks
```

`account_id` is your Stripe account id (`acct_…`). Stripe is the one rail where it
is discoverable from the key itself — copy it from the Dashboard, or run
`curl https://api.stripe.com/v1/account -u "sk_live_...:"`. OpenRails stores it as
declared; it does not derive it at runtime.

### Webhooks

The inbound endpoint is `POST /v1/merchants/{slug}/webhooks/stripe/{account_id}`
(single-merchant installs also serve the shared `POST /v1/webhooks/stripe`). Events
are verified against the `Stripe-Signature` header using the account's
`webhook_signing_secret`.

OpenRails registers the endpoint on Stripe **automatically** — at merchant bootstrap
and via an hourly reconcile job — subscribing exactly to the event types it handles
(`invoice.paid`, `invoice.payment_failed`, `checkout.session.completed` and the other
checkout-session events, `customer.subscription.updated/deleted`, `charge.succeeded`,
`charge.refunded`, `refund.created/updated`, `payment_method.attached`, and dispute
open/close). Registration is skipped when the configured `api_url` is not a public
`https` URL, when the secret key is missing, or when provider writes are disabled.

Signing-secret handling depends on the merchant source:

- **API/DB mode** (`merchant_source: api`): when OpenRails creates the endpoint,
  Stripe mints the signing secret and OpenRails stores it in the merchant secret
  store. Fully hands-off.
- **Manifest mode** (`merchant_source: manifest`): a freshly minted secret would
  live only in process memory and be lost on reboot, so OpenRails refuses to create
  the endpoint. Register it once in the Stripe Dashboard (same URL, same events) and
  declare its `whsec_…` as `secrets.webhook_signing_secret`; reconcile then manages
  the existing endpoint in place.

The endpoint's `api_version` is pinned to the same single constant that stamps the
`Stripe-Version` header on every outbound call, so inbound event shapes and outbound
parsers can never drift apart. A version bump (or a lost secret) triggers a
delete-and-recreate of the endpoint.

### Catalog sync

The Stripe catalog adapter pushes OpenRails catalog definitions into your Stripe
account with **find-or-create** semantics — identity is content-based, so re-syncing
(even after a database rebuild) reattaches to the same Stripe objects instead of
duplicating them:

- **Products**: found by metadata (`openrails_product_key = <product_key>`), created
  if absent with an idempotency key derived from the product key.
- **Prices**: found by a deterministic `lookup_key`
  (`openrails.<product_key>.<currency>.<unit_amount>.<cycle>`), created if absent.
  Amounts convert micros → cents exactly; a non-cent-representable amount errors.
- **Entitlement Features**: each entitlement string on a product is mirrored to a
  Stripe Feature (`lookup_key` = the entitlement string) attached to the Product.
  One-way, best-effort — OpenRails stays the source of truth; a feature-sync failure
  never fails the price link and surfaces as drift on the next reconcile.

Alternatively, link an existing Stripe Price (`psp_links.stripe.price_id`);
OpenRails round-trips it and rejects the link if amount, currency, recurring terms,
or product association don't match the catalog. The pull reconciliation job is
alert-only — it reports drift and never mutates your Stripe objects.

### Checkout

`POST /v1/checkout` with `payment.rail: "stripe"` creates a Stripe-hosted Checkout
session; the response's `next_action` carries the redirect URL. `success_url` and
`cancel_url` are supplied on the request (not configured server-side) and are
required for the hosted flow. Completion arrives via `checkout.session.completed`
(and the async-payment variants) on the webhook.

### Sandbox testing

Set `test_mode: sandbox`, declare the PSP with `environment: test` and a test key
(`sk_test_…` / `rk_test_…`). The live-key-under-sandbox boot refusal guarantees a
sandbox deployment can never hold a credential that moves real money. Use Stripe's
standard test cards. `test_mode` is orthogonal to the environment axis — a fully
gated production-style deployment can legitimately run sandbox rails.

### Read-only safety gate

Every outbound Stripe byte flows through one choke-point HTTP client. With
`mode: readonly`, any mutating request (anything but GET/HEAD) fails locally —
before reaching the network — while reads (verification, reconciliation, webhook
hydration) pass through. This makes read-only mode a transport-level guarantee, not
a convention: no code path can write to Stripe around it.
