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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	riverhelpers "github.com/open-rails/helpers/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostconfig"
	httpserver "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/operator"
)

// The configuration family of a standalone server, defined in internal/config
// and named here.
type (
	// AuthConfig is Config.Auth: the server's AuthKit issuer, signing keys,
	// naming and development allowances.
	AuthConfig = hostconfig.AuthConfig
	// AuthRateLimit is one AuthKit rate-limit bucket of Config.AuthRateLimits.
	AuthRateLimit = hostconfig.AuthRateLimit
	// MerchantCreationConfig is Config.MerchantCreation: the policy for
	// merchant names users claim.
	MerchantCreationConfig = hostconfig.MerchantCreationConfig
	// ResourceServerConfig is Config.ResourceServer: the authorization servers
	// whose access tokens the merchant API accepts.
	ResourceServerConfig = hostconfig.ResourceServerConfig
	// TrustedIssuerConfig is one of ResourceServerConfig.TrustedIssuers.
	TrustedIssuerConfig = hostconfig.TrustedIssuerConfig
	// ConsoleIssuer is Config.ConsoleIssuer: the trusted issuer staff sign in
	// to the admin console at.
	ConsoleIssuer = hostconfig.ConsoleIssuer
	// NamingConfig is AuthConfig.Naming: the rename policy for merchant names
	// and usernames.
	NamingConfig = config.NamingConfig
	// FormerNamesConfig is NamingConfig.FormerNames: how long a former name
	// keeps forwarding.
	FormerNamesConfig = config.FormerNamesConfig
	// FormerNamesMode is FormerNamesConfig.Mode.
	FormerNamesMode = config.FormerNamesMode
)

const (
	// FormerNamesFinite forwards a former name for FormerNamesConfig.Duration.
	FormerNamesFinite = config.FormerNamesFinite
	// FormerNamesForever forwards a former name indefinitely.
	FormerNamesForever = config.FormerNamesForever
	// FormerNamesImmediate releases a former name at once.
	FormerNamesImmediate = config.FormerNamesImmediate
)

// Config is a standalone deployment: plain data.
type Config struct {
	// Engine is the engine's configuration, as any host passes to
	// openrails.New. The server's merchants manage their catalogs through the
	// API, so Engine.Catalog is refused.
	Engine openrails.Config

	// Auth is the server's own AuthKit. Auth.Issuer is required.
	Auth AuthConfig
	// Registration is AuthKit's self-registration mode: open, invite_only or
	// closed. Empty is closed, so a self-hosted server registers nobody. Open
	// and invite-only need LocalSignIn and an email or SMS sender.
	Registration iam.RegistrationMode
	// LocalSignIn serves sign-in to the server's own accounts (password,
	// passwordless, registration). Off, the default, people sign in at a
	// trusted issuer (ResourceServer) and AuthKit serves only its JWKS.
	LocalSignIn bool
	// PasswordlessLogin exposes contact-based passwordless sign-in;
	// PasswordlessAutoRegistration also creates a no-password user for a
	// verified unknown contact (open registration).
	PasswordlessLogin            bool
	PasswordlessAutoRegistration bool
	// FrontendBaseURL is where emailed links point. Empty is Auth.Issuer,
	// which for a hosted product serves no pages.
	FrontendBaseURL string
	// TrustedProxies and CloudflareProxies override Engine's for AuthKit's
	// client-IP resolver; only CloudflareProxies may assert CF-Connecting-IP.
	TrustedProxies    []string
	CloudflareProxies []string
	// AuthRateLimits overlays AuthKit's default rate-limit buckets by name.
	AuthRateLimits map[string]AuthRateLimit
	// MerchantCreation lets signed-in users create merchants under this
	// policy; nil for operator-provisioned deployments.
	MerchantCreation *MerchantCreationConfig
	// ResourceServer accepts RFC 9068 access tokens (at+jwt) that trusted
	// issuers mint for this deployment, so their users reach the merchant API
	// without the server holding their accounts. Nil accepts none.
	ResourceServer *ResourceServerConfig

	// AdminConsole serves the merchant admin console; nil serves none. Its
	// AuthBaseURL defaults to this server's AuthKit with LocalSignIn.
	AdminConsole *openrails.AdminConsole
	// ConsoleIssuer signs staff in to the console at one of ResourceServer's
	// trusted issuers instead of the server's own accounts.
	ConsoleIssuer *ConsoleIssuer

	// Addr is where Run and Serve listen; empty is ":3053".
	Addr string
}

