# Checkout

A browser buys only through a **checkout session** (`ocs_`): the signed-in
customer or the merchant's server mints one, and the payment page reads and pays
it by its id. A **checkout attempt** (`chk_`) is one charge of one price on one
PSP. Each pay on a session creates one; merchant automation creates them
directly.

## Merchant client

These operations run through the same merchant-authenticated handlers in the
embedded and remote clients:

| Client method | HTTP operation | Required permission |
| --- | --- | --- |
| CreateCheckoutSession | POST /v1/merchant/checkout-sessions | merchant:checkout:create |
| CreateCheckoutAttempt | POST /v1/merchant/checkout-attempts | merchant:checkout:create |
| GetCheckoutAttempt | GET /v1/merchant/checkout-attempts/{id} | merchant:customer-settings:read |
| ConfirmCheckoutAttempt | POST /v1/merchant/checkout-attempts/{id}/confirm | merchant:checkout:create |
| GetCheckoutConfig | GET /v1/merchant/checkout-config?price_id=…\|product_key=…&price_key=… | merchant:customer-settings:read |

`Customer.ID` is the merchant-owned customer id. Verified email and username are
host assertions made under merchant checkout authority.

`merchant:checkout:create` is machine-only (`openrails.MachinePermissions()`):
staff never start a purchase for someone else. Over HTTP OpenRails refuses it
to a user acting in person (`403 permission_required`) whatever the host's
roles grant, an owner's `merchant:*` included; an application, or a user's own
API key automating the account, may hold it. Editing a customer profile does
not grant it, and the attempt routes enforce ownership of the addressed
attempt.

`CreateCheckoutSession` hands a price to a customer's browser: the answer is the
session id and, when `Config.Checkout.PageURL` is set, the payment page URL.

`CreateCheckoutAttempt` charges now, on the customer's behalf: the host relays
the customer's pay click, and creating the attempt accepts the price's terms.
`IdempotencyKey` (sent as `Idempotency-Key`) is required; an identical retry
returns the same attempt. Pay with an NMI `payment_token` (saved as the
customer's card) or Solana (`token_symbol` and `flow`: `transfer_request` or
`transaction_request`). A saved method (`payment_method_id`) is the customer's
to spend: over HTTP it is refused with `403 customer_proof_required`, and the
merchant mints a checkout session the customer pays, signed in, instead. The
embedding host's own Go client, acting while its customer is signed in to it,
may still charge one. A Client
never carries a card number; cards are entered on a checkout session. The
answer's `status` is `succeeded`, `failed` (with `failure`), `processing` (read
it again) or `requires_action` with a `next_action`:

| `next_action.type` | The buyer |
| --- | --- |
| `redirect_to_url` | opens `url` (a Stripe or CCBill page) in the top window; it returns to `success_url` |
| `solana_pay` | scans or opens `url`, a `solana:` Solana Pay link |
| `solana_sign_transactions` | signs and sends `transactions` in order; confirm the attempt with each signature |

A card that needs 3-D Secure carries `operation` instead. A declined card leaves
no subscription and no saved card. A provider refusal is a coded 402/502 (see
[errors](errors.md#payment-refusals)); the attempt is recorded as failed and a
new one may be made with another instrument. `GetCheckoutConfig` with a price
lists the `options` that can sell it, in routing order; options report local
readiness and never probe a gateway.

Amounts use native currency units (micros for fiat) as decimal strings in JSON
and `int64` in Go. `created_at`/`expires_at` are RFC3339 instants
([errors and wire rules](errors.md)).

## Checkout sessions

A signed-in customer mints a session for one price; the session id then reads
and pays it with a new card and no other credential. Saved cards are the
customer's own: a session shows them, and pays with one, only to its customer
admitted by the mount's `Auth`. Anyone else presenting a credential reads the
session as missing (`404 checkout_session_not_found`). OpenRails owns the session (id, expiry,
attempts) in `billing.checkout_sessions`; apps sharing a merchant and
database share it, so one of them can serve the payment page for all.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/me/checkout-sessions` | customer session | Mint: `{price_id \| product_key + price_key, auto_renew?, success_url?}` → `201 {id, url, expires_at}` |
| GET | `/v1/checkout-sessions/{id}` | the id (saved cards: and its customer) | The session document |
| POST | `/v1/checkout-sessions/{id}/pay` | the id (a saved card: and its customer) | Pay: `{option_id, payment_method_id?, payment_token?, card?, token_symbol?, …}` → `{status, next_action, operation, failure, …}` |
| GET | `/v1/me/checkout-sessions/{id}` | its customer, on a customer surface | The session document with the customer's saved cards |
| POST | `/v1/me/checkout-sessions/{id}/pay` | its customer in person, on a customer surface | Pay, as above; a saved card needs no other proof |
| GET, POST | `/v1/checkout-attempts/{id}/solana-pay` | the attempt id | The Solana Pay transaction request behind a merchant attempt's `solana_pay` link (`flow: transaction_request`) |
| POST | `/v1/merchant/checkout-sessions` | `merchant:checkout:create` | Mint for a customer server-side (`Client.CreateCheckoutSession`) |

- A host whose customer surface defines who the customer is (its own
  `CustomerRoutes` with an `Auth` that maps the signed-in user) has the
  customer pay there: the surface serves `{prefix}/checkout-sessions/{id}` and
  `{prefix}/checkout-sessions/{id}/pay`, and its `Auth` is the proof a saved
  card needs. A session of another merchant than the surface's, or of another
  customer, is `404 checkout_session_not_found`.
- The id is `ocs_` + 256 random bits, stored as its SHA-256 and never logged.
  Hand it only to that customer's browser; put it in a URL fragment, not a
  query. It is payable for 30 minutes and readable for 24 hours more, so a
  late provider return still learns its outcome.
- Config: `Routes.Storefront` publishes the routes. `Config.Checkout.PageURL` is the shared payment
  page (the mint answers `url = PageURL#id`); `EmbedOrigins` are the sites
  allowed to frame the page this host serves. Both empty is the single-site
  case: the app renders `<Checkout source={client.checkoutSource(id)}>` itself.
- Each option names its `psp_id`, `rail` and `driver`. A PSP whose
  `card_entry` is `server` (driver `card`) takes the card in the pay body's
  `card` field. Stripe (driver `stripe_elements`) pays a card saved in the page
  (`payment_method_id`), with 3-D Secure in the page through `operation`. A
  redirect (Stripe or CCBill hosted pages, one-off only) or a Solana Pay link
  arrives as `next_action`.
- Paying runs the engine's checkout with one idempotency key per attempt, so a
  double click or a retry charges once. The attempt advances only after a
  terminal failure; a session has 10 attempts. A decline answers
  `200 {status: "failed", failure}`; a purchase the buyer cannot make (already a
  member) answers `status: "blocked"`.
- `success_url` must be on one of the minting app's `ReturnOrigins`. The
  payment host accepts its `EmbedOrigins` as return origins too.
- The buyer is checked on every read and pay: a customer the merchant declared
  blocked (`client.EnsureCustomers` with `Blocked`) is refused with
  `403 customer_blocked`, even on a session minted before the block.
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
canonical fixture is `testdata/wire/checkout_session.json`; billing-ui
decodes the same file and rejects a numeric amount or a missing
`unit_decimals` as an unavailable session.
