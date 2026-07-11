package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestRequireNMIPlanForRail_ResolvesAccountKeyedEntry(t *testing.T) {
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

	planID, err := requireNMIPlanForRail(price, "nmi")
	require.NoError(t, err)
	require.Equal(t, "plan_acme_123", planID)
}

func TestRequireNMIPlanForRail_RejectsEntryOnAnotherRail(t *testing.T) {
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"mobius": {
				models.RailKeyRail:   "ccbill",
				models.RailKeyPlanID: "mobius_plan_456",
			},
		},
	}

	_, err := requireNMIPlanForRail(price, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}

func TestRequireNMIPlanForRail_RejectsAmbiguousAccounts(t *testing.T) {
	// Two accounts on the rail: never guess which account's plan to charge.
	price := &models.Price{
		ID: uuid.New(),
		Rails: map[string]map[string]string{
			"mobius":   {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_mobius"},
			"paykings": {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_paykings"},
		},
	}

	_, err := requireNMIPlanForRail(price, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing NMI plan configuration")
}
