# Embedding OpenRails

Embed OpenRails when your Go application should run billing in the same process.
Your app owns sign-in, the HTTP server and shutdown. OpenRails uses PostgreSQL
18 or newer and manages its billing and River schemas. Your backend calls a
merchant-bound `*openrails.Client` without a network hop. To run billing in a
separate service, see [self-hosted OpenRails](standalone.md).

## Quickstart

Install the Go module from your application directory.

```sh
go get github.com/open-rails/openrails@v0.282.0
```

In your app's startup function, use your existing PostgreSQL pool (`pool`),
root HTTP router (`mux`) and initialized AuthKit client (`ak`). Set `issuer` to
the exact issuer your AuthKit uses. Import `github.com/open-rails/openrails`
and `github.com/open-rails/openrails/adapters/http` as `openrailshttp`.

```go
const merchantSlug = "myapp"

bill, err := openrails.New(ctx, openrails.Config{
	ProviderWriteMode: openrails.ProviderWritesReadOnly,
	Merchants: map[string]openrails.MerchantDeclaration{
		merchantSlug: {DisplayName: "My App", Issuer: issuer},
	},
}, openrails.Deps{Postgres: pool, UserInfo: ak.UserInfo()})
if err != nil {
	return err
}
defer bill.Close(context.WithoutCancel(ctx))

shop, err := bill.MerchantBySlug(merchantSlug)
if err != nil {
	return err
}
if err := bill.Start(ctx); err != nil {
	return err
}
if err := openrailshttp.Mount(mux, bill, openrails.Routes{
	Prefix: "/billing",
	Auth:   ak.Authenticator(),
}); err != nil {
	return err
}
if err := bill.Ready(ctx); err != nil {
	return err
}
log.Printf("OpenRails ready for merchant %s", shop.MerchantID())
```

