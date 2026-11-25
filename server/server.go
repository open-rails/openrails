package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/userinfo"
	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/server/internal/adminconsole"
	"github.com/open-rails/openrails/server/internal/controlplane"
	httpserver "github.com/open-rails/openrails/server/internal/http"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/open-rails/openrails/web/console"
	paypage "github.com/open-rails/openrails/web/pay"
)

// MerchantCreationConfig is Config.MerchantCreation: the policy for the
// merchants and names users claim.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved in addition to billing.ReservedMerchantSlugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role whose holders may claim
	// reserved names. Empty: reserved names are never user-claimable.
	ReservedEscalationRole string
	// SlugPattern further restricts claimed names (an unanchored regexp).
	SlugPattern string
	// FreeAllowance is how many merchants a user may own before creating
	// another requires a vaulted payment method (Deps.HasVaultedPaymentMethod).
	// Zero admits every creation that passes the name policy.
	FreeAllowance int
}

// AdminConsole is Config.AdminConsole: the console at /admin.
type AdminConsole struct {
	// Disabled omits the console's pages, assets and bootstrap endpoints.
	// The default keeps the console available; billing and auth APIs are
	// separate.
	Disabled bool `yaml:"disabled"`
}

// AuthConfig is Config.Auth: what a deployment states about the server's
// AuthKit. OpenRails derives the rest from the engine's settings: the issuer
// and resource (Engine.PublicBillingBaseURL), its schema
// (<database.schema>_auth), the client-IP posture, and the rules of the
// server's own accounts.
type AuthConfig struct {
	// Keys is where AuthKit's keys live.
	Keys AuthKeys `yaml:"keys"`
}

// AuthKeys is AuthConfig.Keys.
type AuthKeys struct {
	// Path is the directory holding AuthKit's signing keys (keys.json) and its
	// authenticator-app key (totp.key). Empty is /vault/auth, or for a
	// loopback public_billing_base_url development keys AuthKit generates.
	Path string `yaml:"path"`
}

// Config is a standalone deployment: plain data, as the openrails binary's
// configuration file spells it (the yaml keys). New fills a setting left zero
// from DefaultConfig.
type Config struct {
	// Engine is the engine's configuration, as any host passes to
	// openrails.New; its keys sit at the file's top level. The server's
	// merchants are provisioned at run time, each with its AuthKit group, and
	// manage their catalogs through the API or openrails catalog apply: a
	// declared merchant's Issuer is refused. Its CheckoutOrigin is served by
	// Host, and must also be apart from AuthKit's issuer and public URLs.
	Engine openrails.Config `yaml:",inline"`

	// Auth is what the deployment states about the server's AuthKit, which
	// decides who every request is; the server maps merchants to AuthKit
	// groups. Its own accounts are staff: they join by invitation, need a
	// second factor (a passkey or an authenticator app) and stay signed in
	// for 30 days.
	Auth AuthConfig `yaml:"auth"`
	// AuthKit adjusts the AuthKit configuration the server derives, before
	// the product's invariants are applied: the merchant roles, the API key
	// prefix, the issued audience, the resource's billing scopes, the
	// console's OAuth client, the client-IP posture and the HTTP mount. A
	// hosted product sets its account policy here (open registration with
	// MerchantCreation, agreements, usernames, group OAuth clients, SMS). Set
	// in code.
	AuthKit func(*authkit.Config) `yaml:"-"`
	// Naming is the rename policy for merchant names; AuthKit's username
	// policy governs usernames.
	Naming billing.NamingConfig `yaml:"naming"`
	// MerchantCreation is the policy for the merchants users create and
	// rename through a hosted product (ProvisionMerchant with an owner,
	// RenameUserMerchant); nil for operator-provisioned deployments. With it,
	// AuthKit may open native registration. Set in code.
	MerchantCreation *MerchantCreationConfig `yaml:"-"`

	// RouteGroups turns the merchant route groups on, each off by default:
	// Admin, Catalog, MerchantConfig and Metrics, and Programmatic, each
	// route behind the server's own merchant permission (the Merchant*
	// permissions).
	RouteGroups openrails.RouteGroups `yaml:"route_groups"`
	// AdminConsole controls the console at /admin; the zero value keeps it
	// on. Staff sign in with the server's own accounts or, a button each, at
	// the merchants' trusted issuers (and root's) whose metadata names an
	// authorization endpoint. Alert links point at it.
	AdminConsole AdminConsole `yaml:"admin_console"`

	// Host and Port are where Run and Serve listen.
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// MetricsPort is where Run and Serve also serve MetricsHandler, the
	// operator's Prometheus /metrics, on Host, apart from the public surface.
	// Zero serves none.
	MetricsPort int `yaml:"metrics_port"`
}

