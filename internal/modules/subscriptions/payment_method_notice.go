package subscriptions

import (
	"context"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// Notice ladder resolutions (or#870 bucket 2). None of them changes the
// subscription — they record why the ladder stopped asking.
const (
	// NoticeResolutionRecovered: the subscription left the parked state. The
	// customer updated the card, or the provider-verification plane resolved
	// the row. Either way there is nothing left to ask for.
	NoticeResolutionRecovered = "recovered"
	// NoticeResolutionEnded: the subscription is over. Asking someone to fix
	// the card on a cancelled subscription is worse than saying nothing.
	NoticeResolutionEnded = "ended"
	// NoticeResolutionExhausted: every rung was sent. The subscription stays
	// exactly as it was — parked, entitlements intact, stored payment method
	// untouched. The ladder ran out of things to say, not out of patience.
	NoticeResolutionExhausted = "exhausted"
)

// openPaymentMethodNoticeLadder starts (or restarts) the bucket-2 ladder for a
// parked subscription, inside the caller's transaction.
//
// Restart-on-conflict is deliberate: a fresh bucket-2 decline is a fresh
// problem, so the rungs re-anchor on the new park rather than inheriting a
// ladder that may already be spent.
//
// Best-effort by design. The money decision and the lifecycle transition are
// what must be durable; failing the whole failure-flow transaction because a
// REMINDER could not be scheduled would trade a missed notice for a missed
// state change, which is the worse of the two.
func openPaymentMethodNoticeLadder(ctx context.Context, tx pgx.Tx, subscription *models.Subscription, failureCode *string, now time.Time) {
	next, ok := collection.NextPaymentMethodNoticeAt(1, now)
	var nextAt *time.Time
	if ok {
		nextAt = &next
	}
	if _, err := gen.New(tx).OpenPaymentMethodNotice(ctx, gen.OpenPaymentMethodNoticeParams{
		MerchantID:     subscription.MerchantID,
		CustomerID:     subscription.CustomerID,
		SubscriptionID: subscription.ID,
		Rail:           string(subscription.Rail),
		FailureCode:    normalize.OptionalString(normalize.FromPtr(failureCode)),
		ParkedAt:       now,
		NextNoticeAt:   nextAt,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"customer_id":     subscription.CustomerID,
		}).Error("or#870: failed to open the payment-method notice ladder; the customer got the first notice but will get no reminders")
	}
}
