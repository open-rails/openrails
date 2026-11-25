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

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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
	// posture is the declared merchants' write posture once the engine is
	// up: readonly unless a scenario sets full (fullWrites) or limited.
	posture string
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
	w := &world{pool: pool, schema: "critical_" + strings.ReplaceAll(uuid.NewString(), "-", ""), clock: clockwork.NewFakeClockAt(time.Now().UTC()), posture: "readonly"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	w.cfg = openrails.Config{
		Database:      billing.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema},
		ReturnOrigins: []string{"https://critical.invalid"},
		Merchants: []billing.MerchantDeclaration{
			{MerchantDocument: billing.MerchantDocument{Slug: "alpha", DisplayName: "Alpha"}},
			{MerchantDocument: billing.MerchantDocument{Slug: "beta", DisplayName: "Beta"}},
		},
	}
	w.deps = openrails.Deps{Postgres: pool, Clock: w.clock, FXTransport: fixedFX(t), ClickHouse: criticalClickHouse(t, w.schema)}
	w.cfg.ClickHouse.Database = w.schema
	for _, change := range configure {
		change(w)
	}
	w.engine, err = openrails.New(t.Context(), w.cfg, w.deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.engine.Close(context.Background())) })
	w.alpha, err = declaredMerchant(w.engine, "alpha")
	require.NoError(t, err)
	w.beta, err = declaredMerchant(w.engine, "beta")
	require.NoError(t, err)
	w.local, err = w.engine.LocalMerchant(w.alpha.MerchantID())
	require.NoError(t, err)
	if w.posture != "full" {
		setWritePosture(t, w, w.posture, declaredMerchants(w)...)
	}
	return w
}

// declaredMerchants is the world's declared merchants. A declaration refused
// (a slug declared twice) has no merchant.
func declaredMerchants(w *world) []billing.MerchantID {
	var declared []billing.MerchantID
	for _, m := range w.cfg.Merchants {
		if id, err := w.engine.DeclaredMerchant(m.Slug); err == nil {
			declared = append(declared, id)
		}
	}
	return declared
}

// fullWrites leaves the declared merchants' write posture full.
func fullWrites(w *world) { w.posture = "full" }

// setWritePosture stores the merchants' writes as an operator's
// `openrails merchant writes` would.
func setWritePosture(t *testing.T, w *world, mode string, merchants ...billing.MerchantID) {
	t.Helper()
	setControl(t, w, "writes", mode, merchants...)
}

// setControl stores one of the merchants' controls (writes, rebills_held or
// destructive_armed) as fixture setup, outside the history an operator's
// change records.
func setControl(t *testing.T, w *world, column string, value any, merchants ...billing.MerchantID) {
	t.Helper()
	require.Contains(t, []string{"writes", "rebills_held", "destructive_armed"}, column)
	table := pgx.Identifier{w.schema, "merchant_controls"}.Sanitize()
	for _, id := range merchants {
		_, err := w.pool.Exec(t.Context(), "INSERT INTO "+table+" (merchant_id, writes, rebills_held, destructive_armed) VALUES ($1, 'full', false, false) ON CONFLICT (merchant_id) DO NOTHING", id.UUID())
		require.NoError(t, err)
		_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET "+column+" = $2 WHERE merchant_id = $1", id.UUID(), value)
		require.NoError(t, err)
	}
}

// criticalClickHouse is a connection to the owned ClickHouse; the world's
// event store is database, dropped with the world.
func criticalClickHouse(t *testing.T, database string) driver.Conn {
	t.Helper()
	addr := os.Getenv("OPENRAILS_CRITICAL_CLICKHOUSE_ADDR")
	require.NotEmpty(t, addr, "OPENRAILS_CRITICAL_CLICKHOUSE_ADDR must name a disposable ClickHouse; critical scenarios never skip")
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, conn.Exec(ctx, "DROP DATABASE IF EXISTS `"+database+"` SYNC"))
		require.NoError(t, conn.Close())
	})
	return conn
}

// withoutEventStore runs the world without ClickHouse: usage is off.
func withoutEventStore(w *world) { w.deps.ClickHouse = nil }

// declaredMerchant is the handle of the merchant engine declares under slug.
func declaredMerchant(engine *openrails.Engine, slug string) (*openrails.Client, error) {
	id, err := engine.DeclaredMerchant(slug)
	if err != nil {
		return nil, err
	}
	return engine.Merchant(id)
}

// The only substituted dependency here is the external FX wire. Every known
// currency has an explicitly fixed unit exchange rate. Financial assertions
// use independent literals; none are calculated from these runtime results.
func fixedFX(t *testing.T) http.RoundTripper {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		rates := map[string]int{}
		for _, c := range catalog.Currencies() {
			rates[strings.ToLower(string(c.Code))] = 1
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

// billedTo is a new card's billing details as its form collects them: the
// country, and the postal code where it asks for one.
func billedTo(country, postal string) *billing.BillingDetails {
	details := &billing.BillingDetails{Name: ptr("Alice Buyer"), Address: &billing.BillingAddress{}}
	if country != "" {
		details.Address.Country = ptr(country)
	}
	if postal != "" {
		details.Address.PostalCode = ptr(postal)
	}
	return details
}

// cfgMerchant is a copy of cfg's declaration of slug; the bare slug when it
// declares none.
func cfgMerchant(cfg openrails.Config, slug string) billing.MerchantDeclaration {
	for _, m := range cfg.Merchants {
		if m.Slug == slug {
			return m
		}
	}
	return billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{Slug: slug}}
}

// setCfgMerchant replaces cfg's declaration of slug with m, or adds it.
func setCfgMerchant(cfg *openrails.Config, slug string, m billing.MerchantDeclaration) {
	m.Slug = slug
	for i := range cfg.Merchants {
		if cfg.Merchants[i].Slug == slug {
			cfg.Merchants[i] = m
			return
		}
	}
	cfg.Merchants = append(cfg.Merchants, m)
}

func balance(t *testing.T, client *openrails.Client, customer billing.CustomerID, available, held, posted int64) {
	t.Helper()
	b, err := client.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, customer, b.CustomerID)
	require.Equal(t, catalog.Currency("USD"), b.Currency)
	require.Equal(t, available, b.AvailableAmount)
	require.Equal(t, held, b.HeldAmount)
	require.Equal(t, posted, b.BalanceAmount)
}
