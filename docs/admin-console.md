# Merchant admin console (#740)

A React SPA (web/admin) served by the openrails binary at `/admin/`, driving the
existing `/v1/merchant/*` API. Off by default.

## Enabling

```yaml
admin_console:
  enabled: true
  # auth_base_url: /auth      # default: the standalone control-plane authhttp mount
  # api_base_url: /v1         # default standalone; embedded hosts use /billing/v1
```

Env: `ADMIN_CONSOLE_ENABLED`, `ADMIN_CONSOLE_AUTH_BASE_URL`, `ADMIN_CONSOLE_API_BASE_URL`.

The SPA bootstraps from `GET /admin/config.json` (`{auth_base_url, api_base_url}`).

## Auth

Login is a real human login against AuthKit's authhttp surface at `auth_base_url`:

- **Standalone** — the control plane mounts authhttp at `/auth` on the same server.
  Password login works out of the box (`POST /auth/password/login`). OIDC providers are
  not configured by standalone openrails config, so the login page shows password-only.
- **Embedded** — set `auth_base_url` to the HOST's AuthKit base (may be another origin;
  CORS is the host's concern). The console reads `GET {auth_base_url}/capabilities` and
  shows an OIDC button per login-capable provider; the redirect flow returns tokens in a
  URL fragment (AuthKit's browser-OIDC contract — configure the AuthKit
  `Frontend.OIDCReturnPath` to land inside `/admin/`).

The bearer + refresh token are held by the SPA (localStorage); 401 → refresh once →
login page. Action permissions are enforced by the API's merchant permission catalog
(#567); the console has no client-side RBAC copy — a 403 renders as a "role lacks
permission" toast. Embedded hosts that mount `/v1/merchant/*` for host principals only
(no user bearers) should keep the console disabled or wire a user authenticator (#739).

## Rebuilding the SPA

`web/dist` is the **committed** Vite build output — openrails is imported as a Go module
by embedded hosts and `go:embed` only ships committed files. After changing `web/admin`:

```sh
task admin-build   # npm ci + vite build -> web/dist
git add web/dist
```

`web/admin` is a separate marker Go module so its `node_modules` never enters
`go build ./...`. If `web/dist` holds only the placeholder index, the console serves
503 with a "run task admin-build" message (and logs at boot).

Local UI dev: `cd web/admin && npm run dev` (proxies `/v1`, `/auth`,
`/admin/config.json` to `localhost:3053`).

## Pages

Customers (search → profile: subscriptions/payments/entitlements/payment methods,
grant/revoke entitlement + product access, off-channel payment), Subscriptions (status
filters incl. the past_due dunning view, cancel with typed confirmation, resume,
NMI payment-method change), Payments (filters, detail, rail-aware refund — disabled on
rails without API refunds), Catalog (products/prices CRUD + activate/deactivate,
manifest publish with plan preview, drift view + refresh), Ops (findings queue with
approve/ignore, repair alerts, worker health), Settings (merchant profile, payment
providers, credit limit, trust tier). Dashboard is a placeholder until #733/#741.
