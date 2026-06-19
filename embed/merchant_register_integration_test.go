//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
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
		Options:  embedded.Options{Config: cfg},
		Merchant: slug,
		MerchantConfiguration: openrails.MerchantConfigurationInput{
			Profile: &openrails.MerchantProfileInput{
				FromEmail:  "billing@example.com",
				SupportURL: "https://example.com/support",
			},
			DelegatedInvokerWastedSpendWindows: []openrails.BudgetWindowInput{
				{Key: "burst", WindowSeconds: 900, Limit: 123, Currency: money.DefaultCurrency},
			},
		},
	})
	require.NoError(t, err, "embed.New with an unprovisioned merchant slug must NOT panic/error")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// The merchant row now exists and is active.
	var (
		gotSlug   string
		gotStatus string
	)
	err = pool.QueryRow(ctx,
		`SELECT slug, status FROM openrails.merchants WHERE slug = $1`, slug,
	).Scan(&gotSlug, &gotStatus)
	require.NoError(t, err, "bound merchant row must exist after embed.New")
	require.Equal(t, slug, gotSlug)
	require.Equal(t, "active", gotStatus)

	var rawConfig []byte
	err = pool.QueryRow(ctx, `
		SELECT mc.config
		FROM openrails.merchant_configurations mc
		JOIN openrails.merchants m ON m.id = mc.merchant_id
		WHERE m.slug = $1
	`, slug).Scan(&rawConfig)
	require.NoError(t, err, "merchant configuration must be seeded")
	var gotConfig models.MerchantConfiguration
	require.NoError(t, json.Unmarshal(rawConfig, &gotConfig))
	require.Equal(t, slug, gotConfig.Profile.DisplayName, "embedded startup matches manifest display-name fallback")
	require.Equal(t, "billing@example.com", gotConfig.Profile.FromEmail)
	require.Equal(t, "https://example.com/support", gotConfig.Profile.SupportURL)
	require.Len(t, gotConfig.DelegatedInvokerWastedSpendWindows, 1)
	require.Equal(t, int64(123), gotConfig.DelegatedInvokerWastedSpendWindows[0].Limit)

	// The HTTP handler builds — merchant resolution succeeds (would have panicked
	// pre-#480 on an unprovisioned slug).
	h := rt.Handler(embed.HandlerOptions{})
	require.NotNil(t, h)

	// Second New on the SAME slug is an idempotent no-op (INSERT ... ON CONFLICT
	// DO NOTHING): boots fine, exactly one row.
	rt2, err := embed.New(ctx, embed.Options{
		Options:  embedded.Options{Config: &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}},
		Merchant: slug,
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
