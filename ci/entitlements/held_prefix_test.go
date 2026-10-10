//go:build e2e && integration

package entitlements_test

import (
	"context"
	"errors"
	"fmt"
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

// statements records the SQL, last arguments and run count of each sqlc
// statement by its "-- name:".
type statements struct {
	mu    sync.Mutex
	sql   map[string]string
	args  map[string][]any
	count map[string]int
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if _, rest, ok := strings.Cut(data.SQL, "-- name: "); ok && !strings.HasPrefix(data.SQL, "EXPLAIN") {
		name, _, _ := strings.Cut(rest, " ")
		s.mu.Lock()
		s.sql[name], s.args[name] = data.SQL, data.Args
		s.count[name]++
		s.mu.Unlock()
	}
	return ctx
}

// ran is how many times each statement ran since the last call.
func (s *statements) ran() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.count
	s.count = map[string]int{}
	return out
}

func (*statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type fixture struct {
	t        *testing.T
	client   *openrails.Client
	clock    *clockwork.FakeClock
	pool     *pgxpool.Pool
	schema   string
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
	f := &fixture{t: t, clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second)), executed: &statements{sql: map[string]string{}, args: map[string][]any{}, count: map[string]int{}}}
	config.ConnConfig.Tracer = f.executed
	f.pool, err = pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	schema := "e2e_ent_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	f.schema = schema
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		f.pool.Close()
	})
	cfg := openrails.Config{
		Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
		Merchant: openrails.MerchantDeclaration{Slug: "held-" + uuid.NewString()[:8], DisplayName: "Held"},
	}
	f.client, err = openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, Clock: f.clock})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.client.Close(context.Background())) })
	return f
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

// A read answers exact keys and the keys held under a byte prefix at one
// instant: keys of products whose windows ended, were revoked or have not
// started stay out, the range is bytes (the next byte bounds it), pages
// continue in byte order, and the read walks the customer's products, then
// their keys.
func TestHeldPrefixesAnswerExactByteRanges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	products := 0
	grant := func(key string, ends *time.Time) billing.ProductAccessID {
		t.Helper()
		products++
		product, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: fmt.Sprintf("key-%d", products), DisplayName: key, Entitlements: []string{key}})
		require.NoError(t, err)
		access, err := f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, EndsAt: ends}}})
		require.NoError(t, err)
		return access[0].ID
	}
	start := f.clock.Now()
	for _, key := range []string{"content:t:post:3", "content:t:post:1", "content:t:post:2", "members:t:channel:a", "premium",
		"content:t:post;x", "content:t:postx", "content:t:post", "content:u:post:1"} {
		grant(key, nil)
	}
	ended := start.Add(30 * time.Minute)
	grant("content:t:post:expired", &ended)
	require.NoError(t, f.client.DeleteProductAccess(ctx, customer, grant("content:t:post:revoked", nil)))
	f.clock.Advance(time.Hour)
	grant("content:t:post:future", nil)
	at := start.Add(45 * time.Minute)

	under := func(c billing.CustomerID, prefix string, at time.Time, page billing.PageRequest) *billing.ListPage[billing.CustomerEntitlement] {
		t.Helper()
		got, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{c}, Prefix: prefix, At: at, PageRequest: page})
		require.NoError(t, err)
		return got
	}
	keys := func(page *billing.ListPage[billing.CustomerEntitlement]) []string {
		out := []string{}
		for _, row := range page.Items {
			out = append(out, row.Entitlement)
		}
		return out
	}
	posts, channels := "content:t:post:", "members:t:channel:"
	exact, err := heldKeys(ctx, f.client, customer, at, "premium", "content:t:post:1", "gold")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"premium": true, "content:t:post:1": true, "gold": false}, exact)
	require.Equal(t, []string{"content:t:post:1", "content:t:post:2", "content:t:post:3"}, keys(under(customer, posts, at, billing.PageRequest{})),
		"expired, revoked and future windows, and keys outside the byte range, are not held")
	require.Equal(t, []string{"members:t:channel:a"}, keys(under(customer, channels, at, billing.PageRequest{})))
	require.Empty(t, under(customer, "nothing:", at, billing.PageRequest{}).Items)
	require.Empty(t, under(customer, "t~", at, billing.PageRequest{}).Items)
	filtered, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customer}, Prefix: posts, Entitlements: []string{"content:t:post:2", "premium"}, At: at})
	require.NoError(t, err)
	require.Equal(t, []string{"content:t:post:2"}, keys(filtered), "named keys and a prefix both filter")

	require.Equal(t, []string{"content:t:post:1", "content:t:post:2", "content:t:post:3", "content:t:post:future"}, keys(under(customer, posts, time.Time{}, billing.PageRequest{})), "a zero At is the clock's now")

	first := under(customer, posts, at, billing.PageRequest{Limit: 2})
	require.Equal(t, []string{"content:t:post:1", "content:t:post:2"}, keys(first))
	require.NotEmpty(t, first.Next, "a full page continues")
	rest := under(customer, posts, at, billing.PageRequest{Limit: 2, Cursor: first.Next})
	require.Equal(t, []string{"content:t:post:3"}, keys(rest))
	require.Empty(t, rest.Next)

	require.Empty(t, under(billing.CustomerID(uuid.New()), posts, time.Time{}, billing.PageRequest{}).Items, "another customer's keys never answer")

	for _, prefix := range []string{strings.Repeat("x", 257), "content:\x00:", "content: ", "content:\x7f", "content:é"} {
		_, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customer}, Prefix: prefix})
		invalidParam(t, err, "prefix")
	}
}

