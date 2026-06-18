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
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/pyth"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services/health"
	clickhousemigrations "github.com/open-rails/openrails/migrations/clickhouse"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

const (
	standaloneRiverDefaultQueueMaxWorkers = 10
	standaloneRiverBillingQueueMaxWorkers = 20
)

// standaloneRiverSchema returns the schema for River tables when OpenRails
// constructs its own River client (standalone mode). Per issue #165 the standalone
// River schema is the same as the OpenRails Postgres schema (db.schema, default
// `billing`) — it is NOT separately configurable. In embedded/library mode the
// host instead injects its own River client via embedded.SetRiverClient, and that
// client owns its schema; OpenRails never overrides it.
func standaloneRiverSchema(cfg *config.Config) string {
	if cfg == nil || cfg.DB == nil {
		return config.DefaultSchema
	}
	return cfg.DB.SchemaName()
}

type runtimeOverrides struct {
	DB    *db.DB
	Redis *redis.Client
	Clock clockwork.Clock
}

// effectiveSolanaNetwork derives the Solana network purely from the test_env
// axis — devnet under test_env, mainnet otherwise. There is deliberately no
// override knob (#349): test_env already answers the question.
func effectiveSolanaNetwork(cfg *config.Config) string {
	if cfg != nil && cfg.IsTestEnv() {
		return "devnet"
	}
	return "mainnet"
}

// configureSolanaProcessor normalizes the Solana token set and applies the
// pricing policy (#360). It NEVER fails the boot over token configuration —
// tokens that cannot function are dropped with a loud warning, mirroring the
// "recipient_wallet not configured; Solana payments disabled" pattern:
//
//   - devnet (test_env): NO pricing requirements at all — devnet money is fake.
//   - mainnet, Pyth feed exists for the symbol: feed pricing (for stablecoins
//     the feed is the depeg failsafe).
//   - mainnet, known USD-pegged stablecoin mint without a feed: kept, priced
//     at $1.00 parity, LOUD warning (no depeg protection).
//   - mainnet, non-USD-pegged stablecoin (e.g. EURC) or unknown token without
//     a feed: that TOKEN is disabled with a loud warning; everything else
//     keeps working.
//
// The error return is retained for signature stability; it is always nil.
func configureSolanaProcessor(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	proc := cfg.GetSolanaProcessor()
	if proc == nil {
		return nil
	}
	proc.Network = effectiveSolanaNetwork(cfg)
	if len(proc.Tokens) == 0 {
		proc.Tokens = config.TokensForNetwork(proc.Network)
	}

	normalized := make(map[string]config.TokenConfig, len(proc.Tokens))
	for symbol, token := range proc.Tokens {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if normalizedSymbol == "" {
			log.Warn("⚠️  solana token with empty symbol in configuration; entry dropped")
			continue
		}
		if strings.TrimSpace(token.Mint) == "" {
			log.Warnf("⚠️  solana token %s has no mint configured; payments in %s unavailable", normalizedSymbol, normalizedSymbol)
			continue
		}
		if token.Decimals < 0 {
			log.Warnf("⚠️  solana token %s has invalid decimals (%d); payments in %s unavailable", normalizedSymbol, token.Decimals, normalizedSymbol)
			continue
		}
		if strings.TrimSpace(token.Name) == "" {
			token.Name = normalizedSymbol
		}

		// Pricing policy applies to MAINNET only: devnet money is fake, so a
		// devnet deployment never needs price feeds (or Hermes) at all.
		if proc.Network != "devnet" {
			switch decision, sc := config.ClassifySolanaTokenPricing(normalizedSymbol, token.Mint); decision {
			case config.TokenPricingFeed:
				// Live Pyth pricing; for stablecoins the feed doubles as the
				// depeg failsafe.
			case config.TokenPricingUSDParity:
				log.Warnf("⚠️  solana token %s has no pyth price feed; degrading to $1.00 USD parity (known USD-pegged stablecoin, NO depeg protection)", normalizedSymbol)
			case config.TokenPricingDisabled:
				if sc.Symbol != "" {
					log.Warnf("⚠️  solana token %s is pegged to %s and has no price feed; payments in %s unavailable (cannot default a non-USD peg to USD parity)", normalizedSymbol, strings.ToUpper(sc.Peg), normalizedSymbol)
				} else {
					log.Warnf("⚠️  solana token %s has no pyth price feed and is not a known stablecoin; payments in %s unavailable", normalizedSymbol, normalizedSymbol)
				}
				continue
			}
		}
		normalized[normalizedSymbol] = token
	}
	proc.Tokens = normalized
	return nil
}

