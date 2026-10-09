package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-rails/openrails/internal/captcha"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
	"github.com/open-rails/openrails/internal/modules/copilot"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// Runtime aggregates infrastructure clients and application services.
type Runtime struct {
	// providerPosture holds the PSP credentials verified at load; Ready
	// reports any that are disarmed or still unknown.
	providerPosture providerposture.Tracked
	// background owns goroutines that reconnect optional providers; Close
	// stops them first.
	background backgroundTasks
	// redisState is the latest background Redis health observation.
	redisState dependencyState
	// signerIdentity records a Vault Transit key that no longer matches its
	// stored Solana identity.
	signerIdentity dependencyState
	// declaredCatalog records why Config.Catalog is not applied yet.
	declaredCatalog dependencyState
	// ApproveSolanaSigner, set by the embedded constructor, accepts the
	// identity a changed Transit signer now reports (hosttools.ApproveSolanaSigner).
	ApproveSolanaSigner func(ctx context.Context, merchantID billing.MerchantID, key string) error
	// posturePending counts loaded PSPs whose verdict is not yet known; -1
	// until StartProviderPosture's first pass completes.
	posturePending atomic.Int64
	// NMIPostureV5BaseURL is a test-only seam for the startup sandbox probe.
	NMIPostureV5BaseURL string
	// NMIClients is the single PSP-scoped NMI client factory (#1055).
	NMIClients *railresolve.NMIFactory

	StripeClients *stripeapi.Factory
	DB            *db.DB
	// leaseDB is the small pool idempotency lease renewals use (#1099).
	leaseDB     *db.DB
	RedisClient *redis.Client
	// redisOwned marks a self-dialed client; injected clients are borrowed and
	// must never be closed here (the host owns their lifecycle).
	redisOwned bool
	Config     *config.Config

	// configuredMerchant scopes single-merchant embedded runtimes. Constructor
	// declarations bind before HTTP and worker startup; standalone remains zero.
	// Atomic reads also protect privileged restore/bootstrap integration. Readers
	// use ConfiguredMerchant rather than caching a construction-time snapshot.
	configuredMerchant atomic.Pointer[billing.MerchantID]

	// TrustedProxies is the boot-configured proxy-aware client-IP resolver
	// (#746), built once from config.Config.TrustedProxies. A nil/empty
	// resolver trusts nothing (raw socket peer only). Every consumer that
	// needs "the request's real client IP" — rate-limit subject keys, abuse
	// tracking, webhook IPAddress recording, the CCBill IP allowlist —
	// resolves through this ONE instance instead of reading
	// RemoteAddr/X-Forwarded-For itself.
	TrustedProxies *iputil.TrustedProxies

	// ReservedAPIHosts are the hostnames the deployment itself serves: its
	// public billing URL, console and AuthKit issuer. No merchant may claim one
	// as its api_host. Written before serving (ReserveAPIHosts).
	ReservedAPIHosts []string
	// DNSResolver answers api_host proof lookups (#1107); nil is the system
	// resolver.
	DNSResolver *net.Resolver

	// RouteCapabilities is the advisory, boot-probed view of what OpenRails can
	// actually do (#661), used to gate the provider route surface. Nil means
	// unprobed → the surface stays permissive (no capability gating).
	RouteCapabilities *routesurface.RuntimeCapabilities

	Clock clockwork.Clock
	// RiverProducer inserts jobs: the bound fleet, OpenRails' own or the
	// host's, else the insert-only client New binds in the same schema.
	RiverProducer  *river.Client[pgx.Tx]
	insertProducer *river.Client[pgx.Tx]
	// ProviderRefreshQueue is where per-merchant provider refresh jobs run.
	ProviderRefreshQueue string
	RiverClient          *river.Client[pgx.Tx]
	// CatalogEdits is whether catalog writes reach callers other than the
	// process owner; the merchant API's mount decides it.
	CatalogEdits *catalogpolicy.Exposure

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

	UserSubscriptionService  *subscriptions.UserSubscriptionService
	AdminSubscriptionService *subscriptions.AdminSubscriptionService

	EmailService *subscriptions.EmailService
	// EmailSender delivers all of OpenRails' email; nil sends none.
	EmailSender config.EmailSender

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
	// AlertService delivers immediate merchant notifications and manages webhooks.
	AlertService *alerting.Service
	// WebhookHealth records inbound-webhook liveness per (merchant, rail) at the
	// ingest verify seam (#786). Nil-safe: recording never fails a webhook.
	WebhookHealth *webhookhealth.Recorder

	MoneyCharger          money.Charger
	RailCustomerService   *payments.RailCustomerService
	Merchants             *merchants.Service
	VaultClient           *vaultapi.Client
	MerchantSecretBackend *merchantsecrets.Store
	// ManifestSecrets is the MODE-1 in-memory credential plane (#723), set iff
	// merchant_config_source=manifest. Boot provisioning seeds it (Seeder()); runtime
	// consumers read it through Merchants like any other store. The DB/Vault
	// store is never constructed in this mode.
	ManifestSecrets *merchants.ManifestSecretStore
	// CollectionResolver is the ONE #725/#788 store-armed per-merchant
	// credential resolver (invoice collection adapters + NMI clients for rebills,
	// cancels, refunds and admin actions).
	CollectionResolver money.CollectionPlane
	// RailConfigs is the ONE Layer-C rail resolution seam (#788): every
	// decision-time rail credential/armed-state read resolves the ctx
	// merchant's psps row + scoped secrets through it.
	RailConfigs railresolve.Source

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
	// SolanaPlanService publishes on-chain recurring plans (#254). Injected by
	// the composition root alongside the cranker; nil -> recurring is not
	// configured.
	SolanaPlanService *recurring.PlanService
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

	CheckoutService        *checkout.CheckoutService
	CheckoutAttemptService *checkout.CheckoutAttemptService
	// CheckoutSessions stores checkout sessions (#1124).
	CheckoutSessions *checkoutsession.Store

	// CardAbuseGuard escalates repeated card-charge failures to a captcha, and
	// to a captcha for everyone while the ledger reports an attack (#371). Nil
	// without Redis or a captcha (safe no-op).
	CardAbuseGuard *abuse.CardAbuseGuard
	// CaptchaStore is the process's one captcha challenge store, shared by the
	// card-abuse guard and every HTTP surface so a solve clears it everywhere.
	CaptchaStore *captcha.ChallengeStore
	// CardFailureLedger is the PostgreSQL card-testing ledger (SEC-30),
	// enforced on every replica.
	CardFailureLedger *abuse.FailureLedger

	riverCompositionMu     sync.Mutex
	riverCompositionSealed bool
	riverClosed            atomic.Bool
	riverContributions     []riverhelpers.Contribution
	riverCompositionFailed bool
	riverStarted           bool
	workerConsumerRunning  atomic.Bool
	externalRiverClient    bool
	riverSchema            string // Config.Database.RiverSchema's, or the bound host client's

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
	DeferredDeletes subscriptions.ProviderCancelScheduler
	// Verifier reads unverified subscriptions from their provider as soon as
	// they become unverified (#1094); started with the billing workers.
	Verifier *reconcile.Verifier
}

