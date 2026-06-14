//go:build integration

// Package integrationharness boots REAL OpenRails servers over REAL HTTP for
// integration tests — no stubs. It exposes two surfaces, both implementing the
// same service-token-authenticated /v1/service/* contract, so any integration
// test can drive identical operation scripts against both and assert parity (the
// embed conformance test is the first consumer; #485):
//
//   - EMBEDDED no-auth HOST (Server 1, ≈ doujins minus auth). Builds the engine
//     with embed.New (host-owns-auth) and serves the embedded /v1/service/*
//     surface over httptest with a TRUSTING service-token resolver that accepts
//     every request and pins the bound merchant — it verifies NOTHING (fine for
//     tests). This is "doujins with a no-op authenticator": real HTTP, real
//     engine, no auth checks. Mirrors internal/billing/openrailsembed in the
//     doujins repo (the shape reference).
//
//   - STANDALONE real server + real AuthKit (Server 2, the production path).
//     Boots the actual standalone gin server (internal/bootstrap/ginboot +
//     internal/http, the same graph cmd/billing run-server uses) with the
//     OpenRails-owned AuthKit control plane attached, provisions the merchant via
//     the REAL control-plane bootstrap (links owner_tenant_id + mints a real
//     admin service token through AuthKit core), and authenticates the client
//     with that real token. The /v1/service/* requests are resolved by the real
//     ServiceTokenRequired -> control plane ResolveServiceToken -> AuthKit core
//     path, exercising #481 role-based merchant authz. NO stubResolver.
//
// Both servers run against the SAME shared migrated Postgres (dbtest.RunPostgres
// applies BOTH OpenRails AND AuthKit profiles.* migrations) and a shared Redis
// testcontainer for the admission-throughput axis. Tests scope state by
// per-side payer ids, so the two surfaces never collide.
package integrationharness

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
	authtesting "github.com/open-rails/authkit/testing"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/ginboot"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Harness owns the infrastructure shared by both surfaces (the migrated Postgres
// DSN + a Redis client over a testcontainer). Build one per test with New, then
// start whichever surfaces the test needs. Cleanup is registered on t.
type Harness struct {
	t   *testing.T
	ctx context.Context

	// DSN is the shared, fully-migrated Postgres (OpenRails + AuthKit profiles.*).
	DSN string
	// Redis is a client over a Redis testcontainer (admission throughput axis).
	Redis *redis.Client

	pool *pgxpool.Pool
}

// sharedPool lazily opens a privileged pgx pool over the shared DSN for fixture
// writes (merchant directory row, payer subjects, entitlements).
func (h *Harness) sharedPool() *pgxpool.Pool {
	h.t.Helper()
	if h.pool == nil {
		p, err := pgxpool.New(h.ctx, h.DSN)
		require.NoError(h.t, err, "open shared pgx pool")
		h.pool = p
		h.t.Cleanup(p.Close)
	}
	return h.pool
}

// Pool returns the shared privileged pgx pool for test fixtures.
func (h *Harness) Pool() *pgxpool.Pool { return h.sharedPool() }

// Surface is a single running server: its base URL, the bearer token a client
// must present (empty/trusting for the embedded host — any token works), and a
// ready-to-use openrails.Client (NewRemote over real HTTP).
type Surface struct {
	// Name is "embedded" or "standalone" (for assertion labels).
	Name string
	// BaseURL is the server's HTTP base (no trailing /v1).
	BaseURL string
	// Token is the bearer the remote client presents. For the standalone surface
	// it is a REAL minted service token; for the embedded host it is an arbitrary
	// trusting-resolver-accepted token.
	Token string

	// h/app are the standalone surface's harness + booted app, used by
	// RegisterRemoteApplication to exercise the #484 JWKS-principal credential
	// through the SAME real control plane the server authenticates against. Nil for
	// the embedded host.
	h   *Harness
	app *app.App

	currency string
}

