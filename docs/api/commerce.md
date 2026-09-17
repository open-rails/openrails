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
A provider refusal is a coded 402/502 (see [errors](errors.md#payment-refusals));
the session is recorded as failed and a new session may be opened with another
instrument.

Checkout amounts use native currency units (micros for fiat) and decimal strings
in JSON, retaining `int64` in Go. Session `created_at`/`expires_at` are RFC3339
instants like every other wire timestamp ([errors and wire rules](errors.md)).
This is a pre-v1 contract; remaining whole-API money/list qualification is tracked
in #983/#1002 before the final freeze.
