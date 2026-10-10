//go:build e2e && integration

// Package entitlements is the e2e suite for entitlement reads on real
// PostgreSQL: exact keys and byte-prefix keyspaces in one request. Build-tagged
// (e2e && integration); needs OPENRAILS_E2E_DSN.
package entitlements
