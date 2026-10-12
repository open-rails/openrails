//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
)

// staffMember says every request is the merchant's staff member of this id,
// holding every permission in staffRoot and signed in recently.
type staffMember string

func (m staffMember) Authenticate(*http.Request) (auth.Verified, error) { return m, nil }
func (staffMember) RootScope(context.Context) (auth.Scope, error)       { return staffRoot, nil }

func (m staffMember) Identity() openrails.Identity {
	return openrails.Identity{Issuer: authtest.Issuer, Subject: string(m), SubjectKind: openrails.SubjectUser,
		Invoker: openrails.Invoker{Issuer: authtest.Issuer, ID: string(m)}, Credential: openrails.Credential{Kind: openrails.CredentialSession, ID: "s_staff"}}
}

func (staffMember) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	return scope == staffRoot && permission != "", nil
}

func (staffMember) CheckRecentSignIn(context.Context) error { return nil }

// abuseInstances are instances of one host app over one database: each
// serves buyers, and its staff on another mount.
type abuseInstances struct {
	f       *fixture
	engines []*openrails.Engine
	public  []http.Handler
	staff   []http.Handler
	admin   staffMember
}

func newAbuseInstances(t *testing.T, n int, redisConfig *openrails.RedisConfig) *abuseInstances {
	t.Helper()
	return newAbuseInstancesWithRedis(t, n, redisConfig, nil)
}

func newAbuseInstancesWithRedis(t *testing.T, n int, redisConfig *openrails.RedisConfig, client *redis.Client) *abuseInstances {
	t.Helper()
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"limits-" + uuid.NewString()[:8]: {DisplayName: "Limits"}}
	cfg.Captcha = &openrails.CaptchaConfig{SiteKey: "e2e-site", SecretKey: "e2e-secret"}
	cfg.Redis = redisConfig
	in := &abuseInstances{f: f, admin: staffMember(uuid.NewString())}
	for range n {
		bill, _ := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Redis: client})
		buyers, admins := http.NewServeMux(), http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(buyers, bill, openrails.Routes{Auth: authtest.Deny{}}))
		require.NoError(t, openrailshttp.Mount(admins, bill, openrails.Routes{Auth: in.admin, RouteGroups: staffGroups}))
		in.engines = append(in.engines, bill)
		in.public, in.staff = append(in.public, buyers), append(in.staff, admins)
	}
	return in
}

func (in *abuseInstances) call(on http.Handler, method, path, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.RemoteAddr = addr
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, req)
	return rec
}

// pay is a checkout payment on instance i: the checkout bucket, 10 a minute.
func (in *abuseInstances) pay(i int, addr string) *httptest.ResponseRecorder {
	return in.call(in.public[i%len(in.public)], http.MethodPost, "/v1/me/orders/"+billing.OrderID(uuid.New()).String()+"/pay", addr)
}

// remove is a destructive admin operation on instance i: five a minute.
func (in *abuseInstances) remove(i int) *httptest.ResponseRecorder {
	return in.call(in.staff[i%len(in.staff)], http.MethodPost, "/v1/admin/orders/"+billing.OrderID(uuid.New()).String()+"/waive-balance", "198.51.100.8:4711")
}

// noPostgres asserts PostgreSQL has nowhere to keep abuse state: no
// rate_windows table, which counted limits, lockouts and challenges before.
func (in *abuseInstances) noPostgres(t *testing.T) {
	t.Helper()
	var table *string
	require.NoError(t, in.f.pool.QueryRow(t.Context(), "SELECT to_regclass($1)::text", pgx.Identifier{in.f.schema, "rate_windows"}.Sanitize()).Scan(&table))
	require.Nil(t, table, "abuse state is never in PostgreSQL")
}

// freshAddr is a client address no earlier run used: Redis outlives a test.
func freshAddr() string {
	return fmt.Sprintf("198.18.%d.%d:4711", rand.IntN(256), 1+rand.IntN(254))
}

