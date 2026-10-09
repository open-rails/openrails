package subscriptions

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// #773 typed sentinels (the #750 pattern): one sentinel per constraint class,
// wrapped by a detail struct so callers can either errors.Is() the class or
// errors.As() for the specifics. Fail-closed — every reprice attempt that
// violates a constraint is refused, never silently coerced. Each carries its
// HTTP status and wire code (#983): constraints are 422, scheduling state 409.
var (
	// ErrRebillTermsCommitted prevents a price mutation from revoking an accepted
	// renewal whose provider preparation or money submission may have occurred.
	ErrRebillTermsCommitted = apperr.New(http.StatusConflict, "rebill_terms_committed", "accepted recurring payment owns the pending price terms")

	// ErrMembershipSlotTaken: the customer already holds a subscription of the
	// product or its tier group (or one whose provider stop is pending).
	ErrMembershipSlotTaken = errors.New("membership slot taken")

	// ErrRenewalInProgress refuses a cancel while an accepted renewal payment is
	// unresolved; the cancel succeeds once the payment resolves.
	ErrRenewalInProgress = apperr.New(http.StatusConflict, "payment_in_progress", "a renewal payment for this subscription is unresolved; cancel again once it resolves")

	// ErrRepriceCrossProduct: to_price must belong to the SAME product as the
	// subscription's current price. Cross-product moves are plan changes (a
	// different feature set) — out of scope for v1 (#778).
	ErrRepriceCrossProduct = apperr.New(http.StatusUnprocessableEntity, "reprice_cross_product", "reprice: to_price must be on the same product")

	// ErrRepriceCrossCurrency: to_price must match the subscription's current
	// currency — no FX surprises on a merchant-initiated transaction.
	ErrRepriceCrossCurrency = apperr.New(http.StatusUnprocessableEntity, "reprice_cross_currency", "reprice: to_price must be in the same currency")

	// ErrRepriceInactivePrice: to_price must be active (purchasable) — an
	// archived price cannot be scheduled as a reprice target.
	ErrRepriceInactivePrice = apperr.New(http.StatusUnprocessableEntity, "reprice_inactive_price", "reprice: to_price must be active")

	// ErrRepriceAlreadyScheduled: at most one scheduled reprice may exist per
	// subscription at a time (subscription_reprices_subscription_id_key) — cancel
	// the existing one first.
	ErrRepriceAlreadyScheduled = apperr.New(http.StatusConflict, "reprice_already_scheduled", "reprice: subscription already has a scheduled reprice")

	// ErrRepriceNotScheduled: the reprice is not (or is no longer) in
	// status=scheduled — it was already applied, canceled, or never existed.
	// Surfaced by both Cancel (cancel-before-effective) and the renewal-boundary
	// pickup (safe to retry).
	ErrRepriceNotScheduled = apperr.New(http.StatusConflict, "reprice_not_scheduled", "reprice: not in scheduled status")

	// ErrRepriceNoticeWindowViolation (#781): an INCREASE reprice (to_price
	// amount > from_price amount) whose effective_at is nearer than the
	// merchant's configured notice window (default DefaultRepriceNoticeWindowDays).
	// Decreases are exempt. Bypassed only by an explicit
	// AcknowledgeShortNotice on the request — never silently coerced.
	ErrRepriceNoticeWindowViolation = apperr.New(http.StatusUnprocessableEntity, "reprice_notice_window_violation", "reprice: effective_at is inside the merchant's configured notice window for a price increase")

	// ErrRepriceNotFound: no reprice row has that id for this merchant.
	ErrRepriceNotFound = apperr.New(http.StatusNotFound, "reprice_not_found", "reprice not found")
	// ErrRepriceTargetPriceNotFound: to_price names no price of this merchant.
	ErrRepriceTargetPriceNotFound = apperr.New(http.StatusNotFound, "reprice_target_price_not_found", "reprice: to_price not found")
	// ErrRepriceBatchNotFound: no batch has that id for this merchant.
	ErrRepriceBatchNotFound = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "reprice batch not found")
	// ErrRepricePriceKeyNotFound: the bulk key names no current price.
	ErrRepricePriceKeyNotFound = apperr.New(http.StatusNotFound, "reprice_price_key_not_found", "reprice: price key not found")
)

// DefaultRepriceNoticeWindowDays (#781) is the notice window used when a
// merchant has no explicit billing.merchant_configurations override — card
// networks and consumer-protection law generally require advance notice for
// recurring-amount increases; 30 days is the console's own long-standing UX
// default (#777's price-wizard-logic.ts), now also the server-side floor.
const DefaultRepriceNoticeWindowDays = 30

// RepriceConstraintError carries the offending ids for a refused reprice.
type RepriceConstraintError struct {
	Sentinel       error
	SubscriptionID uuid.UUID
	FromPriceID    uuid.UUID
	ToPriceID      uuid.UUID
}

func (e *RepriceConstraintError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v: subscription_id=%s from_price_id=%s to_price_id=%s",
		e.Sentinel, e.SubscriptionID, e.FromPriceID, e.ToPriceID)
}

func (e *RepriceConstraintError) Unwrap() error { return e.Sentinel }

// ref is a pointer to v.
func ref[T any](v T) *T { return &v }
