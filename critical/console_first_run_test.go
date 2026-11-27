//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The console the openrails binary serves, driven in Chromium
// (web/console/critical): a fresh server's first run.
func TestConsoleFirstRun(t *testing.T) {
	t.Run("OR-J01-S41/the-setup-link-creates-the-owner-then-the-first-merchant-and-asks-for-a-payment-provider", func(t *testing.T) {
		w := bareWorld(t)
		keys := staffKeys(t)
		base, log, _ := runServerAt(t, w, keys, true)
		s := &staffServer{base: base, log: log}
		require.Eventually(t, func() bool { return strings.Contains(log.String(), "OpenRails background work running") }, 90*time.Second, 200*time.Millisecond, "the server starts its background work: %s", log)
		links := s.setupLinks()
		require.Len(t, links, 1, "a fresh server logs one setup link: %s", log)
		require.True(t, strings.HasPrefix(links[0], base+"/admin/setup#token="), links[0])

		directory, err := filepath.Abs("../web/console")
		require.NoError(t, err)
		playwright(t, directory, "playwright.config.ts", []string{"OPENRAILS_BROWSER_URL=" + base, "OPENRAILS_SETUP_LINK=" + links[0]},
			"first-run-owner-then-merchant-then-payment-provider")

		out, err := staffCLI(t, w, keys, nil, "merchants", "get", "first-shop").Output()
		require.NoError(t, err, string(out))
		var merchant billing.Merchant
		require.NoError(t, json.Unmarshal(out, &merchant))
		require.Equal(t, "first-shop", merchant.Slug)
		require.Equal(t, billing.MerchantActive, merchant.Status)
		require.Empty(t, merchant.RailsArmed, "nothing is armed until the operator declares a PSP")
	})
}

// bareWorld is a schema nothing has touched: a fresh server's database. The
// server migrates it.
func bareWorld(t *testing.T) *world {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database")
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	w := &world{pool: pool, schema: criticalSchema()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema}.Sanitize()+", "+pgx.Identifier{w.schema + "_river"}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	return w
}
