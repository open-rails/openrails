//go:build e2e && integration

package ci_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server"
)

// A standalone server declares the proxies in front of it once, on Engine, and
// the engine's limits and AuthKit's both key on the client they resolve: a
// trusted proxy's X-Forwarded-For names the client, an untrusted peer is the
// client whatever it forwards.
func TestServerLimitsTheClientBehindItsProxy(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.TrustedProxies = []string{"10.244.0.0/16"}
		cfg.Engine.RateLimits = &openrails.RateLimitsConfig{"checkout": {RequestsPerMinute: 2}}
		cfg.Auth.DirectPeerIP = false
		cfg.AuthRateLimits = map[string]server.AuthRateLimit{"password_login": {Limit: 2, Window: time.Hour}}
	})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)

	send := func(path func() string, peer, forwardedFor string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path(), strings.NewReader(`{"identifier":"nobody@e2e.test","password":"Wrong-passphrase-1"}`))
		r.RemoteAddr = peer
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", forwardedFor)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	const proxy, outsider = "10.244.0.53:33550", "203.0.113.9:4000"
	// Each pay names its own session, so only the per-client limit can refuse it.
	pay := func() string { return "/v1/checkout-sessions/ocs_" + uuid.NewString()[:8] + "/pay" }
	login := func() string { return "/" + f.schema + "/v1/password/login" }
	for name, path := range map[string]func() string{"engine checkout": pay, "AuthKit password": login} {
		for range 2 {
			require.NotEqual(t, http.StatusTooManyRequests, send(path, proxy, "198.51.100.1"), name)
		}
		require.Equal(t, http.StatusTooManyRequests, send(path, proxy, "198.51.100.1"), "the client behind the proxy spent its budget: %s", name)
		require.NotEqual(t, http.StatusTooManyRequests, send(path, proxy, "198.51.100.2"), "clients behind the proxy share its budget: %s", name)

		for i := range 2 {
			require.NotEqual(t, http.StatusTooManyRequests, send(path, outsider, fmt.Sprintf("198.51.100.%d", 10+i)), name)
		}
		require.Equal(t, http.StatusTooManyRequests, send(path, outsider, "198.51.100.12"), "a forged X-Forwarded-For bought a fresh budget: %s", name)
	}
}
