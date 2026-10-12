# Merchant admin console

### What it is

A React SPA (`web/admin`, Vite), the staff dashboard, driving the
`/v1/admin/*`, `/v1/catalog/*`, `/v1/merchant/*` and `/v1/metrics/*` APIs. It is the browser UI for the **merchant operator**, the
people running a merchant: customers, subscriptions, payments, catalog, ops
findings, settings. It holds no state and no privileges of its own; every
action is a staff-route call its permission admits the caller for.

It has four areas, one per staff route group: customer support
(`RouteGroups.Admin`), the catalog (`Catalog`), merchant config
(`MerchantConfig`: PSPs, findings, settings, notifications) and business metrics
(`Metrics`: the dashboard's charts, the Ops gauges). On load it asks
`GET /v1/admin/access`, which any signed-in staff member may call, and shows an
area only when its group is on and the staff member holds its permission.
Customer support is read-only without `merchant:customer:manage` (or
`root:customer:manage`), and the dashboard's layout is editable only to
someone holding both the metrics and the merchant-config permission. Someone
holding none of them sees only that they have no access. Each catalog edit sends the
revision it loaded, so a product, price, meter or rate another person changed
meanwhile is refused and reloaded, and applying a catalog document lists what
an edit kept.

Settings also has a **Roles** page, AuthKit's rather than an area's: the
console asks the AuthKit API at `auth_base_url` for the caller's permissions
in the root group and in the merchant's own scope, which the access read names
(`scope_id`, the AuthKit group whose id is the merchant's). It shows each to
staff holding that persona's `roles:read` (`server.MerchantRolesRead` on the
standalone server): the root group's roles and who holds them as all-merchants
staff, then the merchant's declared roles and its custom ones with what each
grants. A merchant with no group of its own shows the root group alone. With
`roles:manage` they create, edit and delete custom roles from the persona's permissions, the
`/v1/app` ones listed with the routes they open; a permission they do not hold
cannot be granted. A role is given to a teammate (with `members:manage`) or to
a new API key, its secret shown once (with `credentials:manage`). See
[roles](auth.md#roles).

### Turning it on and off

Off is the default: nothing mounts the console, and no console route exists.
On is one switch, where the HTTP surface is chosen:

- **Embedded:** `AdminConsole: true` in the `openrails.Routes` given to the
  adapter's `Mount` (or the engine's `Routes`). The console is served at the
  prefix's `/admin`; `AdminConsoleAuthBase` says where your AuthKit's JSON
  API is.
- **Standalone server:** `admin_console.enabled: true` in `config.yaml`
  (`ADMIN_CONSOLE_ENABLED`), with `admin_console.path`, and the staff route
  groups it drives turned on in `route_groups`.

```go
err := openrailsgin.Mount(r, client, openrails.Routes{
    Auth:                 ak.Authenticator(), // staff hold root:customer:read and root:customer:manage in its root group
    Prefix:               "/billing",         // the API at /billing/v1/*, the console at /billing/admin
    RouteGroups:          openrails.RouteGroups{Admin: true},
    AdminConsole:         true,
    AdminConsoleAuthBase: ak.APIBase(), // where AuthKit's JSON API is mounted; empty is /api/v1
})
```

```yaml
admin_console:
  enabled: true
  # path: /admin             # default; e.g. /billing/admin when /admin is taken
```

Env: `ADMIN_CONSOLE_ENABLED`, `ADMIN_CONSOLE_PATH`.

Mounting the console fails loudly, before anything registers, when:

- no staff route group is on: the console has no API to drive;
- there is no console build (see below);
- embedded, `Routes.AdminConsoleAuthBase` is not an absolute path (no scheme
  or host, no trailing slash);
- standalone, `admin_console.path` is invalid or overlaps an OpenRails route.

**Path.** Embedded, `Routes.Prefix` + `/admin`. Standalone, an absolute URL
path without a trailing slash, made of letters, digits and `. _ ~ -` segments;
`/admin` by default. One build serves any path:
its URLs are relative to the `<base href="/admin/">` in `index.html`, which the
server rewrites to the path, and the SPA derives its routes and `config.json`
URL from `document.baseURI`.

**Which merchant.** Every admin request acts on the merchant its credential
is bound to; nothing in the request names one
([merchant binding](merchant-binding.md)). Embedded, the console serves
the single merchant its engine declares with an `Issuer` (`config.json`'s
`merchant`); a console over several merchants refuses to mount. Standalone
with a trusted issuer (`admin_console.issuer`), the issuer's tokens are bound
to the merchant that issuer belongs to. Standalone with local sign-in, the
console exchanges the person's session for a token bound to the chosen
merchant (RFC 8693, `group_id` = the merchant id, through the server's
`openrails-console` client, `config.json`'s `exchange`), keeps it in memory and
exchanges again before it expires; the exchange refuses a merchant where the
person holds no role. OpenRails lists no user's merchants: staff open one by
id, or a host with a directory declares it in its console extension
(`merchants`, below), and the switcher lists them with each role. A user with
none sees an empty state that points at an operator or, with a creation page
(`newMerchantPath`), offers "New merchant". Any link may open the console on a
merchant with `#merchant=<id>`: the console asks for an exchange, drops the
fragment, and says so when it is refused.

**Where staff sign in.** Embedded, at the host's AuthKit JSON API on the
console's origin: `Routes.AdminConsoleAuthBase`, AuthKit's `APIBase()` (such as
`/auth/v1` when AuthKit's `APIPath` is `/auth`), or AuthKit's default `/api/v1`
when empty. Standalone, at a trusted issuer when
`server.Config.ConsoleIssuer` (`admin_console.issuer`) names it and the
console's client there, for `auth.resource.id`; otherwise at the server's own
AuthKit. A standalone console needs one of the two: without `local_sign_in` the
server serves no sign-in, and a console with neither refuses to boot. Register
the console at your issuer as a public client with the redirect URI
`<console URL>/callback`, the authorization-code and refresh grants and
OpenRails' resource identifier; its tokens' permissions, within its
application's role in the merchant's group, decide what each person may do.

**Where it finds the API.** The console reads `config.json` beneath its path:
`api_base_url` is `Routes.Prefix` + `/v1` (`/v1` standalone), and
`auth_base_url` is the AuthKit JSON API staff sign in through:
`Routes.AdminConsoleAuthBase` (`/api/v1` by default) embedded, the control
plane's standalone. Both are paths on the console's own origin. The
admin API answers no cross-origin requests, so the console, the API and
AuthKit share one origin. A separate host such as `billing.example.com` works by
sending that host to the same server, or to a router (for example a Gin engine
chosen by `Host`) that mounts AuthKit and OpenRails, with `AdminConsole`, for
it. No CORS or cookie setting is involved: the console sends bearer tokens.

### Console build

`web/admin` go:embeds its `dist` build, which is never committed: a binary
built from an OpenRails checkout carries a console only when `web/admin/dist`
was built before `go build`; an embedding host supplies its own build as
`Deps.ConsoleAssets`. Node/pnpm is a build-time dependency only.

**Standalone binary.** Only `dist/.gitkeep` is committed, so building
`server/cmd/openrails` needs no Node and yields a console-less binary:

```sh
task admin-build           # build web/admin/dist (gitignored)
task build-console-binary  # admin-build + go build ./cmd/openrails in server/
```

Release archives and Docker images always carry the console (still off until
`admin_console.enabled`): goreleaser and the Dockerfile build `web/admin` first.

**Embedded hosts.** A module fetched into the Go module cache carries only
`web/admin/dist/.gitkeep`, so the host builds the console and hands it over.
The host repo owns a tiny embed package over a **gitignored** dist its build
pipeline produces:

```go
// internal/consoleassets/assets.go
package consoleassets

import "embed"

//go:embed all:dist
var FS embed.FS
```

Build the dist straight from the openrails module cache (invoke via `bash`:
module-cache files are not executable, and the script copies the read-only
source to a temp dir before `pnpm install`):

```sh
bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" internal/consoleassets/dist
```

Then pass it as `Deps.ConsoleAssets`:

```go
assets, _ := fs.Sub(consoleassets.FS, "dist")
client, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool, AuthKit: auth, AuthorityFor: staffAuthority, ConsoleAssets: assets})
```

Without `Deps.ConsoleAssets` the engine uses the build embedded in the
OpenRails module (the standalone binary's). The console handler answers a
request outside its path with 500 and a log line rather than serving a page
whose assets cannot load.

### Extending the console

A host can build its own console: the OpenRails console plus its own pages,
sidebar entries and account-menu items, all inside the same shell, session and
merchant switcher. Standalone builds carry no extensions.

The host writes a module whose default export is a list of extensions, typed by
`@openrails/console` (which resolves to `web/admin/src/extensions/public.ts`):

```ts
import { defineConsoleExtension } from "@openrails/console"

export default [
  defineConsoleExtension({
    id: "hosted", // also its key in the server's AdminConsole.Extensions
    routes: [
      { path: "/account", scope: "user", lazy: () => import("./account").then((m) => ({ Component: m.AccountPage })) },
      { path: "/plan", scope: "merchant", lazy: () => import("./plan").then((m) => ({ Component: m.PlanPage })) },
    ],
    nav: [
      { title: "Overview", path: "/account", scope: "user" },
      { title: "Plan", path: "/plan", scope: "merchant", group: "Setup", roles: ["owner"] },
    ],
    newMerchantPath: "/merchants/new",
    merchants: () => authFetch("/api/v1/me/merchants").then((r) => r.json()),
    settingsTabs: [
      { value: "team", title: "Team", lazy: () => import("./team").then((m) => ({ Component: m.TeamTab })) },
    ],
  }),
]
```

- **Scope.** `merchant` pages act on the selected merchant: they sit in the
  console's groups and, for a user with no merchant, show the empty state.
  `user` pages belong to the signed-in user and stay reachable without one.
- **Merchants and settings.** `merchants` is the signed-in user's merchants
  from the host's own directory (`{id, slug, display_name?, role?}`; one
  extension at most). `settingsTabs` add tabs to Settings, by `order` and
  `roles`; a team, API-key or invitation page is the host's, built on the
  server's Go methods. `EmptyState` renders in the no-merchant state, such as
  the invitations a user may accept.
- **Navigation.** `nav` entries join the sidebar by `group` (merchant entries
  default to the console's "Billing" group, user entries to "Account") and
  `order`; `roles`, `visible(ctx)` and the `useVisible` hook (for data the
  extension loads) hide them. `userMenu` adds account-menu
  entries. A path the console already routes refuses to start.
- **Runtime.** `useConsole(id)` gives the user, their merchants, the selected
  one and the extension's `server.Config.AdminConsole.Extensions[id]`; `authFetch()` calls
  the host's own APIs with the console's session (local or a trusted
  issuer's) instead of starting a second one, and `authClient()` is the AuthKit
  client when staff sign in to the deployment's own accounts; `consoleHref(path, merchant?)` links into the
  console. `Provider` wraps the console for host context (outside the router).

Build it with the extension module and the host's source root, which its `@/`
imports resolve against and whose classes Tailwind compiles:

```sh
bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" \
  --extensions web/console/index.ts --extensions-src web/src internal/consoleassets/dist
```

The host installs its own dependencies first; its files resolve them from its
`node_modules`, while React, React Router, TanStack Query, auth-ui and the icon
set are always the console's single copies. The console's own sources are
type-checked in OpenRails; the host type-checks its extension against the three
self-contained files `web/admin/src/extensions/{public,types,runtime}.ts`.

**Embedding contract.** Extensions are a standalone server's: supply the
combined build as `Deps.ConsoleAssets`, and pass extension data as
`server.Config.AdminConsole.Extensions`, served verbatim in `config.json` under
`extensions`. OpenRails never reads it.

### Security posture

What the engine enforces:

- The SPA itself — static assets and `GET <path>/config.json` — is served with
  **no authentication at the transport layer**. Anyone who can reach the
  console path gets the app shell and the bootstrap document (base URLs +
  feature flags; no secrets, no data).
- All **data and actions** go through `/v1/admin/*` with a Bearer token
  (an AuthKit session or a trusted issuer's access token) and are enforced server-side by
  each route's permission plus per-query merchant scoping. The console
  has no client-side privilege of its own; a 403 renders as a
  "role lacks permission" toast.
- Core OpenRails imposes **no environment restriction**: a mounted console
  is served in any env.

Recommendation (not engine-enforced): treat exposing the console like exposing
any login page. If your deployment doesn't want the console reachable at all in
production, gate it at boot — e.g. an embedded host may leave `Routes.AdminConsole` out in
a production-like env precisely because the SPA is
unauthenticated at the transport layer, making it dev-only by convention.
Standalone SaaS deployments that do serve it in production should front it with
their normal edge protections (TLS, rate limits — OpenRails' own rate limiting
covers the auth endpoints).

Who may sign in is decided by the admin API, not the console: each route asks
the mount's `Auth` for its permission, at the merchant or in the root scope
([auth](auth.md#permissions)).

### Viewing it

Browse to the console path, `https://<your-host>/admin/` by default (the bare
path redirects). The SPA bootstraps from `config.json` beneath it:
`{auth_base_url, api_base_url, catalog_copilot_enabled, catalog_drafting_enabled, extensions, issuer, merchant}`.

**Login** is AuthKit's own: the console's session is auth-ui's (`@openrails/auth-ui`),
whose sign-in form offers password, the deployment's login-capable OIDC providers
(AuthKit's `/oidc/{provider}/login`), second factors, account
recovery and backup codes. auth-ui keeps the access token in memory and the
rotating refresh token in the tab's `sessionStorage`, which restores the session
across reloads of that tab; every API call carries the bearer, never a cookie.
OpenRails admin writes do not request a recent sign-in. AuthKit's own
API-key creation and role grants retain auth-ui's step-up dialog: after
`401 step_up_required`, the console asks the user to confirm it's them and
retries that AuthKit operation. Who can sign in
and what they may do: standalone, the server's merchant roles
(`owner`/`support`/`viewer`, or the merchant's custom roles) or a trusted
issuer's token; embedded, the host's `Routes.Auth`.

**At a trusted issuer**, the console is that issuer's OAuth 2.0 client
(auth-ui's issuer client): the login page redirects there, `/callback`
completes the code flow with PKCE, the access token stays in memory and the
rotating refresh token in IndexedDB. The server's AuthKit must trust the
issuer for the merchant, and the console asks for tokens for
`auth.resource.id`. AuthKit operations that request step-up re-authorize
at the issuer with `max_age=0` in a popup and run again. OpenRails admin
writes use no step-up dialog. Sign-out revokes the refresh token and ends
the issuer session.

Local UI dev: `cd web/admin && pnpm run dev` (Vite proxies `/v1`, `/auth`, and
`/admin/config.json` to `localhost:3053`).

### Using it

| Page | Route | What it does |
|------|-------|--------------|
| Dashboard | `/` | Per-merchant metrics widget grid (drag/resize, saved per merchant). A range picker (presets, or a custom start and end date) sets every widget's range and stays in the URL (`?range=7d`, `?from=2026-09-01&to=2026-09-30`); a widget with `fixed_range` keeps its own, as the default "today" row does (each tile against yesterday) |
| Reports | `/reports` | The daily subscription report (started, canceled, net change per UTC day) and the monthly renewal report (renewal payments, cancellations, churn per UTC month) over the dashboard's range, each with a CSV download |
| Customers | `/customers` | Search → customer profile: subscriptions, payments, entitlements, payment methods; grant/revoke entitlement + product access (1, 7, 30 or 365 days after any access the customer has, until a date, or never) |
| Subscriptions | `/subscriptions` | Status filters incl. the past_due dunning view, and two views: Renewing soon (active, period ending within 7 days, soonest first) and Recently canceled (the last 30 days, latest first), as `?view=renewing_soon` / `?view=recently_canceled`; cancel (typed confirmation), resume, NMI payment-method change |
| Payments | `/payments` | Filters, payment detail, rail-aware refund (disabled on rails without API refunds) |
| Catalog | `/catalog` | Products/prices, price detail + change wizard, archive/restore, drift view, catalog copilot panel. Price detail also shows the price's `psp_links` (per-PSP link state, link ids, opt-in live provider verify) and a checkout-readiness dry run naming the PSP a checkout would land on and why each other candidate was skipped. Links are read-only here — the catalog declares them, the provider adapter pushes them. |
| Ops | `/ops` | Findings queue (approve/ignore), the merchant inbox (ledger repairs and worker stalls arrive there as critical notifications) |
| Settings | `/settings` | Tabs: Merchant profile (with MerchantConfig), Notifications (email and encrypted webhooks), PSPs (arm, rotate credentials, archive), Customer controls, Roles (custom roles, given to teammates and API keys), and a host extension's |

**Assistants** are off without the server's `llm:` config and show only in the
area they serve, to staff who see that area. `config.json` mirrors each gate so
the UI shows a pointed empty-state instead of a broken button:

| Assistant | Route group | Gate (config / env) |
|---------|---------|--------------------|
| Catalog copilot Q&A (read-only catalog/subscriber aggregates) | Catalog | key AND `llm.catalog_copilot_enabled` / `LLM_CATALOG_COPILOT_ENABLED` |
| Copilot drafting (drafts price changes into the wizard; a human always confirms — the model never mutates) | Catalog, with catalog updates | copilot AND `llm.catalog_drafting_enabled` / `LLM_CATALOG_DRAFTING_ENABLED` |

Catalog Q&A belongs to the optional console API:
`POST /v1/console/catalog/ask`. It is outside the merchant Go `Client`, while
retaining the same merchant binding and catalog authorization as catalog
reads, the opt-in model capability, and the `catalog-ask` request limit. It
can serve an embedded host's own console as well as the bundled console;
turning on assistance does not grant authority to edit products or prices.
Drafts still require a separate, authorized catalog action by the operator.

Lists page by cursor: the pager announces the rows shown and offers rows
per page, first, previous and next. A plain confirmation is the console's
one confirm dialog; destructive acts ask for a typed word. After a deploy, a
page whose code is gone offers a reload.

The dashboard a merchant starts with, until it saves its own: today's new
subscriptions, renewals, cancellations, failed purchases and net change, each
against yesterday; active subscribers, net new subscribers, MRR, net revenue,
churn rate, failed renewals and subscriptions in dunning; new vs canceled by
day; cancellations by type; net revenue by rail and by stream; payment health
by PSP; new subscriptions by subscriber type; and, with usage, credits sold vs
usage revenue and the top customers by usage revenue.

`llm.provider` is `anthropic` (default) or `openai`; `llm.base_url` points
`openai` at any OpenAI-compatible backend (Groq, Ollama, vLLM). Everything else
on the dashboard works keyless.

Day-to-day workflows — catalog authoring, dunning, refund doctrine — live in
[the merchant guide](merchant-guide.md). For programmatic access to the same
metrics the console uses, see [metrics-for-llms.md](metrics-for-llms.md).
