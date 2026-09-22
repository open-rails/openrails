//go:build integration

package productaccess

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestBoundedProductAccessAndPurchasePages(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, ctx, _ := newTestService(t, now)
	customer := uuid.NewString()
	pool := svc.db.Pool()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer)
	prefix := "access-page-" + uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name)
 SELECT gen_random_uuid(),$1,$2||i::text,'Library product' FROM generate_series(1,1005) AS i`, dbtest.TestMerchantID.UUID(), prefix)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO billing.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,event,starts_at)
 SELECT merchant_id,$2,id,'ownership','admin',id::text,'grant',$3 FROM billing.products WHERE merchant_id=$1 AND key LIKE $4`, dbtest.TestMerchantID.UUID(), customerID, now.Add(-time.Hour), prefix+"%")
	require.NoError(t, err)
	var accessible uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM billing.products WHERE merchant_id=$1 AND key=$2`, dbtest.TestMerchantID.UUID(), prefix+"1").Scan(&accessible))
	var excluded []uuid.UUID
	for i, window := range []string{"expired", "future", "revoked"} {
		id := uuid.New()
		excluded = append(excluded, id)
		_, err = pool.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,$3)`, id, dbtest.TestMerchantID.UUID(), prefix+window)
		require.NoError(t, err)
		start := now.Add(-time.Hour)
		end := now.Add(time.Hour)
		if window == "expired" {
			end = now
		}
		if window == "future" {
			start = now.Add(time.Second)
		}
		grant, _, err := svc.GrantProductAccess(ctx, GrantParams{UserID: customer, ProductID: id, SourceType: models.ProductAccessSourceAdmin, SourceID: fmt.Sprint(i), StartsAt: &start, EndsAt: &end})
		require.NoError(t, err)
		if window == "revoked" {
			_, err = svc.RevokeProductAccess(ctx, grant.ID, models.ProductAccessRevokeRefund)
			require.NoError(t, err)
		}
	}
	candidates := append([]uuid.UUID{accessible, accessible, uuid.New()}, excluded...)
	results, err := svc.CheckProducts(ctx, customer, candidates)
	require.NoError(t, err)
	require.Len(t, results, 5)
	require.True(t, results[accessible])
	for _, id := range candidates[2:] {
		require.False(t, results[id])
	}
	for _, id := range candidates {
		has, err := svc.HasProductAccess(ctx, customer, id)
		require.NoError(t, err)
		require.Equal(t, results[id], has)
	}
	stranger, err := svc.CheckProducts(ctx, uuid.NewString(), candidates)
	require.NoError(t, err)
	for _, has := range stranger {
		require.False(t, has)
	}
	// The same customer can own a different product under another merchant.
	// Its immutable product ID must never grant access in this merchant.
	foreignMerchant, foreignProduct := uuid.New(), uuid.New()
	admin := dbtest.SharedSuperuserPGXPool(t)
	_, err = admin.Exec(ctx, `INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, foreignMerchant, "foreign-access-"+uuid.NewString())
	require.NoError(t, err)
	dbtest.EnsureCustomerIDPgxFor(ctx, t, admin, foreignMerchant, customer)
	_, err = admin.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Foreign purchase')`, foreignProduct, foreignMerchant, uuid.NewString())
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `INSERT INTO billing.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,event,starts_at) VALUES($1,$2,$3,'ownership','admin',$4,'grant',$5)`, foreignMerchant, customerID, foreignProduct, uuid.NewString(), now.Add(-time.Hour))
	require.NoError(t, err)
	foreign, err := svc.CheckProducts(ctx, customer, []uuid.UUID{foreignProduct})
	require.NoError(t, err)
	require.False(t, foreign[foreignProduct])
	empty, err := svc.CheckProducts(ctx, customer, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, err = svc.CheckProducts(ctx, customer, make([]uuid.UUID, 101))
	require.Error(t, err)
	_, err = svc.CheckProducts(ctx, customer, []uuid.UUID{uuid.Nil})
	require.Error(t, err)
	var cursor uuid.UUID
	seen := map[uuid.UUID]bool{}
	for {
		page, more, err := svc.ListAccessibleProductsPage(ctx, customer, cursor, 23)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), 23)
		for _, grant := range page {
			require.False(t, seen[grant.ID])
			seen[grant.ID] = true
			require.Equal(t, customerID, grant.CustomerID)
		}
		if !more {
			break
		}
		require.Len(t, page, 23)
		cursor = page[len(page)-1].ID
	}
	require.Len(t, seen, 1005, "all pages must preserve purchases beyond the first page")
	_, _, err = svc.ListAccessibleProductsPage(ctx, customer, uuid.Nil, 101)
	require.Error(t, err)
}
