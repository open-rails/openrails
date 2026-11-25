package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-rails/openrails/internal/modules/disputes"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/vaultconn"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	redis "github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/messages"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
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
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// Runtime aggregates infrastructure clients and application services.
type Runtime struct {
	// Log receives everything the graph logs.
	Log            *slog.Logger
	solanaBalances solanaint.BalanceCache
	// DestructiveAdmissions owns logical command limits and persistent incidents.
	DestructiveAdmissions *destructive.Admissions
	// providerPosture holds the PSP credentials verified at load; Ready
	// reports any that are disarmed or still unknown.
	providerPosture providerposture.Tracked
	// background owns goroutines that reconnect optional providers; Close
	// stops them first.
	background backgroundTasks
	// refusals are what the engine serves without (Refuse).
	refusals refusals
	// postureWait is what StartProviderPosture's last pass left unknown; Ready
	// counts a waiting PSP only while the registry holds no verdict for it.
	postureWait atomic.Pointer[postureWait]
	// NMIClients is the single PSP-scoped NMI client factory (#1055).
	NMIClients *railresolve.NMIFactory

	StripeClients *stripeapi.Factory
	DB            *db.DB
	// EventStore is the ClickHouse event store (Deps.ClickHouse); nil turns
	// usage, limits and admissions off (RequireEventStore).
	EventStore *eventstore.Store
	// leaseDB is the small pool lease renewals use: idempotency claims, the
	// billing pass and merchant pulls.
	leaseDB     *db.DB
	RedisClient redis.UniversalClient
	Config      *billing.Config
	// ProviderEndpoints are the loopback fakes the provider clients reach
	// (Deps.ProviderEndpoints); zero, the providers.
	ProviderEndpoints config.ProviderEndpoints

	// declared are the merchants Config declares (DeclareMerchants).
	declared atomic.Pointer[declaredMerchants]

	// TrustedProxies is the boot-configured proxy-aware client-IP resolver
	// (#746), built once from billing.Config.TrustedProxies. A nil/empty
	// resolver trusts nothing (raw socket peer only). Every consumer that
	// needs "the request's real client IP" — rate-limit subject keys, abuse
	// tracking, webhook IPAddress recording, the CCBill IP allowlist —
	// resolves through this ONE instance instead of reading
	// RemoteAddr/X-Forwarded-For itself.
	TrustedProxies *iputil.TrustedProxies

	Clock clockwork.Clock
	// Contacts is who customers are: the host's directory when HostUserInfo
	// is set (embedded Deps.UserInfo), else the kept copy SCIM and verified
	// token claims fill.
	Contacts     identity.Directory
	HostUserInfo bool
	// RiverProducer inserts jobs: the bound fleet, OpenRails' own or the
	// host's, else the insert-only client New binds in the same schema.
	RiverProducer  *river.Client[pgx.Tx]
	insertProducer *river.Client[pgx.Tx]
	RiverClient    *river.Client[pgx.Tx]

	SubscriptionService      *subscriptions.SubscriptionService
	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	NotificationService      *subscriptions.NotificationService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	PaymentService           *payments.PaymentService
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	// PriceMigrationService moves subscribers to another price at their
	// renewal.
	PriceMigrationService *subscriptions.PriceMigrationService
	// PaymentSourceUpdateIntents routes NMI payment-method swaps through the
	// durable nmi_payment_source_update intent (#674 write-through). Set by the
	// composition root alongside the other write-through producers.
	PaymentSourceUpdateIntents *intents.PaymentSourceUpdateThrough

	UserSubscriptionService  *subscriptions.UserSubscriptionService
	AdminSubscriptionService *subscriptions.AdminSubscriptionService
	// EmailSender delivers all of OpenRails' email; nil sends none.
	EmailSender config.EmailSender
	// Messages sends customers their billing messages.
	Messages *messages.Service
	// MRRRisk re-evaluates the risk of the customers a change outside their
	// subscriptions queued.
	MRRRisk *subscriptions.MRRRisk

	EntitlementService   *entitlements.EntitlementService
	ProductAccessService *productaccess.Service
	MoneyService         *money.MoneyService
	// MetricsService is the #733 merchant analytics query engine.
	MetricsService *metrics.Service
	// Disputes records disputes and fraud reports and evaluates the card
	// networks' monitoring programs.
	Disputes *disputes.Service
	// DashboardService is the merchant's saved dashboard layout.
	DashboardService *dashboard.Service
	// CheckoutOrigin is where a server serving the payment page
	// serves it; merchant invoices take a purchase only when it is set.
	checkoutOrigin atomic.Pointer[string]
	// paymentDomains are the (PSP, host) registrations known queued.
	paymentDomains sync.Map
	// AlertService delivers immediate merchant notifications and manages webhooks.
	AlertService *alerting.Service
	// WebhookHealth records inbound-webhook liveness per (merchant, rail) at the
	// ingest verify seam (#786). Nil-safe: recording never fails a webhook.
	WebhookHealth *webhookhealth.Recorder

	MoneyCharger        *money.ScopedCharger
	RailCustomerService *payments.RailCustomerService
	Merchants           *merchants.Service
	// Vault is the runtime's Vault connection: the merchant configuration
	// mount when one is named, and Transit signing.
	Vault *vaultconn.Connection
	// MerchantConfig is every merchant's configuration, from a file or
	// Vault, cached in memory.
	MerchantConfig *merchantdocs.Cache
	// hostDeps are the dependencies the host supervises (ReportDependency).
	hostDepsMu sync.Mutex
	hostDeps   []hostDependency

	// configMu guards configReady and configServing (EnsureMerchantsService).
	configMu      sync.Mutex
	configReady   bool
	configServing bool
	// CollectionResolver is the ONE #725/#788 store-armed per-merchant
	// credential resolver (invoice collection adapters + NMI clients for rebills,
	// cancels, refunds and admin actions).
	CollectionResolver *money.MerchantCollectionAdapterBuilder
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
	FXProvider          *fx.Rates
	// FXRates is FXProvider: rates held in memory, refreshed from the
	// process's first cross-currency quote until Close.
	FXRates *fx.Rates
	// SolanaCrankers are the deployment's listed crankers; nil with none.
	SolanaCrankers *recurring.Crankers
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

	PurchaseService       *payment.PurchaseService
	InvoiceAttemptService *payment.InvoiceAttemptService
	// Invoices are purchases (#1168).
	Invoices *invoices.Service
	// Idempotency claims request keys (#1099).
	Idempotency *idempotency.Store

	// CardAbuseGuard escalates repeated card-charge failures to a captcha, and
	// to a captcha for everyone while the ledger reports an attack (#371). Nil
	// without a captcha (safe no-op).
	CardAbuseGuard *abuse.CardAbuseGuard
	// AbuseState is the process's one store of rate-limit windows, admin
	// lockouts, captcha challenges and spent DPoP proofs: in Redis, else in
	// memory.
	AbuseState *abusestate.Store
	// CaptchaStore is the process's one captcha challenge store, shared by the
	// card-abuse guard and every HTTP surface so a solve clears it everywhere.
	CaptchaStore *captcha.ChallengeStore
	// CardFailureLedger is the card-testing ledger (SEC-30), counted in
	// AbuseState.
	CardFailureLedger *abuse.FailureLedger

	riverCompositionMu     sync.Mutex
	riverCompositionSealed bool
	riverClosed            atomic.Bool
	riverContributions     []riverhelpers.Contribution
	// riverWorkers are the workers the composition registered.
	riverWorkers           []any
	riverCompositionFailed bool
	riverStarted           bool
	workerConsumerRunning  atomic.Bool
	externalRiverClient    bool
	riverSchema            string // Config.Database.RiverSchema's, or the bound host client's
	// DeferredDeletes is the SYSTEM-origin deferred NMI-delete scheduler: it
	// enqueues a durable nmi_delete_subscription intent, which its own River
	// job runs. The dunning worker threads it into its per-run lifecycle so terminal
	// cancellations schedule the rail-side delete through the ONE
	// mechanism (no inline deletes). User-asked cancellations use a separate
	// user-origin instance wired into UserSubscriptionService.
	DeferredDeletes subscriptions.ProviderCancelScheduler
	// Verifier reads unverified subscriptions from their provider as soon as
	// they become unverified; started with the billing workers.
	Verifier *reconcile.Verifier
	// changes are the watchers of committed merchant changes (#1232).
	changes merchantChanges
}

