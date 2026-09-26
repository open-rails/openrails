package app

import (
	"database/sql"
	"net/http"

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
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/pyth"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/abuse"
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
)

const (
	standaloneRiverDefaultQueueMaxWorkers = 10
	standaloneRiverBillingQueueMaxWorkers = 20
	// #719: the global cap on concurrent per-merchant provider refresh jobs —
	// the simple, honest per-provider rate limit (thousands of merchants share
	// each provider's API budget; a small worker cap is the brake).
	standaloneRiverProviderRefreshQueueMaxWorkers = 4
	// idempotencyLeaseConns sizes the lease-renewal pool: one short UPDATE per
	// held claim every quarter lease.
	idempotencyLeaseConns = 4
)

type runtimeOverrides struct {
	StripeTransport  http.RoundTripper
	NMITransport     http.RoundTripper
	HostRiver        bool
	RiverSchema      string
	DB               *db.DB
	Redis            *redis.Client
	Clock            clockwork.Clock
	UserDirectory    openrails.UserDirectory
	UsernameResolver openrails.UsernameResolver
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

func buildRuntimeWithOverrides(ctx context.Context, cfg *config.Config, overrides *runtimeOverrides) (_ *Runtime, buildErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
		database, err = createDatabase(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create db: %w", err)
		}
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
	var stripeTransport http.RoundTripper
	if api := cfg.SandboxStripeAPIURL(); api != "" {
		stripeTransport = stripeapi.HostRewriteTransport(api)
	}
	if overrides != nil && overrides.StripeTransport != nil {
		stripeTransport = overrides.StripeTransport
	}
	stripeClients := stripeapi.NewFactory(stripeTransport)
	// #1055: the ONE PSP-scoped NMI client factory every consumer shares.
	nmiClients := &railresolve.NMIFactory{Config: cfg}
	if overrides != nil {
		nmiClients.Transport = overrides.NMITransport
	}
	// #725/#730/#788: the ONE store-armed per-merchant credential builder
	// (invoice collection adapters, manual-rebill + cancel NMI clients).
	collectionResolver := &money.MerchantCollectionAdapterBuilder{
		StripeClients: stripeClients,
		Config:        cfg,
		DB:            database,
		MerchantsFn:   merchantsFn,
		NMIClients:    nmiClients,
	}
	// #728/#788: per-merchant Solana RPC (poller, crank, intent verify legs,
	// request-plane transaction builds).
	solanaRPCResolver := &solanamodule.MerchantRPCBuilder{
		Config:      cfg,
		MerchantsFn: merchantsFn,
		Endpoint:    cfg.SandboxSolanaRPCURL(),
	}

	var userDirectory openrails.UserDirectory
	var usernameResolver openrails.UsernameResolver
	if overrides != nil {
		userDirectory = overrides.UserDirectory
		usernameResolver = overrides.UsernameResolver
	}
	// #1099: idempotency leases renew on their own connections, so a pool
	// saturated by the requests holding them can never starve a renewal.
	leaseDB, err := database.SeparatePool(ctx, idempotencyLeaseConns)
	if err != nil {
		return nil, fmt.Errorf("idempotency lease pool: %w", err)
	}
	defer func() {
		if buildErr != nil {
			_ = leaseDB.Close()
		}
	}()
	serviceInstances, err := createServices(database, leaseDB, cfg, railConfigs, collectionResolver, solanaRPCResolver, redisClient, clock, solanaPriceProvider, usernameResolver, stripeClients)
	if err != nil {
		return nil, err
	}

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
				// OpenRails does not own the host identity schema. A host that
				// wants subscription emails must wire its UserDirectory through
				// its own integration boundary; leaving this nil makes email
				// lookup fail closed instead of silently depending on AuthKit's
				// profiles tables or grants.
				userDirectory,
			)
		}
	}

	// Set emailService on the NotificationService that was created in createServices
	serviceInstances.NotificationService.SetEmailService(emailService)

	// Immediate merchant notifications share the existing email service.
	var alertEmailSender alerting.EmailSender
	if emailService != nil {
		alertEmailSender = emailService
	}
	alertService := alerting.NewService(alerting.Deps{
		DB:               database,
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

	// SEC-30: the durable card-testing ledger works on every replica, Redis or not.
	cardFailureLedger := abuse.NewFailureLedger(database, clock, abuse.DefaultCardAbuseConfig())
	serviceInstances.CheckoutSessionService.SetCardFailureLedger(cardFailureLedger)

	// #725/#788: collection adapters arm PER MERCHANT from the armed rail
	// state at charge time — no boot adapter map exists anymore.
	moneyCharger := money.NewScopedCharger(database, nil)

	runtime := &Runtime{
		StripeClients: stripeClients,
		DB:            database,
		leaseDB:       leaseDB,
		RedisClient:   redisClient,
		redisOwned:    redisOwned,
		Config:        cfg,
		Clock:         clock,
		// #746: one proxy-aware client-IP resolver, built once from config;
		// empty yields a resolver that trusts nothing. Cloudflare peers (ak#298)
		// are trusted for X-Forwarded-For here exactly as AuthKit trusts them.
		TrustedProxies: iputil.ParseTrustedProxies(append(append([]string(nil), cfg.TrustedProxies...), cfg.CloudflareProxies...)),
		RailConfigs:    railConfigs,

		SubscriptionService:      serviceInstances.SubscriptionService,
		ProductService:           serviceInstances.ProductService,
		PriceService:             serviceInstances.PriceService,
		NotificationService:      serviceInstances.NotificationService,
		PaymentMethodService:     serviceInstances.PaymentMethodService,
		PaymentService:           serviceInstances.PurchaseService,
		EntitlementService:       serviceInstances.EntitlementService,
		ProductAccessService:     serviceInstances.ProductAccessService,
		RailPaymentMethodService: serviceInstances.RailPaymentMethodService,
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

		CheckoutService:        serviceInstances.CheckoutService,
		CheckoutSessionService: serviceInstances.CheckoutSessionService,
		CardAbuseGuard:         cardAbuseGuard,
		CardFailureLedger:      cardFailureLedger,
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
	if cfg.SecretStoreBackend() == config.SecretBackendSnapshot {
		runtime.ManifestSecrets, err = merchants.NewManifestSecretStoreWithIdentity(cfg.CredentialSnapshotID)
		if err != nil {
			return nil, fmt.Errorf("initialize snapshot credential store: %w", err)
		}
	}

	// Arm the late-bound resolvers built above (#788): they close over
	// runtimeRef, so every post-boot Merchants bind is visible immediately.
	runtimeRef = runtime
	runtime.CollectionResolver = collectionResolver
	moneyCharger.SetAdapterResolver(collectionResolver)
	// A declared loopback NMI gateway (config.ProviderSandbox) reaches every
	// NMI client through the shared factory.
	runtime.NMIClients = nmiClients
	if serviceInstances.CheckoutService != nil {
		serviceInstances.CheckoutService.NMIClients = nmiClients
	}
	if serviceInstances.CheckoutSessionService != nil {
		serviceInstances.CheckoutSessionService.SetPSPPosture(runtime.PSPPostureDisarmed)
	}
	if runtime.RailPaymentMethodService != nil {
		runtime.RailPaymentMethodService.NMIClients = nmiClients
	}
	runtime.SolanaRPCResolver = solanaRPCResolver
	// #817: decimals come from the SPL mint on-chain, read through the same
	// per-merchant chain reader and cached (mint decimals are immutable).
	runtime.SolanaMintDecimals = solanamodule.NewMintDecimals(solanaRPCResolver.ChainReader())
	if serviceInstances.SolanaPayService != nil {
		serviceInstances.SolanaPayService.SetMintDecimals(runtime.SolanaMintDecimals)
	}
	if serviceInstances.CheckoutSessionService != nil {
		serviceInstances.CheckoutSessionService.SetSolanaMintDecimals(runtime.SolanaMintDecimals)
	}
	if serviceInstances.SolanaPayPoller != nil {
		serviceInstances.SolanaPayPoller.SetMerchantRPC(solanaRPCResolver)
	}

	// Managed HTTP processes need a producer even when their workers run
	// elsewhere. A host-owned fleet publishes its one producer only after shared River composition;
	// construction must never target an undeclared public queue in the meantime.
	if overrides != nil {
		runtime.SetRiverSchema(overrides.RiverSchema)
		runtime.hostRiver = overrides.HostRiver
	}
	if !runtime.hostRiver {
		if producer, pool, err := buildRiverProducer(ctx, cfg, runtime.riverSchemaOrDefault()); err != nil {
			return nil, fmt.Errorf("init river producer: %w", err)
		} else {
			if err := runtime.DB.ValidateRiverJobBinding(ctx, pool, producer.Schema()); err != nil {
				pool.Close() // This producer pool was created internally above.
				return nil, fmt.Errorf("init River producer binding: %w", err)
			}
			runtime.RiverProducer = producer
			runtime.DB.SetRiverJobInserter(producer)
			runtime.riverProducerPool = pool
		}
	}

	// Wire the deferred NMI delete schedulers (issue 216). Since #358 phase A
	// scheduling enqueues a durable nmi_delete_subscription intent on the
	// provider intent ledger and River transactionally; the operation
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
	// #732: every destructive cancel passes the rate ceiling before its
	// write-ahead intent is created — user/admin on the deployment-wide
	// anti-credential-compromise ceilings, system on the per-merchant automation
	// ceiling (or#842: the system scheduler used to pass nil, so the paths that
	// queue the most irreversible work were the only ungated ones).
	rateCeiling := runtime.RateCeiling()
	userDeferredDeletes := newProviderCancelScheduler(database, rateCeiling, intents.OriginUser,
		"user cancellation; the provider schedule must stop billing")
	systemDeferredDeletes := newProviderCancelScheduler(database, rateCeiling, intents.OriginSystem,
		"terminal lifecycle outcome; the provider schedule must stop billing")
	runtime.DeferredDeletes = systemDeferredDeletes
	if runtime.UserSubscriptionService != nil {
		runtime.UserSubscriptionService.SetProviderCancelScheduler(userDeferredDeletes)
	}
	if runtime.SubscriptionLifecycleService != nil {
		runtime.SubscriptionLifecycleService.SetProviderCancelScheduler(systemDeferredDeletes)
	}
	// or#896: a merchant-initiated cancel goes through the same durable
	// intents as the user path — admin-origin (a human asked for it, so it
	// executes under mode=limited like the user cancel) and rate-ceiling gated.
	if runtime.AdminSubscriptionService != nil {
		runtime.AdminSubscriptionService.SetProviderCancelScheduler(newProviderCancelScheduler(database, rateCeiling, intents.OriginAdmin,
			"merchant-initiated cancellation; the provider schedule must stop billing"))
	}

	// #684: fetch-and-converge wake-ups. Late-bound to the runtime so it works
	// whether the producer came from config or an embedded host's external
	// River client (shared River composition).
	runtime.WebhookDispatcher.ConvergeEnqueuer = &runtimeConvergeEnqueuer{runtime: runtime}

	// #674: write-through provider intents. Producers post a durable intent and
	// execute it inline through the SAME registry/runner the scheduled
	// executor/verifier drains — one primitive, identical semantics.
	runtime.ProviderCutovers = &intents.NMIProviderCutover{DB: database, Resolver: runtime.CollectionResolver, Clock: clock}
	runtime.EngineTakeovers = &intents.NMIEngineTakeover{DB: database, Resolver: runtime.CollectionResolver, Clock: clock}
	intentRunner := runtime.IntentRunner()
	if runtime.CheckoutService != nil {
		runtime.CheckoutService.Intents = intentRunner
		if runtime.CheckoutService.NMISaleService != nil {
			runtime.CheckoutService.NMISaleService.Intents = intentRunner
			runtime.CheckoutService.NMISaleService.Config = runtime.Config
			if engines, ok := runtime.CollectionResolver.(intents.StripeEngineServiceResolver); ok {
				runtime.CheckoutService.NMISaleService.StripeEngines = engines
			}
		}
		if runtime.CheckoutService.CustodianSaleService != nil {
			runtime.CheckoutService.CustodianSaleService.Intents = intentRunner
		}
	}
	// #674 tail: user-initiated payment-method deletes route through the
	// durable nmi_vault_delete intent.
	if runtime.RailPaymentMethodService != nil {
		runtime.RailPaymentMethodService.DeleteIntents = &intents.PaymentMethodDeleteThrough{Runner: intentRunner}
		runtime.RailPaymentMethodService.UpdateIntents = &intents.PaymentMethodUpdateThrough{Runner: intentRunner, DB: database}
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

func buildRiverProducer(ctx context.Context, cfg *config.Config, schema string) (*river.Client[pgx.Tx], *pgxpool.Pool, error) {
	if cfg.DB == nil {
		return nil, nil, fmt.Errorf("missing database configuration for River producer")
	}
	dbURL := cfg.DB.GetConnectionString()
	if dbURL == "" {
		return nil, nil, fmt.Errorf("missing database configuration for River producer (DB_URL or DB_HOST/DB_PORT/etc.)")
	}
	pool, err := db.NewPGXPoolWithRetry(ctx, dbURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating pgx pool for River producer: %w", err)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:              schema,
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River producer client: %w", err)
	}
	return client, pool, nil
}

func createDatabase(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	database, err := db.NewDB(ctx, cfg.DB)
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
	//
	// Never Fatal here: this runs inside embed.New, so os.Exit would take the
	// HOST process down on a library precondition. Return the error and let the
	// host refuse to boot with it.
	if err := migratekit.ValidatePostgresMigrations(context.Background(), sqlDB,
		migratekit.MigrationSource{App: config.MigratekitApp, FS: postgresmigrations.FS, Schema: cfg.DB.SchemaName()},
	); err != nil {
		log.WithError(err).Error("Postgres migrations validation failed")
		return err
	}

	// The converse check rejects a database recording migrations this
	// build no longer carries. migratekit only asks "embedded ⊆ applied", which a
	// re-squashed chain satisfies while applying nothing, freezing the schema at
	// the old shape. This is the only migration seam an embedded host cannot skip.
	if err := migrate.AssertNoOrphanedPostgresMigrations(context.Background(), sqlDB, cfg.DB.SchemaName()); err != nil {
		log.WithError(err).Error("OpenRails migration drift: refusing to start")
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

type servicesInstances struct {
	SubscriptionService *subscriptions.SubscriptionService

	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	PurchaseService          *payments.PaymentService
	EntitlementService       *entitlements.EntitlementService
	ProductAccessService     *productaccess.Service
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
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

	WebhookDispatcher *webhooks.WebhookDispatcher

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService
	MoneyService           *money.MoneyService
	MetricsService         *metrics.Service
	DashboardService       *dashboard.Service
	CopilotService         *copilot.Service
	RailCustomerService    *payments.RailCustomerService
}

// alertingDashboardBaseURL uses the independently configured admin destination.
// Empty leaves alert links relative to the consuming console.
func alertingDashboardBaseURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimRight(cfg.DashboardBaseURL, "/")
}

func createServices(database, leaseDB *db.DB, cfg *config.Config, railConfigs railresolve.Source, collectionResolver *money.MerchantCollectionAdapterBuilder, solanaRPCResolver *solanamodule.MerchantRPCBuilder, redisClient *redis.Client, clock clockwork.Clock, solanaPriceProvider solanamodule.TokenPriceProvider, usernameResolver openrails.UsernameResolver, stripeClients *stripeapi.Factory) (*servicesInstances, error) {
	productService := catalog.NewProductService(database)
	priceService := catalog.NewPriceService(database)
	// NotificationService created with nil emailService - will be set later in buildRuntime
	notificationService := subscriptions.NewNotificationService(database, nil)
	paymentMethodService := paymentmethods.NewPaymentMethodService(database)
	purchaseService := payments.NewPaymentService(database, clock)
	entitlementService := entitlements.NewEntitlementService(database, clock)
	productAccessService := productaccess.NewService(database, clock)
	moneyService := money.NewMoneyService(database, clock)
	moneyService.EngineAdmissionHold = cfg.EngineAdmissionHold
	if cfg.HyperSwitch != nil {
		if err := moneyService.SetHyperSwitchDeployment(cfg.HyperSwitch.APIBaseURL); err != nil {
			return nil, err
		}
	}
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
	solanaPayService := solanamodule.NewSolanaPayService(database, cfg, railConfigs, priceService, productService, nil, fxProvider, solanaPriceProvider, clock)
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

	// Catalog Q&A and drafting are independently opt-in. Draft tools only
	// propose changes; the reprice API owns #781 notice-window enforcement.
	// The copilot shares the dashboard LLM client and uses its own rate-limit
	// namespace.
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

	railPMService := paymentmethods.NewRailPaymentMethodService(paymentMethodService, subscriptionService, database, cfg, clock)
	subscriptionService.RailPaymentMethodService = railPMService
	idempotencyService, err := idempotency.NewStore(database, leaseDB, checkout.IdempotencyTTL, checkout.IdempotencyLease)
	if err != nil {
		return nil, err
	}
	webhookClaims, err := idempotency.NewStore(database, leaseDB, webhooks.WebhookClaimTTL, webhooks.WebhookClaimLease)
	if err != nil {
		return nil, err
	}

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
		clock,
	)
	adminSubscriptionService.StripeService = &subscriptions.StripeService{StripeClients: stripeClients, Config: cfg, Rails: railConfigs}

	// #813: plan migrations — cross-product bulk retirement over the #773
	// reprice engine. Observed rails with a server-side push: Stripe, and
	// (#815) gateway-native NMI recurring via the per-merchant client
	// resolver.
	planMigrationService := subscriptions.NewPlanMigrationService(repriceService, &subscriptions.StripeService{StripeClients: stripeClients, Config: cfg, Rails: railConfigs}, subscriptions.NewNMIPlanPusher(collectionResolver))

	// #1099: deliveries are claimed in Postgres; webhook_events is the applied fact (#678).
	deduplicationService, err := webhooks.NewDeduplicationService(webhookClaims, database)
	if err != nil {
		return nil, err
	}
	webhookDispatcher := &webhooks.WebhookDispatcher{
		StripeClients:                stripeClients,
		Config:                       cfg,
		DB:                           database,
		Clock:                        clock,
		PriceService:                 priceService,
		ProductService:               productService,
		NotificationService:          notificationService,
		SubscriptionService:          subscriptionService,
		PaymentService:               purchaseService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		ProfileRepo:                  usernameResolver,
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
		railPMService,
		idempotencyService,
		railCustomerService,
		cfg,
		railConfigs,
		clock,
	)
	checkoutService.StripeClients = stripeClients
	checkoutService.StripeService.StripeClients = stripeClients
	checkoutService.SetSubscriptionLifecycleService(subscriptionLifecycleService)
	webhookDispatcher.PurchaseRegistrar = checkoutService
	// Wire durable product-access grants (issue #250) into the one-time purchase
	// flow. Additive to feature entitlements; nil-safe.
	if checkoutService.PurchaseService != nil {
		checkoutService.PurchaseService.SetProductAccessService(productAccessService)
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

	// The poller settles through the checkout session service, the one
	// crediting path for Solana Pay purchases.
	solanaPayPoller := solanamodule.NewSolanaPayPoller(database, checkoutSessionService, clock)

	return &servicesInstances{
		SubscriptionService:          subscriptionService,
		ProductService:               productService,
		PriceService:                 priceService,
		NotificationService:          notificationService,
		PaymentMethodService:         paymentMethodService,
		PurchaseService:              purchaseService,
		EntitlementService:           entitlementService,
		ProductAccessService:         productAccessService,
		RailPaymentMethodService:     railPMService,
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
		WebhookDispatcher:            webhookDispatcher,
		CheckoutService:              checkoutService,
		CheckoutSessionService:       checkoutSessionService,
		MoneyService:                 moneyService,
		MetricsService:               metricsService,
		DashboardService:             dashboardService,
		CopilotService:               copilotService,
		RailCustomerService:          railCustomerService,
	}, nil
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
