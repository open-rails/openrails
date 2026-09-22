//go:build integration

package integrationharness

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
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
		INSERT INTO billing.customers (id, merchant_id, issuer, created_at, last_seen_at)
		VALUES ($1, $2, 'lookup-test', now(), now())`,
		subjectID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO billing.entitlements (
			id, merchant_id, customer_id, entitlement, start_at, source_id, source_type, created_at, updated_at
		) VALUES ($1, $2, $3, 'premium', now() - interval '1 hour', $4, 'admin', now(), now())`,
		uuid.New(), dbtest.TestMerchantID.UUID(), subjectID, uuid.New())
	require.NoError(t, err)

	productID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO billing.products (id, merchant_id, key, display_name)
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

	ghost := openrails.CustomerID(uuid.New())
	customer := openrails.CustomerID(subjectID)
	batch, err := client.ListActiveEntitlements(ctx, []string{(customer).String(), (ghost).String()}, time.Time{})
	require.NoError(t, err)
	require.Len(t, batch[(customer).String()], 1)
	require.Equal(t, "premium", batch[(customer).String()][0].Entitlement)
	require.Empty(t, batch[(ghost).String()])

	single, err := client.ListEntitlements(ctx, (customer).String(), time.Time{})
	require.NoError(t, err)
	require.Len(t, single, 1)
	require.Equal(t, "premium", single[0].Entitlement)

	hasPremium, err := client.HasEntitlement(ctx, (customer).String(), "premium", time.Time{})
	require.NoError(t, err)
	require.True(t, hasPremium)
	hasMissing, err := client.HasEntitlement(ctx, (customer).String(), "missing", time.Time{})
	require.NoError(t, err)
	require.False(t, hasMissing)

	access, err := client.ProductAccess.List(ctx, &openrails.ProductAccessListParams{CustomerID: customer.String()})
	require.NoError(t, err)
	require.Len(t, access.Data, 1)
	require.Equal(t, openrails.ProductID(productID).String(), access.Data[0].ProductID)
	require.Equal(t, "active", access.Data[0].Status)

	hasProduct, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer.String(), ProductID: openrails.ProductID(productID).String()})
	require.NoError(t, err)
	require.True(t, hasProduct.HasAccess)
	hasOtherProduct, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer.String(), ProductID: openrails.ProductID(uuid.New()).String()})
	require.NoError(t, err)
	require.False(t, hasOtherProduct.HasAccess)

	balance, err := client.GetCreditAccount(ctx, (customer).String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, customer.String(), balance.CustomerID)
	require.Equal(t, money.DefaultCurrency, balance.Currency)
}
