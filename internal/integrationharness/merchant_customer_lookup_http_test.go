//go:build integration

package integrationharness

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/productaccess"
)

func TestStandaloneMerchantCustomerLookupClientHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	client := surface.Client()
	pool := h.Pool()

	subjectID := uuid.New()
	subject := subjectID.String()
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id, issuer, subject, created_at, last_seen_at)
		VALUES ($1, $2, 'lookup-test', $3, now(), now())`,
		subjectID, dbtest.TestMerchantID.UUID(), subject)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.entitlements (
			id, merchant_id, customer_id, entitlement, start_at, source_id, source_type, created_at, updated_at
		) VALUES ($1, $2, $3, 'premium', now() - interval '1 hour', $4, 'admin', now(), now())`,
		uuid.New(), dbtest.TestMerchantID.UUID(), subjectID, uuid.New())
	require.NoError(t, err)

	productID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name)
		VALUES ($1, $2, $3, 'Lookup Test Product')`,
		productID, dbtest.TestMerchantID.UUID(), "lookup-product-"+productID.String())
	require.NoError(t, err)
	pa := productaccess.NewService(dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	_, _, err = pa.GrantProductAccess(dbtest.WithTestMerchant(ctx), productaccess.GrantParams{
		UserID:     subject,
		ProductID:  productID,
		SourceType: models.ProductAccessSourceAdmin,
		SourceID:   "lookup-test:" + productID.String(),
	})
	require.NoError(t, err)

	batch, err := client.ListActiveEntitlements(ctx, []string{subject, "ghost-" + subject}, time.Time{})
	require.NoError(t, err)
	require.Len(t, batch[subject], 1)
	require.Equal(t, "premium", batch[subject][0].Entitlement)
	require.Empty(t, batch["ghost-"+subject])

	single, err := client.ListEntitlements(ctx, subject, time.Time{})
	require.NoError(t, err)
	require.Len(t, single, 1)
	require.Equal(t, "premium", single[0].Entitlement)

	hasPremium, err := client.HasEntitlement(ctx, subject, "premium", time.Time{})
	require.NoError(t, err)
	require.True(t, hasPremium)
	hasMissing, err := client.HasEntitlement(ctx, subject, "missing", time.Time{})
	require.NoError(t, err)
	require.False(t, hasMissing)

	access, err := client.ListProductAccess(ctx, subject)
	require.NoError(t, err)
	require.Len(t, access, 1)
	require.Equal(t, productID.String(), access[0].ProductID)
	require.Equal(t, "active", access[0].Status)

	hasProduct, err := client.HasProductAccess(ctx, subject, productID.String())
	require.NoError(t, err)
	require.True(t, hasProduct)
	hasOtherProduct, err := client.HasProductAccess(ctx, subject, uuid.NewString())
	require.NoError(t, err)
	require.False(t, hasOtherProduct)

	balance, err := client.GetCreditAccount(ctx, subject, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, subject, balance.CustomerID)
	require.Equal(t, money.DefaultCurrency, balance.Currency)
}
