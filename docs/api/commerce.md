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

## Hosted checkout document

A host that sells through the `openrails-checkout` browser package serves that
package one session document (`GET .../checkout/sessions/{id}`) and accepts
its pay request. The Go shape is `openrails.HostedCheckoutSession` (with
`HostedCheckoutPayRequest`/`HostedCheckoutPayResult`); the canonical fixture
is `testdata/wire/hosted_checkout_session.json` and the package decodes the
same file. The host owns the session (id, expiry, attempts, Redis or SQL);
OpenRails owns the shape so every host renders the same checkout.

Money is exact: `plan.unit_amount`, `line_items[].amount`, `tax` and
`due_today` are int64 decimal strings of `plan.currency`'s native unit, and
`plan.unit_decimals` is that currency's registered scale. Build the plan with
`openrails.NewHostedCheckoutPlan(product, price)`, which stamps the scale from
the registry (`openrails.LookupCurrency`, the same table as
`GET /v1/currencies`) and refuses an unregistered currency; never hardcode a
scale. `ListCheckoutRailOptions` lists exactly the armed PSPs whose rail can
make this sale, each with its browser `driver` and `public_config`; copy them
into the session's `rails` and skip an option without a driver. The package (0.3.0 and later) rejects a
numeric amount or a missing `unit_decimals` as an unavailable session.
