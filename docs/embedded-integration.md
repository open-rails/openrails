# Embedding OpenRails in your Go server

The full guide to running the OpenRails billing engine in-process;
`examples/embedded` is the runnable quickstart. Money is an integer in the
currency's native units (`GET /v1/currencies`; micros for USD), a decimal string
on the wire ([money-wire.md](money-wire.md)). Vocabulary: a
**rail** is a gateway kind (`nmi` / `ccbill` / `stripe` / `solana`); a **PSP** is your
concrete account on a rail (e.g. `mobius` on nmi).

### 1. What embedding means

Your Go binary imports the engine and runs it in-process: no second service, no
network hop, no second credential. Concretely:

- The engine owns a configurable schema, defaulting to `billing`, inside **your** Postgres database.
- Its HTTP routes mount on **your** router under a prefix you choose; your users call
  them with their normal session credential.
- `openrails.New` returns the **same** `*openrails.Client` a standalone consumer
  gets from `openrails.NewRemote`. Parity is structural: one client
  implementation, one handler surface, joined by an in-process
  `http.RoundTripper` instead of a socket. The embedded Client adds the hosting
  operations: `Start`, `Close`, `Routes`, `RiverJobs`, `Ready` and `Probes`.
- An engine with `Config.Merchant` serves that merchant. One without a declared
  merchant (`examples/multimerchant`) serves many: `WithDefaultMerchant`
  supplies an immutable default, and each operation can select a slug with
  `WithMerchant` or a stable ID with `ForMerchantID`. Selection never grants
  authority or overrides the declared merchant.

```mermaid
flowchart LR
    B[Browser] -- your session credential --> S[Your Go server]
    subgraph P[Your process]
        S -- Routes.Auth --> OR[OpenRails engine]
        C[Your backend code] -- openrails.Client --> OR
    end
    OR --> PG[(Postgres, billing schema)]
    R[Stripe / NMI / CCBill / Solana] -- webhooks --> S
```

### 2. Install and migrations

```bash
go get github.com/open-rails/openrails
```

A host imports four kinds of package; everything else is `internal/`:

| Package | Holds |
|---|---|
| `openrails` | `New`, `NewRemote`, the `*Client`, `Config`, `Deps` |
| `billing` | request/response types, IDs (`billing.MerchantID`, `billing.ParseMerchantID`), errors and codes, permission names (`billing.MerchantAll`) |
| `catalog` | the catalog document (`catalog.Application`) and its charge models (rate cards, meters, prices) |
| `adapters/http`, `adapters/gin`, `adapters/fiber` | mount the routes an `openrails.Routes` selects on your router |

OpenRails owns its migrations: `New` applies them through your pool before
anything else touches the database. The pool's role owns the objects and is
the role OpenRails runs as (no grants); for another owner, have the pool's
connections `SET ROLE` to it.

`New` creates or upgrades OpenRails' tables in `cfg.Database.Schema`, River's
in `cfg.Database.RiverSchema` (`billing_river` for the default schema),
whichever fleet `Start` will run there, and this month's partitions. A host's
own AuthKit migrates itself in `authkit.New`. Replicas booting together take
turns on an advisory lock, and a migration that fails fails `New`, so new pods
crash-loop while the old ones keep serving. An older build boots against a
schema a newer one already migrated: migrations it does not know are left as
they are.

Billing, AuthKit, application tables and River may share `public` or another
namespace. OpenRails archives contain only billing-owned tables and never include
live River jobs, AuthKit identities or host records. `New` refuses to start
when an applied migration's file changed and the live schema no longer
matches a fresh build of it.

Merchant isolation uses verified application scope, explicit SQL predicates and
composite relationships, not PostgreSQL RLS. OpenRails creates no roles and
issues no grants.

### 3. Config and Deps

`openrails.Config` is plain data; `openrails.Deps` is everything the engine
reaches outside its process. Construction refuses to boot unless posture is
explicit:

