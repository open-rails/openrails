// Package stripemock is a stateful, deterministic Stripe for tests and
// disposable sandbox stacks, serving the Stripe API surface OpenRails uses.
//
// Serve it over loopback (New, then point Config.ProviderSandbox.StripeAPIURL
// at URL) or in process (NewUnstarted as Deps.StripeTransport); either way
// OpenRails' Stripe client, read-only guard and pinned API version stand in
// front of it. Webhooks go, signed, where SendWebhooksTo (snapshot events)
// or SendThinEventsTo (thin events) says. Time comes from Options.Clock.
// Only tests and sandbox commands may import it.
//
// # Known differences from real Stripe
//
//   - Authentication is not checked. GET /v1/account answers acct_<name> for
//     a key [sr]k_(test|live)_acct_<name>, else acct_e2e; GET
//     /v1/balance's livemode is the key's mode.
//   - A payment method's Card.Decline decides every charge: "" succeeds,
//     "auth" requires_action until Authenticate, anything else is that
//     decline_code (402). No 3DS redirects, radar or captures: a
//     PaymentIntent is charged on create.
//   - An idempotency key replays its first executed answer for 24 hours on
//     the mock clock; validation failures (400, 404) are not retained.
//   - Charge and refund lists honor created[gte]/[lte], customer, limit and
//     starting_after; PaymentIntent lists filter by customer only; disputes
//     are what Dispute seeds.
//   - Products and prices are what AddProduct and AddPrice seed. Lists honor
//     active (prices list only active ones unless asked) and never show
//     inline prices; a by-id read does. Moving a subscription onto an
//     inactive price is refused.
//   - Checkout amount_total sums inline price_data lines and seeded prices.
//     Only a payment-mode session completes, paid by a visa
//     ending 4242; a guest gets no customer; the only event sent is
//     checkout.session.completed.
//   - A thin event's v2 event has no data, context or snapshot_event.
//   - Subscriptions bill only when a test plays Stripe's renewal; periods are
//     30 days; currency is usd.
package stripemock
