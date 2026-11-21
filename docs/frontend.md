# Frontend

Use [the Billing UI SDK](../sdk/billing-ui/README.md) for a browser checkout or
customer billing screen. The [API conventions](api/conventions.md#money-ids-and-times)
explain how to handle amounts as decimal strings in native units.

## Authentication

The customer credential identifies the merchant and the customer. Send the
credential in a header and keep backend credentials on the server.

| Integration | Browser credential and endpoint |
| --- | --- |
| [Embedded](embedded.md) | Send the application's user credential to the OpenRails routes mounted by your Go server, such as `/billing/v1`. |
| [Self-hosted standalone](standalone.md) | Send an `openrails:self` access token to the billing service. Your trusted issuer supplies it through authorization code or token exchange. |
| [OpenRails SaaS](saas.md) | Follow the SaaS guide for hosted checkout or customer tokens issued by the platform. |

For deployments that use a backend-signed RFC 7523 assertion, the browser can
redeem the assertion and keep the resulting access token in memory.

```ts
import { createBillingClient, redeemAssertion, tokenSource } from "@openrails/billing-ui"

const billing = createBillingClient({
  baseUrl: "https://billing.example/v1",
  getToken: tokenSource({
    fetchToken: async () => {
      const { assertion } = await (await fetch("/api/billing/assertion", { method: "POST" })).json()
      return redeemAssertion({ tokenEndpoint: "https://billing.example/oauth2/token", assertion })
    },
  }),
})
```

`tokenSource` keeps the token in memory (never storage) until `skew` seconds (60)
before expiry, fetches the next once however many calls wait, and resends a call
answered 401 `credential_expired` once (the gate refused it before the route ran,
so this is safe for writes). A `fetch` adding a DPoP proof (auth-ui's `dpopFetch`)
binds the token to the browser's key. CORS allows any origin with
`credentials: "omit"`.

## Self-service routes

The authenticated customer selects their own orders, payments, cards, balance,
usage, subscriptions and entitlements through `/v1/me`. Find the current paths
and methods in the [HTTP route list](api/routes.md).

`GET /v1/me/usage` accepts `from`, `to`, and a limit of 1 to 1,000 groups. It
uses the last month by default. Keep the currency, grouping, and time window
fixed while paging. For a storefront you render, fetch the products on the
server with `Client.ListProducts` and `billing.BuyerPrices`. Pass them to
`ProductList` or `BuyButton` in the browser.

## Configuration

`GET /v1/config` (public) and `GET /v1/me/config` (the merchant's, for a bound
credential) are described in [conventions](api/conventions.md#public-configuration).
billing-ui reads each once (`useConfig()`, `useCustomerConfig()`); a page your
server renders can carry both into `BillingProvider` as `config` and
`customerConfig` (embedded: `engine.PublicConfig(routes)` and
`engine.CustomerConfig(routes, r)` with your own request; remote:
`GetPublicConfig`).

`payment.psps` lists only armed PSPs. Switch on `flow`, never on a list of rails:

```json
{ "psps": [
  { "psp_id": "psp_…", "key": "nmi-main", "rail": "nmi", "custodian": "psp", "flow": "tokenize", "routed": true,
    "config": { "tokenization_key": "<public Collect.js key>", "tokenization_url": "https://secure.networkmerchants.com/token/Collect.js" } },
  { "key": "nmi-bt", "rail": "nmi", "custodian": "basis_theory", "flow": "tokenize", "config": { "public_api_key": "<public key>" } },
  { "key": "stripe", "rail": "stripe", "custodian": "psp", "flow": "elements", "routed": true, "config": { "publishable_key": "pk_test_…" } },
  { "key": "ccbill", "rail": "ccbill", "custodian": "psp", "flow": "redirect" },
  { "key": "solana", "rail": "solana", "custodian": "psp", "flow": "wallet" } ] }
```

- `tokenize`: load `tokenization_url`, tokenize with the key, send the token.
- `elements`: Stripe Elements makes a `pm_`, which an order's `payment.token`
  charges and saves, or `POST /v1/me/payment-methods` saves.
- `redirect`: paying answers a `redirect_to_url` (CCBill; Stripe without a
  publishable key, one-off only).
- `wallet`: the buyer's wallet pays; `payment.solana` has network and tokens.

`custodian` is who holds the card (`psp`, or a third party whose SDK you tokenize
against), not who charges it. `routed` marks PSPs taking new purchases under the
merchant's routing; others stay for existing cards. Only public values appear.

What each rail sells: [rails](rails/README.md). An order's `payment_options` (and a
preview's) list the PSPs that can take its lines, each with `psp_id`, `rail`,
`accepts` (`saved_card`, `new_card`, `redirect`, `solana_pay`) and
`failed_attempts`. billing-ui's `fallbacks` prop holds a PSP back until others fail
(`fallbacks={{ ccbill: { for: ["nmi-main"], afterFailures: 2 } }}`), offering it
under "Card payment isn't working?".

## Orders

A browser buys through an order: priced lines read and paid by their own customer.

- `POST /v1/me/orders/preview` prices lines (with refusals) and lists
  `payment_options`. `POST /v1/me/orders` creates the order, and with `payment`
  and `expected_total` pays it in the same call; without, it is `open` for
  `/{id}/pay`. Asked again for the same lines without a payment, it answers the
  customer's open order of them (so reloads make no second order).
- A payment, here and on `/pay`, takes an `Idempotency-Key` for its attempt.
  `payment` is a saved card `{payment_method_id}` or a new one
  `{token, psp_id?, billing_details?}` (a Stripe `pm_`, an NMI Collect.js token, a
  Basis Theory token intent). One provider request charges and saves a new card;
  a declined card is not saved. In the EEA and UK a new card is kept for later
  only with `reusable: true` (the customer's unticked opt-in).
- `status` is `open`, `processing`, `complete`, `canceled`, `expired` or (renewal)
  `void`; `payment.status` is `requires_payment_method`, `requires_action`,
  `processing` or `succeeded`.
- `requires_action` carries `next_action`: `authenticate` (run the PSP's script,
  e.g. Stripe.js `handleNextAction` with `payload.client_secret`, then
  `POST /v1/me/orders/{id}/confirm`), `redirect_to_url` (top window; back at an
  allowed `return_url`), or `solana_pay` (render `url` as a QR code or wallet link;
  the wallet fetches the transaction from `/v1/orders/{id}/solana-pay`).
- A decline is `402` (`card_declined`, `payment_failed`) with the order in
  `error.order`, open again with `payment.last_payment_error`. Fulfil on the
  `order.completed` event or a read of the order, never the browser's word.
- `billing_details` (`name`, `email`, `phone`, `address`) is one object
  everywhere; collect the name in one `autocomplete="cc-name"` field.
- A line of a `customer_amount` price takes `amount`; fixed prices refuse one.
- A second subscription in one tier group is refused `already_owned` with the hint
  `change`.
- A Solana plan's order subscribes: the wallet's transaction is the delegated
  approval and first pull; `confirm` with `{signature}` reads it at once.

**Merchant orders.** Your backend may create an order for a customer
(`CreateOrder`, `POST /v1/app/orders`, `merchant:orders:manage`; `PreviewOrder`,
`CancelOrder`): open for 30 days unless `expires_at` says otherwise, and only that
customer pays it, signed in (`<OrderPayment orderId>`). Staff read orders and
never pay them.

**Renewals** OpenRails bills are orders (`origin: subscription`, `renewal:
{subscription_id, period_starts_at, period_ends_at, due_at}`). A declined one stays
`open`, overdue past `due_at`, named by the subscription's `recovery.order_id`; the
customer pays it with a saved card (another card becomes the subscription's) or
completes its authentication. Canceling the subscription voids it. A subscription
its PSP schedules retries with `POST /v1/me/subscriptions/{id}/retry-now` (200
succeeded, 202 processing; `customer_payment_unsupported` elsewhere). A retry of
the card the subscription waits to have replaced, or one past the retry limit
([dunning](dunning.md)), is refused before any provider call: `retry-now` answers
`payment_not_retryable` and the recovery reads
`blocked_reason: payment_method_update_required`; a renewal order paid with that
card answers `order_not_payable`. Another card is admitted. A customer the
card-testing ledger blocks gets `429 card_attempts_blocked`. Paying
requires the customer in person: a delegated invoker, an application or a personal
API key is `403 customer_action_required`. Reads report holds without charging:
`dunning.paused` with `pause_reason` `automatic_collection_paused` (limited) or
`provider_writes_disabled` (readonly), and `recovery.last_failure_reason`.
`GET /v1/me/orders` filters by `subscription_id`, `status`, `origin` and
`overdue=true`. Provider-run bills are orders too: check `collection_owner`
([provider-managed orders](provider-managed-orders.md)).

**Receipts after a lost response.** `GET /v1/me/orders/{id}/payment-attempt` (and
`GET /v1/app/orders/{id}/payment-attempt`, `GetOrderPaymentAttempt`) with the
original pay `Idempotency-Key` returns that exact attempt's outcome, read-only;
`payment_method_id` appears only for the succeeded operation that kept the card.

**Cart.** One per customer per merchant, shared by its sites: `GET /v1/me/cart`
answers its lines priced as a preview; `PUT /v1/me/cart/lines/{price_id}`
`{quantity?, amount?}` adds or changes (0 removes; another price of the same
product replaces); at most 100 lines, nothing reserved, expires 30 days after its
last change. `cart: true` in place of `lines` previews or buys it. billing-ui's
`<Cart>`; `<OrderPayment orderId>` pays any order (saved card, new card, hosted
page, Solana wallet, Apple and Google Pay through Stripe's Express Checkout with
`wallets`), and `<EmbeddedPayment url>` frames a pay URL ([payment page](payment-page.md)).

## Payment methods

A method is `{id, customer_id, rail, psp_id, card, billing_details, health,
subscriptions, default_currencies, created_at}`; `card` is `{brand, last4,
exp_month, exp_year}`, read from the PSP. `psp_id` is null for a card a custodian
holds. A customer's default card per currency
(`PUT /v1/me/default-payment-methods/{currency}`) pays their usage orders and every
subscription in that currency without its own card; changing it moves those
subscriptions. A move carries the recurring agreement to the card, verifying it
first ($0) where needed (`402 card_declined` changes nothing); a card that cannot
pay a subscription's PSP is `409 payment_method_psp_mismatch`.

- **Save** (`POST /v1/me/payment-methods`, `Idempotency-Key`): `psp_id`, the
  PSP's `token` (or `card` for server card entry) and `billing_details`. A Stripe
  card needing 3-D Secure answers `requires_action`, finished with
  `POST /v1/me/payment-methods/{id}/confirm` (abandoned ones go after a day). A
  different token is a new attempt and a new key; after
  `provider_outcome_unknown`, refresh the list before trying again.
- **Edit** (`PATCH`): expiry, `billing_details`, `reusable`. A new number is a new
  method.
- **Delete**: `204` when provider and local removal are confirmed, `202` while it
  converges (keep it visible and refresh). Stripe cards are managed in Stripe's
  billing portal.
- `status` is `active`, `closed`, `replaced` or `removed`; only active ones are
  charged. A card reissued under another brand turns its `mandates`
  `requires_reconsent` until the customer pays with it in person.

## Errors

The envelope is `{"error": {"type", "code", "message", "param"}}` ([errors](api/errors.md)).

- **401** `credential_expired`, `authentication_required`: get a new token, retry once.
- **403** someone else's resource, or `captcha_required` (solve, send `X-Captcha-Token`).
- **409** `idempotency_key_in_use`, `payment_in_progress` (the original is still
  running), `provider_outcome_unknown` (read before retrying).
- **410** `order_attempt_expired`: start a new one.
- **422** `idempotency_key_reused`: same key, other terms.
- **413**, **429** with `Retry-After`: purchase allows 10 a minute, subscription
  writes 20, payment methods 40 ([limits](limits.md)). Never retry purchases in a loop.
