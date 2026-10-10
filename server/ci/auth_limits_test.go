//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server"
)

// AuthKit's rate limits are shared by every instance through PostgreSQL:
// with no Redis at all, and while a declared Redis is down. A request past
// the limit is refused whichever instance it reaches.
func TestAuthKitLimitsSharedAcrossInstances(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ name, redis, client string }{
		{name: "no Redis", client: "203.0.113.10:4000"},
		{name: "Redis down", redis: "127.0.0.1:1", client: "203.0.113.11:4000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit := func(cfg *server.Config, _ *server.Deps) {
				cfg.AuthRateLimits = map[string]server.AuthRateLimit{"password_login": {Limit: 3, Window: time.Hour}}
				if tc.redis != "" {
					cfg.Engine.Redis = &openrails.RedisConfig{Addr: tc.redis}
				}
			}
			a, b := f.newServer(t, edit), f.newServer(t, edit)
			ha, err := standaloneHandler(a)
			require.NoError(t, err)
			hb, err := standaloneHandler(b)
			require.NoError(t, err)
			u := newAccount(t, a)
			login := func(h http.Handler) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/"+f.schema+"/v1/password/login",
					strings.NewReader(`{"identifier":"`+u.Email+`","password":"wrong-password"}`))
				r.Header.Set("Content-Type", "application/json")
				r.RemoteAddr = tc.client
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			for i, h := range []http.Handler{ha, hb, ha} {
				w := login(h)
				require.Equal(t, http.StatusUnauthorized, w.Code, "request %d: %s", i+1, w.Body.String())
			}
			for _, h := range []http.Handler{hb, ha} {
				w := login(h)
				require.Equal(t, http.StatusTooManyRequests, w.Code, "an instance kept its own budget: %s", w.Body.String())
				require.NotEmpty(t, w.Header().Get("Retry-After"))
			}
		})
	}
}
