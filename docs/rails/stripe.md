# Stripe

> Which flows are supported on this rail, and how well each one is verified:
> [rail certification matrix](certification-matrix.md).

Connect a Stripe account to OpenRails as a PSP on the `stripe` rail. Stripe is a
reserved gateway: the PSP name is `stripe` (a sandbox account is conventionally
`stripe-sandbox`). Purchase uses Stripe-hosted pages — the browser is redirected to
Stripe, enters card details there, and Stripe redirects back to your frontend;
fulfillment is driven by webhooks.

### API keys

OpenRails needs one server-side key per Stripe account: either a standard secret key
(`sk_live_…` / `sk_test_…`) or a restricted key (`rk_live_…` / `rk_test_…`). Both are
accepted; the live/test prefix must agree with the PSP's declared `environment`
(`live` or `test`):

- `environment: test` + a live key (`sk_live_`/`rk_live_`) → **refused**.
- `environment: live` + a test key (`sk_test_`/`rk_test_`) → **refused** rather
  than silently disabling the rail.
- A publishable key's `pk_live_`/`pk_test_` prefix is held to the same rule.

Key health is validated with a read-only `GET /v1/balance` — no charge is made.

### PSP manifest entry

Declare the account under `merchants.<slug>.psps.<key>` with `rail: stripe`:

```yaml
merchants:
  acme:
    psps:
      stripe:                    # PSP name (reserved gateway name)
        rail: stripe
        account_id: acct_XXXXXXXXXXXXXXXX
        secrets:
          secret_key: sk_live_...                # or rk_live_...
          webhook_signing_secret: whsec_...      # required in manifest mode; see Webhooks
```

`account_id` is your Stripe account id (`acct_…`). Copy it from the Dashboard, or run
`curl https://api.stripe.com/v1/account -u "sk_live_...:"`. OpenRails stores it as
declared; it does not derive it at runtime.

### Webhooks

The inbound endpoint is `POST /v1/webhooks/stripe/{account_id}` (standalone) or
`POST <prefix>/v1/webhooks/stripe/{account_id}` (embedded). Events
are verified against the `Stripe-Signature` header using the account's
`webhook_signing_secret`.

OpenRails registers the endpoint on Stripe **automatically** — at merchant bootstrap
and via an hourly reconcile job — subscribing exactly to the event types it handles
(`invoice.paid`, `invoice.payment_failed`, `checkout.session.completed` and the other
purchase-session events, `customer.subscription.updated/deleted`, `charge.succeeded`,
`charge.refunded`, `refund.created/updated`, `payment_method.attached` and
`.automatically_updated`, and dispute open/close). Registration is skipped when the configured `public_billing_base_url` is not a public
`https` URL, when the secret key is missing, or when provider writes are disabled.

Signing-secret handling depends on where the configuration lives:

- **Vault**: endpoint creation writes the returned signing secret into the PSP's
  document, keeping the outgoing one through the overlap. Only after that write
  can the old endpoint become superseded; a failed write keeps the previous
  configuration.
- **A file**: OpenRails cannot retain a newly minted key, so it refuses endpoint
  creation. Register the endpoint once in the Stripe Dashboard and declare its
  `whsec_…`. Reconciliation can manage the existing endpoint within
  provider-write policy.

The endpoint's `api_version` is pinned to the same single constant that stamps the
`Stripe-Version` header on every outbound call, so inbound event shapes and outbound
parsers can never drift apart.

**Version rollover.** Stripe's `api_version` is set at creation and cannot be
updated, and the signing secret is returned only at creation — so a bump needs a new
endpoint. It does not need an outage. OpenRails follows Stripe's own dual-endpoint
migration: the successor is **created first**, the predecessor is stamped
`metadata[openrails_superseded_at]` and **left enabled**, and the outgoing secret is
retained as `webhook_signing_secret_previous` so anything already queued on the old
endpoint still verifies — for at most 7 days (`webhook_overlap_expires_at`), after
which the old secret is refused. Both endpoints deliver during the overlap; duplicate events
are deduplicated by event id. Nothing is deleted by the bump.

