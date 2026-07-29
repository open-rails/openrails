package app

import (
	"database/sql"

	"context"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/pyth"
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
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/railresolve"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

const (
	standaloneRiverDefaultQueueMaxWorkers = 10
	standaloneRiverBillingQueueMaxWorkers = 20
	// #719: the global cap on concurrent per-merchant provider refresh jobs —
	// the simple, honest per-provider rate limit (thousands of merchants share
	// each provider's API budget; a small worker cap is the brake).
	standaloneRiverProviderRefreshQueueMaxWorkers = 4
)

// standaloneRiverSchema returns the schema for River tables when OpenRails
// constructs its own River client (standalone, or embedded with no injected
// client). River tables ALWAYS live in `public` (#545, config.RiverSchema) —
// River's own documented default — so the OpenRails billing schema stays free of
// non-portable runtime state (keeps the #544 data move a clean whole-schema
// dump). This is NO LONGER tied to db.schema (reversing the #165 coupling). In
// embedded/library mode a host that runs River injects its own client via
// embedded.SetRiverClient, and that client owns its schema; OpenRails never
// overrides it.
func standaloneRiverSchema(_ *config.Config) string {
	return config.RiverSchema
}

type runtimeOverrides struct {
	DB    *db.DB
	Redis *redis.Client
	Clock clockwork.Clock
}

// effectiveSolanaNetwork derives the Solana network purely from the test_mode
// axis — devnet under test_mode, mainnet otherwise. There is deliberately no
// override knob (#349): test_mode already answers the question.
func effectiveSolanaNetwork(cfg *config.Config) string {
	if cfg != nil && cfg.IsTestMode() {
		return "devnet"
	}
	return "mainnet"
}

// devnetParityPriceProvider wraps the Pyth client under test_mode (#360).
// Devnet money is fake and a devnet deployment must never require Hermes:
// feed-backed symbols are still priced via Hermes when it is reachable
// (realistic SOL quotes), but ANY failure — missing feed, network error,
// staleness — degrades to $1.00 parity instead of erroring.
type devnetParityPriceProvider struct {
	inner solanamodule.TokenPriceProvider
}

func (p devnetParityPriceProvider) PriceUSD(ctx context.Context, symbol string) (float64, error) {
	if p.inner != nil {
		if price, err := p.inner.PriceUSD(ctx, symbol); err == nil && price > 0 {
			return price, nil
		} else if err != nil {
			log.WithError(err).WithField("token", symbol).
				Debug("devnet: pyth price unavailable; using $1.00 parity (fake money)")
		}
	}
	return 1.0, nil
}

func createPythPriceProvider(cfg *config.Config) (solanamodule.TokenPriceProvider, error) {
	// Always constructed (#788): whether a merchant's Solana rail is armed is
	// per-merchant runtime state, not boot config; the client is a cheap
	// lazily-used HTTP wrapper.
	// Pyth is not configurable (#352): Hermes URL, freshness bounds and the
	// price-feed map are protocol constants.
	hermesURL := solanatokens.DefaultPythHermesURL
	maxPriceAgeText := solanatokens.DefaultPythMaxPriceAge
	maxConfidenceBPS := solanatokens.DefaultPythMaxConfidenceBPS
	priceFeeds := solanatokens.DefaultPythPriceFeeds()
	maxPriceAge, err := time.ParseDuration(strings.TrimSpace(maxPriceAgeText))
	if err != nil {
		return nil, fmt.Errorf("parse pyth max price age: %w", err)
	}
	client, err := pyth.NewClient(pyth.Config{
		HermesURL:        hermesURL,
		MaxPriceAge:      maxPriceAge,
		MaxConfidenceBPS: maxConfidenceBPS,
		PriceFeeds:       priceFeeds,
	})
	if err != nil {
		return nil, fmt.Errorf("create pyth client: %w", err)
	}
	if effectiveSolanaNetwork(cfg) == "devnet" {
		return devnetParityPriceProvider{inner: client}, nil
	}
	return client, nil
}

func buildRuntimeWithOverrides(cfg *config.Config, overrides *runtimeOverrides) (*Runtime, error) {
	// Create clock early so it can be passed to services.
	clock := runtimeClock(overrides)

	var (
		database    *db.DB
		redisClient *redis.Client
		err         error
	)
	if overrides != nil && overrides.DB != nil {
		if err = validateDatabase(cfg, overrides.DB); err != nil {
			return nil, err
		}
		database = overrides.DB
	} else {
		database, err = createDatabase(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create db: %w", err)
		}
	}

	// Surface (and, outside development, enforce) the Row Level Security
	// posture of the connected role (issue #227/#763): RLS policies only
	// constrain a non-superuser, non-BYPASSRLS role. This ONE call covers BOTH
	// construction paths above — a config-built standalone pool (createDatabase)
	// AND a host-injected embedded pool (overrides.DB) — so an embedded host
	// connecting as a privileged/BYPASSRLS role outside development fails boot
	// exactly like standalone does, with no separate gate to keep in sync.
	// Previously this ran only inside createDatabase, so embedded construction
	// (which always supplies overrides.DB) never hit it at all.
	if err := database.EnforceRLSPosture(context.Background(), cfg.RequiresRLS()); err != nil {
		return nil, err
	}

	redisOwned := false
	if overrides != nil && overrides.Redis != nil {
		redisClient = overrides.Redis
	} else {
		redisClient, err = createRedisClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis client: %w", err)
		}
		redisOwned = redisClient != nil
	}

	// Webhook-dedup posture (#678). Embedded hosts are identified by the injected
	// DB pool.
	if err := enforceWebhookDedupPosture(cfg, redisClient != nil, overrides != nil && overrides.DB != nil); err != nil {
		return nil, err
	}

	solanaPriceProvider, err := createPythPriceProvider(cfg)
	if err != nil {
		return nil, err
	}

	// #788 Layer C: ONE rail resolution seam for the whole runtime. The
	// merchants service late-binds (EnsureMerchantsService / manifest
	// provisioning / the standalone server arm it after build), so every
	// resolver closes over the runtime pointer assigned below.
	var runtimeRef *Runtime
	merchantsFn := func() *merchants.Service {
		if runtimeRef == nil {
			return nil
		}
		return runtimeRef.Merchants
	}
	railConfigs := railresolve.NewMerchantsSource(cfg, merchantsFn)
	// #725/#730/#788: the ONE store-armed per-merchant credential builder
	// (arrears/top-up adapters, manual-rebill + cancel NMI clients).
	collectionResolver := &money.MerchantCollectionAdapterBuilder{
		Config:      cfg,
		DB:          database,
		MerchantsFn: merchantsFn,
	}
	// #728/#788: per-merchant Solana RPC (poller, crank, intent verify legs,
	// request-plane transaction builds).
	solanaRPCResolver := &solanamodule.MerchantRPCBuilder{
		Config:      cfg,
		MerchantsFn: merchantsFn,
	}

	serviceInstances := createServices(database, cfg, railConfigs, collectionResolver, solanaRPCResolver, redisClient, clock, solanaPriceProvider)

	var emailService *subscriptions.EmailService
	if cfg.SendGrid != nil {
		if es, err := subscriptions.NewEmailService(cfg.SendGrid, merchantconfig.NewStore(database), clock); err != nil {
			log.WithError(err).Warn("EmailService init failed; email disabled")
		} else {
			emailService = es
			// Configure domain services for subscription emails
			emailService.SetDomainServices(
				serviceInstances.SubscriptionService,
				serviceInstances.ProductService,
				serviceInstances.PriceService,
				identity.NewProfilesDirectory(identity.NewProfileRepo(database)),
			)
		}
	}

	// Set emailService on the NotificationService that was created in createServices
	serviceInstances.NotificationService.SetEmailService(emailService)

	// #736 alerting: rule templates → #733 metrics queries, edge-triggered
	// evaluator, multi-channel delivery. The email channel reuses the same
	// SendGrid seam (nil emailService = email fails soft to a note). The
	// dashboard deep link in alert payloads is prefixed by the configured
	// frontend base URL when present.
	var alertEmailSender alerting.EmailSender
	if emailService != nil {
		alertEmailSender = emailService
	}
	alertService := alerting.NewService(alerting.Deps{
		DB:               database,
		Metrics:          serviceInstances.MetricsService,
		Email:            alertEmailSender,
		Clock:            clock,
		DashboardBaseURL: alertingDashboardBaseURL(cfg),
	})

	// Card-abuse guard (#371): escalates repeated card-charge failures to
	// captcha/block and detects site-wide card-testing attacks. Requires Redis
	// for its windowed counters; nil (safe no-op) otherwise.
	var cardAbuseGuard *abuse.CardAbuseGuard
	if redisClient != nil {
		cardAbuseGuard = abuse.NewCardAbuseGuard(
			ratelimit.NewLimiter(redisClient),
			captcha.NewChallengeStore(redisClient),
			abuse.DefaultCardAbuseConfig(),
		)
	}

	// #725/#788: collection adapters arm PER MERCHANT from the armed rail
	// state at charge time — no boot adapter map exists anymore.
	moneyCharger := money.NewScopedCharger(database, nil)

	runtime := &Runtime{
		DB:          database,
		RedisClient: redisClient,
		redisOwned:  redisOwned,
		Config:      cfg,
		Clock:       clock,
		// #746: one proxy-aware client-IP resolver, built once from config;
		// empty cfg.TrustedProxies yields a resolver that trusts nothing.
		TrustedProxies:       iputil.ParseTrustedProxies(cfg.TrustedProxies),
		AdmissionPolicyCache: admission.NewPolicyCache(0), // #513: default long TTL (config)
		RailConfigs:          railConfigs,

		SubscriptionService:      serviceInstances.SubscriptionService,
		ProductService:           serviceInstances.ProductService,
		PriceService:             serviceInstances.PriceService,
		NotificationService:      serviceInstances.NotificationService,
		PaymentMethodService:     serviceInstances.PaymentMethodService,
		PaymentService:           serviceInstances.PurchaseService,
		EntitlementService:       serviceInstances.EntitlementService,
		ProductAccessService:     serviceInstances.ProductAccessService,
		VaultService:             serviceInstances.VaultService,
		SolanaPayService:         serviceInstances.SolanaPayService,
		SolanaPayPoller:          serviceInstances.SolanaPayPoller,
		SolanaTransactionService: serviceInstances.SolanaTransactionService,
		SolanaPriceProvider:      solanaPriceProvider,
		FXProvider:               serviceInstances.FXProvider,
		FXRateRefresher:          serviceInstances.FXRateRefresher,

		UserSubscriptionService:   serviceInstances.UserSubscriptionService,
		PublicSubscriptionService: serviceInstances.PublicSubscriptionService,
		AdminSubscriptionService:  serviceInstances.AdminSubscriptionService,
		RepriceService:            serviceInstances.RepriceService,
		PlanMigrationService:      serviceInstances.PlanMigrationService,

		EmailService:                 emailService,
		SubscriptionLifecycleService: serviceInstances.SubscriptionLifecycleService,
		WebhookDispatcher:            serviceInstances.WebhookDispatcher,
		DeduplicationService:         serviceInstances.DeduplicationService,
		IdempotencyService:           serviceInstances.IdempotencyService,
		HTTPIdempotency:              serviceInstances.HTTPIdempotency,

		CheckoutService:        serviceInstances.CheckoutService,
		CheckoutSessionService: serviceInstances.CheckoutSessionService,
		CardAbuseGuard:         cardAbuseGuard,
		MoneyService:           serviceInstances.MoneyService,
		MetricsService:         serviceInstances.MetricsService,
		DashboardService:       serviceInstances.DashboardService,
		CopilotService:         serviceInstances.CopilotService,
		AlertService:           alertService,
		WebhookHealth:          &webhookhealth.Recorder{DB: database, Clock: clock},
		MoneyCharger:           moneyCharger,
		RailCustomerService:    serviceInstances.RailCustomerService,
	}

	// MODE 1 (#723): the in-memory credential plane exists from boot; manifest
	// provisioning seeds it and every store consumer reads it. No persistent
	// merchant-secret store is ever constructed in this mode.
	if cfg.IsManifestMerchantSource() {
		runtime.ManifestSecrets = merchants.NewManifestSecretStore()
	}

	// Arm the late-bound resolvers built above (#788): they close over
	// runtimeRef, so every post-boot Merchants bind is visible immediately.
	runtimeRef = runtime
	runtime.CollectionResolver = collectionResolver
	moneyCharger.SetAdapterResolver(collectionResolver)
	runtime.SolanaRPCResolver = solanaRPCResolver
	if serviceInstances.SolanaPayPoller != nil {
		serviceInstances.SolanaPayPoller.SetMerchantRPC(solanaRPCResolver)
	}

	// River producer is always initialized in the runtime so HTTP handlers can enqueue jobs
	// even when workers run in a separate process.
	if producer, pool, err := buildRiverProducer(cfg); err != nil {
		return nil, fmt.Errorf("init river producer: %w", err)
	} else {
		runtime.RiverProducer = producer
		runtime.riverProducerPool = pool
	}

	// Wire the deferred NMI delete schedulers (issue 216). Since #358 phase A
	// scheduling enqueues a durable nmi_delete_subscription intent on the
	// provider intent ledger (no River producer involved); the scheduled
	// intent executor drains it. Two instances of the one mechanism,
	// differing only in origin:
	//   - user-origin for user-asked cancellations (UserSubscriptionService):
	//     reactive completion, executes under mode=limited;
	//   - system-origin for dunning exhaustion (the lifecycle service shared
	//     by the webhook handlers, and Runtime.DeferredDeletes threaded into
	//     the dunning worker's per-run lifecycle): proactive, requires
	//     mode=full. The window-expiry path no longer deletes inline — every
	//     terminal cancellation funnels through the one ledger, so no
	//     double-delete is possible.
	// #732: user/admin destructive cancels pass the anti-credential-compromise
	// rate ceiling before their write-ahead intent is created. System-origin
	// deletes (dunning) skip it (the gate is inert for system origin).
	rateCeiling := runtime.RateCeiling()
	userDeferredDeletes := newIntentDeferredDeleteScheduler(database, rateCeiling, intents.OriginUser,
		"user cancellation retained an undo window; rail delete deferred to its close")
	systemDeferredDeletes := newIntentDeferredDeleteScheduler(database, nil, intents.OriginSystem,
		"terminal dunning failure; remote NMI subscription must stop rebilling")
	runtime.DeferredDeletes = systemDeferredDeletes
	if runtime.UserSubscriptionService != nil {
		runtime.UserSubscriptionService.SetDeferredDeleteScheduler(userDeferredDeletes)
		// #696: user CCBill cancels queue a durable ccbill_cancel_subscription
		// intent atomically with the local cancel. User-origin: reactive
		// completion, executes under mode=limited.
		runtime.UserSubscriptionService.SetCCBillCancelScheduler(intents.NewCCBillCancelScheduler(database, rateCeiling, intents.OriginUser,
			"user cancellation; remote CCBill subscription must stop rebilling"))
	}
	if runtime.SubscriptionLifecycleService != nil {
		runtime.SubscriptionLifecycleService.SetDeferredDeleteScheduler(systemDeferredDeletes)
	}

	// #684: fetch-and-converge wake-ups. Late-bound to the runtime so it works
	// whether the producer came from config or an embedded host's external
	// River client (SetExternalRiverClient).
	runtime.WebhookDispatcher.ConvergeEnqueuer = &runtimeConvergeEnqueuer{runtime: runtime}

	// #674: write-through provider intents. Producers post a durable intent and
	// execute it inline through the SAME registry/runner the scheduled
	// executor/verifier drains — one primitive, identical semantics.
	intentRunner := runtime.IntentRunner()
	if runtime.CheckoutService != nil {
		runtime.CheckoutService.Intents = intentRunner
		if runtime.CheckoutService.NMISaleService != nil {
			runtime.CheckoutService.NMISaleService.Intents = intentRunner
		}
		if runtime.CheckoutService.VaultedCardService != nil {
			runtime.CheckoutService.VaultedCardService.Intents = intentRunner
		}
	}
	// #674 tail: user-initiated payment-method deletes route through the
	// durable nmi_vault_delete intent.
	if runtime.VaultService != nil {
		runtime.VaultService.DeleteIntents = &intents.VaultDeleteThrough{Runner: intentRunner}
	}
	// #674: user/admin payment-method swaps route through the durable
	// nmi_payment_source_update intent (ambiguity ⇒ pending_verify, never a
	// silent local↔remote split).
	runtime.PaymentSourceUpdateIntents = &intents.PaymentSourceUpdateThrough{Runner: intentRunner, DB: database}

	return runtime, nil
}

