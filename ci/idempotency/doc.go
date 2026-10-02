// Package idempotency is the e2e suite for durable request and webhook
// claims (#1099) on real PostgreSQL: claim races, replay, release, stale
// leases, expiry collection, cross-replica webhook dedupe, and the
// connection discipline claims rely on (pin reuse, bounded pool waits). Scenarios are
// build-tagged (e2e && integration) and need OPENRAILS_E2E_DSN.
package idempotency
