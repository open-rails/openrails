package intents

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
)

// CustomerRetryCap is how many declined customer attempts one renewal takes
// on one card; then only another card is charged.
const CustomerRetryCap = 3

// ErrRetryNeedsNewMethod refuses a customer's retry on a card this renewal
// does not charge again. Another payment method is admitted.
var ErrRetryNeedsNewMethod = fmt.Errorf("%w: this renewal does not charge the card again", ErrRebillNotRetryable)

// AdmitCustomerRetry decides, under the subscription lock and before any
// provider call, whether the customer may charge method for the renewal due
// now. Refused: past the cap; after a refusal that ended charging (a held
// terminal) or an issuer code that forbids a reattempt; and, awaiting a new
// card, unless the latest refusal is one the cardholder can clear on this
// card. It refuses only: nothing here cancels or revokes.
func AdmitCustomerRetry(ctx context.Context, q *gen.Queries, sub *models.Subscription, method uuid.UUID) error {
	if sub.CurrentPeriodEndsAt == nil {
		return ErrRebillNotRetryable
	}
	standing, err := q.GetCycleCardStanding(ctx, gen.GetCycleCardStandingParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, DueAt: sub.CurrentPeriodEndsAt.UTC(), PaymentMethodID: method})
	if err != nil {
		return err
	}
	switch {
	case standing.CustomerDeclines >= CustomerRetryCap, standing.Action == string(decline.NonRecoverable), decline.IssuerForbidsReattempt(standing.IssuerCode):
		return ErrRetryNeedsNewMethod
	case sub.Status == models.StatusAwaitingMethod && (standing.Declines == 0 || !decline.HolderCanClear(billing.DeclineReason(standing.Reason))):
		return ErrRetryNeedsNewMethod
	}
	return nil
}
