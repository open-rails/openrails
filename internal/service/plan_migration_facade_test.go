package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// #814 gap 2: an embedding host must be able to NAME every type it receives.
// The roots were re-exported but their leaves were not, so hosts mirrored
// RailCounts / PlanMigrationOutcome / PlanMigrationCancelResult field-by-field
// and drifted on every upstream change. This is the compile-time proof that a
// host can build and read the whole tree through the facade alone.
func TestPlanMigrationLeafTypesAreNameableFromTheFacade(t *testing.T) {
	subID := uuid.New()
	res := PlanMigrationResult{
		ByRail:   map[string]*RailCounts{"stripe": {Auto: 2, RequiresAction: 1, Skipped: 0}},
		Outcomes: []PlanMigrationOutcome{{SubscriptionID: subID, Rail: "stripe", Disposition: "scheduled"}},
	}
	require.Equal(t, 2, res.ByRail["stripe"].Auto)
	require.Equal(t, subID, res.Outcomes[0].SubscriptionID)

	cancel := PlanMigrationCancelResult{Canceled: 1, RailReleaseRequired: []uuid.UUID{subID}, Warning: "release the schedules"}
	require.Equal(t, 1, cancel.Canceled)
	require.Equal(t, subID, cancel.RailReleaseRequired[0])
}