// devnetParityPriceProvider wraps the Pyth client under test_env (#360).
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
	if cfg == nil || cfg.GetSolanaProcessor() == nil {
		return nil, nil
	}
	// Pyth is not configurable (#352): Hermes URL, freshness bounds and the
	// price-feed map are protocol constants.
	hermesURL := config.DefaultPythHermesURL
	maxPriceAgeText := config.DefaultPythMaxPriceAge
	maxConfidenceBPS := config.DefaultPythMaxConfidenceBPS
	priceFeeds := config.DefaultPythPriceFeeds()
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
	// Initialize NMI-backed processors from config BEFORE creating clients
	// This ensures IsNMIBacked() works correctly for all configured processors
	processors.InitNMIBackedProcessors(cfg)

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

	if overrides != nil && overrides.Redis != nil {
		redisClient = overrides.Redis
	} else {
		redisClient, err = createRedisClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis client: %w", err)
		}
	}

	ccbillClient := createCCBillClient(cfg)
	ccbillRESTClient := createCCBillRESTClient(cfg)
	ccbillDataLinkClient := createCCBillDataLinkClient(cfg)
	nmiClients, err := createNMIClients(cfg, database)
	if err != nil {
		return nil, fmt.Errorf("failed to create nmi clients: %w", err)
	}

	if err := configureSolanaProcessor(cfg); err != nil {
		return nil, err
	}
	solanaPriceProvider, err := createPythPriceProvider(cfg)
	if err != nil {
		return nil, err
	}

	serviceInstances := createServices(database, cfg, ccbillRESTClient, nmiClients, redisClient, clock, solanaPriceProvider)
	healthManager := createHealthManager(database, redisClient)

	var emailService *subscriptions.EmailService
	if cfg.SendGrid != nil {
		if es, err := subscriptions.NewEmailService(cfg.SendGrid, cfg.Store, clock); err != nil {
			log.WithError(err).Warn("EmailService init failed; email disabled")
		} else {
			emailService = es
			// Configure domain services for subscription emails
			emailService.SetDomainServices(
				serviceInstances.SubscriptionService,
				serviceInstances.ProductService,
				serviceInstances.PriceService,
				identity.NewProfilesDirectory(repo.NewProfileRepo(database)),
			)
		}
	}

	// Set emailService on the NotificationService that was created in createServices
	serviceInstances.NotificationService.SetEmailService(emailService)

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

	runtime := &Runtime{
		DB:                   database,
		RedisClient:          redisClient,
		Config:               cfg,
		Clock:                clock,
		AdmissionPolicyCache: admission.NewPolicyCache(0), // #513: default long TTL (config)
		HealthManager:        healthManager,
		CCBillClient:         ccbillClient,
		CCBillRESTClient:     ccbillRESTClient,
		CCBillDataLink:       ccbillDataLinkClient,
		NMIClients:           nmiClients,

		SubscriptionService:      serviceInstances.SubscriptionService,
		ProductService:           serviceInstances.ProductService,
		PriceService:             serviceInstances.PriceService,
		NotificationService:      serviceInstances.NotificationService,
		PaymentMethodService:     serviceInstances.PaymentMethodService,
		PaymentService:           serviceInstances.PurchaseService,
		EntitlementService:       serviceInstances.EntitlementService,
		FeatureService:           serviceInstances.FeatureService,
		ProductAccessService:     serviceInstances.ProductAccessService,
		VaultService:             serviceInstances.VaultService,
		SolanaPayService:         serviceInstances.SolanaPayService,
		SolanaPayPoller:          serviceInstances.SolanaPayPoller,
		SolanaTransactionService: serviceInstances.SolanaTransactionService,
		SolanaRPC:                serviceInstances.SolanaRPC,
		SolanaPriceProvider:      solanaPriceProvider,
		FXProvider:               serviceInstances.FXProvider,
		FXRateRefresher:          serviceInstances.FXRateRefresher,

		UserSubscriptionService:   serviceInstances.UserSubscriptionService,
		PublicSubscriptionService: serviceInstances.PublicSubscriptionService,
		AdminSubscriptionService:  serviceInstances.AdminSubscriptionService,

		EmailService:                 emailService,
		SubscriptionLifecycleService: serviceInstances.SubscriptionLifecycleService,
		WebhookDispatcher:            serviceInstances.WebhookDispatcher,
		DeduplicationService:         serviceInstances.DeduplicationService,
		IdempotencyService:           serviceInstances.IdempotencyService,

		CheckoutService:        serviceInstances.CheckoutService,
		CheckoutSessionService: serviceInstances.CheckoutSessionService,
		CardAbuseGuard:         cardAbuseGuard,
		MoneyService:           serviceInstances.MoneyService,
		MoneyCharger: money.NewScopedCharger(database, func() map[string]money.CollectionAdapter {
			adapters := money.NewNMICollectionAdapters(nmiClients)
			adapters[string(models.ProcessorStripe)] = money.NewStripeCollectionAdapter(database, &subscriptions.StripeService{Config: cfg})
			return adapters
		}()),
		ProcessorCustomerService: serviceInstances.ProcessorCustomerService,
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
	userDeferredDeletes := newIntentDeferredDeleteScheduler(database, runtime.ProviderAccounts(), intents.OriginUser,
		"user cancellation retained an undo window; processor delete deferred to its close")
	systemDeferredDeletes := newIntentDeferredDeleteScheduler(database, runtime.ProviderAccounts(), intents.OriginSystem,
		"terminal dunning failure; remote NMI subscription must stop rebilling")
	runtime.DeferredDeletes = systemDeferredDeletes
	if runtime.UserSubscriptionService != nil {
		runtime.UserSubscriptionService.SetDeferredDeleteScheduler(userDeferredDeletes)
	}
	if runtime.SubscriptionLifecycleService != nil {
		runtime.SubscriptionLifecycleService.SetDeferredDeleteScheduler(systemDeferredDeletes)
	}

	if cfg.ClickHouse != nil {
		if bes, err := analytics.NewEventLogService(cfg.ClickHouse, clock); err != nil {
			log.WithError(err).Warn("EventLogService init failed; analytics disabled")
		} else {
			runtime.EventLogService = bes
		}
	}

	runtime.WebhookDispatcher.EventLogService = runtime.EventLogService
	runtime.SubscriptionLifecycleService.EventLogService = runtime.EventLogService

	if runtime.AdminSubscriptionService != nil {
		runtime.AdminSubscriptionService.EventLogService = runtime.EventLogService
	}

	if runtime.HealthManager != nil {
		runtime.HealthManager.Start()
	}

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

	// Surface (and, in managed multi-tenant mode, enforce) the Row Level Security
	// posture of the connected role (issue #227): RLS policies only constrain a
	// non-superuser, non-BYPASSRLS role. With DB.RequireRLS set this FAILS startup
	// if the app would connect as a privileged role that silently bypasses every
	// per-tenant policy.
	if err := database.EnforceRLSPosture(context.Background(), cfg.DB != nil && cfg.DB.RequireRLS); err != nil {
		return nil, err
	}
	return database, nil
}

