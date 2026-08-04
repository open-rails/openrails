# Embedded OpenRails

The `embedded` package allows you to integrate OpenRails directly into your Go
application instead of running it as a standalone service.

## Basic Usage

```go
import (
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/pkg/embedded"
)

func main() {
    cfg := &config.Config{
        // ... your OpenRails configuration.
        // Set TestMode EXPLICITLY: embedded mode never runs config.Load, so the
        // zero value means LIVE credential posture (standalone dev boots default
        // to sandbox; embedded ones do not). New() warns when TestMode is left
        // false in a dev-like Env.
    }

    openrails, err := embedded.New(embedded.Options{
        Config:  cfg,
        PGXPool: yourPgxPool,  // Share your existing connection pool
        Redis:   yourRedis,    // Share your existing Redis client
        PaymentProviders: []embedded.PaymentProvider{
            {
                Config: config.RailMerchantAccountConfig{
                    Rail:      models.RailStripe,
                    AccountID: "acct_1M9QZULkdIwHu7ix",
                    Stripe:    &config.StripeRailConfig{SecretKey: hostSecrets.StripeKey},
                },
            },
            {
                Name: "stripe_old",
                Config: config.RailMerchantAccountConfig{
                    Rail:      models.RailStripe,
                    AccountID: "acct_1N2YbMLkdIwHu7ix",
                    Archived:  true,
                    Stripe:    &config.StripeRailConfig{SecretKey: hostSecrets.StripeOldKey},
                },
            },
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    defer openrails.Close(context.Background())

    // Mount OpenRails on its canonical billing namespace. MountHandler strips
    // MountPrefix and combines the user/admin/webhook surface with the /v1/me
    // self-service subtree; select only the route groups this host needs.
    handler, err := embedded.MountHandler(openrails, embedded.MountOptions{
        MountPrefix: "/billing",
        RouteSets: []embedded.RouteSet{
            embedded.RouteSetCheckout,
            embedded.RouteSetCustomer,
            embedded.RouteSetWebhooks,
        },
        Authenticator:          hostAuthn, // checkout + customer routes
        DelegatedAuthenticator: hostDeleg, // /v1/me + /v1/customers
        Gate:                   hostGate,  // merchant groups (catalog/providers/admin)
    })
    if err != nil {
        log.Fatal(err)
    }

    mux := http.NewServeMux()
    mux.Handle("/billing/", handler) // plain net/http; or gin.WrapH(handler) / chi Mount
}
```