// Deps is everything a standalone server reaches outside its process.
type Deps struct {
	// Engine is what openrails.New takes. AuthKit's tables live in the same
	// database; the server opens its own pool to it for authority reads.
	Engine openrails.Deps
	// SMS delivers AuthKit's text messages.
	SMS authkit.SMSSender
	// AuthEmail delivers AuthKit's email with the host's own templates. Nil
	// renders it and sends it through the engine's sender (Engine.Email or
	// SendGrid).
	AuthEmail authkit.EmailSender
	// HasVaultedPaymentMethod answers whether a user has a payment method on
	// file, unlocking merchant creation beyond MerchantCreation.FreeAllowance.
	HasVaultedPaymentMethod func(ctx context.Context, userID string) (bool, error)
}

// Server is a standalone OpenRails server.
type Server struct {
	cfg     Config
	client  *openrails.Client
	graph   *app.App
	pool    *pgxpool.Pool
	cp      *controlplane.ControlPlane
	surface *httpserver.Server

	closeOnce sync.Once
	closeErr  error
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
func New(ctx context.Context, cfg Config, deps Deps) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Engine.Catalog != nil {
		return nil, errors.New("server: Engine.Catalog declares one embedded merchant's catalog; a standalone server's merchants manage theirs through the API")
	}
	client, err := openrails.New(ctx, cfg.Engine, deps.Engine)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, client: client, graph: engine.Graph(client)}
	fail := func(err error) (*Server, error) {
		_ = s.Close(context.WithoutCancel(ctx))
		return nil, err
	}

	opts := operator.Options{
		Auth: cfg.Auth, Registration: cfg.Registration, ResourceServer: cfg.ResourceServer,
		PasswordlessLogin: cfg.PasswordlessLogin, PasswordlessAutoRegistration: cfg.PasswordlessAutoRegistration,
		LocalSignIn: cfg.LocalSignIn, EmailSender: deps.AuthEmail, SMSSender: deps.SMS,
		Frontend:       authkit.FrontendConfig{BaseURL: cfg.FrontendBaseURL},
		TrustedProxies: cfg.TrustedProxies, CloudflareProxies: cfg.CloudflareProxies,
		Redis: s.graph.RedisClient,
	}
	if opts.EmailSender == nil && s.graph.Runtime.EmailSender != nil {
		opts.EmailSender = controlplane.AuthKitSender{Sender: s.graph.Runtime.EmailSender}
	}
	if len(cfg.AuthRateLimits) > 0 {
		opts.AuthRateLimitOverrides = make(map[string]authkit.RateLimit, len(cfg.AuthRateLimits))
		for name, l := range cfg.AuthRateLimits {
			opts.AuthRateLimitOverrides[name] = authkit.RateLimit{Limit: l.Limit, Window: l.Window, Cooldown: l.Cooldown}
		}
	}
	if c := cfg.MerchantCreation; c != nil {
		policy := controlplane.MerchantCreationConfig{ReservedSlugs: c.ReservedSlugs, ReservedEscalationRole: c.ReservedEscalationRole, SlugPattern: c.SlugPattern}
		if c.FreeAllowance > 0 {
			admission, err := operator.MerchantCreationAdmission(func() *controlplane.ControlPlane { return s.cp }, operator.MerchantCreationPolicy{
				FreeAllowance: c.FreeAllowance, HasVaultedPaymentMethod: deps.HasVaultedPaymentMethod,
			})
			if err != nil {
				return fail(err)
			}
			policy.Admission = admission
		}
		opts.MerchantCreation = &policy
	}
	cpOpts, err := operator.ControlPlaneOptions(opts)
	if err != nil {
		return fail(err)
	}

	// AuthKit's own pool: an authority read can follow a pinned billing read,
	// so a one-connection engine pool must not also serve it.
	if s.pool, err = pgxpool.NewWithConfig(ctx, s.graph.Runtime.DB.Pool().Config()); err != nil {
		return fail(fmt.Errorf("server: AuthKit pool: %w", err))
	}
	akCfg, akDeps, err := controlplane.AuthKit(s.graph.Config, &cfg.Auth, s.pool, cpOpts...)
	if err != nil {
		return fail(err)
	}
	ak, err := authkit.New(ctx, akCfg, akDeps)
	if err != nil {
		return fail(fmt.Errorf("server: build AuthKit (declare auth.mint_disabled=true if verify-only is intentional, #748): %w", err))
	}
	if s.cp, err = controlplane.New(ak, s.graph.Config, &cfg.Auth, s.pool, cpOpts...); err != nil {
		_ = ak.Close(context.WithoutCancel(ctx))
		return fail(err)
	}
	if err := operator.Join(s.graph, s.cp, cfg.Auth, cfg.FrontendBaseURL); err != nil {
		return fail(err)
	}
	if s.surface, err = operator.StandaloneServer(s.graph, s.cp, operator.Surface{
		AdminConsole: cfg.AdminConsole, ConsoleIssuer: cfg.ConsoleIssuer,
		ResourceServer: cfg.ResourceServer, Issuer: cfg.Auth.Issuer,
	}); err != nil {
		return fail(fmt.Errorf("server: HTTP surface: %w", err))
	}
	return s, nil
}

