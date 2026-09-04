# OpenRails #802 invoice administration

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/cozy/.worktrees/tracker/audit-remediation-status/openrails/802.md
Branch: fix/invoice-admin
Base: ba3b1f9ad447b283e1743b6bf9f2c81783a4e24e (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/invoice-admin

Contract:
- Merchant list/detail/filter and paginated collection history over existing invoices.
- Read/update/collect invoice permissions; profile editing uses customer-settings permissions.
- Existing void, uncollectible, off-channel remittance, and durable keyed collection retry only. Unknown/in-flight outcomes cannot be blindly unparked or retried.
- Issuance stays with the existing billing-cycle engine; profile changes affect only future snapshots.
- Dedicated invoice pages/query/API modules. Invoice profile mounts on customer detail, coordinated with the credit-support owner.

No new invoice engine, tax calculator, PDF/CSV generator, or payment rail.

Implemented: merchant-scoped invoice reads/filters/history; read/update/collect permissions and server-projected action eligibility; existing ledger-aware void/uncollectible/remittance and keyed retry; customer invoice-profile reads/editing; invoice list/detail/navigation and profile console. Issued facts remain immutable. Terms exceeding the supported due-date duration are rejected before overflow.

Shared exact unit helpers imported from credit-support commits 34d646d5c and e9084e71c. Invoice values were traced through AccrueOwed, FinalizeInvoice, manual PayOwed, and NativeToRailMinor at collection; JPY4 is verified by HTTP/PG acceptance and the fake-charger boundary, independently of catalog/payment micros.

Local validation passed: invoice/collection/HTTP integration race suites, affected unit race suites, 108 console tests, production admin build, targeted frontend lint, sqlc generation/vet, and query audit. The new status-filter index is migration 0019; migration lint passes with the repository's existing transactional DDL contention bounds. Shared refund SQL lint is resolved by integrating #339 before final CI. Route golden updated for these nine endpoints; regenerate after combining concurrent credit routes. Root owns merge and tracker closure, including rebasing onto shared SQL repair #339 before final CI.
