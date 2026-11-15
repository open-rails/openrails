//go:build integration

package critical_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"
)

func TestBaselineAdoption(t *testing.T) {
	t.Run("OR-J31-S07/replaced-chain-adopts-the-baseline-and-a-partial-one-is-refused", baselineAdoption)
}

// retiredChain is the migration chain the single baseline replaced.
func retiredChain(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../internal/migrate/retired.txt")
	require.NoError(t, err)
	var names []string
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "release ") {
			names = append(names, line)
		}
	}
	require.Greater(t, len(names), 1)
	return names
}

func baselineAdoption(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := "critical_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `DELETE FROM public.migrations WHERE schema = $1`, schema)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `DELETE FROM public.migration_repairs WHERE schema = $1`, schema)
		require.NoError(t, err)
		pool.Close()
	})
	cfg := openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}
	require.NoError(t, openrails.Migrate(ctx, pool, cfg))

	const ledgerRows = `SELECT sequence, filename, content_sha256 FROM public.migrations
	 WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 ORDER BY sequence`
	type row struct {
		Sequence int64
		Filename string
		Digest   string
	}
	read := func() []row {
		rows, err := pool.Query(ctx, ledgerRows, schema)
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		require.NoError(t, err)
		return out
	}
	baseline := read()
	require.Len(t, baseline, 1)
	var book string
	require.NoError(t, pool.QueryRow(ctx, "SELECT book_id::text FROM "+pgx.Identifier{schema, "book_identity"}.Sanitize()).Scan(&book))

	// A database the replaced chain built: the same schema, with a ledger
	// naming every file of that chain.
	chain := retiredChain(t)
	_, err = pool.Exec(ctx, `UPDATE public.migrations SET content_sha256 = 'replaced', semantic_sha256 = 'replaced'
	 WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 AND sequence = 1`, schema)
	require.NoError(t, err)
	record := func(name string) {
		seq, err := migratekit.Sequence(name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO public.migrations (app, database, schema, sequence, filename, content_sha256, semantic_sha256)
		 VALUES ('openrails', 'postgres', $1, $2, $3, 'replaced', 'replaced')`, schema, seq, name)
		require.NoError(t, err)
	}
	for _, name := range chain[1 : len(chain)-1] {
		record(name)
	}

	// One migration short of the chain's end: refused, the ledger untouched.
	err = openrails.Migrate(ctx, pool, cfg)
	require.ErrorContains(t, err, "short of its end")
	require.ErrorContains(t, err, chain[len(chain)-1])
	require.Len(t, read(), len(chain)-1)

	// The whole chain: the ledger becomes the baseline, nothing runs again,
	// and every change is audited.
	record(chain[len(chain)-1])
	require.NoError(t, openrails.Migrate(ctx, pool, cfg))
	require.Equal(t, baseline, read())
	var audited int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM public.migration_repairs
	 WHERE app = 'openrails' AND database = 'postgres' AND schema = $1 AND verb = 'convert'`, schema).Scan(&audited))
	require.Equal(t, len(chain), audited)
	var after string
	require.NoError(t, pool.QueryRow(ctx, "SELECT book_id::text FROM "+pgx.Identifier{schema, "book_identity"}.Sanitize()).Scan(&after))
	require.Equal(t, book, after)
	require.NoError(t, openrails.Migrate(ctx, pool, cfg))
	require.Equal(t, baseline, read())
}