// DefaultConfig is every setting's default: the engine's (openrails.
// DefaultConfig) and the server's own. A finite former-names duration left
// unset is 90 days.
func DefaultConfig() Config {
	return Config{
		Engine: openrails.DefaultConfig(),
		Naming: billing.NamingConfig{
			Enabled: new(true), RenameInterval: new(72 * time.Hour),
			FormerNames: billing.FormerNamesConfig{Mode: billing.FormerNamesFinite},
		},
		Host: "0.0.0.0",
		Port: 3053,
	}
}

// WithDefaults is c with every setting left zero at DefaultConfig's, the
// engine's by openrails.Config's WithDefaults.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	c.Engine = c.Engine.WithDefaults()
	c.Host = cmp.Or(c.Host, d.Host)
	c.Port = cmp.Or(c.Port, d.Port)
	c.Naming.Enabled = cmp.Or(c.Naming.Enabled, d.Naming.Enabled)
	c.Naming.RenameInterval = cmp.Or(c.Naming.RenameInterval, d.Naming.RenameInterval)
	c.Naming.FormerNames.Mode = cmp.Or(c.Naming.FormerNames.Mode, d.Naming.FormerNames.Mode)
	c.Naming.FormerNames = formerNamesDuration(c.Naming.FormerNames)
	return c
}

// formerNamesDuration is f with a finite mode's unset duration at 90 days.
func formerNamesDuration(f billing.FormerNamesConfig) billing.FormerNamesConfig {
	if f.Mode == billing.FormerNamesFinite && f.Duration == nil {
		f.Duration = new(2160 * time.Hour)
	}
	return f
}

// registrationGuard runs controlplane.RegistrationGuard inside a host's own
// Wrap.
func registrationGuard(host func(iam.Route, http.Handler) http.Handler) func(iam.Route, http.Handler) http.Handler {
	return func(route iam.Route, next http.Handler) http.Handler {
		next = controlplane.RegistrationGuard(route, next)
		if host != nil {
			next = host(route, next)
		}
		return next
	}
}

// Addr is c's public listener, host:port.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

const (
	// drainDelay is how long Run and Serve keep serving once ctx ends, with
	// /health/ready failing, so load balancers stop routing here before the
	// listener closes.
	drainDelay = 5 * time.Second
	// shutdownTimeout bounds finishing in-flight requests and closing the
	// server, background work included, after the drain. With drainDelay it
	// fits Kubernetes' default 30s grace period.
	shutdownTimeout = 20 * time.Second
)

// Deps is everything a standalone server reaches outside its process.
type Deps struct {
	// Engine is what openrails.New takes. AuthKit's tables live in the same
	// database. Nil CheckoutAssets serves the payment page build embedded in
	// this module, when the binary was built with one.
	Engine openrails.Deps
	// Auth is what the server's AuthKit reaches: its email and SMS senders,
	// its signing key source and the rest of authkit.Deps. Nil Postgres is a
	// pool of its own on the engine's database, nil Redis the engine's, nil
	// Logger the engine's, and nil Email renders AuthKit's email and sends it
	// through the engine's sender.
	Auth authkit.Deps
	// CustomerAdmissionPolicy may refuse an otherwise verified ordinary customer
	// request before automatic customer admission. The original identity and
	// resolved merchant/customer are preserved. Nil keeps the normal behavior;
	// staff, AuthKit, host-mapped routes and immutable pay URLs do not call it.
	CustomerAdmissionPolicy func(context.Context, auth.Verified, openrails.CustomerBinding) error
	// HasVaultedPaymentMethod answers whether a user has a payment method on
	// file, unlocking merchant creation beyond MerchantCreation.FreeAllowance.
	HasVaultedPaymentMethod func(ctx context.Context, userID string) (bool, error)
	// ConsoleAssets is a built admin console rooted at index.html. Nil uses
	// the console build embedded in the binary. Ignored when the console is disabled.
	ConsoleAssets fs.FS
}

