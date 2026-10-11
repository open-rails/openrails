# Hosted checkout

A page the standalone server serves where a customer pays one order, on an
origin of its own. The merchant's backend creates the order with `checkout`,
sends the customer to its checkout URL, and learns of payment from the
`order.completed` host event. It works like Stripe Checkout: a short-lived
URL, the provider's own card fields, 3-D Secure in the page, then back to the
merchant. Embedded hosts sell on their own pages with billing-ui instead.

## Turning it on

The standalone server serves it at `hosted_checkout.url` (`HOSTED_CHECKOUT_URL`),
or `server.Config.HostedCheckoutURL`:

```yaml
hosted_checkout:
  url: https://checkout.example.com
```

The URL is an origin (no path) the server answers by `Host`, so point its DNS
and TLS at the same server. It must differ from `public_billing_base_url`,
AuthKit's issuer and the frontend's origin: the page runs the PSPs' scripts
and never shares an origin with the API or the console. Boot refuses
otherwise. Without the setting the server serves no checkout, and an order
asking for one is refused `400 hosted_checkout_unavailable`.

The page is `web/checkout`, built from billing-ui and embedded in the binary
(`task checkout-build`; the release image carries it). A host building its own
binary on `server` builds it with `scripts/build-checkout-page.sh <dir>` and
passes it as `server.Deps.CheckoutAssets`. A host that mounts `Routes()` on its
own router sends the checkout host's requests to `HostedCheckout()`.

## Creating one

`POST /v1/app/orders` (`Client.CreateOrder`) with `checkout`:

```json
{
  "customer_id": "4d1c…",
  "lines": [{ "price_id": "price_…" }],
  "checkout": {
    "success_url": "https://shop.example/thanks?order={ORDER_ID}",
    "cancel_url": "https://shop.example/cart",
    "saved_payment_methods": false
  }
}
```

- `success_url` is where the customer goes once the order is complete, or
  processing on an asynchronous rail; `cancel_url` is the page's back link.
  `{ORDER_ID}` in either becomes the order's id. They are https (http on
  loopback) and may name any site: only your backend sets them.
- `expires_at` is 30 minutes to 24 hours away; omitted, an hour. An order
  given no `expires_at` of its own expires with its checkout, and
  `order.expired` follows.
- `saved_payment_methods` shows the customer's saved cards they allowed to be
  shown again. Set it only when your site signed the customer in.

The answer's `checkout.url` is the checkout URL. Redirect the customer to it.
It is shown once: OpenRails keeps only a hash of its secret, every later read
shows `url: null`, and a retry with the same `Idempotency-Key` answers the
same URL. A lost URL means canceling the order and creating another.

## Paying

The URL carries a secret in its fragment, which browsers never send to a
server. The page keeps it for the tab, drops it from the address bar, and
sends it as a bearer token to its own origin. It acts as the order's
customer for that order alone: `GET /v1/me/orders/{id}`, `pay`, `confirm`,
`GET /v1/config` and, when offered, `GET /v1/me/payment-methods`. It opens
nothing else, and the API's own origin does not accept it. Past the
checkout's expiry it answers `410 checkout_expired`.

The customer pays with a new card in the PSP's own fields, charged and saved
in one request, with a saved card when offered, or with Apple Pay or Google
Pay (below). The page never takes a card number itself. A bank's challenge runs in the page. Reads and pays under one
URL are rate limited, a pay the server challenges asks the customer to solve
the deployment's captcha, and an order that has had ten cards declined takes
no more (`409 checkout_attempts_exhausted`).

Fulfil on `order.completed`, never on the redirect to `success_url`.

## Apple Pay and Google Pay

A merchant with a Stripe PSP that has a `publishable_key` is offered Apple Pay
and Google Pay through Stripe's Express Checkout Element, where the customer's
browser and wallet support them. The wallet's card is charged as a new card is.

Stripe shows them only on a payment-method domain registered on the merchant's
account, so declaring a Stripe PSP registers the checkout host there: a
`stripe_payment_method_domain` provider intent, once per PSP and host. A PSP
declared before the server served the checkout is registered when its merchant
next creates an order with a checkout. A domain the account already holds is
not registered again (its Apple Pay is validated when inactive), and one the
merchant disabled in Stripe stays disabled. The checkout host serves Stripe's
Apple Pay domain-association file at
`/.well-known/apple-developer-merchantid-domain-association`.

NMI's wallets are not offered.

## The page

Merchant name, logo and support link come from the merchant's settings
(`settings.profile`): the logo and support link only when they are https. The
page is never framed (`frame-ancestors 'none'`) and sends no referrer.
