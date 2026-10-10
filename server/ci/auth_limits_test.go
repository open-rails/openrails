//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server"
)

// AuthKit's rate limits hold without Redis on one instance, in its memory, and
// are shared through Redis by two instances: the request past the limit is
// refused whichever instance it reaches.
func TestAuthKitLimits(t *testing.T) {
	f := newFixture(t)
	shared := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if shared == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	for _, tc := range []struct {
		name, redis, client string
		instances           int
	}{
		{name: "one instance without Redis", client: "203.0.113.10:4000", instances: 1},
		{name: "two instances sharing Redis", redis: shared, client: "203.0.113.11:4000", instances: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit := func(cfg *server.Config, _ *server.Deps) {
				cfg.Auth.HTTP.RateLimits = map[string]authkit.RateLimit{"password_login": {Limit: 3, Window: time.Hour}}
				if tc.redis != "" {
					cfg.Engine.Redis = &openrails.RedisConfig{Addr: tc.redis}
				}
			}
			handlers := make([]http.Handler, tc.instances)
			var first *server.Server
			for i := range handlers {
				srv := f.newServer(t, edit)
				if first == nil {
					first = srv
				}
				h, err := standaloneHandler(srv)
				require.NoError(t, err)
				handlers[i] = h
			}
			u := newAccount(t, first)
			login := func(h http.Handler) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/"+f.schema+"/v1/password/login",
					strings.NewReader(`{"identifier":"`+u.Email+`","password":"wrong-password"}`))
				r.Header.Set("Content-Type", "application/json")
				r.RemoteAddr = tc.client
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			at := func(i int) http.Handler { return handlers[i%len(handlers)] }
			for i := range 3 {
				w := login(at(i))
				require.Equal(t, http.StatusUnauthorized, w.Code, "request %d: %s", i+1, w.Body.String())
			}
			for i := 3; i < 5; i++ {
				w := login(at(i))
				require.Equal(t, http.StatusTooManyRequests, w.Code, "the budget was lifted: %s", w.Body.String())
				require.NotEmpty(t, w.Header().Get("Retry-After"))
			}
		})
	}
}
