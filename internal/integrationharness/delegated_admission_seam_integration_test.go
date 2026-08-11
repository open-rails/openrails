//go:build integration

package integrationharness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/embedded"
	orauthkit "github.com/open-rails/openrails/pkg/embedded/authkit"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// or#918 end to end: the admission seam absorbs BOTH decisions a privileged
// host used to keep a hand-rolled bridge for (doujins #803), over real
// AuthKit, real tokens, real permission-group state and the real embedded
// mount — no stub authenticator anywhere.
//
//  1. LIVENESS. The host injects its own verifier and an Admission backed by
//     the live user authority. A user who is banned AFTER minting a token
//     still presents a token that VERIFIES; the veto is what turns that into a
//     401, on every delegated principal including the /v1/me self path.
//  2. A REQUEST-SCOPED, DB-BACKED GRANT. merchant:* comes from a live
//     permission-group read scoped to the merchant surface — not from the
//     token's role snapshot, and not on the hot self path. WithRolePermissions
//     (func([]string) []string) cannot express either half.
//
// It also pins the two properties that make the seam safe to adopt: the
// merchant stays the engine's bound merchant (never token-derived, th#1765),
// and WithMerchantSlug is load-bearing — or#916's merchant-payer treasury
// address is the slug.
func TestDelegatedAdmissionSeam_LivenessAndDBBackedGrant(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.sharedPool())

	cfg := &config.Config{
		Env:            "dev",
		TestMode:       config.CredentialPostureSandbox,
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendDB,
		DB:             &config.DBConfig{URL: h.DSN},
		Auth:           &config.AuthConfig{Issuer: "https://or918.openrails.test"},
	}
	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: cfg, Redis: h.Redis, River: embedded.RiverManagedByOpenRails()},
	})
	require.NoError(t, err, "embed.New")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	app := rt.Embedded().App()
	app.Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	// The host's own AuthKit, beside the engine — the doujins shape.
	require.NoError(t, embcp.Attach(ctx, app, cfg, nil), "attach control plane")
	_, err = embcp.RunBootstrap(ctx, app, embcp.BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug})
	require.NoError(t, err, "control plane bootstrap links the merchant permission group")
	cp := embcp.Get(app)
	require.NotNil(t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(t, core, "authkit core")

	sfx := strings.ToLower(uuid.NewString()[:8])
	newUser := func(name string) (id, token string) {
		t.Helper()
		u, err := core.CreateUser(ctx, name+"-"+sfx+"@example.test", name+sfx)
		require.NoError(t, err, "create user")
		tok, _, err := core.MintAccessToken(ctx, u.ID, nil)
		require.NoError(t, err, "mint access token")
		return u.ID, tok
	}
	adminID, adminToken := newUser("or918-admin")
	plainID, plainToken := newUser("or918-plain")

	// The merchant-admin authority is live permission-group state, not a claim:
	// the token was minted BEFORE this grant and never learns about it.
	require.NoError(t, core.Genesis().AssignGroupRole(ctx, controlplane.MerchantType, dbtest.TestMerchantSlug,
		adminID, authcore.SubjectKindUser, controlplane.MerchantRoleOwner), "grant merchant owner")

	// --- the seam, wired the way doujins would wire it -------------------
	var lookups []string
	admissions := 0
	liveUserGate := func(ctx context.Context, _ *http.Request, cl verify.Claims) error {
		admissions++
		allowed, err := core.IsUserAllowed(ctx, cl.UserID)
		if err != nil {
			return fmt.Errorf("liveness lookup: %w", err) // fail closed
		}
		if !allowed {
			return errors.New("user is not allowed")
		}
		return nil
	}
	// The merchant surface + a request naming the merchant's own treasury
	// account are the only paths worth a live authority read; the /v1/me self
	// path stays free of them (doujins #774's posture).
	needsMerchantAuthority := func(r *http.Request) bool {
		p := r.URL.Path
		return strings.HasPrefix(p, "/billing/v1/merchant") ||
			strings.HasPrefix(p, "/billing/v1/customers/"+dbtest.TestMerchantSlug)
	}
	grant := func(ctx context.Context, r *http.Request, cl verify.Claims) ([]string, error) {
		perms := []string{permissions.CustomerAll}
		if needsMerchantAuthority(r) {
			lookups = append(lookups, cl.UserID)
			admin, err := cp.IsAdmin(ctx, dbtest.TestMerchantSlug, cl.UserID)
			if err != nil {
				return nil, fmt.Errorf("merchant authority: %w", err)
			}
			if admin {
				perms = append(perms, permissions.MerchantAll)
			}
		}
		return perms, nil
	}

	authn, err := orauthkit.NewDelegatedAuthenticator(
		cp.AuthService().Verifier(), // the HOST's own verifier, not a JWKS refetch
		dbtest.TestMerchantID.String(),
		orauthkit.WithMerchantSlug(dbtest.TestMerchantSlug),
		orauthkit.WithAdmission(liveUserGate),
		orauthkit.WithPermissionResolver(grant),
	)
	require.NoError(t, err)

	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		MountPrefix:            "/billing",
		RouteSets:              []embedded.RouteSet{embedded.RouteSetMerchantAPI, embedded.RouteSetCustomer},
		Gate:                   httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: authn}),
		DelegatedAuthenticator: authn,
	})
	require.NoError(t, err)

	get := func(token, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	const selfBalance = "/billing/v1/me/balance?currency=USD"
	merchantBalance := "/billing/v1/customers/" + dbtest.TestMerchantSlug + "/balance?currency=USD"

	// (1) A live user reaches the self surface, and no authority lookup ran.
	rec := get(plainToken, selfBalance)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, lookups, "the hot self path performs no permission lookup")
	require.Equal(t, 1, admissions, "admission runs on every delegated principal, self included")

	// (2) The merchant's OWN treasury account, addressed by slug (or#916),
	// opens only for the DB-backed merchant grant. Same route, same shape of
	// token, different LIVE authority.
	rec = get(adminToken, merchantBalance)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{adminID}, lookups, "the grant is a live read, scoped to the merchant path")

	rec = get(plainToken, merchantBalance)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a subject without live merchant authority cannot bind the merchant payer")
	require.Equal(t, []string{adminID, plainID}, lookups)

	// (3) LIVENESS. Ban the admin; the token it already holds is untouched and
	// still verifies. Without the veto this request would still be a 200.
	reason := "or918 liveness proof"
	require.NoError(t, core.BanUser(ctx, adminID, &reason, nil, adminID), "ban the user")
	verified, verr := cp.AuthService().Verifier().Verify(adminToken)
	require.NoError(t, verr, "the banned user's token still VERIFIES — this is the gap")
	require.Equal(t, adminID, verified.UserID)

	before := len(lookups)
	rec = get(adminToken, merchantBalance)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "the admission veto turns a valid token into a 401")
	require.Len(t, lookups, before, "an inadmissible subject never reaches the grant lookup")
	rec = get(adminToken, selfBalance)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "the self surface is billing too — it is gated as well")

	// (4) The merchant pin is the ENGINE's, and the slug is load-bearing: the
	// same seam without WithMerchantSlug cannot resolve the merchant-payer
	// address at all, even for the merchant admin (th#1765/or#916).
	unslugged, err := orauthkit.NewDelegatedAuthenticator(
		cp.AuthService().Verifier(), dbtest.TestMerchantID.String(),
		orauthkit.WithPermissionResolver(grant),
	)
	require.NoError(t, err)
	noSlugHandler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		MountPrefix:            "/billing",
		RouteSets:              []embedded.RouteSet{embedded.RouteSetCustomer},
		DelegatedAuthenticator: unslugged,
	})
	require.NoError(t, err)
	_, plainToken2 := newUser("or918-owner2")
	req := httptest.NewRequest(http.MethodGet, merchantBalance, nil)
	req.Header.Set("Authorization", "Bearer "+plainToken2)
	rec = httptest.NewRecorder()
	noSlugHandler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"without the slug the merchant treasury has no name to answer to")
}
