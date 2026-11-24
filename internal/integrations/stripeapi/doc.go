// Package stripeapi is the choke point for all outbound Stripe HTTP (there is
// no stripe-go dependency). Every call site takes its *http.Client from here:
// the transport pins Stripe-Version to APIVersion and, for a readonly client
// or a merchant whose write posture is readonly, fails every non-GET/HEAD
// request locally with ErrProviderReadOnly, so no path can write to Stripe
// around it.
//
// A PSP's key (sk_ or rk_) must match its declared environment (live keys
// only on live, test keys only on test; publishable keys alike) and belong to
// the declared account_id (GET /v1/account, posture.go); health is a
// read-only GET /v1/balance.
//
// The webhook endpoint at /v1/webhooks/stripe/{account_id} is registered,
// updated and re-enabled by the Stripe webhook reconcile, with APIVersion as
// its api_version and exactly the handled event types. With configuration in
// Vault the minted signing secret is written to the PSP's document; with a
// file the operator registers the endpoint once and declares its whsec_
// (consistency.stripe_webhook_endpoint names what to register). A version
// bump creates the successor first, stamps the predecessor
// openrails_superseded_at, keeps it enabled and verifies its secret for at
// most 7 days; retiring it is destructive and waits for the kill switch. A
// rotated signing secret verifies only within webhook_secret_overlap (default
// 24h, at most 168h); retire_webhook_overlap refuses the old one at once. A
// secret missing locally is unknown, never lost: no remote endpoint is deleted
// for it. Thin v1.* events (webhook_signing_secret_thin) are verified,
// hydrated through the pinned version and may only wake work the operation
// worker then qualifies; they never admit a charge.
//
// New products and prices stay in OpenRails: no Stripe Subscription, product
// or price is created or edited. product.* and price.* events queue a
// read-only observe of the object. Subscriptions Stripe bills keep their
// status mirrored; a held one ends only on canceled, expired or
// 404 resource_missing. A revoking dispute or refund queues a
// stripe_cancel_subscription intent. A card reissued under another brand
// needs re-consent (requires_reconsent; off-session charges fail with
// stored_credential_required).
//
// Tests use openrailstest/stripemock. Its package comment has the wiring.
package stripeapi
