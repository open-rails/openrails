# Standalone console

This server-owned app composes `BillingAdmin`, AuthKit group administration and account security at `/admin` by default. Set `admin_console.disabled: true` (or `ADMIN_CONSOLE_DISABLED=true`) to serve only the APIs, without this app or its bootstrap endpoints. Staff sign in with the server's own accounts (the first owner by the setup link the server logs, `/setup`; the rest by invitation, `/invite`, from the Team page; with a passkey or an authenticator app); a merchant's own staff can instead use the admin screens in the merchant's site. See [the standalone guide](../../docs/standalone.md) and [billing-ui's staff administration](../billing-ui/README.md#staff-administration).

Build with `task admin-build` from the repository root; `scripts/check.sh browser` builds and lints it in CI.
