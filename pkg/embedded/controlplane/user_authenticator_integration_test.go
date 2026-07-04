//go:build integration

package controlplane_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
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
	cfg := hostedTestConfig(dsn, "https://unreachable-issuer.openrails.test")
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
	status, body = postJSON(t, srv.URL+"/email/verify/confirm",
		`{"email":"`+email+`","code":"`+code+`"}`)
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

	// Garbage credentials are rejected.
	bad, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://saas.internal/api/v1/me", nil)
	require.NoError(t, err)
	bad.Header.Set("Authorization", "Bearer not-a-token")
	_, err = authn.Authenticate(ctx, bad)
	require.Error(t, err, "garbage token must not authenticate")

	// No control plane -> no authenticator (nil, not a panic).
	require.Nil(t, embcp.Get(nil).UserAuthenticator())
}
