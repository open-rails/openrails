//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// Every scenario owns its schemas and server. SQL below is fixture lifecycle,
// never an alternate business API or an oracle for application behavior.
type world struct {
	pool   *pgxpool.Pool
	schema string
	clock  *clockwork.FakeClock
	engine *openrails.Engine
	alpha  *openrails.Client
	beta   *openrails.Client
	local  *openrails.LocalClient
	cfg    openrails.Config
	deps   openrails.Deps
}

func newWorld(t *testing.T, configure ...func(*world)) *world {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pc, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pc.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(t.Context()))
	w := &world{pool: pool, schema: "critical_" + strings.ReplaceAll(uuid.NewString(), "-", ""), clock: clockwork.NewFakeClockAt(time.Now().UTC())}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	w.cfg = openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema},
		ProviderWriteMode: openrails.ProviderWritesReadOnly,
		ReturnOrigins:     []string{"https://critical.invalid"},
		Merchants: map[string]openrails.MerchantDeclaration{
			"alpha": {DisplayName: "Alpha"},
			"beta":  {DisplayName: "Beta"},
		},
	}
	w.deps = openrails.Deps{Postgres: pool, Clock: w.clock, FXTransport: fixedFX(t)}
	for _, change := range configure {
		change(w)
	}
	w.engine, err = openrails.New(t.Context(), w.cfg, w.deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.engine.Close(context.Background())) })
	w.alpha, err = w.engine.MerchantBySlug("alpha")
	require.NoError(t, err)
	w.beta, err = w.engine.MerchantBySlug("beta")
	require.NoError(t, err)
	w.local, err = w.engine.LocalMerchant(w.alpha.MerchantID())
	require.NoError(t, err)
	return w
}

// The only substituted dependency here is the external FX wire. Every known
// currency has an explicitly fixed unit exchange rate. Financial assertions
// use independent literals; none are calculated from these runtime results.
func fixedFX(t *testing.T) http.RoundTripper {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		rates := map[string]int{}
		for _, c := range billing.Currencies() {
			rates[strings.ToLower(c.Code)] = 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"date": time.Now().UTC().Format(time.DateOnly), base: rates})
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	wire := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(wire.CloseIdleConnections)
	return loopbackWire{target: target, wire: wire}
}

type loopbackWire struct {
	target *url.URL
	wire   http.RoundTripper
}

func (w loopbackWire) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host, copy.Host = w.target.Scheme, w.target.Host, w.target.Host
	return w.wire.RoundTrip(copy)
}

func ptr[T any](v T) *T { return &v }

func balance(t *testing.T, client *openrails.Client, customer billing.CustomerID, available, held, posted int64) {
	t.Helper()
	b, err := client.GetBalance(t.Context(), customer, "USD")
	require.NoError(t, err)
	require.Equal(t, customer, b.CustomerID)
	require.Equal(t, "USD", b.Currency)
	require.Equal(t, available, b.AvailableAmount)
	require.Equal(t, held, b.HeldAmount)
	require.Equal(t, posted, b.BalanceAmount)
}
