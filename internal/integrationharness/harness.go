//go:build integration

// Package integrationharness boots REAL OpenRails servers over REAL HTTP for
// integration tests — no stubs. It exposes two surfaces, both implementing the
// same service-credential-authenticated /v1/merchant/* contract, so any integration
// test can drive identical operation scripts against both and assert parity (the
// embed conformance test is the first consumer; #485):
//
//   - EMBEDDED no-auth HOST (Server 1, ≈ doujins minus auth). Builds the engine
//     with embed.New (host-owns-auth) and serves the embedded /v1/merchant/*
//     surface over httptest with a TRUSTING API-key resolver that accepts
//     every request and pins the bound merchant — it verifies NOTHING (fine for
//     tests). This is "doujins with a no-op authenticator": real HTTP, real
//     engine, no auth checks. Mirrors internal/billing/openrailsembed in the
//     doujins repo (the shape reference).
//
//   - STANDALONE real server + real AuthKit (Server 2, the production path).
//     Boots the actual standalone gin server (internal/bootstrap/ginboot +
//     internal/http, the same graph cmd/openrails run-server uses) with the
//     OpenRails-owned AuthKit control plane attached, provisions the merchant via
//     the REAL control-plane bootstrap (links permission_group_id + mints a real
//     admin API key through AuthKit core), and authenticates the client
//     with that real token. The /v1/merchant/* requests are resolved by the real
//     ServiceCredentialRequired -> control plane ResolveAPIKey -> AuthKit core
//     path, exercising #481 role-based merchant authz. NO stubResolver.
//
// Both servers run against the SAME shared migrated Postgres (dbtest.RunPostgres
// applies BOTH OpenRails AND AuthKit profiles.* migrations) and a shared Redis
// for the admission-throughput axis. Tests scope state by
// per-side payer ids, so the two surfaces never collide.
package integrationharness

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authtesting "github.com/open-rails/authkit/authtest"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwt"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/ginboot"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginrouter "github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Harness owns the infrastructure shared by both surfaces (the migrated Postgres
// DSN + a Redis client). Build one per test with New, then
// start whichever surfaces the test needs. Cleanup is registered on t.
type Harness struct {
	t   *testing.T
	ctx context.Context

	// DSN is the shared, fully-migrated Postgres (OpenRails + AuthKit profiles.*).
	DSN string
	// Redis is a client over shared Redis (admission throughput axis).
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
	// it is a REAL minted API key; for the embedded host it is an arbitrary
	// trusting-resolver-accepted token.
	Token string

	// h/app are the standalone surface's harness + booted app, used by
	// RegisterRemoteApplication to exercise the #484 remote application access
	// token through the SAME real control plane the server authenticates against.
	// Nil for the embedded host.
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

	rdb, _ := dbtest.SharedRedisClient(t)

	return &Harness{t: t, ctx: ctx, DSN: dsn, Redis: rdb}
}

// trustingResolver is the embedded no-auth host's API-key resolver: it
// accepts EVERY presented token as a merchant-wide credential for the
// bound merchant and verifies NOTHING. This is the "no-op authenticator" half of
// "doujins minus auth" — the host is trusted for its own merchant, exactly as an
// embedding host treats its own in-process engine. It replaces the per-test
// stubResolver the conformance test used to carry.
type trustingResolver struct {
	merchantID   merchant.ID
	merchantSlug string
}

func (trustingResolver) LooksLikeAPIKey(string) bool { return true }

func (r trustingResolver) ResolveAPIKey(context.Context, string) (*controlplane.ResolvedServiceCredential, error) {
	return &controlplane.ResolvedServiceCredential{
		OwnerGroupRef: "embedded-host",
		MerchantID:    r.merchantID,
		MerchantSlug:  r.merchantSlug,
		// Full merchant authority: the trusting embedded host owns its merchant.
		Permissions: []string{authcore.OwnerGrant(controlplane.MerchantType)},
	}, nil
}