// ReserveAPIHosts adds the hostnames urls name to ReservedAPIHosts.
func (r *Runtime) ReserveAPIHosts(urls ...string) {
	for _, raw := range urls {
		if host := merchants.DeploymentHost(raw); host != "" && !slices.Contains(r.ReservedAPIHosts, host) {
			r.ReservedAPIHosts = append(r.ReservedAPIHosts, host)
		}
	}
}

// ConfiguredMerchant returns the current single-merchant binding. Zero means
// the caller must explicitly select a merchant through its authority.
func (r *Runtime) ConfiguredMerchant() billing.MerchantID {
	if r == nil {
		return billing.MerchantID{}
	}
	if id := r.configuredMerchant.Load(); id != nil {
		return *id
	}
	return billing.MerchantID{}
}

// SetConfiguredMerchant binds constructor and privileged restore/bootstrap
// integrations atomically with respect to request readers.
func (r *Runtime) SetConfiguredMerchant(id billing.MerchantID) {
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
	r.riverCompositionMu.Lock()
	if r.riverClosed.Load() {
		r.riverCompositionMu.Unlock()
		return nil
	}
	r.riverClosed.Store(true)
	r.riverCompositionSealed = true
	r.riverCompositionMu.Unlock()

	r.background.stop()
	var errs []error
	if r.MerchantSecretBackend != nil {
		defer r.MerchantSecretBackend.Close()
	}

	// #895: stop the out-of-River progress detector first — it outlives the
	// River client on purpose, so nothing else will cancel it.
	r.stopRiverProgressMonitor()

	if r.Verifier != nil {
		r.Verifier.Close()
	}

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
		r.workerConsumerRunning.Store(false)
		log.Info("Stopping River background workers...")
		if err := r.RiverClient.Stop(ctx); err != nil {
			// Stop may return before workers finish when ctx is canceled.
			// Join cancellation before closing worker pools or attached services.
			if stopErr := r.RiverClient.StopAndCancel(context.Background()); stopErr != nil {
				errs = append(errs, fmt.Errorf("cancel River workers: %w", stopErr))
			}
			// During shutdown, Stop can surface context cancellation if the passed ctx is canceled.
			// Treat this as an expected shutdown condition.
			if !errors.Is(err, context.Canceled) {
				errs = append(errs, fmt.Errorf("failed to stop River client: %w", err))
			}
		}
		r.riverStarted = false
	}
	if r.leaseDB != nil {
		_ = r.leaseDB.Close()
		r.leaseDB = nil
	}
	if r.DB != nil {
		if err := r.DB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close db: %w", err))
		}
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

