//go:build integration

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authhttp"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/stretchr/testify/require"
)

// The billing wrapper must leave AuthKit's independently protected refresh
// cookies intact, including rejection before rotation on an invalid Origin.
func TestAuthKitRefreshCookieSurvivesBillingWrapper(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	httpServer := httptest.NewUnstartedServer(nil)
	t.Cleanup(httpServer.Close)
	origin := "https://" + httpServer.Listener.Addr().String()
	cfg := &config.Config{Env: "dev", APIURL: origin, Auth: &config.AuthConfig{Issuer: origin, KeysPath: t.TempDir()}}
	cp, err := controlplane.New(ctx, cfg, pool, controlplane.WithRedis(rdb))
	require.NoError(t, err)
	t.Cleanup(cp.Close)
	mount, err := authhttp.MountHandler(cp.AuthService(), authhttp.MountOptions{APIPrefix: ControlPlaneAuthPrefix, Groups: cp.MountedRouteGroups(), RefreshCookie: true})
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle(ControlPlaneAuthPrefix+"/", mount)
	s := &Server{cfg: cfg, runtime: &app.Runtime{Config: cfg}, rdb: rdb, authenticator: cp.UserAuthenticator(), controlPlane: cp, browserTierRoutes: middleware.NewBrowserTierRoutes()}
	httpServer.Config.Handler = s.wrapPublicHandler(mux)
	httpServer.StartTLS()
	suffix := uuid.NewString()[:8]
	email := "refresh" + suffix + "@example.test"
	user, err := cp.Core().CreateUser(ctx, email, "refresh"+suffix)
	require.NoError(t, err)
	require.NoError(t, cp.Core().AdminSetPassword(ctx, user.ID, "Refresh-cookie-test-2026!"))
	post := func(path, body, requestOrigin string, cookie *http.Cookie) (int, authkit.TokenSet, *http.Cookie) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", requestOrigin)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response, err := httpServer.Client().Do(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)
		var tokens authkit.TokenSet
		if response.StatusCode == 200 {
			require.NoError(t, json.Unmarshal(raw, &tokens))
		}
		var refreshed *http.Cookie
		for _, cookie := range response.Cookies() {
			if cookie.Name == authhttp.RefreshCookieName {
				refreshed = cookie
			}
		}
		return response.StatusCode, tokens, refreshed
	}
	status, tokens, cookie := post("/auth/password/login", `{"identifier":"`+email+`","password":"Refresh-cookie-test-2026!"}`, origin, nil)
	require.Equal(t, 200, status)
	require.NotEmpty(t, tokens.AccessToken)
	require.Empty(t, tokens.RefreshToken)
	require.NotNil(t, cookie)
	require.True(t, cookie.HttpOnly)
	status, _, _ = post("/auth/token", `{"grant_type":"refresh_token"}`, "https://attacker.example", cookie)
	require.Equal(t, 400, status)
	status, tokens, rotated := post("/auth/token", `{"grant_type":"refresh_token"}`, origin, cookie)
	require.Equal(t, 200, status)
	require.NotEmpty(t, tokens.AccessToken)
	require.Empty(t, tokens.RefreshToken)
	require.NotNil(t, rotated)
	require.NotEqual(t, cookie.Value, rotated.Value)
}
