package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
)

type recordingPurchaseCreditGranter struct {
	called bool
}

func (f *recordingPurchaseCreditGranter) GrantPurchaseCredits(context.Context, money.GrantPurchaseCreditsParams) error {
	f.called = true
	return nil
}

func TestGrantPurchaseCredits_InvalidPayerReturnsError(t *testing.T) {
	granter := &recordingPurchaseCreditGranter{}
	service := &CheckoutPurchaseService{MoneyService: granter}

	err := service.grantPurchaseCredits(context.Background(), "not-a-uuid", models.CreditsSpec{
		"welcome": {Unit: money.DefaultCurrency, Amount: 25},
	}, uuid.New(), false)

	require.ErrorContains(t, err, "not a UUID")
	require.False(t, granter.called, "an invalid payer must not reach the money service")
}
