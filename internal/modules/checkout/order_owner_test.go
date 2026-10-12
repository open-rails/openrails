package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/orders"
)

func TestObservedOrderCannotEnterGenericCollection(t *testing.T) {
	for _, owner := range []string{"provider", "nmi_schedule", "", "unknown"} {
		t.Run(owner, func(t *testing.T) {
			// No database, resolver or provider is installed: an observed bill
			// must be refused before any of those dependencies are touched.
			var service CheckoutAttemptService
			attempt := uuid.New()
			order := &orders.Order{BillingOrder: gen.BillingOrder{
				ID: uuid.New(), CollectionOwner: owner, Status: string(billing.OrderOpen),
				PaymentStatus: string(billing.OrderPaymentRequiresAction), AttemptID: &attempt,
			}}
			payments := []OrderPayInput{
				{PaymentMethodID: uuid.New()},
				{Card: &OrderCard{Token: "unused"}},
				{Solana: &OrderSolana{TokenSymbol: "USDC"}},
				{Hosted: &OrderHosted{}},
			}
			for _, input := range payments {
				input.Order, input.User, input.Key = order, &UserIdentity{}, "refused"
				_, err := service.PayOrder(t.Context(), input)
				require.ErrorIs(t, err, orders.ErrNotPayable)
			}
			_, err := service.ConfirmOrder(t.Context(), order, billingauth.Payer{}, "unused")
			require.ErrorIs(t, err, orders.ErrNotPayable)
			next, err := service.OrderNextAction(t.Context(), order, billingauth.Payer{}, nil)
			require.NoError(t, err)
			require.Nil(t, next)
		})
	}
}
