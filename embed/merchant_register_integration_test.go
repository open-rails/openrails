//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestEmbedNew_RegistersBoundMerchant verifies the #480 embed merchant registration (supersedes #478): embed.New
// bound to a merchant SLUG that does not yet exist BOOTS (no panic), the merchants
// row is registered, the HTTP handler builds (merchant resolution succeeds), and
// a second New on the same slug is an idempotent no-op.
func TestEmbedNew_RegistersBoundMerchant(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	// A slug that is guaranteed NOT to exist yet (fresh-tenant case).
	slug := fmt.Sprintf("embed-register-%d", time.Now().UnixNano())

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: cfg},
		Tenant:  slug,
	})
	require.NoError(t, err, "embed.New with an unprovisioned merchant slug must NOT panic/error")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// The merchant row now exists, active, name == slug.
	var (
		gotSlug   string
		gotName   string
		gotStatus string
	)
	err = pool.QueryRow(ctx,
		`SELECT slug, name, status FROM openrails.merchants WHERE slug = $1`, slug,
	).Scan(&gotSlug, &gotName, &gotStatus)
	require.NoError(t, err, "bound merchant row must exist after embed.New")
	require.Equal(t, slug, gotSlug)
	require.Equal(t, slug, gotName)
	require.Equal(t, "active", gotStatus)

	// The HTTP handler builds — merchant resolution succeeds (would have panicked
	// pre-#480 on an unprovisioned slug).
	h := rt.Handler(embed.HandlerOptions{})
	require.NotNil(t, h)

	// Second New on the SAME slug is an idempotent no-op (INSERT ... ON CONFLICT
	// DO NOTHING): boots fine, exactly one row.
	rt2, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}},
		Tenant:  slug,
	})
	require.NoError(t, err, "second embed.New on the same slug must be idempotent")
	t.Cleanup(func() { _ = rt2.Close(context.Background()) })

	var count int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.merchants WHERE slug = $1`, slug,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "ensure is idempotent — exactly one merchant row")
}
