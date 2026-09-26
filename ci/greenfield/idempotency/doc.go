// Package idempotency is the greenfield suite for durable request and webhook
// claims (#1099) on real PostgreSQL: claim races, replay, release, stale
// leases, expiry collection, cross-replica webhook dedupe, and the
// connection discipline claims rely on (pin reuse, bounded pool waits). Scenarios are
// build-tagged (greenfield && integration) and need OPENRAILS_GREENFIELD_DSN.
package idempotency