// Client is the engine: the merchant Go API, as on any host.
func (s *Server) Client() *openrails.Client { return s.client }

// AuthKit is the server's own AuthKit client.
func (s *Server) AuthKit() *authkit.Client { return s.cp.Core() }

// Handler is the whole standalone surface: health, the engine's routes gated
// by the server's Auth, the control plane's routes, AuthKit's and the admin
// console.
func (s *Server) Handler() http.Handler { return s.surface.Handler() }

// Routes is the standalone surface without the health routes, for a host's
// own root router (the adapters' MountRoutes), plus further customer
// surfaces, each with its own Auth. A surface without a Merchant serves the
// merchant each request selects (the OpenRails-Merchant header or the
// merchant's API host), bound before its Auth runs (openrails.RequestMerchant);
// a request that selects none is refused merchant_unresolved.
func (s *Server) Routes(profiles ...openrails.CustomerRoutes) ([]openrails.Route, error) {
	a := s.graph
	extra, err := embedhttp.BuildCustomerRoutes(a, profiles, s.cp.ResolveMerchantByHost)
	if err != nil {
		return nil, err
	}
	router.ResolveMerchantSelectors(extra, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
	}, embedhttp.CustomerPrefixes("", profiles)...)
	table := s.surface.HTTPRoutes()
	table.Entries = append(table.Entries, extra.Entries...)
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
func (s *Server) RiverJobs() riverhelpers.Contribution { return s.client.RiverJobs() }

// Start starts the background work, AuthKit's jobs included, on the engine's
// own River or, with openrails.WithRiverClient, the host's fleet. Close stops
// it.
func (s *Server) Start(ctx context.Context, opts ...openrails.StartOption) error {
	return s.client.Start(ctx, opts...)
}

// Run serves Handler at Config.Addr and runs the background work until ctx
// ends or serving fails, then drains requests and closes the server.
func (s *Server) Run(ctx context.Context) error { return s.run(ctx, true) }

// Serve is Run without the background work, which another process runs.
func (s *Server) Serve(ctx context.Context) error { return s.run(ctx, false) }

// shutdownGrace bounds draining requests and stopping workers.
const shutdownGrace = 30 * time.Second

func (s *Server) run(ctx context.Context, workers bool) (err error) {
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
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
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	log.Infof("OpenRails serving on %s", ln.Addr())
	if workers {
		err = s.Start(ctx)
	}
	if err == nil {
		select {
		case <-ctx.Done():
		case err = <-served:
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if serr := hs.Shutdown(shutdownCtx); serr != nil {
		log.WithError(serr).Error("server: HTTP shutdown")
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// Close stops the background work and closes the engine, AuthKit and the
// server's pool. The host's own pool, Redis and Vault stay open.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.client != nil {
			s.closeErr = s.client.Close(ctx)
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

// identity is a control-plane user session as an openrails.Identity.
func (s *Server) identity(userID, sessionID, email, username string, verified bool) (openrails.Identity, error) {
	if _, err := billing.ParseCustomerID(userID); err != nil {
		return openrails.Identity{}, openrails.ErrUnauthenticated
	}
	issuer := s.cfg.Auth.Issuer
	return openrails.Identity{
		Issuer: issuer, Subject: userID, SubjectKind: openrails.SubjectUser,
		Invoker:    openrails.Invoker{Issuer: issuer, ID: userID},
		Credential: openrails.Credential{Kind: openrails.CredentialSession, ID: sessionID},
		Email:      email, EmailVerified: verified, Username: username,
	}, nil
}