// limitsHold checks the per-address rate limit, an admin's lockout and a
// captcha challenge, each reached on instance 0, from every instance: shared
// says whether the others hold them too.
func (in *abuseInstances) limitsHold(t *testing.T, shared bool) {
	others := func(t *testing.T, check func(t *testing.T, i int)) {
		for i := 1; i < len(in.public); i++ {
			check(t, i)
		}
	}
	t.Run("rate limit", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		addr := freshAddr()
		for i := range 10 {
			at := 0
			if shared {
				at = i
			}
			require.NotEqual(t, http.StatusTooManyRequests, in.pay(at, addr).Code, "request %d", i+1)
		}
		rec := in.pay(0, addr)
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		others(t, func(t *testing.T, i int) {
			rec := in.pay(i, addr)
			require.Equal(t, shared, rec.Code == http.StatusTooManyRequests, "instance %d counts the same window: %t: %d %s", i, shared, rec.Code, rec.Body.String())
		})
	})

	t.Run("admin lockout", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		for i := range 5 {
			at := 0
			if shared {
				at = i
			}
			require.NotEqual(t, http.StatusTooManyRequests, in.remove(at).Code, "operation %d", i+1)
		}
		require.Equal(t, http.StatusTooManyRequests, in.remove(0).Code, "the sixth locks the admin out")
		rec := in.remove(0)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		require.NoError(t, err)
		require.Greater(t, retry, 60, "for the lockout's hour")
		others(t, func(t *testing.T, i int) {
			rec := in.remove(i)
			require.Equal(t, shared, rec.Code == http.StatusTooManyRequests, "instance %d holds the lockout: %t: %d %s", i, shared, rec.Code, rec.Body.String())
		})
	})

	t.Run("captcha challenge", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		addr := freshAddr()
		var last *httptest.ResponseRecorder
		for range 30 { // three times the limit challenges the address
			last = in.pay(0, addr)
		}
		require.Equal(t, "true", last.Header().Get("X-Captcha-Required"), last.Body.String())
		others(t, func(t *testing.T, i int) {
			rec := in.call(in.public[i], http.MethodGet, "/v1/me/payment-methods", addr)
			require.Equal(t, shared, rec.Header().Get("X-Captcha-Required") == "true", "instance %d challenges the address: %t: %s", i, shared, rec.Body.String())
		})
	})
}

// Without Redis an instance keeps its rate limits, admin lockouts and captcha
// challenges in its own memory and enforces each; PostgreSQL holds none.
func TestOneInstanceWithoutRedisEnforcesAbuseLimits(t *testing.T) {
	in := newAbuseInstances(t, 1, nil)
	in.limitsHold(t, false)
	in.noPostgres(t)
}

// Instances with Redis share every limit: a window counted, a lockout set or
// an address challenged on one holds on the others.
func TestInstancesWithRedisShareAbuseLimits(t *testing.T) {
	in := newAbuseInstances(t, 2, &openrails.RedisConfig{Addr: e2eRedis(t)})
	for i, bill := range in.engines {
		require.NoError(t, bill.Start(t.Context()))
		require.Eventually(t, func() bool { dep, ok := redisState(t, bill); return ok && dep.Available }, 15*time.Second, 50*time.Millisecond, "instance %d reaches Redis", i)
	}
	in.limitsHold(t, true)
	in.noPostgres(t)
}

// Construction returns while every command on the injected Redis client is
// blocked, including PING. Its background health probes may wait; a synchronous
// command would prevent construction from returning and fail the test timeout.
// After release the client stays unavailable: each engine serves and enforces
// its limits in memory, counting fallbacks, and PostgreSQL holds none of it.
func TestRedisDownKeepsAbuseLimitsPerInstance(t *testing.T) {
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	// Defers run before engine cleanup, so an assertion failure never leaves
	// Close waiting for a health probe whose hook is still blocked.
	defer release()
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	client.AddHook(heldRedisCommands{released: released})
	in := newAbuseInstancesWithRedis(t, 2, &openrails.RedisConfig{Addr: client.Options().Addr}, client)
	for _, bill := range in.engines {
		require.Same(t, client, engine.Graph(bill).Runtime.RedisClient, "the blocked client is the actual dependency")
	}
	release()

	for i, bill := range in.engines {
		require.NoError(t, bill.Start(t.Context()))
		require.Eventually(t, func() bool { return degraded(t, bill) }, 15*time.Second, 50*time.Millisecond, "instance %d is ready with Redis degraded", i)
	}
	in.limitsHold(t, false)
	in.noPostgres(t)
	for i, bill := range in.engines {
		require.Positive(t, engine.Graph(bill).Runtime.AbuseState.Fallbacks(), "instance %d counts what memory took", i)
	}
}

// heldRedisCommands never contacts a server. No command (including connection
// setup, an individual PING, or a pipeline) can finish until release; afterward
// each reports Redis unavailable. There are no listener or reader goroutines.
type heldRedisCommands struct{ released <-chan struct{} }

var errHeldRedisUnavailable = errors.New("test Redis is unavailable")

func (h heldRedisCommands) wait() error {
	<-h.released
	return errHeldRedisUnavailable
}

func (h heldRedisCommands) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, h.wait() }
}

func (h heldRedisCommands) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(context.Context, redis.Cmder) error { return h.wait() }
}

func (h heldRedisCommands) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return h.wait() }
}
