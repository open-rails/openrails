package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestRequireNMIPlanForTarget_ResolvesProviderEntry(t *testing.T) {
	// Entries key on the merchant's account name; the rail lives in the entry.
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"acme": {
				models.RailKeyRail:   "nmi",
				models.RailKeyPlanID: "plan_acme_123",
			},
		},
	}

	planID, err := requireNMIPlanForTarget(price, railTarget{Provider: "acme", Rail: "nmi"})
	require.NoError(t, err)
	require.Equal(t, "plan_acme_123", planID)
}

func TestRequireNMIPlanForTarget_FallsBackToRailNamedEntry(t *testing.T) {
	// A manifest that declared the link under the rail name still resolves
	// when the provider is an account key.
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"nmi": {
				models.RailKeyRail:   "nmi",
				models.RailKeyPlanID: "plan_rail_default",
			},
		},
	}

	planID, err := requireNMIPlanForTarget(price, railTarget{Provider: "mobius", Rail: "nmi"})
	require.NoError(t, err)
	require.Equal(t, "plan_rail_default", planID)
}

func TestRequireNMIPlanForTarget_NeverCrossesProviders(t *testing.T) {
	// Another provider's plan on the same rail is NOT this provider's plan.
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"paykings": {
				models.RailKeyRail:   "nmi",
				models.RailKeyPlanID: "plan_paykings",
			},
		},
	}

	_, err := requireNMIPlanForTarget(price, railTarget{Provider: "mobius", Rail: "nmi"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}

func TestRequireNMIPlanForTarget_RejectsEntryOnAnotherRail(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"mobius": {
				models.RailKeyRail:   "ccbill",
				models.RailKeyPlanID: "mobius_plan_456",
			},
		},
	}

	_, err := requireNMIPlanForTarget(price, railTarget{Provider: "mobius", Rail: "nmi"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}
