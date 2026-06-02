package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/pyth"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	riverjobs "github.com/open-rails/openrails/internal/river"
	clickhousemigrations "github.com/open-rails/openrails/migrations/clickhouse"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

const (
	standaloneRiverDefaultQueueMaxWorkers = 10
	standaloneRiverBillingQueueMaxWorkers = 20
	riverSchema                           = "billing"
)

type runtimeOverrides struct {
	DB    *db.DB
	Redis *redis.Client
	Clock clockwork.Clock
}

func effectiveSolanaNetwork(cfg *config.Config, proc *config.ProcessorConfig) string {
	if proc != nil {
		if network := strings.ToLower(strings.TrimSpace(proc.Network)); network != "" {
			return network
		}
	}
	if cfg != nil && cfg.IsTestMode() {
		return "devnet"
	}
	return "mainnet"
}

func configureSolanaProcessor(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	proc := cfg.GetSolanaProcessor()
	if proc == nil {
		return nil
	}
	proc.Network = effectiveSolanaNetwork(cfg, proc)
	if len(proc.Tokens) == 0 {
		proc.Tokens = config.TokensForNetwork(proc.Network)
	}

	priceFeeds := config.DefaultPythPriceFeeds()
	if cfg.Pyth != nil {
		for symbol, feedID := range cfg.Pyth.PriceFeeds {
			priceFeeds[strings.ToUpper(strings.TrimSpace(symbol))] = strings.TrimSpace(feedID)
		}
	}

	normalized := make(map[string]config.TokenConfig, len(proc.Tokens))
	for symbol, token := range proc.Tokens {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if normalizedSymbol == "" {
			return fmt.Errorf("solana token symbol cannot be empty")
		}
		if strings.TrimSpace(token.Mint) == "" {
			return fmt.Errorf("solana token %s missing mint", normalizedSymbol)
		}
		if token.Decimals < 0 {
			return fmt.Errorf("solana token %s has invalid decimals", normalizedSymbol)
		}
		if strings.TrimSpace(token.Name) == "" {
			token.Name = normalizedSymbol
		}
		if strings.TrimSpace(priceFeeds[normalizedSymbol]) == "" {
			return fmt.Errorf("solana token %s missing pyth price feed", normalizedSymbol)
		}
		normalized[normalizedSymbol] = token
	}
	proc.Tokens = normalized
	return nil
}

func createPythPriceProvider(cfg *config.Config) (solanamodule.TokenPriceProvider, error) {
	if cfg == nil || cfg.GetSolanaProcessor() == nil {
		return nil, nil
	}
	hermesURL := config.DefaultPythHermesURL
	maxPriceAgeText := config.DefaultPythMaxPriceAge
	maxConfidenceBPS := config.DefaultPythMaxConfidenceBPS
	priceFeeds := config.DefaultPythPriceFeeds()
	if cfg.Pyth != nil {
		if strings.TrimSpace(cfg.Pyth.HermesURL) != "" {
			hermesURL = cfg.Pyth.HermesURL
		}
		if strings.TrimSpace(cfg.Pyth.MaxPriceAge) != "" {
			maxPriceAgeText = cfg.Pyth.MaxPriceAge
		}
		if cfg.Pyth.MaxConfidenceBPS > 0 {
			maxConfidenceBPS = cfg.Pyth.MaxConfidenceBPS
		}
		for symbol, feedID := range cfg.Pyth.PriceFeeds {
			priceFeeds[strings.ToUpper(strings.TrimSpace(symbol))] = strings.TrimSpace(feedID)
		}
	}
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
	nmiClients, err := createNMIClients(cfg)
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
				repo.NewProfileRepo(database),
			)
		}
	}

	// Set emailService on the NotificationService that was created in createServices
	serviceInstances.NotificationService.SetEmailService(emailService)

	runtime := &Runtime{
		DB:               database,
		RedisClient:      redisClient,
		Config:           cfg,
		Clock:            clock,
		CCBillClient:     ccbillClient,
		CCBillRESTClient: ccbillRESTClient,
		CCBillDataLink:   ccbillDataLinkClient,
		NMIClients:       nmiClients,

		SubscriptionService:      serviceInstances.SubscriptionService,
		ProductService:           serviceInstances.ProductService,
		PriceService:             serviceInstances.PriceService,
		NotificationService:      serviceInstances.NotificationService,
		PaymentMethodService:     serviceInstances.PaymentMethodService,
		PaymentService:           serviceInstances.PurchaseService,
		EntitlementService:       serviceInstances.EntitlementService,
		VaultService:             serviceInstances.VaultService,
		SolanaPayService:         serviceInstances.SolanaPayService,
		SolanaPayPoller:          serviceInstances.SolanaPayPoller,
		SolanaTransactionService: serviceInstances.SolanaTransactionService,
		SolanaRPC:                serviceInstances.SolanaRPC,
		SolanaPriceProvider:      solanaPriceProvider,
		FXProvider:               serviceInstances.FXProvider,

		UserSubscriptionService:   serviceInstances.UserSubscriptionService,
		PublicSubscriptionService: serviceInstances.PublicSubscriptionService,
		AdminSubscriptionService:  serviceInstances.AdminSubscriptionService,

		EmailService:                 emailService,
		SubscriptionLifecycleService: serviceInstances.SubscriptionLifecycleService,
		WebhookDispatcher:            serviceInstances.WebhookDispatcher,
		DeduplicationService:         serviceInstances.DeduplicationService,
		IdempotencyService:           serviceInstances.IdempotencyService,

		CheckoutService:          serviceInstances.CheckoutService,
		CheckoutSessionService:   serviceInstances.CheckoutSessionService,
		CreditsService:           serviceInstances.CreditsService,
		CreditTypeService:        serviceInstances.CreditTypeService,
		ProcessorCustomerService: serviceInstances.ProcessorCustomerService,
	}

	// River producer is always initialized in the runtime so HTTP handlers can enqueue jobs
	// even when workers run in a separate process.
	if producer, pool, err := buildRiverProducer(cfg); err != nil {
		return nil, fmt.Errorf("init river producer: %w", err)
	} else {
		runtime.RiverProducer = producer
		runtime.riverProducerPool = pool
		// Wire the deferred NMI delete scheduler now that the producer exists
		// (issue 216). Without it, NMI cancellations fall back to deleting inline.
		if runtime.UserSubscriptionService != nil {
			runtime.UserSubscriptionService.SetDeferredDeleteScheduler(newRiverDeferredDeleteScheduler(producer))
		}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating pgx pool for River producer: %w", err)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:              "billing",
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
	return database, nil
}

