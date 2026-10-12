# Hosted checkout

A page where a customer pays one order, on an
origin of its own. The merchant's backend creates the order with `checkout`,
sends the customer to its checkout URL, and learns of payment from the
`order.completed` host event. It works like Stripe Checkout: a short-lived
URL, the provider's own card fields, 3-D Secure in the page, then back to the
merchant. Standalone and embedded hosts use the same checkout surface.

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

## Embedded applications

Before starting an embedded engine or serving HTTP, configure its checkout:

```go
host, checkout, err := engine.HostedCheckout("https://pay.example.com", assets)
if err != nil { return err }
// Dispatch only requests for host to checkout; keep the application's router
// on its own origin. Then start the engine and host workers normally.
```

`assets` is an `fs.FS` rooted at the built `web/checkout` page; build it from the
pinned module with `scripts/build-checkout-page.sh`. The handler owns checkout
security headers, credential resolution, framing and rate limits. It mounts no
host AuthKit or admin routes. The origin must differ from the engine's API,
dashboard and declared merchant issuers, including equivalent port/host spellings.

When two applications share a merchant's billing database and one serves the
payment domain, the other calls `engine.HostedCheckout(origin, nil)` only to
configure URL issuance; its returned handler is nil. Each application still
owns its authentication. Its pages sell with billing-ui's `<BuyButton>`, which
creates the signed-in customer's order with its embedded checkout on the
payment domain (below); it needs no purchase route of its own. A publisher's
inbox and notification receipt state remain independent of checkout.

## Creating one

`POST /v1/app/orders` (`Client.CreateOrder`) with `checkout`:

```json
{
  "customer_id": "4d1c…",
  "lines": [{ "price_id": "premium:buy-usd:v1" }],
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
The order stores its secret's hash. A bounded creation receipt retains only
that secret, in the same transaction, until the checkout's original expiry.
An authenticated retry with the same `Idempotency-Key` and request recovers
the same URL while the order and checkout remain open. The reply reads the
current order: completed, canceled or expired checkouts return no URL, and a
retry never extends either expiry. Ordinary order reads always show `url: null`.

Creation receipts use the existing expiry cleanup and are excluded from billing
archives. If a receipt has expired or is unavailable after a restore, no secret
is reconstructed from its hash; cancel an unpaid order and create another when
a new checkout link is needed. Create the replacement only after cancellation
succeeds; an unresolved payment must be settled or reconciled first.

## Paying

The URL carries a secret in its fragment, which browsers never send to a
server. The page keeps it for the tab, drops it from the address bar, and
sends it as a bearer token to its own origin. It acts as the order's
customer for that order alone: `GET /v1/me/orders/{id}`, `pay`, `confirm`,
`GET /v1/config`, `GET /v1/me/config` (the order's merchant's payment setup
and name) and, when offered, `GET /v1/me/payment-methods`. It opens
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

## Embedded checkout

`checkout.ui_mode: embedded` makes a checkout the merchant's own page frames,
like Stripe's embedded Checkout (`hosted` is the default). The merchant first
registers the sites that may frame it, in its settings:

```yaml
settings:
  checkout:
    embed_origins: [https://shop.example, https://www.shop.example]
```

They are exact origins: https (http on loopback), no path or wildcard, at most
10. An embedded order without any is refused `400
checkout_embed_origins_required`. The order's page answers `frame-ancestors`
with those origins and no `X-Frame-Options` (it cannot list origins); a hosted
order's page is never framed. Withdrawing an origin stops its frames at once.

billing-ui frames the order's `checkout.url`:

```tsx
<EmbeddedCheckout url={order.checkout.url} onComplete={({ orderId }) => showThanks(orderId)} />
```

The iframe allows payment and passkeys. The page follows its height, and once
the order completes it posts `openrails.checkout.complete` (`order_id`,
`success_url`) to the registered origins only, never `*`; the component hears
only its own frame on the checkout host's origin. Without `onComplete` it goes
to `success_url`. Fulfil on `order.completed`, as always.

The registered https sites are also registered as payment-method domains on
the merchant's Stripe accounts (as the checkout host is): Stripe shows Apple
Pay and Google Pay in a frame only when the framing page's domain is
registered. Apple Pay there also needs the site itself to serve Stripe's
domain-association file at the same path.

### The customer's own order

A customer creates its own order with its embedded checkout in one call, as
billing-ui's `<BuyButton>` does on a registered origin:

```
POST /v1/me/orders  {"lines": [{"price_id": "…"}], "checkout": {"ui_mode": "embedded"}}
→ 201 the order, with checkout {url, expires_at, ui_mode: "embedded", success_url: null, …}
```

- Only the merchant's own person signed in asks for one: a user of the issuer
  the merchant declares, acting itself in a session or with a device key, its
  credential bound to no scope. Anyone else is `403 customer_session_required`.
- The checkout is the order's own, framed by the merchant's registered
  origins. It offers the customer's saved cards, takes no payment beside it
  and lasts as long as the order is open (24 hours).
- It returns nowhere: the request names no URL, and the order's checkout has
  no `success_url` or `cancel_url`. Once paid the frame says so and posts
  `openrails.checkout.complete` to the page; a redirect PSP or a bank's
  challenge comes back to the frame's own page on the checkout host.
- Asked again for the same lines, it answers the customer's open order (200)
  with a fresh URL, and the last one stops working; the order's expiry does not
  move. A reload, a reopened dialog or two clicks pay one order. A paid,
  canceled or expired order does not count; a product the customer already
  owns is `409 already_owned`, and one whose `required_entitlements` they do
  not hold is `403 entitlement_required`, when asking and when paying.
- Without registered origins it is `400 checkout_embed_origins_required`,
  without a checkout host `400 hosted_checkout_unavailable`; neither makes an
  order.
- On the checkout host the secret opens that order alone, as a merchant's
  checkout URL does.

## Save with the customer network

On a server running the customer network ([Customer
network](customer-network.md)), the page offers before the card to save the
purchase to the shopper's network account. It signs the shopper in at the
server's issuer with its own client (`openrails-checkout`: code, PKCE, DPoP,
`prompt=login`, the link challenge's nonce) and links the order's customer
with the checkout URL. At the top level the page goes to the sign-in and back
to `/c/sign-in`; framed, the sign-in opens in a popup, whose return hands the
code back to the frame. `GET /v1/config`'s `network` names the issuer and
client.

## The page

Merchant name, logo and support link come from the merchant's settings
(`settings.profile`): the logo and support link only when they are https. A
hosted checkout's page is never framed (`frame-ancestors 'none'`); an embedded
one only by its merchant's registered origins. It sends no referrer.