| Config field | Required | Meaning |
|---|---|---|
| `TestMode` | yes | `openrails.Sandbox` or `openrails.Live`. The zero value is refused; it never silently means live. |
| `ProviderWriteMode` | yes | `ProviderWritesFull`, `ProviderWritesLimited` (renewals and retries wait) or `ProviderWritesReadOnly` (never charges). |
| `Database` | default `billing` | `Schema`: the Postgres schema of OpenRails' tables. `RiverSchema` (default `Schema` + `_river`): where OpenRails' River jobs live; `Start` runs them there, on its own client or the host's fleet (section 4). `New` creates or upgrades both. |
| `Merchant` | no | The merchant this engine serves (section 5). |
| `Catalog` | no | Optional startup batch, equivalent to calling `ApplyCatalog` once (section 5). Requires `Merchant`. |
| `Checkout` | no | The shared payment page (`PageURL`, `EmbedOrigins`) when several sites sell through one (section 6). |
| `SecretBackend` | default `snapshot` | Credential custody: host snapshot, Vault or encrypted database. |
| `PublicBillingBaseURL` | for callbacks and links | External billing mount, excluding `/v1`. |
| `SendGrid` | no | The built-in email sender (`APIKey`, the deployment's `From`). Billing mail is sent from the merchant's profile `from_email` when it has one. Without it or `Deps.Email`, OpenRails sends no email. |

| Deps field | Meaning |
|---|---|
| `Postgres` | Your pool. Nil opens one from `Config.DB`. |
| `Redis` | Optional shared rate limits, FX rates and abuse statistics. |
| `Vault` | A borrowed Vault client. PSP secrets come from `Config.Merchant`'s PSPs or the secret store. |
| `ConsoleAssets` | A host-built admin console, which `Routes.AdminConsole` serves (section 6). |
| `Email` | Your own sender for OpenRails' rendered email; replaces `Config.SendGrid` (set one). An empty `From` is the deployment's own mail. |
| `StripeTransport`, `NMITransport`, `DNSResolver`, `Clock` | Test seams, refused with `TestMode` live. |

`Deps` holds no auth: the engine authenticates nobody. Your auth guards the
routes you mount (section 6), and what OpenRails knows about a customer is what
you tell it with `client.EnsureCustomers`: the email receipts and notices go to,
the username the CCBill username bridge matches, and whether the customer is
blocked (banned or deleted), which checkout sessions refuse. Call it whenever
they change.

Under `Sandbox` every rail routes to its test environment and live credentials
refuse to boot. NMI accounts get an arm-time probe that refuses a conclusively
live gateway. See [operations.md](operations.md).

**Rate limiting is on by default**: a nil `RateLimits` gets the built-in limits
on checkout, card and subscription writes and webhooks (tight on checkout to
deter card testing; shared across replicas with `Deps.Redis`). Other routes are
not limited: a per-address ceiling belongs to your proxy. Override the limits,
or set `RateLimitsDisabled` if your own gateway fronts billing. See
[rate-limiting.md](rate-limiting.md).

### 4. Boot, lifecycle and River

```go
client, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool})
if err != nil { return err }
defer client.Close(ctx)
```

Only Postgres (including the migrations `New` applies first) and a refused
`Config.Catalog` can fail `New`; it starts no workers. Vault login, PSP posture checks and Redis reconnect in the background
(capped full-jitter backoff, forever); until then only their features answer
503. `Ready` is the readiness check (Postgres, the merchant directory, the
declared catalog, River). Register `Probes` with the host's
`github.com/open-rails/helpers/deps` supervisor:

```go
for _, p := range client.Probes() { // openrails_vault, openrails_psp_posture, openrails_job_progress, ...
    sup.Add(p.Name, deps.Optional, p.Check, nil)
}
```

River is required: renewals, dunning, invoices, reconciliation and provider
intents are River jobs. Jobs queue in `Config.Database.RiverSchema` from `New`
on and wait there; `Start` runs them, and `Ready` fails until it does. Without
options `Start` builds and runs OpenRails' own River client there:

```go
if err := client.Start(ctx); err != nil { return err }
```

A host that already runs a River fleet adds OpenRails' jobs to it, beside its
own and AuthKit's, and passes the fleet to `Start`. The fleet must use
`Config.Database.RiverSchema`, where `New` created River's tables:

```go
workers, err := riverhelpers.New(ctx, pool, &river.Config{
    Schema: "billing_river", // Config.Database.RiverSchema
    Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 10}},
}, auth.RiverJobs(), client.RiverJobs())
if err != nil { return err }
if err := workers.Start(ctx); err != nil { return err }
defer workers.StopAndCancel(context.WithoutCancel(ctx))
if err := client.Start(ctx, openrails.WithRiverClient(workers)); err != nil { return err }
```

OpenRails enqueues through the host's fleet and never starts or stops it.
Composing `RiverJobs` into a fleet in another schema is refused. Once
`RiverJobs` went to a fleet, `Start` without `WithRiverClient` is refused, and
`WithRiverClient` refuses a fleet built without this client's `RiverJobs`; a
second `Start` is refused too. Cancelling `Start`'s context stops nothing:
`Close` stops what `Start` started; close the client before the pool. OpenRails
also watches the fleet from outside River: `openrails_job_progress` fails while
it is stalled.

**Inserting an engine job.** The one job a host inserts itself is the invoice
sweep: `workers.Insert(ctx, openrails.InvoiceSweepArgs{FinalizePreviousMonth: true}, nil)`
finalizes the previous period of every customer active in it now; `Collect: true` runs the
collection pass. Runs are idempotent.

**No job clock.** `river.Config.JobTimeout` does not apply to OpenRails'
workers: each declares `Timeout() = -1` and ends on observed lack of progress
(3x its cadence, floored at 30 min). A running job beats
`river_job.attempted_at`, so `RescueStuckJobsAfter` measures silence from a dead
process, never the age of a live job.

### 5. Declaring the merchant

`Config.Merchant` is create-if-missing, reconcile-if-present on every boot,
before routes or workers can observe it. The fields are the merchant manifest's
(same shape as `config/merchants_config.example.yaml`). `Settings` is the
`billing.MerchantSettings` document the configuration API reads and applies:
one shape, one validator, in YAML and over the API.

```go
cfg.Merchant = openrails.MerchantDeclaration{
    Slug:        "myapp",
    DisplayName: "My App",
    Settings: billing.MerchantSettings{
        Profile:                &billing.MerchantProfile{FromEmail: "billing@myapp.example", SupportURL: "https://myapp.example/support"},
        InvoiceBillingBoundary: "calendar_month",
    },
    PSPs: map[string]openrails.PSPConfig{ // PSP key -> account
        "my-nmi-sandbox": openrails.NMIPSP{
            AccountID:            "000000", // NMI dashboard "Gateway ID"
            TokenizationKey:      "placeholder-tokenization-key",
            SecurityKey:          "placeholder-security-key",
            WebhookSigningSecret: "placeholder-webhook-secret",
        }.PSPConfig(),
    },
}
```

`client.MerchantID()` is the declared merchant's ID. A host builds the
declaration from its own configuration (koanf, kong, flags), or keeps it in a
YAML file: `openrails.ReadMerchantFile(path)` reads one merchant strictly
(unknown fields refused; `openrails.ParseMerchantDeclaration` takes the bytes).
The file names its merchant with a required top-level `slug:`; a standalone
manifest keys each merchant by slug instead and refuses the field inside an
entry. `MerchantDeclaration` and `PSPConfig` carry `yaml` tags, so they
can also sit inside the host's own YAML config. The file holds PSP secrets: keep
it out of version control.

In Go, `openrails.NMIPSP`, `StripePSP`, `CCBillPSP` and `SolanaPSP` name each
rail's slots as fields, so a misspelled one fails to compile; `PSPConfig()`
returns the declaration, and an empty field is an omitted key. A PSP missing a
required secret is declared but not armed, in Go as in YAML.

| Rail | Secrets (required first) | Settings |
| --- | --- | --- |
| `stripe` | `secret_key`, `webhook_signing_secret`; `webhook_signing_secret_thin`, `webhook_signing_secret_previous` | `publishable_key`, `webhook_overlap_expires_at` |
| `nmi` | `security_key`, `webhook_signing_secret`; `webhook_signing_secret_previous` | `tokenization_key`, `tokenization_url`, `endpoint_deployment`, `card_entry`, `webhook_overlap_expires_at` |
| `ccbill` | `salt`; `datalink_username`, `datalink_password` | |
| `solana` | `private_key` (or `signer: {mode: vault_transit, key: …}`) | `rpc_provider`, `rpc_api_key`, `tokens`, `recipient_wallet` |

The database owns merchant metadata. Startup initializes missing metadata and
reloads snapshot credentials without overwriting later API edits or reviving
archived accounts. Deliberate metadata changes use
`Client.ApplyMerchantConfiguration` with a stable application ID and reviewed
revision. Imported billing facts attributed to a PSP without credentials need
its identity first: `client.DeclarePSP(ctx, merchantID, billing.PSPDeclaration{...})`
during setup.

`SecretBackend` selects only credential custody. Snapshot values stay in memory;
managed PSP credentials are published through `Client.CreatePSP`/`UpdatePSP`
with an operation ID and the expected PSP revision. `Routes.Merchant` publishes
these routes with the rest of the merchant API, each gated by its permission.

**Startup catalog batch:** read a YAML or JSON document with `catalog.ReadFile`
and set it as `Config.Catalog`; `New` applies it, like calling
`client.ApplyCatalog(ctx, document)` once. Leave `Routes.CatalogEdits` off to
omit catalog-write HTTP routes; the in-process client still edits the catalog.
YAML batches and individual client/API edits operate on the same merchant
catalog.

The server computes a canonical content hash and commits it with the batch. The
same document replays forever, even after intervening API edits or an archive
restore. Concurrent applications of identical content commit once. Failed batches
are not marked applied. No caller `application_id`, `expected_revision`, or
`catalog_version` is needed or accepted. Hashes deduplicate; they do not order
previously unseen batches. Intentionally restoring old state requires a new
operation rather than replaying an already-applied document.

Omitted products, prices and fields keep their stored values. Use `archived: true`
to retire an entry and `archived: false` to restore it. `prune: true` explicitly
opts into archiving omitted products/prices; no catalog operation deletes them.
The apply makes no provider writes. It may read a provider to verify an explicit
reference. `Config.Catalog` can finish a temporarily unavailable provider check in
the background, with `Ready` failing until that startup batch succeeds.

**Catalog authoring**: storage is always the database.
The in-process Client is the process owner and writes its catalog directly
(`ApplyCatalog`, or `CreateProduct`, `CreatePrice`, `UpdatePrice`);
`Routes.CatalogEdits` only publishes catalog mutations to HTTP and delegated
callers. YAML is decoded into the same `catalog.Application` as JSON
(`catalog.ReadFile`, `catalog.ParseApplicationYAML`).
Omitted records survive by default; explicit `archived: true` retires a known
record, and `prune: true` archives omitted products and prices. Price keys are unique within their product. Key lookups and checkout selections
therefore carry both `product_key` and `price_key`; price IDs select one immutable
record directly. Each product/key chain has automatic price revisions starting at
zero. New financial terms create a new revision; repeating or reactivating old
terms reuses their original ID and revision. Keys, terms and product association
are immutable; products and prices can be archived but never deleted. Changing a
price never silently reprices existing subscriptions. For dynamic products with host-owned Stripe credentials use
`SecretBackend: openrails.SecretBackendSnapshot` with the account in
`Config.Merchant`; rotate by updating configuration and constructing a new engine.

### 6. Authentication and HTTP

OpenRails has no logins of its own. You supply your auth only when you mount
routes, as `Routes.Auth`: net/http middleware in AuthKit's own shape.

| Method | OpenRails stacks it on |
|---|---|
| `Required()` | every customer route: a signed-in request |
| `RequirePermission(permission)` | every merchant route, with the route's exact merchant permission (never a glob), checked live on the mounted merchant; it authenticates the request itself |
| `Sensitive()` | after `RequirePermission`, when a user in person calls a merchant route whose permission moves money, removes access or exports data (`billing.RequiresRecentSignIn`): a recent sign-in, by your policy. An API key or application has no sign-in to renew |
| `Identity(ctx)` | read after them: who was admitted |

Refusals are your middleware's own responses. After them OpenRails reads
`Identity` and refuses a request it finds none on, so a middleware that checks
nothing admits no one. Each handler checks the identity again before it runs.

`Identity` returns an `openrails.Identity` in three parts:

- `Subject` (with `SubjectKind`, `openrails.SubjectUser` or
  `openrails.SubjectApplication`) is the native account acted as. On a
  customer route it is the customer: a canonical UUID, the same user whatever
  credential they signed in with. A merchant permission is the subject's.
- `Invoker` is the party actually acting: `{Issuer, Subject}` when the subject
  acts itself, else whoever acts on its behalf, possibly another issuer's user
  spending the subject's balance. Spend limits and staff rate limits key on
  it (`issuer|id` when another issuer vouches for it). An invoker acting for
  someone else, or an application subject, may only read its own
  `/v1/me/spend-limits`.
- `Credential` is how it was proven: `openrails.CredentialSession`,
  `CredentialDeviceKey`, `CredentialAPIKey`, `CredentialSignedToken` or
  `CredentialAccessToken`, with its id for audit. Only a user acting in person
  (not with an API key or signed token) starts a payment for itself.

Hosts register the merchant permissions (`openrails.Permissions()`) in their
RBAC and grant them to staff roles. `openrails.MachinePermissions()`
(`merchant:checkout:create`) are never a person's: OpenRails refuses them to a
user acting in person whatever the roles say. `Auth` is helpers/auth's, and
AuthKit's `*authkit.Client` implements it (set `authkit.Config.Merchant` to the
group your staff roles apply in); a host with its own sessions implements the
four methods directly. `openrailstest.CheckAuth`
checks an implementation in your CI: it fires anonymous, refused, wrong
permission, other merchant, stale sign-in and machine requests through it and
fails on any acceptance.

