// Package invariantaudit exercises database invariants and explicit tenant SQL.
// It tests scoped access using both owner and separately provisioned runtime
// connections, with no RLS policies hiding missing predicates. Global platform
// discovery and merchant fan-out remain deliberate contracts.
// Build tag: integration. Fixtures live in test-owned databases.
package invariantaudit