**Rotating a signing secret** (Stripe and NMI). Publishing a new
`webhook_signing_secret` keeps the outgoing one verifying only for the
`webhook_secret_overlap` window (default 24h, at most 168h), then refuses it.
Rotate a leaked secret with `retire_webhook_overlap: true` on the PSP update
(`Client.UpdatePSP`) to refuse the old one at once; the same flag alone retires
a running overlap. A host-declared `webhook_signing_secret_previous` must carry the
setting `webhook_overlap_expires_at` (RFC 3339); without it the previous
secret never verifies.

Retiring the superseded endpoint is the only destructive step. It happens no sooner
than 7 days after it was replaced, requires a live enabled endpoint at the current
version, and is gated on the operator kill switch
(`billing.destructive_action_switch`), which is **off by default**. Until it runs,
an operator finding (`consistency.stripe_webhook_endpoint`) names every superseded
endpoint and when it becomes retireable; it auto-resolves once the rollover drains.

A signing secret missing locally is treated as **unknown, never lost**: OpenRails
first tries to repair the local record (a `psps` row whose `environment` or
`account_id` changed moves the derived secret name), and only if that fails does it
roll over. It never deletes a remote endpoint because of a local miss.

### Thin event destinations (account qualification required)

Stripe API-v1 thin events are a **private preview**, separate from SDK or CLI
support. OpenRails continues to register snapshot endpoints. It does not enable
thin delivery or change an existing endpoint to a preview API version. Qualify a
separate thin destination in a Stripe sandbox before activating one; configure its
secret as `webhook_signing_secret_thin` on the same exact account.

For supported `v1.*` equivalents of the snapshot events listed above, ingestion
verifies the original signed bytes, verifies the API key's account, retrieves the
full v2 event (using the documented `2025-11-17.preview` metadata version), and
fetches the related resource with OpenRails' regular pinned resource API version.
Only the canonical resource endpoint for that event type can be fetched. The
original context and event metadata survive normalization; `snapshot_event`, when
present, becomes the same deduplication key used by the snapshot destination.
Processing remains inline before acknowledgement except subscription convergence
and accepted engine-payment verification, which are durably enqueued. This does not make all webhook effects atomically exactly-once.

Unknown or malformed thin events, scope mismatches, and hydration failures return
an error so that delivery is not silently acknowledged. The supported
`v1.payment_intent.succeeded`, `v1.payment_intent.payment_failed`, and
`v1.payment_intent.requires_action` notifications resolve an existing accepted
engine operation from its merchant/account and immutable provider metadata,
then enqueue its receipt verification through River. Customer, payment method,
amount, currency, environment and retained payment identity must match. A
notification cannot admit a new charge, release a hold, or declare payment
success; the operation worker reads current Stripe truth and qualifies its
receipt. Native invoice payments without OpenRails engine metadata continue
through their existing handlers.

Real account preview access, provider event fixtures, and live destination
overlap remain unqualified by local transport tests.

