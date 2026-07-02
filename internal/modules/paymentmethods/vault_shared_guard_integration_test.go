//go:build integration

package paymentmethods

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
)

type noSubsReader struct{}

func (noSubsReader) GetPaginatedByUserID(context.Context, string, int, int) ([]models.Subscription, int, error) {
	return nil, 0, nil
}

// TestDeleteVaultRefusesSharedVault (#682): a whole-vault delete would destroy
// every billing entry in the vault, so when another payment-method row shares
// the vault id (an imported multi-card vault), DeleteVault must refuse BEFORE
// any provider call. The refusal happens ahead of NMI client resolution, so no
// gateway (real or fake) is needed.
func TestDeleteVaultRefusesSharedVault(t *testing.T) {
	pool := dbtest.SharedPGXPool(t)
	database, err := db.NewWithPGXPool(pool, "openrails")
	require.NoError(t, err)
	ctx := dbtest.WithTestMerchant(context.Background())

	userID := uuid.NewString()
	customerID, err := repo.EnsureCustomerID(ctx, database.Qx(ctx), uuid.Nil, userID)
	require.NoError(t, err)

	sharedVault := "vault-shared-" + uuid.NewString()[:8]
	pmRepo := repo.NewPaymentMethodRepo(database)
	mk := func(methodRef string) *models.PaymentMethod {
		pm := &models.PaymentMethod{
			ID:                   uuid.New(),
			CustomerID:           customerID,
			Rail:                 models.RailNMI,
			RailCustomerRef:      sharedVault,
			RailMethodRef:        methodRef,
			RebillDriver:         models.RebillDriverProvider,
			InitialTransactionID: "txn-" + uuid.NewString()[:8],
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}
		require.NoError(t, pmRepo.Create(ctx, pm))
		t.Cleanup(func() { _ = pmRepo.Delete(ctx, pm.ID) })
		return pm
	}
	pmA := mk("bill-a-" + uuid.NewString()[:8])
	_ = mk("bill-b-" + uuid.NewString()[:8])

	svc := &VaultService{
		SubscriptionService: noSubsReader{},
		DB:                  database,
	}

	err = svc.DeleteVault(ctx, pmA)
	require.Error(t, err, "shared vault must refuse the whole-vault delete")
	require.Contains(t, err.Error(), "share vault", "refusal must name the sharing condition: %v", err)
}
