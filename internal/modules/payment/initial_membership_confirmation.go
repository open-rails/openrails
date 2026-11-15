package payment

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// enrollmentInstrument freezes a new subscription's card and the recurring
// lineage its enrollment cites: the card's on the account, or none when the
// enrollment charge stores it. On Stripe every enrollment's PaymentIntent
// stores its own agreement (setup_future_usage), and Stripe links the rest.
func enrollmentInstrument(ctx context.Context, q *gen.Queries, method gen.BillingPaymentMethod, psp uuid.UUID) (charge.FrozenInstrument, error) {
	instrument := charge.FreezeInstrument(method, psp)
	if method.Rail == string(models.RailStripe) {
		return instrument, nil
	}
	var err error
	instrument.Mandate, err = mandates.Citable(ctx, q, method.MerchantID, method.CustomerID, method.ID, psp, method.Rail, charge.AgreementRecurring)
	return instrument, err
}
