package subscriptions

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPlanProviderAccountCutoverSamePSPUsesDurablePath(t *testing.T) {
	psp := uuid.New()
	plan := PlanProviderAccountCutover(ProviderAccountCutoverRequest{SourcePSPID: psp, TargetPSPID: psp})
	require.Equal(t, ProviderAccountCutoverSameAccount, plan.Disposition)
	require.True(t, plan.Executable)
	require.Contains(t, plan.Steps[0], "nmi_payment_source_update")
}

func TestPlanProviderAccountCutoverCrossPSPRequiresCardReentry(t *testing.T) {
	plan := PlanProviderAccountCutover(ProviderAccountCutoverRequest{
		SourcePSPID: uuid.New(), TargetPSPID: uuid.New(), SourceArchived: true,
		TargetArchived: false, ReplacementCardCollected: false,
	})
	require.Equal(t, ProviderAccountCutoverRequiresReentry, plan.Disposition)
	require.False(t, plan.Executable)
	require.Contains(t, plan.Reason, "not portable")
	require.Contains(t, plan.Steps[0], "re-enter")
}

func TestPlanProviderAccountCutoverBlocksUnsafeSourceOrTarget(t *testing.T) {
	tests := []struct {
		name string
		req  ProviderAccountCutoverRequest
		want string
	}{
		{"active source", ProviderAccountCutoverRequest{SourcePSPID: uuid.New(), TargetPSPID: uuid.New()}, "archived source"},
		{"archived target", ProviderAccountCutoverRequest{SourcePSPID: uuid.New(), TargetPSPID: uuid.New(), SourceArchived: true, TargetArchived: true}, "archived"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanProviderAccountCutover(tt.req)
			require.Equal(t, ProviderAccountCutoverBlocked, plan.Disposition)
			require.False(t, plan.Executable)
			require.Contains(t, plan.Reason, tt.want)
		})
	}
}

func TestPlanProviderAccountCutoverReportsUnqualifiedCrossPSPTransaction(t *testing.T) {
	plan := PlanProviderAccountCutover(ProviderAccountCutoverRequest{
		SourcePSPID: uuid.New(), TargetPSPID: uuid.New(), SourceArchived: true,
		ReplacementCardCollected: true,
	})
	require.Equal(t, ProviderAccountCutoverRequiresReentry, plan.Disposition)
	require.False(t, plan.Executable)
	require.Contains(t, plan.Reason, "not qualified")
	require.Len(t, plan.Steps, 5)
}
