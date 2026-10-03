package checkout

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

type TierChangeRequest struct {
	PriceID        string    `json:"price_id"`
	SubscriptionID uuid.UUID `json:"-"`
	IdempotencyKey string    `json:"-"`
}

var (
	ErrTierChangeNoSubscription = errors.New("no active subscription found")
	ErrTierChangeNotSupported   = errors.New("tier change not supported for this rail")
	ErrTierChangeBlocked        = errors.New("tier change blocked")
	ErrTierChangePending        = errors.New("tier change already pending")
	ErrTierChangeSameProduct    = errors.New("already on this plan")
	ErrTierChangeDifferentGroup = errors.New("cannot change to a different tier group")
	// ErrTierChangeCrossCurrency (#820): proration subtracts the old plan's
	// unused value from the new plan's price, which is only meaningful inside
	// one currency. Wraps the repo-wide FX sentinel used by reprice and plan
	// migration, so every FX-crossing plan move answers to one errors.Is.
	ErrTierChangeCrossCurrency = fmt.Errorf("cannot change to a plan in a different currency: %w", subscriptions.ErrRepriceCrossCurrency)
)

// PriceAmount is an amount together with the currency it is denominated in.
// Money crosses API boundaries as this pair, never a bare int64, so an FX
// boundary cannot be crossed by accident (#820). Micros are the system-wide
// money unit; no float ever represents or converts one.
type PriceAmount struct {
	Micros   int64
	Currency string
}

// PriceAmountOf lifts a catalog price into its (amount, currency) pair. A nil
// price yields the zero value — no amount AND no currency — which fails the
// currency guard instead of quietly prorating against an invented currency.
func PriceAmountOf(p *models.Price) PriceAmount {
	if p == nil {
		return PriceAmount{}
	}
	return PriceAmount{Micros: p.Amount, Currency: p.Currency}
}

func normalizedCurrency(c string) string { return strings.ToLower(strings.TrimSpace(c)) }

// RequireSameCurrency refuses an absent currency as firmly as a mismatched one:
// a missing currency is never defaulted or invented (docs/invariants.md).
func RequireSameCurrency(old, new PriceAmount) error {
	oldCurrency, newCurrency := normalizedCurrency(old.Currency), normalizedCurrency(new.Currency)
	if oldCurrency == "" || newCurrency == "" || oldCurrency != newCurrency {
		return fmt.Errorf("%w (from %q to %q)", ErrTierChangeCrossCurrency, old.Currency, new.Currency)
	}
	return nil
}

type TierChangeError struct {
	HTTPStatus int
	Message    string
	Code       string
}

func (e *TierChangeError) Error() string {
	return e.Message
}

// Is matches a coded refusal sentinel by its wire code.
func (e *TierChangeError) Is(target error) bool {
	other, ok := target.(*TierChangeError)
	return ok && e.Code != "" && other.Code == e.Code
}

// tierChangeKeyRequired refuses a tier change without a client
// Idempotency-Key before anything is admitted or mutated: the key is the only
// handle a client has to read back a lost response.
func tierChangeKeyRequired() error {
	return &TierChangeError{HTTPStatus: http.StatusBadRequest, Code: billing.CodeTierChangeIdempotencyKeyRequired, Message: "Idempotency-Key is required for a tier change"}
}

// tierChangeIdempotencyConflict refuses a key that already names a different
// tier change (another customer, subscription or target). It never carries
// that operation's result or identity.
func tierChangeIdempotencyConflict() error {
	return &TierChangeError{HTTPStatus: http.StatusConflict, Code: billing.CodeTierChangeIdempotencyConflict, Message: "Idempotency-Key already names a different tier change; use a new key"}
}

// TierChangeInFlightError: an unresolved tier change already owns the
// subscription; a request under another idempotency key is refused with it.
type TierChangeInFlightError struct{ OperationID uuid.UUID }

func (e *TierChangeInFlightError) Error() string {
	return "tier change " + e.OperationID.String() + " is unresolved; retry with its Idempotency-Key"
}
func (e *TierChangeInFlightError) Unwrap() error { return ErrTierChangePending }

// SolanaTierChange decides an on-chain tier change, which takes effect at
// once: a downgrade grants the new plan for the rest of the paid period with
// no payment. So the subscription must be active, the move must stay inside
// one declared tier group and one currency, and it is an upgrade (the prorated
// difference is pulled in the same transaction) unless the new plan costs no
// more per hour than the current one. Rank never makes a costlier plan free.
func SolanaTierChange(sub *models.Subscription, current, next *models.Product, currentPrice, nextPrice *models.Price) (upgrade bool, err error) {
	if sub == nil || sub.Status != models.StatusActive {
		return false, &TierChangeError{HTTPStatus: http.StatusConflict, Message: "only an active subscription can change tier on Solana"}
	}
	if current.ID == next.ID {
		return false, ErrTierChangeSameProduct
	}
	if !sameTierGroup(current, next) {
		return false, ErrTierChangeDifferentGroup
	}
	if err := RequireSameCurrency(PriceAmountOf(currentPrice), PriceAmountOf(nextPrice)); err != nil {
		return false, err
	}
	currentHours, nextHours := currentPrice.RecurringCycleHours(), nextPrice.RecurringCycleHours()
	if currentHours == nil || nextHours == nil || *currentHours <= 0 || *nextHours <= 0 {
		return false, ErrTierChangeCycleUnknown
	}
	// next/nextHours > current/currentHours, without division or overflow.
	nextRate := new(big.Int).Mul(big.NewInt(nextPrice.Amount), big.NewInt(int64(*currentHours)))
	currentRate := new(big.Int).Mul(big.NewInt(currentPrice.Amount), big.NewInt(int64(*nextHours)))
	return nextRate.Cmp(currentRate) > 0, nil
}

// sameTierGroup reports whether a tier change stays inside one declared tier
// group. A product without a group has no tiers: moving into or out of one would
// bypass the catalog's tier structure and its one-membership-per-group guard.
func sameTierGroup(current, next *models.Product) bool {
	if current == nil || next == nil || current.TierGroup == nil || next.TierGroup == nil {
		return false
	}
	group := strings.TrimSpace(*current.TierGroup)
	return group != "" && group == strings.TrimSpace(*next.TierGroup)
}
