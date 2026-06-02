# Embedded Billing

The `embedded` package allows you to integrate Open Rails Billing directly into your Go application instead of running it as a standalone service.

## Basic Usage

```go
import (
    "github.com/open-rails/openrails/config"
    "github.com/open-rails/openrails/pkg/embedded"
)

func main() {
    cfg := &config.Config{
        // ... your billing configuration
    }

    billing, err := embedded.New(embedded.Options{
        Config:  cfg,
        PGXPool: yourPgxPool,  // Share your existing connection pool
        Redis:   yourRedis,    // Share your existing Redis client
    })
    if err != nil {
        log.Fatal(err)
    }
    defer billing.Close(context.Background())

    // Mount billing routes on your router
    router := gin.Default()
    billing.RegisterUserRoutes(router.Group("/billing/v1"), embedded.RouteOptions{})
    billing.RegisterWebhookRoutes(router.Group("/billing/v1/webhooks"))
}
```

## River Integration (Background Jobs)

If your application uses [River](https://riverqueue.com) for background jobs, you can share a single River client with billing instead of running separate clients.

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

func setupBillingWithSharedRiver(ctx context.Context, pool *pgxpool.Pool) error {
    // 1. Create billing instance
    billing, err := embedded.New(embedded.Options{
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

    // Add billing workers to the same registry
    if err := billing.AddWorkersTo(ctx, workers); err != nil {
        return err
    }

    // 3. Create single River client with combined workers
    client, err := river.NewClient(riverpgxv5.New(pool), river.Config{
        Workers: workers,
        Queues: map[string]river.QueueConfig{
            river.QueueDefault:      {MaxWorkers: 10},
            embedded.QueueBilling:   {MaxWorkers: 5},  // Billing uses this queue
        },
    })
    if err != nil {
        return err
    }

    // 4. Add billing's periodic jobs
    periodicJobs, err := billing.GetPeriodicJobs(ctx)
    if err != nil {
        return err
    }
    for _, job := range periodicJobs {
        client.PeriodicJobs().Add(job)
    }

    // 5. Inject client into billing for job enqueueing
    billing.SetRiverClient(client)

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
// QueueBilling is the River queue name used by billing workers.
// Configure this queue when creating your River client.
const QueueBilling = "billing"
```

#### Methods

```go
// AddWorkersTo adds billing's River workers to your worker registry.
// Call after creating your registry but before creating the River client.
func (e *Embedded) AddWorkersTo(ctx context.Context, workers *river.Workers) error

// GetPeriodicJobs returns billing's periodic jobs (dunning, cleanup, etc.).
// Add these to your River client before starting it.
func (e *Embedded) GetPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error)

// SetRiverClient injects your River client for billing to use for enqueueing.
// When set, billing won't create its own client.
func (e *Embedded) SetRiverClient(client *river.Client[pgx.Tx])

// HasExternalRiverClient returns true if an external client was configured.
func (e *Embedded) HasExternalRiverClient() bool
```

### Billing's Periodic Jobs

When you call `GetPeriodicJobs()`, billing returns these scheduled jobs:

| Job | Interval | Purpose |
|-----|----------|---------|
| Dunning | 4 hours | Retry failed subscription payments |
| Idempotency Cleanup | 24 hours | Remove old idempotency keys |
| CCBill Reconcile | 6 hours | Reconcile with CCBill DataLink |
| Cleanup Expired Data | 1 hour | Remove expired wallet challenges, payment intents |
| Credit Expiry | 1 hour | Expire credit batches |

### Without River Sharing

If you don't use River or prefer billing to manage its own client:

```go
billing, _ := embedded.New(opts)

// Start billing's own River workers (blocking)
go func() {
    if err := billing.RunWorkers(ctx); err != nil {
        log.Error(err)
    }
}()
```

## Handlers

The embedded instance provides HTTP handlers suitable for mounting into your host app.

Canonical embedded contract: routes live under `/billing/v1/*`.

```go
// Full public billing API (health + user + admin + webhooks; debug routes in dev only)
billing.Handler() http.Handler

// Selective handler (choose route groups)
billing.NewHTTPHandler(embedded.HTTPHandlerOptions{
	IncludeUser:     true,
	IncludeAdmin:    true,
	IncludeWebhooks: true,
})

// Internal service-to-service API. Embedded hosts usually call Service() instead.
billing.PrivateHandler() http.Handler
```

## In-Process Service API

For server-to-server operations (credits, entitlements), use the in-process API instead of HTTP:

```go
svc, err := billing.Service()
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