func validateDatabase(cfg *config.Config, database *db.DB) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}

	// Validate that all migrations have been applied before starting
	bunDB, ok := database.GetDB().(*bun.DB)
	if !ok || bunDB == nil || bunDB.DB == nil {
		return fmt.Errorf("database must wrap *bun.DB")
	}
	sqlDB := bunDB.DB

	if err := migratekit.ValidatePostgresMigrations(context.Background(), sqlDB,
		migratekit.MigrationSource{App: "billing", FS: postgresmigrations.FS},
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
				App:        "billing",
			},
			clickhousemigrations.FS,
		); err != nil {
			log.WithError(err).Warn("ClickHouse migrations validation failed - analytics disabled")
		}
	}

	return nil
}

func createNMIClients(cfg *config.Config) (map[string]*nmi.NMIClient, error) {
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

		client, err := nmi.NewClient(providerKey, settings, cfg.IsTestMode())
		if err != nil {
			return nil, err
		}

		clients[providerKey] = client
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

	return ccbill.NewClient(ccbillProc.ToCCBillConfig(), cfg.IsTestMode())
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
	VaultService             *vault.VaultService
	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaRPC                *solana.RPCClient
	SolanaPriceProvider      solanamodule.TokenPriceProvider
	FXProvider               fx.Provider

	UserSubscriptionService   *subscriptions.UserSubscriptionService
	PublicSubscriptionService *catalog.PublicSubscriptionService
	AdminSubscriptionService  *subscriptions.AdminSubscriptionService

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	DeduplicationService         *webhooks.DeduplicationService
	IdempotencyService           *idempotency.IdempotencyService
	WebhookDispatcher            *webhooks.WebhookDispatcher

	CheckoutService          *checkout.CheckoutService
	CheckoutSessionService   *checkout.CheckoutSessionService
	CreditsService           *credits.CreditsService
	CreditTypeService        *credits.CreditTypeService
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
	creditsService := credits.NewCreditsService(database, clock)
	creditTypeService := credits.NewCreditTypeService(database)
	processorCustomerService := payments.NewProcessorCustomerService(database)
	profileRepo := repo.NewProfileRepo(database)

	// Create FX provider for Solana token quoting with non-USD prices
	// Uses CC0 exchange-api with 5-minute cache TTL
	fxProvider := fx.NewCachedProvider(fx.NewExchangeAPIProvider(), 5*time.Minute)

	// Note: solanaPayService and SolanaPayPoller need checkoutService, which is created later
	// We'll create solanaPayService with nil checkoutService and set it after checkoutService is created
	solanaPayService := solanamodule.NewSolanaPayService(database, redisClient, cfg, priceService, productService, nil, fxProvider, solanaPriceProvider, clock)
	var solanaRPC *solana.RPCClient
	if solanaProc := cfg.GetSolanaProcessor(); solanaProc != nil {
		solanaNetwork := effectiveSolanaNetwork(cfg, solanaProc)
		solanaRPC = solana.NewRPCClientWithConfig(solana.RPCClientConfig{
			Endpoint:     solanaProc.RPCEndpoint,
			HeliusAPIKey: solanaProc.HeliusAPIKey,
			Network:      solanaNetwork,
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

	vaultService := vault.NewVaultService(paymentMethodService, subscriptionService, nmiClients, database, clock)
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
		CreditsService:               creditsService,
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
		VaultService:                 vaultService,
		SolanaPayService:             solanaPayService,
		SolanaPayPoller:              solanaPayPoller,
		SolanaTransactionService:     solanaTransactionService,
		SolanaRPC:                    solanaRPC,
		SolanaPriceProvider:          solanaPriceProvider,
		FXProvider:                   fxProvider,
		UserSubscriptionService:      userSubscriptionService,
		PublicSubscriptionService:    publicSubscriptionService,
		AdminSubscriptionService:     adminSubscriptionService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		DeduplicationService:         deduplicationService,
		IdempotencyService:           idempotencyService,
		WebhookDispatcher:            webhookDispatcher,
		CheckoutService:              checkoutService,
		CheckoutSessionService:       checkoutSessionService,
		CreditsService:               creditsService,
		CreditTypeService:            creditTypeService,
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating pgx pool for River: %w", err)
	}

	drv := riverpgxv5.New(pool)
	client, err := river.NewClient(drv, &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault:     {MaxWorkers: standaloneRiverDefaultQueueMaxWorkers},
			riverjobs.QueueBilling: {MaxWorkers: standaloneRiverBillingQueueMaxWorkers},
		},
		Schema:  riverSchema,
		Workers: workers,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River client: %w", err)
	}
	return client, pool, nil
}
