//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// An upgrade keeps every usage receipt an earlier build wrote, one per
// pricing period, in the shape a receipt per posting has.
func TestReceiptMigration(t *testing.T) {
	t.Run("OR-J24-S91/an-old-receipts-forgiveness-is-shared-over-its-lines-by-their-amounts", oldReceiptsForgiveness)
}

func oldReceiptsForgiveness(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := criticalSchema()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	logger := slog.New(slog.DiscardHandler)
	// The chain an earlier build shipped: everything before receipts per
	// posting.
	earlier := fstest.MapFS{}
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	for _, name := range names {
		if name >= "0009_" {
			continue
		}
		data, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		earlier[name] = &fstest.MapFile{Data: data}
	}
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: earlier, Logger: logger}))

	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	merchant, customer := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, "INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgraded', 'active')", merchant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO "+table("customers")+" (merchant_id, id) VALUES ($1, $2)", merchant, customer)
	require.NoError(t, err)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nov := oct.AddDate(0, 1, 0)
	type line struct {
		MeterKey       string    `json:"meter_key"`
		Amount         int64     `json:"amount"`
		Forgiven       int64     `json:"forgiven"`
		PeriodStartsAt time.Time `json:"period_starts_at"`
		PeriodEndsAt   time.Time `json:"period_ends_at"`
	}
	type receipt struct {
		start            time.Time
		currency, reason string
		amount, forgiven int64
		lines            []line
		wantReason       string
		wantForgiven     []int64
	}
	lines := func(amounts ...int64) []line {
		out := make([]line, len(amounts))
		for i, a := range amounts {
			out[i] = line{MeterKey: string(rune('a' + i)), Amount: a}
		}
		return out
	}
	receipts := []receipt{
		// 5 forgiven over 3, 3 and 4 is 1.5, 1.5 and 2: the unit the floors
		// leave goes to the earlier of the tied halves.
		{oct, "GOLD", "daily", 10, 5, lines(3, 3, 4), "daily", []int64{2, 1, 2}},
		// A reversal took back 4 over its falls of 6 and 4, 2.4 and 1.6,
		// beside a rise of 2 it does not touch.
		{oct, "GOLD", "reversal", -8, -4, lines(-6, 2, -4), "daily", []int64{-2, 0, -2}},
		{nov, "USD", "credit", 7, 0, lines(7), "credit", []int64{0}},
	}
	ids := make([]uuid.UUID, len(receipts))
	for i, r := range receipts {
		raw := make([]map[string]any, len(r.lines))
		for j, l := range r.lines {
			raw[j] = map[string]any{"meter_key": l.MeterKey, "cell": "", "print": "p", "requests": 1, "quantity": 1, "stated": 1, "amount": l.Amount}
		}
		taken, err := json.Marshal(raw)
		require.NoError(t, err)
		var shares []byte
		if r.forgiven != 0 {
			shares = []byte(`[{"invoker": "` + customer.String() + `", "event_type": "e", "meter_key": "a", "cell": "", "amount": 1}]`)
		}
		require.NoError(t, pool.QueryRow(ctx, "INSERT INTO "+table("usage_receipts")+
			" (merchant_id, customer_id, currency, period_starts_at, period_ends_at, cut_at, reason, amount, lines, forgiven, forgiven_lines)"+
			" VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9, $10) RETURNING id",
			merchant, customer, r.currency, r.start, r.start.AddDate(0, 1, 0), r.reason, r.amount, taken, r.forgiven, shares).Scan(&ids[i]))
	}

	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}))
	for i, r := range receipts {
		var reason string
		var amount, forgiven int64
		var taken, charges []byte
		require.NoError(t, pool.QueryRow(ctx, "SELECT reason, amount, forgiven, lines, charges FROM "+table("usage_receipts")+" WHERE merchant_id = $1 AND id = $2",
			merchant, ids[i]).Scan(&reason, &amount, &forgiven, &taken, &charges))
		require.Equal(t, [3]any{r.wantReason, r.amount, r.forgiven}, [3]any{reason, amount, forgiven}, "receipt %d keeps its amounts", i)
		require.JSONEq(t, "[]", string(charges))
		var got []line
		require.NoError(t, json.Unmarshal(taken, &got))
		want := make([]line, len(r.lines))
		for j, l := range r.lines {
			l.Forgiven, l.PeriodStartsAt, l.PeriodEndsAt = r.wantForgiven[j], r.start, r.start.AddDate(0, 1, 0)
			want[j] = l
		}
		for j := range got {
			got[j].PeriodStartsAt, got[j].PeriodEndsAt = got[j].PeriodStartsAt.UTC(), got[j].PeriodEndsAt.UTC()
		}
		require.Equal(t, want, got, "receipt %d: each line in its period, with its share of what the receipt forgave", i)
	}
}
