package disputes

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// BlockCard keeps the card a disputed charge was taken from from being
// charged again: the card is disputed and its mandates end, so engine
// subscriptions on it wait for another card, and the customer's other
// subscriptions a provider bills on it are canceled to end at period end.
// ended are the subscriptions the dispute already ended. A charge whose card
// OpenRails does not hold blocks nothing.
func (s *Service) BlockCard(ctx context.Context, d *db.DB, lc *subscriptions.SubscriptionLifecycleService, payment *models.Payment, cause string, ended []uuid.UUID) error {
	if payment == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := d.NewWithPgxTx(tx)
		q := txDB.Gen(ctx)
		cards, err := q.ListChargeCards(ctx, gen.ListChargeCardsParams{MerchantID: mid.UUID(), PaymentID: payment.ID})
		if err != nil {
			return fmt.Errorf("find the disputed card: %w", err)
		}
		now := s.now()
		for _, card := range cards {
			if _, err := q.MarkPaymentMethodDisputed(ctx, gen.MarkPaymentMethodDisputedParams{MerchantID: mid.UUID(), ID: card.ID}); err != nil {
				return fmt.Errorf("block disputed card: %w", err)
			}
			if _, err := mandates.EndForPaymentMethod(ctx, q, mid.UUID(), card.ID, mandates.EndDisputed, now); err != nil {
				return fmt.Errorf("end the disputed card's mandates: %w", err)
			}
			subs, err := q.ListLiveSubscriptionsOnCard(ctx, gen.ListLiveSubscriptionsOnCardParams{MerchantID: mid.UUID(), CustomerID: payment.CustomerID, PaymentMethodID: card.ID})
			if err != nil {
				return fmt.Errorf("list subscriptions on the disputed card: %w", err)
			}
			for _, sub := range subs {
				if slices.Contains(ended, sub.ID) || sub.CollectionPolicy == string(models.CollectionPolicyEngine) || lc == nil {
					continue
				}
				if err := endAtPeriodEnd(ctx, txDB, lc, sub.ID, cause); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// endAtPeriodEnd cancels a provider-billed subscription without ending its
// paid access, and queues the provider's cancel.
func endAtPeriodEnd(ctx context.Context, d *db.DB, lc *subscriptions.SubscriptionLifecycleService, id uuid.UUID, cause string) error {
	sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id)
	if err != nil {
		return fmt.Errorf("lock subscription %s: %w", id, err)
	}
	rail := sub.Rail
	if _, err := lc.CancelMembershipTx(ctx, d, &subscriptions.CancelMembershipParams{
		SubscriptionID: &id, Rail: &rail, CancelType: models.CancelTypeChargeback, CancelFeedback: &cause,
	}); err != nil {
		return fmt.Errorf("cancel subscription %s on the disputed card: %w", id, err)
	}
	if sub, err = subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id); err != nil {
		return fmt.Errorf("reload subscription %s: %w", id, err)
	}
	if err := intents.NewProviderCancelScheduler(d, nil, intents.OriginProvider, cause).ScheduleProviderCancel(ctx, sub, timeutil.FirstClock(lc.Clock()).Now()); err != nil {
		return fmt.Errorf("queue provider cancel for %s: %w", id, err)
	}
	return nil
}

// raiseCustomerReview asks staff to review a customer a chargeback was filed
// against. Only the disputed card is blocked: whether to keep serving the
// customer is staff's decision.
func raiseCustomerReview(ctx context.Context, d *db.DB, customer uuid.UUID, dispute gen.BillingDispute) error {
	count, err := d.Gen(ctx).CountCustomerChargebacks(ctx, gen.CountCustomerChargebacksParams{MerchantID: dispute.MerchantID, CustomerID: customer})
	if err != nil {
		return fmt.Errorf("count the customer's chargebacks: %w", err)
	}
	id := billing.CustomerID(customer).String()
	action := fmt.Sprintf("Customer %s filed a chargeback (%s, %s); %d in all. The card is blocked. Review the customer: refund or block further purchases if the pattern continues.",
		id, dispute.Reason, billing.DisputeID(dispute.ID).String(), count)
	_, err = (&reconcile.PGStore{DB: d}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingCustomerReview, SubjectKey: id, Severity: reconcile.SeverityMedium, RecommendedAction: action,
		Evidence: map[string]any{"customer_id": id, "dispute_id": billing.DisputeID(dispute.ID).String(), "reason": dispute.Reason,
			"network": dispute.Network, "chargebacks": count},
	})
	if err != nil {
		return fmt.Errorf("raise customer review: %w", err)
	}
	return nil
}
