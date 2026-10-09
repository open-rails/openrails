//go:build e2e && integration

// Package entitlements is the e2e suite for entitlement reads on real
// PostgreSQL: exact checks and byte-prefix keyspaces answered from one
// request. Scenarios are build-tagged (e2e && integration) and need
// OPENRAILS_E2E_DSN.
package entitlements
