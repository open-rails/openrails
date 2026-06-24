package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestRequireNMIPlanForRail_UsesProviderSpecificConfig(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"acme": {
				models.RailKeyPlanID: "plan_acme_123",
			},
		},
	}

	planID, err := requireNMIPlanForRail(price, "acme")
	require.NoError(t, err)
	require.Equal(t, "plan_acme_123", planID)
}

func TestRequireNMIPlanForRail_RejectsProviderFromDifferentRailSlot(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"mobius": {
				models.RailKeyPlanID:   "mobius_plan_456",
				models.RailKeyProvider: "acme",
			},
		},
	}

	_, err := requireNMIPlanForRail(price, "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}

func TestRequireNMIPlanForRail_RejectsMissingProviderConfig(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"mobius": {
				models.RailKeyPlanID: "plan_mobius_only",
			},
		},
	}

	_, err := requireNMIPlanForRail(price, "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}
