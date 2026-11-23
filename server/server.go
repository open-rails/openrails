// Package server is standalone OpenRails: the engine (openrails.New)
// composed, like any host of the library, with its own AuthKit and the
// multi-merchant control plane, serving the standalone HTTP surface. The
// openrails binary (cmd/openrails) and hosted products build on it; a host
// embedding OpenRails for one merchant uses package openrails alone.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
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
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	httpserver "github.com/open-rails/openrails/server/internal/http"
	"github.com/open-rails/openrails/server/internal/operator"
	admin "github.com/open-rails/openrails/server/web/console"
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

// AdminConsole is Config.AdminConsole: the console at /admin and its app
// sign-in.
type AdminConsole struct {
	// Disabled omits the console's pages, assets and bootstrap endpoints.
	// The default keeps the console available; billing and auth APIs are
	// separate.
	Disabled bool
	// Issuers are keyed by a merchant id or name, or "root" for operators.
	Issuers map[string]ConsoleIssuer
}

// ConsoleIssuer is an authorization server the console signs staff in at,
// as an OAuth 2.0 public client (code flow with PKCE).
type ConsoleIssuer struct {
	// URL is the issuer: one of AuthKit's trusted issuers.
	URL string
	// ClientID is the console's public client there, registered with the
	// console path plus /callback as a redirect URI.
	ClientID string
	// Name is shown on the sign-in button; empty is the trusted issuer's.
	Name string
	// Scope is what the console asks for; empty is "openid profile email
	// openrails:merchant". An issuer that grants refresh tokens only for
	// offline_access needs it added.
	Scope string
}

// NamingConfig is Config.Naming: the rename policy for merchant names,
// whether a name may change, how often, and how long a former name keeps
// forwarding. Unset fields take the defaults: renames on, every 72 hours,
// former names kept 90 days.
type NamingConfig struct {
	Enabled        *bool
	RenameInterval *time.Duration
	FormerNames    FormerNamesConfig
}

// FormerNamesConfig is NamingConfig.FormerNames: how long a former name
// keeps forwarding. Duration applies to FormerNamesFinite only.
type FormerNamesConfig struct {
	Mode     FormerNamesMode
	Duration *time.Duration
}

// FormerNamesMode is FormerNamesConfig.Mode.
type FormerNamesMode string

const (
	// FormerNamesFinite forwards a former name for FormerNamesConfig.Duration.
	FormerNamesFinite FormerNamesMode = "finite"
	// FormerNamesForever forwards a former name indefinitely.
	FormerNamesForever FormerNamesMode = "forever"
	// FormerNamesImmediate releases a former name at once.
	FormerNamesImmediate FormerNamesMode = "immediate"
)

// namingConfig is c as the control plane reads it.
func namingConfig(c NamingConfig) config.NamingConfig {
	return config.NamingConfig{Enabled: c.Enabled, RenameInterval: c.RenameInterval,
		FormerNames: config.FormerNamesConfig{Mode: config.FormerNamesMode(c.FormerNames.Mode), Duration: c.FormerNames.Duration}}
}

// consoleConfig is c as the HTTP surface reads it.
func consoleConfig(c *AdminConsole) *hostconfig.ConsoleConfig {
	if c == nil {
		return nil
	}
	out := &hostconfig.ConsoleConfig{Disabled: c.Disabled}
	if c.Issuers != nil {
		out.Issuers = make(map[string]hostconfig.ConsoleIssuer, len(c.Issuers))
		for key, issuer := range c.Issuers {
			out.Issuers[key] = hostconfig.ConsoleIssuer(issuer)
		}
	}
	return out
}