func createHealthManager(database *db.DB, redisClient *redis.Client) *health.ServiceHealthManager {
	manager := health.NewServiceHealthManager()
	if database != nil {
		if pool := database.Pool(); pool != nil {
			manager.RegisterChecker(health.NewPostgresHealthChecker(pool))
		} else {
			log.Warn("database health checker not registered: runtime DB has no pgx pool")
		}
	}
	if redisClient != nil {
		manager.RegisterChecker(health.NewRedisHealthChecker(redisClient))
	}
	return manager
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

	if err := migratekit.ValidatePostgresMigrations(context.Background(), sqlDB,
		migratekit.MigrationSource{App: config.MigratekitApp, FS: postgresmigrations.FS},
	); err != nil {
		log.WithError(err).Fatal("Postgres migrations validation failed")
		return err
	}

	// Validate ClickHouse migrations if ClickHouse is configured
	// ClickHouse is optional - warn if validation fails but continue running
	if cfg.ClickHouse != nil {
		log.Infof("Validating ClickHouse migrations for database %s at %s", cfg.ClickHouse.Database, cfg.ClickHouse.ClientAddr)
		if err := migratekit.ValidateClickHouseMigrations(
			context.Background(),
			&migratekit.ClickHouseConfig{
				ClientAddr: cfg.ClickHouse.ClientAddr,
				Database:   cfg.ClickHouse.Database,
				Username:   cfg.ClickHouse.Username,
				Password:   cfg.ClickHouse.Password,
				App:        config.MigratekitApp,
				Cluster:    cfg.ClickHouse.Cluster,
				PostgresDB: sqlDB,
			},
			clickhousemigrations.FS,
		); err != nil {
			log.WithError(err).Warn("ClickHouse migrations validation failed - analytics disabled")
		}
	}

	return nil
}

