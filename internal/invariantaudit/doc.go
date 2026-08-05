// Package invariantaudit executes the docs/invariants.md audit bundle as a test.
//
// The register's whole premise is that "an invariant that nothing can
// mechanically check is a wish". This package is where the DB-facing half of
// that register is actually run — and it runs as the unprivileged
// `openrails_app` role with RLS enforcing, because a harness that cannot
// reproduce the production role cannot catch the class of defect that or#824 /
// or#860 / or#861 exposed: a GUC-less read of a policied table returns ZERO
// ROWS AND NO ERROR, so guards built on such reads never fire and their tests
// still pass.
//
// Everything here is read-only or self-cleaning. Build tag: integration.
package invariantaudit