// AddRiverContribution attaches optional component jobs before RiverJobs seals
// the component set. The shared River composer performs registration and binding together.
func (r *Runtime) AddRiverContribution(jobs riverhelpers.Contribution) error {
	if r == nil {
		return fmt.Errorf("runtime is nil")
	}
	r.riverCompositionMu.Lock()
	defer r.riverCompositionMu.Unlock()
	if err := r.riverConfigurableLocked(); err != nil {
		return err
	}
	r.riverContributions = append(r.riverContributions, jobs)
	return nil
}

// InitRiver binds OpenRails' own River client, without starting it, in the
// runtime's River schema, which runtime construction migrated; it runs no
// DDL. The client is composed from the same contributions a host fleet takes.
// A process that only serves HTTP binds its producers this way; StartWorkers
// starts it.
func (r *Runtime) InitRiver(ctx context.Context) error {
	r.riverCompositionMu.Lock()
	switch {
	case r.riverClosed.Load():
		r.riverCompositionMu.Unlock()
		return fmt.Errorf("client is closed")
	case r.riverCompositionFailed:
		r.riverCompositionMu.Unlock()
		return fmt.Errorf("River composition failed; recreate the client")
	case r.externalRiverClient:
		r.riverCompositionMu.Unlock()
		return errHostFleet
	case r.RiverClient != nil:
		r.riverCompositionMu.Unlock()
		return nil
	case r.riverCompositionSealed:
		r.riverCompositionMu.Unlock()
		return errHostFleet
	}
	r.riverCompositionMu.Unlock()
	if r.DB == nil || r.DB.Pool() == nil {
		return fmt.Errorf("River requires the runtime PostgreSQL pool")
	}
	_, err := riverhelpers.New(ctx, r.DB.Pool(), &river.Config{Schema: r.riverSchemaOrDefault()}, r.riverJobs(false))
	return err
}

var errHostFleet = fmt.Errorf("RiverJobs is composed into a host fleet; pass it to Start with WithRiverClient")

// RunWorkers runs StartWorkers until ctx is done: on the host fleet bound to
// RiverJobs, else on OpenRails' own River.
func (r *Runtime) RunWorkers(ctx context.Context) error {
	var fleet *river.Client[pgx.Tx]
	if r != nil && r.externalRiverClient {
		fleet = r.RiverClient
	}
	stop, err := r.StartWorkers(ctx, fleet)
	if err != nil {
		return err
	}
	<-ctx.Done()
	stop()
	return ctx.Err()
}

