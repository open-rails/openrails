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

	authkitPostgres "github.com/PaulFidika/authkit/migrations/postgres"
	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/db"
	repo "github.com/doujins-org/doujins-billing/internal/db/repo"
	"github.com/doujins-org/doujins-billing/internal/integrations/ccbill"
	"github.com/doujins-org/doujins-billing/internal/integrations/fx"
	"github.com/doujins-org/doujins-billing/internal/integrations/jupiter"
	"github.com/doujins-org/doujins-billing/internal/integrations/nmi"
	solana "github.com/doujins-org/doujins-billing/internal/integrations/solana"
	"github.com/doujins-org/doujins-billing/internal/processors"
	"github.com/doujins-org/doujins-billing/internal/services"
	clickhousemigrations "github.com/doujins-org/doujins-billing/migrations/clickhouse"
	postgresmigrations "github.com/doujins-org/doujins-billing/migrations/postgres"
	"github.com/doujins-org/migratekit"
	"github.com/jonboulle/clockwork"
)

type runtimeOverrides struct {
	DB    *db.DB
	Redis *redis.Client
}

func buildRuntimeWithOverrides(cfg *config.Config, overrides *runtimeOverrides) (*Runtime, error) {
	// Initialize NMI-backed processors from config BEFORE creating clients
	// This ensures IsNMIBacked() works correctly for all configured processors
	processors.InitNMIBackedProcessors(cfg)

	// Create clock early so it can be passed to services
	clock := clockwork.NewRealClock()

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

	serviceInstances := createServices(database, cfg, ccbillRESTClient, nmiClients, redisClient, clock)

	// Initialize Solana token registry (must be done here since createServices doesn't return errors)
	var solanaTokenRegistry *jupiter.TokenRegistry
	if solanaProc := cfg.GetSolanaProcessor(); solanaProc != nil {
		solanaTokenRegistry = jupiter.NewTokenRegistry()
		if len(solanaProc.EnabledTokens) > 0 {
			// Use new enabled_tokens approach with Jupiter lookup
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := solanaTokenRegistry.LoadFromJupiter(ctx, solanaProc.EnabledTokens); err != nil {
				cancel()
				return nil, fmt.Errorf("failed to initialize Solana token registry: %w", err)
			}
			cancel()
		} else if len(solanaProc.SupportedTokens) > 0 {
			// Use supported_tokens config (backwards compatibility)
			legacyTokens := make(map[string]struct {
				Symbol      string
				Name        string
				Mint        string
				MainnetMint string
				Decimals    int
				Enabled     bool
			})
			for symbol, t := range solanaProc.SupportedTokens {
				legacyTokens[symbol] = struct {
					Symbol      string
					Name        string
					Mint        string
					MainnetMint string
					Decimals    int
					Enabled     bool
				}{
					Symbol:      t.Symbol,
					Name:        t.Name,
					Mint:        t.Mint,
					MainnetMint: t.MainnetMint,
					Decimals:    t.Decimals,
					Enabled:     t.Enabled,
				}
			}
			solanaTokenRegistry.LoadFromConfig(legacyTokens)
		} else {
			// No tokens configured - use default enabled_tokens with Jupiter lookup
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := solanaTokenRegistry.LoadFromJupiter(ctx, nil); err != nil {
				cancel()
				log.WithError(err).Warn("Failed to load default tokens from Jupiter - Solana payments may be limited")
			}
			cancel()
		}
	}

	var emailService *services.EmailService
	if cfg.SendGrid != nil {
		if es, err := services.NewEmailService(cfg.SendGrid, cfg.Store); err != nil {
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
		SolanaTokenRegistry:      solanaTokenRegistry,
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

	if cfg.ClickHouse != nil {
		if bes, err := services.NewEventLogService(cfg.ClickHouse); err != nil {
			log.WithError(err).Warn("EventLogService init failed; analytics disabled")
		} else {
			runtime.EventLogService = bes
		}
	}

	runtime.WebhookDispatcher.EventLogService = runtime.EventLogService
	runtime.SubscriptionLifecycleService.EventLogService = runtime.EventLogService

	return runtime, nil
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
		migratekit.MigrationSource{App: "authkit", FS: authkitPostgres.FS},
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
	if cfg == nil {
		return clients, nil
	}

	// Use the new unified config helper that checks both Processors map and legacy NMI config
	nmiProcessors := cfg.GetNMIProcessors()
	if len(nmiProcessors) == 0 {
		return clients, nil
	}

	for name, procConfig := range nmiProcessors {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" {
			providerKey = "mobius"
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

	client := ccbill.NewDataLinkClient(ccbillProc.ToCCBillConfig())
	if err := client.ValidateConfig(); err != nil {
		log.WithError(err).Warn("Invalid CCBill DataLink configuration; worker disabled")
		return nil
	}
	return client
}

type servicesInstances struct {
	SubscriptionService *services.SubscriptionService

	ProductService           *services.ProductService
	PriceService             *services.PriceService
	NotificationService      *services.NotificationService
	PaymentMethodService     *services.PaymentMethodService
	PurchaseService          *services.PaymentService
	EntitlementService       *services.EntitlementService
	VaultService             *services.VaultService
	SolanaPayService         *services.SolanaPayService
	SolanaPayPoller          *services.SolanaPayPoller
	SolanaTransactionService *services.SolanaTransactionService
	SolanaRPC                *solana.RPCClient
	FXProvider               fx.Provider

	UserSubscriptionService   *services.UserSubscriptionService
	PublicSubscriptionService *services.PublicSubscriptionService
	AdminSubscriptionService  *services.AdminSubscriptionService

	SubscriptionLifecycleService *services.SubscriptionLifecycleService
	DeduplicationService         *services.DeduplicationService
	IdempotencyService           *services.IdempotencyService
	WebhookDispatcher            *services.WebhookDispatcher

	CheckoutService          *services.CheckoutService
	CheckoutSessionService   *services.CheckoutSessionService
	CreditsService           *services.CreditsService
	ProcessorCustomerService *services.ProcessorCustomerService
}

func createServices(database *db.DB, cfg *config.Config, ccbillRESTClient *ccbill.RESTClient, nmiClients map[string]*nmi.NMIClient, redisClient *redis.Client, clock clockwork.Clock) *servicesInstances {
	productService := services.NewProductService(database)
	priceService := services.NewPriceService(database)
	// NotificationService created with nil emailService - will be set later in buildRuntime
	notificationService := services.NewNotificationService(database, nil)
	paymentMethodService := services.NewPaymentMethodService(database)
	purchaseService := services.NewPaymentService(database)
	purchaseService.Clock = clock
	entitlementService := services.NewEntitlementService(database)
	entitlementService.Clock = clock
	creditsService := services.NewCreditsService(database)
	creditsService.Clock = clock
	processorCustomerService := services.NewProcessorCustomerService(database)
	profileRepo := repo.NewProfileRepo(database)

	// Create FX provider for Solana token quoting with non-USD prices
	// Uses CC0 exchange-api with 5-minute cache TTL
	fxProvider := fx.NewCachedProvider(fx.NewExchangeAPIProvider(), 5*time.Minute)

	// Note: solanaPayService and SolanaPayPoller need checkoutService, which is created later
	// We'll create solanaPayService with nil checkoutService and set it after checkoutService is created
	solanaPayService := services.NewSolanaPayService(database, redisClient, cfg, priceService, productService, nil, fxProvider)
	solanaPayService.Clock = clock
	var solanaRPC *solana.RPCClient
	if solanaProc := cfg.GetSolanaProcessor(); solanaProc != nil {
		// Derive network from test_mode: devnet when true, mainnet when false
		solanaNetwork := "mainnet"
		if cfg.IsTestMode() {
			solanaNetwork = "devnet"
		}
		solanaRPC = solana.NewRPCClientWithConfig(solana.RPCClientConfig{
			Endpoint:     solanaProc.RPCEndpoint,
			HeliusAPIKey: solanaProc.HeliusAPIKey,
			Network:      solanaNetwork,
		})
	}
	solanaTransactionService := services.NewSolanaTransactionService(database, solanaRPC, cfg, priceService, purchaseService, fxProvider)
	solanaTransactionService.Clock = clock

	subscriptionLifecycleService := services.NewSubscriptionLifecycleService(
		database,
		productService,
		priceService,
		entitlementService,
		notificationService,
		purchaseService, // For creating Payment records on renewal
		nil,             // EventLogService - set later in buildRuntime after ClickHouse init
	)
	subscriptionLifecycleService.Clock = clock
	subscriptionLifecycleService.SetConfig(cfg) // For feature flag access (dunning_mode, etc.)

	subscriptionService := services.NewSubscriptionService(
		database,
		priceService,
		productService,
		notificationService,
		ccbillRESTClient,
		nmiClients,
		paymentMethodService,
	)
	subscriptionService.Clock = clock

	vaultService := services.NewVaultService(paymentMethodService, subscriptionService, nmiClients, database)
	vaultService.Clock = clock
	subscriptionService.VaultService = vaultService
	idempotencyService := services.NewIdempotencyService(redisClient)

	userSubscriptionService := services.NewUserSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		purchaseService,
		notificationService,
		entitlementService,
		nmiClients,
	)

	publicSubscriptionService := services.NewPublicSubscriptionService(
		productService,
		priceService,
	)

	adminSubscriptionService := services.NewAdminSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		entitlementService,
		notificationService,
		purchaseService,
		nmiClients,
	)

	deduplicationService := services.NewDeduplicationService(idempotencyService)
	webhookDispatcher := &services.WebhookDispatcher{
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
	checkoutService := services.NewCheckoutService(
		subscriptionService,
		productService,
		priceService,
		purchaseService,
		entitlementService,
		paymentMethodService,
		vaultService,
		idempotencyService,
		nmiClients,
		cfg,
	)
	checkoutService.Clock = clock
	webhookDispatcher.CheckoutService = checkoutService

	checkoutSessionService := services.NewCheckoutSessionService(
		database,
		priceService,
		productService,
		paymentMethodService,
		idempotencyService,
		checkoutService,
		solanaPayService,
		solanaTransactionService,
		fxProvider,
		cfg,
	)
	checkoutSessionService.Clock = clock
	webhookDispatcher.CheckoutSessionService = checkoutSessionService

	// Wire up checkoutService to solanaPayService for eligibility checks
	solanaPayService.SetCheckoutService(checkoutService)

	// Create SolanaPayPoller (depends on checkoutService for RegisterPurchase)
	solanaPayPoller := services.NewSolanaPayPoller(
		database,
		redisClient,
		cfg,
		solanaPayService,
		solanaTransactionService,
		checkoutService,
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

	// Get schema for River tables (same as billing schema)
	schema := "billing" // Hardcoded schema

	drv := riverpgxv5.New(pool)
	client, err := river.NewClient(drv, &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
			"billing":          {MaxWorkers: 20},
		},
		Schema:  schema, // Use billing schema for River tables
		Workers: workers,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("failed creating River client: %w", err)
	}
	return client, pool, nil
}