func createNMIClients(cfg *config.Config, database *db.DB) (map[string]*nmi.NMIClient, error) {
	clients := make(map[string]*nmi.NMIClient)

	nmiProcessors := cfg.GetNMIProcessors()
	if len(nmiProcessors) == 0 {
		return clients, nil
	}

	for name, procConfig := range nmiProcessors {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" {
			return nil, fmt.Errorf("nmi provider name cannot be empty")
		}

		if _, exists := clients[providerKey]; exists {
			return nil, fmt.Errorf("duplicate nmi provider '%s' detected in configuration", providerKey)
		}

		// Convert ProcessorConfig to NMIProviderSettings
		settings := procConfig.ToNMIProviderSettings(providerKey)

		// Validate required fields
		if settings.SecurityKey == "" {
			return nil, fmt.Errorf("nmi provider '%s' security key is required", providerKey)
		}
		if settings.WebhookSecret == "" {
			log.Warnf("nmi provider '%s' webhook secret is not configured; signature validation will be disabled", providerKey)
		}

		client, err := nmi.NewClient(providerKey, settings, cfg.IsTestEnv())
		if err != nil {
			return nil, err
		}
		client.SubscriptionDeletesDisabled = cfg.IsProcessorSubscriptionDeletionDisabled()
		client.ReadOnly = cfg.IsProviderReadOnly()

		clients[providerKey] = client
	}

	// #348: test_env guarantees sandbox money, but NMI accounts are
	// undetectable by configuration (sandbox hits the same gateway URL, the key
	// carries no marker). Probe instead: only a simulating account can approve
	// the non-issued test card, so a decline proves PRODUCTION credentials —
	// refuse to start. Probe errors (offline dev, bad credentials) are
	// inconclusive and only warn.
	//
	// Probe-cooldown cache (#348 tail): conclusive verdicts persist in
	// openrails.probe_verdicts keyed by sha256(security key). A fresh 'live'
	// verdict refuses the boot from cache (a crash-looping supervisor stops
	// paying one declined auth per restart); a fresh 'simulated' verdict skips
	// the probe. A rotated key or a stale verdict always re-probes, and cache
	// failures degrade to probing.
	if cfg.IsTestEnv() {
		for providerKey, client := range clients {
			if client.SecurityKey == "" {
				continue // unconfigured dev client, nothing to verify
			}
			keyHash := probeKeyHash(client.SecurityKey)
			if verdict, checkedAt, ok := lookupProbeVerdict(database, providerKey, keyHash); ok {
				switch probeCacheDecision(verdict, checkedAt, time.Now()) {
				case probeCacheRefuseBoot:
					return nil, fmt.Errorf("processor %q: PRODUCTION NMI credentials detected while test_env is enabled — cached probe verdict 'live' from %s (within the %s cooldown; not re-probing); refusing to start (use the sandbox account credentials, rotate the key, or unset test_env)", providerKey, checkedAt.UTC().Format(time.RFC3339), probeVerdictCooldown)
				case probeCacheSkipProbe:
					log.Infof("processor %q: NMI account verified as simulating (cached probe verdict from %s; #348 probe cooldown, no probe sent)", providerKey, checkedAt.UTC().Format(time.RFC3339))
					continue
				}
			}
			result, probeErr := client.ProbeTestMode()
			switch result {
			case nmi.ProbeLive:
				storeProbeVerdict(database, providerKey, keyHash, probeVerdictLive)
				return nil, fmt.Errorf("processor %q: PRODUCTION NMI credentials detected while test_env is enabled — the account did not simulate the test-card probe, so real charges could occur; refusing to start (use the sandbox account credentials, or unset test_env)", providerKey)
			case nmi.ProbeSimulated:
				storeProbeVerdict(database, providerKey, keyHash, probeVerdictSimulated)
				log.Infof("processor %q: NMI account verified as simulating (test env)", providerKey)
			default:
				// Indeterminate verdicts are never cached: the next boot
				// re-probes once the transport/credential issue clears.
				log.WithError(probeErr).Warnf("⚠️  processor %q: could not verify the NMI account is a sandbox account; proceeding, but confirm the credentials before relying on test_env", providerKey)
			}
		}
	}

	return clients, nil
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

