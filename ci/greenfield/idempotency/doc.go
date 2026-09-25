// Package idempotency is the greenfield suite for durable request and webhook
// claims (#1099) on real PostgreSQL: claim races, replay, release, stale
// leases, expiry collection and cross-replica webhook dedupe. Scenarios are
// build-tagged (greenfield && integration) and need OPENRAILS_GREENFIELD_DSN.
package idempotency
