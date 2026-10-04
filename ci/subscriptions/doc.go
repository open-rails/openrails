//go:build e2e && integration

// Package subscriptions is the e2e membership-lifecycle contract suite:
// engine-owned and provider-owned subscriptions on Stripe and NMI, including
// OpenRails recovery of NMI provider-owned schedules, driven
// through the public embed runtime, its HTTP routes and the portable Client
// (embedded and remote) against deterministic provider journals. Scenarios are
// build-tagged (e2e && integration) and need OPENRAILS_E2E_DSN.
package subscriptions
