package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/jupiter"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/services"
)

// Runtime aggregates infrastructure clients and application services.
type Runtime struct {
	DB               *db.DB
	RedisClient      *redis.Client
	Config           *config.Config
	Clock            clockwork.Clock
	CCBillClient     *ccbill.CCBillClient
	CCBillRESTClient *ccbill.RESTClient
	CCBillDataLink   *ccbill.DataLinkClient
	NMIClients       map[string]*nmi.NMIClient
	// RiverProducer is an enqueue-only River client. It should never be started.
	RiverProducer     *river.Client[pgx.Tx]
	riverProducerPool *pgxpool.Pool
	RiverClient       *river.Client[pgx.Tx]
	riverPool         *pgxpool.Pool

	SubscriptionService  *subscriptions.SubscriptionService
	ProductService       *catalog.ProductService
	PriceService         *catalog.PriceService
	NotificationService  *subscriptions.NotificationService
	PaymentMethodService *vault.PaymentMethodService
	PaymentService       *payments.PaymentService
	VaultService         *vault.VaultService

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService

	EmailService *subscriptions.EmailService

	EventLogService          *analytics.EventLogService
	EntitlementService       *entitlements.EntitlementService
	CreditsService           *credits.CreditsService
	CreditTypeService        *credits.CreditTypeService
	ProcessorCustomerService *payments.ProcessorCustomerService

	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaRPC                *solana.RPCClient
	SolanaTokenRegistry      *jupiter.TokenRegistry
	FXProvider               fx.Provider

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	WebhookDispatcher            *webhooks.WebhookDispatcher
	DeduplicationService         *webhooks.DeduplicationService
	IdempotencyService           *services.IdempotencyService

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService

	riverStarted        bool
	externalRiverClient bool // true if River client was provided externally
}

// Close gracefully shuts down runtime resources.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var errs []error

	// Stop Solana Pay poller
	if r.SolanaPayPoller != nil {
		log.Info("Stopping Solana Pay poller...")
		r.SolanaPayPoller.Stop()
	}

	// Only stop River client if we created it (not external)
	if r.RiverClient != nil && r.riverStarted && !r.externalRiverClient {
		log.Info("Stopping River background workers...")
		if err := r.RiverClient.Stop(ctx); err != nil {
			// During shutdown, Stop can surface context cancellation if the passed ctx is cancelled.
			// Treat this as an expected shutdown condition.
			if !errors.Is(err, context.Canceled) {
				errs = append(errs, fmt.Errorf("failed to stop River client: %w", err))
			}
		}
		r.riverStarted = false
	}
	if r.riverPool != nil {
		r.riverPool.Close()
		r.riverPool = nil
	}
	if r.riverProducerPool != nil {
		r.riverProducerPool.Close()
		r.riverProducerPool = nil
	}
	if r.DB != nil {
		if err := r.DB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close db: %w", err))
		}
	}
	if r.EventLogService != nil {
		if err := r.EventLogService.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close billing event service: %w", err))
		}
	}
	if r.RedisClient != nil {
		if err := r.RedisClient.Close(); err != nil {
			// Make shutdown idempotent: Close can be called multiple times across layers.
			if !errors.Is(err, redis.ErrClosed) {
				errs = append(errs, fmt.Errorf("failed to close Redis client: %w", err))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("failed to close some resources: %v", errs)
}

// InitRiver initialises the River client for background workers.
// If an external client was provided via SetExternalRiverClient, this is a no-op.
func (r *Runtime) InitRiver(ctx context.Context) error {
	if r.RiverClient != nil {
		return nil
	}
	// If external client was set, we don't create our own
	if r.externalRiverClient {
		return nil
	}
	workers, err := r.buildRiverWorkers(ctx)
	if err != nil {
		return fmt.Errorf("build river workers: %w", err)
	}
	client, pool, err := buildRiverClient(r.Config, workers)
	if err != nil {
		return err
	}
	r.RiverClient = client
	r.riverPool = pool
	return nil
}

// RunWorkers starts River workers (and other background loops) and blocks until ctx is done.
// This is intended to run in a dedicated worker process, not inside the HTTP server.
//
// If an external River client was provided via SetExternalRiverClient, this only starts
// non-River background loops (e.g., Solana Pay poller). The host is responsible for
// starting the shared River client.
func (r *Runtime) RunWorkers(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("runtime is nil")
	}

	// Start Solana Pay poller if configured (regardless of River setup).
	if r.SolanaPayPoller != nil && r.Config != nil && r.Config.GetSolanaProcessor() != nil {
		go r.SolanaPayPoller.Start(ctx)
	}

	// If external client, don't start River workers - host is responsible
	if r.externalRiverClient {
		log.Info("External River client configured - skipping River worker startup")
		// Block until context is cancelled
		<-ctx.Done()
		return ctx.Err()
	}

	if err := r.InitRiver(ctx); err != nil {
		return err
	}
	if r.RiverClient == nil {
		return fmt.Errorf("river client not initialized")
	}

	periodicJobs, err := r.buildRiverPeriodicJobs(ctx)
	if err != nil {
		return fmt.Errorf("configure periodic jobs: %w", err)
	}
	for _, job := range periodicJobs {
		r.RiverClient.PeriodicJobs().Add(job)
	}

	r.riverStarted = true
	log.Info("Starting River background workers")
	return r.RiverClient.Start(ctx)
}

// AddBillingWorkersTo adds billing's River workers to the provided worker registry.
// This is used by embedded hosts who want to share their River client with billing.
func (r *Runtime) AddBillingWorkersTo(ctx context.Context, workers *river.Workers) error {
	if r == nil {
		return fmt.Errorf("runtime is nil")
	}
	return r.addBillingWorkersToRegistry(ctx, workers)
}

// GetBillingPeriodicJobs returns billing's periodic jobs for external River client setup.
// This is used by embedded hosts who want to add billing's periodic jobs to their client.
func (r *Runtime) GetBillingPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error) {
	return r.buildRiverPeriodicJobs(ctx)
}

// SetExternalRiverClient sets an external River client for billing to use.
// When set, billing will use this client for enqueueing and will not create its own.
// The host is responsible for registering billing workers (via AddBillingWorkersTo)
// and starting the client.
func (r *Runtime) SetExternalRiverClient(client *river.Client[pgx.Tx]) {
	if r == nil {
		return
	}
	r.RiverClient = client
	r.RiverProducer = client // Use same client for enqueueing
	r.externalRiverClient = true
}

// HasExternalRiverClient returns true if an external River client was configured.
func (r *Runtime) HasExternalRiverClient() bool {
	if r == nil {
		return false
	}
	return r.externalRiverClient
}