// Client returns a fresh openrails.Client (NewRemote) for this surface, carrying
// its token + currency. opts append/override.
func (s *Surface) Client(opts ...openrails.RemoteOption) openrails.Client {
	base := []openrails.RemoteOption{
		openrails.WithTokenProvider(func(context.Context) (string, error) { return s.Token, nil }),
		openrails.WithCurrency(s.currency),
		openrails.WithTimeout(30 * time.Second),
	}
	return openrails.NewRemote(s.BaseURL, append(base, opts...)...)
}

// New provisions the shared infrastructure: the migrated Postgres DSN (shared
// per package via dbtest) and a Redis testcontainer. Both surfaces started from
// this harness share them.
func New(t *testing.T, ctx context.Context) *Harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dsn := dbtest.SharedPostgresDSN(t)

	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err, "start redis testcontainer")
	t.Cleanup(func() { _ = rc.Terminate(context.Background()) })
	redisURL, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	ropt, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(ropt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	return &Harness{t: t, ctx: ctx, DSN: dsn, Redis: rdb}
}

// trustingResolver is the embedded no-auth host's service-token resolver: it
// accepts EVERY presented token as a merchant-wide PermAdmin credential for the
// bound merchant and verifies NOTHING. This is the "no-op authenticator" half of
// "doujins minus auth" — the host is trusted for its own merchant, exactly as an
// embedding host treats its own in-process engine. It replaces the per-test
// stubResolver the conformance test used to carry.
type trustingResolver struct {
	merchantID   merchant.ID
	merchantSlug string
}

func (trustingResolver) LooksLikeServiceToken(string) bool { return true }

func (r trustingResolver) ResolveServiceToken(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	return &controlplane.ResolvedServiceToken{
		OwnerTenantSlug: "embedded-host",
		MerchantID:      r.merchantID,
		MerchantSlug:    r.merchantSlug,
		Permissions:     []string{controlplane.PermAdmin},
		Resources:       []authcore.ServiceTokenResource{controlplane.MerchantResource(r.merchantID)},
	}, nil
}

// StartEmbeddedHost boots Server 1: an embedded engine (embed.New, host-owns-auth)
// over the shared Postgres + Redis, serving the real embedded /v1/service/*
// surface over httptest behind the REAL ServiceTokenRequired middleware wired to
// a TRUSTING resolver (no auth). It registers the bound merchant (embed.New does
// this) and returns a Surface whose client speaks real HTTP. currency is the
// client's default currency (Balance keys off it).
func (h *Harness) StartEmbeddedHost(currency string) *Surface {
	h.t.Helper()

	dbtest.EnsureTestTenant(h.ctx, h.t, h.sharedPool())

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: h.DSN}}
	rt, err := embed.New(h.ctx, embed.Options{
		Tenant:  dbtest.TestTenantSlug,
		Options: embedded.Options{Config: cfg, Redis: h.Redis},
	})
	require.NoError(h.t, err, "embed.New")
	h.t.Cleanup(func() { _ = rt.Close(context.Background()) })

	router := gin.New()
	router.Use(ginmw.ResolveTenant(dbtest.TestTenantID))
	ginroutes.RegisterServiceRoutes(
		router.Group("/v1/service"),
		rt.Embedded().App().Runtime,
		ginmw.ServiceTokenRequired(trustingResolver{
			merchantID:   dbtest.TestTenantID,
			merchantSlug: dbtest.TestTenantSlug,
		}),
	)
	srv := httptest.NewServer(router)
	h.t.Cleanup(srv.Close)

	return &Surface{
		Name:     "embedded",
		BaseURL:  srv.URL,
		Token:    "embedded-host-trusting-token", // any token works (trusting resolver)
		currency: currency,
	}
}

