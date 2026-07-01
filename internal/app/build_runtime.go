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
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
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
	Rails config.ProviderAccountSet
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

// configureSolanaRail normalizes the Solana token set and applies the
// pricing policy (#360). It NEVER fails the boot over token configuration —
// tokens that cannot function are dropped with a loud warning, mirroring the
// "recipient_wallet not configured; Solana payments disabled" pattern:
//
//   - devnet (test_mode): NO pricing requirements at all — devnet money is fake.
//   - mainnet, Pyth feed exists for the symbol: feed pricing (for stablecoins
//     the feed is the depeg failsafe).
//   - mainnet, known USD-pegged stablecoin mint without a feed: kept, priced
//     at $1.00 parity, LOUD warning (no depeg protection).
//   - mainnet, non-USD-pegged stablecoin (e.g. EURC) or unknown token without
//     a feed: that TOKEN is disabled with a loud warning; everything else
//     keeps working.
//
// The error return is retained for signature stability; it is always nil.
func configureSolanaRail(cfg *config.Config, rails config.ProviderAccountSet) error {
	proc := rails.GetSolanaRail()
	if proc == nil {
		return nil
	}
	if proc.Solana == nil {
		proc.Solana = &config.SolanaRailConfig{}
	}
	proc.Solana.Network = effectiveSolanaNetwork(cfg)
	if len(proc.Solana.Tokens) == 0 {
		proc.Solana.Tokens = solanatokens.ForNetwork(proc.Solana.Network)
	}

	normalized := make(map[string]config.TokenConfig, len(proc.Solana.Tokens))
	for symbol, token := range proc.Solana.Tokens {
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
		if proc.Solana.Network != "devnet" {
			switch decision, sc := solanatokens.ClassifyPricing(normalizedSymbol, token.Mint); decision {
			case solanatokens.TokenPricingFeed:
				// Live Pyth pricing; for stablecoins the feed doubles as the
				// depeg failsafe.
			case solanatokens.TokenPricingUSDParity:
				log.Warnf("⚠️  solana token %s has no pyth price feed; degrading to $1.00 USD parity (known USD-pegged stablecoin, NO depeg protection)", normalizedSymbol)
			case solanatokens.TokenPricingDisabled:
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
	proc.Solana.Tokens = normalized
	return nil
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

func createPythPriceProvider(cfg *config.Config, rails config.ProviderAccountSet) (solanamodule.TokenPriceProvider, error) {
	if rails.GetSolanaRail() == nil {
		return nil, nil
	}
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
	var railSet config.ProviderAccountSet
	if overrides != nil {
		railSet = overrides.Rails
	}
	if err := config.ValidateRailSet(cfg, railSet); err != nil {
		return nil, err
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

	ccbillClient := createCCBillClient(cfg, railSet)
	ccbillRESTClient := createCCBillRESTClient(railSet)
	ccbillDataLinkClient := createCCBillDataLinkClient(cfg, railSet)
	nmiClients, err := createNMIClients(cfg, railSet, database)
	if err != nil {
		return nil, fmt.Errorf("failed to create nmi clients: %w", err)
	}

	if err := configureSolanaRail(cfg, railSet); err != nil {
		return nil, err
	}
	solanaPriceProvider, err := createPythPriceProvider(cfg, railSet)
	if err != nil {
		return nil, err
	}

	serviceInstances := createServices(database, cfg, railSet, ccbillRESTClient, nmiClients, redisClient, clock, solanaPriceProvider)
	healthManager := createHealthManager(database, redisClient)

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
		Rails:                railSet,
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
			adapters[string(models.RailStripe)] = money.NewStripeCollectionAdapter(database, &subscriptions.StripeService{Config: cfg, Rails: railSet})
			return adapters
		}()),
		RailCustomerService: serviceInstances.RailCustomerService,
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
	userDeferredDeletes := newIntentDeferredDeleteScheduler(database, intents.OriginUser,
		"user cancellation retained an undo window; rail delete deferred to its close")
	systemDeferredDeletes := newIntentDeferredDeleteScheduler(database, intents.OriginSystem,
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

	// Surface (and, outside development, enforce) the Row Level Security
	// posture of the connected role (issue #227): RLS policies only constrain a
	// non-superuser, non-BYPASSRLS role. Non-development boots fail if the app
	// would connect as a privileged role that silently bypasses every per-merchant
	// policy.
	if err := database.EnforceRLSPosture(context.Background(), cfg.RequiresRLS()); err != nil {
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

func createNMIClients(cfg *config.Config, rails config.ProviderAccountSet, database *db.DB) (map[string]*nmi.NMIClient, error) {
	clients := make(map[string]*nmi.NMIClient)

	nmiRails := rails.ByRail(models.RailNMI)
	if len(nmiRails) == 0 {
		return clients, nil
	}

	// #641: one client per configured NMI account, keyed by its operator-declared
	// account_id (gateway-id). Multiple NMI accounts per deployment are supported;
	// the primary is aliased under the rail key below for back-compat resolution.
	for _, procConfig := range nmiRails {
		accountID := procConfig.EffectiveAccountID()
		if accountID == "" {
			return nil, fmt.Errorf("nmi provider account is missing account_id (the rail-native gateway id)")
		}
		if _, exists := clients[accountID]; exists {
			return nil, fmt.Errorf("duplicate NMI provider account_id %q", accountID)
		}

		// The client's identity IS its account_id (#641). TestMode set by caller.
		settings := procConfig.ToNMIProviderSettings(accountID)
		if settings.SecurityKey == "" {
			return nil, fmt.Errorf("nmi provider account %q security key is required", accountID)
		}
		if settings.WebhookSecret == "" {
			log.Warnf("nmi provider account %q webhook secret is not configured; signature validation will be disabled", accountID)
		}

		client, err := nmi.NewClient(accountID, settings, cfg.IsTestMode())
		if err != nil {
			return nil, err
		}
		client.ReadOnly = cfg.IsProviderReadOnly()

		clients[accountID] = client
	}

	// #348: test_mode guarantees sandbox money, but NMI accounts are
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
	if cfg.IsTestMode() {
		for providerKey, client := range clients {
			if client.SecurityKey == "" {
				continue // unconfigured dev client, nothing to verify
			}
			keyHash := probeKeyHash(client.SecurityKey)
			if verdict, checkedAt, ok := lookupProbeVerdict(database, providerKey, keyHash); ok {
				switch probeCacheDecision(verdict, checkedAt, time.Now()) {
				case probeCacheRefuseBoot:
					return nil, fmt.Errorf("rail %q: PRODUCTION NMI credentials detected while test_mode is enabled — cached probe verdict 'live' from %s (within the %s cooldown; not re-probing); refusing to start (use the sandbox account credentials, rotate the key, or unset test_mode)", providerKey, checkedAt.UTC().Format(time.RFC3339), probeVerdictCooldown)
				case probeCacheSkipProbe:
					log.Infof("rail %q: NMI account verified as simulating (cached probe verdict from %s; #348 probe cooldown, no probe sent)", providerKey, checkedAt.UTC().Format(time.RFC3339))
					continue
				}
			}
			result, probeErr := client.ProbeTestMode()
			switch result {
			case nmi.ProbeLive:
				storeProbeVerdict(database, providerKey, keyHash, probeVerdictLive)
				return nil, fmt.Errorf("rail %q: PRODUCTION NMI credentials detected while test_mode is enabled — the account did not simulate the test-card probe, so real charges could occur; refusing to start (use the sandbox account credentials, or unset test_mode)", providerKey)
			case nmi.ProbeSimulated:
				storeProbeVerdict(database, providerKey, keyHash, probeVerdictSimulated)
				log.Infof("rail %q: NMI account verified as simulating (test env)", providerKey)
			default:
				// Indeterminate verdicts are never cached: the next boot
				// re-probes once the transport/credential issue clears.
				log.WithError(probeErr).Warnf("⚠️  rail %q: could not verify the NMI account is a sandbox account; proceeding, but confirm the credentials before relying on test_mode", providerKey)
			}
		}
	}

	// Also expose the active account under the rail key "nmi" so existing
	// NMIClients["nmi"] consumers keep resolving it. Provider-account-specific
	// callers should use observed provider identity instead.
	railKey := string(models.RailNMI)
	_, active, err := rails.ActiveRailByType(models.RailNMI)
	if err != nil {
		return nil, err
	}
	if active != nil {
		clients[railKey] = clients[active.EffectiveAccountID()]
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

func createCCBillClient(cfg *config.Config, rails config.ProviderAccountSet) *ccbill.CCBillClient {
	ccbillProc := rails.GetCCBillRail()
	if ccbillProc == nil {
		log.Info("CCBill config missing; CCBill integration disabled")
		return nil
	}

	return ccbill.NewClient(ccbillProc.ToCCBillConfig(), cfg.IsTestMode())
}

func createCCBillRESTClient(rails config.ProviderAccountSet) *ccbill.RESTClient {
	ccbillProc := rails.GetCCBillRail()
	if ccbillProc == nil {
		return nil
	}
	return ccbill.NewRESTClient(ccbillProc.ToCCBillConfig())
}

func createCCBillDataLinkClient(cfg *config.Config, rails config.ProviderAccountSet) *ccbill.DataLinkClient {
	ccbillProc := rails.GetCCBillRail()
	if ccbillProc == nil {
		return nil
	}
	if ccbillProc.CCBill == nil || ccbillProc.CCBill.DataLinkUsername == "" || ccbillProc.CCBill.DataLinkPassword == "" || ccbillProc.CCBill.ClientAccNum == "" {
		log.Info("CCBill DataLink credentials missing; DataLink worker disabled")
		return nil
	}

	ccbillConfig := ccbillProc.ToCCBillConfig()
	ccbillConfig.TestMode = cfg.IsTestMode()
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

	CheckoutService        *checkout.CheckoutService
	CheckoutSessionService *checkout.CheckoutSessionService
	MoneyService           *money.MoneyService
	RailCustomerService    *payments.RailCustomerService
}

func createServices(database *db.DB, cfg *config.Config, railSet config.ProviderAccountSet, ccbillRESTClient *ccbill.RESTClient, nmiClients map[string]*nmi.NMIClient, redisClient *redis.Client, clock clockwork.Clock, solanaPriceProvider solanamodule.TokenPriceProvider) *servicesInstances {
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
	railCustomerService := payments.NewRailCustomerService(database)
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
	solanaPayService := solanamodule.NewSolanaPayService(database, redisClient, cfg, railSet, priceService, productService, nil, fxProvider, solanaPriceProvider, clock)
	var solanaRPC *solana.RPCClient
	if solanaProc := railSet.GetSolanaRail(); solanaProc != nil && solanaProc.Solana != nil {
		solanaNetwork := effectiveSolanaNetwork(cfg)
		solanaRPC = solana.NewRPCClientWithConfig(solana.RPCClientConfig{
			HeliusAPIKey: solanaProc.Solana.HeliusAPIKey,
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
	subscriptionLifecycleService.SetConfig(cfg)

	subscriptionService := subscriptions.NewSubscriptionService(
		database,
		priceService,
		productService,
		ccbillRESTClient,
		nmiClients,
		paymentMethodService,
		clock,
	)

	vaultService := vault.NewVaultService(paymentMethodService, subscriptionService, nmiClients, database, cfg, railSet, clock)
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
	adminSubscriptionService.StripeService = &subscriptions.StripeService{Config: cfg, Rails: railSet}

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
		RailCustomerService:          railCustomerService,
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
		railCustomerService,
		cfg,
		railSet,
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
		railSet,
		clock,
	)
	webhookDispatcher.CheckoutSessionService = checkoutSessionService
	solanaPayService.SetEligibilityChecker(&solanaEligibilityAdapter{service: checkoutService})

	// Create SolanaPayPoller (depends on checkoutService for RegisterPurchase)
	solanaPayPoller := solanamodule.NewSolanaPayPoller(
		database,
		redisClient,
		cfg,
		railSet,
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
		RailCustomerService:          railCustomerService,
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