func createCCBillClient(cfg *config.Config) *ccbill.CCBillClient {
	ccbillProc := cfg.GetCCBillProcessor()
	if ccbillProc == nil {
		log.Info("CCBill config missing; CCBill integration disabled")
		return nil
	}

	return ccbill.NewClient(ccbillProc.ToCCBillConfig(), cfg.IsTestEnv())
}

func createCCBillRESTClient(cfg *config.Config) *ccbill.RESTClient {
	ccbillProc := cfg.GetCCBillProcessor()
	if ccbillProc == nil {
		return nil
	}
	return ccbill.NewRESTClient(ccbillProc.ToCCBillConfig())
}

func createCCBillDataLinkClient(cfg *config.Config) *ccbill.DataLinkClient {
	ccbillProc := cfg.GetCCBillProcessor()
	if ccbillProc == nil {
		return nil
	}
	if ccbillProc.DataLinkUsername == "" || ccbillProc.DataLinkPassword == "" || ccbillProc.ClientAccNum == "" {
		log.Info("CCBill DataLink credentials missing; DataLink worker disabled")
		return nil
	}

	ccbillConfig := ccbillProc.ToCCBillConfig()
	ccbillConfig.TestMode = cfg.IsTestEnv()
	client := ccbill.NewDataLinkClient(ccbillConfig)
	if err := client.ValidateConfig(); err != nil {
		log.WithError(err).Warn("Invalid CCBill DataLink configuration; worker disabled")
		return nil
	}
	return client
}

