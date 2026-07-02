package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// CancelMode describes how a subscription's cancellation behaves for a given
// rail (and, for NMI, the subscription's current lifecycle state). Registry-
// owned since #686 (rails.CancelMode); aliased here for the existing exported
// surface (handlers, pkg/service).
type CancelMode = rails.CancelMode

const (
	CancelModeReversible     = rails.CancelModeReversible
	CancelModeDestructive    = rails.CancelModeDestructive
	CancelModeExternalPortal = rails.CancelModeExternalPortal
)

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

// CancelModeFor returns the cancellation capability for a subscription — each
// rail's answer lives in its #669 descriptor (rails.Descriptor.CancelMode; NMI
// is state-conditional: reversible only while its deferred delete is pending,
// issue 216). A nil subscription or unknown rail yields CancelModeDestructive
// (the safe default).
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
// external_portal. Returns nil otherwise (every current rail since #696 moved
// CCBill onto the DataLink SMS cancel). The URL is the rail descriptor's (#686).
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
