// Package stripemock is a stateful, deterministic Stripe for tests and
// disposable sandbox stacks. It serves the Stripe API surface OpenRails uses:
//
//   - customers (create, read, metadata search), SetupIntents and payment
//     methods,
//   - PaymentIntents with their charges (create, read, list, cancel),
//     charges, refunds and an empty dispute list,
//   - subscriptions Stripe bills, each with its latest invoice, and legacy
//     prices,
//   - hosted Checkout Sessions (create, read, expire).
//
// Serve it over loopback (New, then point Config.ProviderSandbox.StripeAPIURL
// at URL) or in process (NewUnstarted, then hand it over as
// Deps.StripeTransport). Either way OpenRails' Stripe client, its read-only
// guard and pinned API version stand in front of it. Time comes from
// Options.Clock.
//
// Stripe's webhooks go where SendWebhooksTo says (the host's mounted
// /v1/webhooks/stripe/{account_id}), signed with the endpoint's secret:
// CompleteCheckoutSession pays a session and sends checkout.session.completed,
// SendEvent sends any event, Redeliver sends one again.
//
// Tests seed state (CompleteSetup, AddSubscription, SetLegacyPrice), play
// Stripe's own actions (Authenticate, RenewSubscription, DraftRenewal,
// CollectDraft, CancelSubscription, PortalPriceChange, ReissueCard, Refund),
// inspect what it saw (Ledger, Attempts, Mutations, Submitted, Unexpected)
// and inject failures (SetDecline, LoseSubmissions, the …Unavailable
// switches, DelayIntentVisibility, Intercept, Hold).
//
// Only tests and sandbox commands may import it; guard_test.go enforces this.
//
// # Known differences from real Stripe
//
//   - Authentication is not checked; any secret key works. GET /v1/account
//     answers acct_<name> for a key sk_test_acct_<name>, else acct_e2e.
//   - Cards: CompleteSetup's Card decides every charge of its payment method:
//     Decline "" succeeds at once, "auth" requires_action until Authenticate,
//     any other value is that decline_code (402). There are no 3DS redirects,
//     radar rules or captures: a PaymentIntent is charged on create.
//   - Idempotency: a key replays its first executed answer for 24 hours on
//     the mock clock; validation failures (400, 404) are not retained.
//   - Lists: charges and refunds honor created[gte]/[lte], customer, limit and
//     starting_after; PaymentIntents filter by customer only; disputes are
//     always empty.
//   - Checkout: amount_total sums inline price_data lines (a price id is a
//     legacy price). Only a payment-mode session completes, paid by a visa
//     ending 4242; no customer is created for a guest. Stripe sends only
//     checkout.session.completed for it, never payment_intent or charge
//     events.
//   - Subscriptions bill only when a test plays Stripe's renewal; periods are
//     30 days; currency is usd.
package stripemock