// StartEmbeddedHost boots Server 1: an embedded engine (embed.New, host-owns-auth)
// over the shared Postgres + Redis, serving the real embedded /v1/merchant/*
// surface over httptest behind the REAL ServiceCredentialRequired middleware wired to
// a TRUSTING resolver (no auth). It registers the bound merchant (embed.New does
// this) and returns a Surface whose client speaks real HTTP. currency is the
// client's default currency (Balance keys off it).
func (h *Harness) StartEmbeddedHost(currency string) *Surface {
	h.t.Helper()

	dbtest.EnsureTestMerchant(h.ctx, h.t, h.sharedPool())

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: h.DSN}}
	rt, err := embed.New(h.ctx, embed.Options{
		Merchant: dbtest.TestMerchantSlug,
		Options:  embedded.Options{Config: cfg, Redis: h.Redis},
	})
	require.NoError(h.t, err, "embed.New")
	h.t.Cleanup(func() { _ = rt.Close(context.Background()) })

	router := gin.New()
	router.Use(ginmw.ResolveMerchant(dbtest.TestMerchantID))
	runtime := rt.Embedded().App().Runtime
	httproutes.RegisterServiceRoutes(
		ginrouter.New(router.Group("/v1/merchant"), runtime),
		runtime,
		httproutes.Options{
			ServiceCredentialResolver: trustingResolver{
				merchantID:   dbtest.TestMerchantID,
				merchantSlug: dbtest.TestMerchantSlug,
			},
		},
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
// -> internal/http, the cmd/openrails run-server graph) over the shared Postgres +
// Redis with the OpenRails control plane attached, then provisions the merchant
// through the REAL control-plane bootstrap (links permission_group_id + mints a real
// admin API key via AuthKit core) and returns a Surface whose client
// authenticates with that real token. The /v1/merchant/* path is authenticated by
// the real ServiceCredentialRequired -> ResolveAPIKey -> AuthKit core chain
// (#481 role-based merchant authz). No stubs.
// StartStandalone boots the standalone server for an integration test. The server
// connects as the unprivileged openrails_app role (NOBYPASSRLS), so the per-merchant
// RLS policies enforce exactly as in production — every integration test exercises
// real RLS, never a privileged bypass. Fixtures are still seeded via the super pool
// (h.sharedPool), which bypasses RLS for cross-merchant setup an admin does out of band.
func (h *Harness) StartStandalone(currency string) *Surface {
	_, appDSN := dbtest.SharedRLSPostgres(h.t)
	return h.startStandalone(currency, appDSN, "standalone")
}

func (h *Harness) startStandalone(currency, appDSN, name string) *Surface {
	h.t.Helper()

	// The merchant directory row must exist before bootstrap links permission_group_id
	// onto it (#480: no default merchant).
	dbtest.EnsureTestMerchant(h.ctx, h.t, h.sharedPool())

	cfg := &config.Config{
		Env:      "dev",
		TestMode: true,
		Host:     "127.0.0.1",
		Port:     0, // ephemeral; we serve via httptest below
		DB:       &config.DBConfig{URL: appDSN},
		Auth: &config.AuthConfig{
			// The control plane's own AuthKit issuer.
			Issuer: "https://controlplane.openrails.test",
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
	// the merchant's permission_group_id to it, and mints a REAL admin API key
	// scoped to the merchant — the production provisioning path (cmd/openrails
	// run-server does the same at boot). MintInitialAPIKey returns the
	// one-time secret we hand to the client.
	res, err := embcp.RunBootstrap(h.ctx, app, controlplane.BootstrapOptions{
		BootstrapOrgSlug:  dbtest.TestMerchantSlug,
		MintInitialAPIKey: true,
	})
	require.NoError(h.t, err, "control plane bootstrap")
	require.NotNil(h.t, res)
	token := res.APIKeySecret
	if token == "" {
		// A previous run already minted one (shared DB across the package): mint a
		// fresh real token directly through AuthKit core, scoped to the merchant.
		token = h.mintFreshAPIKey(app)
	}
	require.NotEmpty(h.t, token, "standalone API key")
	// Sanity: the real resolver must accept the real token end-to-end (#481 path).
	cp := embcp.Get(app)
	require.NotNil(h.t, cp, "control plane attached")
	resolved, rerr := cp.ResolveAPIKey(h.ctx, token)
	require.NoError(h.t, rerr, "real API key must resolve through AuthKit core")
	require.Equal(h.t, dbtest.TestMerchantID, resolved.MerchantID)

	// Workers: the conformance script does not need background workers, but River
	// must be initialised for the runtime to be healthy. The standalone server
	// graph does not start workers itself here; the service routes used by the
	// script (credits/admit/windows) are synchronous, so we serve without workers.
	srv := httptest.NewServer(assembled.Server.Handler())
	h.t.Cleanup(srv.Close)

	return &Surface{
		Name:     name,
		BaseURL:  srv.URL,
		Token:    token,
		h:        h,
		app:      app,
		currency: currency,
	}
}

// RemoteAppCaller is a remote_application (#76/#484) provisioned
// against the standalone surface's real control plane: its registered slug/issuer
// and a freshly minted remote application access token. Present it as a Bearer
// credential to the /v1/merchant/* surface to drive the #484 path.
type RemoteAppCaller struct {
	Slug   string
	Issuer string
	// Token is a minted remote application access token
	// (typ=remote-application-access+jwt) signed by the principal's own key.
	// Authority is STORED (assigned role/perms), never self-claimed.
	Token string
}

// DelegatedCaller is a registered remote_application issuer plus a delegated
// access token signed by that issuer. Present it to /v1/me/* or /v1/merchant/*.
type DelegatedCaller struct {
	Slug    string
	Issuer  string
	Subject string
	Token   string
}

// OwnedMerchant is an additional AuthKit-org-owned merchant fixture provisioned
// against a standalone surface's real control plane.
type OwnedMerchant struct {
	OrgSlug      string
	OrgID        string
	MerchantID   merchant.ID
	MerchantSlug string
	APIKey       string
}

// ServiceJWTCaller is a registered first-party service-JWT issuer plus a freshly
// minted token signed by that issuer.
type ServiceJWTCaller struct {
	Slug   string
	Issuer string
	Token  string
}

// ensureMerchantGroup idempotently ensures the root + merchant permission-group
// for slug exist (#567) and returns the merchant group's internal id.
func (h *Harness) ensureMerchantGroup(core authkit.Client, slug string) string {
	h.t.Helper()
	_, err := core.EnsureRootGroup(h.ctx)
	require.NoError(h.t, err, "ensure root group")
	require.NoError(h.t, core.SeedPermissionGroupContainment(h.ctx), "seed containment")
	gid, err := core.ResolveGroupIDForSlug(h.ctx, controlplane.MerchantType, slug)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		gid, err = core.CreatePermissionGroup(h.ctx, authkit.CreatePermissionGroupRequest{
			Persona:       controlplane.MerchantType,
			InstanceSlug:  slug,
			ParentPersona: authcore.RootPersona,
		})
	}
	require.NoError(h.t, err, "ensure merchant permission-group")
	return gid
}

// ProvisionOwnedMerchant creates a merchant permission-group (#567), links an
// OpenRails merchant row to it (recording the group id), and mints a real API key
// scoped to the merchant under the merchant `owner` role. It is for standalone
// integration fixtures that need more than the default bootstrapped merchant.
func (s *Surface) ProvisionOwnedMerchant(slug string) OwnedMerchant {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "ProvisionOwnedMerchant requires the standalone surface")
	slug = strings.ToLower(strings.TrimSpace(slug))
	require.NotEmpty(h.t, slug, "owned merchant slug")

	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(h.t, core, "authkit core")

	groupID := h.ensureMerchantGroup(core, slug)

	mid := merchant.ID(uuid.New())
	_, err := h.sharedPool().Exec(h.ctx, `
		INSERT INTO openrails.merchants (id, slug, status, permission_group_id)
		VALUES ($1, $2, 'active', $3)
	`, mid.UUID(), slug, groupID)
	require.NoError(h.t, err, "insert owned merchant")

	token := s.MintAPIKey(slug, slug+"-operator", nil)

	return OwnedMerchant{
		OrgSlug:      slug,
		OrgID:        groupID,
		MerchantID:   mid,
		MerchantSlug: slug,
		APIKey:       token,
	}
}

// CreateAuthKitOrg creates a top-level merchant permission-group through the
// standalone control plane (there is no `org` persona under #567) and returns its
// slug. Kept under the old name for caller source-compat.
func (s *Surface) CreateAuthKitOrg(slug string) string {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "CreateAuthKitOrg requires the standalone surface")
	slug = strings.ToLower(strings.TrimSpace(slug))
	require.NotEmpty(h.t, slug, "merchant group slug")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	h.ensureMerchantGroup(cp.Core(), slug)
	return slug
}

