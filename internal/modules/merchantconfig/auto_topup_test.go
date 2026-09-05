package merchantconfig

import (
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAutoTopupSafetyDefaultsAndValidation(t *testing.T) {
	policy, err := AutoTopupSafety(nil)
	require.NoError(t, err)
	require.Equal(t, models.AutoTopupSafetyPolicy{MaxDaily: 3, MaxWeekly: 10, MaxMonthly: 30, DeclinesBeforeDisable: 3}, policy)
	for _, invalid := range []models.AutoTopupSafetyPolicy{{}, {MaxDaily: -1, MaxWeekly: 10, MaxMonthly: 30, DeclinesBeforeDisable: 3}, {MaxDaily: 3, MaxWeekly: 10, MaxMonthly: 30}} {
		_, err := AutoTopupSafety(&invalid)
		require.Error(t, err)
	}
	custom := models.AutoTopupSafetyPolicy{MaxDaily: 1, MaxWeekly: 2, MaxMonthly: 5, DeclinesBeforeDisable: 2}
	got, err := AutoTopupSafety(&custom)
	require.NoError(t, err)
	require.Equal(t, custom, got)
}
