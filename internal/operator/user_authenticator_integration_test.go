//go:build integration

package operator_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
)

// TestUserAuthenticator_InProcess drives the #739 seam through the exported
// surface only: a hosted embedder registers + verifies a user over the mounted
// AuthKit routes, then authenticates its OWN wrapper-style *http.Request with
// ControlPlane.UserAuthenticator. The token issuer is a URL nothing listens on,
// so a passing verify proves the keys came from process memory — no JWKS HTTP
// fetch.
func TestUserAuthenticator_InProcess(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	// Nothing serves this issuer: a JWKS fetch would fail, an in-process verify
	// must not care.
	cfg := hostedTestConfig(t, dsn, "https://unreachable-issuer.openrails.test")
	e := newHostApp(t, cfg)

	sender := &captureEmailSender{}
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture: true,
		EmailSender:   sender,
	}))
	srv := mountAuthRoutes(t, e)

	// Register -> verify; the confirm response establishes a session and returns
	// the user's access token.
	sfx := strings.ToLower(uuid.NewString()[:8])
	email := "authee-" + sfx + "@example.test"
	status, body := postJSON(t, srv.URL+"/register",
		`{"identifier":"`+email+`","username":"authee`+sfx+`","password":"str0ng-horse-battery!"}`)
	require.Equal(t, http.StatusAccepted, status, "register: %v", body)
	code := sender.code(email)
	require.NotEmpty(t, code)
	status, body = postJSON(t, srv.URL+"/verify/confirm",
		`{"identifier":"`+email+`","code":"`+code+`"}`)
	require.Equal(t, http.StatusOK, status, "verify confirm: %v", body)
	token, _ := body["access_token"].(string)
	require.NotEmpty(t, token, "verify confirm returns an access token")

	cp := embcp.Get(e.App())
	user, err := cp.Core().GetUserByEmail(ctx, email)
	require.NoError(t, err)

	authn := cp.UserAuthenticator()
	require.NotNil(t, authn, "attached control plane vends a user authenticator")

	// A host route request carrying the bearer token resolves to the minted user.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://saas.internal/api/v1/me", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	uc, err := authn.Authenticate(ctx, req)
	require.NoError(t, err, "in-process verify of our own token")
	require.Equal(t, user.ID, uc.UserID)
	require.NoError(t, uc.ValidateSubject())

	subject := authkit.UserSubject(user.ID)
	role := authkit.Role("merchant-directory-viewer")
	const permission = "root:merchants:read"
	permissionAllowed := func(want bool) {
		t.Helper()
		allowed, err := cp.HasRootPermission(ctx, user.ID, permission)
		require.NoError(t, err)
		require.Equal(t, want, allowed)
	}
	require.NoError(t, cp.Core().OperatorAssignGroupRole(ctx, authkit.RootGroup(), subject, role))
	permissionAllowed(true)

	// Native identity remains valid for its issued lifetime; permission gates
	// independently consult live authority, and login/refresh enforce bans.
	reason := "hosted token lifetime test"
	require.NoError(t, cp.Core().BanUser(ctx, user.ID, &reason, nil, user.ID))
	_, err = authn.Authenticate(ctx, req)
	require.NoError(t, err)
	permissionAllowed(true)
	require.NoError(t, cp.Core().UnbanUser(ctx, user.ID))
	_, err = authn.Authenticate(ctx, req)
	require.NoError(t, err)
	require.NoError(t, cp.Core().OperatorUnassignGroupRole(ctx, authkit.RootGroup(), subject, role))
	permissionAllowed(false)
	require.NoError(t, cp.Core().OperatorAssignGroupRole(ctx, authkit.RootGroup(), subject, role))
	_, err = cp.Core().UpdateImportedUser(ctx, user.ID, authkit.ImportUserInput{Email: email, Username: "authee" + sfx, EmailVerified: true, Metadata: map[string]any{"reserved": true}})
	require.NoError(t, err)
	permissionAllowed(false)
	latent, err := cp.Core().ListEffectivePermissions(ctx, subject, authkit.RootGroup())
	require.NoError(t, err)
	require.Contains(t, latent, permission, "raw grant enumeration must not substitute for authorization")
	_, err = cp.Core().UpdateImportedUser(ctx, user.ID, authkit.ImportUserInput{Email: email, Username: "authee" + sfx, EmailVerified: true, Metadata: map[string]any{"reserved": false}})
	require.NoError(t, err)
	permissionAllowed(true)
	deleted, err := cp.Core().SoftDeleteUsers(ctx, []string{user.ID})
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	require.NoError(t, deleted[0].Err)
	_, err = authn.Authenticate(ctx, req)
	require.NoError(t, err)
	permissionAllowed(false)

	// Garbage credentials are rejected.
	bad, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://saas.internal/api/v1/me", nil)
	require.NoError(t, err)
	bad.Header.Set("Authorization", "Bearer not-a-token")
	_, err = authn.Authenticate(ctx, bad)
	require.Error(t, err, "garbage token must not authenticate")

	// No control plane -> no authenticator (nil, not a panic).
	require.Nil(t, embcp.Get(nil).UserAuthenticator())
}