// MintUserAccessToken creates a real AuthKit user and mints a normal user access
// token from the standalone control plane issuer.
func (s *Surface) MintUserAccessToken(username string) string {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "MintUserAccessToken requires the standalone surface")
	username = strings.ToLower(strings.TrimSpace(username))
	require.NotEmpty(h.t, username, "username")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	user, err := cp.Core().CreateUser(h.ctx, username+"@example.com", username)
	require.NoError(h.t, err, "create user")
	token, _, err := cp.Core().IssueAccessToken(h.ctx, user.ID, username+"@example.com", nil)
	require.NoError(h.t, err, "mint user access token")
	return token
}

// RegisterRemoteApplication provisions a static-key remote_application (#484) on the standalone
// surface's REAL control plane and returns a minted remote application access token. It stands up a
// test issuer for the principal, registers the remote_application, assigns it
// role (when non-empty) on ownerOrgSlug — the merchant that
// owns the test merchant — and signs a token with the principal's own key.
//
// With the org `owner` role on the merchant's owner_org, the principal can
// administer the merchant via the existing #481 role-based authz. Pass role=""
// to provision a principal with NO authority (the deny case).
func (s *Surface) RegisterRemoteApplication(slug, ownerOrgSlug, role string) RemoteAppCaller {
	return s.registerRemoteApplication(slug, ownerOrgSlug, role, nil)
}

