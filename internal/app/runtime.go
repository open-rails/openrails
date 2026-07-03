package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Runtime aggregates infrastructure clients and application services.
type Runtime struct {
	DB          *db.DB
	RedisClient *redis.Client
	Config      *config.Config

	// ConfiguredMerchant scopes this engine instance to one merchant — set by embedded
	// hosts that run one engine per merchant. Zero in standalone, where the merchant is
	// resolved per-credential. OpenRails is multi-merchant either way.
	ConfiguredMerchant merchant.ID

	// Rails is the temporary in-memory provider credential bridge for
	// embedded/private construction. It is not loaded from config.yaml/.env.
	Rails config.RailMerchantAccountSet

	// RouteCapabilities is the advisory, boot-probed view of what OpenRails can
	// actually do (#661), used to gate the provider route surface. Nil means
	// unprobed → the surface stays permissive (no capability gating).
	RouteCapabilities *routesurface.RuntimeCapabilities

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
	PaymentMethodService *paymentmethods.PaymentMethodService
	PaymentService       *payments.PaymentService
	VaultService         *paymentmethods.VaultService
	// PaymentSourceUpdateIntents routes NMI payment-method swaps through the
	// durable nmi_payment_source_update intent (#674 write-through). Set by the
	// composition root alongside the other write-through producers.
	PaymentSourceUpdateIntents *intents.PaymentSourceUpdateThrough

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService

	EmailService *subscriptions.EmailService

	EventLogService      *analytics.EventLogService
	EntitlementService   *entitlements.EntitlementService
	ProductAccessService *productaccess.Service
	MoneyService         *money.MoneyService
	// AdmissionPolicyCache is the process-local long-TTL spend-cap CONFIG cache
	// (tier + delegated-spend caps). nil = read the config from Postgres every admit.
	AdmissionPolicyCache *admission.PolicyCache
	MoneyCharger         money.Charger
	RailCustomerService  *payments.RailCustomerService
	Merchants            *merchants.Service
	// ManifestSecrets is the MODE-1 in-memory credential plane (#723), set iff
	// merchant_source=manifest. Boot provisioning seeds it (Seeder()); runtime
	// consumers read it through Merchants like any other store. The DB/Vault
	// store is never constructed in this mode.
	ManifestSecrets *merchants.ManifestSecretStore
	// CollectionResolver is the ONE #725 store-armed per-merchant credential
	// builder (arrears/top-up adapters + #730 manual-rebill NMI clients).
	CollectionResolver *money.MerchantCollectionAdapterBuilder

	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaRPC                *solana.RPCClient
	// SolanaRPCResolver is the #728 store-armed per-merchant RPC builder for the
	// process-wide Solana services (poller, crank, intent verify legs): merchant
	// rail-account settings win, SolanaRPC is the boot fallback.
	SolanaRPCResolver   *solanamodule.MerchantRPCBuilder
	SolanaPriceProvider solanamodule.TokenPriceProvider
	FXProvider               fx.Provider
	FXRateRefresher          interface {
		Stop()
		LastRefresh() time.Time
	}
	// SolanaCranker drives recurring Solana pulls (#256). Injected by the
	// composition root once the merchant secret store is available; nil -> the
	// cranker worker log-and-skips.
	SolanaCranker *recurring.CrankService
	// SolanaPlanService publishes on-chain recurring plans (#254) and
	// SolanaEnrollService activates a wallet enrollment (#255). Both are injected
	// by the composition root alongside the cranker; nil -> the HTTP handlers
	// return 503 (recurring not configured).
	SolanaPlanService   *recurring.PlanService
	SolanaEnrollService *recurring.EnrollService
	// SolanaPrepareCancelService builds the unsigned on-chain cancel transaction a
	// subscriber signs to trustlessly revoke a recurring Solana subscription
	// (#266). Injected alongside the other recurring services; nil -> the handler
	// returns 503 (recurring not configured).
	SolanaPrepareCancelService *recurring.PrepareCancelService
	// SolanaPrepareTierChangeService builds the SINGLE ATOMIC co-signed tier-change
	// transaction (cancel-old + subscribe-new [+ prorated transfer for an upgrade])
	// a subscriber signs to change tier on an existing Solana subscription (#272).
	// Injected alongside the other recurring services; nil -> the prepare handler
	// returns 503 (recurring not configured).
	SolanaPrepareTierChangeService *recurring.PrepareTierChangeService

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	WebhookDispatcher            *webhooks.WebhookDispatcher
	DeduplicationService         *webhooks.DeduplicationService
	IdempotencyService           *idempotency.IdempotencyService

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService

	// CardAbuseGuard escalates repeated card-charge failures to captcha/block
	// and detects site-wide card-testing attacks (#371). Nil when Redis isn't
	// configured (safe no-op).
	CardAbuseGuard *abuse.CardAbuseGuard

	riverStarted        bool
	externalRiverClient bool // true if River client was provided externally

	// workerHealthRegs captures every registered worker kind + periodic cadence
	// for the #689 health checker; lazily built (see workerHealthRegistrations).
	workerHealthRegs     *riverjobs.WorkerRegistrations
	workerHealthRegsOnce sync.Once
	// DeferredDeletes is the SYSTEM-origin deferred NMI-delete scheduler
	// (issue 216 / #344). Since #358 phase A it enqueues durable
	// nmi_delete_subscription intents on the provider intent ledger (no River
	// producer involved); the scheduled intent executor drains them. The
	// dunning worker threads it into its per-run lifecycle so terminal
	// cancellations schedule the rail-side delete through the ONE
	// mechanism (no inline deletes). User-asked cancellations use a separate
	// user-origin instance wired into UserSubscriptionService.
	DeferredDeletes subscriptions.DeferredDeleteScheduler
}

