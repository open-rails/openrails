# Merchant admin console

### What it is

A React SPA (`web/admin`, Vite) served by the engine at `/admin/`, driving the
`/v1/merchant/*` API. It is the browser UI for the **merchant operator** — the
human running a merchant on an OpenRails deployment: customers, subscriptions,
payments, catalog, ops findings, team, API keys. It holds no state and no
privileges of its own; every action is a merchant-API call under the caller's
own permissions. Off by default.

### Turning it on and off

Two independent requirements, both needed (#740/#754):

1. **Assets in the binary.** The engine ships no frontend bytes — `go:embed`
   cannot cross module boundaries, so whoever builds the binary owns the embed
   and hands the engine an `fs.FS` rooted at `index.html`. `dist` is never
   committed; Node/pnpm is a build-time dependency only, and only for builds
   that opt in.
2. **Config.** `admin_console.enabled: true`.

| assets | `admin_console.enabled` | result |
|--------|-------------------------|--------|
| yes | true | console served at `/admin/` |
| yes | false | not mounted — `/admin/*` 404s |
| no | false (default) | silently absent — `/admin/*` 404s |
| no | true | **loud boot error** naming the build step |
| dist missing at a `go:embed` build | — | **loud compile error** (`pattern all:dist: no matching files found`) |

```yaml
admin_console:
  enabled: true
  # auth_base_url: /auth   # default: the standalone control-plane authhttp mount
  # api_base_url: /v1      # default standalone; embedded hosts typically /billing/v1
```

Env: `ADMIN_CONSOLE_ENABLED`, `ADMIN_CONSOLE_AUTH_BASE_URL`,
`ADMIN_CONSOLE_API_BASE_URL`. Turning it **off** is the default: leave
`enabled` unset (and/or build without assets — plain `go build ./...` links
zero frontend bytes and never needs Node).

**Standalone binary.** Assets live behind the `console_assets` build tag in the
binary-boundary package `cmd/openrails/consoleassets` (untagged builds compile
a `FS() = nil` stub):

```sh
task admin-build           # scripts/build-admin-console.sh -> cmd/openrails/consoleassets/dist (gitignored)
task build-console-binary  # admin-build + go build -tags console_assets ./cmd/openrails
```

The Dockerfile is multi-stage: a Node stage builds the SPA, the Go stage embeds
it with the tag — the published image always carries the console, still
config-gated at runtime.

**Embedded hosts.** The host repo owns a tiny embed package over a
**gitignored** dist its build pipeline produces:

```go
// internal/consoleassets/assets.go
package consoleassets

import "embed"

//go:embed all:dist
var FS embed.FS
```

Build the dist straight from the openrails module cache (no vendoring; the
script copies the read-only source to a temp dir before `pnpm install`):

```sh
"$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" internal/consoleassets/dist
```

Then hand the FS to the engine:

```go
sub, _ := fs.Sub(consoleassets.FS, "dist")
rt, err := embed.New(ctx, opts, embed.WithAdminConsole(sub))
```

`WithAdminConsole` feeds the standalone surface (`embedded.StandaloneServer`),
which mounts `/admin/` per the table above. The embedded mount surface
(`embedded.MountHandler` / `Runtime.Client()`) does NOT serve `/admin` — hosts
that mount billing under their own mux serve the console themselves by mounting
`adminconsole.Handler(cfg, assets)` (package `pkg/adminconsole`), gated on
`adminconsole.Present(assets)`, with `APIBaseURL` set to their billing mount
(e.g. `/billing/v1`) and `AuthBaseURL` to their AuthKit base.

Fail-loud behaviors, verified: enabled without assets refuses boot
(`admin_console.enabled is set but no console assets were provided: …`);
forgetting to build dist before a tagged/host `go:embed` build is a compile
error; mounting `adminconsole.Handler` with no build answers every request 503
naming the build step. Opt-out is doing nothing.

### Security posture

What the engine enforces (verified):

- The SPA itself — static assets and `GET /admin/config.json` — is served with
  **no authentication at the transport layer**. Anyone who can reach `/admin/`
  gets the app shell and the bootstrap document (base URLs + feature flags; no
  secrets, no data).
- All **data and actions** go through `/v1/merchant/*` with a Bearer token
  (AuthKit user session or merchant API key) and are enforced server-side by
  the merchant permission catalog (#567) plus RLS merchant scoping. The console
  has no client-side privilege of its own; a 403 renders as a
  "role lacks permission" toast.
- Core OpenRails imposes **no environment restriction** — `enabled: true`
  serves the console in any env.

Recommendation (not engine-enforced): treat exposing `/admin/` like exposing
any login page. If your deployment doesn't want the console reachable at all in
production, gate it at boot — e.g. the doujins host refuses to boot with
`admin_console.enabled` in a production-like env precisely because the SPA is
unauthenticated at the transport layer, making it dev-only by convention.
Standalone SaaS deployments that do serve it in production should front it with
their normal edge protections (TLS, rate limits — OpenRails' own rate limiting
covers the auth endpoints).

Embedded hosts that mount `/v1/merchant/*` for host principals only (no user
bearers) should keep the console disabled or wire a user authenticator (#739).

### Viewing it

Browse to `https://<your-openrails-host>/admin/` (bare `/admin` redirects). The
SPA bootstraps from `GET /admin/config.json`:
`{auth_base_url, api_base_url, nl_widgets_enabled, ask_enabled, catalog_copilot_enabled, catalog_drafting_enabled}`.

- `auth_base_url` — where AuthKit's authhttp surface lives. Standalone default
  `/auth` (same server). Embedded: the host's AuthKit base, possibly another
  origin (CORS is then the host's concern).
- `api_base_url` — the merchant API base: `/v1` standalone, typically
  `/billing/v1` embedded.

**Login** is a real human login against AuthKit: the SPA reads
`{auth_base_url}/capabilities` and offers password login
(`POST {auth_base_url}/password/login`) plus one button per login-capable OIDC
provider; the OIDC redirect returns tokens in a URL fragment (configure
AuthKit's `Frontend.OIDCReturnPath` to land inside `/admin/`). Tokens are held
in `sessionStorage`; a 401 triggers one refresh (`POST {auth_base_url}/token`),
then the login page. Who can log in and what they may do is the merchant team
roster + fixed roles (`owner`/`support`/`viewer`) — see the merchant guide.

Local UI dev: `cd web/admin && pnpm run dev` (Vite proxies `/v1`, `/auth`, and
`/admin/config.json` to `localhost:3053`).

### Using it

| Page | Route | What it does |
|------|-------|--------------|
| Dashboard | `/` | Per-merchant metrics widget grid (drag/resize, saved per merchant) + the Ask panel |
| Customers | `/customers` | Search → customer profile: subscriptions, payments, entitlements, payment methods; grant/revoke entitlement + product access; off-channel payment |
| Subscriptions | `/subscriptions` | Status filters incl. the past_due dunning view; cancel (typed confirmation), resume, NMI payment-method change |
| Payments | `/payments` | Filters, payment detail, rail-aware refund (disabled on rails without API refunds) |
| Catalog | `/catalog` | Products/prices, price detail + change wizard, activate/deactivate, drift view, catalog copilot panel |
| Ops | `/ops` | Findings queue (approve/ignore), repair alerts, worker health |
| Settings | `/settings` | Tabs: Merchant profile, Team, Alerts, Payment providers, API keys, Customer controls |

**Natural-language features** are fail-closed on the server's `llm:` config and
mirrored into `config.json` so the UI shows a pointed empty-state (naming the
knob) instead of a broken button:

| Feature | Gate (config / env) |
|---------|--------------------|
| NL widget generation (widgets are LLM-authored; sees only the metrics schema) | `llm.api_key` / `LLM_API_KEY` |
| Ask panel — free-form metrics Q&A (aggregate query results flow to the LLM provider, hence its own consent) | key AND `llm.ask_enabled` / `LLM_ASK_ENABLED` |
| Catalog copilot Q&A (read-only catalog/subscriber aggregates) | key AND `llm.catalog_copilot_enabled` / `LLM_CATALOG_COPILOT_ENABLED` |
| Copilot drafting (drafts price changes into the wizard; a human always confirms — the model never mutates) | copilot AND `llm.catalog_drafting_enabled` / `LLM_CATALOG_DRAFTING_ENABLED` |

`llm.provider` is `anthropic` (default) or `openai`; `llm.base_url` points
`openai` at any OpenAI-compatible backend (Groq, Ollama, vLLM). Everything else
on the dashboard works keyless.

Day-to-day workflows — catalog authoring, dunning, refund doctrine, team
management, API keys for agents — live in [the merchant guide](merchant-guide.md).
For programmatic access to the same metrics the console uses, see
[metrics-for-llms.md](metrics-for-llms.md).
