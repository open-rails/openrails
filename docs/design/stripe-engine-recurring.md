# Stripe payment execution for engine agreements

Stripe-native subscriptions and invoice collection retain their existing adapters.
Engine initial/renewal charges create direct PaymentIntents with no Stripe
Subscription, Product, Price or Invoice prerequisite. The engine's accepted ledger
operation owns the amount, currency, customer, card, account and period.

The unique durable submission-fence winner may create/confirm a PaymentIntent.
After possible dispatch, all financial recovery is read-only: GET the retained PI,
or paginate that customer's PaymentIntents to find the exact operation metadata.
An absent response/list match never authorizes another create, including after
Stripe's idempotency retention expires. Successful PI state is insufficient: a
separate captured Charge read must match the PI, customer, actual card, amount and
currency. Already refunded/disputed charges cannot confer fresh paid access.

Authentication is an unresolved original payment, not a decline. A failed PI is
canceled and read back canceled before terminal refusal releases the operation.
This prevents a previously exposed client secret from paying an abandoned attempt
while a new attempt executes. Uncertain cancellations remain owned and recoverable.

## Browser API

These routes use the existing `/v1/me` interactive customer-session authentication.
Merchant/API-key and delegated-spender credentials cannot acquire setup/payment
secrets. Every response uses `Cache-Control: no-store`. Secrets are fetched from
Stripe for the owned resource and never stored in checkout state or payment evidence.

1. Display explicit consent to save a card for future agreed payments, then POST
   `/v1/me/payment-methods/stripe-setup` with `{"psp_id":"<uuid>","consent":true}`
   and a stable `Idempotency-Key` header. The response carries an existing checkout
   session `id`, `setup_intent_id` and an ephemeral `client_secret`. Repeating the
   key preserves the accepted account/customer; changing the account conflicts.
2. Use Stripe.js Elements with the merchant's publishable key to confirm that
   SetupIntent. Card data goes directly to Stripe. GET
   `/v1/me/payment-methods/stripe-setup/:id` recovers the same setup; POST its
   `/confirm` endpoint reads Stripe truth and returns the local `payment_method_id`.
   Confirmation accepts no browser-supplied Stripe/customer/card identity. Setup
   grants no membership, payment or entitlement.
3. POST `/v1/me/checkout` with subscription mode, local price, selected PSP and the
   returned local saved-method ID. Display its immutable membership quote, including
   amount, frequency and recurring permission. POST `/v1/me/checkout/:id/confirm`
   with `{"payment":{"rail":"stripe"}}` after the customer accepts those terms.
4. If the original operation needs authentication, GET
   `/v1/me/payment-operations/:id/authentication`. Pass its `client_secret` and
   frozen `provider_payment_method_id` to
   `stripe.confirmCardPayment(client_secret, {payment_method: provider_payment_method_id})`.
   Card replacement is a separately authorized saved-method/agreement action.
   POST `/v1/me/payment-operations/:id/authentication/confirm` then runs the existing
   operation verifier; browser success assertions cannot settle money locally.
5. Read the original checkout for the completed subscription. Renewals use the same
   authentication recovery resource with their original operation IDs.

Setup rows are `checkout_sessions.mode=payment_method`, rail `stripe`. The only
stored binding is customer/account/session plus the original SetupIntent reference;
completion attaches the exact successful setup's card under a customer/session lock.
The first paid engine membership captures its PaymentIntent ID as the recurring
anchor. Finite/trial/unsupported terms remain refused by shared engine qualification.

## Verification

`TestStripeEngineSignupSelfHTTP` drives the actual standalone HTTP routes with an
AuthKit customer token, merchant Client catalog creation, fake guarded Stripe,
saved setup, immutable quote, first-payment authentication and local completion.
`TestStripeInitialMembershipOwnedWorkflow` covers receipt mismatch, lost reply,
same-payment authentication, cancellation before decline, replay and admission hold.
Transport race tests cover exact binding, read-only mode, pagination and concurrent
GET recovery. These deterministic tests do not replace Stripe sandbox card entry,
issuer authentication and provider-account qualification; activation stays gated.

Primary contracts: [SetupIntent usage and consent](https://docs.stripe.com/payments/setup-intents),
[PaymentIntent creation](https://docs.stripe.com/api/payment_intents/create),
[idempotency retention](https://docs.stripe.com/api/idempotent_requests).
