package app

import (
	"io/fs"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/writeposture"

	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/email"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/integrations/pyth"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/copilot"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/railresolve"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

const (
	standaloneRiverDefaultQueueMaxWorkers = 10
	standaloneRiverBillingQueueMaxWorkers = 20
	// #719: the global cap on concurrent per-merchant provider refresh jobs —
	// the simple, honest per-provider rate limit (thousands of merchants share
	// each provider's API budget; a small worker cap is the brake).
	standaloneRiverProviderRefreshQueueMaxWorkers = 4
	// riverPSPImportQueueMaxWorkers bounds the PSP pulls one fleet runs at
	// once: after an upgrade every PSP of every merchant pulls.
	riverPSPImportQueueMaxWorkers = 2
	// idempotencyLeaseConns sizes the lease-renewal pool: one short UPDATE per
	// held claim every quarter lease.
	idempotencyLeaseConns = 4
)

type runtimeOverrides struct {
	StripeTransport http.RoundTripper
	// BasisTheoryTransport substitutes the Basis Theory wire; test custody only.
	BasisTheoryTransport http.RoundTripper
	NMITransport         http.RoundTripper
	FXTransport          http.RoundTripper
	DB                   *db.DB
	Redis                redis.UniversalClient
	Clock                clockwork.Clock
	EmailSender          config.EmailSender
	UserInfo             userinfo.Lookup
	Notifications        *config.NotificationChannel
	Migrations           fs.FS
	Vault                *vaultapi.Client
}

// devnetParityPriceProvider wraps the Pyth client (#360) for a merchant whose
// Solana PSP is on devnet. Devnet money is fake and a sandbox PSP must never
// require Hermes: feed-backed symbols are still priced via Hermes when it is
// reachable (realistic SOL quotes), but ANY failure — missing feed, network
// error, staleness — degrades to $1.00 parity instead of erroring.
type devnetParityPriceProvider struct {
	inner   solanamodule.TokenPriceProvider
	network func(context.Context) string
}

func (p devnetParityPriceProvider) PriceUSD(ctx context.Context, symbol string) (float64, error) {
	if p.network == nil || p.network(ctx) == config.EnvironmentMainnet {
		return p.inner.PriceUSD(ctx, symbol)
	}
	if p.inner != nil {
		if price, err := p.inner.PriceUSD(ctx, symbol); err == nil && price > 0 {
			return price, nil
		} else if err != nil {
			log.WithError(err).WithField("token", symbol).
				Debug("solana devnet/testnet: pyth price unavailable; using $1.00 parity (fake money)")
		}
	}
	return 1.0, nil
}

func createPythPriceProvider(network func(context.Context) string) (solanamodule.TokenPriceProvider, error) {
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
	return devnetParityPriceProvider{inner: client, network: network}, nil
}

func buildRuntimeWithOverrides(ctx context.Context, cfg *config.Config, overrides *runtimeOverrides) (_ *Runtime, buildErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Create clock early so it can be passed to services.
	clock := runtimeClock(overrides)

	var (
		database    *db.DB
		redisClient redis.UniversalClient
		err         error
	)
	var chain fs.FS
	if overrides != nil {
		database, chain = overrides.DB, overrides.Migrations
	}
	if database == nil {
		if database, err = db.NewDB(ctx, cfg); err != nil {
			return nil, fmt.Errorf("failed to create db: %w", err)
		}
	}
	// OpenRails' and River's tables, before anything else touches the database.
	prepare := migrate.Apply
	if cfg.Database.SkipMigrations {
		prepare = migrate.Validate
	}
	if err := prepare(ctx, database.Pool(), migrate.Options{
		Schema: config.SchemaName(cfg), RiverSchema: config.RiverSchemaName(cfg), Chain: chain,
	}); err != nil {
		if overrides == nil || database != overrides.DB {
			_ = database.Close()
		}
		return nil, err
	}
	if overrides != nil && overrides.Notifications != nil {
		if overrides.Notifications.Sink == nil || overrides.Notifications.Recipient == nil {
			return nil, fmt.Errorf("notification channel requires a transactional sink and recipient mapping")
		}
		database.SetNotifications(overrides.Notifications)
	}

	redisOwned := false
	if overrides != nil && !nilRedis(overrides.Redis) {
		redisClient = overrides.Redis
	} else {
		redisClient, err = createRedisClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis client: %w", err)
		}
		redisOwned = redisClient != nil
	}

	// #788 Layer C: ONE rail resolution seam for the whole runtime. The
	// merchants service is wired after the services it arms, so every
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
	if api := config.SandboxStripeAPIURL(cfg); api != "" {
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
	if overrides != nil {
		collectionResolver.BasisTheoryTransport = overrides.BasisTheoryTransport
	}
	// #728/#788: per-merchant Solana RPC (poller, crank, intent verify legs,
	// request-plane transaction builds).
	solanaRPCResolver := &solanamodule.MerchantRPCBuilder{
		Config:      cfg,
		MerchantsFn: merchantsFn,
		Endpoint:    config.SandboxSolanaRPCURL(cfg),
	}
	solanaPriceProvider, err := createPythPriceProvider(solanaRPCResolver.Network)
	if err != nil {
		return nil, err
	}

	// Who customers are: the host's directory in process, else the kept copy.
	var directory identity.Directory = identity.Kept{DB: database}
	hostUserInfo := overrides != nil && overrides.UserInfo != nil
	if hostUserInfo {
		directory = identity.Live{Lookup: overrides.UserInfo, DB: database}
	}
	customers := identity.Customers{Directory: directory}
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
	var fxTransport http.RoundTripper
	if overrides != nil {
		fxTransport = overrides.FXTransport
	}
	fxRates := fx.NewRates(fx.NewSource(fxTransport))
	serviceInstances, err := createServices(database, leaseDB, cfg, railConfigs, collectionResolver, solanaRPCResolver, fxRates, clock, solanaPriceProvider, customers, stripeClients)
	if err != nil {
		return nil, err
	}

	// One sender for all of OpenRails' email: the host's, else Config.SMTP.
	var sender config.EmailSender
	var smtpErr error
	if overrides != nil && overrides.EmailSender != nil {
		sender = overrides.EmailSender
	} else if cfg.SMTP != nil {
		smtp, err := email.NewSMTP(*cfg.SMTP)
		if err != nil {
			smtpErr = fmt.Errorf("%w; OpenRails sends no email", err)
		} else {
			sender = smtp
		}
	}
	// Operator alerts and AuthKit still use the deployment's sender.
	alertEmailSender := alerting.NewMailSender(sender, merchantconfig.NewStore(database))
	alertService := alerting.NewService(alerting.Deps{
		DB:               database,
		Email:            alertEmailSender,
		Clock:            clock,
		DashboardBaseURL: alertingDashboardBaseURL(cfg),
	})

	// Card-abuse guard (#371): the captcha accelerator over the ledger below.
	// It needs a captcha to solve; nil (safe no-op) otherwise, and the
	// ledger's blocks are the whole policy.
	abuseState := abusestate.New(redisClient)
	captchaStore := captcha.NewChallengeStore(abuseState)
	var cardAbuseGuard *abuse.CardAbuseGuard
	if config.CaptchaEnabled(cfg.Captcha) {
		cardAbuseGuard = abuse.NewCardAbuseGuard(abuseState, captchaStore, abuse.DefaultCardAbuseConfig())
	}

	// SEC-30: the card-testing ledger, in the abuse state.
	cardFailureLedger := abuse.NewFailureLedger(abuseState, clock, abuse.DefaultCardAbuseConfig())
	serviceInstances.OrderAttemptService.SetCardFailureLedger(cardFailureLedger)

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
		PaymentService:           serviceInstances.PaymentService,
		EntitlementService:       serviceInstances.EntitlementService,
		ProductAccessService:     serviceInstances.ProductAccessService,
		RailPaymentMethodService: serviceInstances.RailPaymentMethodService,
		SolanaPayService:         serviceInstances.SolanaPayService,
		SolanaPayPoller:          serviceInstances.SolanaPayPoller,
		SolanaTransactionService: serviceInstances.SolanaTransactionService,
		SolanaPriceProvider:      solanaPriceProvider,
		FXProvider:               serviceInstances.FXProvider,
		FXRates:                  serviceInstances.FXRates,

		UserSubscriptionService:      serviceInstances.UserSubscriptionService,
		AdminSubscriptionService:     serviceInstances.AdminSubscriptionService,
		PriceMigrationService:        serviceInstances.PriceMigrationService,
		EmailSender:                  sender,
		SubscriptionLifecycleService: serviceInstances.SubscriptionLifecycleService,
		WebhookDispatcher:            serviceInstances.WebhookDispatcher,
		DeduplicationService:         serviceInstances.DeduplicationService,

		PurchaseService:     serviceInstances.PurchaseService,
		OrderAttemptService: serviceInstances.OrderAttemptService,
		Orders:              orders.New(database, serviceInstances.SubscriptionLifecycleService, clock),
		Idempotency:         serviceInstances.Idempotency,
		CardAbuseGuard:      cardAbuseGuard,
		AbuseState:          abuseState,
		CaptchaStore:        captchaStore,
		CardFailureLedger:   cardFailureLedger,
		MoneyService:        serviceInstances.MoneyService,
		MetricsService:      serviceInstances.MetricsService,
		DashboardService:    serviceInstances.DashboardService,
		CopilotService:      serviceInstances.CopilotService,
		AlertService:        alertService,
		WebhookHealth:       &webhookhealth.Recorder{DB: database, Clock: clock},
		MoneyCharger:        moneyCharger,
		RailCustomerService: serviceInstances.RailCustomerService,
	}

	// Arm the late-bound resolvers built above (#788): they close over
	// runtimeRef, so every post-boot Merchants bind is visible immediately.
	runtime.DestructiveAdmissions = runtime.NewDestructiveAdmissions(clock)
	// Redirect PSPs and bank challenges come back to the payment page itself.
	serviceInstances.OrderAttemptService.SetCheckoutOrigin(runtime.CheckoutOrigin)
	runtime.AdminSubscriptionService.SetDestructiveAdmissions(runtime.DestructiveAdmissions)
	runtime.AdminSubscriptionService.SetRequestedCancellationLimiter(func(ctx context.Context, sub *models.Subscription) error {
		return destructive.NewUserActionLimiter(runtime.DB, runtime.Config.UserActionRateLimit).Reserve(ctx, destructive.CancelSubscription, fmt.Sprintf("cancel:%s:%d", sub.ID, sub.LifecycleRev), sub.CustomerID)
	})
	runtime.PriceMigrationService.WithDestructiveAdmissions(runtime.DestructiveAdmissions).WithPushes(intents.PriceMigrationPushes{})
	runtime.MoneyService.SetDestructiveAdmissions(runtime.DestructiveAdmissions)
	runtime.ProductAccessService.SetDestructiveAdmissions(runtime.DestructiveAdmissions)
	runtimeRef = runtime
	if smtpErr != nil {
		runtime.Refuse("smtp", smtpErr)
	}
	if serviceInstances.HyperSwitchErr != nil {
		runtime.Refuse("hyperswitch", serviceInstances.HyperSwitchErr)
	}
	runtime.CollectionResolver = collectionResolver
	runtime.MoneyService.SetUsageRecoveryResolver(collectionResolver)
	moneyCharger.SetAdapterResolver(collectionResolver)
	// A declared loopback NMI gateway (config.ProviderSandbox) reaches every
	// NMI client through the shared factory.
	runtime.NMIClients = nmiClients
	if serviceInstances.PurchaseService != nil {
		serviceInstances.PurchaseService.NMIClients = nmiClients
		if held := serviceInstances.PurchaseService.CustodianSaleService; held != nil {
			held.NMIClients = nmiClients
			if overrides != nil {
				held.BasisTheoryTransport = overrides.BasisTheoryTransport
			}
		}
	}
	if serviceInstances.OrderAttemptService != nil {
		serviceInstances.OrderAttemptService.SetPSPPosture(runtime.PSPPostureDisarmed)
	}
	if runtime.RailPaymentMethodService != nil {
		runtime.RailPaymentMethodService.NMIClients = nmiClients
		if overrides != nil {
			runtime.RailPaymentMethodService.BasisTheoryTransport = overrides.BasisTheoryTransport
		}
	}
	runtime.SolanaRPCResolver = solanaRPCResolver
	// #817: decimals come from the SPL mint on-chain, read through the same
	// per-merchant chain reader and cached (mint decimals are immutable).
	runtime.SolanaMintDecimals = solanamodule.NewMintDecimals(solanaRPCResolver.ChainReader())
	if serviceInstances.OrderAttemptService != nil {
		serviceInstances.OrderAttemptService.SetSolanaMintDecimals(runtime.SolanaMintDecimals)
		serviceInstances.OrderAttemptService.SetSolanaMintInfo(solanaRPCResolver.ChainReader())
	}
	if serviceInstances.SolanaPayPoller != nil {
		serviceInstances.SolanaPayPoller.SetMerchantRPC(solanaRPCResolver)
	}
	var borrowedVault *vaultapi.Client
	if overrides != nil {
		borrowedVault = overrides.Vault
	}
	var custodianTransport http.RoundTripper
	if overrides != nil {
		custodianTransport = overrides.BasisTheoryTransport
	}
	if err := runtime.wireMerchantConfig(ctx, borrowedVault, custodianTransport); err != nil {
		return nil, fmt.Errorf("initialize merchant configuration: %w", err)
	}

	// Jobs queue in RiverSchema from the start; Engine.Start binds the fleet
	// that runs them there, OpenRails' own or the host's.
	runtime.SetRiverSchema(config.RiverSchemaName(cfg))
	if err := runtime.BindRiverProducer(ctx); err != nil {
		return nil, err
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
	runtime.WebhookDispatcher.ObserveEnqueuer = &runtimeObserveEnqueuer{runtime: runtime}

	// #674: write-through provider intents. Producers post a durable intent and
	// execute it inline through the SAME registry/runner the scheduled
	// executor/verifier drains — one primitive, identical semantics.
	intentRunner := runtime.IntentRunner()
	if runtime.PurchaseService != nil {
		runtime.PurchaseService.Intents = intentRunner
		runtime.PurchaseService.Posture = runtime.Posture()
		if runtime.PurchaseService.NMISaleService != nil {
			runtime.PurchaseService.NMISaleService.Intents = intentRunner
			runtime.PurchaseService.NMISaleService.Config = runtime.Config
			runtime.PurchaseService.NMISaleService.Posture = runtime.Posture()
			if engines, ok := runtime.CollectionResolver.(intents.StripeEngineServiceResolver); ok {
				runtime.PurchaseService.NMISaleService.StripeEngines = engines
			}
		}
		if engines, ok := runtime.CollectionResolver.(intents.StripeEngineServiceResolver); ok {
			runtime.PurchaseService.StripeEngines = engines
		}
	}
	if runtime.OrderAttemptService != nil {
		runtime.OrderAttemptService.SetOrders(runtime.Orders)
	}
	// User card removals execute only in the explicit request. The retained
	// operation supplies idempotency and fences, without a background retry.
	if runtime.RailPaymentMethodService != nil {
		runtime.RailPaymentMethodService.DeleteIntents = &intents.PaymentMethodDeleteThrough{Runner: intentRunner,
			BeforeUserDelete: func(ctx context.Context, pm *models.PaymentMethod) error {
				return destructive.NewUserActionLimiter(runtime.DB, runtime.Config.UserActionRateLimit).Reserve(ctx, destructive.DeletePaymentMethod, "method-delete:"+pm.ID.String(), pm.CustomerID)
			},
		}
		runtime.RailPaymentMethodService.CardVaults = &intents.CardVaultThrough{Runner: intentRunner}
	}
	// #674: user/admin payment-method swaps route through the durable
	// nmi_payment_source_update intent (ambiguity ⇒ pending_verify, never a
	// silent local↔remote split).
	runtime.PaymentSourceUpdateIntents = &intents.PaymentSourceUpdateThrough{Runner: intentRunner, DB: database}
	runtime.Contacts, runtime.HostUserInfo = directory, hostUserInfo

	return runtime, nil
}

func runtimeClock(overrides *runtimeOverrides) clockwork.Clock {
	if overrides != nil && overrides.Clock != nil {
		return overrides.Clock
	}
	return clockwork.NewRealClock()
}

// createRedisClient opens the declared Redis. It does not wait for it: the
// health monitor reports it, degraded until it answers.
func createRedisClient(cfg *config.Config) (redis.UniversalClient, error) {
	if cfg.Redis == nil {
		return nil, nil
	}
	opts, err := config.RedisOptions(cfg.Redis)
	if err != nil {
		return nil, err
	}
	log.WithFields(log.Fields{"addr": opts.Addr, "tls": opts.TLSConfig != nil, "acl_user": opts.Username != ""}).Info("Redis declared")
	return redis.NewClient(opts), nil
}

// nilRedis reports a missing client, including a nil pointer a host passed
// as Deps.Redis.
func nilRedis(c redis.UniversalClient) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

type servicesInstances struct {
	// HyperSwitchErr is why the HyperSwitch deployment is not served.
	HyperSwitchErr      error
	SubscriptionService *subscriptions.SubscriptionService
	// Idempotency claims request keys (#1099).
	Idempotency *idempotency.Store

	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	PaymentService           *payments.PaymentService
	EntitlementService       *entitlements.EntitlementService
	ProductAccessService     *productaccess.Service
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	SolanaPayService         *solanamodule.SolanaPayService
	SolanaPayPoller          *solanamodule.SolanaPayPoller
	SolanaTransactionService *solanamodule.SolanaTransactionService
	SolanaPriceProvider      solanamodule.TokenPriceProvider
	FXProvider               fx.Provider
	FXRates                  *fx.Rates

	UserSubscriptionService  *subscriptions.UserSubscriptionService
	AdminSubscriptionService *subscriptions.AdminSubscriptionService
	// PriceMigrationService moves subscribers to another price at their
	// renewal.
	PriceMigrationService *subscriptions.PriceMigrationService

	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	DeduplicationService         *webhooks.DeduplicationService

	WebhookDispatcher *webhooks.WebhookDispatcher

	PurchaseService     *payment.PurchaseService
	OrderAttemptService *payment.OrderAttemptService
	MoneyService        *money.MoneyService
	MetricsService      *metrics.Service
	DashboardService    *dashboard.Service
	CopilotService      *copilot.Service
	RailCustomerService *payments.RailCustomerService
}

// alertingDashboardBaseURL uses the independently configured admin destination.
// Empty leaves alert links relative to the consuming console.
func alertingDashboardBaseURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimRight(cfg.DashboardBaseURL, "/")
}

