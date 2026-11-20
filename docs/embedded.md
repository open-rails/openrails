# Embedding OpenRails

Running the billing engine inside your Go server. [`examples/embedded`](../examples/embedded)
is the runnable quickstart. Your binary imports the engine: no second service, no
network hop, no second credential.

- It owns a schema (default `billing`) and its River schema in **your** Postgres.
- Its routes mount on **your** router under a prefix; users call them with their
  normal credential.
- `openrails.New` returns the `*Engine` (`Start`, `Close`, `Routes`, `RiverJobs`,
  `Ready`, `Probes`). A merchant's operations are on its handle, the `*Client`
  from `MerchantBySlug` or `Merchant(id)`: the same type a remote consumer gets
  from `openrails.NewRemote`, joined to the same handlers by an in-process
  transport instead of a socket. Holding the engine is the authority.

```bash
go get github.com/open-rails/openrails
```

| Package | Holds |
|---|---|
| `openrails` | `New`, `*Engine`, `NewRemote`, `*Client`, `Config`, `Deps` |
| `billing` | request and response types, typed IDs, errors and codes |
| `catalog` | the catalog document and its charge models |
| `adapters/http`, `adapters/gin`, `adapters/fiber` | mount the selected routes on your router |

The module imports only AuthKit's standard-library contracts (`authkit/auth`,
`authkit/userinfo`); you implement them or opt into AuthKit.

## Config and Deps

`openrails.Config` is plain data (with `yaml`/`json` keys, so you can decode your
own file into it); `openrails.Deps` is everything outside the process.

