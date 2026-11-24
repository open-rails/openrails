# Standalone console

This server-owned app composes `BillingAdmin`, AuthKit group administration and account security at `/admin` by default. Set `admin_console.disabled: true` (or `ADMIN_CONSOLE_DISABLED=true`) to serve only the APIs, without this app or its bootstrap endpoints. App sign-in is configured per merchant; local accounts default to invitation-only enrollment. See [the standalone guide](../../docs/standalone.md) and [billing-ui's staff administration](../billing-ui/README.md#staff-administration).

Build with `task admin-build` from the repository root; `scripts/check.sh browser` builds and lints it in CI.
