package subscriptions

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// Every prerequisite of the durable update gates Executable, and every
// non-executable plan carries a machine code.
func TestPlanProviderAccountCutoverGatesOnEveryPrerequisite(t *testing.T) {
	source, target := uuid.New(), uuid.New()
	card := func(psp uuid.UUID) *ProviderAccountCutoverReplacement {
		return &ProviderAccountCutoverReplacement{Found: true, OwnedByPayer: true, PSPID: psp, Rail: models.RailNMI, PSPVaulted: true}
	}
	base := func(targetPSP uuid.UUID, r *ProviderAccountCutoverReplacement) ProviderAccountCutoverRequest {
		return ProviderAccountCutoverRequest{
			Rail: models.RailNMI, Status: models.StatusActive, HasRailSubscription: true,
			SourcePSPID: source, TargetPSPID: targetPSP, TargetRail: models.RailNMI, Replacement: r,
		}
	}
	tests := []struct {
		name        string
		mutate      func(*ProviderAccountCutoverRequest)
		disposition ProviderAccountCutoverDisposition
		code        ProviderAccountCutoverCode
	}{
		{"ready", func(r *ProviderAccountCutoverRequest) {}, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReady},
		{"past_due ready", func(r *ProviderAccountCutoverRequest) { r.Status = models.StatusPastDue }, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReady},
		{"missing identity", func(r *ProviderAccountCutoverRequest) { r.TargetPSPID = uuid.Nil }, ProviderAccountCutoverBlocked, ProviderAccountCutoverIdentityMissing},
		{"stripe", func(r *ProviderAccountCutoverRequest) { r.Rail = models.RailStripe }, ProviderAccountCutoverBlocked, ProviderAccountCutoverRailUnsupported},
		{"ccbill", func(r *ProviderAccountCutoverRequest) { r.Rail = models.RailCCBill }, ProviderAccountCutoverBlocked, ProviderAccountCutoverRailUnsupported},
		{"cancelled", func(r *ProviderAccountCutoverRequest) { r.Status = models.StatusCancelled }, ProviderAccountCutoverBlocked, ProviderAccountCutoverSubscriptionNotRebilling},
		{"no provider record", func(r *ProviderAccountCutoverRequest) { r.HasRailSubscription = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverSubscriptionNotAtProvider},
		{"target on another rail", func(r *ProviderAccountCutoverRequest) { r.TargetRail = models.RailStripe }, ProviderAccountCutoverBlocked, ProviderAccountCutoverTargetRailMismatch},
		{"archived target", func(r *ProviderAccountCutoverRequest) { r.TargetArchived = true }, ProviderAccountCutoverBlocked, ProviderAccountCutoverTargetArchived},
		{"no card", func(r *ProviderAccountCutoverRequest) { r.Replacement = nil }, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReplacementCardRequired},
		{"card missing", func(r *ProviderAccountCutoverRequest) { r.Replacement = &ProviderAccountCutoverReplacement{} }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardNotFound},
		{"card not owned", func(r *ProviderAccountCutoverRequest) { r.Replacement.OwnedByPayer = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardNotOwned},
		{"card on another psp", func(r *ProviderAccountCutoverRequest) { r.Replacement.PSPID = target }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardPSP},
		{"card parked", func(r *ProviderAccountCutoverRequest) { r.Replacement.Parked = true }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardUnusable},
		{"card custodian-held", func(r *ProviderAccountCutoverRequest) { r.Replacement.PSPVaulted = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardUnusable},
		{"cross account, active source", func(r *ProviderAccountCutoverRequest) {
			r.TargetPSPID, r.Replacement = target, card(target)
		}, ProviderAccountCutoverBlocked, ProviderAccountCutoverSourceNotArchived},
		{"cross account, no card", func(r *ProviderAccountCutoverRequest) {
			r.TargetPSPID, r.SourceArchived, r.Replacement = target, true, nil
		}, ProviderAccountCutoverRequiresReentry, ProviderAccountCutoverReplacementCardRequired},
		{"cross account, card collected", func(r *ProviderAccountCutoverRequest) {
			r.TargetPSPID, r.SourceArchived, r.Replacement = target, true, card(target)
		}, ProviderAccountCutoverRequiresReentry, ProviderAccountCutoverCrossAccountNotQualified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base(source, card(source))
			tt.mutate(&req)
			plan := PlanProviderAccountCutover(req)
			require.Equal(t, tt.disposition, plan.Disposition)
			require.Equal(t, tt.code, plan.Code)
			require.Equal(t, tt.code == ProviderAccountCutoverReady, plan.Executable)
			require.NotEmpty(t, plan.Reason)
			if plan.Disposition == ProviderAccountCutoverBlocked {
				require.Empty(t, plan.Steps, "no instructions for an unsupported or unprepared path")
			}
			if !plan.Executable {
				for _, step := range plan.Steps {
					require.NotContains(t, step, "payment-method with the replacement", "update instructions only for the executable path")
				}
			}
		})
	}
}