Keep the engine open while your HTTP server runs. Use `Close` during host
shutdown before closing the PostgreSQL pool. Keep `shop` in your backend to
[call the engine](#calling-the-engine). This setup checks the integration
before adding payment credentials. For a complete app with AuthKit, Gin and a
browser checkout, see [the embedded example](../examples/embedded).

Once your app is listening, check the public mount and then the customer route
with an access token from signing in to your own app.

```sh
curl --fail-with-body http://localhost:8080/billing/v1/config
curl --fail-with-body -H "Authorization: Bearer $USER_ACCESS_TOKEN" \
  http://localhost:8080/billing/v1/me/config
```

The first response describes the mounted capabilities. The second should name
`My App` and include the payment configuration, with no armed PSPs in this
setup. It exercises both the host authenticator and merchant binding.

## Config and Deps

Your app builds `openrails.Config` and supplies integrations through
`openrails.Deps`. Your app can decode `Config` from its own YAML or JSON.

| Field | What to provide |
|---|---|
| `Config.ProviderWriteMode` | The deployment's provider-write posture. The quickstart uses `ProviderWritesReadOnly`. See [safety levers](operations.md#safety-levers). |
| `Config.Database` | OpenRails' schema (`billing` by default) and River schema (defaults to the billing schema plus `_river`). |
| `Deps.Postgres` or `Config.DB` | Your pool or settings for an engine-owned pool. Include `Config.DB.LeaseConns` (default 4) in the connection budget for idempotency lease renewals. |
| `Config.PublicBillingBaseURL` | The external billing prefix without `/v1`, such as `https://shop.example/billing`, when using external callbacks or billing links. |
| `Deps.UserInfo` | Your customer directory, for example `ak.UserInfo()`. See [customer contacts](customer-contacts.md). |
| `Deps.Redis` or `Config.Redis` | Shared abuse state for deployments with multiple instances. See [requirements](operations.md#requirements). |
| `Deps.Vault` or `Config.Vault` | An existing Vault client or settings for connecting to Vault. See [Vault](vault.md). |
| `Deps.Email` or `Config.SMTP` | Your sender or SMTP settings for billing mail. For account inbox delivery, see [notifications](events.md#account-inbox-channel). |

The [configuration reference](configuration.md) covers schema and merchant
configuration. Set HTTP authentication separately through `Routes.Auth`. An
application using its own authentication can implement the `authkit/auth`
contracts. See [your own auth](auth.md#your-own-auth).

For PgBouncer transaction pooling, give `Postgres` the PgBouncer pool and set
`PostgresSession` to a `pgxpool.ParseConfig` result for the same database and
role through PostgreSQL's direct endpoint. On the PgBouncer pool, set
`poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe`
to retain PostgreSQL's parameter types without server-side prepared statements.
OpenRails scopes merchant identity to each transaction and
uses direct sessions for migrations, notification listeners, and session locks.
OpenRails' own River workers poll for jobs in this mode. For a host fleet on the
PgBouncer pool, set `river.Config.PollOnly` to `true` as well.

## Boot and River

`New` migrates the billing and River schemas under an advisory lock and
provisions declared merchants. The PostgreSQL role owns the objects it creates.
If you set `Config.Database.SkipMigrations`, run `openrails.Migrate` before
startup. `Start` runs background work, including billing jobs. Register
`Ready` with your host's readiness check and expose `Probes()` to report
optional dependency problems such as a PSP that could not arm. See
[health and operations](operations.md#health-and-observability).

If your app already operates a River fleet, register `bill.RiverJobs()` when
you build it in `Config.Database.RiverSchema`. Start the fleet first, then call
`bill.Start(ctx, openrails.WithRiverClient(fleet))`. OpenRails uses the supplied
fleet and leaves its lifecycle to your app. AuthKit can join the same fleet
when configured with the same River schema. See [River jobs](river-jobs.md)
and [several instances](operations.md#several-instances).
`RiverJobs` adds the `provider_refresh` queue (4 workers) beside the billing
and PSP-import queues. See the [provider refresh upgrade](operations.md#provider-refresh)
before replacing a fleet that scheduled refresh jobs.

Different hosts embedding OpenRails against one billing schema declare the
same merchants and use separate River schemas. They share Redis and coordinate
periodic billing work through PostgreSQL leases.

## Declaring the merchant

`Config.Merchants` is keyed by merchant slug. The quickstart declares the
merchant in Go. To read a merchant and its PSPs from a file, call
`openrails.ReadMerchantFile("merchant.yaml")`, set `merchant.Issuer` in your
host code, and assign the declaration under `merchant.Slug`. The file contains
a top-level `slug` and provider configuration. Store its secret values outside
version control. See [merchant configuration](merchant-configuration.md) and
[the example merchant file](../examples/embedded/merchant.example.yaml).

File-backed configuration is read at boot. To edit it at runtime, configure
`Config.Vault.KVMount` and declare only the merchant slug and issuer in
`Config.Merchants`. See [Vault](vault.md).

The catalog has its own lifecycle. Apply it explicitly with
`shop.ApplyCatalog(ctx, doc, billing.ApplyCatalogParams{})`, using a document
from `catalog.ReadFile`. See [catalog](catalog.md) and [the example's catalog
command](../examples/embedded/catalog.go).

## Mounting routes

Mount on the root router so the adapter applies `Prefix` once.
`adapters/http` supports `net/http` and Chi, with dedicated adapters for Gin
and Fiber. Public configuration, customer routes and provider webhooks are
available on the basic mount. Enable staff or programmatic routes with
`Routes.RouteGroups`.

| Group | Routes |
|---|---|
| `Admin` | `/v1/admin/*` for customer support |
| `Catalog` | `/v1/catalog/*` for catalog reads and edits |
| `MerchantConfig` | `/v1/merchant/*` for configuration and billing import/export |
| `Metrics` | `/v1/metrics/*` for business metrics |
| `Programmatic` | `/v1/app/*` for application credentials |

`Routes.Auth` verifies the requester. OpenRails binds the verified credential
to a merchant and checks the route's permissions. For one merchant, the host
issuer binds its user sessions to that merchant. With several merchants on
one issuer, issue sessions with an explicit verified merchant scope. Send
browser credentials in a header because billing routes strip ambient cookies.
Enable groups only after declaring their permissions in your auth setup. For
example roles, see [authentication and authority](auth.md#permissions) and
[the embedded app](../examples/embedded/main.go).

## Calling the engine

The in-process client acts as the host application at the selected merchant.
Keep it in trusted backend code and authorize the app request before using
it. `MerchantBySlug` resolves a declared merchant, and `Merchant(id)` creates
a handle for its UUID. If a call acts for a signed-in person, pass their verified
identity with `openrails.WithInvoker` so OpenRails applies their destructive
operation limits. See [destructive actions](destructive-actions.md).

For a first backend read, import `github.com/open-rails/openrails/billing` and
list the merchant's products. The first page is empty until you apply a catalog.

```go
products, err := shop.ListProducts(ctx, billing.ProductListParams{})
if err != nil {
	return err
}
log.Printf("OpenRails returned %d products", len(products.Items))
```

Use `engine.LocalMerchant(id)` for operations that need the local engine,
such as `AdmitTx` in your own PostgreSQL transaction, `DeclarePSP` for an
imported PSP without credentials, or `SCIMHandler` for directory pushes.
`*openrails.LocalClient` also holds the merchant's regular client. For buyer
flows, see [frontend](frontend.md).
`openrails.NewGate(shop, openrails.GateConfig{})` admits against usage limits in
your process over the same handle, as it does over a remote one
([the gate](usage-billing.md#the-gate)).

## Webhooks and testing

Point each PSP to `{Prefix}/v1/webhooks/{rail}/{account_id}` on your server.
For local testing, see [local webhooks](dev/local-webhooks.md). End-to-end
tests can use `openrailstest/nmimock`, `openrailstest/stripemock` and
`openrailstest.UserInfo`. Your app can consume billing events for receipts and
other notices. See [the event log](events.md) and [embedded payment pages](payment-page.md#embedded-applications).
