//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestRailMerchantAccountScopedLocalStateDoesNotBlendCollidingProviderIDs(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	var accountA gen.OpenrailsPsp
	var accountB gen.OpenrailsPsp
	var customerA uuid.UUID
	var customerB uuid.UUID

	dbtest.EnsureTestMerchant(context.Background(), t, appDB.Pool())

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := appDB.Gen(ctx)
		now := time.Now().UTC()
		var err error
		accountA, err = q.UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			Rail:           "nmi",
			AccountID:      "nmi-account-a",
			LastVerifiedAt: &now,
		})
		require.NoError(t, err)
		accountB, err = q.UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			Rail:           "nmi",
			AccountID:      "nmi-account-b",
			LastVerifiedAt: &now,
		})
		require.NoError(t, err)
		require.False(t, accountA.Archived)
		require.False(t, accountB.Archived)

		customerA = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		customerB = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())

		productID := uuid.New()
		priceID := uuid.New()
		suffix := uuid.NewString()[:8]
		_, err = appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
			 VALUES ($1, $2, $2, $3, jsonb_build_object('premium', null), $4)`,
			productID, "provider-account-scope-"+suffix, "provider-account-scope-"+suffix, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)
		_, err = appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
			 VALUES ($1, $2, 999, 'USD', 720, true, $3)`,
			priceID, productID, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)

		seedMirrorRows := func(account gen.OpenrailsPsp, customerID uuid.UUID, marker string) {
			t.Helper()
			pmID := uuid.New()
			subID := uuid.New()
			start := now.Add(-24 * time.Hour)
			end := now.Add(29 * 24 * time.Hour)
			_, err = appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.payment_methods
				   (id, rail, rail_customer_ref, initial_transaction_id, last_four, expiry_date, merchant_id, customer_id, psp_id)
				 VALUES ($1, 'nmi', 'vault-shared', 'init-' || $2::text, $3, '1029', $4, $5, $6)`,
				pmID, marker, marker, dbtest.TestMerchantID.UUID(), customerID, account.ID)
			require.NoError(t, err)
			_, err = appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.subscriptions
				   (id, price_id, product_id, status, rail, rail_subscription_id,
				    payment_method_id, current_period_starts_at, current_period_ends_at, started_at,
				    entitlements_spec_snapshot, customer_id, merchant_id, psp_id)
				 VALUES ($1, $2, $3, 'active', 'nmi', 'sub-' || $10::text,
				    $4, $5, $6, $5, jsonb_build_object('premium', null), $7, $8, $9)`,
				subID, priceID, productID, pmID, start, end, customerID, dbtest.TestMerchantID.UUID(), account.ID, marker)
			require.NoError(t, err)
			_, err = appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.payments
				   (id, price_id, rail, transaction_id, amount, list_amount, currency,
				    status, subscription_id, purchased_at, customer_id, merchant_id, psp_id)
				 VALUES ($1, $2, 'nmi', 'txn-' || $8::text, 999, 999, 'USD',
				    'completed', $3, $4, $5, $6, $7)`,
				uuid.New(), priceID, subID, now, customerID, dbtest.TestMerchantID.UUID(), account.ID, marker)
			require.NoError(t, err)
		}
		seedMirrorRows(accountA, customerA, "1111")
		seedMirrorRows(accountB, customerB, "2222")
		return nil
	}))

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		loader := &PGLocalStateLoader{DB: appDB}
		stateA, err := loader.Load(ctx, ProviderNMI, &accountA.ID)
		require.NoError(t, err)
		require.Len(t, stateA.Subscriptions, 1)
		require.Equal(t, customerA, stateA.Subscriptions[0].CustomerID)
		require.Len(t, stateA.PaymentMethods, 1)
		require.Equal(t, "1111", stateA.PaymentMethods[0].LastFour)

		paymentsA, err := loader.PaymentsByTransactionIDs(ctx, ProviderNMI, &accountA.ID, []string{"txn-1111", "txn-2222"})
		require.NoError(t, err)
		require.Len(t, paymentsA, 1)
		require.Equal(t, customerA, paymentsA[0].CustomerID)

		stateB, err := loader.Load(ctx, ProviderNMI, &accountB.ID)
		require.NoError(t, err)
		require.Len(t, stateB.Subscriptions, 1)
		require.Equal(t, customerB, stateB.Subscriptions[0].CustomerID)
		require.Len(t, stateB.PaymentMethods, 1)
		require.Equal(t, "2222", stateB.PaymentMethods[0].LastFour)

		paymentsB, err := loader.PaymentsByTransactionIDs(ctx, ProviderNMI, &accountB.ID, []string{"txn-1111", "txn-2222"})
		require.NoError(t, err)
		require.Len(t, paymentsB, 1)
		require.Equal(t, customerB, paymentsB[0].CustomerID)
		return nil
	}), fmt.Sprintf("provider accounts should isolate colliding NMI ids: %s vs %s", accountA.ID, accountB.ID))
}
