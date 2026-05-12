package vault

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestApplyUpdatedCardMetadataReplacesStoredCardDetails(t *testing.T) {
	lastFour := "4242"
	cardType := "Visa"
	expiryDate := "12/30"
	pm := &models.PaymentMethod{}

	applyUpdatedCardMetadata(pm, &UpdateVaultRequest{
		LastFour:   &lastFour,
		CardType:   &cardType,
		ExpiryDate: &expiryDate,
	})

	require.NotNil(t, pm.LastFour)
	require.Equal(t, "4242", *pm.LastFour)
	require.NotNil(t, pm.CardType)
	require.Equal(t, "Visa", *pm.CardType)
	require.NotNil(t, pm.ExpiryDate)
	require.Equal(t, "12/30", *pm.ExpiryDate)
}

func TestApplyUpdatedCardMetadataClearsOmittedCardDetails(t *testing.T) {
	oldLastFour := "1111"
	oldCardType := "Visa"
	oldExpiryDate := "01/29"
	pm := &models.PaymentMethod{
		LastFour:   &oldLastFour,
		CardType:   &oldCardType,
		ExpiryDate: &oldExpiryDate,
	}

	applyUpdatedCardMetadata(pm, &UpdateVaultRequest{})

	require.Nil(t, pm.LastFour)
	require.Nil(t, pm.CardType)
	require.Nil(t, pm.ExpiryDate)
}
