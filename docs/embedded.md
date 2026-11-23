# Embedding OpenRails

Embed OpenRails when your Go server should run billing in its own process. Your
app owns sign-in, the HTTP server and shutdown; OpenRails owns its schemas in
your PostgreSQL (18 or newer), its River jobs and its routes. Your code calls
the merchant's `*openrails.Client` with no network hop; the [client
guide](client.md) covers every method. [`examples/embedded`](../examples/embedded)
is a complete app; [`examples/own-auth`](../examples/own-auth) runs two
merchants behind the host's own authentication.

## Boot

```go guide:embedded-new
// You, the seller, and your payment processor accounts (PSPs), declared in merchant.yaml.
merchant, err := openrails.ReadMerchantFile("merchant.yaml")
if err != nil {
	return nil, nil, err
}

merchant.Issuer = issuer // your users, signed in at your AuthKit, are this merchant's customers and staff
cfg := openrails.Config{
	Database:          openrails.DatabaseConfig{Schema: "billing"}, // the Postgres schema OpenRails' tables go in
	ProviderWriteMode: openrails.ProviderWritesFull,                // Set to ProviderWritesReadOnly to prevent any billing
	Merchants:         map[string]openrails.MerchantDeclaration{merchant.Slug: merchant},
}

// Build the billing engine; it creates or upgrades its own tables. It has no logins
// of its own: your auth guards its routes when you mount them.
bill, err := openrails.New(ctx, cfg, openrails.Deps{
	Postgres: db,            // the same pool your app uses
	UserInfo: ak.UserInfo(), // AuthKit's user lookup: each answer is the user's current email, name and username
})
if err != nil {
	return nil, nil, err
}
// Your merchant's handle: the Go client your own code calls, in process.
shop, err := bill.MerchantBySlug(merchant.Slug)
if err != nil {
	_ = bill.Close(context.WithoutCancel(ctx))
	return nil, nil, err
}
```

`New` does, in order: validate `Config`; migrate the billing and River schemas
under an advisory lock (one replica at a time; a failed migration fails `New`);
create this month's partitions; provision every declared merchant; start
checking PSP credentials in the background. It starts no workers and never
changes the catalog. Then:

```go guide:embedded-start
// Background work. OpenRails runs its own River workers: rebills, retries of
// failed payments, invoices.
if err := ak.Start(ctx); err != nil {
	return err
}
if err := bill.Start(ctx); err != nil {
	return err
}
```

| Call | Does |
|---|---|
| `Engine.Start(ctx, opts...)` | runs OpenRails' River workers, or binds them to your fleet (`WithRiverClient`, [below](#river)). A second `Start` is refused. |
| `Engine.Ready(ctx)` | nil once PostgreSQL, the merchants and a local River consumer serve. Register it as your readiness check. |
| `Engine.Probes()` | named checks of the optional parts, for your supervisor: `openrails_vault`, `openrails_psp_posture`, `openrails_configuration`, `openrails_job_progress`, … |
| `Engine.Close(ctx)` | stops what `Start` started. Close the engine before your pool; OpenRails never closes `Deps.Postgres`. |

`New` fails only on what it cannot serve without: PostgreSQL and its migrations,
an unset `ProviderWriteMode`, an invalid River schema, a test seam beside a live
PSP. Anything else is refused alone and the rest serves: an optional setting is
turned off, a merchant whose document is invalid is not provisioned, a PSP that
does not check is not armed. `openrails_configuration` names what is not served
and why; Vault, PSP posture and Redis reconnect in the background.

### Health

```go guide:embedded-health
mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
	if err := bill.Ready(r.Context()); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
})
for _, p := range bill.Probes() { // optional parts: report them, never fail on them
	if err := p.Check(ctx); err != nil {
		logger.WarnContext(ctx, "billing degraded", "probe", p.Name, "error", err)
	}
}
```

`openrails_job_progress` fails with no River leader, an overrunning job, or a
billing pass that has not finished within a day or left critical work (usage
postings, renewals and retries, balance watches, delinquency, order expiry,
usage billing) due since before yesterday (UTC).

## Config

`openrails.Config` is plain data: decode it from your own file or build it in
Go. Libraries never read the environment.