func createServices(database, leaseDB *db.DB, cfg *config.Config, railConfigs railresolve.Source, collectionResolver *money.MerchantCollectionAdapterBuilder, solanaRPCResolver *solanamodule.MerchantRPCBuilder, fxRates *fx.Rates, clock clockwork.Clock, solanaPriceProvider solanamodule.TokenPriceProvider, usernameResolver identity.UsernameResolver, stripeClients *stripeapi.Factory) (*servicesInstances, error) {
	productService := catalog.NewProductService(database)
	priceService := catalog.NewPriceService(database)
	// Domain services publish immutable notification facts; applications consume them.
	notificationService := subscriptions.NewNotificationService(database)
	paymentMethodService := paymentmethods.NewPaymentMethodService(database)
	paymentService := payments.NewPaymentService(database, clock)
	entitlementService := entitlements.NewEntitlementService(database, clock)
	productAccessService := productaccess.NewService(database, clock)
	moneyService := money.NewMoneyService(database, clock)
	moneyService.EngineAdmissionHold = cfg.EngineAdmissionHold
	moneyService.UsageBillingEnabled = cfg.UsageBillingEnabled
	var hyperSwitchErr error
	if cfg.HyperSwitch != nil {
		if err := moneyService.SetHyperSwitchDeployment(cfg.HyperSwitch.APIBaseURL); err != nil {
			hyperSwitchErr = fmt.Errorf("%w; HyperSwitch collection is off", err)
		}
	}
	metricsService := metrics.NewService(database)
	// The catalog copilot needs an LLM key and explicit consent to send
	// catalog data to the provider.
	var catalogLLM copilot.LLM
	if config.LLMConfigured(cfg.LLM) {
		llmBaseURL := strings.TrimSpace(cfg.LLM.BaseURL)
		switch config.LLMProvider(cfg.LLM) {
		case config.LLMProviderOpenAI:
			catalogLLM = copilot.NewOpenAILLM(cfg.LLM.APIKey, config.LLMModel(cfg.LLM), llmBaseURL)
		default: // anthropic — unknown providers refuse boot in config.Validate
			catalogLLM = copilot.NewAnthropicLLM(cfg.LLM.APIKey, config.LLMModel(cfg.LLM), llmBaseURL)
		}
	}
	dashboardService := dashboard.NewService(database)
	railCustomerService := payments.NewRailCustomerService(database)
	// FX for Solana token quotes and policy-currency admission: live rates
	// held in process memory. Never a flat 1.0 rate.
	fxProvider := fxRates

	// Note: solanaPayService and SolanaPayPoller need purchaseService, which is created later
	// We'll create solanaPayService with nil purchaseService and set it after purchaseService is created
	solanaPayService := solanamodule.NewSolanaPayService(database, clock)
	solanaTransactionService := solanamodule.NewSolanaTransactionService(database, nil, cfg, priceService, fxProvider, clock)
	solanaTransactionService.SetMerchantRPC(solanaRPCResolver)

	subscriptionLifecycleService := subscriptions.NewSubscriptionLifecycleService(
		database,
		productService,
		priceService,
		entitlementService,
		notificationService,
		paymentService, // For creating Payment records on renewal
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

	// Price migrations push to Stripe and to NMI gateway schedules through
	// the per-merchant client resolver.
	priceMigrationService := subscriptions.NewPriceMigrationService(database, priceService, subscriptionService, merchantconfig.NewStore(database),
		&subscriptions.StripeService{StripeClients: stripeClients, Config: cfg, Rails: railConfigs}, subscriptions.NewNMIPlanPusher(collectionResolver), clock)

	// Catalog Q&A and drafting are independently opt-in. Draft tools only
	// propose changes; the price-migration API enforces notice windows.
	copilotService := copilot.NewService(copilot.Deps{
		Products: productService,
		Prices:   priceService,
		Subs:     subscriptionService,
		Reprices: priceMigrationService,
		LLM:      catalogLLM,
		Enabled:  cfg.LLM != nil && cfg.LLM.CatalogCopilotEnabled,
		Drafting: cfg.LLM != nil && cfg.LLM.CatalogDraftingEnabled,
		Clock:    clock,
	})

	railPMService := paymentmethods.NewRailPaymentMethodService(paymentMethodService, subscriptionService, database, cfg, clock)
	subscriptionService.RailPaymentMethodService = railPMService
	idempotencyService, err := idempotency.NewStore(database, leaseDB, payment.IdempotencyTTL, payment.IdempotencyLease)
	if err != nil {
		return nil, err
	}
	webhookClaims, err := idempotency.NewStore(database, leaseDB, webhooks.WebhookClaimTTL, webhooks.WebhookClaimLease)
	if err != nil {
		return nil, err
	}

	postures := writeposture.View{Config: cfg, DB: database}
	userSubscriptionService := subscriptions.NewUserSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		paymentService,
		notificationService,
		entitlementService,
		collectionResolver,
		postures,
		clock,
	)

	adminSubscriptionService := subscriptions.NewAdminSubscriptionService(
		subscriptionService,
		productService,
		priceService,
		entitlementService,
		notificationService,
		paymentService,
		postures,
		clock,
	)
	adminSubscriptionService.StripeService = &subscriptions.StripeService{StripeClients: stripeClients, Config: cfg, Rails: railConfigs}

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
		PaymentService:               paymentService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		ProfileRepo:                  usernameResolver,
		DeduplicationService:         deduplicationService,
		RailCustomerService:          railCustomerService,
		RailConfigs:                  railConfigs,
		NMIResolver:                  collectionResolver,
		MoneyService:                 moneyService,
	}

	// Create purchase service for unified purchase endpoint
	purchaseService := payment.NewPurchaseService(
		subscriptionService,
		productService,
		priceService,
		paymentService,
		entitlementService,
		paymentMethodService,
		railPMService,
		railCustomerService,
		cfg,
		railConfigs,
		clock,
	)
	purchaseService.StripeClients = stripeClients
	purchaseService.StripeService.StripeClients = stripeClients
	purchaseService.SetSubscriptionLifecycleService(subscriptionLifecycleService)
	webhookDispatcher.PurchaseRegistrar = purchaseService
	orderAttemptService := payment.NewOrderAttemptService(
		database,
		priceService,
		productService,
		paymentMethodService,
		purchaseService,
		solanaPayService,
		solanaTransactionService,
		fxProvider,
		solanaPriceProvider,
		cfg,
		railConfigs,
		clock,
	)
	webhookDispatcher.OrderAttemptService = orderAttemptService

	// The poller settles through the order attempt service, the one
	// crediting path for Solana Pay purchases.
	solanaPayPoller := solanamodule.NewSolanaPayPoller(database, orderAttemptService, clock)

	return &servicesInstances{
		HyperSwitchErr:               hyperSwitchErr,
		SubscriptionService:          subscriptionService,
		ProductService:               productService,
		PriceService:                 priceService,
		NotificationService:          notificationService,
		PaymentMethodService:         paymentMethodService,
		PaymentService:               paymentService,
		EntitlementService:           entitlementService,
		ProductAccessService:         productAccessService,
		RailPaymentMethodService:     railPMService,
		SolanaPayService:             solanaPayService,
		SolanaPayPoller:              solanaPayPoller,
		SolanaTransactionService:     solanaTransactionService,
		SolanaPriceProvider:          solanaPriceProvider,
		FXProvider:                   fxProvider,
		FXRates:                      fxRates,
		UserSubscriptionService:      userSubscriptionService,
		AdminSubscriptionService:     adminSubscriptionService,
		PriceMigrationService:        priceMigrationService,
		SubscriptionLifecycleService: subscriptionLifecycleService,
		DeduplicationService:         deduplicationService,
		WebhookDispatcher:            webhookDispatcher,
		PurchaseService:              purchaseService,
		OrderAttemptService:          orderAttemptService,
		Idempotency:                  idempotencyService,
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

// runtimeObserveEnqueuer is runtimeConvergeEnqueuer for catalog observe jobs.
type runtimeObserveEnqueuer struct {
	runtime *Runtime
}

func (e *runtimeObserveEnqueuer) EnqueueProviderObjectObserve(ctx context.Context, req webhooks.ObserveRequest) error {
	if e == nil || e.runtime == nil || e.runtime.RiverProducer == nil {
		return fmt.Errorf("provider object observe enqueuer: river producer unavailable")
	}
	return riverjobs.ProviderObjectObserveEnqueuer{Client: e.runtime.RiverProducer}.EnqueueProviderObjectObserve(ctx, req)
}
