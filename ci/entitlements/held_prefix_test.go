//go:build e2e && integration

package entitlements_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// statements records the SQL of each sqlc statement by its "-- name:".
type statements struct {
	mu  sync.Mutex
	sql map[string]string
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if _, rest, ok := strings.Cut(data.SQL, "-- name: "); ok {
		name, _, _ := strings.Cut(rest, " ")
		s.mu.Lock()
		s.sql[name] = data.SQL
		s.mu.Unlock()
	}
	return ctx
}

func (*statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type fixture struct {
	t        *testing.T
	client   *openrails.Client
	clock    *clockwork.FakeClock
	pool     *pgxpool.Pool
	executed *statements
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	if dsn == "" {
		t.Fatal("OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	f := &fixture{t: t, clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second)), executed: &statements{sql: map[string]string{}}}
	config.ConnConfig.Tracer = f.executed
	f.pool, err = pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	schema := "e2e_ent_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		f.pool.Close()
	})
	cfg := openrails.Config{
		Schema: schema, RiverSchema: schema, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
		Merchant: openrails.MerchantDeclaration{Slug: "held-" + uuid.NewString()[:8], DisplayName: "Held"},
	}
	require.NoError(t, openrails.Migrate(t.Context(), f.pool, cfg))
	f.client, err = openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, Clock: f.clock})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.client.Close(context.Background())) })
	return f
}

// plan is the generic plan of an executed statement with sequential scans
// off, as TestQueryAudit plans it.
func (f *fixture) plan(name string) string {
	f.t.Helper()
	f.executed.mu.Lock()
	sql := f.executed.sql[name]
	f.executed.mu.Unlock()
	require.NotEmpty(f.t, sql, "%s never ran", name)
	conn, err := f.pool.Acquire(f.t.Context())
	require.NoError(f.t, err)
	defer conn.Release()
	tx, err := conn.Begin(f.t.Context())
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(f.t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(f.t, err)
	results, err := tx.Conn().PgConn().Exec(f.t.Context(), "EXPLAIN (GENERIC_PLAN) "+sql).ReadAll()
	require.NoError(f.t, err)
	var lines []string
	for _, result := range results {
		for _, row := range result.Rows {
			lines = append(lines, string(row[0]))
		}
	}
	return strings.Join(lines, "\n")
}

func invalidParam(t *testing.T, err error, param string) {
	t.Helper()
	var refusal *billing.StatusError
	require.True(t, errors.As(err, &refusal), "%v", err)
	require.Equal(t, billing.CodeInvalidParam, refusal.Code)
	if param == "" {
		return
	}
	require.NotNil(t, refusal.Param, "%v", err)
	require.Equal(t, param, *refusal.Param)
}

// A check answers exact keys and the keys held under each byte prefix at one
// instant: windows that ended, were revoked or have not started stay out, the
// range is bytes (the next byte bounds it), the limit truncates, and the read
// uses the keyspace index.
func TestHeldPrefixesAnswerExactByteRanges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := f.client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	grant := func(key string, ends *time.Time) billing.EntitlementID {
		t.Helper()
		record, err := f.client.CreateEntitlement(ctx, customer, billing.CreateEntitlementParams{Entitlement: key, EndsAt: ends})
		require.NoError(t, err)
		return record.ID
	}
	start := f.clock.Now()
	for _, key := range []string{"content:t:post:3", "content:t:post:1", "content:t:post:2", "members:t:channel:a", "premium",
		"content:t:post;x", "content:t:postx", "content:t:post", "content:u:post:1"} {
		grant(key, nil)
	}
	ended := start.Add(30 * time.Minute)
	grant("content:t:post:expired", &ended)
	require.NoError(t, f.client.DeleteEntitlement(ctx, customer, grant("content:t:post:revoked", nil)))
	f.clock.Advance(time.Hour)
	grant("content:t:post:future", nil)
	at := start.Add(45 * time.Minute)

	posts, channels := "content:t:post:", "members:t:channel:"
	got, err := f.client.CheckEntitlements(ctx, customer, billing.CheckEntitlementsParams{
		Entitlements: []string{"premium", "content:t:post:1", "gold"},
		Prefixes:     []string{posts, channels, "nothing:", "t~"},
		At:           at,
	})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"premium": true, "content:t:post:1": true, "gold": false}, got.Entitlements)
	require.Equal(t, map[string]billing.HeldEntitlements{
		posts:      {Keys: []string{"content:t:post:1", "content:t:post:2", "content:t:post:3"}},
		channels:   {Keys: []string{"members:t:channel:a"}},
		"nothing:": {Keys: []string{}},
		"t~":       {Keys: []string{}},
	}, got.Held, "expired, revoked and future windows, and keys outside the byte range, are not held")

	now, err := f.client.CheckEntitlements(ctx, customer, billing.CheckEntitlementsParams{Prefixes: []string{posts}})
	require.NoError(t, err)
	require.Empty(t, now.Entitlements)
	require.Equal(t, []string{"content:t:post:1", "content:t:post:2", "content:t:post:3", "content:t:post:future"}, now.Held[posts].Keys, "a zero At is the clock's now")

	truncated, err := f.client.CheckEntitlements(ctx, customer, billing.CheckEntitlementsParams{Prefixes: []string{posts, channels}, PrefixLimit: 2, At: at})
	require.NoError(t, err)
	require.Equal(t, billing.HeldEntitlements{Keys: []string{"content:t:post:1", "content:t:post:2"}, Truncated: true}, truncated.Held[posts])
	require.Equal(t, billing.HeldEntitlements{Keys: []string{"members:t:channel:a"}}, truncated.Held[channels])

	other, err := f.client.CheckEntitlements(ctx, billing.CustomerID(uuid.New()), billing.CheckEntitlementsParams{Prefixes: []string{posts}})
	require.NoError(t, err)
	require.Equal(t, billing.HeldEntitlements{Keys: []string{}}, other.Held[posts], "another customer's keys never answer")

	require.Contains(t, f.plan("ListHeldEntitlementsByPrefix"), "entitlements_customer_keyspace_idx")

	tooMany := make([]string, billing.MaxEntitlementPrefixes+1)
	for i := range tooMany {
		tooMany[i] = "p" + strings.Repeat("x", i) + ":"
	}
	for _, refused := range []struct {
		params billing.CheckEntitlementsParams
		param  string
	}{
		{billing.CheckEntitlementsParams{}, ""},
		{billing.CheckEntitlementsParams{Prefixes: tooMany}, ""},
		{billing.CheckEntitlementsParams{Prefixes: []string{posts, posts}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{""}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{strings.Repeat("x", 257)}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{"content:\x00:"}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{"content: "}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{"content:\x7f"}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{"content:é"}}, "prefixes"},
		{billing.CheckEntitlementsParams{Prefixes: []string{posts}, PrefixLimit: -1}, "prefix_limit"},
		{billing.CheckEntitlementsParams{Prefixes: []string{posts}, PrefixLimit: billing.MaxHeldEntitlements + 1}, "prefix_limit"},
	} {
		_, err := f.client.CheckEntitlements(ctx, customer, refused.params)
		invalidParam(t, err, refused.param)
	}
	limit, err := f.client.CheckEntitlements(ctx, customer, billing.CheckEntitlementsParams{Prefixes: []string{posts}, PrefixLimit: billing.MaxHeldEntitlements})
	require.NoError(t, err)
	require.False(t, limit.Held[posts].Truncated)
}
