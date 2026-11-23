# Integrate with OpenRails SaaS

Use [OpenRails SaaS](https://openrails.dev/) when you want OpenRails to run the
billing API, merchant console and checkout. Your application calls the hosted
API and your merchant connects its payment provider. With a [standalone
server](standalone.md), you deploy OpenRails, configure its auth and public
origins, and provision the merchant on your own instance. With SaaS, create and
configure the merchant in the hosted console.

## Set up your merchant

1. [Sign up](https://openrails.dev/signup), verify your contact and create a
   merchant from **New merchant**. Select it in the merchant console.
2. In the console, create a product and price and configure a [payment provider
   (PSP)](rails/README.md). Start with a [supported non-live
   environment](psp-environments.md). Copy the provider's webhook URL from
   **Integration** into that provider's dashboard and configure its signing
   credentials in the merchant console.
3. Copy **API URL** from **Integration**. In **Settings → API keys**, create a
   key for your backend and save the secret when it is shown. Choose **Owner**
   for this walkthrough. Checkout creation requires `merchant:orders:manage`
   and the event reader requires `merchant:events:read`. Keep the key on your
   server and send it as `Authorization: Bearer <key>`.

Your API key is bound to that merchant. Verify the binding with
`GET /v1/admin/access` and use its `scope_id` as the merchant UUID in the
checkout-session path below. Use `GET /v1/catalog/prices` to obtain a price ID
for your storefront. The [HTTP route reference](api/routes.md) lists the shared
billing routes and their permission requirements.

## Run a first checkout

On your backend, select the price from your catalog and create a session. Set
`OPENRAILS_API_URL`, `OPENRAILS_API_KEY`, `OPENRAILS_MERCHANT_ID` and `PRICE_ID`
to the API URL, key, merchant UUID and price ID from setup. This example uses
`curl` and `jq`.

```sh
curl --fail-with-body --silent --show-error \
  "$OPENRAILS_API_URL/api/v1/merchants/$OPENRAILS_MERCHANT_ID/checkout-sessions" \
  -H "Authorization: Bearer $OPENRAILS_API_KEY" \
  -H 'Content-Type: application/json' \
  --data "$(jq -n --arg price_id "$PRICE_ID" '{lines: [{price_id: $price_id}]}')"
```

The `201` response contains `id`, `url` and `expires_at`. Redirect the buyer to
`url` unchanged, including its secret fragment. The session is valid for one
hour before a buyer claims it. OpenRails signs in the buyer and creates their
order after they confirm the checkout. The hosted page handles payment, provider
redirects and payment status.

Poll `GET /v1/app/events?type=order.completed` from your backend using the same
API key. Use the event's `subject.id` to fetch the order from
`GET /v1/admin/orders/{id}` if you need its full details. Apply fulfillment
idempotently by event ID and persist `next_cursor` with your changes. Use it as
`after` on the next request, keeping the same event filter. In your first
non-live test, verify both the completed order and the event. Fulfill from the
event even if the buyer closes the tab. The webhook URLs shown in **Integration**
are where payment providers send callbacks to OpenRails. [Event log](events.md#reading-the-event-log)

## Add billing to your own signed-in pages

If your app already signs in its customers, register your backend in your
merchant's **Integration → Customer tokens**. Supply your HTTPS issuer and a
JWKS URL or a public PEM key and its key ID. Keep the private key in your
backend. For each authenticated customer, have your backend sign an RFC 7523
assertion with the registered issuer (`iss`), a stable customer UUID (`sub`), the
console's token endpoint (`aud`), `iat`, an `exp` at most two minutes ahead, and a
random, single-use `jti` of 16–128 characters. Include the signing key's `kid` in
the header.

Send the following fields in an `application/x-www-form-urlencoded` `POST` to
the console's token endpoint.

```text
grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer
assertion=<signed JWT>
scope=openrails:self
```

The resulting access token is for the billing API resource (the SaaS API URL by
default). It authorizes calls such as `GET /v1/me` as that buyer, within your
merchant. Use the response's token type, refresh according to `expires_in` by
signing a new assertion, and keep browser tokens in memory.
[`@openrails/billing-ui`](../sdk/billing-ui/README.md) provides the client,
assertion redemption and account billing UI. [Browser integration](frontend.md#authentication)

If your own authorization server issues RFC 9068 tokens (`typ: at+jwt`), you
can register its issuer and signing keys in the same place. Request a token for
the SaaS billing API resource (`aud`) with `openrails:self` and the buyer's UUID
as `sub`. For a DPoP-bound token, send `Authorization: DPoP <token>` and a proof
for each request. Include verified contact claims if OpenRails should use them
for receipts. [Trusted issuers](auth.md#trusted-issuers)

If you want OpenRails to sign in customers on your site, register an OAuth
client under **Settings → Sign in with openrails.dev**. Register your callback
URI and use the issuer shown there for OpenID Connect discovery and authorization
code with PKCE. Use a public client for a browser-only app, or keep a confidential
client's secret or private key on your server. Request `openid` for sign-in and
exchange the code received by your callback to establish your app's session.
For purchases, send buyers through hosted checkout so OpenRails can verify the
account and bind it to the merchant's customer.