// StartStandalone boots Server 2: the REAL standalone gin server (ginboot.NewServer
// -> internal/http, the cmd/billing run-server graph) over the shared Postgres +
// Redis with the OpenRails control plane attached, then provisions the merchant
// through the REAL control-plane bootstrap (links owner_tenant_id + mints a real
// admin service token via AuthKit core) and returns a Surface whose client
// authenticates with that real token. The /v1/service/* path is authenticated by
// the real ServiceTokenRequired -> ResolveServiceToken -> AuthKit core chain
// (#481 role-based merchant authz). No stubs.
func (h *Harness) StartStandalone(currency string) *Surface {
	h.t.Helper()

	// The merchant directory row must exist before bootstrap links owner_tenant_id
	// onto it (#480: no default merchant).
	dbtest.EnsureTestTenant(h.ctx, h.t, h.sharedPool())

	// The standalone server builds its first-party JWT verifier from auth.issuers
	// for the user/admin surface (server.New requires an authenticator). The
	// /v1/service/* surface the conformance script drives uses SERVICE TOKENS, not
	// this verifier, but a real standalone boot always wires it — so we stand up a
	// real AuthKit JWKS test issuer, exactly like the integration suite does.
	issuer := authtesting.NewTestIssuerWithAudience("openrails-app")
	h.t.Cleanup(issuer.Close)

	cfg := &config.Config{
		Env:     "dev",
		TestEnv: true,
		Host:    "127.0.0.1",
		Port:    0, // ephemeral; we serve via httptest below
		Tenant:  dbtest.TestTenantSlug,
		DB:      &config.DBConfig{URL: h.DSN},
		Auth: &config.AuthConfig{
			Issuers:          []string{issuer.URL()},
			ExpectedAudience: "openrails-app",
			// The control plane's own AuthKit issuer + service-token brand prefix;
			// service-token resolution needs only these.
			ControlPlane: &config.ControlPlaneConfig{
				Issuer:      "https://controlplane.openrails.test",
				TokenPrefix: "openrails",
			},
		},
	}
	if h.Redis != nil {
		cfg.Redis = &config.RedisConfig{Addr: h.Redis.Options().Addr}
	}

	assembled, err := ginboot.NewServer(cfg, &bootstrap.Options{})
	require.NoError(h.t, err, "ginboot.NewServer (real standalone)")
	app := assembled.App
	h.t.Cleanup(func() { _ = app.Close(context.Background()) })

	// Real control-plane bootstrap: ensures the operator AuthKit org + role, links
	// the merchant's owner_tenant_id to it, and mints a REAL admin service token
	// scoped to the merchant — the production provisioning path (cmd/billing
	// run-server does the same at boot). MintInitialServiceToken returns the
	// one-time secret we hand to the client.
	res, err := embcp.RunBootstrap(h.ctx, app, controlplane.BootstrapOptions{
		BootstrapTenantSlug:     dbtest.TestTenantSlug,
		MintInitialServiceToken: true,
	})
	require.NoError(h.t, err, "control plane bootstrap")
	require.NotNil(h.t, res)
	token := res.ServiceTokenSecret
	if token == "" {
		// A previous run already minted one (shared DB across the package): mint a
		// fresh real token directly through AuthKit core, scoped to the merchant.
		token = h.mintFreshServiceToken(app)
	}
	require.NotEmpty(h.t, token, "standalone service token")
	// Sanity: the real resolver must accept the real token end-to-end (#481 path).
	cp := embcp.Get(app)
	require.NotNil(h.t, cp, "control plane attached")
	resolved, rerr := cp.ResolveServiceToken(h.ctx, token)
	require.NoError(h.t, rerr, "real service token must resolve through AuthKit core")
	require.Equal(h.t, dbtest.TestTenantID, resolved.MerchantID)

	// Workers: the conformance script does not need background workers, but River
	// must be initialised for the runtime to be healthy. The standalone server
	// graph does not start workers itself here; the service routes used by the
	// script (credits/admit/windows) are synchronous, so we serve without workers.
	srv := httptest.NewServer(assembled.Server.Handler())
	h.t.Cleanup(srv.Close)

	return &Surface{
		Name:     "standalone",
		BaseURL:  srv.URL,
		Token:    token,
		h:        h,
		app:      app,
		currency: currency,
	}
}