// MerchantConfigManageable reports whether merchant configuration can change at
// runtime: Vault holds it. A file is read-only.
func (r *Runtime) MerchantConfigManageable() bool {
	return r != nil && r.Config != nil && config.MerchantConfigKVMount(r.Config) != ""
}

// declaredMerchants are the merchants Config declares: their ids, and the
// one each declared issuer binds.
type declaredMerchants struct {
	ids     []billing.MerchantID
	issuers map[string]billing.MerchantID
}

// DeclareMerchants records the merchants Config declares, before routes or
// workers run: ids in declaration order, and for each MerchantDeclaration.
// Issuer the merchant its credentials are bound to. Nothing else binds a
// request to a merchant by default.
func (r *Runtime) DeclareMerchants(ids []billing.MerchantID, issuers map[string]billing.MerchantID) {
	r.declared.Store(&declaredMerchants{ids: slices.Clone(ids), issuers: maps.Clone(issuers)})
}

// DeclaredMerchants are the merchants Config declares; none on a standalone
// server, whose merchants are provisioned at run time.
func (r *Runtime) DeclaredMerchants() []billing.MerchantID {
	if r == nil {
		return nil
	}
	if d := r.declared.Load(); d != nil {
		return slices.Clone(d.ids)
	}
	return nil
}