// StartWorkers starts OpenRails' work and returns once it runs: the River
// fleet, the non-River loops (the Solana Pay poller) and the River progress
// monitor. With fleet nil it binds and starts OpenRails' own River client.
// Otherwise fleet is the host's client, composed with RiverJobs; the host
// starts and stops it. ctx bounds the workers' lifetime; stop joins the loops
// so the host can then stop its fleet and close pools.
func (r *Runtime) StartWorkers(ctx context.Context, fleet *river.Client[pgx.Tx]) (stop func(), err error) {
	if r == nil {
		return nil, fmt.Errorf("runtime is nil")
	}
	if r.riverClosed.Load() {
		return nil, fmt.Errorf("runtime is closed")
	}
	if fleet != nil {
		r.riverCompositionMu.Lock()
		bound := r.externalRiverClient && r.RiverClient == fleet
		failed := r.riverCompositionFailed
		r.riverCompositionMu.Unlock()
		switch {
		case failed:
			return nil, fmt.Errorf("River composition failed; recreate the client")
		case !bound:
			return nil, fmt.Errorf("WithRiverClient takes the fleet riverhelpers.New built with RiverJobs")
		}
	} else if err := r.InitRiver(ctx); err != nil {
		return nil, err
	}
	loopCtx, stopLoops := context.WithCancel(ctx)
	var pollerDone chan struct{}
	if r.SolanaPayPoller != nil {
		pollerDone = make(chan struct{})
		go func() { defer close(pollerDone); r.SolanaPayPoller.Start(loopCtx) }()
	}
	r.StartRiverProgressMonitor(ctx)
	joinLoops := func() {
		stopLoops()
		if pollerDone != nil {
			<-pollerDone
		}
	}
	if fleet != nil {
		return joinLoops, nil
	}
	if r.RiverClient == nil {
		joinLoops()
		return nil, fmt.Errorf("river client not initialized")
	}
	r.riverStarted = true
	log.Info("Starting River background workers")
	if err := r.RiverClient.Start(ctx); err != nil {
		r.riverStarted = false
		joinLoops()
		return nil, err
	}
	r.workerConsumerRunning.Store(true)
	return func() {
		r.workerConsumerRunning.Store(false)
		joinLoops()
	}, nil
}

// GetBillingPeriodicJobs returns billing's periodic jobs for external River client setup.
// This is used by embedded hosts who want to add billing's periodic jobs to their client.
func (r *Runtime) GetBillingPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error) {
	return r.buildRiverPeriodicJobs(ctx)
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
// the bound client's schema, else the one OpenRails' own River uses.
func (r *Runtime) riverSchemaOrDefault() string {
	if r.riverSchema != "" {
		return r.riverSchema
	}
	return config.RiverSchemaName(r.Config)
}

// RiverBound reports whether a River fleet is bound: OpenRails' own
// (InitRiver) or the host's (RiverJobs).
func (r *Runtime) RiverBound() bool {
	if r == nil {
		return false
	}
	r.riverCompositionMu.Lock()
	defer r.riverCompositionMu.Unlock()
	return r.RiverClient != nil
}

// BindRiverProducer binds an insert-only River client in the runtime's River
// schema, so jobs queued before Start wait there for whichever fleet runs
// them. Runtime construction created River's tables there.
func (r *Runtime) BindRiverProducer(ctx context.Context) error {
	schema := r.riverSchemaOrDefault()
	producer, err := river.NewClient(riverpgxv5.New(r.DB.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true})
	if err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	if err := r.DB.ValidateRiverJobBinding(ctx, r.DB.Pool(), schema); err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	r.riverCompositionMu.Lock()
	defer r.riverCompositionMu.Unlock()
	r.insertProducer, r.RiverProducer = producer, producer
	r.DB.SetRiverJobInserter(producer)
	return nil
}

// SetSolanaCranker injects the recurring Solana cranker built once the merchant
// secret store is available (composition root). It must be called before
// InitRiver so the cranker worker picks it up.
func (r *Runtime) SetSolanaCranker(cranker *recurring.CrankService) {
	r.SolanaCranker = cranker
}

// SetSolanaPlanService injects the plan-publish service (#254) built once the
// merchant secret store is available (composition root).
func (r *Runtime) SetSolanaPlanService(plan *recurring.PlanService) {
	r.SolanaPlanService = plan
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