// Server is a standalone OpenRails server.
type Server struct {
	cfg     Config
	engine  *openrails.Engine
	graph   *app.App
	pool    *pgxpool.Pool
	cp      *controlplane.ControlPlane
	surface *httpserver.Server

	closeOnce sync.Once
	closeErr  error
	log       *slog.Logger
}

func init() {
	operator.Of = func(srv any) (*app.App, *controlplane.ControlPlane) {
		if s, ok := srv.(*Server); ok && s != nil {
			return s.graph, s.cp
		}
		return nil, nil
	}
}

// New builds a standalone server the way any host builds on OpenRails: the
// engine, then its own AuthKit (which creates or upgrades its tables), then
// the control plane over both and the HTTP surface. ctx bounds the wait for
// the database.
// Engine.Database.SkipMigrations applies to the internal AuthKit as well.
func New(ctx context.Context, cfg Config, deps Deps) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.WithDefaults()
	for _, m := range cfg.Engine.Merchants {
		if m.Issuer != "" {
			return nil, fmt.Errorf("server: Engine.Merchants %q's Issuer binds an embedded host's own issuer to its merchant; a standalone server's credentials are bound to a merchant by its AuthKit group", m.Slug)
		}
	}
	consoleEnabled := !cfg.AdminConsole.Disabled
	opts := controlplane.Options{KeysPath: cfg.Auth.Keys.Path, Naming: cfg.Naming, Console: consoleEnabled, Configure: cfg.AuthKit}
	if c := cfg.MerchantCreation; c != nil {
		opts.MerchantCreation = &controlplane.MerchantCreationConfig{ReservedSlugs: c.ReservedSlugs, ReservedEscalationRole: c.ReservedEscalationRole, SlugPattern: c.SlugPattern}
	}
	akCfg, err := controlplane.AuthKit(&cfg.Engine, opts)
	if err != nil {
		return nil, err
	}
	if err := checkCheckoutOrigin(cfg, akCfg); err != nil {
		return nil, err
	}
	if cfg.Engine.CheckoutOrigin != "" && deps.Engine.CheckoutAssets == nil {
		deps.Engine.CheckoutAssets = paypage.FS()
	}
	if deps.Engine.UserInfo != nil {
		return nil, errors.New("server: Engine.UserInfo reads one embedded host's directory; a standalone server's merchants' customers are AuthKit's, pushed by their issuers over SCIM")
	}
	// Read once: the engine and the server log to the same logger.
	logger := logx.New(deps.Engine.Logger)
	deps.Engine.Logger = logger
	s := &Server{cfg: cfg, log: logger}
	deps.Engine.UserInfo = customerDirectory{s}
	eng, err := openrails.New(ctx, cfg.Engine, deps.Engine)
	if err != nil {
		return nil, err
	}
	s.engine, s.graph = eng, engine.Graph(eng)
	fail := func(err error) (*Server, error) {
		_ = s.Close(context.WithoutCancel(ctx))
		return nil, err
	}

	if c := cfg.MerchantCreation; c != nil && c.FreeAllowance > 0 {
		admission, err := operator.MerchantCreationAdmission(logger, func() *controlplane.ControlPlane { return s.cp }, operator.MerchantCreationPolicy{
			FreeAllowance: c.FreeAllowance, HasVaultedPaymentMethod: deps.HasVaultedPaymentMethod,
		})
		if err != nil {
			return fail(err)
		}
		opts.MerchantCreation.Admission = admission
	}

	akDeps := deps.Auth
	if akDeps.Postgres == nil {
		// AuthKit's own pool: an authority read can follow a pinned billing
		// read, so a one-connection engine pool must not also serve it.
		if s.pool, err = pgxpool.NewWithConfig(ctx, s.graph.Runtime.DB.Pool().Config()); err != nil {
			return fail(fmt.Errorf("server: AuthKit pool: %w", err))
		}
		akDeps.Postgres = s.pool
	}
	if akDeps.Logger == nil {
		akDeps.Logger = logger
	}
	if akDeps.Redis == nil && s.graph.RedisClient != nil {
		akDeps.Redis = s.graph.RedisClient
	}
	if akDeps.Email == nil && deps.Engine.Email != nil {
		akDeps.Email = controlplane.AuthKitSender{Sender: deps.Engine.Email}
	}
	akDeps.Wrap = registrationGuard(akDeps.Wrap)
	if akDeps.GroupName == nil {
		akDeps.GroupName = s.merchantName
	}
	ak, err := authkit.New(ctx, akCfg, akDeps)
	if err != nil {
		return fail(fmt.Errorf("server: build AuthKit: %w", err))
	}
	if s.cp, err = controlplane.New(logger, ak, s.graph.Config, akCfg, akDeps.Postgres, opts); err != nil {
		_ = ak.Close(context.WithoutCancel(ctx))
		return fail(err)
	}
	if err := operator.Join(s.graph, s.cp); err != nil {
		return fail(err)
	}
	consoleAssets := deps.ConsoleAssets
	if consoleEnabled && consoleAssets == nil {
		consoleAssets = console.FS()
	}
	checkout, page := eng.CheckoutPage()
	var admission func(context.Context, auth.Verified, config.CustomerBinding) error
	if policy := deps.CustomerAdmissionPolicy; policy != nil {
		admission = func(ctx context.Context, v auth.Verified, b config.CustomerBinding) error {
			return policy(ctx, v, openrails.CustomerBinding(b))
		}
	}
	if s.surface, err = operator.StandaloneServer(s.graph, s.cp, operator.Surface{
		RouteGroups: config.RouteGroups(cfg.RouteGroups), ConsoleDisabled: !consoleEnabled, Resource: akCfg.Resource.ID,
		CustomerAdmissionPolicy: admission,
		CheckoutOrigin:          checkout, CheckoutHandler: page, ConsoleAssets: consoleAssets,
	}); err != nil {
		return fail(fmt.Errorf("server: HTTP surface: %w", err))
	}
	if consoleEnabled {
		s.graph.Runtime.AlertService.SetConsoleURL(strings.TrimRight(cfg.Engine.PublicBillingBaseURL, "/") + adminconsole.Path)
	}
	return s, nil
}