// Config is a standalone deployment: plain data.
type Config struct {
	// Engine is the engine's configuration, as any host passes to
	// openrails.New. The server's merchants are provisioned at run time, each
	// with its AuthKit group, and manage their catalogs through the API or
	// openrails catalog apply: a declared merchant's Issuer is refused. Its
	// CheckoutOrigin is served by Host, and must also be apart from AuthKit's
	// issuer and public URLs.
	Engine openrails.Config

	// Auth is the server's AuthKit, as AuthKit documents its configuration:
	// accounts, sign-in and the tokens it issues (Auth.SignIn.DPoP), the
	// resource server (Auth.Resource; its ID defaults to
	// Engine.PublicBillingBaseURL) and its trusted issuers. It decides who
	// every request is; the server only maps merchants to AuthKit groups.
	// Auth.Token.Issuer and Auth.DefaultTenantID are required. DefaultTenantID
	// identifies the persisted platform/operator account directory, not a
	// merchant's customer tenant. The server sets what is the product's:
	// the merchant roles, the API key prefix, the issued audience, the
	// resource's scopes, the enabled console's OAuth client (openrails-console),
	// a 15-minute access-token lifetime unless
	// Auth.AuthorizationServer.AccessTokenTTL sets one, AuthKit's River schema
	// (the engine's) and its HTTP mount. An empty client-IP posture takes the
	// engine's TrustedProxies. Hosted account policy belongs to the host.
	Auth authkit.Config
	// LocalSignIn serves sign-in to the server's own accounts (password,
	// passwordless, registration). Off, the default, people sign in at a
	// trusted issuer and AuthKit serves only its JWKS.
	LocalSignIn bool
	// Naming is the rename policy for merchant names; AuthKit's own
	// (Auth.Username) governs usernames.
	Naming NamingConfig
	// MerchantCreation is the policy for the merchants users create and
	// rename through a hosted product (ProvisionMerchant with an owner,
	// RenameUserMerchant); nil for operator-provisioned
	// deployments. Hosted products with LocalSignIn may explicitly enable
	// open Auth.Registration; contact verification still defaults to required.
	MerchantCreation *MerchantCreationConfig

	// RouteGroups turns the merchant route groups on, each off by default:
	// Admin, Catalog, MerchantConfig and Metrics, and Programmatic, each
	// route behind the server's own merchant permission (the Merchant*
	// permissions).
	RouteGroups openrails.RouteGroups
	// AdminConsole controls the console at /admin and declares app sign-in
	// methods. Nil keeps it on; Disabled omits it. LocalSignIn is independent.
	// Alert links point at it.
	AdminConsole *AdminConsole

	// Addr is where Run and Serve listen; empty is ":3053".
	Addr string
	// PrivateAddr is where Run and Serve also serve PrivateHandler, the
	// operator's /metrics, apart from the public surface. Empty serves none.
	PrivateAddr string
	// DrainDelay is how long Run and Serve keep serving once ctx ends, with
	// /health/ready failing, so load balancers stop routing here before the
	// listener closes. 0 closes it at once.
	DrainDelay time.Duration
	// ShutdownTimeout bounds finishing in-flight requests and closing the
	// server, background work included, after the drain; 0 is 30s.
	ShutdownTimeout time.Duration
}

