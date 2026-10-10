package subscriptions

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// CancelMode describes how a subscription's cancellation behaves on its rail
// (for NMI, also its lifecycle state). It is rails.CancelMode.
type CancelMode = rails.CancelMode

const (
	CancelModeReversible     = rails.CancelModeReversible
	CancelModeDestructive    = rails.CancelModeDestructive
	CancelModeExternalPortal = rails.CancelModeExternalPortal
)

// NMIDeleteSafetyMargin is how far ahead of the paid-period end the deferred
// NMI delete_subscription fires. NMI rebills at current_period_ends_at, so the
// delete must land strictly before it or a canceled customer is billed; the
// margin leaves River room to retry. With less margin left, or an unknown or
// past period end, the delete is immediate.
const NMIDeleteSafetyMargin = 48 * time.Hour

// NMIDeferredDeleteAt returns (deleteAt, true) when an NMI cancellation should
// defer the rail-side delete to periodEnd - margin (still in the future), or
// (zero, false) when it must delete immediately.
func NMIDeferredDeleteAt(sub *models.Subscription, now time.Time) (time.Time, bool) {
	if sub == nil {
		return time.Time{}, false
	}
	if !periodEndsInFuture(sub, now) {
		return time.Time{}, false
	}
	deleteAt := sub.CurrentPeriodEndsAt.Add(-NMIDeleteSafetyMargin)
	if !deleteAt.After(now) {
		// The pre-rebill window has opened: too close to defer.
		return time.Time{}, false
	}
	return deleteAt, true
}

// SystemDeleteCoolingOff is how long an automated (system-origin) terminal
// cancellation waits before its irreversible rail-side delete is due, so a
// cancel our own malfunction produced can be caught. NMIDeleteHandler
// re-reads the row at execution and supersedes the delete unless it is still
// canceled-awaiting-delete, so a late renewal, a converge resurrection or an
// operator undo inside the window leaves the rail schedule intact.
const SystemDeleteCoolingOff = 24 * time.Hour

// SystemDeferredDeleteAt is when an automated terminal cancellation's rail-side
// delete is due: now + SystemDeleteCoolingOff, clamped to land before a known
// future rebill (as NMIDeleteSafetyMargin), and immediate once that margin has
// opened. Past its period end there is no known charge date, so the full
// window applies.
func SystemDeferredDeleteAt(sub *models.Subscription, now time.Time) time.Time {
	due := now.Add(SystemDeleteCoolingOff)
	if !periodEndsInFuture(sub, now) {
		return due
	}
	margin := sub.CurrentPeriodEndsAt.Add(-NMIDeleteSafetyMargin)
	if due.Before(margin) {
		return due
	}
	if margin.Before(now) {
		return now
	}
	return margin
}

// CancelModeFor returns the subscription's cancellation capability from its
// rail descriptor (NMI is reversible only while its deferred delete is
// pending). A nil subscription or unknown rail yields CancelModeDestructive.
func CancelModeFor(sub *models.Subscription, now time.Time) CancelMode {
	return rails.CancelModeFor(sub, now)
}

// periodEndsInFuture reports whether the subscription's current paid period ends
// strictly after now. A nil/zero period end is treated as not-in-future.
func periodEndsInFuture(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return false
	}
	return sub.CurrentPeriodEndsAt.After(now)
}

// CancelScheduled reports whether a subscription is canceled but still paid
// through the current period, regardless of whether the cancel can be undone.
func CancelScheduled(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	if sub.Status != models.StatusCanceled {
		return false
	}
	return periodEndsInFuture(sub, now)
}

// Resumable is the one resume gate (handler, worker, DTO and facade): the
// cancel is reversible on this rail, the subscription is canceled, and its
// paid period is still in the future.
func Resumable(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	// Engine resume only undoes an ordinary user cancellation. Terminal
	// failures, merchant revocation and chargebacks require a new decision.
	if sub.CollectionPolicy == models.CollectionPolicyEngine && (sub.CancelType == nil || *sub.CancelType != models.CancelTypeUser || (sub.PaymentMethodID == nil && !FollowsDefault(sub))) {
		return false
	}
	if CancelModeFor(sub, now) != CancelModeReversible {
		return false
	}
	if sub.Status != models.StatusCanceled {
		return false
	}
	return periodEndsInFuture(sub, now)
}

// CancelPortalURL returns the rail descriptor's consumer-portal URL when the
// cancel mode is external_portal, else nil (no current rail uses it).
func CancelPortalURL(sub *models.Subscription, now time.Time) *string {
	if CancelModeFor(sub, now) != CancelModeExternalPortal {
		return nil
	}
	url := rails.CancelPortalURL(sub.Rail)
	if url == "" {
		return nil
	}
	return &url
}

// PaidRunwayRefusal refuses buying a product again while a canceled
// subscription of it is still paid: a resumable one is resumed instead, any
// other is bought again once its paid period ends.
func PaidRunwayRefusal(sub *models.Subscription, now time.Time) error {
	end := sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339)
	if Resumable(sub, now) {
		return apperr.New(http.StatusConflict, billing.CodeSubscriptionResumable, fmt.Sprintf("the customer's canceled subscription %s to this product is paid through %s; resume it instead of buying again", billing.SubscriptionID(sub.ID), end))
	}
	return apperr.New(http.StatusConflict, billing.CodeSubscriptionPaidThrough, fmt.Sprintf("the customer's canceled subscription %s to this product is paid through %s; buy again once it ends", billing.SubscriptionID(sub.ID), end))
}

// IsMembershipSlotTaken reports whether creating a membership failed because
// the customer's product or tier-group slot is already held.
func IsMembershipSlotTaken(err error) bool {
	if errors.Is(err, ErrMembershipSlotTaken) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "subscriptions_customer_id_product_id_key" || pgErr.ConstraintName == "subscriptions_customer_id_tier_group_key")
}
