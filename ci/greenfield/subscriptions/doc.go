// Package subscriptions is the greenfield membership-lifecycle contract suite:
// engine-owned and provider-owned subscriptions on Stripe and NMI, including
// OpenRails recovery of NMI provider-owned schedules, driven
// through the public embed runtime, its HTTP routes and the portable Client
// (embedded and remote) against deterministic provider journals. Scenarios are
// build-tagged (greenfield && integration) and need OPENRAILS_GREENFIELD_DSN.
package subscriptions