// RegisterRemoteApplicationWithPermissionsClaim is the same remote_application
// setup as RegisterRemoteApplication, but includes an explicit permissions claim
// in the remote application access token. AuthKit treats this as a down-scope request against STORED
// authority, never as a widening grant; tests use this to prove self-claimed
// permissions alone do not authorize a principal.
func (s *Surface) RegisterRemoteApplicationWithPermissionsClaim(slug, ownerOrgSlug, role string, permissions []string) RemoteAppCaller {
	perms := append([]string(nil), permissions...)
	return s.registerRemoteApplication(slug, ownerOrgSlug, role, perms)
}

func (s *Surface) registerRemoteApplication(slug, ownerOrgSlug, role string, permissionsClaim []string) RemoteAppCaller {
	h := s.h
	require.NotNil(h.t, s.app, "RegisterRemoteApplication requires the standalone surface")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(h.t, core, "authkit core")

	// The principal's own signing key. AuthKit rejects loopback HTTP JWKS URLs
	// now, so integration tests register the same key through static public_keys.
	issuer := authtesting.NewTestIssuerWithAudience("openrails")
	h.t.Cleanup(issuer.Close)

	// #567: a remote_application is nested under its controlling merchant
	// permission-group (PermissionGroupID carries the permission_group_id).
	groupID := h.ensureMerchantGroup(core, ownerOrgSlug)
	ra, err := core.UpsertRemoteApplication(h.ctx, authkit.RemoteApplication{
		Slug:              slug,
		PermissionGroupID: groupID,
		Issuer:            issuer.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(h.t, issuer),
		Enabled:           true,
	})
	require.NoError(h.t, err, "register remote_application")

	if role != "" {
		require.NoError(h.t, core.AssignGroupRole(h.ctx, controlplane.MerchantType, ownerOrgSlug, ra.ID, authcore.SubjectKindRemoteApp, role),
			"assign merchant role to remote_application")
	}

	// Pick up the new issuer in the verifier's in-memory registry (it also
	// lazy-loads on first use, but reload makes the test deterministic).
	require.NoError(h.t, cp.ReloadRemoteApplications(h.ctx), "reload remote_applications")

	token, err := authcore.MintRemoteApplicationAccessToken(h.ctx, issuer.Signer(), authkit.RemoteApplicationAccessParams{
		Issuer:      issuer.URL(),
		Audiences:   []string{"openrails"},
		TTL:         time.Hour,
		Permissions: permissionsClaim,
	})
	require.NoError(h.t, err, "mint remote application access token")

	return RemoteAppCaller{Slug: slug, Issuer: issuer.URL(), Token: token}
}

