package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestRequireNMIPlanForProcessor_UsesProviderSpecificConfig(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Processors: map[string]map[string]string{
			"acme": {
				models.ProcessorKeyPlanID: "plan_acme_123",
			},
		},
	}

	planID, err := requireNMIPlanForProcessor(price, "acme")
	require.NoError(t, err)
	require.Equal(t, "plan_acme_123", planID)
}

func TestRequireNMIPlanForProcessor_UsesLegacyProviderSlotWhenProviderMatches(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Processors: map[string]map[string]string{
			"mobius": {
				models.ProcessorKeyPlanID:   "legacy_plan_456",
				models.ProcessorKeyProvider: "acme",
			},
		},
	}

	planID, err := requireNMIPlanForProcessor(price, "acme")
	require.NoError(t, err)
	require.Equal(t, "legacy_plan_456", planID)
}

func TestRequireNMIPlanForProcessor_RejectsMissingProviderConfig(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Processors: map[string]map[string]string{
			"mobius": {
				models.ProcessorKeyPlanID: "plan_mobius_only",
			},
		},
	}

	_, err := requireNMIPlanForProcessor(price, "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}