References: [Stripe event destinations](https://docs.stripe.com/event-destinations)
and [Stripe snapshot-to-thin migration](https://docs.stripe.com/webhooks/migrate-snapshot-to-thin-events).

### Catalog ownership

New Stripe products and
prices stay in OpenRails, including secondary provider accounts. Recurring setup uses saved Customer/PaymentMethod references and
PaymentIntents; it creates no Stripe Subscription. One-time payment page uses
inline accepted product/price presentation without a maintained remote catalog.
Existing explicit Stripe links remain available for provider-owned agreements.

Legacy Stripe prices are declared under a product's `provider_prices`:

```yaml
provider_prices:
  legacy-monthly:
    psp: stripe
    price_id: price_existing
```

Applying reads the existing Stripe price's amount, currency and recurring
interval; access defaults to that interval and can be declared separately.
OpenRails does not create or edit a maintained Stripe catalog or Entitlement
Features. Hosted one-time Checkout uses inline price/product data for its
inactive, nonreusable payment objects; it creates no Stripe subscription. The pull reconciliation job reports drift without mutating Stripe.
New subscriptions are sold only on our `prices`, whose terms remain local.

### Purchase

With a declared `publishable_key` the page takes a card in Stripe Elements: the
`pm_` it makes pays an order as `payment: {token, psp_id}`, one PaymentIntent
that charges and saves it, with 3-D Secure answered in the page through the
order's `authenticate` next action and `confirm`. This is how Stripe
subscriptions are sold. `POST /v1/me/payment-methods` saves a card without
buying (one SetupIntent).

An order of one-time lines can also be paid on a Stripe-hosted Checkout
session: its Stripe option accepts `redirect`, and `payment: {return_url, psp_id?}` opens a payment-mode Purchase
Session for the order's total. The order answers `requires_action` with
`next_action: {type: "redirect_to_url", url}`; the session's completion (its
webhook, or `POST /v1/me/orders/{id}/confirm` reading it) completes the order,
and its expiry returns the order to `requires_payment_method`. Canceling or
expiring the order expires the session. `return_url` must be on an allowed
origin.

On a server serving the payment page, a declared Stripe PSP gets the
payment origin registered as a payment-method domain on its account
(through Stripe's payment method domains API, after reading whether the
account holds it),
so the page's Express Checkout Element offers Apple Pay and Google Pay; see
[Payment page](../payment-page.md#apple-pay-and-google-pay).

### Card updates

`payment_method.automatically_updated` (Stripe's card updater) and
`payment_method.updated` (a change through Stripe's API) are versions of the
same payment method, read from Stripe: brand, last four, expiry and
fingerprint. A same-brand reissue keeps billing, and memberships waiting on the
card retry. A reissue under another brand needs the customer's consent again:
the card's mandates wait (`requires_reconsent`), OpenRails refuses to charge it
off-session (renewals it bills, invoice collection) with failure code
`stored_credential_required`, and asks the members it pays for to act. The
customer paying an order with the card (the renewal waiting on it included)
first confirms a SetupIntent on the same card with them present, and new
mandates cite it. A detached card is
removed and its mandates end.

### Subscriptions Stripe bills

Stripe runs the retries of the subscriptions it owns, and OpenRails mirrors its
status. `unpaid`, `paused`, and a `past_due` subscription whose open invoice has
no further payment attempt grant no access. A `past_due` subscription Stripe is
still retrying keeps access until Stripe cancels it or stops retrying.

The customer manages them in Stripe's own portal:
`POST /v1/me/stripe/billing-portal-sessions` answers its `url`. A merchant
without an armed Stripe PSP answers `404 route_not_found`.

A portal cancel at period end and a portal resume are mirrored as the member's
own cancel and resume; a subscription whose paid period is over is not resumed
by Stripe's status alone. A price change takes effect only once Stripe's paid
invoice bills the new price: an upgrade whose invoice is still open grants
nothing.

A dispute or a provider refund that revokes access also queues a durable
`stripe_cancel_subscription` intent in the same transaction. It reads the
subscription first: a paid-up one is set to `cancel_at_period_end`, a delinquent
one is ended at once so its open invoice stops retrying. Failures retry, and an
abandoned intent raises `life.provider_intent.abandoned`.

### Sandbox testing

Declare the PSP `environment: test`, on a sandbox merchant (`sandbox: true`), with a test key (`sk_test_…` / `rk_test_…`).
A test PSP can never hold a credential that moves real money, and a
deployment with a test PSP holds no live one. Use Stripe's standard test
cards.

Your own end-to-end tests need no Stripe account: package
`openrailstest/stripemock` is a fake Stripe on loopback. Set
`Config.ProviderSandbox.StripeAPIURL` to its `URL()` (or pass `Transport()` as
`Deps.StripeTransport`), declare a Stripe PSP with
`Environment: openrails.EnvironmentTest` and any `sk_test_` key, and point `SendWebhooksTo` at
your mounted `/v1/webhooks/stripe/{account_id}` with the PSP's signing secret.
Without a publishable key a customer pays on hosted Checkout:
`CompleteCheckoutSession(ctx, id)` is the customer paying there, and Stripe's
signed `checkout.session.completed` reaches your route; `Redeliver` sends it
again. `CompleteSetup` saves a card for Elements, `SetDecline` declines one,
and `Ledger` reads what Stripe charged. The package comment has the wiring.

### Read-only safety gate

Every outbound Stripe byte flows through one choke-point HTTP client. With
`provider_write_mode: readonly`, any mutating request (anything but GET/HEAD) fails locally —
before reaching the network — while reads (verification, reconciliation, webhook
hydration) pass through. This makes read-only mode a transport-level guarantee, not
a convention: no code path can write to Stripe around it.
