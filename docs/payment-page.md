# Payment page

An order has a pay URL: a page on an origin of its own where the customer pays
that one order, `/pay/<order>#<secret>`. The merchant's backend creates the
order with `pay_url`, sends the customer to `order.pay_url`, and learns of
payment from the `order.completed` host event. It works like Stripe
Checkout: a short-lived URL, the provider's own card fields, 3-D Secure in the
page, then back to the merchant. Standalone and embedded hosts serve the same
payment page.

## Turning it on

The standalone server serves it at `payment_origin`, or
`server.Config.PaymentOrigin`:

```yaml
payment_origin: https://pay.example.com
```

The payment origin has no path; the server answers it by `Host`, so point its
DNS and TLS at the same server. It must differ from
`public_billing_base_url`, AuthKit's issuer and the frontend's origin: the page
runs the PSPs' scripts and never shares an origin with the API or the console.
Boot refuses otherwise. Without it the server issues no pay URL, and an order
asking for one is refused `400 pay_url_unavailable`.

The page is `web/pay`, built from billing-ui and embedded in the binary
(`task payment-page-build`; the release image carries it). A host building its
own binary on `server` builds it with `scripts/build-payment-page.sh <dir>` and
passes it as `server.Deps.PaymentPageAssets`. A host that mounts `Routes()` on
its own router sends the payment origin's requests to `PaymentPage()`.

## Embedded applications

Before starting an embedded engine or serving HTTP, configure its payment page
and serve it ahead of the application's routes. With Gin:

```go
origin, page, err := engine.PaymentPage("https://pay.example.com", assets)
if err != nil { return err }
pay, err := openrailsgin.PaymentOrigin(origin, page)
if err != nil { return err }
router.Use(pay) // before other middleware and routes
router.Use(gin.LoggerWithFormatter(openrailsgin.RedactLog(format)))
```

`openrailsgin.PaymentOrigin` sends a request whose `Host` is the payment origin
to the page alone, so no application, sign-in or admin route answers there; a
malformed `Host` is refused, and forwarded headers never select it.
`RedactLog` logs payment origin requests and `/pay/` paths without their query
string or error details; `PaymentRequest(c)` tells a host's own loggers and
panic recovery the same.

`assets` is an `fs.FS` rooted at the built `web/pay` page; build it from the
pinned module with `scripts/build-payment-page.sh`. The handler owns the
page's security headers, credential resolution, framing and rate limits. It
mounts no host AuthKit or admin routes. The origin must differ from the
engine's API, dashboard and declared merchant issuers, including equivalent
port/host spellings.

When two applications share a merchant's billing database and one serves the
payment origin, the other calls `engine.PaymentPage(origin, nil)` only to
issue pay URLs; its returned handler is nil. Each application still owns its
authentication. Its pages sell with billing-ui's `<BuyButton>`, which creates
the signed-in customer's order with its embedded pay URL (below); it needs no
route of its own. A publisher's inbox and notification receipt state remain
independent of payment.

## Creating one

`POST /v1/app/orders` (`Client.CreateOrder`) with `pay_url`:

