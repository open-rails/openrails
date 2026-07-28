package checkout

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

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