// IssuerMerchant is the merchant that declares issuer as its own: every
// credential of that issuer bound to no scope acts at it.
func (r *Runtime) IssuerMerchant(issuer string) (billing.MerchantID, bool) {
	if r == nil || issuer == "" {
		return billing.MerchantID{}, false
	}
	d := r.declared.Load()
	if d == nil {
		return billing.MerchantID{}, false
	}
	id, ok := d.issuers[issuer]
	return id, ok && !id.IsZero()
}

// MerchantIssuer is the Issuer the merchant declares, "" without one.
func (r *Runtime) MerchantIssuer(mid billing.MerchantID) string {
	if r == nil {
		return ""
	}
	if d := r.declared.Load(); d != nil {
		for issuer, id := range d.issuers {
			if id == mid {
				return issuer
			}
		}
	}
	return ""
}

// EmbedOrigins are merchant mid's checkout.embed_origins, its own sites.
func (r *Runtime) EmbedOrigins(ctx context.Context, mid billing.MerchantID) ([]string, error) {
	if r == nil || r.Merchants == nil {
		return nil, nil
	}
	_, settings, err := r.Merchants.MerchantSettings(merchant.WithID(ctx, mid), mid)
	if err != nil {
		return nil, err
	}
	return merchantconfig.EmbedOrigins(settings), nil
}

// ReturnOrigins are the exact origins merchant mid's return URLs may name
// (config.ReturnOrigins): the billing origin, its declared Issuer's and its
// checkout.embed_origins.
func (r *Runtime) ReturnOrigins(ctx context.Context, mid billing.MerchantID) ([]string, error) {
	embed, err := r.EmbedOrigins(ctx, mid)
	if err != nil {
		return nil, err
	}
	var cfg *billing.Config
	if r != nil {
		cfg = r.Config
	}
	return config.ReturnOrigins(cfg, r.MerchantIssuer(mid), embed), nil
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
	if r.Vault != nil {
		defer r.Vault.Close()
	}
	if r.MerchantConfig != nil {
		defer r.MerchantConfig.Close()
	}

	if r.Verifier != nil {
		r.Verifier.Close()
	}

	if r.FXRates != nil {
		r.FXRates.Close()
	}

	// Stop Solana Pay poller
	if r.SolanaPayPoller != nil {
		r.Log.InfoContext(ctx, "Stopping Solana Pay poller...")
		r.SolanaPayPoller.Stop()
	}

	// Only stop River client if we created it (not external)
	if r.RiverClient != nil && r.riverStarted && !r.externalRiverClient {
		r.workerConsumerRunning.Store(false)
		r.Log.InfoContext(ctx, "Stopping River background workers...")
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
	_, err := riverhelpers.New(ctx, r.DB.Pool(), &river.Config{
		Schema:   r.riverSchemaOrDefault(),
		PollOnly: r.DB.UsesTransactionPooler(),
		Logger:   r.Log,
	}, r.riverJobs(false))
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
// fleet and the non-River loops (the Solana Pay poller). With fleet nil it binds and starts OpenRails' own River client.
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
	var pollerDone, dispatchDone chan struct{}
	if r.SolanaPayPoller != nil {
		pollerDone = make(chan struct{})
		go func() { defer close(pollerDone); r.SolanaPayPoller.Start(loopCtx) }()
	}
	if r.Messages != nil {
		dispatchDone = make(chan struct{})
		go func() { defer close(dispatchDone); r.Messages.Run(loopCtx) }()
	}
	var wakeDone chan struct{}
	if r.Disputes != nil {
		wakeDone = make(chan struct{})
		go func() { defer close(wakeDone); r.Disputes.Run(loopCtx) }()
	}
	var riskDone chan struct{}
	if r.MRRRisk != nil {
		riskDone = make(chan struct{})
		go func() { defer close(riskDone); r.MRRRisk.Run(loopCtx) }()
	}
	joinLoops := func() {
		stopLoops()
		for _, done := range []chan struct{}{pollerDone, dispatchDone, wakeDone, riskDone} {
			if done != nil {
				<-done
			}
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
	r.Log.InfoContext(ctx, "Starting River background workers")
	if err := r.RiverClient.Start(cancelOnly(ctx)); err != nil {
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

// cancelOnly is ctx that ends only by cancellation: River's notifier spins
// on a context that ended by deadline, so River never sees one.
func cancelOnly(ctx context.Context) context.Context {
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, cancel)
	return run
}

// SetRiverSchema records the schema the bound River client keeps its tables
// in, so out-of-client reads (job health, periodic leases) use the same one.
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
	producer, err := river.NewClient(riverpgxv5.New(r.DB.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true, Logger: r.Log})
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

// CheckoutOrigin is safe to read from provider posture hooks started
// during New, while the host composes its payment page surface before Start.
func (r *Runtime) CheckoutOrigin() string {
	if value := r.checkoutOrigin.Load(); value != nil {
		return *value
	}
	return ""
}
func (r *Runtime) SetCheckoutOrigin(origin string) { r.checkoutOrigin.Store(&origin) }