The handler is framework-neutral `net/http` (#670): a gin host mounts it with
`gin.WrapH`, chi with `Mount`, etc. `Embedded.NewHTTPHandler` remains the
lower-level single-surface constructor for hosts that do their own prefix
handling.

### Route-group discovery

Which groups a deployment serves is config-dependent, so OpenRails advertises it
two ways over one source of truth:

- **HTTP** (browsers, the remote SDK, other services): `GET /billing/v1/capabilities`
  returns `{"route_groups": {"checkout": true, "customer": true, "payment_providers": false, ...}, "routes": {"billing_portal": true, "solana": false}}`
  — public, cached, hand-written (not OpenAPI; nothing is generated).
- **Go** (in-process host code): `rt.ActiveRouteSets()` returns the same selection.

### Host-routed webhooks (#734)

A host serving multiple merchants behind distinct hostnames (e.g. openrails-saas's
`api.<slug>.<domain>` scheme) gets Host-routed webhooks for free, with NO extra
`MountOptions`/`Options` field to set: attaching a control plane
(`pkg/embedded/controlplane.AttachWithOptions`) BEFORE calling
`MountHandler`/`NewHTTPHandler` is the only step — the Host->merchant resolver
and the `/v1/webhooks/:provider` mount under `RouteSetWebhooks` derive from
whatever control plane is attached (`embedhttp.HostMerchantResolverFrom`). A
host with no control plane attached, or one that never sets a merchant's
`api_host` (see docs/operations.md), is completely unaffected.

### Browser CORS (#765)

Unrelated to the above: the checkout and self-service/customer-treasury route
sets always answer a static `Access-Control-Allow-Origin: *` (never
credentials) — no control plane, no `api_host`, no configuration required.
Every other route set gets no CORS headers at all. See docs/operations.md's
"Browser CORS doctrine" for the reasoning (bearer JWTs, not cookies, are the
security boundary).

## Payment Providers

Embedded hosts have two credential planes; `config.yaml` carries neither:

1. **Per-merchant secrets store (preferred)** — declare provider accounts +
   secrets in the merchant manifest (`embed.Runtime.UpsertMerchantConfig` /
   bootstrap). Checkout, webhooks AND the pull plane (provider refresh,
   unknown-cohort resolution, per-subscription probes, `PullProvider`) all
   resolve those credentials per merchant at use time (#699). A host that only
   seeds the store needs **no** `Options.PaymentProviders`.
2. **Boot-config plane** — `Options.PaymentProviders` on `embedded.New` feeds
   the process-wide rail set. It remains the fallback for rails a merchant has
   no declared account row on; when both planes are configured and differ, the
   merchant store wins (with a WARN).

```go
openrails, err := embedded.New(embedded.Options{
    Config: cfg,
    PaymentProviders: []embedded.PaymentProvider{
        {
            Config: config.RailMerchantAccountConfig{
                Rail:      models.RailStripe,
                AccountID: "acct_1M9QZULkdIwHu7ix",
                Stripe:    &config.StripeRailConfig{SecretKey: stripeSecret},
            },
        },
        {
            Name: "stripe_old",
            Config: config.RailMerchantAccountConfig{
                Rail:      models.RailStripe,
                AccountID: "acct_1N2YbMLkdIwHu7ix",
                Archived:  true,
                Stripe:    &config.StripeRailConfig{SecretKey: stripeOldSecret},
            },
        },
        {
            Name: "mobius",
            Config: config.RailMerchantAccountConfig{
                Rail:      models.RailNMI,
                AccountID: "579145",
                NMI:       &config.NMIRailConfig{SecurityKey: mobiusSecurityKey},
            },
        },
    },
})
```

`Name` is only a local selector for config, logs, and explicit operations. It is
not durable identity. OpenRails resolves durable provider-account identity from
the provider itself, such as Stripe `/v1/account` or the NMI profile report, and
stores provider-owned rows against that provider account id.

`Archived` controls provider-account lifecycle. Omit it, or set it false, for
accounts eligible for new checkout/subscription work. Set `Archived: true` for
old accounts that must still handle existing provider-bound obligations,
historical pulls, refunds, webhooks, or subscriptions that still rebill there.

Most embedded applications should configure one non-archived account per rail:
one Stripe account, one NMI/Mobius account, one CCBill account, and so on. Extra
accounts are mainly for account rotation: keep the old account archived while
new work uses a non-archived account.

If `Name` is omitted, OpenRails generates a local selector like `stripe`,
`stripe_2`, or `nmi`. If a host needs to target a credential set by name, provide
an explicit `Name`.

## Postgres & River schema contract

OpenRails owns a single configurable Postgres schema, set via config `db.schema` (env `DB_SCHEMA`), defaulting to `billing`. It holds OpenRails' own billing tables.

River job-queue tables (`river_*`) follow these rules:

- **Standalone:** OpenRails builds its own River client and the River schema equals the OpenRails schema (`db.schema`). It is not separately configurable.
- **Embedded (this package):** the **host owns River**. Inject your unified River client via `SetRiverClient` — OpenRails enqueues through it and never constructs a River client or assumes/overrides a River schema. Your injected client's schema decides where River tables live, so they can sit in the host's primary schema even though OpenRails' billing tables sit under `db.schema`.

**Migration safety:** changing `db.schema` does not move existing tables; it creates a second set under the new schema. OpenRails does not auto-migrate River tables across schemas — decommission old objects yourself if you switch.

## River Integration (Background Jobs)

If your application uses [River](https://riverqueue.com) for background jobs, you can share a single River client with OpenRails instead of running separate clients.

### Why Share?

- **Single connection pool** - One River client = one connection pool to Postgres
- **Unified monitoring** - All jobs visible in one place
- **Resource efficiency** - Avoid duplicate polling of `river_job` table

### Setup

```go
import (
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/riverqueue/river"
    "github.com/riverqueue/river/riverdriver/riverpgxv5"

    "github.com/open-rails/openrails/pkg/embedded"
)

func setupOpenRailsWithSharedRiver(ctx context.Context, pool *pgxpool.Pool) error {
    // 1. Create OpenRails instance
    openrails, err := embedded.New(embedded.Options{
        Config:  cfg,
        PGXPool: pool,
    })
    if err != nil {
        return err
    }

    // 2. Create combined worker registry
    workers := river.NewWorkers()

    // Add your application's workers
    river.AddWorkerSafely(workers, &MyAppWorker{})
    river.AddWorkerSafely(workers, &AnotherWorker{})

    // Add OpenRails workers to the same registry
    if err := openrails.AddWorkersTo(ctx, workers); err != nil {
        return err
    }

    // 3. Create single River client with combined workers
    client, err := river.NewClient(riverpgxv5.New(pool), river.Config{
        Workers: workers,
        Queues: map[string]river.QueueConfig{
            river.QueueDefault:      {MaxWorkers: 10},
            embedded.QueueBilling:   {MaxWorkers: 5},  // OpenRails uses this queue
        },
    })
    if err != nil {
        return err
    }

    // 4. Add OpenRails periodic jobs
    periodicJobs, err := openrails.GetPeriodicJobs(ctx)
    if err != nil {
        return err
    }
    for _, job := range periodicJobs {
        client.PeriodicJobs().Add(job)
    }

    // 5. Inject client into OpenRails for job enqueueing
    openrails.SetRiverClient(client)

    // 6. Start the unified client (you manage the lifecycle)
    if err := client.Start(ctx); err != nil {
        return err
    }

    return nil
}
```

### API Reference

#### Constants

```go
// QueueBilling is the River queue name used by OpenRails workers.
// Configure this queue when creating your River client.
const QueueBilling = "billing"
```

#### Methods

```go
// AddWorkersTo adds OpenRails River workers to your worker registry.
// Call after creating your registry but before creating the River client.
func (e *Embedded) AddWorkersTo(ctx context.Context, workers *river.Workers) error

// GetPeriodicJobs returns OpenRails periodic jobs (dunning, cleanup, etc.).
// Add these to your River client before starting it.
func (e *Embedded) GetPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error)

// SetRiverClient injects your River client for OpenRails enqueueing.
// When set, OpenRails won't create its own client.
func (e *Embedded) SetRiverClient(client *river.Client[pgx.Tx])

// HasExternalRiverClient returns true if an external client was configured.
func (e *Embedded) HasExternalRiverClient() bool
```

### OpenRails periodic jobs

When you call `GetPeriodicJobs()`, OpenRails returns these scheduled jobs:

| Job | Interval | Purpose |
|-----|----------|---------|
| Dunning | 4 hours | Retry failed subscription payments |
| Idempotency Cleanup | 24 hours | Remove old idempotency keys |
| CCBill Reconcile | 6 hours | Reconcile with CCBill DataLink |
| Cleanup Expired Data | 1 hour | Remove expired wallet challenges, payment intents |
| Credit Expiry | 1 hour | Expire credit batches |

### Without River Sharing

If you don't use River or prefer OpenRails to manage its own client:

```go
openrails, _ := embedded.New(opts)

// Start OpenRails' own River workers (blocking)
go func() {
    if err := openrails.RunWorkers(ctx); err != nil {
        log.Error(err)
    }
}()
```

## Handlers

The embedded instance provides HTTP handlers suitable for mounting into your host app.

Canonical embedded contract: routes live under `/billing/v1/*`.

```go
// Full standalone OpenRails API (health + user + admin + webhooks + merchant API; debug routes in dev only)
openrails.Handler() http.Handler

// Selective handler (choose route groups)
openrails.NewHTTPHandler(embedded.HTTPHandlerOptions{
	RouteSets: []embedded.RouteSet{
		embedded.RouteSetPublicCatalog,
		embedded.RouteSetCustomer,
		embedded.RouteSetMerchantAdmin,
		embedded.RouteSetWebhooks,
	},
})

// Server-to-server operations: embedded hosts call the in-process Service()
// facade (below) after authorizing the action themselves. Hosts that need HTTP
// loopback parity can opt into embedded.RouteSetMerchantAPI; standalone machine
// callers use OpenRails-issued API keys against the public /v1/merchant/* routes.
```

## In-Process Service API

For server-to-server operations (credits, entitlements), use the in-process API instead of HTTP:

```go
svc, err := openrails.Service()
if err != nil {
    return err
}

// Check entitlements
entitled, err := svc.CheckEntitlement(ctx, userID, "feature_name")

// Withdraw credits
trx, err := svc.WithdrawCredits(ctx, service.WithdrawCreditsRequest{
	UserID:     userID,
	CreditType: "api_dollars",
	Amount:     100,
	Source:     "api_call",
})

// Hold credits for long-running job
// HoldCredits returns a durable hold ID (backed by `billing.credit_transactions`).
hold, err := svc.HoldCredits(ctx, service.HoldCreditsRequest{
	UserID:     userID,
	CreditType: "gpu_minutes",
	Amount:     6000,
	Source:     "gpu_job",
	SourceID:   jobID,
	ExpiresAt:  expiry,
})

// Capture actual usage
trx, err = svc.CaptureHold(ctx, service.CaptureHoldRequest{HoldID: hold.ID, Amount: actualAmount})
```

### Merchant scoping (required)

Every merchant-owned table is RLS-scoped by the `app.merchant_id` session GUC.
HTTP callers get it from middleware; an in-process Go caller must ask for it, or
reads silently see zero rows and writes are rejected:

```go
// The engine is bound to one merchant: no id to pass, no id to get wrong.
err := openrails.RunInMerchant(ctx, func(ctx context.Context) error {
	_, err := svc.CreateProduct(ctx, service.CreateProductRequest{ /* … */ })
	return err
})

// Multi-merchant engines name the merchant explicitly.
err = openrails.RunInMerchantConn(ctx, merchantID, func(ctx context.Context) error { /* … */ })
```

Both PROVE the pin took before running the callback, and a bound engine refuses
a call that names a different merchant.

## Testing rail pushes against a fake Stripe

`Options.StripeTransport` installs a host-supplied `http.RoundTripper` under
OpenRails' Stripe choke point, so an integration test can drive real rail-push
paths (catalog push, checkout, plan migration) against an `httptest` server.
Readonly enforcement and the pinned `Stripe-Version` still apply above it, and
the option is refused when `Config.TestMode` is `CredentialPostureLive`.

```go
e, err := embedded.New(embedded.Options{
	Config:          cfg, // TestMode: config.CredentialPostureSandbox
	PGXPool:         pool,
	River:           embedded.RiverManagedByOpenRails(),
	StripeTransport: hostRewriteTransport{target: fakeStripe.URL},
})
```

`Close` removes it. It is process-global, so one engine per process may set it.