```json
{
  "customer_id": "4d1c…",
  "lines": [{ "price_id": "premium:buy-usd:v1" }],
  "pay_url": {
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
  given no `expires_at` of its own expires with its pay URL, and
  `order.expired` follows.
- `saved_payment_methods` shows the customer's saved cards they allowed to be
  shown again. Set it only when your site signed the customer in.

The answer's `pay_url` is the URL; `pay_url_expires_at` is when it stops
working. Redirect the customer to it. The order stores its secret's hash. A
bounded creation receipt retains only that secret, in the same transaction,
until the pay URL's original expiry. An authenticated retry with the same
`Idempotency-Key` and request recovers the same URL while the order and its
pay URL remain open. The reply reads the current order: a completed, canceled
or expired order returns no URL, and a retry never extends either expiry.
Ordinary order reads always show `pay_url: null`.

Creation receipts use the existing expiry cleanup and are excluded from billing
archives. If a receipt has expired or is unavailable after a restore, no secret
is reconstructed from its hash; cancel an unpaid order and create another when
a new pay URL is needed. Create the replacement only after cancellation
succeeds; an unresolved payment must be settled or reconciled first.

## Paying

The URL carries a secret in its fragment, which browsers never send to a
server. The page keeps it for the tab, drops it from the address bar, and
sends it as a bearer token to its own origin. It acts as the order's
customer for that order alone: `GET /v1/me/orders/{id}`, `pay`, `confirm`,
`GET /v1/config`, `GET /v1/me/config` (the order's merchant's payment setup
and name) and, when offered, `GET /v1/me/payment-methods`. It opens
nothing else, and the API's own origin does not accept it. Past the
pay URL's expiry it answers `410 pay_url_expired`.

The customer pays with a new card in the PSP's own fields, charged and saved
in one request, with a saved card when offered, or with Apple Pay or Google
Pay (below). The page never takes a card number itself. A bank's challenge runs in the page. Reads and pays under one
URL are rate limited, a pay the server challenges asks the customer to solve
the deployment's captcha, and an order that has had ten cards declined takes
no more (`409 pay_url_attempts_exhausted`).

Fulfil on `order.completed`, never on the redirect to `success_url`.

## Apple Pay and Google Pay

A merchant with a Stripe PSP that has a `publishable_key` is offered Apple Pay
and Google Pay through Stripe's Express Checkout Element, where the customer's
browser and wallet support them. The wallet's card is charged as a new card is.

Stripe shows them only on a payment-method domain registered on the merchant's
account, so declaring a Stripe PSP registers the payment origin there: a
`stripe_payment_method_domain` provider intent, once per PSP and host. A PSP
declared before the server served the payment origin is registered when its
merchant next creates an order with a pay URL. A domain the account already holds is
not registered again (its Apple Pay is validated when inactive), and one the
merchant disabled in Stripe stays disabled. The payment origin serves Stripe's
Apple Pay domain-association file at
`/.well-known/apple-developer-merchantid-domain-association`.

NMI's wallets are not offered.

## Embedded payment page

`pay_url.embedded: true` makes a pay URL the merchant's own page frames, like
Stripe's embedded Checkout (otherwise the customer is sent to it). The
merchant first registers the sites that may frame it, in its settings:

```yaml
settings:
  payment_page:
    embed_origins: [https://shop.example, https://www.shop.example]
```

They are exact origins: https (http on loopback), no path or wildcard, at most
10. An embedded order without any is refused `400
embed_origins_required`. The order's page answers `frame-ancestors`
with those origins and no `X-Frame-Options` (it cannot list origins); a hosted
order's page is never framed. Withdrawing an origin stops its frames at once.

billing-ui frames the order's `pay_url`:

```tsx
<EmbeddedPayment url={order.pay_url} onComplete={({ orderId }) => showThanks(orderId)} />
```

The iframe allows payment and passkeys. The page follows its height, and once
the order completes it posts `openrails.payment.complete` (`order_id`,
`success_url`) to the registered origins only, never `*`; the component hears
only its own frame on the payment origin's origin. Without `onComplete` it goes
to `success_url`. Fulfil on `order.completed`, as always.

The registered https sites are also registered as payment-method domains on
the merchant's Stripe accounts (as the payment origin is): Stripe shows Apple
Pay and Google Pay in a frame only when the framing page's domain is
registered. Apple Pay there also needs the site itself to serve Stripe's
domain-association file at the same path.

### The customer's own order

A customer creates its own order with its embedded pay URL in one call, as
billing-ui's `<BuyButton>` does on a registered origin:

```
POST /v1/me/orders  {"lines": [{"price_id": "…"}], "pay_url": {"embedded": true}}
→ 201 the order, with pay_url, pay_url_expires_at, embedded: true, success_url: null, …
```

- Only the merchant's own person signed in asks for one: a user of the issuer
  the merchant declares, acting itself in a session or with a device key, its
  credential bound to no scope. Anyone else is `403 customer_session_required`.
- The pay URL is the order's own, framed by the merchant's registered
  origins. It offers the customer's saved cards, takes no payment beside it
  and lasts as long as the order is open (24 hours).
- It returns nowhere: the request names no URL, and the order has no
  `success_url` or `cancel_url`. Once paid the frame says so and posts
  `openrails.payment.complete` to the page; a redirect PSP or a bank's
  challenge comes back to the frame's own page on the payment origin.
- Asked again for the same lines, it answers the customer's open order (200)
  with a fresh URL, and the last one stops working; the order's expiry does not
  move. A reload, a reopened dialog or two clicks pay one order. A paid,
  canceled or expired order does not count; a product the customer already
  owns is `409 already_owned`, and one whose `required_entitlements` they do
  not hold is `403 entitlement_required`, when asking and when paying.
- Without registered origins it is `400 embed_origins_required`,
  without a payment origin `400 pay_url_unavailable`; neither makes an
  order.
- On the payment origin the secret opens that order alone, as a merchant's
  pay URL does.

## Save with the customer network

On a server running the customer network ([Customer
network](customer-network.md)), the page offers before the card to save the
payment to the shopper's network account. It signs the shopper in at the
server's issuer with its own client (`openrails-payment-page`: code, PKCE, DPoP,
`prompt=login`, the link challenge's nonce) and links the order's customer
with the pay URL. At the top level the page goes to the sign-in and back
to `/pay/sign-in`; framed, the sign-in opens in a popup, whose return hands the
code back to the frame. `GET /v1/config`'s `network` names the issuer and
client.

## The page

Merchant name, logo and support link come from the merchant's settings
(`settings.profile`): the logo and support link only when they are https. A
hosted pay URL's page is never framed (`frame-ancestors 'none'`); an embedded
one only by its merchant's registered origins. It sends no referrer.