// RemoteAppCaller is a JWKS principal (remote_application, #76/#484) provisioned
// against the standalone surface's real control plane: its registered slug/issuer
// and a freshly minted remote-application-access+jwt SELF-token. Present it as a
// Bearer credential to the /v1/service/* surface to drive the #484 path.
type RemoteAppCaller struct {
	Slug   string
	Issuer string
	// Token is a minted self-token (typ=remote-application-access+jwt) signed by
	// the principal's own key. Authority is STORED (assigned role/perms), never
	// self-claimed.
	Token string
}

// RegisterRemoteApplication provisions a JWKS principal (#484) on the standalone
// surface's REAL control plane and returns a minted self-token. It stands up a
// test JWKS issuer for the principal, registers the remote_application (jwks
// mode), assigns it role (when non-empty) on ownerTenantSlug — the tenant that
// owns the test merchant — and signs a self-token with the principal's own key.
//
// With OperatorRole on the merchant's owner_tenant, the principal can administer
// the merchant via the existing #481 role-based authz. Pass role="" to provision
// a principal with NO authority (the deny case).
func (s *Surface) RegisterRemoteApplication(slug, ownerTenantSlug, role string) RemoteAppCaller {
	h := s.h
	require.NotNil(h.t, s.app, "RegisterRemoteApplication requires the standalone surface")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(h.t, core, "authkit core")

	// The principal's own signing key + JWKS endpoint (its credential, #74 jwks mode).
	issuer := authtesting.NewTestIssuerWithAudience("openrails-app")
	h.t.Cleanup(issuer.Close)

	// #77: a remote_application is owned by exactly one org (org_id NOT NULL).
	ownerOrg, err := core.ResolveOrgBySlug(h.ctx, ownerTenantSlug)
	require.NoError(h.t, err, "resolve owner org")
	ra, err := core.UpsertRemoteApplication(h.ctx, authcore.RemoteApplication{
		Slug:      slug,
		OrgID:     ownerOrg.ID,
		Issuer:    issuer.URL(),
		JWKSURI:   issuer.URL() + "/.well-known/jwks.json",
		Mode:      authcore.RemoteAppModeJWKS,
		Audiences: []string{"openrails-app"},
		Enabled:   true,
	})
	require.NoError(h.t, err, "register remote_application")

	if role != "" {
		require.NoError(h.t, core.AddRemoteApplicationMember(h.ctx, ownerTenantSlug, ra.ID, role),
			"assign role on owner tenant")
	}

	// Pick up the new issuer in the verifier's in-memory registry (it also
	// lazy-loads on first use, but reload makes the test deterministic).
	require.NoError(h.t, cp.ReloadRemoteApplications(h.ctx), "reload remote_applications")

	token, err := authcore.MintRemoteApplicationAccessToken(h.ctx, issuer.Signer(), authcore.RemoteApplicationAccessParams{
		Issuer:    issuer.URL(),
		Audiences: []string{"openrails-app"},
		TTL:       time.Hour,
	})
	require.NoError(h.t, err, "mint remote_application self-token")

	return RemoteAppCaller{Slug: slug, Issuer: issuer.URL(), Token: token}
}

// mintFreshServiceToken mints a new real admin service token through AuthKit core,
// scoped to the test merchant — used when bootstrap found an existing token (the
// shared-DB second-run case) and returned no secret.
func (h *Harness) mintFreshServiceToken(a *app.App) string {
	h.t.Helper()
	cp := embcp.Get(a)
	require.NotNil(h.t, cp, "control plane attached")
	_, secret, err := cp.Core().MintServiceTokenWithOptions(h.ctx, dbtest.TestTenantSlug, authcore.ServiceTokenMintOptions{
		Name:        "integrationharness-extra",
		Permissions: controlplane.OperatorRolePermissions(),
		Resources:   []authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestTenantID)},
	})
	require.NoError(h.t, err, "mint fresh service token")
	return secret
}