| Field | Meaning |
|---|---|
| `ProviderWriteMode` | Required. `ProviderWritesFull`, `ProviderWritesLimited` (OpenRails initiates no provider writes; customers still buy) or `ProviderWritesReadOnly` (nothing is written to any provider). |
| `EngineAdmissionHold` | Pauses admission and first submission of new engine payments; submitted ones are still verified. |
| `UsageBillingEnabled` | Bills owed usage automatically ([usage](client.md#usage-and-running-charges)). Deploy every worker first. |
| `Database` | `DatabaseConfig`: `Schema` (default `billing`) and `RiverSchema` (default the schema plus `_river`), plain identifiers; `SkipMigrations`, check without migrating: `New` is refused with `ErrMigrationsRequired` until `Migrate` has run ([migrations](#migrations)); `LeaseConns`, the cap of the separate pool that renews idempotency leases (default 4), part of your connection budget. |
| `Merchants` | The merchants this engine serves ([merchants](#merchants)). |
| `PublicBillingBaseURL` | The external billing prefix without `/v1` (`https://shop.example/billing`), for provider callbacks and billing links. |
| `ReturnOrigins` | Origins a buyer may return to after a redirect step (`returnUrl`). |
| `TrustedProxies` | Load-balancer CIDRs; `X-Forwarded-For` is walked right to left past them. Unset, the socket peer is the client, so every client behind a proxy shares one rate-limit bucket. |
| `CheckoutOrigin` | The payment page's origin ([checkout page](#checkout-page)). |
| `RateLimits` | Per-minute buckets on customer routes (`purchase`, `subscribe`, `payment`, `webhook`, `catalog-ask`): nil is the built-in set, empty is none. |
| `Captcha` | `SiteKey`, `SecretKey`, `Provider` (`turnstile`, `recaptcha-v3`, `hcaptcha`): challenges a subject at 3× a bucket's limit. |
| `UserActionRateLimit` | The merchant-wide budget for customer cancellations and card removals. |
| `DestructiveActionLimits` | Per-actor, per-merchant and deployment ceilings on staff destructive actions ([destructive holds](client.md#destructive-holds)). |
| `Vault` | `KVMount` (merchant configuration in Vault), `ScopePrefix`, `TransitMount` (Solana signing). The client is `Deps.Vault`. |
| `CardVault`, `HyperSwitch` | A platform card vault and HyperSwitch ([custody](client.md#merchant-configuration-and-psps)). |
| `Solana` | The deployment's crankers: they publish plans and pull renewals. Without one, Solana rebilling is off. |
| `LLM` | The model behind the console's catalog Q&A and drafting. |
| `ReadCache` | Overrides how long a handle serves catalog and configuration reads from memory ([caching](client.md#caching)). |
| `ProviderSandbox` | Fake provider endpoints and CCBill webhook sources for tests; refused beside a live PSP. |
| `EventRetention` | How long the event log keeps facts (default 90 days). |
| `WebhookSecretOverlap` | How long a rotated webhook secret still verifies (default 24 h, at most 168 h). |

Several instances must agree on the schemas, `ProviderWriteMode`, `Merchants`,
`RateLimits`, `Captcha`, `TrustedProxies`, `ReturnOrigins` and the mounted
`Routes` (a route some instances lack answers 404 there).

## Deps

`openrails.Deps` is everything outside the process. OpenRails borrows each one
and closes none.

| Field | Meaning |
|---|---|
| `Postgres` | Required: your `*pgxpool.Pool`. Its role owns every object OpenRails creates; another owner is your `SET ROLE`. |
| `PostgresSession` | A direct-connection `*pgxpool.Config` when `Postgres` goes through a transaction pooler ([PgBouncer](#pgbouncer)). |
| `Redis` | Abuse state (rate limits, lockouts, captcha, card-testing declines, spent DPoP proofs) shared across instances. Without it each process counts alone: right for one instance. No money path needs Redis. |
| `Vault` | A logged-in `*vaultapi.Client` (`helpers/vault` logs in and renews). |
| `Email` | Your `EmailSender` for operator alerts (`helpers/smtp` is one). Customer notices are events, not email ([events](client.md#events)). |
| `Logger` | A `*slog.Logger`; nil is `slog.Default()`, read once in `New`. OpenRails never changes your logger or the default, and never logs secrets, card data or SQL arguments. |
| `UserInfo` | Your directory, for customer contacts and staff search: `ak.UserInfo()`, or your own `Get` and `Search`. Without it the SCIM routes mount instead ([customers](client.md#customers)). |
| `Notifications` | An account-inbox channel for selected customer notices ([account inbox](#account-inbox)). |
| `MerchantGroups` | Live group metadata for tenant-bound merchants ([merchants](#merchants)). |
| `CheckoutAssets` | The built payment page (`web/pay`) as an `fs.FS`. |
| `StripeTransport`, `NMITransport`, `BasisTheoryTransport`, `FXTransport`, `Clock` | Test seams; refused beside a live PSP. |

`Deps` holds no auth: your `Routes.Auth` guards what you mount.

## Merchants

`Config.Merchants` declares each merchant this engine serves, as
`billing.MerchantDeclaration`: the merchant's document, the `Issuer` whose
sign-ins are its customers and staff, and an optional tenant `Binding`.

- **The document** holds the slug, display name, settings, PSPs, custodians,
  alert webhooks and secrets, in the format [client: merchant
  configuration](client.md#merchant-configuration-and-psps) describes.
  `ParseMerchantDocument` reads one from YAML or JSON strictly: unknown or
  duplicate fields, aliases, anchors and several documents are refused. Keep
  secret values out of the committed file.
- **Slugs** are unique per engine; a duplicate is refused and reported. Each
  declared merchant is created on first boot and keeps its UUID;
  `Engine.DeclaredMerchant(slug)` returns it.
- **`Issuer`** binds sign-ins: a request whose credential comes from that
  issuer acts at that merchant (one merchant per issuer per engine). Several
  merchants on one issuer bind through the credential's scope instead
  ([auth](#auth)).
- **Vault.** With `Config.Vault.KVMount`, Vault holds each merchant's
  configuration and staff edit it over HTTP or in the console; the declaration
  then names the slug, `Issuer` and `Binding` only, and a document beside Vault
  is refused. Without it the document is read at startup and the edit routes
  are not mounted.
- **Tenant binding.** The `adapters/authkit` module's `BindMerchant` associates
  an existing merchant with one AuthKit tenant, once and immutably; declare the
  result as `Binding` and pass `MerchantGroups` (`MerchantGroups(authClient)`
  from the same module). Customer routes then also require the user's tenant
  (`customer_tenant_mismatch` otherwise). Billing archives carry the binding.

The catalog has its own lifecycle: `New` never applies one ([catalog](client.md#catalog)).

## Migrations

`New` migrates through your pool, under an advisory lock, before it serves:
one instance migrates, the rest wait. To migrate as a deploy step instead, run
`openrails.Migrate(ctx, pool, cfg.Database)` (or `openrails migrate up`) and set
`DatabaseConfig.SkipMigrations` on the servers, which then only check the ledger.

- An older build boots on a schema a newer one migrated (it leaves unknown
  migrations alone), so rolling upgrades work in both directions within v1.
- A database that recorded a migration with other content is compared with a
  fresh build of its recorded migrations, whichever monthly partitions exist:
  equal is re-stamped, different is refused with the difference.
- Only OpenRails' objects may live in its schemas. Before v1 a release may break
  the schema in one step; its notes say so, and hosts sharing it upgrade together.

## River

River is required. Jobs queue from `New` on; `Start` runs them, and `Ready`
fails until it does. By default OpenRails runs its own client in
`DatabaseConfig.RiverSchema`. A host with its own fleet registers OpenRails' jobs in it
and passes it to `Start`:

```go guide:embedded-fleet
fleet, err := riverhelpers.New(ctx, pool, &river.Config{
	Schema: "billing_river", // Config.Database.RiverSchema
	Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 10}},
}, bill.RiverJobs()) // and any other library's jobs, e.g. AuthKit's
if err != nil {
	return err
}
if err := fleet.Start(ctx); err != nil {
	return err
}
defer func() { _ = fleet.StopAndCancel(context.WithoutCancel(ctx)) }()
if err := bill.Start(ctx, openrails.WithRiverClient(fleet)); err != nil {
	return err
}
```

`RiverJobs` adds the `billing`, `psp_import` (2 workers) and `provider_refresh`
(4 workers) queues when missing and sets `RescueStuckJobsAfter` to 10 minutes
when unset; each worker declares its own timeout. The fleet must be in
`DatabaseConfig.RiverSchema`, run on every replica, and is yours to start and stop.
`Start` without `WithRiverClient` after `RiverJobs` went to a fleet is refused.
AuthKit joins the same fleet when its River schema matches. Every kind, queue
and schedule: [River jobs](river-jobs.md).

Replacing a fleet that ran an older release's scheduled provider refresh: stop
the old workers first; the new fleet cancels the retired refresh jobs in that
schema and keeps their history ([provider refresh](standalone.md#provider-refresh)).

## Several instances

Any number of instances can serve one billing database; there is no primary
and no sticky session.

- **PostgreSQL** holds every record, River's queue and leader election,
  idempotency claims, webhook deduplication and charge admission. All
  instances share one writable primary: independently writable copies using the
  same PSP accounts would collect the same renewal twice. Keep clocks
  NTP-synchronized.
- **Redis** (`Deps.Redis`) holds the abuse state. A Redis that stops answering
  costs sharing, not service: each instance counts in memory meanwhile.
- **Background work** runs on every instance with workers, each job on one.
  River's leader schedules periodic work; a crashed leader is replaced within
  about 20 seconds.
- **Caches.** An edit through OpenRails reaches every instance at once
  (`NOTIFY`); an edit made in Vault directly within 30 seconds.
- **Several hosts, one database.** Hosts embedding OpenRails on one schema
  declare the same merchants, each with its own `RiverSchema`, share one Redis
  and clocks within five minutes, and share periodic passes through leases in
  `periodic_leases`.

Size PostgreSQL `max_connections` for every pool, plus `DatabaseConfig.LeaseConns`
per instance, plus one per instance with Vault configuration (it listens for edits).

## PgBouncer

With transaction pooling, give `Postgres` the PgBouncer pool and set
`poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe` on
it; set `PostgresSession` to a `pgxpool.ParseConfig` result for the same database
and role through PostgreSQL's direct endpoint. OpenRails scopes the merchant to
each transaction and uses direct sessions for migrations, listeners and session
locks. Its own River workers poll in this mode; a host fleet on the PgBouncer
pool sets `river.Config.PollOnly`.

## Routes

```go guide:embedded-mount
// Billing. Public, customer (/me) and webhook routes are always mounted.
err = openrailsgin.Mount(r, bill, openrails.Routes{
	Auth:        ak.Authenticator(), // says who a request is; OpenRails decides what to admit
	Prefix:      "/billing",         // the API is served at /billing/v1/*
	RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true},
})
if err != nil {
	return err
}
```

Mount on the root router so the adapter applies `Prefix` once: `adapters/http`
(`net/http`, Chi), `adapters/gin` and `adapters/fiber` each have `Mount`.
`Engine.Routes(selection)` returns the same routes as `[]Route` (method, path,
handler) for any other router; `openrailsgin.MountRoutes` mounts such a list.

Public (`GET /v1/config`), customer (`/v1/me`) and webhook
(`/v1/webhooks/{rail}/{account_id}`) routes always mount. `Routes.RouteGroups`
adds the rest:

| Group | Routes | For |
|---|---|---|
| `Admin` | `/v1/admin` | customer support |
| `Catalog` | `/v1/catalog` | catalog reads and edits, price migrations |
| `MerchantConfig` | `/v1/merchant` | configuration, PSPs, imports, findings, archives |
| `Metrics` | `/v1/metrics` | business metrics and dashboards |
| `Programmatic` | `/v1/app` | your backends' application credentials |

A group never mounts without `Auth`, and `Mount` fails for a route neither of
whose permissions `Auth` knows. The mount strips ambient cookies: browsers send
credentials in `Authorization`. Customer and public routes answer any origin
without credentials; staff routes send no CORS headers. In-process `Client`
calls pass through none of this and count against no rate limit. Point each
PSP's webhooks at `{Prefix}/v1/webhooks/{rail}/{account_id}`.

## Auth

`Routes.Auth` is an `authkit/auth` `Authenticator`: it says who a request is and
whether they hold a permission in a scope; OpenRails decides what to admit and
answers every refusal itself (401 with `WWW-Authenticate`, 403
`permission_required` or `application_required`, 503 when your auth cannot answer).

**With AuthKit**, pass `ak.Authenticator()` and build roles from OpenRails'
permission constants, so a renamed one fails your build:

```go guide:embedded-roles
// OpenRails' permissions, by its own names. Held on root, they reach every
// merchant you serve.
perm := func(p billing.Permission) iam.Perm { return rbac.Root.Permission(p.Resource(), p.Action()) }
billingRead, billingManage := perm(billing.RootCustomerRead), perm(billing.RootCustomerManage)
catalogManage, configManage, metricsRead := perm(billing.RootCatalogManage), perm(billing.RootConfigManage), perm(billing.RootMetricsRead)

rbac.Root.Role("support", billingRead, billingManage)
rbac.Root.Role("operator", billingRead, billingManage, catalogManage, configManage, metricsRead)
```

A permission is `persona:resource:action`. Each staff or programmatic route
admits its `merchant:` name held in the bound merchant's scope (with AuthKit, the
group whose id is the merchant's) or its `root:` name in the root scope (every
merchant). Each route's pair is in the [route reference](api/routes.md); the
groups are in [client: credentials](client.md#credentials).

**Your own auth** implements the standard-library `authkit/auth` contracts and
runs no AuthKit ([`examples/own-auth`](../examples/own-auth/auth.go)):

- `Authenticate(r)` reads the credential live: `auth.ErrUnauthenticated` for none
  or unknown, `auth.ErrUnavailable` when your store is down.
- `Identity()` names your issuer, the user's UUID as subject (the customer id),
  the user as its own invoker, and the credential.
- `RootScope` is your all-merchants staff scope; a merchant's scope is the same
  authority with the merchant's id. `BoundScope` is the merchant the credential
  acts at, zero for none (and for a single merchant that declares your issuer).
- `Can(ctx, scope, permission)` answers exactly that permission in exactly that
  scope, live; `KnownPermission` knows `billing.Permissions()`.
- `openrailstest.CheckAuth(t, routes, cases)` checks your implementation against
  this contract in CI.

**Merchant binding.** A request acts at the merchant its credential is bound to,
never one the request names: its `BoundScope`, else the merchant whose `Issuer`
signed it, else the OpenRails object it opens (a pay URL, a webhook signature).
A valid credential bound to no live merchant is `403 merchant_unresolved`. With
one merchant, declare your issuer; with several, mint sessions bound to the store
(AuthKit's `MintAccessToken` with `Group`, or your own `BoundScope`).

## Calling the engine

`Engine.Merchant(id)` returns the merchant's `*Client`; `Engine.DeclaredMerchant(slug)`
gives a declared merchant's id. The handle acts as the host application at that
merchant and checks no permission: authorize your own request first. It reads
the directory on each call, so a merchant provisioned while running is served at
once and a deleted one is `merchant_unresolved`.

- `Client.ActingFor(invoker)` returns a handle whose calls act for a signed-in
  person: their destructive-action limits apply and the provider records name
  them.
- `Client.Uncached()` returns a handle that reads catalog, configuration and
  entitlements live.
- `NewGate(client, deps)` admits usage in process over the same handle
  ([the Gate](client.md#limits-admissions-and-the-gate)).

`Engine.LocalMerchant(id)` returns a `*LocalClient` for what only the engine in
your process can do: commit with your own rows. `AdmitTx`,
`CaptureAdmissionsTx`, `ReleaseAdmissionsTx`, `StartRunningChargeTx` and
`StopRunningChargeTx` take a `pgx.Tx` from your pool, so a crash leaves both
your row and the billing change, or neither:

```go guide:embedded-tx
local, err := bill.LocalMerchant(merchant)
if err != nil {
	return err
}
tx, err := pool.Begin(ctx)
if err != nil {
	return err
}
defer func() { _ = tx.Rollback(ctx) }()
verdicts, err := local.AdmitTx(ctx, tx, []billing.AdmitParams{{
	RequestID: jobID, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
	Usage:     &billing.UsageEstimate{EventType: "render", Dimensions: map[string]int64{"frames": 1200}},
	ExpiresAt: new(time.Now().Add(time.Hour)),
}})
if err != nil {
	return err
}
if !verdicts[0].Allowed() {
	return errors.New("refused") // verdicts[0] says why
}
if _, err := tx.Exec(ctx, `INSERT INTO app.jobs (id, customer_id) VALUES ($1, $2)`, jobID, customer); err != nil {
	return err
}
return tx.Commit(ctx)
```

`LocalClient.Client()` returns the merchant's ordinary handle.

### Account inbox

`Deps.Notifications` publishes selected customer notices to an AuthKit account
inbox in the billing transaction: unexpected payment failures, required card
updates or authentication, stopped collection, access loss and applied
downgrades, each withdrawn when resolved. The `adapters/authkit` module's
`Notifications(publisher, merchantID, issuer)` builds the channel from AuthKit's
producer. To adopt an existing inbox, stop billing writers, call
`LocalClient.ExportNotificationAdoption` and pass its adoptions to AuthKit's
`AdoptNotifications`; after a billing-archive restore, call
`RestoreNotificationOrdering` with the matching snapshot before enabling delivery.

## Browser configuration

billing-ui reads `GET /v1/config` (public) and `GET /v1/me/config` (the bound
merchant's name and payment setup). A page your server renders can carry both
in its own response, so the browser makes neither request to load:
`Engine.PublicConfig(routes)` and `Engine.CustomerConfig(routes, r)` with the
`Routes` you mount and your own signed-in request, passed to `BillingProvider`
as `config` and `customerConfig`.

## Host customer APIs

A host that keeps its own account relationships (a hosted product linking its
accounts to merchants' customers) reuses OpenRails' customer checks without
OpenRails storing the relationship:

- `Engine.ResolveCustomer(ctx, authority, verified)` returns the `CustomerProof`
  (merchant, customer, stored issuer) the original request proves, binding the
  merchant exactly as `/v1/me` does. It authenticates nothing again, admits only
  a user acting in person and changes nothing; a missing customer is
  `billing.ErrNotFound`.
- `Engine.CustomerRoutes(selection)` returns customer operations under its
  `Prefix`: `Auth` verifies once, and `Resolve` maps the request to one existing
  `CustomerBinding` (return `billing.ErrNotFound` to hide a relationship).
  Covered: config, payments, entitlements, orders (list, detail, payment
  attempt, pay, confirm), subscriptions (list, detail, cancel, resume, payment
  method), payment methods and agreements. Every mutation asks for a recent
  sign-in (`step_up_required`, RFC 9470); order creation is not mapped.

## Checkout page

A pay URL is a page on an origin of its own where the customer pays one order:
your backend creates the order with `pay_url` and redirects the customer
([orders and checkout](client.md#orders-and-checkout)). The origin must differ
from the API's, your app's, the frontend's and every declared issuer's: the page
runs the PSPs' scripts and never shares an origin with them. Without it,
an order asking for a pay URL is `400 pay_url_unavailable`.

Set `Config.CheckoutOrigin` and `Deps.CheckoutAssets`, then route that origin's
requests to `Engine.CheckoutPage()`'s handler ahead of everything else. With Gin:

```go guide:embedded-checkout
origin, page, err := bill.CheckoutPage("https://pay.example.com", assets)
if err != nil {
	return err
}
pay, err := openrailsgin.CheckoutOrigin(origin, page)
if err != nil {
	return err
}
router.Use(pay) // before other middleware and routes
router.Use(gin.LoggerWithFormatter(openrailsgin.RedactLog(format)))
```

`openrailsgin.CheckoutOrigin` sends a request whose `Host` is the checkout origin
to the page alone, so no app, sign-in or admin route answers there; forwarded
headers never select it. `RedactLog` logs those requests without query strings
or error details, and `PaymentRequest(c)` tells your own loggers and panic
recovery the same. Build the assets from the pinned module with
`scripts/build-payment-page.sh`. Two apps sharing one billing database: the one
that does not serve the page sets `CheckoutOrigin` with no assets, only to issue
pay URLs.

## Testing

- `openrailstest/nmimock` and `openrailstest/stripemock` are provider fakes
  behind `Deps.NMITransport` and `Deps.StripeTransport`; `openrailstest.UserInfo`
  is an in-memory directory. Test seams are refused beside a live PSP.
- `openrailstest.CheckAuth` checks your `Authenticator`.
- Run your tests against a real PostgreSQL 18: `New` migrates a fresh schema in
  seconds.
