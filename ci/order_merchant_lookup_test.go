//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
)

// lastRun records the SQL and arguments a named sqlc statement last ran with.
type lastRun struct {
	name string
	mu   sync.Mutex
	sql  string
	args []any
}

func (l *lastRun) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: "+l.name+" ") && !strings.HasPrefix(data.SQL, "EXPLAIN") {
		l.mu.Lock()
		l.sql, l.args = data.SQL, data.Args
		l.mu.Unlock()
	}
	return ctx
}

func (*lastRun) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A wallet's Solana Pay request names only its order, so the route finds the
// order's merchant by id alone. That is one probe of orders_id_idx however
// many merchants the database holds: never a walk of the primary key, which
// searches once per merchant.
func TestOrderMerchantLookupUsesItsIndex(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	lookup := &lastRun{name: "GetOrderMerchant"}
	poolConfig, err := pgxpool.ParseConfig(f.dsn(t))
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = lookup
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	client, err := openrails.New(ctx, f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	// 400 merchants, each with one customer and 25 orders.
	schema := pgx.Identifier{f.schema}.Sanitize()
	_, err = pool.Exec(ctx, `WITH m AS (
			INSERT INTO `+schema+`.merchants (slug, status) SELECT 'lookup-' || g, 'active' FROM generate_series(1, 400) g RETURNING id),
		c AS (INSERT INTO `+schema+`.customers (merchant_id) SELECT id FROM m RETURNING merchant_id, id)
		INSERT INTO `+schema+`.orders (merchant_id, customer_id, origin, status, payment_status, currency, total, expires_at)
		SELECT c.merchant_id, c.id, 'customer', 'open', 'requires_payment_method', 'USD', 1000000, now() + interval '1 hour'
		FROM c, generate_series(1, 25)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ANALYZE "+schema+".merchants, "+schema+".orders")
	require.NoError(t, err)
	var order, owner uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, "SELECT id, merchant_id FROM "+schema+".orders ORDER BY merchant_id, id OFFSET 5000 LIMIT 1").Scan(&order, &owner))

	owners, err := engine.Graph(client).Runtime.DB.GenDirectory().GetOrderMerchant(ctx, order)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{owner}, owners)

	// The statement as the engine ran it, with its own argument.
	lookup.mu.Lock()
	sql, args := lookup.sql, lookup.args
	lookup.mu.Unlock()
	require.NotEmpty(t, sql, "GetOrderMerchant never ran")
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	var indexes []string
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "orders" {
			name, _ := node["Index Name"].(string)
			indexes = append(indexes, name)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if c, ok := child.(map[string]any); ok {
				walk(c)
			}
		}
	}
	walk(plans[0].Plan)
	require.Equal(t, []string{"orders_id_idx"}, indexes, "%s", raw)
	hit, _ := plans[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := plans[0].Plan["Shared Read Blocks"].(float64)
	require.LessOrEqual(t, int(hit+read), 16, "the lookup's work does not grow with the merchants: %s", raw)
}