func runtimeClock(overrides *runtimeOverrides) clockwork.Clock {
	if overrides != nil && overrides.Clock != nil {
		return overrides.Clock
	}
	return clockwork.NewRealClock()
}

func buildRiverProducer(cfg *config.Config) (*river.Client[pgx.Tx], *pgxpool.Pool, error) {
	if cfg.DB == nil {
		return nil, nil, fmt.Errorf("missing database configuration for River producer")
	}
	dbURL := cfg.DB.GetConnectionString()
	if dbURL == "" {
		return nil, nil, fmt.Errorf("missing database configuration for River producer (DB_URL or DB_HOST/DB_PORT/etc.)")
	}
	pool, err := db.NewPGXPoolWithRetry(context.Background(), dbURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating pgx pool for River producer: %w", err)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:              standaloneRiverSchema(cfg),
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River producer client: %w", err)
	}
	return client, pool, nil
}

func createDatabase(cfg *config.Config) (*db.DB, error) {
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, err
	}

	if err := validateDatabase(cfg, database); err != nil {
		return nil, err
	}
	// RLS posture (issue #227/#763) is enforced once, in buildRuntimeWithOverrides,
	// for both this path and the overrides.DB (embedded) path — see there.
	return database, nil
}

func validateDatabase(cfg *config.Config, database *db.DB) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}

	// Validate that all migrations have been applied before starting.
	// migratekit drives a database/sql handle; open a short-lived one over the
	// pgx stdlib driver.
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("database config is nil")
	}
	sqlDB, err := sql.Open("pgx", cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("open db for migration validation: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// Schema must mirror the apply step's WithSchema (#731): migratekit v1.2.0
	// filters the ledger by schema, so a schema-less source stops matching rows
	// applied via WithSchema.
	if err := migratekit.ValidatePostgresMigrations(context.Background(), sqlDB,
		migratekit.MigrationSource{App: config.MigratekitApp, FS: postgresmigrations.FS, Schema: cfg.DB.SchemaName()},
	); err != nil {
		log.WithError(err).Fatal("Postgres migrations validation failed")
		return err
	}

	return nil
}

func createRedisClient(cfg *config.Config) (*redis.Client, error) {
	if cfg.Redis == nil {
		return nil, nil
	}
	redisOpts := &redis.Options{
		Addr: cfg.Redis.Addr,
		DB:   cfg.Redis.DB,
	}
	if cfg.Redis.Password != "" {
		redisOpts.Password = cfg.Redis.Password
		log.Info("Redis authentication enabled")
	} else {
		log.Info("Redis authentication disabled - connecting without credentials")
	}
	client := redis.NewClient(redisOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Ping(ctx).Result(); err != nil {
		log.Warnf("Redis connection test failed: %v - rate limiting will fall back to permissive mode", err)
	} else {
		log.Info("Redis connection successful - rate limiting enabled")
	}
	return client, nil
}

// enforceWebhookDedupPosture (#678): dedup TRUTH now lives in Postgres
// (openrails.webhook_events), so running without Redis is CORRECT in any
// topology — a replica can never replay effects. It is merely wasteful: the
// pending-lease coordination degrades to per-process (concurrent duplicate
// deliveries burn work; #675 row locks keep them safe) and every dedup check
// pays a Postgres round-trip. The old hard boot-refusal existed because the
// memStore was per-process TRUTH; that reason is gone, so this is a loud
// warning now. Always returns nil (kept as error-shaped for the call site).
func enforceWebhookDedupPosture(cfg *config.Config, hasRedis, embeddedHost bool) error {
	if hasRedis {
		return nil
	}
	log.Warn(webhookDedupPostureWarning(cfg, embeddedHost))
	return nil
}

// webhookDedupPostureWarning is pure (no redis/db) so the message choice is unit testable.
func webhookDedupPostureWarning(cfg *config.Config, embeddedHost bool) string {
	if embeddedHost || cfg == nil || cfg.IsDev() {
		return "redis not configured: webhook dedup truth stays in Postgres (safe); lease coordination and the completed-key cache degrade to per-process memory (#678)"
	}
	return fmt.Sprintf(
		"redis not configured in standalone mode (env %q): webhook dedup truth stays in Postgres (safe), but multi-replica deployments lose cross-replica lease coordination and the fast-path cache — expect wasted duplicate processing attempts; configure redis (#678)",
		cfg.Env,
	)
}

type servicesInstances struct {
	SubscriptionService *subscriptions.SubscriptionService

	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	PurchaseService          *payments.PaymentService
	EntitlementService       *entitlements.EntitlementService
	ProductAccessService     *productaccess.Service
	VaultService             *paymentmethods.VaultService
	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaPriceProvider      solanamodule.TokenPriceProvider
	FXProvider               fx.Provider
	FXRateRefresher          interface {
		Stop()
		LastRefresh() time.Time
	}

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService
	// RepriceService is the #773 reprice primitive (move subscribers to a
	// different price at their next renewal).
	RepriceService *subscriptions.RepriceService
	// PlanMigrationService (#813) is the operator-driven cross-product bulk
	// migration over the reprice engine.
	PlanMigrationService *subscriptions.PlanMigrationService

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	DeduplicationService         *webhooks.DeduplicationService
	IdempotencyService           *idempotency.IdempotencyService
	// HTTPIdempotency is the client-facing Idempotency-Key replay store (#579).
	HTTPIdempotency   *idempotency.IdempotencyService
	WebhookDispatcher *webhooks.WebhookDispatcher

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService
	MoneyService           *money.MoneyService
	MetricsService         *metrics.Service
	DashboardService       *dashboard.Service
	CopilotService         *copilot.Service
	RailCustomerService    *payments.RailCustomerService
}

// alertingDashboardBaseURL builds the absolute base the #736 alert dashboard
// deep links hang off. Prefers the deployment's own APIURL (so emailed/webhook
// links are clickable); appends the /admin console mount when that SPA is
// enabled. Empty => the link is a console-relative path.
func alertingDashboardBaseURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	base := strings.TrimRight(cfg.APIURL, "/")
	if cfg.AdminConsole.IsEnabled() {
		return base + "/admin"
	}
	return base
}

func createServices(database *db.DB, cfg *config.Config, railConfigs railresolve.Source, collectionResolver *money.MerchantCollectionAdapterBuilder, solanaRPCResolver *solanamodule.MerchantRPCBuilder, redisClient *redis.Client, clock clockwork.Clock, solanaPriceProvider solanamodule.TokenPriceProvider) *servicesInstances {
	productService := catalog.NewProductService(database)
	priceService := catalog.NewPriceService(database)
	// NotificationService created with nil emailService - will be set later in buildRuntime
	notificationService := subscriptions.NewNotificationService(database, nil)
	paymentMethodService := paymentmethods.NewPaymentMethodService(database)
	purchaseService := payments.NewPaymentService(database, clock)
	entitlementService := entitlements.NewEntitlementService(database, clock)
	productAccessService := productaccess.NewService(database, clock)
	moneyService := money.NewMoneyService(database, clock)
	metricsService := metrics.NewService(database)
	// #741 dashboard: NL widget generation is armed only when an LLM key is
	// configured — nil LLM = the generate endpoint fail-closes with 501.
	// #756 metrics Q&A additionally requires the llm.ask_enabled consent
	// (aggregate query results flow to the provider) and is rate-limited
	// per merchant (Redis-backed when available, in-process fallback).
	var dashboardLLM dashboard.LLM
	if cfg.LLM.IsConfigured() {
		llmBaseURL := strings.TrimSpace(cfg.LLM.BaseURL)
		switch cfg.LLM.ResolvedProvider() {
		case config.LLMProviderOpenAI:
			dashboardLLM = dashboard.NewOpenAILLM(cfg.LLM.APIKey, cfg.LLM.ResolvedModel(), llmBaseURL)
		default: // anthropic — unknown providers refuse boot in config.Validate
			dashboardLLM = dashboard.NewAnthropicLLM(cfg.LLM.APIKey, cfg.LLM.ResolvedModel(), llmBaseURL)
		}
	}
	var askLimiter dashboard.AskLimiter
	if redisClient != nil {
		askLimiter = dashboard.NewAskLimiter(redisClient, clock.Now)
	} else {
		askLimiter = dashboard.NewAskLimiter(nil, clock.Now)
	}
	dashboardService := dashboard.NewService(dashboard.Deps{
		DB:         database,
		Metrics:    metricsService,
		LLM:        dashboardLLM,
		AskEnabled: cfg.LLM != nil && cfg.LLM.AskEnabled,
		AskLimiter: askLimiter,
		Clock:      clock,
	})
	railCustomerService := payments.NewRailCustomerService(database)
	profileRepo := identity.NewProfileRepo(database)

	// Create FX provider for Solana token quoting and policy-currency admission.
	// Runtime enforcement reads fresh cross-currency rates from Redis; same-currency
	// paths do not require FX.
	//
	// THIS IS THE DEFAULT FX PROVIDER for the whole app — LIVE rates, always on.
	// ExchangeAPIProvider uses the fawazahmed0 exchange-api (CC0, free, NO API key),
	// wrapped in a 5-minute in-memory cache, or (when Redis is present) a 3-hour
	// Redis cache with a background refresher. There is no config switch and no
	// NoOp fallback here: production never runs at a flat 1.0 rate.
	liveFX := fx.NewExchangeAPIProvider()
	var fxProvider fx.Provider = fx.NewCachedProvider(liveFX, 5*time.Minute)
	var fxRateRefresher interface {
		Stop()
		LastRefresh() time.Time
	}
	if redisClient != nil {
		redisFX := fx.NewRedisCachedProvider(redisClient, liveFX, 3*time.Hour)
		redisFX.Start(context.Background(), moneyutil.CurrencyCodes(), 2*time.Hour)
		fxProvider = redisFX
		fxRateRefresher = redisFX
	}

	// Note: solanaPayService and SolanaPayPoller need checkoutService, which is created later
	// We'll create solanaPayService with nil checkoutService and set it after checkoutService is created
	solanaPayService := solanamodule.NewSolanaPayService(database, redisClient, cfg, railConfigs, priceService, productService, nil, fxProvider, solanaPriceProvider, clock)
	solanaTransactionService := solanamodule.NewSolanaTransactionService(database, nil, cfg, priceService, fxProvider, clock)
	solanaTransactionService.SetMerchantRPC(solanaRPCResolver)

	subscriptionLifecycleService := subscriptions.NewSubscriptionLifecycleService(
		database,
		productService,
		priceService,
		entitlementService,
		notificationService,
		purchaseService, // For creating Payment records on renewal
		clock,
	)
	subscriptionLifecycleService.SetConfig(cfg)

	subscriptionService := subscriptions.NewSubscriptionService(
		database,
		priceService,
		productService,
		paymentMethodService,
		clock,
	)

	// #773: reprice primitive — moving existing subscribers to a different
	// (same-product, same-currency, active) price at their next renewal.
	repriceRepo := subscriptions.NewRepriceRepo(database)
	repriceService := subscriptions.NewRepriceService(database, repriceRepo, priceService, subscriptionService, notificationService, merchantconfig.NewStore(database), clock)

	// #779 catalog copilot: read-only Q&A always (gated on
	// llm.catalog_copilot_enabled); the Phase 2 draft_* tools additionally on
	// llm.catalog_drafting_enabled, which MUST stay off until #781 (server-
	// side notice-window enforcement) ships — see config.go's field doc.
	// Rides the SAME LLM client as the dashboard (one provider/model/key
	// config for the whole deployment); its own Redis-backed rate limiter
	// (own key namespace, never shares #756's budget).
	var copilotLimiter copilot.AskLimiter
	if redisClient != nil {
		copilotLimiter = copilot.NewAskLimiter(redisClient, clock.Now)
	} else {
		copilotLimiter = copilot.NewAskLimiter(nil, clock.Now)
	}
	copilotService := copilot.NewService(copilot.Deps{
		Products: productService,
		Prices:   priceService,
		Subs:     subscriptionService,
		Reprices: repriceService,
		LLM:      dashboardLLM,
		Enabled:  cfg.LLM != nil && cfg.LLM.CatalogCopilotEnabled,
		Drafting: cfg.LLM != nil && cfg.LLM.CatalogDraftingEnabled,
		Limiter:  copilotLimiter,
		Clock:    clock,
	})

	vaultService := paymentmethods.NewVaultService(paymentMethodService, subscriptionService, database, cfg, clock)
	subscriptionService.VaultService = vaultService
	idempotencyService := idempotency.NewIdempotencyService(redisClient)
	webhookIdempotencyService := idempotency.NewIdempotencyServiceWithTTL(redisClient, webhooks.WebhookIdempotencyTTL)
	// #579: a THIRD idempotency instance backs the client-facing Idempotency-Key
	// HTTP replay middleware, separate from the internal checkout dedup
	// (idempotencyService) and webhook dedup (webhookIdempotencyService) above.
	httpIdempotencyService := idempotency.NewIdempotencyServiceWithTTL(redisClient, idempotency.HTTPIdempotencyTTL)

	userSubscriptionService := subscriptions.NewUserSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		purchaseService,
		notificationService,
		entitlementService,
		collectionResolver,
		clock,
	)

	publicSubscriptionService := catalog.NewPublicSubscriptionService(
		productService,
		priceService,
	)

	adminSubscriptionService := subscriptions.NewAdminSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		entitlementService,
		notificationService,
		purchaseService,
		collectionResolver,
		clock,
	)
	adminSubscriptionService.StripeService = &subscriptions.StripeService{Config: cfg, Rails: railConfigs}

	// #813: plan migrations — cross-product bulk retirement over the #773
	// reprice engine. Observed rails with a server-side push: Stripe, and
	// (#815) gateway-native NMI recurring via the per-merchant client
	// resolver.
	planMigrationService := subscriptions.NewPlanMigrationService(repriceService, &subscriptions.StripeService{Config: cfg, Rails: railConfigs}, subscriptions.NewNMIPlanPusher(collectionResolver), paymentMethodService)

	// #678: Postgres (webhook_events) is the dedup truth; Redis is cache + lease coordination.
	deduplicationService := webhooks.NewDeduplicationService(webhookIdempotencyService, database)
	webhookDispatcher := &webhooks.WebhookDispatcher{
		Config:                       cfg,
		DB:                           database,
		Clock:                        clock,
		PriceService:                 priceService,
		ProductService:               productService,
		NotificationService:          notificationService,
		SubscriptionService:          subscriptionService,
		PaymentService:               purchaseService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		ProfileRepo:                  profileRepo,
		DeduplicationService:         deduplicationService,
		RailCustomerService:          railCustomerService,
		RailConfigs:                  railConfigs,
		MoneyService:                 moneyService,
	}

	// Create checkout service for unified checkout endpoint
	checkoutService := checkout.NewCheckoutService(
		subscriptionService,
		productService,
		priceService,
		purchaseService,
		entitlementService,
		paymentMethodService,
		vaultService,
		idempotencyService,
		railCustomerService,
		cfg,
		railConfigs,
		clock,
	)
	webhookDispatcher.PurchaseRegistrar = checkoutService
	// Wire durable product-access grants (issue #250) into the one-time purchase
	// flow. Additive to feature entitlements; nil-safe.
	if checkoutService.PurchaseService != nil {
		checkoutService.PurchaseService.SetProductAccessService(productAccessService)
		// Wire credit/currency balance grants (#472) into the one-time purchase
		// flow. Additive to feature entitlements; nil-safe.
		checkoutService.PurchaseService.SetMoneyService(moneyService)
	}
	checkoutSessionService := checkout.NewCheckoutSessionService(
		database,
		priceService,
		productService,
		paymentMethodService,
		idempotencyService,
		checkoutService,
		solanaPayService,
		solanaTransactionService,
		fxProvider,
		solanaPriceProvider,
		cfg,
		railConfigs,
		clock,
	)
	webhookDispatcher.CheckoutSessionService = checkoutSessionService
	solanaPayService.SetEligibilityChecker(&solanaEligibilityAdapter{service: checkoutService})

	// Create SolanaPayPoller (depends on checkoutService for RegisterPurchase)
	solanaPayPoller := solanamodule.NewSolanaPayPoller(
		database,
		redisClient,
		cfg,
		solanaPayService,
		solanaTransactionService,
		&solanaPurchaseRegistrarAdapter{service: checkoutService},
		purchaseService,
		checkoutSessionService,
	)

	return &servicesInstances{
		SubscriptionService:          subscriptionService,
		ProductService:               productService,
		PriceService:                 priceService,
		NotificationService:          notificationService,
		PaymentMethodService:         paymentMethodService,
		PurchaseService:              purchaseService,
		EntitlementService:           entitlementService,
		ProductAccessService:         productAccessService,
		VaultService:                 vaultService,
		SolanaPayService:             solanaPayService,
		SolanaPayPoller:              solanaPayPoller,
		SolanaTransactionService:     solanaTransactionService,
		SolanaPriceProvider:          solanaPriceProvider,
		FXProvider:                   fxProvider,
		FXRateRefresher:              fxRateRefresher,
		UserSubscriptionService:      userSubscriptionService,
		PublicSubscriptionService:    publicSubscriptionService,
		AdminSubscriptionService:     adminSubscriptionService,
		RepriceService:               repriceService,
		PlanMigrationService:         planMigrationService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		DeduplicationService:         deduplicationService,
		IdempotencyService:           idempotencyService,
		HTTPIdempotency:              httpIdempotencyService,
		WebhookDispatcher:            webhookDispatcher,
		CheckoutService:              checkoutService,
		CheckoutSessionService:       checkoutSessionService,
		MoneyService:                 moneyService,
		MetricsService:               metricsService,
		DashboardService:             dashboardService,
		CopilotService:               copilotService,
		RailCustomerService:          railCustomerService,
	}
}

