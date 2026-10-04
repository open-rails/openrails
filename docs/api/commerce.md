# Commerce client

These operations execute through the same merchant-authenticated handlers in the
embedded and remote clients:

| Client method | HTTP operation | Required permission |
| --- | --- | --- |
| CreateCheckoutSession | POST /v1/merchant/checkout-sessions | merchant:checkout:create |
| GetCheckoutSession | GET /v1/merchant/checkout-sessions/{id}?customer_id=... | merchant:customer-settings:read |
| ConfirmCheckoutSession | POST /v1/merchant/checkout-sessions/{id}/confirm | merchant:checkout:create |
| ListCheckoutRailOptions | GET /v1/merchant/checkout-options?price_id=... | merchant:customer-settings:read |
| ResolveEffectiveTier | GET /v1/merchant/customers/{customer_id}/effective-tier?group=... | merchant:customer-settings:read |

Merchant automation supplies `CreateCheckoutSessionRequest.Customer`, whose ID is
the merchant-owned customer UUID. Verified email and username are host assertions
made under merchant checkout authority, as in embedded checkout. The permission
is owner-only by default; editing a customer profile does not grant it. Each
operation enforces any customer restriction on the credential. Read/confirmation
also enforce ownership of the addressed checkout session.

The client sends `IdempotencyKey` as `Idempotency-Key`; it is required for merchant
checkout creation. Identical retries recover the existing checkout. Use the
returned session ID unchanged for subsequent reads and confirmation. Provider
options report local readiness; they do not execute a payment or probe a gateway.
An effective-tier read returns null when the customer has no active tier.
A recurring card price (NMI, Stripe) is a quote until the customer accepts it.
A host relaying the customer's pay click sets `Confirm`: OpenRails saves an NMI
`PaymentToken` as the customer's card, accepts the price's terms and charges it
in that call. A declined card leaves no subscription and no saved card; a retry
with the same key replays the accepted enrollment. Without `Confirm` the
signed-in customer accepts at `POST /v1/me/checkout/{id}/confirm`. Stripe cards
are saved in the page first (`payment_method_id`); Solana subscriptions use the
price's on-chain plan.
A provider refusal is a coded 402/502 (see [errors](errors.md#payment-refusals));
the session is recorded as failed and a new session may be opened with another
instrument.

Checkout amounts use native currency units (micros for fiat) and decimal strings
in JSON, retaining `int64` in Go. Session `created_at`/`expires_at` are RFC3339
instants like every other wire timestamp ([errors and wire rules](errors.md)).
This is a pre-v1 contract; remaining whole-API money/list qualification is tracked
in #983/#1002 before the final freeze.

## Hosted checkout

A signed-in customer mints a session for one price; the session id then reads
and pays it with no other credential. OpenRails owns the session (id, expiry,
attempts) in `billing.hosted_checkout_sessions`; apps sharing a merchant and
database share it, so one of them can serve the payment page for all.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/me/checkout/sessions` | customer session | Mint: `{price_key \| price_id, success_url?}` → `201 {id, url?, expires_at}` |
| GET | `/v1/checkout-sessions/{id}` | the id | The session document (`billing.HostedCheckoutSession`) |
| POST | `/v1/checkout-sessions/{id}/pay` | the id | Pay (`billing.HostedCheckoutPayRequest` → `HostedCheckoutPayResult`) |
| POST | `/v1/merchant/hosted-checkout-sessions` | `merchant:checkout:create` | Mint for a customer server-side (`Client.CreateHostedCheckoutSession`) |

- The id is `ocs_` + 256 random bits, stored as its SHA-256 and never logged.
  Hand it only to that customer's browser; put it in a URL fragment, not a
  query. It is payable for 30 minutes and readable for 24 hours more, so a
  late provider return still learns its outcome.
- Config: `HTTP.Checkout` publishes the routes. `PageURL` is the shared payment
  page (the mint answers `url = PageURL#id`); `EmbedOrigins` are the sites
  allowed to frame the page this host serves. Both empty is the single-site
  case: the app renders `<Checkout source={client.checkoutSource(id)}>` itself.
- A PSP whose `card_entry` is `server` (driver `card`) takes the card in the
  pay body's `card` field, through the shared page as anywhere else.
- Paying runs the engine's checkout with one idempotency key per attempt, so a
  double click or a retry charges once. The attempt advances only after a
  terminal failure; a session has 10 attempts. A decline answers
  `200 {status: "failed", failure}`; a purchase the buyer cannot make (already a
  member) answers `status: "blocked"`.
- `success_url` must be on one of the minting app's `ReturnOrigins`. The
  payment host accepts its `EmbedOrigins` as return origins too.
- The buyer is checked on every read and pay through `Deps.CheckoutCustomer`
  (default with AuthKit: a banned or deleted user is refused with 403).
- The session records the minting app's origin from that app's configuration
  (the `success_url` origin, else `ReturnOrigins[0]`, else the origin of
  `PublicBillingBaseURL`) and the document carries it as `embed_origin` only
  when the serving host lists it in `EmbedOrigins` or it is `PageURL`'s own.
- The host serving the page wraps it with the adapter's `CheckoutFramePolicy`
  (`Content-Security-Policy: frame-ancestors 'self' <EmbedOrigins>`).
- Limits: per address as any checkout route, and per session id 120 reads and
  10 pays a minute.

Money is exact: `plan.unit_amount`, `line_items[].amount`, `tax` and
`due_today` are int64 decimal strings of `plan.currency`'s native unit, and
`plan.unit_decimals` is that currency's registered scale
(`billing.LookupCurrency`, the same table as `GET /v1/currencies`). The
canonical fixture is `testdata/wire/hosted_checkout_session.json`; billing-ui
decodes the same file and rejects a numeric amount or a missing
`unit_decimals` as an unavailable session.
