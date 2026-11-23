# Standalone server

The `server/` module (`github.com/open-rails/openrails/server`): the `openrails`
binary and the library hosted products build on. It composes the engine
(`openrails.New`) with its own AuthKit and a multi-merchant control plane, loads
configuration, provisions the merchants a manifest declares, serves every route on
one port, and ships as an image and a Helm chart. It adds no billing logic. Guides:
[standalone](../standalone.md), [server package](../standalone.md),
[configuration](../standalone.md#configuration-reference), [Kubernetes](../standalone.md#kubernetes-and-helm),
[operations](../standalone.md).

## Concepts

- **Module**: its own `go.mod`, built against the root's source (`replace … => ../`);
  it imports the root's `internal/` packages and the root never imports it.
  `scripts/release` versions both together.
- **`server.Server`**: what `server.New(ctx, server.Config, server.Deps)` returns.
- **Control plane**: `controlplane.ControlPlane`, over the server's AuthKit client.
  A merchant is an AuthKit group whose id is the merchant id; teams, API keys and
  trusted issuers are AuthKit's, in that group.
- **Merchant manifest**: the file of merchant declarations (`--merchant-manifest`,
  default `DefaultMerchantConfigManifestPath`) and its overlays.
- **Operator**: `server/internal/operator`, the operations the CLI and the
  `server.Server` methods share.
- **Console**: the SPA at `/admin`, built from `server/web/console` (package `admin`,
  `admin.FS`) and served by `adminconsole.Handler`.

## How it works

### Build

`server.New` (`server/server.go`):

1. Refuses an empty `Auth.DefaultTenantID` (`controlplane.RequireDefaultTenant`), a
   declared merchant's `Issuer` (a credential's merchant is its group) and a host
   `Engine.UserInfo`.
2. Installs `customerDirectory` as the engine's `UserInfo`
   ([customer directory](customer-directory.md)) and calls `openrails.New`, which
   migrates unless `SkipMigrations`.
3. Builds AuthKit: `controlplane.AuthKit` fixes merchant roles, the API-key prefix,
   the resource's scopes, the console's client (`ConsoleClientID`), AuthKit's River
   schema and mount, and `SkipMigrations`. AuthKit gets its own pool on the engine's
   database unless one is given.
4. `controlplane.New`, `operator.Join`, then `operator.StandaloneServer` builds the
   HTTP surface.

### Configuration

`hostconfig.Load` (`server/internal/hostconfig`) layers defaults, `config.yaml`
(`--config`, `OPENRAILS_CONFIG`), a logged `.env`, the environment (`DB_URL` is
`db.url`), files under `VAULT_SECRETS_PATH` and flags (`WithOverride`). Unknown keys
and retired names (`ENV`, `API_URL`, `MERCHANT_CONFIG_SOURCE`, …) refuse boot;
`Validate` checks the result. `--provider-write-mode` or `provider_write_mode` is
required. One-off commands load less: `LoadForCommand`, `LoadDatabaseForCommand`,
`LoadDirectoryForCommand`.

### Boot merchants

`serverboot.ReconcileBootMerchantManifest` runs on `run-server` and `run-worker`. It
loads the manifest and overlays (`bootstrap.LoadMerchantConfigManifestWithOverlays`),
creates each missing merchant with its AuthKit group and `remote_application` (a
trusted issuer), and puts file declarations in place
(`merchantbootstrap.ProvisionMerchant`). Only PostgreSQL or an unreadable manifest
fails boot. A merchant it cannot provision is refused alone, and `retry.Forever`
re-runs the whole manifest in the background until every one is provisioned.
Without the default manifest the server boots control-plane only.

### Control plane

- `ControlPlane.CreateMerchant` commits the merchant row and its group together
  (`merchants.Service.Provision` with `createMerchantGroup` under `authkit.InTx`).
  `ProvisionMerchant` answers the merchant a name resolves to, else creates one; a
  concurrent claim reports the winner with `Created: false`.
- A hosted product's `MerchantCreation` adds reserved names, a pattern and
  `operator.MerchantCreationAdmission` (a verified email, `FreeAllowance`, then
  `HasVaultedPaymentMethod`). Open self-registration is refused without it
  (`validate` in `server/internal/controlplane/service.go`).
- `MerchantScope` is a live merchant's group scope; route gates ask `Can` there or in
  the root scope. Roles: `MerchantRoles` (`viewer`, `support`, `owner`) plus custom
  roles (`MerchantRolesRead`, `MerchantRolesManage`).
