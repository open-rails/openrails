//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// #826 regression: metered rating under the merchant-scoped path (pinned
// connection, unprivileged openrails_app role, RLS enforced) must see the rate
// cards and usage events and rate a NON-ZERO amount. Before the fix the
// RunInMerchantConn bodies queried the raw pool (no GUC → zero rows
// fail-closed) and silently rated $0.
func TestSweepUsage_AppRoleMerchantScoped_RatesNonZero(t *testing.T) {
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)
	ctx := context.Background()

	superPool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(superPool.Close)

	dbtest.EnsureTestMerchant(ctx, t, superPool)
	merchantID := dbtest.TestMerchantID.UUID()
	payer := identity.CustomerIDFromString(uuid.NewString())
	productID := uuid.New()
	meterKey := "rls-seconds-" + uuid.NewString()
	eventType := "rls.usage." + uuid.NewString()

	t.Cleanup(func() {
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.metered_rating_watermarks WHERE customer_id = $1", payer.UUID())
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1", payer.UUID())
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payer.UUID())
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meterKey)
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		_, _ = superPool.Exec(ctx, "DELETE FROM openrails.customers WHERE id = $1", payer.UUID())
	})

	// Seed catalog + usage with the RLS-bypassing super role.
	_, err = superPool.Exec(ctx, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, payer.UUID(), merchantID)
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'RLS Rating', $3)`,
		productID, "rls-rating-"+uuid.NewString(), merchantID)
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit)
VALUES ($1, $2, $3, 'seconds', 'sum', 'second')`, merchantID, meterKey, eventType)
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', '{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":10000}}'::jsonb)`,
		merchantID, productID, meterKey)
	require.NoError(t, err)
	occurred := time.Now().UTC()
	_, err = superPool.Exec(ctx, `
INSERT INTO openrails.usage_events (merchant_id, customer_id, invoker_id, currency, event_type, dimensions, amount, source, source_id, occurred_at)
VALUES ($1, $2, $3, 'USD', $4, '{"seconds": 7200}'::jsonb, 0, 'rls-826', $5, $6)`,
		merchantID, payer.UUID(), payer.UUID().String(), eventType, uuid.NewString(), occurred)
	require.NoError(t, err)

	// Run the sweep exactly like a worker: app role + RunInMerchantConn.
	appDB := dbtest.OpenAppDB(t, appDSN)
	svc := money.NewMoneyService(appDB)
	mctx := dbtest.WithTestMerchant(ctx)
	from, to := occurred.Add(-time.Hour), occurred.Add(time.Hour)
	require.NoError(t, appDB.RunInMerchantConn(mctx, func(ctx context.Context) error {
		return svc.SweepUsage(ctx, payer, money.DefaultCurrency, from, to)
	}))

	var total int64
	require.NoError(t, superPool.QueryRow(ctx, `
SELECT COALESCE(sum(amount), 0)::bigint
FROM openrails.invoice_items
WHERE customer_id = $1 AND status = 'pending' AND source_id LIKE 'metered:%'`, payer.UUID()).Scan(&total))
	require.Equal(t, int64(72_000_000), total, "7200 units x 10000 micros must rate non-zero under the app role")
}
