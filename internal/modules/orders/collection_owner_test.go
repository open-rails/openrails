package orders

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

func TestCollectionOwnerControlsOrderActions(t *testing.T) {
	psp := uuid.New()
	for _, owner := range []string{"engine", "provider", "nmi_schedule", ""} {
		t.Run(owner, func(t *testing.T) {
			order := Order{BillingOrder: gen.BillingOrder{
				ID: uuid.New(), CollectionOwner: owner, PspID: &psp,
				Status: string(billing.OrderOpen), PaymentStatus: string(billing.OrderPaymentRequiresAction),
			}}
			view := order.View(&billing.NextAction{Type: "authenticate"}, []billing.OrderPaymentOption{{PSPID: billing.PSPID(psp), Rail: "stripe"}})
			require.Equal(t, billing.OrderCollectionOwner(owner), view.CollectionOwner)
			require.Equal(t, billing.PSPID(psp), *view.CollectionPSPID)
			require.Equal(t, owner == "engine", Payable(order.BillingOrder, time.Now()))
			if owner == "engine" {
				require.Len(t, view.PaymentOptions, 1)
				require.NotNil(t, view.Payment.NextAction)
			} else {
				require.Empty(t, view.PaymentOptions)
				require.Nil(t, view.Payment.NextAction)
			}
		})
	}
}
