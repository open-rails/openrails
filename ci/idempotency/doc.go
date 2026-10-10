//go:build e2e && integration

// Package idempotency is the e2e suite for durable request and webhook claims
// on real PostgreSQL: races, replay, stale leases, expiry, webhook dedupe and
// pinned connections. Build-tagged (e2e && integration); needs OPENRAILS_E2E_DSN.
package idempotency
