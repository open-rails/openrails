package disputes

import (
	"context"
	"errors"
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
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// BlockCard keeps the card a disputed charge was taken from from being
// charged again: the card is disputed and its mandates end, so engine
// subscriptions on it wait for another card. Only the card the charge names
// is blocked; a charge that names none blocks nothing. review, when a
// chargeback calls for it, tells staff which.
func (s *Service) BlockCard(ctx context.Context, d *db.DB, payment *models.Payment, review *gen.BillingDispute) error {
	if payment == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := d.NewWithPgxTx(tx)
		note := "The card the charge was taken from is blocked."
		switch {
		case payment.Rail == models.RailCCBill:
			note = "CCBill holds the card, so OpenRails blocked none."
		case payment.PaymentMethodID == nil:
			note = "No saved card is known to have made the charge, so none was blocked."
		default:
			q := txDB.Gen(ctx)
			if _, err := q.MarkPaymentMethodDisputed(ctx, gen.MarkPaymentMethodDisputedParams{MerchantID: mid.UUID(), ID: *payment.PaymentMethodID}); err != nil {
				return fmt.Errorf("block disputed card: %w", err)
			}
			if _, err := mandates.EndForPaymentMethod(ctx, q, mid.UUID(), *payment.PaymentMethodID, mandates.EndDisputed, s.now()); err != nil {
				return fmt.Errorf("end the disputed card's mandates: %w", err)
			}
		}
		if review == nil {
			return nil
		}
		return raiseCustomerReview(ctx, txDB, *review, note)
	})
}

// providerBilledOnCard are the customer's live subscriptions a provider bills
// on the charge's card, but for those the charge itself paid for.
func providerBilledOnCard(ctx context.Context, d *db.DB, payment *models.Payment) ([]uuid.UUID, error) {
	if payment == nil || payment.PaymentMethodID == nil {
		return nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	subs, err := d.Gen(ctx).ListLiveSubscriptionsOnCard(ctx, gen.ListLiveSubscriptionsOnCardParams{MerchantID: mid.UUID(), CustomerID: payment.CustomerID, PaymentMethodID: *payment.PaymentMethodID})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions on the disputed card: %w", err)
	}
	own, err := ChargeSubscriptions(ctx, d, payment)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, sub := range subs {
		if sub.CollectionPolicy != string(models.CollectionPolicyEngine) && !slices.Contains(own, sub.ID) {
			out = append(out, sub.ID)
		}
	}
	return out, nil
}

// ChargeSubscriptions are the subscriptions a charge paid for: its own and
// its invoice's.
func ChargeSubscriptions(ctx context.Context, d *db.DB, payment *models.Payment) ([]uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	if payment.SubscriptionID != nil {
		ids = append(ids, *payment.SubscriptionID)
	}
	if payment.InvoiceID == nil {
		return ids, nil
	}
	lines, err := d.Gen(ctx).ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: mid.UUID(), InvoiceIds: []uuid.UUID{*payment.InvoiceID}})
	if err != nil {
		return nil, fmt.Errorf("load the charge's invoice lines: %w", err)
	}
	for _, line := range lines {
		if line.SubscriptionID != nil && !slices.Contains(ids, *line.SubscriptionID) {
			ids = append(ids, *line.SubscriptionID)
		}
	}
	return ids, nil
}

// endSubscription cancels a subscription and queues its provider's cancel:
// at once with its access when revoke, else at period end. A subscription
// no longer active is left as it is.
func endSubscription(ctx context.Context, d *db.DB, lc *subscriptions.SubscriptionLifecycleService, id uuid.UUID, cancelType models.CancelType, revoke bool, cause string) error {
	sub, err := subscriptions.NewSubscriptionRepo(d).GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load subscription %s: %w", id, err)
	}
	rail := sub.Rail
	_, err = lc.CancelMembershipTx(ctx, d, &subscriptions.CancelMembershipParams{
		SubscriptionID: &id, Rail: &rail, CancelType: cancelType, CancelFeedback: &cause, RevokeAccess: revoke,
	})
	if errors.Is(err, subscriptions.ErrSubscriptionNotActive) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cancel subscription %s: %w", id, err)
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
// against: whether to keep serving them is staff's decision. note says what
// OpenRails did about the card.
func raiseCustomerReview(ctx context.Context, d *db.DB, dispute gen.BillingDispute, note string) error {
	if dispute.CustomerID == nil {
		return nil
	}
	count, err := d.Gen(ctx).CountCustomerChargebacks(ctx, gen.CountCustomerChargebacksParams{MerchantID: dispute.MerchantID, CustomerID: *dispute.CustomerID})
	if err != nil {
		return fmt.Errorf("count the customer's chargebacks: %w", err)
	}
	id := billing.CustomerID(*dispute.CustomerID).String()
	action := fmt.Sprintf("Customer %s filed a chargeback (%s, %s); %d in all.", id, dispute.Reason, billing.DisputeID(dispute.ID).String(), count)
	if note != "" {
		action += " " + note
	}
	var refunded, chargeAmount int64
	var currency string
	if dispute.PaymentID != nil {
		charge, err := d.Gen(ctx).GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: dispute.MerchantID, ID: *dispute.PaymentID})
		if err != nil {
			return fmt.Errorf("read disputed charge: %w", err)
		}
		chargeAmount, currency = charge.Amount, charge.Currency
		rows, err := d.Gen(ctx).ListRefundsForPayment(ctx, gen.ListRefundsForPaymentParams{MerchantID: dispute.MerchantID, RefundedPaymentID: dispute.PaymentID})
		if err != nil {
			return fmt.Errorf("read disputed charge refunds: %w", err)
		}
		for _, row := range rows {
			if row.Status == "succeeded" && row.ReversalKind != nil && *row.ReversalKind == "refund" {
				refunded -= row.Amount
			}
		}
	}
	if refunded > 0 {
		action += fmt.Sprintf(" The charge was already refunded (%s of %s): contest the chargeback with your processor and cite the refund.", moneyutil.FormatAmount(refunded, currency), moneyutil.FormatAmount(chargeAmount, currency))
	} else {
		action += " OpenRails does not block customers; to stop serving this customer, suspend their account in your application."
	}
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
