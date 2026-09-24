package checkout

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

var (
	// ErrTierChangeCycleUnknown: the target price has no positive billing
	// cycle, so the reset period it would start is undefined.
	ErrTierChangeCycleUnknown = &TierChangeError{HTTPStatus: http.StatusUnprocessableEntity, Code: openrails.CodeTierChangeCycleUnknown, Message: "target price has no positive billing cycle"}
	// ErrTierChangePeriodUnknown: the subscription has no open current period
	// to measure the old plan's unused value against.
	ErrTierChangePeriodUnknown = &TierChangeError{HTTPStatus: http.StatusUnprocessableEntity, Code: openrails.CodeTierChangePeriodUnknown, Message: "subscription has no valid current period"}
	// ErrTierChangeCreditExceedsPrice: the old plan's unused value is larger
	// than the new plan's price. Model B has no stored balance to carry the
	// excess, so the change is refused rather than forfeiting it.
	ErrTierChangeCreditExceedsPrice = &TierChangeError{HTTPStatus: http.StatusConflict, Code: openrails.CodeTierChangeCreditExceedsPrice, Message: "unused value of the current plan exceeds the new plan's price; change at period end instead"}
)

// ModelBUpgrade is one reset-period upgrade: the subscription's current paid
// period on the old price, and the new price with its own cycle. The two
// cadences may differ.
type ModelBUpgrade struct {
	Old, New               PriceAmount
	PeriodStart, PeriodEnd *time.Time
	NewCycleHours          *int
}

// ModelBUpgradeQuote is what an upgrade charges now and the period it opens.
type ModelBUpgradeQuote struct {
	Credit      int64 // unused old-plan value, internal units
	ChargeNow   int64 // New - Credit, internal units
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// QuoteModelBUpgrade prices a Model B upgrade (#268): the customer pays
// New - Credit now for a fresh period [now, now+New's cycle], where
//
//	Credit = ceilMinor(Old × (PeriodEnd − max(now, PeriodStart)) / (PeriodEnd − PeriodStart))
//
// measured against the subscription's actual current period at nanosecond
// precision, so the old plan's own cadence governs its credit whatever the
// new cadence is. Money is rounded once, up to a whole rail minor unit of the
// currency (customer-favored), and never exceeds Old. Unknown cycles or
// periods and a credit larger than New are typed refusals, never defaults.
func QuoteModelBUpgrade(u ModelBUpgrade, now time.Time) (ModelBUpgradeQuote, error) {
	if err := RequireSameCurrency(u.Old, u.New); err != nil {
		return ModelBUpgradeQuote{}, err
	}
	if u.NewCycleHours == nil || *u.NewCycleHours <= 0 {
		return ModelBUpgradeQuote{}, ErrTierChangeCycleUnknown
	}
	if u.PeriodStart == nil || u.PeriodEnd == nil || u.PeriodStart.IsZero() || !u.PeriodEnd.After(*u.PeriodStart) {
		return ModelBUpgradeQuote{}, ErrTierChangePeriodUnknown
	}
	if u.Old.Micros < 0 || u.New.Micros < 0 {
		return ModelBUpgradeQuote{}, errors.New("prices must be nonnegative")
	}
	from := now
	if from.Before(*u.PeriodStart) {
		from = *u.PeriodStart
	}
	remaining := u.PeriodEnd.Sub(from)
	if remaining < 0 {
		remaining = 0
	}
	credit, err := prorateCredit(u.Old, remaining, u.PeriodEnd.Sub(*u.PeriodStart))
	if err != nil {
		return ModelBUpgradeQuote{}, err
	}
	if credit > u.New.Micros {
		return ModelBUpgradeQuote{}, fmt.Errorf("%w (credit %d, price %d)", ErrTierChangeCreditExceedsPrice, credit, u.New.Micros)
	}
	return ModelBUpgradeQuote{
		Credit:      credit,
		ChargeNow:   u.New.Micros - credit,
		PeriodStart: now,
		PeriodEnd:   now.Add(time.Duration(*u.NewCycleHours) * time.Hour),
	}, nil
}

// prorateCredit is ceil(amount × remaining / period) in rail minor units,
// widened back to internal units and capped at amount.
func prorateCredit(amount PriceAmount, remaining, period time.Duration) (int64, error) {
	cur, ok := moneyutil.LookupCurrency(amount.Currency)
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", amount.Currency)
	}
	num := new(big.Int).Mul(big.NewInt(amount.Micros), big.NewInt(int64(remaining)))
	den := big.NewInt(int64(period))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(cur.NativeShift()))), nil)
	if cur.NativeShift() >= 0 {
		den.Mul(den, scale)
	} else {
		num.Mul(num, scale)
	}
	minor, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() > 0 {
		minor.Add(minor, big.NewInt(1))
	}
	if !minor.IsInt64() {
		return 0, errors.New("unused subscription value exceeds int64 precision")
	}
	credit, err := moneyutil.RailMinorToNative(amount.Currency, moneyutil.Cents(minor.Int64()))
	if err != nil {
		return 0, err
	}
	return min(credit, amount.Micros), nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// modelBUpgradeOf reads the upgrade inputs from the subscription's current
// period on its current price and the target price's cycle.
func modelBUpgradeOf(sub *models.Subscription, current, target *models.Price) ModelBUpgrade {
	return ModelBUpgrade{
		Old: PriceAmountOf(current), New: PriceAmountOf(target),
		PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt,
		NewCycleHours: target.RecurringCycleHours(),
	}
}