An `openrails.Routes` selects the routes `client.Routes` returns; mount them
on your root router with the adapter for it. Validation happens here: a group
that needs `Auth` without one fails the mount before anything is registered,
so nothing is ever served open.

```go
routes := openrails.Routes{Auth: auth, Prefix: "/billing", Storefront: true, Customers: openrails.CustomerSelfService}
// net/http or Chi: github.com/open-rails/openrails/adapters/http
if err := openrailshttp.Mount(mux, client, routes); err != nil { return err }
// Gin: github.com/open-rails/openrails/adapters/gin
if err := openrailsgin.Mount(r, client, routes); err != nil { return err }
// Fiber v3: github.com/open-rails/openrails/adapters/fiber
if err := openrailsfiber.Mount(app, client, routes); err != nil { return err }
```

| `Routes` | Exposed surface |
|---|---|
| `Prefix` | Where the API lives: `/billing` serves `/billing/v1/*`; empty is the root |
| (always) | Capability discovery (what this mount serves) and signature-checked provider callbacks |
| `Storefront` | Products, prices, currencies, checkout config, reading and paying [checkout sessions](api/commerce.md#checkout-sessions) by id, Solana Pay and the captcha. A shared payment page is `Config.Checkout` (`PageURL`, `EmbedOrigins`). The signed-in customer mints at `/v1/me/checkout-sessions` (a customer route) |
| `Customers` | `/v1/me/*` for `Config.Merchant`: `CustomerSelfService`, `CustomerSubscriptionManagement` or `CustomerBillingManagement`; the zero value `CustomersNone` mounts none |
| `Auth` | Your auth (above); required by `Customers`, `CustomerProfiles` and `Merchant`. A checkout session shows saved cards only to its own customer, admitted by it |
| `CustomerProfiles` | Further customer surfaces (`openrails.CustomerRoutes`): another `Prefix`, another `Merchant`, or their own `Auth` |
| `Merchant` | The merchant API (`/v1/merchant/*`) for `Config.Merchant`, each route gated by `Auth.RequirePermission` for its merchant permission |
| `CatalogEdits` | The merchant API's catalog-write routes; requires `Merchant`. Every mount of the merchant API in one process must agree |
| `CookieOrigin` | Admits cookie-authenticated requests from this exact origin |
| `AdminConsole` | The staff dashboard at its own `Path` (`/admin` by default); requires `Merchant`. Nil mounts none |

A customer surface serves `Config.Merchant` (or its own `Merchant` slug). A
further audience mounts a `CustomerProfiles` entry under its own `Prefix`,
optionally with its own `Auth`. Every customer route acts only on the admitted
subject: no path, query or body names a customer, and another customer's
resource reads exactly like a missing one. See
[hosted customer audiences](architecture/customer-http-exposures.md).

Each adapter registers ordinary method and path routes, so route inspection
sees the real endpoints and unrelated paths keep the host's 404/405 behavior.
Original request URLs and bodies reach authentication and webhook verification
unchanged. One selection is materialized once, so remounting it never resets
rate limits.

**Admin console** (optional): `Routes.AdminConsole` mounts the staff
dashboard at its `Path` beside the merchant API it drives; the console finds
that API at `Routes.Prefix` and signs staff in through your AuthKit's JSON API
at `AdminConsole.AuthBaseURL`. It needs `Merchant` and a console build
(`Deps.ConsoleAssets`); `Mount` fails without either. See
[admin-console.md](admin-console.md).

### 7. Calling the engine

```go
if err := client.Ready(ctx); err != nil { return err } // fail fast at boot
```

The shared concrete `*openrails.Client`, grouped by job:

| Group | Methods |
|---|---|
| Admission (hot path) | `Admit`, `GetAdmission`, `CaptureAdmission`, `ReleaseAdmission`, `ExtendAdmission`, `ReportWastedSpend` |
| Usage | `RecordUsage` (metered events outside the hold/capture cycle), `GetUsage` |
| Configuration | `GetMerchantConfiguration`, `ApplyMerchantConfiguration`, `GetAPIHost`, `SetAPIHost`, `VerifyAPIHost` |
| Policy | `ListSpendDelegations`, `SetSpendDelegations`, `SetSpendDelegation`, `DeleteSpendDelegation`, `GetTrustLevel`, `SetTrustLevel`, `GetCreditLimit`, `SetCreditLimit` |
| Credits | `CreateCreditGrant`, `ListCreditGrants`, `GetCreditGrant`, `RevokeCreditGrant`, `ListCreditTransactions`, `GetBalance` |
| Customers / entitlements | `EnsureCustomers`, `GetCustomers`, `ListCustomers`, `GetCustomerBillingProfile`, `GetCustomerBillingPolicy`, `SetCustomerBillingPolicy`, `ListCustomerDelinquency`, `ListDelinquency`, `CheckEntitlements`, `ListCustomerEntitlements`, `ListEntitlementCustomers`, `GetEffectiveTier`, `CheckProductAccess`, `ListProductAccess`, `CreateProductAccess`, `DeleteProductAccess` |
| Catalog (API hosts) | `ApplyCatalog`, `GetCatalogRevision`, `CreateProduct`, `EnsureProduct`, `GetProduct`, `GetProductByKey`, `ListProducts`, `UpdateProduct`, `CreatePrice`, `GetPrice`, `GetPriceByKey`, `ListPrices`, `ListPriceKeyHistory`, `UpdatePrice`, `ListOffers`, `ListMeters`, `GetMeter`, `SetMeter`, `SetMeterRateCard`, `DeleteMeterRateCard`, `ListMeterRateOverrides`, `ListRateOverrides`, `SetRateOverride`, `DeleteRateOverride`, `ListCatalogDrift`, `RefreshCatalogDrift` |
| Checkout | `CreateCheckoutSession`, `CreateCheckoutAttempt`, `GetCheckoutAttempt`, `ConfirmCheckoutAttempt`, `GetCheckoutConfig` |
| Subscriptions | `GetSubscription`, `ListSubscriptions`, `CancelSubscription`, `ResumeSubscription`, `ChangeTier`, `PreviewTierChange`, `SetSubscriptionPaymentMethod`, `CreatePlanMigration`, `PreviewPlanMigration`, `CreateRepriceBatch`, `PreviewRepriceBatch`, `ListRepriceBatches`, `GetRepriceBatch`, `CancelRepriceBatch`, `ListReprices`, `GetReprice`, `CancelReprice` |
| Payments | `GetPayment`, `ListPayments`, `CreateOffChannelPayment`, `RefundPayment`, `GetPaymentSettlementStatus`, `ListPaymentAttempts`, `GetPaymentAttempt`, `ListRebillCycles`, `GetRebillCycle`, `ListPaymentMethods`, `DeletePaymentMethod` |
| Invoices | `ListInvoices`, `GetInvoice`, `ListInvoicePayments`, `CreateInvoicePayment`, `RetryInvoiceCollection`, `MarkInvoiceUncollectible`, `VoidInvoice`, `GetInvoiceProfile`, `SetInvoiceProfile` (`IfAbsent` to only create) |
| Provider obligations | `OpenOperationAuthorization`, `GetOperationAuthorization`, `ReleaseOperationAuthorization`, `RecordProviderBillingObservation`, `GetProviderBillingQualification` |
| Host feed / import | `ListHostEvents`, `AcknowledgeHostEvents`, `ImportBilling` |

```go
verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
    RequestID:       requestID, // idempotency key
    CustomerID:      billing.CustomerID(customerID), // the host's subject UUID
    Invoker:         userID,
    InvokerType:     billing.InvokerTypeDelegated,
    Currency:        "USD",
    EstimatedAmount: 50_000,    // native units (USD: micros)
    ExpiresAt:       &deadline, // required with a hold: the job's deadline
}})
receipt, err := client.CaptureAdmission(ctx, requestID, billing.CaptureAdmissionParams{
    Amount: 43_000, Usage: &billing.CaptureUsage{EventType: "chat.completion"},
})
held, err := client.CheckEntitlements(ctx, billing.CustomerID(customerID), billing.CheckEntitlementsParams{Entitlements: []string{"premium"}})
```

Entitlement checks address customers by the ids your auth system already holds;
a customer who never touched billing holds no keys, never an error. Deny verdicts are `(Allowed=false, nil
error)`.

`openrails.WithTimeout` configures the same call behavior as the remote
constructor; the credential and transport options (`WithAPIKey`,
`WithTokenProvider`, `WithCredentialProvider`, `WithHTTPClient`) belong to
`NewRemote` and `New` refuses them. Both modes default to a two-second call deadline;
`openrails.WithTimeout(0)` explicitly delegates the deadline to the caller.

Checkout sessions and attempts and checkout options use the shared client
too. See [checkout](api/commerce.md).

A host that must commit its own provider obligation atomically with the OpenRails
authorization, release or settlement uses the embedded Client's `Tx` operations
(`OpenOperationAuthorizationTx` and siblings) with a transaction from its pool.
See [provider obligations](architecture/provider-obligation-contract.md).

The in-process transport resolves and pins the selected immutable merchant for
each operation. An engine without a declared merchant serves many through one
Client:

```go
client, err := openrails.New(ctx, cfg, deps, openrails.WithDefaultMerchant("store-a"))
if err != nil { return err }
products, err := client.ListProducts(ctx, billing.ProductListParams{}, openrails.WithMerchant("store-b"))
// For stored UUIDs use openrails.ForMerchantID(id) instead of a slug selector.
```

The default does not restrict an engine, and the per-operation option does not
mutate it. An engine with a declared merchant refuses a different one. Both
selectors require the same operation permission; neither acts as authorization.

### 8. Hosted products

The library serves the merchants its host declares, behind the host's own auth.
A product that serves many merchants with OpenRails' own accounts (merchant
sign-up, teams, merchant API keys, trusted issuers, fleet analytics) builds on
`github.com/open-rails/openrails/server` instead, which composes this engine
with its own AuthKit like any host does; see
[standalone-integration.md](standalone-integration.md#building-on-the-server-package).

### 9. Acting on delinquency

For arrears billing, OpenRails decides when a customer's unpaid debt has outlived
the merchant's grace window and refuses their new spend at admission — but only
your app can shut off what your app runs. Transitions land on a durable,
acknowledged feed you drain:

```go
// client is openrails.New(...) or openrails.NewRemote(...); both use the same operations.
for _, kind := range []billing.HostEventType{
    billing.HostEventDelinquencyGrace,
    billing.HostEventDelinquencyEntered,
    billing.HostEventDelinquencyCleared,
} {
    page, err := client.ListHostEvents(ctx, billing.HostEventListParams{
        Type: kind, PageRequest: billing.PageRequest{Limit: 100},
    })
    if err != nil { return err }
    var done []billing.HostEventID
    var failed error
    for _, event := range page.Items {
        if failed = applyHostAction(ctx, event.Type, event.Delinquency); failed != nil {
            break // leave this and later events pending for replay
        }
        done = append(done, event.ID)
    }
    if len(done) > 0 {
        if _, err := client.AcknowledgeHostEvents(ctx, done); err != nil { return err }
    }
    if failed != nil { return failed }
}
```

Ack after your action is durable — an unacked event is redelivered. OpenRails
never revokes an entitlement for an unpaid arrears bill. Full boundary and policy:
[arrears-delinquency.md](arrears-delinquency.md).

### 10. Webhooks and ops

Point each rail's webhook at the webhook routes on **your** server, under your mount
prefix (`/v1/webhooks/{rail}/{account_id}`; see [the API guide](api/endpoints.md#provider-webhooks)). OpenRails verifies rail
signatures and updates subscriptions/entitlements; your app just reads the results.
Local rail sandboxes: [dev/local-webhooks.md](dev/local-webhooks.md).

Further reading: [operations.md](operations.md) (operating modes, safety levers,
dunning, the intents ledger), [billing-policies.md](billing-policies.md)
(named spend/credit-line policies and how you bind them),
[arrears-delinquency.md](arrears-delinquency.md),
[rate-limiting.md](rate-limiting.md),
[auth.md](auth.md) (the full one-credential-per-trust-domain rationale),
[self-hosting-mode1.md](self-hosting-mode1.md).


Merchant team and API-key routes belong to the standalone server's control
plane. An embedded engine mounts no placeholder management routes; the host
owns its identity and team UI.

### Merchant checkout authority

`Client.CreateCheckoutAttempt` uses the privileged merchant checkout endpoint. The host supplies the customer identity and is trusted to invoke this command for a real customer action. A merchant API key authorizes the host; it does not itself establish that a customer is interacting. Do not use merchant checkout as an unattended way to establish an initial customer-initiated stored-card agreement. Customer-facing self routes retain their authenticated customer boundary.

Set `CheckoutCustomerIdentity.ClientIP` on `CreateCheckoutAttempt` to the customer request's client address, resolved behind the host's trusted proxies. Declined cards then count per address as well as per customer, as on the customer routes, and a card-testing wave through the host can reach attack mode ([rate-limiting.md](rate-limiting.md)). An invalid address is refused with `400`.