// Holders page by whole customers: one with several live windows of a
// product is one row with the most seats a window gives, and never cuts a
// page short of the holders after it.
func TestHoldersPageWholeCustomers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a := billing.CustomerID(uuid.MustParse("00000000-0000-4000-8000-00000000000a"))
	b := billing.CustomerID(uuid.MustParse("00000000-0000-4000-8000-00000000000b"))
	c := billing.CustomerID(uuid.MustParse("00000000-0000-4000-8000-00000000000c"))
	for _, id := range []billing.CustomerID{a, b, c} {
		_, err := f.client.UpdateCustomer(ctx, id, billing.UpdateCustomerParams{})
		require.NoError(t, err)
	}
	s := pgx.Identifier{f.schema}.Sanitize()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('openrails.catalog_batch_merchant_id', $1, true)", f.client.MerchantID().String())
	require.NoError(t, err)
	from := f.clock.Now().Add(-time.Hour)
	// a holds three windows of the first product (2 seats, 5 seats, none), b
	// one of it, c one of the second; both products grant team:k.
	_, err = tx.Exec(ctx, `WITH p AS (
		INSERT INTO `+s+`.products (merchant_id, id, key, display_name, tier_rank, archived)
		VALUES ($1, uuidv7(), 'team-1', 'Team 1', 0, false), ($1, uuidv7(), 'team-2', 'Team 2', 0, false)
		RETURNING id, key
	), k AS (
		INSERT INTO `+s+`.product_entitlements (merchant_id, product_id, entitlement, added_at, added_by)
		SELECT $1, p.id, 'team:k', $5, 'seed' FROM p RETURNING 1
	), w AS (
		SELECT p.id AS product_id, w.customer_id, w.quantity FROM p
		JOIN (VALUES ('team-1', $2::uuid, 2), ('team-1', $2::uuid, 5), ('team-1', $2::uuid, NULL), ('team-1', $3::uuid, NULL), ('team-2', $4::uuid, NULL))
			AS w(key, customer_id, quantity) ON w.key = p.key
	), gr AS (
		INSERT INTO `+s+`.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, event, starts_at, actor, grant_reason, quantity)
		SELECT $1, w.customer_id, w.product_id, 'access', 'grant', 'seed:' || gen_random_uuid(), 'grant', $5, 'seed', 'comp', w.quantity FROM w
		RETURNING id, customer_id, product_id, source_id, quantity
	)
	INSERT INTO `+s+`.product_access (merchant_id, customer_id, product_id, grant_id, source_type, source_id, starts_at, quantity)
	SELECT $1, gr.customer_id, gr.product_id, gr.id, 'grant', gr.source_id, $5, gr.quantity FROM gr`,
		f.client.MerchantID().UUID(), a.UUID(), b.UUID(), c.UUID(), from)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var holders []billing.CustomerEntitlement
	for cursor := ""; ; {
		page, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{Entitlements: []string{"team:k"}, PageRequest: billing.PageRequest{Limit: 2, Cursor: cursor}})
		require.NoError(t, err)
		holders = append(holders, page.Items...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	five := 5
	require.Equal(t, []billing.CustomerEntitlement{
		{CustomerID: a, Entitlement: "team:k", Quantity: &five},
		{CustomerID: b, Entitlement: "team:k"},
		{CustomerID: c, Entitlement: "team:k"},
	}, holders)

	held, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{a, b}, Entitlements: []string{"team:k"}})
	require.NoError(t, err)
	require.Equal(t, holders[:2], held.Items, "the batch read answers the same seats")
	all, err := f.client.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{a}})
	require.NoError(t, err)
	require.Equal(t, holders[:1], all.Items, "and so does a page of a customer's keys")
}
