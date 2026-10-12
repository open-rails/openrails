# Standalone console

This server-owned app composes `BillingAdmin`, AuthKit group administration and account security at `/admin`. App sign-in is configured per merchant; local accounts default to invitation-only enrollment. See [Billing administration](../../../docs/billing-admin.md).

Build with `task admin-build` from the repository root. `pnpm test:e2e` runs the compiled app against the real standalone server, AuthKit and disposable PostgreSQL 18. The separate test module keeps the embedded billing-ui fixture independent of the server module.
