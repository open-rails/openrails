package payment

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestStaffChangesNeverAdmitImmediateEngineCharges(t *testing.T) {
	service := &PurchaseService{}
	_, err := service.enqueueEngineChange(t.Context(), &SubscriptionChangeRequest{Staff: true}, nil, nil, subscriptions.InitialMembershipTerms{})
	require.ErrorIs(t, err, errStaffImmediateCharge, "refuse before any account lookup or provider intent")
	for _, staff := range []bool{false, true} {
		for _, tier := range []bool{false, true} {
			target := &changeTarget{sub: &models.Subscription{Quantity: new(1)}, quantity: new(2), tier: tier, upgrade: true}
			if staff {
				target.staff = &subscriptions.StaffChange{Invoker: "support", Reason: "customer request"}
			}
			require.Equal(t, staff, target.deferred(), "staff=%v tier=%v", staff, tier)
		}
	}
}
