//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
)

// outageRedis is a disposable Redis container the test pauses: a paused Redis
// takes connections and never answers, as one cut off mid-flow.
type outageRedis struct {
	t               *testing.T
	container, addr string
}

func newOutageRedis(t *testing.T) *outageRedis {
	t.Helper()
	r := &outageRedis{t: t,
		container: strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_OUTAGE_CONTAINER")),
		addr:      strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_OUTAGE_ADDR"))}
	if r.container == "" || r.addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_OUTAGE_CONTAINER and OPENRAILS_E2E_REDIS_OUTAGE_ADDR must name a disposable Redis container this test pauses")
	}
	unpause := func() { _ = exec.Command("docker", "unpause", r.container).Run() }
	unpause()
	t.Cleanup(unpause)
	return r
}

func (r *outageRedis) docker(command string) {
	r.t.Helper()
	out, err := exec.Command("docker", command, r.container).CombinedOutput()
	require.NoError(r.t, err, "docker %s: %s", command, out)
}

// redisInUse reports readiness's redis entry: whether Redis answers, and
// whether the engine is ready either way.
func redisInUse(w *world) (answers, ready bool) {
	deps, err := engine.Graph(w.rt).Runtime.Ready(w.t.Context())
	for _, d := range deps {
		if d.Name == "redis" {
			return d.Available, err == nil
		}
	}
	return false, err == nil
}

// A Redis that stops answering mid-flow leaves card checkout and Solana Pay
// running: abuse state moves to process memory, Solana Pay never needed Redis,
// and the engine stays ready with Redis degraded. Once Redis answers again it
// is used again, and what memory counted meanwhile still counts: card refusals
// before, during and after the outage add up to a block.
func TestRedisLossAndRecoveryMidFlow(t *testing.T) {
	r := newOutageRedis(t)
	p := newSolanaPay(t, func(c *config.Config) { c.Redis = &config.RedisConfig{Addr: r.addr} })
	w, ctx := p.w, t.Context()
	direct := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{r.addr}, ReadTimeout: 500 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = direct.Close() })
	require.Eventually(t, func() bool { up, ready := redisInUse(w); return up && ready }, 15*time.Second, 50*time.Millisecond, "Redis in use")

	price := w.membership("content:members", 9_990_000).ID
	declined := card{Brand: "visa", Last4: "0002", Decline: "202"}
	// One address testing cards, each through a fresh account.
	ip := "198.51.100.77"
	attempt := func(cd card) error {
		return succeeded(w.newCustomer().checkout(embedded, order{price: price, rail: "nmi", token: w.nmi.Tokenize(cd), ip: ip}))
	}
	refused := func(err error) {
		t.Helper()
		require.ErrorContains(t, err, "order payment failed")
	}
	burst := "card_declines:{" + w.client[embedded].MerchantID().String() + "}:ip:" + ip + ":burst"
	inRedis := func() int64 {
		t.Helper()
		n, err := direct.ZCard(ctx, burst).Result()
		require.NoError(t, err)
		return n
	}
	// A second store over the same Redis, on a clock that stands mid-window.
	state := abusestate.New(direct)
	at := time.Now().Truncate(time.Hour).Add(30 * time.Minute)
	state.SetClock(func() time.Time { return at })
	window := "e2e-outage:" + uuid.NewString()
	count := func() int64 { n, _ := state.Count(ctx, window, 1, time.Hour); return n }

	// Before: a Solana Pay order awaits its wallet; one refusal is in Redis.
	req := p.checkout(w.newCustomer())
	refused(attempt(declined))
	require.EqualValues(t, 1, inRedis())
	require.EqualValues(t, 1, count())

	r.docker("pause")
	sig := p.pay(req, req.amount)
	start := time.Now()
	p.confirm(req)
	require.Less(t, time.Since(start), 15*time.Second, "a hung Redis delays a request by its timeouts only")
	p.until(func() bool { return p.status(req) == "succeeded" }, "the Solana Pay order is credited with Redis down")
	disposition, _ := p.receipt(sig)
	require.Equal(t, "credited", disposition)
	require.Equal(t, 1, p.payments(req))
	require.Eventually(t, func() bool { up, ready := redisInUse(w); return !up && ready }, 20*time.Second, 100*time.Millisecond, "ready, with Redis degraded")
	require.NoError(t, succeeded(w.newCustomer().checkout(embedded, order{price: price, rail: "nmi", token: w.nmi.Tokenize(visa)})), "a card checkout pays with Redis down")
	for range 4 {
		refused(attempt(declined))
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	require.Error(t, state.Probe(probe), "a probe out of time finds Redis down")
	cancel()
	require.EqualValues(t, 1, count(), "memory counts while Redis is down")

	r.docker("unpause")
	require.Eventually(t, func() bool { up, ready := redisInUse(w); return up && ready }, 45*time.Second, 100*time.Millisecond, "Redis in use again")
	require.Eventually(t, func() bool { return state.Probe(ctx) == nil }, 15*time.Second, 100*time.Millisecond)
	require.EqualValues(t, 3, count(), "the outage's count adds to Redis's")

	// One refusal in Redis before, four in memory during, one in Redis after:
	// six in fifteen minutes block the next card before the gateway.
	refused(attempt(declined))
	require.EqualValues(t, 2, inRedis(), "refusals are counted in Redis again")
	sales := len(w.nmi.Sales())
	blocked := requireStatus(t, attempt(visa), http.StatusTooManyRequests)
	require.Equal(t, "card_attempts_blocked", blocked.Code)
	require.Len(t, w.nmi.Sales(), sales, "a blocked attempt never reaches the gateway")

	again := p.checkout(w.newCustomer())
	p.pay(again, again.amount)
	p.confirm(again)
	p.until(func() bool { return p.status(again) == "succeeded" }, "Solana Pay after recovery")
	require.Equal(t, 1, p.payments(again))
}