// Engine is the engine, as on any host: Engine().Merchant(id) is the handle
// of any live merchant, with every merchant-scoped operation.
func (s *Server) Engine() *openrails.Engine { return s.engine }

// AuthKit is the server's own AuthKit client.
func (s *Server) AuthKit() *authkit.Client { return s.cp.Core() }

// Handler is the whole public surface: health, the engine's routes gated by
// the server's Auth, AuthKit's and the admin console. The operator's
// /metrics is MetricsHandler.
func (s *Server) Handler() http.Handler { return s.surface.Handler() }

// Routes is the standalone surface without the health routes, for a host's
// own root router (the adapters' MountRoutes).
func (s *Server) Routes() ([]openrails.Route, error) {
	table := s.surface.HTTPRoutes()
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	bundle := routebundle.FromTable(table)
	out := make([]openrails.Route, len(bundle))
	for i, r := range bundle {
		out[i] = openrails.Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out, nil
}

// RiverJobs is the server's contribution to a host's River fleet: the
// engine's jobs and AuthKit's. Pass the fleet to Start with
// openrails.WithRiverClient.
func (s *Server) RiverJobs() riverhelpers.Contribution { return s.engine.RiverJobs() }

// Start starts the background work, AuthKit's jobs included, on the engine's
// own River or, with openrails.WithRiverClient, the host's fleet. Close stops
// it.
func (s *Server) Start(ctx context.Context, opts ...openrails.StartOption) error {
	return s.engine.Start(ctx, opts...)
}

// Run serves Handler at Config's Host and Port and runs the background work until ctx
// ends or serving fails, then drains for 5s, finishes in-flight requests and
// closes the server within 20s.
func (s *Server) Run(ctx context.Context) error { return s.run(ctx, true) }

// Serve is Run without the background work, which another process runs.
func (s *Server) Serve(ctx context.Context) error { return s.run(ctx, false) }

func (s *Server) run(ctx context.Context, workers bool) (err error) {
	// One shutdownTimeout bounds the HTTP shutdown and Close together.
	var deadline time.Time
	defer func() {
		if deadline.IsZero() {
			deadline = time.Now().Add(shutdownTimeout)
		}
		closeCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
		err = errors.Join(err, s.Close(closeCtx))
	}()
	// Job producers bind before HTTP accepts work, also when another process
	// runs the workers.
	if err := s.graph.Runtime.InitRiver(ctx); err != nil {
		return fmt.Errorf("server: bind job producers: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.Addr())
	if err != nil {
		return err
	}
	// No request-wide WriteTimeout: a route with a budget owns its deadline
	// (xs-007 row 37). The timeouts bound a peer that is not sending.
	hs := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	served := make(chan error, 2)
	go func() { served <- hs.Serve(ln) }()
	s.log.InfoContext(ctx, "OpenRails serving", "addr", ln.Addr().String())
	var metrics *http.Server
	if s.cfg.MetricsPort != 0 {
		mln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.MetricsPort)))
		if err != nil {
			_ = hs.Close()
			return err
		}
		metrics = &http.Server{Handler: s.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		go func() { served <- metrics.Serve(mln) }()
		s.log.InfoContext(ctx, "OpenRails metrics listener", "addr", mln.Addr().String())
	}
	if workers {
		err = s.Start(ctx)
	}
	if err == nil {
		select {
		case <-ctx.Done():
			err = drain(s.log, s.surface.Drain, drainDelay, served, hs)
		case err = <-served:
		}
	}
	deadline = time.Now().Add(shutdownTimeout)
	shutdown(s.log, deadline, hs, metrics)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// drain marks the process unready and keeps serving for delay, with
// keep-alives off so clients reconnect to another instance. A server failing
// meanwhile ends it early.
func drain(logger *slog.Logger, unready func(), delay time.Duration, served <-chan error, servers ...*http.Server) error {
	if delay <= 0 {
		return nil
	}
	unready()
	for _, hs := range servers {
		hs.SetKeepAlivesEnabled(false)
	}
	logger.Info("server: draining", "delay", delay.String())
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case err := <-served:
		return err
	}
}

// shutdown stops the servers accepting and lets in-flight requests finish
// by deadline, then closes whatever is left.
func shutdown(logger *slog.Logger, deadline time.Time, servers ...*http.Server) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, hs := range servers {
		if hs == nil {
			continue
		}
		if err := hs.Shutdown(ctx); err != nil {
			logger.ErrorContext(ctx, "server: HTTP shutdown; closing the remaining connections", logx.Err(err))
			_ = hs.Close()
		}
	}
}

// Close stops the background work and closes the engine, AuthKit and the
// server's pool. The host's own pool, Redis and Vault stay open.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.engine != nil {
			s.closeErr = s.engine.Close(ctx)
		}
		if s.cp != nil {
			s.cp.Close()
		}
		if s.pool != nil {
			s.pool.Close()
		}
	})
	return s.closeErr
}

// customerDirectory is the engine's directory of customers: each
// merchant's, per issuer, in AuthKit (identity.IssuerLookup).
type customerDirectory struct{ s *Server }

func (d customerDirectory) ForIssuer(mid billing.MerchantID, issuer string) userinfo.Lookup {
	return d.s.cp.Customers(mid, issuer)
}

func (d customerDirectory) Issuers(ctx context.Context, mid billing.MerchantID) ([]string, error) {
	return d.s.cp.TrustedIssuers(ctx, mid)
}

func (customerDirectory) Get(context.Context, []string) (map[string]userinfo.User, error) {
	return nil, errors.New("server: customers are looked up per merchant and issuer")
}

func (customerDirectory) Search(context.Context, string, int) ([]userinfo.User, error) {
	return nil, errors.New("server: customers are searched per merchant and issuer")
}