type servicesInstances struct {
	SubscriptionService *subscriptions.SubscriptionService

	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *vault.PaymentMethodService
	PurchaseService          *payments.PaymentService
	EntitlementService       *entitlements.EntitlementService
	FeatureService           *entitlements.FeatureService
	ProductAccessService     *productaccess.Service
	VaultService             *vault.VaultService
	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaRPC                *solana.RPCClient
	SolanaPriceProvider      solanamodule.TokenPriceProvider
	FXProvider               fx.Provider
	FXRateRefresher          interface {
		Stop()
		LastRefresh() time.Time
	}

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	DeduplicationService         *webhooks.DeduplicationService
	IdempotencyService           *idempotency.IdempotencyService
	WebhookDispatcher            *webhooks.WebhookDispatcher

	CheckoutService          *checkout.CheckoutService
	CheckoutSessionService   *checkout.CheckoutSessionService
	MoneyService             *money.MoneyService
	ProcessorCustomerService *payments.ProcessorCustomerService
}

func createServices(database *db.DB, cfg *config.Config, ccbillRESTClient *ccbill.RESTClient, nmiClients map[string]*nmi.NMIClient, redisClient *redis.Client, clock clockwork.Clock, solanaPriceProvider solanamodule.TokenPriceProvider) *servicesInstances {
	productService := catalog.NewProductService(database)
	priceService := catalog.NewPriceService(database)
	// NotificationService created with nil emailService - will be set later in buildRuntime
	notificationService := subscriptions.NewNotificationService(database, nil)
	paymentMethodService := vault.NewPaymentMethodService(database)
	purchaseService := payments.NewPaymentService(database, clock)
	entitlementService := entitlements.NewEntitlementService(database, clock)
	featureService := entitlements.NewFeatureService(database, clock)
	productAccessService := productaccess.NewService(database, clock)
	moneyService := money.NewMoneyService(database, clock)
	processorCustomerService := payments.NewProcessorCustomerService(database)
	profileRepo := repo.NewProfileRepo(database)

	// Create FX provider for Solana token quoting and policy-currency admission.
	// Runtime enforcement reads fresh cross-currency rates from Redis; same-currency
	// paths do not require FX.
	//
	// THIS IS THE DEFAULT FX PROVIDER for the whole app — LIVE rates, always on.
	// ExchangeAPIProvider uses the fawazahmed0 exchange-api (CC0, free, NO API key),
	// wrapped in a 5-minute in-memory cache, or (when Redis is present) a 3-hour
	// Redis cache with a background refresher. There is no config switch and no
	// NoOp fallback here: production never runs at a flat 1.0 rate. (fx.NoOpProvider
	// is test/standalone-only — see internal/integrations/fx/provider.go.)
	liveFX := fx.NewExchangeAPIProvider()
	var fxProvider fx.Provider = fx.NewCachedProvider(liveFX, 5*time.Minute)
	var fxRateRefresher interface {
		Stop()
		LastRefresh() time.Time
	}
	if redisClient != nil {
		redisFX := fx.NewRedisCachedProvider(redisClient, liveFX, 3*time.Hour)
		redisFX.Start(context.Background(), money.CurrencyCodes(), 2*time.Hour)
		fxProvider = redisFX
		fxRateRefresher = redisFX
	}

	// Note: solanaPayService and SolanaPayPoller need checkoutService, which is created later
	// We'll create solanaPayService with nil checkoutService and set it after checkoutService is created
	solanaPayService := solanamodule.NewSolanaPayService(database, redisClient, cfg, priceService, productService, nil, fxProvider, solanaPriceProvider, clock)
	var solanaRPC *solana.RPCClient
	if solanaProc := cfg.GetSolanaProcessor(); solanaProc != nil {
		solanaNetwork := effectiveSolanaNetwork(cfg)
		solanaRPC = solana.NewRPCClientWithConfig(solana.RPCClientConfig{
			HeliusAPIKey: solanaProc.HeliusAPIKey,
			Network:      solanaNetwork,
			ReadOnly:     cfg.IsProviderReadOnly(),
		})
	}
	solanaTransactionService := solanamodule.NewSolanaTransactionService(database, solanaRPC, cfg, priceService, fxProvider, clock)

	subscriptionLifecycleService := subscriptions.NewSubscriptionLifecycleService(
		database,
		productService,
		priceService,
		entitlementService,
		notificationService,
		purchaseService, // For creating Payment records on renewal
		nil,             // EventLogService - set later in buildRuntime after ClickHouse init
		clock,
	)
	subscriptionLifecycleService.SetConfig(cfg) // For feature flag access (dunning_mode, etc.)

	subscriptionService := subscriptions.NewSubscriptionService(
		database,
		priceService,
		productService,
		ccbillRESTClient,
		nmiClients,
		paymentMethodService,
		clock,
	)

	vaultService := vault.NewVaultService(paymentMethodService, subscriptionService, nmiClients, database, cfg, clock)
	subscriptionService.VaultService = vaultService
	idempotencyService := idempotency.NewIdempotencyService(redisClient)
	webhookIdempotencyService := idempotency.NewIdempotencyServiceWithTTL(redisClient, webhooks.WebhookIdempotencyTTL)

	userSubscriptionService := subscriptions.NewUserSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		purchaseService,
		notificationService,
		entitlementService,
		nmiClients,
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
		nmiClients,
		clock,
	)
	adminSubscriptionService.StripeService = &subscriptions.StripeService{Config: cfg}

	deduplicationService := webhooks.NewDeduplicationService(webhookIdempotencyService)
	webhookDispatcher := &webhooks.WebhookDispatcher{
		Config:                       cfg,
		DB:                           database,
		Clock:                        clock,
		PriceService:                 priceService,
		ProductService:               productService,
		NotificationService:          notificationService,
		SubscriptionService:          subscriptionService,
		PaymentService:               purchaseService,
		EventLogService:              nil,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		ProfileRepo:                  profileRepo,
		DeduplicationService:         deduplicationService,
		ProcessorCustomerService:     processorCustomerService,
		CCBillRESTClient:             ccbillRESTClient,
		NMIClients:                   nmiClients,
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
		idempotency.NewPaymentsIdempotencyAdapter(idempotencyService),
		nmiClients,
		processorCustomerService,
		cfg,
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
	subscriptionLifecycleService.EventLogService = nil // reset until ClickHouse init

	checkoutSessionService := checkout.NewCheckoutSessionService(
		database,
		priceService,
		productService,
		paymentMethodService,
		idempotency.NewPaymentsIdempotencyAdapter(idempotencyService),
		checkoutService,
		solanaPayService,
		solanaTransactionService,
		fxProvider,
		solanaPriceProvider,
		cfg,
		clock,
	)
	checkoutSessionService.SetProviderAccounts(intents.NewRuntimeProviderAccounts(cfg, nmiClients))
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
		FeatureService:               featureService,
		ProductAccessService:         productAccessService,
		VaultService:                 vaultService,
		SolanaPayService:             solanaPayService,
		SolanaPayPoller:              solanaPayPoller,
		SolanaTransactionService:     solanaTransactionService,
		SolanaRPC:                    solanaRPC,
		SolanaPriceProvider:          solanaPriceProvider,
		FXProvider:                   fxProvider,
		FXRateRefresher:              fxRateRefresher,
		UserSubscriptionService:      userSubscriptionService,
		PublicSubscriptionService:    publicSubscriptionService,
		AdminSubscriptionService:     adminSubscriptionService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		DeduplicationService:         deduplicationService,
		IdempotencyService:           idempotencyService,
		WebhookDispatcher:            webhookDispatcher,
		CheckoutService:              checkoutService,
		CheckoutSessionService:       checkoutSessionService,
		MoneyService:                 moneyService,
		ProcessorCustomerService:     processorCustomerService,
	}
}

func buildRiverClient(cfg *config.Config, workers *river.Workers) (*river.Client[pgx.Tx], *pgxpool.Pool, error) {
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
			river.QueueDefault:     {MaxWorkers: standaloneRiverDefaultQueueMaxWorkers},
			riverjobs.QueueBilling: {MaxWorkers: standaloneRiverBillingQueueMaxWorkers},
		},
		Schema:  standaloneRiverSchema(cfg),
		Workers: workers,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River client: %w", err)
	}
	return client, pool, nil
}
