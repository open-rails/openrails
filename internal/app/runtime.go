package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/copilot"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/railresolve"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Runtime aggregates infrastructure clients and application services.
type Runtime struct {
	DB          *db.DB
	RedisClient *redis.Client
	// redisOwned marks a self-dialed client; injected clients are borrowed and
	// must never be closed here (the host owns their lifecycle).
	redisOwned bool
	Config     *config.Config

	// configuredMerchant scopes this engine instance to one merchant — set by
	// embedded hosts that run one engine per merchant (zero in standalone,
	// where the merchant is resolved per-credential). It is an atomic because
	// UpsertMerchantConfig may bind it AFTER New (embed/provision.go) while an
	// HTTP handler built from this Runtime may already be mounted and serving
	// requests (#744): every reader MUST go through ConfiguredMerchant() and
	// re-resolve live (mirroring embed/transport.go's in-process live read) —
	// never cache the value at handler-construction time, or mount order
	// relative to the bind silently determines which merchant a request lands
	// on. Use SetConfiguredMerchant to write it.
	configuredMerchant atomic.Pointer[merchant.ID]

	// TrustedProxies is the boot-configured proxy-aware client-IP resolver
	// (#746), built once from config.Config.TrustedProxies. A nil/empty
	// resolver trusts nothing (raw socket peer only). Every consumer that
	// needs "the request's real client IP" — rate-limit subject keys, abuse
	// tracking, webhook IPAddress recording, the CCBill IP allowlist —
	// resolves through this ONE instance instead of reading
	// RemoteAddr/X-Forwarded-For itself.
	TrustedProxies *iputil.TrustedProxies

	// RouteCapabilities is the advisory, boot-probed view of what OpenRails can
	// actually do (#661), used to gate the provider route surface. Nil means
	// unprobed → the surface stays permissive (no capability gating).
	RouteCapabilities *routesurface.RuntimeCapabilities

	Clock clockwork.Clock
	// RiverProducer is an enqueue-only River client. It should never be started.
	RiverProducer     *river.Client[pgx.Tx]
	riverProducerPool *pgxpool.Pool
	RiverClient       *river.Client[pgx.Tx]
	riverPool         *pgxpool.Pool

	SubscriptionService      *subscriptions.SubscriptionService
	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	PaymentService           *payments.PaymentService
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	// RepriceService is the #773 reprice primitive (move subscribers to a
	// different price at their next renewal).
	RepriceService *subscriptions.RepriceService

	// PlanMigrationService (#813): operator-driven cross-product bulk plan
	// migration (retire plan-A -> move cohort to plan-B) over the reprice
	// engine.
	PlanMigrationService *subscriptions.PlanMigrationService
	// PaymentSourceUpdateIntents routes NMI payment-method swaps through the
	// durable nmi_payment_source_update intent (#674 write-through). Set by the
	// composition root alongside the other write-through producers.
	PaymentSourceUpdateIntents *intents.PaymentSourceUpdateThrough

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService

	EmailService *subscriptions.EmailService

	EntitlementService   *entitlements.EntitlementService
	ProductAccessService *productaccess.Service
	MoneyService         *money.MoneyService
	// MetricsService is the #733 merchant analytics query engine.
	MetricsService *metrics.Service
	// DashboardService is the #741 configurable dashboard (saved widgets +
	// NL widget generation; nil-LLM = generation fail-closed).
	DashboardService *dashboard.Service
	// CopilotService is the #779 catalog copilot (read-only Q&A always; the
	// Phase 2 draft_* tools are additionally gated on
	// llm.catalog_drafting_enabled — see copilot.Service.DraftingConfigured).
	CopilotService *copilot.Service
	// AlertService is the #736 metric-threshold alerting engine (rules,
	// webhooks, notifications, the evaluator).
	AlertService *alerting.Service
	// WebhookHealth records inbound-webhook liveness per (merchant, rail) at the
	// ingest verify seam (#786). Nil-safe: recording never fails a webhook.
	WebhookHealth *webhookhealth.Recorder
	// AdmissionPolicyCache is the process-local long-TTL cache of the or#897
	// billing-policy RESOLUTION (which named policy binds to a payer at a tier).
	// nil = resolve the binding from Postgres on every admit.
	AdmissionPolicyCache *admission.PolicyCache
	MoneyCharger         money.Charger
	RailCustomerService  *payments.RailCustomerService
	Merchants            *merchants.Service
	// MerchantGroupResolver is the or#914 rename-forwarding seam an attached
	// control plane installs (slug -> merchant group id + current slug,
	// tombstone-following). ArmMerchantsService applies it to any merchants
	// service armed AFTER the control plane attached; the attach path applies
	// it directly when Merchants already exists. Nil without a control plane.
	MerchantGroupResolver          merchants.GroupSlugResolver
	MerchantGroupCanonicalResolver merchants.GroupIDResolver
	// ManifestSecrets is the MODE-1 in-memory credential plane (#723), set iff
	// merchant_source=manifest. Boot provisioning seeds it (Seeder()); runtime
	// consumers read it through Merchants like any other store. The DB/Vault
	// store is never constructed in this mode.
	ManifestSecrets *merchants.ManifestSecretStore
	// MerchantSecretPing, when set, live-probes whether the merchant-secret
	// backend built for Merchants is reachable RIGHT NOW (#748 Ready()) — the
	// counterpart to Merchants' boot-time arming. Nil when arming hasn't run
	// (see Ready's armed/unarmed check) or the backend needs no separate
	// liveness probe (e.g. MODE 1's in-memory manifest plane). Set by
	// EnsureMerchantsService / the standalone server alongside Merchants.
	MerchantSecretPing func(ctx context.Context) error
	// CollectionResolver is the ONE #725/#788 store-armed per-merchant
	// credential resolver (arrears/top-up adapters + NMI clients for rebills,
	// cancels, refunds and admin actions).
	CollectionResolver money.CollectionPlane
	// RailConfigs is the ONE Layer-C rail resolution seam (#788): every
	// decision-time rail credential/armed-state read resolves the ctx
	// merchant's psps row + scoped secrets through it.
	RailConfigs railresolve.Source
	// CCBillDataLinkEndpoint overrides the DataLink endpoint on store-armed
	// CCBill intent clients (test seam; empty = real endpoint). Read at
	// registry build time — IntentRunner() rebuilds per call, so mutation
	// takes effect for synchronous ledger execution immediately.
	CCBillDataLinkEndpoint string

	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	// SolanaRPCResolver is the #728/#788 store-armed per-merchant RPC builder
	// for the process-wide Solana services (poller, crank, intent verify legs,
	// request-plane chain reads): merchant rail-account settings are the ONLY
	// credential plane.
	SolanaRPCResolver *solanamodule.MerchantRPCBuilder
	// SolanaMintDecimals reads SPL mint decimals from the chain and caches them
	// (#817). The chain is the source of truth for decimals — merchants do not
	// declare them — so every micros->base-units conversion sources its shift
	// here. Armed over SolanaRPCResolver's merchant-scoped chain reader.
	SolanaMintDecimals  *solanamodule.MintDecimals
	SolanaPriceProvider solanamodule.TokenPriceProvider
	FXProvider          fx.Provider
	FXRateRefresher     interface {
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
	IdempotencyService           *replaycache.Store
	// HTTPIdempotency is the client-facing Idempotency-Key replay store (#579):
	// a THIRD idempotency instance (24h TTL), separate from IdempotencyService
	// (checkout's internal dedup) and the webhook dedup instance, backing the
	// generic HTTP response-replay middleware on public mutating routes.
	HTTPIdempotency *replaycache.Store

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService

	// CardAbuseGuard escalates repeated card-charge failures to captcha/block
	// and detects site-wide card-testing attacks (#371). Nil when Redis isn't
	// configured (safe no-op).
	CardAbuseGuard *abuse.CardAbuseGuard

	riverStarted        bool
	externalRiverClient bool
	riverSchema         string // true if River client was provided externally

	// progressLifecycle owns the #895 out-of-River progress detector: a plain
	// goroutine that answers "is the periodic fleet progressing?" without
	// needing a job to run to find out.
	progressLifecycle

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

// ConfiguredMerchant returns the merchant this engine instance is scoped to,
// resolved fresh on every call — NEVER cache this at handler-construction
// time (#744: UpsertMerchantConfig can bind it after New, after a handler
// built from this Runtime is already mounted and serving requests). Zero in
// standalone, or before any bind.
func (r *Runtime) ConfiguredMerchant() merchant.ID {
	if r == nil {
		return merchant.ID{}
	}
	if id := r.configuredMerchant.Load(); id != nil {
		return *id
	}
	return merchant.ID{}
}

// SetConfiguredMerchant binds this engine instance to a merchant. Safe to
// call post-boot (embed.UpsertMerchantConfig) concurrently with in-flight
// requests calling ConfiguredMerchant() — that concurrency safety is the
// entire point of #744's fix.
func (r *Runtime) SetConfiguredMerchant(id merchant.ID) {
	if r == nil {
		return
	}
	r.configuredMerchant.Store(&id)
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

	// #895: stop the out-of-River progress detector first — it outlives the
	// River client on purpose, so nothing else will cancel it.
	r.stopRiverProgressMonitor()

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
	if r.IdempotencyService != nil {
		r.IdempotencyService.Close()
	}
	if r.HTTPIdempotency != nil {
		r.HTTPIdempotency.Close()
	}
	if r.RedisClient != nil && r.redisOwned {
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
	// #895: health bookkeeping rides on each worker (addTrackedWorker), not on
	// the client, so it cannot be omitted by whoever builds the client.
	client, pool, err := buildRiverClient(ctx, r.Config, workers, nil)
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
	// #719: embedded hosts configure only QueueBilling on their client
	// (pkg/embedded contract), so per-merchant refresh jobs ride that queue —
	// no host changes; a single embedded merchant is one job per tick anyway.
	return r.addBillingWorkersToRegistry(ctx, workers, riverjobs.QueueBilling)
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

// SetRiverSchema records the schema the bound River client keeps its tables
// in, so out-of-client reads (the progress monitor) look in the same place.
func (r *Runtime) SetRiverSchema(schema string) {
	if r == nil {
		return
	}
	r.riverSchema = strings.TrimSpace(schema)
}

// riverSchemaOrDefault is the schema for every direct read of River's tables:
// the bound client's schema when a host injected one, else OpenRails' default.
func (r *Runtime) riverSchemaOrDefault() string {
	if r.riverSchema != "" {
		return r.riverSchema
	}
	return config.RiverSchema
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