func buildRiverClient(cfg *config.Config, workers *river.Workers, middleware []rivertype.Middleware) (*river.Client[pgx.Tx], *pgxpool.Pool, error) {
	if cfg.DB == nil {
		return nil, nil, fmt.Errorf("missing database configuration for River")
	}
	dbURL := cfg.DB.GetConnectionString()
	if dbURL == "" {
		return nil, nil, fmt.Errorf("missing database configuration for River (DB_URL or DB_HOST/DB_PORT/etc.)")
	}
	pool, err := db.NewPGXPoolWithRetry(context.Background(), dbURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating pgx pool for River: %w", err)
	}

	drv := riverpgxv5.New(pool)
	client, err := river.NewClient(drv, &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault:             {MaxWorkers: standaloneRiverDefaultQueueMaxWorkers},
			riverjobs.QueueBilling:         {MaxWorkers: standaloneRiverBillingQueueMaxWorkers},
			riverjobs.QueueProviderRefresh: {MaxWorkers: standaloneRiverProviderRefreshQueueMaxWorkers},
		},
		Schema:     standaloneRiverSchema(cfg),
		Workers:    workers,
		Middleware: middleware,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River client: %w", err)
	}
	return client, pool, nil
}

// runtimeConvergeEnqueuer adapts the runtime's enqueue-only River producer to
// webhooks.SubscriptionConvergeEnqueuer (#684), resolved at call time so a
// producer injected after runtime construction (embedded hosts) still works.
type runtimeConvergeEnqueuer struct {
	runtime *Runtime
}

func (e *runtimeConvergeEnqueuer) EnqueueSubscriptionConverge(ctx context.Context, req webhooks.ConvergeRequest) error {
	if e == nil || e.runtime == nil || e.runtime.RiverProducer == nil {
		return fmt.Errorf("subscription converge enqueuer: river producer unavailable")
	}
	return riverjobs.NewSubscriptionConvergeEnqueuer(e.runtime.RiverProducer).EnqueueSubscriptionConverge(ctx, req)
}
