package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestValidateTierChangePreviewTarget(t *testing.T) {
	t.Parallel()
	ctx := merchant.WithID(t.Context(), dbtest.TestMerchantID)
	currentStripe := &models.Price{}
	currentStripe.SetStripeConfig("price_current")
	targetStripe := &models.Price{}
	targetStripe.SetStripeConfig("price_target")

	t.Run("accepts configured Stripe target", func(t *testing.T) {
		t.Parallel()
		err := (&CheckoutService{}).validateTierChangePreviewTarget(ctx,
			&models.Subscription{Rail: models.RailStripe}, currentStripe, targetStripe,
			&UserIdentity{}, "downgrade")
		require.NoError(t, err)
	})

	t.Run("rejects missing Stripe target", func(t *testing.T) {
		t.Parallel()
		err := (&CheckoutService{}).validateTierChangePreviewTarget(ctx,
			&models.Subscription{Rail: models.RailStripe}, currentStripe, &models.Price{},
			&UserIdentity{}, "upgrade")
		require.ErrorContains(t, err, "target price not configured for Stripe")
	})

	t.Run("rejects CCBill downgrade before quoting it", func(t *testing.T) {
		t.Parallel()
		err := (&CheckoutService{}).validateTierChangePreviewTarget(ctx,
			&models.Subscription{Rail: models.RailCCBill}, &models.Price{}, &models.Price{},
			&UserIdentity{}, "downgrade")
		require.ErrorContains(t, err, "downgrades are not supported")
	})

	t.Run("requires the persisted NMI provider link", func(t *testing.T) {
		t.Parallel()
		mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
		svc := railTargetTestService(mobius)
		price := &models.Price{ID: uuid.New(), PSPLinks: map[string]map[string]string{
			"paykings": {
				models.RailKeyRail:   "nmi",
				models.RailKeyPlanID: "plan_paykings",
			},
		}}
		err := svc.validateTierChangePreviewTarget(ctx,
			&models.Subscription{Rail: models.RailNMI, PspID: mobius.ID}, &models.Price{}, price,
			&UserIdentity{}, "upgrade")
		require.ErrorContains(t, err, "missing NMI plan configuration for payment provider mobius")
	})
}
