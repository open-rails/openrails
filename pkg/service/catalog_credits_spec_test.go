package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestCatalogCreditsSpecPreservesUnit(t *testing.T) {
	expiresDays := 30
	in := CreditsSpec{
		"monthly_eur": {
			Unit:        "EUR",
			Amount:      2500,
			ExpiresDays: &expiresDays,
			Cadence:     CreditGrantCadencePerRenewal,
		},
	}

	modelSpec := toModelCreditsSpec(in)
	require.Equal(t, "EUR", modelSpec["monthly_eur"].Unit)
	require.Equal(t, int64(2500), modelSpec["monthly_eur"].Amount)

	product := productToCatalogProduct(&models.Product{
		ID: uuid.New(),
		CreditsSpec: models.CreditsSpec{
			"monthly_eur": {
				Unit:        "EUR",
				Amount:      2500,
				ExpiresDays: &expiresDays,
				Cadence:     models.CreditGrantCadencePerRenewal,
			},
		},
	})
	require.Equal(t, "EUR", product.CreditsSpec["monthly_eur"].Unit)

	userSpec := creditsSpecFromModel(models.CreditsSpec{
		"monthly_eur": {
			Unit:        "EUR",
			Amount:      2500,
			ExpiresDays: &expiresDays,
			Cadence:     models.CreditGrantCadencePerRenewal,
		},
	})
	require.Equal(t, "EUR", userSpec["monthly_eur"].Unit)
}