// Deps is everything a standalone server reaches outside its process.
type Deps struct {
	// Engine is what openrails.New takes. AuthKit's tables live in the same
	// database. Nil CheckoutAssets serves the payment page build embedded in
	// this module, when the binary was built with one.
	Engine openrails.Deps
	// Auth is what the server's AuthKit reaches: its email and SMS senders,
	// its signing key source and the rest of authkit.Deps. Nil Postgres is a
	// pool of its own on the engine's database, nil Redis the engine's, and
	// nil Email renders AuthKit's email and sends it through the engine's
	// sender.
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
	if err := controlplane.RequireDefaultTenant(cfg.Auth); err != nil {
		return nil, err
	}
	for _, m := range cfg.Engine.Merchants {
		if m.Issuer != "" {
			return nil, fmt.Errorf("server: Engine.Merchants %q's Issuer binds an embedded host's own issuer to its merchant; a standalone server's credentials are bound to a merchant by its AuthKit group", m.Slug)
		}
	}
	if err := checkCheckoutOrigin(cfg); err != nil {
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

	consoleEnabled := cfg.AdminConsole == nil || !cfg.AdminConsole.Disabled
	opts := controlplane.Options{LocalSignIn: cfg.LocalSignIn, Naming: namingConfig(cfg.Naming)}
	if cfg.LocalSignIn && consoleEnabled {
		opts.ConsoleClientID = ConsoleClientID
	}
	if c := cfg.MerchantCreation; c != nil {
		policy := controlplane.MerchantCreationConfig{ReservedSlugs: c.ReservedSlugs, ReservedEscalationRole: c.ReservedEscalationRole, SlugPattern: c.SlugPattern}
		if c.FreeAllowance > 0 {
			admission, err := operator.MerchantCreationAdmission(logger, func() *controlplane.ControlPlane { return s.cp }, operator.MerchantCreationPolicy{
				FreeAllowance: c.FreeAllowance, HasVaultedPaymentMethod: deps.HasVaultedPaymentMethod,
			})
			if err != nil {
				return fail(err)
			}
			policy.Admission = admission
		}
		opts.MerchantCreation = &policy
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
	if akDeps.Redis == nil && s.graph.RedisClient != nil {
		akDeps.Redis = s.graph.RedisClient
	}
	if akDeps.Email == nil && deps.Engine.Email != nil {
		akDeps.Email = controlplane.AuthKitSender{Sender: deps.Engine.Email}
	}
	akCfg, err := controlplane.AuthKit(s.graph.Config, cfg.Auth, opts)
	if err != nil {
		return fail(err)
	}
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
		consoleAssets = admin.FS()
	}
	checkout, page := eng.CheckoutPage()
	var admission func(context.Context, auth.Verified, config.CustomerBinding) error
	if policy := deps.CustomerAdmissionPolicy; policy != nil {
		admission = func(ctx context.Context, v auth.Verified, b config.CustomerBinding) error {
			return policy(ctx, v, openrails.CustomerBinding(b))
		}
	}
	if s.surface, err = operator.StandaloneServer(s.graph, s.cp, operator.Surface{
		RouteGroups: config.RouteGroups(cfg.RouteGroups), AdminConsole: consoleConfig(cfg.AdminConsole), Resource: akCfg.Resource.ID,
		CustomerAdmissionPolicy: admission,
		CheckoutOrigin:          checkout, CheckoutHandler: page, ConsoleAssets: consoleAssets,
	}); err != nil {
		return fail(fmt.Errorf("server: HTTP surface: %w", err))
	}
	if consoleEnabled {
		s.graph.Runtime.AlertService.SetConsoleURL(strings.TrimRight(cfg.Engine.PublicBillingBaseURL, "/") + hostconfig.ConsolePath)
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
// /metrics is PrivateHandler.
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

// Run serves Handler at Config.Addr and runs the background work until ctx
// ends or serving fails, then drains (Config.DrainDelay), finishes in-flight
// requests and closes the server.
func (s *Server) Run(ctx context.Context) error { return s.run(ctx, true) }

// Serve is Run without the background work, which another process runs.
func (s *Server) Serve(ctx context.Context) error { return s.run(ctx, false) }

// shutdownTimeout is Config.ShutdownTimeout, defaulted.
func (s *Server) shutdownTimeout() time.Duration {
	if s.cfg.ShutdownTimeout > 0 {
		return s.cfg.ShutdownTimeout
	}
	return 30 * time.Second
}

func (s *Server) run(ctx context.Context, workers bool) (err error) {
	// One ShutdownTimeout bounds the HTTP shutdown and Close together.
	var deadline time.Time
	defer func() {
		if deadline.IsZero() {
			deadline = time.Now().Add(s.shutdownTimeout())
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
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":3053"
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
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
	var private *http.Server
	if s.cfg.PrivateAddr != "" {
		pln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.PrivateAddr)
		if err != nil {
			_ = hs.Close()
			return err
		}
		private = &http.Server{Handler: s.PrivateHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		go func() { served <- private.Serve(pln) }()
		s.log.InfoContext(ctx, "OpenRails private listener (metrics)", "addr", pln.Addr().String())
	}
	if workers {
		err = s.Start(ctx)
	}
	if err == nil {
		select {
		case <-ctx.Done():
			err = drain(s.log, s.surface.Drain, s.cfg.DrainDelay, served, hs)
		case err = <-served:
		}
	}
	deadline = time.Now().Add(s.shutdownTimeout())
	shutdown(s.log, deadline, hs, private)
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