- AuthKit's routes mount selectively: `IntentionalRouteGroups`, registration when
  enabled, and `trustedIssuerRouteGroups` (token endpoint, SCIM directory) for a
  resource server. Browser OIDC and AuthKit's admin surface are never mounted.
- `FleetAnalytics` and `FleetTimeseries` aggregate across merchants for the operator.
- Lifecycle methods (`DeleteMerchant`, `RetireUnusedMerchant`, `PurgeMerchant`, …) call
  the engine's [merchant configuration](merchant-configuration.md) services.

### HTTP

`server/internal/http` builds one public handler: health and meta, the engine's route
groups (`RouteGroups`, each off by default), `/v1/me`, `/v1/webhooks/{rail}/{account_id}`,
AuthKit's routes, `/admin`, and the payment page on `CheckoutOrigin` by Host. Every
native route passes `wrapHandler`: recovery, request log, security headers, CORS for
browser-tier routes only (`BrowserTierRoutes`), `RequestLimitsHTTP`, explicit billing
credentials (`billingauth.ExplicitCredentials`) and the engine's `RateLimitHTTP`.
`PrivateHandler` serves `/metrics` on `PrivateAddr`.

`Server.Run` binds job producers (`InitRiver`) before listening on `:3053`, starts the
workers, and on stop fails `/health/ready` (`Drain`) and keeps serving for
`DrainDelay`, then finishes within `ShutdownTimeout`. `Serve` runs no workers. A host
with its own router mounts `Routes` (checked by `embedhttp.ValidateRouteTable`) and
adds `RiverJobs` to its fleet.

### Console

`registerAdminConsoleRoutes` serves the SPA and its `config.json` (AuthKit base,
sign-in issuers, the token exchange for `ConsoleClientID`) under
`hostconfig.ConsolePath`. `GET /admin/merchants` (`consoleMerchants`) is the selector's
directory: it needs an explicit `Authorization` header and lists only merchants the
credential's permissions reach. The SPA calls `/v1` with the staff token.
`AdminConsole.Disabled` removes the subtree.

### Binary, image and chart

- `server/cmd/openrails`: `run-server` (`--no-workers`, `--merchant-manifest`),
  `run-worker`, `migrate up|pg|status`, `version`, and the operator commands
  (`merchants`, `merchant`, `book`, `billing`, `catalog`, `destructive-holds`,
  `admin-lockouts`, `workers`, …). `applyStandaloneMigrations` runs `engine.Migrate`,
  then `authkit.Migrate` with River's schema.
- `Dockerfile`: builds billing-ui, the console and the payment page, embeds them,
  cross-compiles with `CGO_ENABLED=0`, and runs as uid 1001 on Alpine with
  `EXPOSE 3053`, a `/health/ready` healthcheck and `CMD ["run-server"]`.
- `deploy/helm/openrails`: one Deployment running `run-server`, with ConfigMap,
  Service, Ingress, PodDisruptionBudget and ServiceMonitor templates. Releases publish
  it at the image's version.

## Interfaces

- Go: `server.New`, `Run`, `Serve`, `Start`, `Handler`, `Routes`, `RiverJobs`,
  `PrivateHandler`, `Engine`, `AuthKit`, and the control-plane methods
  ([server package](../standalone.md)), including `ListOpenDestructiveHolds`;
  `admin.FS`.
- HTTP: the engine's catalog ([routes](../api/routes.md)), AuthKit's routes under its
  issuer path, `/admin`, `/health/live`, `/health/ready`; `/metrics` privately.
