//go:build integration

package metrics_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #796: approval_rate by token_type — the network-token uplift number. Seeds
// its own merchant: pan_via_proxy 2 settled + 2 failed (0.5), psp_token
// 3 settled + 1 failed (0.75), one NULL-token_type row folding to 'unknown'.
func TestMetrics_ApprovalRateByTokenType(t *testing.T) {
	ctx := context.Background()
	m := uuid.New()
	dbi := dbtest.OpenMerchantDB(t, m)
	pool := dbi.Pool()
	svc := metrics.NewService(dbi)

	mctx := merchant.WithID(ctx, merchant.ID(m))
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	customerID, productID, priceID := uuid.New(), uuid.New(), uuid.New()
	exec(ctx, t, pool, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, m, "tokentype-"+suffix)
	exec(ctx, t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, customerID, m)
	exec(ctx, t, pool, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'TT', $3)`, productID, "tokentype-p-"+suffix, m)
	exec(ctx, t, pool, `INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1, $2, 1990000, 'USD', $3)`, priceID, productID, m)

	type row struct {
		status, tokenType string
	}
	rows := []row{
		{"completed", "pan_via_proxy"}, {"completed", "pan_via_proxy"}, {"failed", "pan_via_proxy"}, {"failed", "pan_via_proxy"},
		{"completed", "psp_token"}, {"completed", "psp_token"}, {"completed", "psp_token"}, {"failed", "psp_token"},
		{"completed", ""}, // legacy NULL -> 'unknown'
	}
	for i, r := range rows {
		var tt any
		if r.tokenType != "" {
			tt = r.tokenType
		}
		exec(ctx, t, pool, `INSERT INTO openrails.payments
			(id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, token_type, purchased_at)
			VALUES ($1, $2, $3, $4, 'nmi', $5, 1990000, 1990000, 'USD', $6::openrails.payment_status, $7, '2026-06-10')`,
			uuid.New(), m, customerID, priceID, "tt-txn-"+suffix+"-"+strings.Repeat("x", i+1), r.status, tt)
	}

	res := run(t, svc, mctx, usd(&metrics.Query{
		Measures: []string{"approval_rate", "payment_count", "payment_failures"},
		By:       []string{"token_type"},
		Range:    juneQ,
	}))
	require.InDelta(t, 0.5, cell(t, res, map[string]string{"token_type": "pan_via_proxy"}, "approval_rate").(float64), 1e-9)
	require.InDelta(t, 0.75, cell(t, res, map[string]string{"token_type": "psp_token"}, "approval_rate").(float64), 1e-9)
	require.Equal(t, int64(1), cell(t, res, map[string]string{"token_type": "unknown"}, "payment_count"))

	// Filtering to one token_type excludes legacy-unknown rows entirely.
	filtered := run(t, svc, mctx, usd(&metrics.Query{
		Measures: []string{"payment_count"},
		Filters:  map[string][]string{"token_type": {"psp_token"}},
		Range:    juneQ,
	}))
	require.Len(t, filtered.Rows, 1)
	require.Equal(t, int64(3), cell(t, filtered, map[string]string{}, "payment_count"))
}
