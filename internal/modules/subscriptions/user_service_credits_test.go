package subscriptions

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestCreditsSpecToAPIObjectPreservesUnit(t *testing.T) {
	got := creditsSpecToAPIObject(models.CreditsSpec{
		"monthly_eur": {Unit: "EUR", Amount: 2500},
	})

	require.Equal(t, "EUR", got["monthly_eur"].Unit)
	require.Equal(t, int64(2500), got["monthly_eur"].Amount)
}