- Configuration: `config.yaml` keys ([configuration](../standalone.md#configuration-reference)), the
  manifest and `merchant_manifest_overlays`, and the chart's `values.yaml`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: the server makes no provider calls; the engine's run unchanged inside it.

### 2. Agreement with providers
Does not apply: callbacks reach the engine's webhook route unchanged (`RegisterWebhookRoutes`).

### 3. Exact money
The server keeps no balances. Its one money output, fleet analytics, stays per
currency in `int64` native units and never sums across currencies.

Checks:
- `FleetRevenueByCurrency` and `FleetMRRByCurrency` group by currency.
- `billing.FleetCurrencyRevenue` and `billing.FleetMRR` carry amounts as decimal
  strings beside `currency`.

### 4. Access control and tenant isolation
AuthKit's Authenticator decides who every request is, and a merchant's authority is
its group: a credential acts only in the group it is bound to. No route provisions or
deletes merchants or manages teams and keys; those are Go methods whose caller
authorizes and audits (`server/merchants.go`). The console's `GET /admin/merchants`
lists only the merchants a credential reaches. Fleet analytics read across merchants
by design and are Go-only.

Checks:
- `server.New` refuses `Engine.Merchants[…].Issuer` and `Engine.UserInfo`.
- `MerchantScope` answers `ErrMerchantUnresolved` for a merchant that is not live or
  has no group.
- `consoleMerchants` refuses a request without an `Authorization` header.
- OR-J32-S03: deleting a merchant closes its credentials' authority.

### 5. Contracts match the code
`api/go.txt` lists `pkg openrails/server` and `pkg openrails/server/web/console`. The
HTTP surface is the engine's catalog plus AuthKit's routes and `/admin`, so the
in-process and remote clients see the same API. The image, chart and modules share one
version.

Checks:
- `go run ./scripts/contracts` covers the server module's API.
- Every engine route is mounted from `internal/http/routes` (`RegisterMetaRoutes`,
  `RegisterUserRoutes`, `RegisterCustomerRoutes`, `RegisterStaffRoutes`,
  `RegisterAppRoutes`, `RegisterWebhookRoutes`), never copied; `Routes` passes
  `embedhttp.ValidateRouteTable`.
- `hostconfig` names each retired key in its refusal.

### 6. Data lifecycle
The server adds no OpenRails table. AuthKit's tables are outside OpenRails'
retention classes and the billing archive.

### 7. Work scales with activity
The server schedules nothing of its own: the boot manifest is reconciled once, and
while a merchant is unprovisioned `retry.Forever` re-runs it with backoff. Retirement
recovery and fleet analytics run when the operator calls them.

Checks:
- `server/` registers no periodic job; `RiverJobs` is the engine's contribution.
- `ReconcileBootMerchantManifest` starts a background retry only while a merchant is unprovisioned, and it stops once all are.
- `CompletePendingMerchantRetirements` takes a bounded limit.

### 8. Abuse protection
Every native route passes the engine's rate limiter and the 1 MiB body cap, and
AuthKit applies its own buckets (`auth.http.rate_limits`). Listener timeouts bound
slow peers. Native registration is closed by default and invite-only with
`LocalSignIn`; open registration needs `MerchantCreation`, whose admission asks for a
verified email and, past the free allowance, a vaulted payment method.

Checks:
- `wrapHandler` chains `RequestLimitsHTTP(middleware.DefaultMaxBodyBytes)` and
  `RateLimitHTTP`.
- `Server.run` sets `ReadHeaderTimeout`, `ReadTimeout`, `IdleTimeout` and
  `MaxHeaderBytes`.
- `validate` refuses `RegistrationModeOpen` without `MerchantCreation`.

### 9. Destructive-action safeguards
`PurgeMerchant` uses the engine's gated purge, `RetireUnusedMerchant` refuses any
activity, and the merchant handle's `ClearDestructiveHold` (`ActingFor` the
operator in process) needs a reason and a person other than the hold's opener. Gap:
`DeleteMerchant`, `RestoreMerchant` and an operator's `RenameMerchant` record no
actor; the caller audits them.

Checks:
- `PurgeMerchant` calls `merchants.Service.Purge`; `openrails merchant purge` requires
  `--by`.
- OR-J32-S04 and OR-J32-S05.

## Tests

- **Server fixture.** `critical/server-fixture` is built once as a test binary
  (`OPENRAILS_CRITICAL_SERVER_BINARY`) around `server.New` and driven over HTTP and a
  command channel (`newStandaloneWorld`): OR-J01-S09, OR-J01-S16, OR-J07-S01 to S03,
  OR-J07-S05, OR-J32-S01 to S04. It runs with the console disabled.
- **CLI.** The built binary (`OPENRAILS_CRITICAL_CLI_BINARY`, `hostCLI`) runs
  OR-J04-S41, OR-J22-S04, OR-J31-S03, OR-J31-S10, OR-J32-S05, OR-J38-S21 and
  OR-J38-S72, and the archive tests' `billing prepare-target`.
- **Image smoke** (`smoke-image` in `.github/workflows/docker-publish.yaml`): CVE scan,
  arm64 build, `version`, `migrate pg`, then `run-server` from environment only until
  `/health/ready` passes and `/admin/` serves the console.
- **Chart.** `scripts/check.sh chart` lints and renders it with `values.yaml` and
  `ci/full-values.yaml`.
- Gaps: no test runs `run-server` with a merchant manifest or overlays, or
  `hostconfig.Load` beyond the smoke's environment; no browser test drives the
  console (the smoke only fetches `/admin/`), and its `config.json` and
  `/admin/merchants` have no scenario; `server/ci/paymentpage` is run by no script or
  workflow.