// RegisterDelegatedCaller registers an AuthKit remote_application issuer for an
// owner org and mints a delegated access token from it. The token's issuer maps
// to the OpenRails merchant through the remote_application registry; the token's
// delegated_sub remains the acting user/admin subject.
func (s *Surface) RegisterDelegatedCaller(slug, ownerOrgSlug, subject string, permissions []string) DelegatedCaller {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "RegisterDelegatedCaller requires the standalone surface")
	require.NotEmpty(h.t, strings.TrimSpace(subject), "delegated subject")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(h.t, core, "authkit core")

	issuer := authtesting.NewTestIssuerWithAudience("openrails")
	h.t.Cleanup(issuer.Close)

	groupID := h.ensureMerchantGroup(core, ownerOrgSlug)
	ra, err := core.UpsertRemoteApplication(h.ctx, authkit.RemoteApplication{
		Slug:              slug,
		PermissionGroupID: groupID,
		Issuer:            issuer.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(h.t, issuer),
		Enabled:           true,
	})
	require.NoError(h.t, err, "register delegated issuer")
	if len(permissions) > 0 {
		// #567: merchant groups have fixed catalog roles (no custom roles). Grant
		// the merchant `owner` role (= merchant:*); the delegated token's claim is
		// then bounded down to its requested subset at verify/gate time.
		require.NoError(h.t, core.AssignGroupRole(h.ctx, controlplane.MerchantType, ownerOrgSlug, ra.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner),
			"assign merchant owner role to delegated remote_application")
	}
	require.NoError(h.t, cp.ReloadRemoteApplications(h.ctx), "reload remote_applications")

	token, err := authcore.MintDelegatedAccessToken(h.ctx, issuer.Signer(), authkit.DelegatedAccessParams{
		Issuer:           issuer.URL(),
		Audiences:        []string{"openrails"},
		DelegatedSubject: subject,
		Permissions:      append([]string(nil), permissions...),
		TTL:              time.Hour,
	})
	require.NoError(h.t, err, "mint delegated access token")

	return DelegatedCaller{Slug: slug, Issuer: issuer.URL(), Subject: subject, Token: token}
}

// RegisterServiceJWTIssuer registers a remote_application issuer for an org and
// mints a service JWT from it. Registering the issuer to the org is the
// authority root; AuthKit no longer carries opaque resource scopes on service
// JWTs, so the issuer's permission group is the merchant boundary.
func (s *Surface) RegisterServiceJWTIssuer(slug, ownerOrgSlug string, permissions []string) ServiceJWTCaller {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "RegisterServiceJWTIssuer requires the standalone surface")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(h.t, core, "authkit core")

	issuer := authtesting.NewTestIssuerWithAudience("openrails")
	h.t.Cleanup(issuer.Close)

	groupID := h.ensureMerchantGroup(core, ownerOrgSlug)
	ra, err := core.UpsertRemoteApplication(h.ctx, authkit.RemoteApplication{
		Slug:              slug,
		PermissionGroupID: groupID,
		Issuer:            issuer.URL(),
		Mode:              authkit.RemoteAppModeStatic,
		PublicKeys:        testIssuerRemoteAppKeys(h.t, issuer),
		Enabled:           true,
	})
	if len(permissions) == 0 {
		permissions = []string{controlplane.PermMerchantCustomerSettingsUpdate}
	}
	require.NoError(h.t, err, "register service-JWT issuer")
	// #567: assign the merchant `owner` role (= merchant:*); the service JWT's
	// claimed permissions are bounded down to its requested subset at verify time.
	require.NoError(h.t, core.AssignGroupRole(h.ctx, controlplane.MerchantType, ownerOrgSlug, ra.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner),
		"assign merchant owner role to service-JWT remote_application")
	require.NoError(h.t, cp.ReloadRemoteApplications(h.ctx), "reload remote_applications")

	token, _, err := authcore.MintServiceJWT(h.ctx, issuer.Signer(), issuer.URL(), authkit.ServiceJWTMintOptions{
		Subject:     "service:" + slug,
		Audiences:   []string{"openrails"},
		Permissions: permissions,
		JTI:         uuid.NewString(),
	})
	require.NoError(h.t, err, "mint service JWT")

	return ServiceJWTCaller{Slug: slug, Issuer: issuer.URL(), Token: token}
}

// MintAPIKey mints a real AuthKit API key under the merchant permission-group
// (#567). merchantSlug is the merchant group's resource ref. The key is minted
// against a merchant catalog role: the merchant `owner` (= merchant:*) by default
// (or `viewer` when permissions are all reads). The legacy `permissions` slice is
// no longer a bespoke role — merchant groups have FIXED catalog roles — so it is
// used only to pick owner vs viewer; callers needing finer scope must use a
// catalog role directly.
func (s *Surface) MintAPIKey(merchantSlug, name string, permissions []string) string {
	h := s.h
	h.t.Helper()
	require.NotNil(h.t, s.app, "MintAPIKey requires the standalone surface")
	cp := embcp.Get(s.app)
	require.NotNil(h.t, cp, "control plane attached")
	h.ensureMerchantGroup(cp.Core(), merchantSlug)
	createdBy := h.ensureAPIKeyActor(cp, merchantSlug)
	_, secret, err := cp.Core().MintAPIKeyWithOptions(h.ctx, controlplane.MerchantType, merchantSlug, authkit.APIKeyMintOptions{
		Name:      name,
		Role:      roleForPerms(permissions),
		CreatedBy: createdBy,
	})
	require.NoError(h.t, err, "mint API key")
	return secret
}