| Config | Meaning |
|---|---|
| `ProviderWriteMode` | required: `ProviderWritesFull`, `ProviderWritesLimited` or `ProviderWritesReadOnly` ([safety levers](operations.md#safety-levers)) |
| `Database` | `Schema` (default `billing`) and `RiverSchema` (default schema + `_river`) |
| `Merchants` | the merchants this engine declares, by slug (below) |
| `Vault` | the Vault connection; its `KVMount` makes Vault the merchant configuration's home |
| `PublicBillingBaseURL` | the external billing mount without `/v1`, for callbacks and links |
| `SMTP` | a built-in sender (`Host`, `Port`: 465 implicit TLS, else STARTTLS, 0 is 587; `Username`, `Password`, `From`); billing mail uses the merchant's `from_email` |
| `UsageBillingEnabled` | [automatic usage billing](usage-billing.md#automatic-usage-billing) |
| `RateLimits`, `RateLimitsDisabled`, `Captcha`, `TrustedProxies`, `CloudflareProxies`, `ReadCache` | [limits](limits.md), [auth](auth.md#sessions-and-proxies), [read cache](catalog.md#through-the-client) |
| `Solana` | `Crankers` for Solana rebilling ([Solana](rails/solana.md)) |
| `EngineAdmissionHold`, `EventRetention`, `DestructiveActionLimits`, `UserActionRateLimit`, `ReturnOrigins`, `LLM`, … | the same keys as the server's [`config.example.yaml`](../config.example.yaml) |

| Deps | Meaning |
|---|---|
| `Postgres` | Your pool. A nil value opens a pool from `Config.DB`. Idempotency lease renewals use a separate pool with the main pool's connection settings and a cap of `Config.DB.LeaseConns` (default 4). Include this cap in the host's connection budget. |
| `Redis` | abuse state shared across instances; without it, one instance only |
| `Vault` | a borrowed Vault client instead of `Config.Vault` |
| `Email` | your own sender, replacing `Config.SMTP` |
| `UserInfo` | your directory (`ak.UserInfo()` or your own): customer contacts and staff search ([customer contacts](customer-contacts.md)) |
| `Notifications` | an [account inbox channel](events.md#account-inbox-channel) |
| `StripeTransport`, `NMITransport`, `BasisTheoryTransport`, `FXTransport`, `Clock` | test seams, refused beside a live PSP |

`Deps` holds no auth: your auth guards the routes you mount.

## Boot and River

```go
bill, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool, UserInfo: ak.UserInfo()})
if err != nil { return err }
defer bill.Close(context.WithoutCancel(ctx))
shop, err := bill.MerchantBySlug("myapp")
if err != nil { return err }
if err := bill.Start(ctx); err != nil { return err } // River workers: renewals, dunning, provider operations
```

`New` applies OpenRails' migrations through your pool first (one replica at a
time, under an advisory lock; a failed migration fails `New`), creates the River
schema and this month's partitions, starts no workers and never changes the
catalog. The pool's role owns every object; for another owner, `SET ROLE` on its
connections. An older build boots on a schema a newer one migrated. `New` fails
only on what it cannot serve without: Postgres and migrations, an unset write
mode, an invalid River schema, a test seam beside a live PSP. Anything else that
cannot be served is refused alone and the rest serves: an optional setting is
turned off, a merchant with an invalid slug or document is not provisioned, a PSP
that does not check is not armed. Vault, PSP posture and Redis reconnect in the
background. Register the probes with your supervisor; `openrails_configuration`
names what is not served and why:

```go
for _, p := range bill.Probes() { // openrails_vault, openrails_psp_posture, openrails_configuration, openrails_job_progress, …
    sup.Add(p.Name, deps.Optional, p.Check, nil)
}
```

River is required. Jobs queue from `New` on; `Start` runs them (`Ready` fails until
it does). A host with its own River fleet adds OpenRails' jobs to it, in
`Config.Database.RiverSchema`, and passes it to `Start`:

```go
fleet, err := riverhelpers.New(ctx, pool, &river.Config{
    Schema: "billing_river",
    Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 10}},
}, auth.RiverJobs(), bill.RiverJobs())
if err != nil { return err }
if err := fleet.Start(ctx); err != nil { return err }
defer fleet.StopAndCancel(context.WithoutCancel(ctx))
if err := bill.Start(ctx, openrails.WithRiverClient(fleet)); err != nil { return err }
```

`RiverJobs` adds `openrails.QueueBilling` and `QueuePSPImport` (2 workers) if
missing and sets `RescueStuckJobsAfter` to 10 minutes when unset; each OpenRails
worker declares its own timeout. OpenRails never starts or stops your fleet. A
fleet in another schema, `Start` without `WithRiverClient` after `RiverJobs` went
to a fleet, and a second `Start` are refused. `Close` stops what `Start` started;
close the engine before the pool. AuthKit joins the same fleet when its
`Database.RiverSchema` matches.

**Several hosts, one database.** Hosts embedding OpenRails on one database declare
the same merchants, each with its own `RiverSchema`, and share periodic passes
through leases in `periodic_leases` (held for four fifths of the period; a stopped
fleet's lease expires). Give them one Redis and clocks within five minutes. Work a
request starts runs on the engine that received it. [Several instances](operations.md#several-instances)
covers replicas of one host.

## Declaring the merchant

Each `Config.Merchants` entry is created if missing and reconciled on every boot,
before routes or workers see it. Set `Issuer` in code (never a file): your
AuthKit's issuer makes its users the merchant's customers and staff, and one issuer
belongs to one merchant.

```go
merchant, err := openrails.ReadMerchantFile("merchant.yaml") // strict: unknown fields refused
if err != nil { return err }
merchant.Issuer = "https://shop.example"
cfg.Merchants = map[string]openrails.MerchantDeclaration{merchant.Slug: merchant}
```

Or build it in Go, where each rail's slots are fields so a typo fails to compile:

```go
cfg.Merchants = map[string]openrails.MerchantDeclaration{"myapp": {
    DisplayName: "My App",
    MerchantSettings: billing.MerchantSettings{
        Profile: &billing.MerchantProfile{FromEmail: "billing@myapp.example", SupportURL: "https://myapp.example/support"},
    },
    PSPs: openrails.PSPs{
        "nmi-sandbox": openrails.NMIPSP{
            Environment:          openrails.EnvironmentSandbox,
            GatewayID:            "000000",
            TokenizationKey:      "placeholder-tokenization-key",
            SecurityKey:          "placeholder-security-key",
            WebhookSigningSecret: "placeholder-webhook-secret",
        }.PSPConfig(),
    },
}}
```

The file names its merchant with a top-level `slug:` and holds PSP secrets, so
keep it out of version control. Fields are the [merchant configuration](merchant-configuration.md)'s
(`StripePSP`, `CCBillPSP`, `SolanaPSP` likewise); a field outside its rail, or a
PSP missing a required secret, is refused or left unarmed, naming it. Declared in
`Config.Merchants` the configuration is read-only: change it and construct a new
engine. With `Config.Vault.KVMount` the declaration names only the merchant and
its `Issuer`, and the configuration methods edit Vault. Imported billing facts
for a PSP without credentials need `LocalClient.DeclarePSP` first.

Applying the catalog is your own command, never a boot step:

```go
func applyCatalog(ctx context.Context, shop *openrails.Client, path string) error {
    doc, err := catalog.ReadFile(path)
    if err != nil { return err }
    receipt, err := shop.ApplyCatalog(ctx, doc, billing.ApplyCatalogParams{})
    if err != nil { return err }
    return openrails.WriteCatalogReceipt(os.Stdout, receipt)
}
```

See [catalog](catalog.md) for documents, edits and conflicts.

## Mounting routes

```go
routes := openrails.Routes{
    Auth:   ak.Authenticator(), // required: says who a request is
    Prefix: "/billing",         // serves /billing/v1/*
    RouteGroups: openrails.RouteGroups{Admin: true, Metrics: true},
}
if err := openrailsgin.Mount(r, bill, routes); err != nil { return err } // or openrailshttp, openrailsfiber
```

| Selection | Serves |
|---|---|
| always | `GET /v1/config`, Solana Pay, customer routes `/v1/me/*`, provider webhooks |
| `RouteGroups.Admin` | customer support `/v1/admin/*` |
| `RouteGroups.Catalog` | `/v1/catalog/*`, price migrations included |
| `RouteGroups.MerchantConfig` | `/v1/merchant/*`: PSPs, imports, findings, proposals, settings, billing import and export |
| `RouteGroups.Metrics` | `/v1/metrics/*` |
| `RouteGroups.Programmatic` | `/v1/app/*`: entitlement checks, usage, admissions, running charges, events, orders, SCIM; applications only |

Each route checks its own permission; you only grant OpenRails' permission names
to your roles ([auth](auth.md#permissions)). A mount without `Auth` fails before
registering anything. Customer routes act only on the admitted customer: another
customer's resource reads like a missing one, and ambient cookies never reach your
`Auth`. Adapters register ordinary method and path routes, and pass original URLs
and bodies to authentication and webhook verification. A host serving the checkout
origin puts `openrailsgin.CheckoutOrigin` first and logs through
`openrailsgin.RedactLog` ([payment page](payment-page.md#embedded-applications)).
The embedded engine serves no console: your admin pages call the staff groups
([billing admin](billing-admin.md)).

## Calling the engine

The handle's methods, by job:

| Group | Methods |
|---|---|
| Admission | `Admit`, `CaptureAdmissions`, `ReleaseAdmissions`, `ExtendAdmissions` |
| Usage | `RecordUsage`, `QueryMetrics` |
| Customers and access | `ListCustomers`, `GetCustomer`, `UpdateCustomer`, `CheckEntitlements`, `ListEntitlements`, `ListProductAccess`, `CreateProductAccess`, `RevokeProductAccess` |
| Credits | `CreateCreditGrants`, `ListCreditGrants`, `RevokeCreditGrant`, `ListBalanceTransactions`, `GetBalance` |
| Catalog | `ApplyCatalog`, products, prices, meters, price migrations |
| Subscriptions | `GetSubscription`, `ListSubscriptions`, `CancelSubscription`, `ResumeSubscription`, `ChangeSubscription`, `PreviewSubscriptionChange`, `SetSubscriptionPaymentMethod` |
| Orders and payments | `CreateOrder`, `PreviewOrder`, `CancelOrder`, `ListOrders`, `WaiveOrderBalance`, `MarkOrderUncollectible`, `GetPayment`, `RecordExternalPayment`, `RefundPayment`, `ListPaymentAttempts`, `ListRenewals`, `ListPaymentMethods` |
| Running charges | `StartRunningCharge`, `StopRunningCharge`, `GetRunningCharge`, `ListRunningCharges` |
| Configuration and events | `GetMerchantConfiguration`, `UpdateMerchantConfiguration`, `ListEvents`, `ImportBilling` |

```go
check, err := shop.CheckEntitlements(ctx, billing.CheckEntitlementsParams{
    CustomerID: customerID, Entitlements: []string{"course:101", "channel:membership"},
})
// check.Entitlements["course:101"] is true while held
```

A customer is your user's id; one who never touched billing holds nothing, never
an error. Deny verdicts are `(Allowed=false, nil)`. An in-process call runs as
long as its context allows. `Merchant(id)` reads the directory on each call, so a
merchant provisioned while running is served at once and a deleted one is
`403 merchant_unresolved`.

`engine.LocalMerchant(id)` returns a `*LocalClient` for what only the local engine
can do: `DeclarePSP`, `SCIMHandler`, and the transaction methods (`AdmitTx`,
`CaptureAdmissionsTx`, `ReleaseAdmissionsTx`, `StartRunningChargeTx`,
`StopRunningChargeTx`) that commit with your own rows. It embeds `*Client`.

A customer normally buys with their own credential (`POST /v1/me/orders`);
`CreateOrder` makes an unpaid order for them that only they pay
([frontend](frontend.md#orders)). Customer contacts come from `Deps.UserInfo`, or
from your directory's SCIM pushes to `{Prefix}/v1/app/scim/v2` (or
`local.SCIMHandler()` in the same binary); `openrailstest.UserInfo` is an in-memory
one for tests.

## Webhooks and testing

Point each PSP's webhooks at `{Prefix}/v1/webhooks/{rail}/{account_id}` on your
server ([conventions](api/conventions.md#provider-webhooks)); locally, through a
tunnel ([local webhooks](dev/local-webhooks.md)). In end-to-end tests,
`openrailstest/nmimock` and `openrailstest/stripemock` stand in for NMI and Stripe
on loopback. Delinquency, balance and payment notices reach your app through the
[event log](events.md). A product serving many merchants with OpenRails' own
accounts builds on the [server package](server-package.md) instead.
