package checkout

import (
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestValidateTierChangeSubscriptionStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []models.SubscriptionStatus{models.StatusActive, models.StatusPastDue} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			require.NoError(t, validateTierChangeSubscriptionStatus(&models.Subscription{Status: status}))
		})
	}

	for _, status := range []models.SubscriptionStatus{
		models.StatusPending,
		models.StatusCancelled,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			err := validateTierChangeSubscriptionStatus(&models.Subscription{Status: status})
			var tierErr *TierChangeError
			require.ErrorAs(t, err, &tierErr)
			require.Equal(t, http.StatusConflict, tierErr.HTTPStatus)
		})
	}
}