// roleForPerms maps a requested permission set onto one of the merchant's FIXED
// catalog roles (owner/support/viewer; #567 — API keys are role-based, authkit
// dropped per-key perms in #95). Rules: empty -> owner; ANY write (non-:read)
// perm -> owner (writes imply broad service authority); an all-reads set -> the
// NARROWEST catalog role whose perms cover every requested read (support before
// viewer), else owner. Narrowest-covering is what lets a customer-settings:read
// key be denied catalog (-> support, which lacks catalog:read) instead of being
// widened to viewer.
func roleForPerms(perms []string) string {
	if len(perms) == 0 {
		return controlplane.MerchantRoleOwner
	}
	for _, p := range perms {
		if !strings.HasSuffix(p, ":read") {
			return controlplane.MerchantRoleOwner
		}
	}
	cat := merchantRoleCatalog()
	for _, role := range []string{controlplane.MerchantRoleSupport, controlplane.MerchantRoleViewer} {
		if permsCovered(cat[role], perms) {
			return role
		}
	}
	return controlplane.MerchantRoleOwner
}

// merchantRoleCatalog maps each declared merchant catalog role to its permission set.
func merchantRoleCatalog() map[string][]string {
	out := map[string][]string{}
	for _, gt := range controlplane.Groups() {
		if gt.Name == controlplane.MerchantType {
			for _, r := range gt.Roles {
				out[r.Name] = r.Permissions
			}
		}
	}
	return out
}

// permsCovered reports whether every requested perm is in have.
func permsCovered(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// mintFreshAPIKey mints a new real admin API key under the test merchant group,
// scoped to the test merchant — used when bootstrap found an existing token (the
// shared-DB second-run case) and returned no secret.
func (h *Harness) mintFreshAPIKey(a *app.App) string {
	h.t.Helper()
	cp := embcp.Get(a)
	require.NotNil(h.t, cp, "control plane attached")
	h.ensureMerchantGroup(cp.Core(), dbtest.TestMerchantSlug)
	createdBy := h.ensureAPIKeyActor(cp, dbtest.TestMerchantSlug)
	_, secret, err := cp.Core().MintAPIKeyWithOptions(h.ctx, controlplane.MerchantType, dbtest.TestMerchantSlug, authkit.APIKeyMintOptions{
		Name:      "integrationharness-extra",
		Role:      controlplane.MerchantRoleOwner,
		CreatedBy: createdBy,
	})
	require.NoError(h.t, err, "mint fresh API key")
	return secret
}

func (h *Harness) ensureAPIKeyActor(cp *controlplane.ControlPlane, merchantSlug string) string {
	h.t.Helper()
	username := "openrailsci" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(strings.TrimSpace(merchantSlug)))
	if username == "openrailsci" {
		username = "openrailscitest"
	}
	email := username + "@example.test"
	user, err := cp.Core().GetUserByUsername(h.ctx, username)
	if err != nil {
		user, err = cp.Core().CreateUser(h.ctx, email, username)
	}
	require.NoError(h.t, err, "ensure API-key actor")
	require.NoError(h.t, cp.Core().AssignGroupRole(h.ctx, controlplane.MerchantType, merchantSlug, user.ID, authcore.SubjectKindUser, controlplane.MerchantRoleOwner),
		"assign API-key actor merchant owner")
	return user.ID
}

func testIssuerRemoteAppKeys(t *testing.T, issuer *authtesting.TestIssuer) []authkit.RemoteAppKey {
	t.Helper()
	signer, ok := issuer.Signer().(jwtkit.PublicKeySigner)
	require.True(t, ok, "test issuer signer must expose a public key")
	der, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
	require.NoError(t, err, "marshal test issuer public key")
	return []authkit.RemoteAppKey{{
		KID:          issuer.Signer().KID(),
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	}}
}
