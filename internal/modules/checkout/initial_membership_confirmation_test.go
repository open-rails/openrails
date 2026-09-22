package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInitialMembershipRequiresVerifiedInteractivePayer(t *testing.T) {
	mid := merchant.ID(uuid.New())
	customer := uuid.New()
	ctx := merchant.WithID(context.Background(), mid)
	correct := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: customer.String()}
	for _, name := range []string{"unknown", "automation", "invoker", "other_merchant", "other_customer"} {
		t.Run(name, func(t *testing.T) {
			p := correct
			switch name {
			case "unknown":
				p.CredentialClass = billingauth.CredentialClassUnknown
			case "automation":
				p.CredentialClass = billingauth.CredentialClassAutomation
			case "invoker":
				p.Invoker = "agent"
			case "other_merchant":
				p.MerchantID = uuid.NewString()
			case "other_customer":
				p.SubjectID = uuid.NewString()
			}
			_, err := (&CheckoutService{}).ConfirmInitialMembership(ctx, subscriptions.InitialMembershipTerms{CustomerID: customer}, "session-key", p, nil)
			var refusal *apperr.Error
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, 403, refusal.Status)
		})
	}
}
