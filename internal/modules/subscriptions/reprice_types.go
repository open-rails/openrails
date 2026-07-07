package subscriptions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// #773 typed sentinels (the #750 pattern): one sentinel per constraint class,
// wrapped by a detail struct so callers can either errors.Is() the class or
// errors.As() for the specifics. Fail-closed — every reprice attempt that
// violates a constraint is refused, never silently coerced.
var (
	// ErrRepriceCrossProduct: to_price must belong to the SAME product as the
	// subscription's current price. Cross-product moves are plan changes (a
	// different feature set) — out of scope for v1 (#778).
	ErrRepriceCrossProduct = errors.New("reprice: to_price must be on the same product")

	// ErrRepriceCrossCurrency: to_price must match the subscription's current
	// currency — no FX surprises on a merchant-initiated transaction.
	ErrRepriceCrossCurrency = errors.New("reprice: to_price must be in the same currency")

	// ErrRepriceInactivePrice: to_price must be active (purchasable) — an
	// archived price cannot be scheduled as a reprice target.
	ErrRepriceInactivePrice = errors.New("reprice: to_price must be active")

	// ErrRepriceAlreadyScheduled: at most one scheduled reprice may exist per
	// subscription at a time (uq_subscription_reprices_one_scheduled) — cancel
	// the existing one first.
	ErrRepriceAlreadyScheduled = errors.New("reprice: subscription already has a scheduled reprice")

	// ErrRepriceNotScheduled: the reprice is not (or is no longer) in
	// status=scheduled — it was already applied, canceled, or never existed.
	// Surfaced by both Cancel (cancel-before-effective) and the renewal-boundary
	// pickup (safe to retry).
	ErrRepriceNotScheduled = errors.New("reprice: not in scheduled status")
)

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

// RepriceRequest is a single subscription's reprice(subscription, to_price,
// effective) call.
type RepriceRequest struct {
	SubscriptionID uuid.UUID
	ToPriceID      uuid.UUID
	EffectiveAt    time.Time
}

// RepriceAllPriorVersionsRequest is the bulk reprice_all_prior_versions(key,
// effective_date) call: every active subscription pinned to a PRIOR version
// of PriceKey (the archived members of its version chain) is scheduled to
// move to the key's CURRENT price at EffectiveAt.
type RepriceAllPriorVersionsRequest struct {
	PriceKey    string
	EffectiveAt time.Time
}

// RepriceBatchResult summarizes a bulk reprice_all_prior_versions call. JSON
// tags match the shape documented for #777 console consumers: {batch_id,
// to_price_id, matched, scheduled:[...], skipped:[...]}.
type RepriceBatchResult struct {
	BatchID   uuid.UUID        `json:"batch_id"`
	ToPriceID uuid.UUID        `json:"to_price_id"`
	Matched   int              `json:"matched"`
	Scheduled []RepriceOutcome `json:"scheduled"`
	Skipped   []RepriceOutcome `json:"skipped"`
}

// RepriceOutcome is one subscription's result within a bulk reprice.
type RepriceOutcome struct {
	SubscriptionID uuid.UUID `json:"subscription_id"`
	RepriceID      uuid.UUID `json:"reprice_id,omitempty"` // zero when Skipped
	Reason         string    `json:"reason,omitempty"`     // set when skipped (constraint violation)
}

// RepricePreviewResult is the #777 read-only "affected-count preview": how
// many active subscriptions would move if the wizard's migration step ran
// right now, WITHOUT scheduling anything. Computed over the key's WHOLE
// version chain (current + archived) rather than #773's "prior versions"
// definition, because preview always runs BEFORE the price-edit step creates
// the new version — at that moment every existing subscriber on the key is
// still, by definition, a "prior version" candidate once the bump lands.
type RepricePreviewResult struct {
	PriceKey  string    `json:"price_key"`
	ToPriceID uuid.UUID `json:"to_price_id"`
	Matched   int       `json:"matched"`
}
