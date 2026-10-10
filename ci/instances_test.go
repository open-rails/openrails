//go:build e2e && integration

package ci_test

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/stripemock"
)

// instance is one replica of a host app embedding OpenRails: its own
// connections, River and HTTP mux over the database every replica shares.
type instance struct {
	client *openrails.Client
	mux    *http.ServeMux
	closed bool
}

func (in *instance) close(t *testing.T) {
	t.Helper()
	if in.closed {
		return
	}
	in.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, in.client.Close(ctx))
}

func e2eRedis(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	return addr
}

// gcJob queues the idempotency sweep by hand: cheap, harmless work.
type gcJob struct{}

func (gcJob) Kind() string { return riverjobs.KindIdempotencyGC }

// Three replicas of one host app share a database, as a deployment scaled to
// three runs them: they boot together, share River's work and schedule, answer
// one provider webhook with one effect, count one rate limit, and read each
// other's catalog edits at once.
func TestInstancesShareOneDatabase(t *testing.T) {
	dsn := emptyDatabase(t)
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	const account, secret = "acct_instances", "whsec_instances"
	key := "shared-" + uuid.NewString()[:8]
	declared, err := catalog.ParseApplicationYAML(fmt.Appendf(nil, `schema_version: 1
products:
  %[1]s:
    display_name: Shared
    entitlements: ["content:%[1]s"]
    prices:
      %[1]s-once:
        currency: usd
        unit_amount: 1000000
`, key))
	require.NoError(t, err)
	cfg := openrails.Config{
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesFull,
		ReturnOrigins:     []string{"https://e2e.test"},
		Redis:             &openrails.RedisConfig{Addr: e2eRedis(t)},
		Merchant: openrails.MerchantDeclaration{Slug: "instances-" + uuid.NewString()[:8], DisplayName: "Instances",
			PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", AccountID: account,
				Secrets: map[string]string{"secret_key": "sk_test_instances", "webhook_signing_secret": secret}}}},
		Catalog: declared,
	}

	// Boot: every replica runs New at once on the empty database. One migrates
	// while the others wait, and the declared merchant and catalog land once.
	instances := make([]*instance, 3)
	errs := make([]error, len(instances))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range instances {
		pool := connect(t, dsn)
		wg.Go(func() {
			<-start
			client, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool, StripeTransport: stripe})
			if err != nil {
				errs[i] = err
				return
			}
			mux := http.NewServeMux()
			errs[i] = openrailshttp.Mount(mux, client, openrails.Routes{Auth: authtest.Deny{}})
			instances[i] = &instance{client: client, mux: mux}
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "replica %d boots", i)
		t.Cleanup(func() { instances[i].close(t) })
	}
	admin := connect(t, dsn)
	count := func(sql string, args ...any) (n int) {
		t.Helper()
		require.NoError(t, admin.QueryRow(t.Context(), sql, args...).Scan(&n))
		return n
	}
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.Equal(t, len(files), count(`SELECT count(*) FROM public.migrations WHERE app = 'openrails'`), "each migration applied once")
	require.Equal(t, 1, count(`SELECT count(*) FROM billing.catalog_applications`), "the declared catalog applied once")
	require.Equal(t, 1, count(`SELECT count(*) FROM billing.psps WHERE key = 'stripe'`))

	// River: the first replica to start leads; the others join its fleet.
	leader := func() (id string) {
		_ = admin.QueryRow(t.Context(), `SELECT leader_id FROM billing_river.river_leader WHERE expires_at > now()`).Scan(&id)
		return id
	}
	require.NoError(t, instances[0].client.Start(t.Context()))
	require.Eventually(t, func() bool { return leader() != "" }, 30*time.Second, 20*time.Millisecond, "the first replica leads River")
	first := leader()
	for _, in := range instances[1:] {
		require.NoError(t, in.client.Start(t.Context()))
	}
	for i, in := range instances {
		require.Eventually(t, func() bool { return in.client.Ready(t.Context()) == nil }, 30*time.Second, 50*time.Millisecond, "replica %d ready", i)
	}

	t.Run("one execution per job", func(t *testing.T) {
		producer, err := river.NewClient(riverpgxv5.New(admin), &river.Config{Schema: "billing_river", SkipUnknownJobCheck: true})
		require.NoError(t, err)
		// More jobs than one replica takes at once (20), queued together.
		params := make([]river.InsertManyParams, 60)
		for i := range params {
			params[i] = river.InsertManyParams{Args: gcJob{}, InsertOpts: &river.InsertOpts{Queue: openrails.QueueBilling}}
		}
		inserted, err := producer.InsertMany(t.Context(), params)
		require.NoError(t, err)
		var ids []int64
		for _, res := range inserted {
			ids = append(ids, res.Job.ID)
		}
		require.Eventually(t, func() bool {
			return count(`SELECT count(*) FROM billing_river.river_job WHERE id = ANY($1) AND state = 'completed'`, ids) == len(ids)
		}, 60*time.Second, 50*time.Millisecond, "every job runs")
		require.Equal(t, 0, count(`SELECT count(*) FROM billing_river.river_job WHERE id = ANY($1) AND (attempt <> 1 OR cardinality(attempted_by) <> 1)`, ids),
			"each job ran once, on one replica")
		require.Greater(t, count(`SELECT count(DISTINCT attempted_by[1]) FROM billing_river.river_job WHERE id = ANY($1)`, ids), 1,
			"the replicas share the work")
	})

	t.Run("one provider webhook, one effect", func(t *testing.T) {
		buyer := uuid.NewString()
		price, err := instances[0].client.ListPrices(t.Context(), billing.PriceListParams{ProductKey: key, Key: key + "-once"})
		require.NoError(t, err)
		require.Len(t, price.Items, 1)
		session, err := sell(t, instances[0].client, billing.CreateCheckoutSessionParams{
			Customer: billing.CheckoutCustomerIdentity{ID: cid(buyer), VerifiedEmail: "buyer@example.test"},
			PriceID:  price.Items[0].ID, SuccessURL: "https://e2e.test/success",
		})
		require.NoError(t, err)
		paying, err := session.on(instances[1].client).pay("stripe", nil)
		require.NoError(t, err)
		require.Equal(t, "requires_action", paying.Status)
		sessions := stripe.CheckoutSessions()
		require.Len(t, sessions, 1)
		_, err = stripe.CompleteCheckoutSession(t.Context(), sessions[0]["id"].(string))
		require.ErrorIs(t, err, stripemock.ErrNoWebhook, "the test delivers the event itself")
		events := stripe.Events()
		event := events[len(events)-1]
		require.Equal(t, "checkout.session.completed", event.Type)

		// Stripe's delivery and its retries reach every replica at once.
		statuses := make([]int, len(instances)*2)
		bodies := make([]string, len(statuses))
		go1 := make(chan struct{})
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Go(func() {
				<-go1
				statuses[i], bodies[i] = postSignedStripeWebhook(t, instances[i%len(instances)].mux, account, secret, event.Payload, time.Now())
			})
		}
		close(go1)
		wg.Wait()
		for i, status := range statuses {
			require.Equal(t, http.StatusOK, status, "delivery %d: %s", i, bodies[i])
		}

		payments, err := instances[2].client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: cid(buyer)})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "one payment")
		held, err := heldKeys(t.Context(), instances[1].client, cid(buyer), time.Time{}, "content:"+key)
		require.NoError(t, err)
		require.True(t, held["content:"+key])
		require.Equal(t, 1, count(`SELECT count(*) FROM billing.grants WHERE customer_id = $1`, uuid.MustParse(buyer)), "one grant")
		require.Equal(t, 1, count(`SELECT count(*) FROM billing.notifications WHERE customer_id = $1`, uuid.MustParse(buyer)), "one receipt")
		require.Equal(t, 1, count(`SELECT count(*) FROM billing.webhook_events WHERE event_id = $1`, event.ID), "one applied event")
	})

	t.Run("one rate limit", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		// Redis outlives the test: a fresh client address.
		addr := fmt.Sprintf("198.18.%d.%d:4711", rand.IntN(256), 1+rand.IntN(254))
		pay := func(in *instance) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/checkout-sessions/ocs_"+strings.ReplaceAll(uuid.NewString(), "-", "")+"/pay", strings.NewReader(`{}`))
			req.RemoteAddr = addr
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			in.mux.ServeHTTP(rec, req)
			return rec
		}
		limit := (*config.DefaultRateLimits())["checkout"].RequestsPerMinute
		for i := range limit {
			rec := pay(instances[i%len(instances)])
			require.NotEqual(t, http.StatusTooManyRequests, rec.Code, "request %d of %d: %s", i+1, limit, rec.Body.String())
		}
		for _, in := range instances {
			rec := pay(in)
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "every replica counts the same window: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
		}
	})

	t.Run("an edit on one replica, read on another", func(t *testing.T) {
		products, err := instances[0].client.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{key}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		_, err = instances[0].client.UpdateProduct(t.Context(), products.Items[0].ID, billing.UpdateProductParams{DisplayName: catalog.Value("Renamed")})
		require.NoError(t, err)
		read, err := instances[2].client.GetProduct(t.Context(), products.Items[0].ID)
		require.NoError(t, err)
		require.Equal(t, "Renamed", read.DisplayName)
		rec := httptest.NewRecorder()
		instances[1].mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/catalog/products?keys="+key, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"Renamed"`, "the public catalog of another replica")
	})

	t.Run("the leader stops", func(t *testing.T) {
		// Periodic sweeps are queued once per period however many replicas run
		// and whoever leads: a new leader's start-up runs dedupe against the old.
		periods := map[string]time.Duration{
			riverjobs.PriceMigrationRedriveArgs{}.Kind(): time.Hour,
			riverjobs.MerchantSecretCleanupArgs{}.Kind(): 5 * time.Minute,
			riverjobs.ConvergeSweepArgs{}.Kind():         15 * time.Minute,
			riverjobs.LedgerIntegrityArgs{}.Kind():       24 * time.Hour,
			riverjobs.AccountUpdaterBatchArgs{}.Kind():   6 * time.Hour,
		}
		oncePerPeriod := func() {
			t.Helper()
			require.Eventually(t, func() bool {
				return count(`SELECT count(DISTINCT kind) FROM billing_river.river_job WHERE kind = ANY($1)`, keys(periods)) == len(periods)
			}, 30*time.Second, 50*time.Millisecond, "the leader runs its start-up sweeps")
			rows, err := admin.Query(t.Context(), `SELECT kind, scheduled_at FROM billing_river.river_job WHERE kind = ANY($1)`, keys(periods))
			require.NoError(t, err)
			buckets := map[string]int{}
			for rows.Next() {
				var kind string
				var at time.Time
				require.NoError(t, rows.Scan(&kind, &at))
				buckets[kind+" from "+at.Truncate(periods[kind]).UTC().String()]++
			}
			require.NoError(t, rows.Err())
			for bucket, n := range buckets {
				require.Equal(t, 1, n, "%s queued once", bucket)
			}
		}
		oncePerPeriod()
		var before int64
		require.NoError(t, admin.QueryRow(t.Context(), `SELECT coalesce(max(id), 0) FROM billing_river.river_job`).Scan(&before))
		instances[0].close(t)
		require.Eventually(t, func() bool { id := leader(); return id != "" && id != first }, 30*time.Second, 20*time.Millisecond, "another replica leads")
		// The passes a new leader repeats on start show its schedule is running.
		repeated := []string{riverjobs.DunningArgs{}.Kind(), riverjobs.JobRescueArgs{}.Kind(), riverjobs.ProviderRefreshArgs{}.Kind()}
		require.Eventually(t, func() bool {
			return count(`SELECT count(*) FROM billing_river.river_job WHERE kind = ANY($1) AND id > $2`, repeated, before) > 0
		}, 60*time.Second, 50*time.Millisecond, "the new leader runs its start-up passes")
		oncePerPeriod()
	})
}

// oneMinute waits out a minute with less than burst left: rate limits count
// clock minutes, and a burst straddling two would start over halfway.
func oneMinute(t *testing.T, burst time.Duration) {
	t.Helper()
	if left := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); left < burst {
		time.Sleep(left + 100*time.Millisecond)
	}
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
