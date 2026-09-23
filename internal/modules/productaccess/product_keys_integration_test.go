//go:build integration

package productaccess

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestProductKeyAccessKeepsIdentityMerchantAndArchivedOwnership(t *testing.T) {
	now := time.Now().UTC()
	svc, ctx, idTarget := newTestService(t, now)
	pool := svc.db.Pool()
	customer := uuid.NewString()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer)
	keyTarget := uuid.New()
	key := idTarget.String()
	_, err := pool.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,archived) VALUES($1,$2,$3,'Retained purchase',true)`, keyTarget, dbtest.TestMerchantID.UUID(), key)
	require.NoError(t, err)
	_, _, err = svc.GrantProductAccess(ctx, GrantParams{UserID: customer, ProductID: keyTarget, SourceType: models.ProductAccessSourceAdmin, SourceID: uuid.NewString()})
	require.NoError(t, err)
	byID, err := svc.CheckProducts(ctx, customer, []uuid.UUID{idTarget, keyTarget})
	require.NoError(t, err)
	require.False(t, byID[idTarget])
	require.True(t, byID[keyTarget])
	byKey, err := svc.CheckProductKeys(ctx, customer, []string{key, key, "missing"})
	require.NoError(t, err)
	require.Len(t, byKey, 2)
	require.Equal(t, keyTarget, byKey[key].ProductID)
	require.True(t, byKey[key].HasAccess, "archival is a sale policy, not revocation")
	require.Equal(t, KeyDecision{}, byKey["missing"])
	other := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, other, "keys-"+other.String())
	require.NoError(t, err)
	foreignProduct := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Foreign key')`, foreignProduct, other, key)
	require.NoError(t, err)
	foreign, err := svc.CheckProductKeys(merchant.WithID(ctx, merchant.ID(other)), customer, []string{key})
	require.NoError(t, err)
	require.Equal(t, foreignProduct, foreign[key].ProductID)
	require.False(t, foreign[key].HasAccess)
	ownerCtx, err := catalogscope.WithOwner(ctx, catalogscope.Scope{MerchantID: dbtest.TestMerchantID, CatalogID: uuid.New(), OwnerSubject: "other-owner"})
	require.NoError(t, err)
	attenuated, err := svc.CheckProductKeys(ownerCtx, customer, []string{key})
	require.NoError(t, err)
	require.Equal(t, KeyDecision{}, attenuated[key], "key resolution cannot escape catalog attenuation")
	_, err = svc.CheckProductKeys(ctx, customer, make([]string, 101))
	require.Error(t, err)
	_, err = svc.CheckProductKeys(ctx, customer, []string{""})
	require.Error(t, err)
}
