//go:build integration

package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestPriceBindingsKeepPSPIdentityThroughCollisionRenameArchive(t *testing.T) {
	database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	pspA, pspB, productID := uuid.New(), uuid.New(), uuid.New()
	keyA, keyB := "binding-a-"+uuid.NewString(), "binding-b-"+uuid.NewString()
	for id, key := range map[uuid.UUID]string{pspA: keyA, pspB: keyB} {
		_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps (id, merchant_id, rail, environment, account_id, key) VALUES ($1,$2,'nmi','test',$3,$3)`, id, dbtest.TestMerchantID.UUID(), key)
		require.NoError(t, err)
	}
	_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO billing.products (id, merchant_id, key, display_name) VALUES ($1,$2,$3,'Bindings')`, productID, dbtest.TestMerchantID.UUID(), uuid.NewString())
	require.NoError(t, err)
	service := NewPriceService(database)
	planID := "shared-plan-" + uuid.NewString()
	hours := 720
	prices := []*models.Price{}
	for i, key := range []string{keyA, keyB} {
		price := &models.Price{ID: uuid.New(), Key: uuid.NewString(), MerchantID: dbtest.TestMerchantID.UUID(), ProductID: productID, Amount: int64(i+1) * 1000000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
			PSPLinks: map[string]map[string]string{key: {models.RailKeyRail: "nmi", models.RailKeyPlanID: planID}}}
		require.NoError(t, service.Create(ctx, price))
		prices = append(prices, price)
	}
	ctxA, ctxB := db.WithPSPID(ctx, pspA), db.WithPSPID(ctx, pspB)
	gotA, err := service.GetByNMIPlan(ctxA, "nmi", planID)
	require.NoError(t, err)
	require.Equal(t, prices[0].ID, gotA.ID)
	gotB, err := service.GetByNMIPlan(ctxB, "nmi", planID)
	require.NoError(t, err)
	require.Equal(t, prices[1].ID, gotB.ID)
	_, err = service.GetByNMIPlan(ctx, "nmi", planID)
	require.ErrorIs(t, err, db.ErrNoPSPInContext)
	_, err = database.Qx(ctx).Exec(ctx, `UPDATE billing.psps SET key=$2, archived=true WHERE id=$1`, pspA, keyA+"-renamed")
	require.NoError(t, err)
	gotA, err = service.GetByNMIPlan(ctxA, "nmi", planID)
	require.NoError(t, err)
	require.Equal(t, pspA.String(), gotA.PSPLinks[keyA+"-renamed"][models.RailKeyPSPID])
	// A previously read catalog payload remains bound to A after rename.
	require.NoError(t, service.UpdatePSPLinks(ctx, prices[0].ID, map[string]map[string]string{keyA: {models.RailKeyPSPID: pspA.String(), models.RailKeyRail: "nmi", models.RailKeyPlanID: planID}}))
	merchantService, err := merchants.NewService(db.WrapPool(database.Pool(), ""), merchants.NewMemorySecretStore(), "test")
	require.NoError(t, err)
	_, available, err := merchantService.PSPScopeByKey(ctx, dbtest.TestMerchantID, keyA+"-renamed", "test")
	require.NoError(t, err)
	require.False(t, available, "archived account cannot admit a new purchase")
	scope, available, err := merchantService.PSPScopeByAccountID(ctx, dbtest.TestMerchantID, "nmi", keyA)
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, pspA, scope.ID, "existing obligations retain exact account identity")
	// Duplicate ownership within one account fails atomically, preserving B.
	err = service.UpdatePSPLinks(ctx, prices[1].ID, map[string]map[string]string{keyA: {models.RailKeyPSPID: pspA.String(), models.RailKeyRail: "nmi", models.RailKeyPlanID: planID}})
	require.Error(t, err)
	gotB, err = service.GetByNMIPlan(ctxB, "nmi", planID)
	require.NoError(t, err)
	require.Equal(t, prices[1].ID, gotB.ID)
}
