//go:build e2e && integration

// Package subscriptions is the e2e membership-lifecycle suite: engine- and
// provider-owned subscriptions on Stripe and NMI through the runtime, its HTTP
// routes and the Client. Build-tagged (e2e && integration); needs OPENRAILS_E2E_DSN.
package subscriptions
