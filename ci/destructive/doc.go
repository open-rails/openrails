//go:build e2e && integration

// Package destructive is the e2e suite for destructive-command admission on
// real PostgreSQL: concurrent last-unit admission, durable holds and clears,
// replay and batches, scope budgets, transaction boundaries and recovery.
// Build-tagged (e2e && integration); needs OPENRAILS_E2E_DSN.
package destructive
