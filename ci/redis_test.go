//go:build e2e && integration

package ci_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
)

// redisEngine is one engine over cfg's Redis, serving buyers.
func redisEngine(t *testing.T, f *fixture, rc openrails.RedisConfig) (*openrails.Client, http.Handler) {
	t.Helper()
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: "redis-" + uuid.NewString()[:8], DisplayName: "Redis"}
	cfg.Redis = &rc
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	require.NoError(t, client.Start(t.Context()))
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{Auth: authtest.Deny{}}))
	return client, mux
}

// redisState is the redis entry readiness reports while the engine is
// ready; ok is false otherwise.
func redisState(t *testing.T, client *openrails.Client) (dep app.ReadinessDependency, ok bool) {
	deps, err := engine.Graph(client).Runtime.Ready(t.Context())
	if err != nil {
		return dep, false
	}
	for _, d := range deps {
		if d.Name == "redis" {
			return d, true
		}
	}
	return dep, false
}

// degraded reports an unavailable Redis on a ready engine.
func degraded(t *testing.T, client *openrails.Client) bool {
	dep, ok := redisState(t, client)
	return ok && dep.Optional && !dep.Available && dep.Err != nil
}

// pay is a checkout payment from addr: the rate-limited checkout bucket.
func pay(h http.Handler, addr string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/checkout-sessions/ocs_"+strings.ReplaceAll(uuid.NewString(), "-", "")+"/pay", strings.NewReader(`{}`))
	req.RemoteAddr = addr
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// A managed Redis takes only TLS and an ACL user. OpenRails reaches it by a
// rediss:// URL or by address, user and password, verifying it against the
// operator's CA, and counts its rate limits there. A wrong credential or an
// untrusted certificate leaves it degraded.
func TestRedisOverTLSWithACLUser(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_TLS_URL"))
	caFile := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_TLS_CA"))
	if rawURL == "" || caFile == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_TLS_URL and OPENRAILS_E2E_REDIS_TLS_CA must name a TLS-only Redis with an ACL user (scripts/e2e-redis-tls.sh)")
	}
	ca, err := os.ReadFile(caFile)
	require.NoError(t, err)
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	password, _ := u.User.Password()

	opts, err := redis.ParseURL(rawURL)
	require.NoError(t, err)
	opts.TLSConfig.RootCAs = x509.NewCertPool()
	require.True(t, opts.TLSConfig.RootCAs.AppendCertsFromPEM(ca))
	direct := redis.NewClient(opts)
	t.Cleanup(func() { _ = direct.Close() })
	f := newFixture(t)

	for name, rc := range map[string]openrails.RedisConfig{
		"url":     {URL: rawURL, CACert: string(ca)},
		"address": {Addr: u.Host, Username: u.User.Username(), Password: password, CACert: string(ca)},
	} {
		t.Run(name, func(t *testing.T) {
			client, h := redisEngine(t, f, rc)
			require.Eventually(t, func() bool { dep, ok := redisState(t, client); return ok && dep.Available }, 15*time.Second, 50*time.Millisecond)
			ip := "198.51.100." + map[string]string{"url": "21", "address": "22"}[name]
			for range 3 {
				require.NotEqual(t, http.StatusTooManyRequests, pay(h, ip+":4711"))
			}
			keys, err := direct.Keys(t.Context(), "rl:*"+ip+"*").Result()
			require.NoError(t, err)
			require.NotEmpty(t, keys, "the checkout window is counted in this Redis")
		})
	}

	for name, rc := range map[string]openrails.RedisConfig{
		"wrong password":      {URL: rawURL, Password: "wrong", CACert: string(ca)},
		"untrusted CA":        {URL: rawURL},
		"plain TCP over TLS":  {Addr: u.Host, Username: u.User.Username(), Password: password},
		"default user is off": {Addr: u.Host, CACert: string(ca)},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			client, _ := redisEngine(t, f, rc)
			require.Eventually(t, func() bool { return degraded(t, client) }, 15*time.Second, 50*time.Millisecond, "ready, with Redis degraded")
			require.Never(t, func() bool { return !degraded(t, client) }, 2*time.Second, 100*time.Millisecond)
		})
	}
}

// A declared Redis that does not answer costs speed, not correctness: the
// engine is ready and reports Redis degraded, and limits still hold, counted
// in PostgreSQL. Without a declared Redis none is reported.
func TestDeclaredRedisOutageDegrades(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	down, h := redisEngine(t, f, openrails.RedisConfig{Addr: "127.0.0.1:1"})
	require.Less(t, time.Since(start), 10*time.Second, "construction never waits on Redis")
	require.Eventually(t, func() bool { return degraded(t, down) }, 15*time.Second, 50*time.Millisecond, "ready, with Redis degraded")
	const addr = "198.51.100.23:4711"
	// Eleven requests inside one fixed minute window.
	if s := time.Now().Second(); s > 45 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}
	for i := range 10 {
		require.NotEqual(t, http.StatusTooManyRequests, pay(h, addr), "request %d", i+1)
	}
	require.Equal(t, http.StatusTooManyRequests, pay(h, addr), "the window is still counted")

	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: "noredis-" + uuid.NewString()[:8], DisplayName: "No Redis"}
	none, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, none.Close(context.Background())) })
	require.NoError(t, none.Start(t.Context()))
	require.Eventually(t, func() bool { return none.Ready(t.Context()) == nil }, 15*time.Second, 50*time.Millisecond)
	deps, err := engine.Graph(none).Runtime.Ready(t.Context())
	require.NoError(t, err)
	for _, dep := range deps {
		require.NotEqual(t, "redis", dep.Name)
	}
}
