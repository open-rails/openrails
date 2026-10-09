//go:build e2e && integration

package ci_test

// The server suite's own copy of the few root e2e helpers it uses (ci/), so
// neither module exports test fixtures to the other.

import (
	"context"
	"net/http"
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
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: pool})
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

func (f *fixture) runtime(t *testing.T, slug string) *openrails.Client {
	t.Helper()
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	return client
}

// perm is a test host's own permission.
type perm string

func (p perm) String() string { return string(p) }

// staffPermissions give every bundle its own permission.
var staffPermissions = openrails.Permissions{AdminRead: perm("host:billing:read"), AdminWrite: perm("host:billing:write"), CatalogWrite: perm("host:catalog:write"), MerchantConfig: perm("host:billing:admin")}

// adminPermissions mount the admin bundle alone.
var adminPermissions = openrails.Permissions{AdminRead: staffPermissions.AdminRead, AdminWrite: staffPermissions.AdminWrite}

// hostKey admits every request as the host backend's API key.
type hostKey struct{}

func pass(next http.Handler) http.Handler { return next }

func (hostKey) Required() func(http.Handler) http.Handler                { return pass }
func (hostKey) RequirePermission(string) func(http.Handler) http.Handler { return pass }
func (hostKey) Sensitive() func(http.Handler) http.Handler               { return pass }
func (hostKey) Identity(context.Context) (openrails.Identity, bool) {
	return openrails.Identity{Issuer: "test", Subject: "test-host", SubjectKind: openrails.SubjectApplication,
		Invoker: openrails.Invoker{Issuer: "test", ID: "test-host"}, Credential: openrails.Credential{Kind: openrails.CredentialAPIKey, ID: "k_test"}}, true
}

// rescueClock is River's test clock pinned at one instant.
type rescueClock struct{ at time.Time }

func (c rescueClock) Now() time.Time       { return c.at }
func (c rescueClock) NowOrNil() *time.Time { return &c.at }

const transitKey = "e2e-solana"

// probe runs the client's readiness probe called name.
func probe(t *testing.T, rt *openrails.Client, name string) error {
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
