//go:build e2e && integration

package ci_test

// The server suite's own copy of the few root e2e helpers it uses (ci/), so
// neither module exports test fixtures to the other.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

type fixture struct {
	pool   *pgxpool.Pool
	schema string
}

// newFixture migrates OpenRails into a fresh schema of OPENRAILS_E2E_DSN; the
// schema and its AuthKit sibling are dropped at cleanup.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	if dsn == "" {
		t.Fatal("OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(t.Context()))

	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, schema := range []string{f.schema, f.authSchema()} {
			_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
		f.pool.Close()
	})
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, client.Close(t.Context()))
	return f
}

func (f *fixture) config() openrails.Config {
	return openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: f.schema, RiverSchema: f.schema},
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesReadOnly,
		ReturnOrigins:     []string{"https://e2e.test"},
	}
}

// runtime is an embedded host's engine declaring merchant slug, closed at
// cleanup, and that merchant's handle.
func (f *fixture) runtime(t *testing.T, slug string) (*openrails.Engine, *openrails.Client) {
	t.Helper()
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug}
	return f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
}

// boot builds an engine, closed at cleanup, and the handle of the merchant
// cfg declares.
func (f *fixture) boot(t *testing.T, cfg openrails.Config, deps openrails.Deps) (*openrails.Engine, *openrails.Client) {
	t.Helper()
	bill, err := openrails.New(t.Context(), cfg, deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	shop, err := bill.MerchantBySlug(cfg.Merchant.Slug)
	require.NoError(t, err)
	return bill, shop
}

// adminGroups turns on the admin group alone.
var adminGroups = openrails.RouteGroups{Admin: true}

// rescueClock is River's test clock pinned at one instant.
type rescueClock struct{ at time.Time }

func (c rescueClock) Now() time.Time       { return c.at }
func (c rescueClock) NowOrNil() *time.Time { return &c.at }

const transitKey = "e2e-solana"

// probe runs the engine's readiness probe called name.
func probe(t *testing.T, rt *openrails.Engine, name string) error {
	t.Helper()
	for _, p := range rt.Probes() {
		if p.Name == name {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			return p.Check(ctx)
		}
	}
	t.Fatalf("probe %s not registered", name)
	return nil
}
