package subscriptions

import (
	"context"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
)

func init() {
	payments.EndRefundedMembership = EndRefundedMembership
}

// EndRefundedMembership stops the renewal of the membership a payment paid
// for once a refund that revoked its access has returned all of it. It ends
// now when the payment bought the latest period, else when the period ends,
// so a newer paid period stays. Only a membership OpenRails renews: a
// provider's schedule is canceled as its own action. Access a review keeps
// (a combined seat or plan change) keeps its renewal too. A renewal already
// sent is not waited for: one that lands on the canceled membership is
// refunded as a charge after cancel.
func EndRefundedMembership(ctx context.Context, d *db.DB, clock clockwork.Clock, payment *models.Payment, reason string) error {
	if payment.SubscriptionID == nil || payment.Metadata["refund_access_revoked"] != true {
		return nil
	}
	sub, err := NewSubscriptionRepo(d).GetByID(ctx, *payment.SubscriptionID)
	if err != nil || sub.CollectionPolicy != models.CollectionPolicyEngine {
		return err
	}
	refunded, err := payments.NewPaymentService(d, clock).RefundTotals(ctx, []uuid.UUID{payment.ID})
	if err != nil || refunded[payment.ID] < payment.Amount {
		return err
	}
	latest, err := d.Gen(ctx).GetLatestPaidPaymentIDForSubscription(ctx, gen.GetLatestPaidPaymentIDForSubscriptionParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil && !db.IsNotFound(err) {
		return err
	}
	endNow := latest == payment.ID
	switch {
	case sub.Status == models.StatusActive, sub.Status == models.StatusPastDue, sub.Status == models.StatusAwaitingMethod:
	case endNow && sub.Status == models.StatusCanceled && (sub.EndedAt == nil || sub.EndedAt.After(clock.Now())): // canceled at period end: it ends now
	default:
		return nil
	}
	if reason == "" {
		reason = "refunded"
	}
	_, err = NewSubscriptionLifecycleService(d, nil, nil, nil, nil, nil, clock).CancelMembershipTx(ctx, d, &CancelMembershipParams{
		SubscriptionID: &sub.ID, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: endNow,
		EndedReason: PremiumEndReasonRefund, Requested: true,
	})
	return err
}