func (r *Runtime) FXRateHealth() (time.Time, bool) {
	if r == nil || r.FXRateRefresher == nil {
		return time.Time{}, false
	}
	last := r.FXRateRefresher.LastRefresh()
	return last, !last.IsZero() && time.Since(last) < 4*time.Hour
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
	if r.FXRateRefresher != nil {
		r.FXRateRefresher.Stop()
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
	if r.IdempotencyService != nil {
		r.IdempotencyService.Close()
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
	// #689: one middleware covers every registered worker's health bookkeeping.
	client, pool, err := buildRiverClient(r.Config, workers, []rivertype.Middleware{r.WorkerHealthMiddleware()})
	if err != nil {
		return err
	}
	r.RiverClient = client
	r.riverPool = pool
	return nil
}

// RunWorkers starts River workers (and other background loops) and blocks until ctx is done.
//
// If an external River client was provided via SetExternalRiverClient, this only starts
// non-River background loops (e.g., Solana Pay poller). The host is responsible for
// starting the shared River client.
func (r *Runtime) RunWorkers(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("runtime is nil")
	}

	// Start Solana Pay poller if configured (regardless of River setup). No
	// boot-rail gate (#728): a store-only merchant declares Solana purely in the
	// merchant store, and each merchant pass arms its own RPC; without pending
	// references a tick is a single cheap Redis read.
	if r.SolanaPayPoller != nil {
		go r.SolanaPayPoller.Start(ctx)
	}

	// Startup sweep (#358): convert deletion markers that exist WITHOUT a
	// live intent — in steady state none exist (producers write marker +
	// intent in one transaction); this heals markers written out of band,
	// chiefly direct-DB legacy imports (#391). Idempotent: markers already
	// on the ledger are a no-op. Pure DB work, so it runs in both the
	// in-process and external River wirings. Best-effort: on failure the
	// markers stay discoverable for #107 reconciliation and the next boot
	// retries.
	if _, err := r.ConvertDeferredDeleteMarkersToIntents(ctx); err != nil {
		log.WithError(err).Warn("Deferred-delete marker sweep failed; markers remain for reconciliation")
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
	if err := r.RiverClient.Start(ctx); err != nil {
		r.riverStarted = false
		return err
	}

	<-ctx.Done()
	return ctx.Err()
}

// AddBillingWorkersTo adds billing's River workers to the provided worker registry.
// This is used by embedded hosts who want to share their River client with openrails.
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

// SetSolanaCranker injects the recurring Solana cranker built once the merchant
// secret store is available (composition root). It must be called before
// InitRiver so the cranker worker picks it up.
func (r *Runtime) SetSolanaCranker(cranker *recurring.CrankService) {
	r.SolanaCranker = cranker
}

// SetSolanaRecurringServices injects the plan-publish (#254) and enroll (#255)
// services built once the merchant secret store is available (composition root).
func (r *Runtime) SetSolanaRecurringServices(plan *recurring.PlanService, enroll *recurring.EnrollService) {
	r.SolanaPlanService = plan
	r.SolanaEnrollService = enroll
}

// SetSolanaPrepareCancelService injects the on-chain cancel-tx builder (#266),
// built once the merchant secret store + RPC are available (composition root).
func (r *Runtime) SetSolanaPrepareCancelService(svc *recurring.PrepareCancelService) {
	r.SolanaPrepareCancelService = svc
}

// SetSolanaPrepareTierChangeService injects the atomic co-signed tier-change tx
// builder (#272), built once the merchant secret store + RPC are available
// (composition root). It uses the SAME per-merchant signer + RPC + network as the
// cranker so the cranker slot it co-signs is the merchant's own key.
func (r *Runtime) SetSolanaPrepareTierChangeService(svc *recurring.PrepareTierChangeService) {
	r.SolanaPrepareTierChangeService = svc
}
