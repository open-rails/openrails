package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// CancelMode describes how a subscription's cancellation behaves for a given
// rail (and, for NMI, the subscription's current lifecycle state). It is a
// capability classification — NOT a rail-name check — so that callers can
// reason about "can this be undone" uniformly across rails.
type CancelMode string

const (
	// CancelModeReversible: cancellation can be undone in-place before the paid
	// period ends (Stripe's cancel_at_period_end; NMI while a deferred delete is
	// still pending — see issue 216).
	CancelModeReversible CancelMode = "reversible"

	// CancelModeDestructive: cancellation tears down the rail-side
	// subscription immediately and cannot be undone (NMI delete_subscription
	// once executed, Solana).
	CancelModeDestructive CancelMode = "destructive"

	// CancelModeExternalPortal: we cannot cancel or resume from our system; the
	// user must use the rail's own consumer portal (CCBill).
	CancelModeExternalPortal CancelMode = "external_portal"
)

// CCBillSupportPortalURL is the consumer support portal CCBill subscribers must
// use to manage (cancel) their subscription. Mirrors CCBillCancelError.SupportURL.
const CCBillSupportPortalURL = "https://support.ccbill.com"

// NMIDeleteSafetyMargin is how far ahead of the paid-period end we fire the
// deferred NMI delete_subscription (issue 216).
//
// Timing assumption: NMI attempts the recurring auto-charge AT the period end
// (current_period_ends_at). We must delete the recurring subscription on the NMI
// side strictly BEFORE that charge fires, or the user gets billed after they
// cancelled. A 48h margin gives River ample room to run the job (and retry on
// transient failures) before NMI's rebill window opens, while still preserving
// the user's full paid access right up to ~48h before expiry as an undo window.
//
// Corollary: if a cancel arrives when there is less than this margin left
// (now >= current_period_ends_at - 48h), or the period end is unknown/past, we
// must delete IMMEDIATELY — there is no safe room to defer.
const NMIDeleteSafetyMargin = 48 * time.Hour

// NMIDeferredDeleteAt returns (deleteAt, true) when an NMI-backed cancellation
// should DEFER the rail-side delete to a scheduled time (a genuine future
// undo window exists), or (zero, false) when it must delete IMMEDIATELY.
//
// Defer only when there is a real future window: the paid period end is known,
// in the future, and far enough out that deleteAt = periodEnd - margin is still
// in the future. Otherwise (window already open, or period unknown/past) the
// caller must delete inline exactly as before.
func NMIDeferredDeleteAt(sub *models.Subscription, now time.Time) (time.Time, bool) {
	if sub == nil {
		return time.Time{}, false
	}
	if !periodEndsInFuture(sub, now) {
		return time.Time{}, false
	}
	deleteAt := sub.CurrentPeriodEndsAt.Add(-NMIDeleteSafetyMargin)
	if !deleteAt.After(now) {
		// The 48h pre-rebill window has already opened — too close to defer safely.
		return time.Time{}, false
	}
	return deleteAt, true
}

// CancelModeFor returns the cancellation capability for a subscription.
//
// It takes the full subscription (not just the rail) because NMI is
// conditionally reversible: while the subscription is cancelled, its deferred
// delete_subscription has not yet executed, and the paid period is still in the
// future (issue 216), the cancellation can be undone. Once the delete fires
// (DeletionScheduledAt cleared by the finalizer, or period elapsed) it is
// destructive like before.
//
// A nil subscription yields CancelModeDestructive (the safe default).
func CancelModeFor(sub *models.Subscription, now time.Time) CancelMode {
	if sub == nil {
		return CancelModeDestructive
	}

	switch {
	case sub.Rail == models.RailStripe:
		return CancelModeReversible
	case sub.Rail == models.RailCCBill:
		return CancelModeExternalPortal
	case rails.IsNMIBackedRail(sub.Rail):
		// NMI is reversible only while a deferred delete is still pending and the
		// paid-through window is in the future. nmiDeletePending encapsulates that.
		if nmiDeletePending(sub, now) {
			return CancelModeReversible
		}
		return CancelModeDestructive
	default:
		// Solana and any other self-contained rail: destructive.
		return CancelModeDestructive
	}
}

// nmiDeletePending reports whether an NMI-backed subscription has been cancelled
// with a deferred delete that has not yet executed and whose paid period is
// still in the future. This is the window during which an NMI cancellation can
// be resumed (issue 216).
//
// The deferred delete is finalized by clearing DeletionScheduledAt (the River
// finalizer sets it to nil after calling DeleteRecurringSubscription), so a
// cancelled NMI subscription with a non-nil DeletionScheduledAt and a future
// period end is still recoverable.
func nmiDeletePending(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	if sub.Status != models.StatusCancelled {
		return false
	}
	if sub.DeletionScheduledAt == nil || sub.DeletionScheduledAt.IsZero() {
		return false
	}
	return periodEndsInFuture(sub, now)
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

// CancelScheduled reports whether a subscription is cancelled but the user still
// retains paid access until the end of the current period (status == cancelled
// && current_period_ends_at > now). This is independent of whether the cancel
// can be undone.
func CancelScheduled(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	if sub.Status != models.StatusCancelled {
		return false
	}
	return periodEndsInFuture(sub, now)
}

// Resumable is the single shared predicate for "can this subscription be resumed
// right now". It is true when the cancellation is reversible for this rail
// AND the subscription is cancelled AND the paid period is still in the future.
//
// This is the ONE place that gates resume — the HTTP handler, the River worker,
// the public DTO, and the library facade all consult it so they cannot drift.
func Resumable(sub *models.Subscription, now time.Time) bool {
	if sub == nil {
		return false
	}
	if CancelModeFor(sub, now) != CancelModeReversible {
		return false
	}
	if sub.Status != models.StatusCancelled {
		return false
	}
	return periodEndsInFuture(sub, now)
}

// CancelPortalURL returns the external consumer-portal URL a client should send
// the user to in order to manage their subscription, when the cancel mode is
// external_portal (CCBill). Returns nil otherwise.
func CancelPortalURL(sub *models.Subscription, now time.Time) *string {
	if CancelModeFor(sub, now) != CancelModeExternalPortal {
		return nil
	}
	url := CCBillSupportPortalURL
	return &url
}
