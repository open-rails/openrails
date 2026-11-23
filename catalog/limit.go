package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/open-rails/openrails/internal/catalogrules/syntax"
)

// BalanceLimit is the built-in limit of a customer's prepaid balance and
// credit line, per currency. It is never declared.
const BalanceLimit = "balance"

// ResetKind is when a limit's window starts over.
type ResetKind string

const (
	// ResetFixed tiles windows of WindowSeconds from its Anchor.
	ResetFixed ResetKind = "fixed"
	// ResetBillingPeriod follows the customer's pricing period.
	ResetBillingPeriod ResetKind = "billing_period"
	// ResetSession opens a window of WindowSeconds at the first use after the
	// previous one ended.
	ResetSession ResetKind = "session"
	// ResetRolling counts the last WindowSeconds.
	ResetRolling ResetKind = "rolling"
	// ResetRate refills the capacity continuously over WindowSeconds.
	ResetRate ResetKind = "rate"
	// ResetLifetime never resets.
	ResetLifetime ResetKind = "lifetime"
)

// ResetAnchor is what a fixed window tiles from.
type ResetAnchor string

const (
	// AnchorSubscription tiles from the customer's subscription start.
	AnchorSubscription ResetAnchor = "subscription"
	// AnchorFirstUse tiles from the customer's first use of the limit.
	AnchorFirstUse ResetAnchor = "first_use"
)

// Overrun is how a request that would cross a limit is admitted.
type Overrun string

const (
	// OverrunRequireEstimate admits only when the whole estimate fits.
	OverrunRequireEstimate Overrun = "require_estimate"
	// OverrunAdmitWhilePositive admits while any capacity remains; the
	// request that crosses the limit finishes and the limit stays negative.
	OverrunAdmitWhilePositive Overrun = "admit_while_positive"
)

// MaxLimitWindowSeconds bounds a limit's window: 366 days.
const MaxLimitWindowSeconds = 366 * 24 * 60 * 60

// LimitReset is when a limit's window starts over. WindowSeconds is set for
// fixed, session, rolling and rate; Anchor for fixed only.
type LimitReset struct {
	Kind          ResetKind   `json:"kind"`
	WindowSeconds int         `json:"window_seconds,omitempty"`
	Anchor        ResetAnchor `json:"anchor,omitempty"`
}

// Windowed reports whether the reset kind takes window_seconds.
func (k ResetKind) Windowed() bool {
	return k == ResetFixed || k == ResetSession || k == ResetRolling || k == ResetRate
}

// Validate refuses an unknown kind, or a window or anchor it does not take.
func (r LimitReset) Validate() error {
	switch r.Kind {
	case ResetFixed, ResetBillingPeriod, ResetSession, ResetRolling, ResetRate, ResetLifetime:
	default:
		return fmt.Errorf("reset.kind must be fixed, billing_period, session, rolling, rate or lifetime")
	}
	if r.Kind.Windowed() != (r.WindowSeconds != 0) {
		if r.Kind.Windowed() {
			return fmt.Errorf("reset %s requires window_seconds", r.Kind)
		}
		return fmt.Errorf("reset %s takes no window_seconds", r.Kind)
	}
	if r.WindowSeconds < 0 || r.WindowSeconds > MaxLimitWindowSeconds {
		return fmt.Errorf("reset.window_seconds must be between 1 and %d", MaxLimitWindowSeconds)
	}
	switch {
	case r.Kind == ResetFixed && r.Anchor != AnchorSubscription && r.Anchor != AnchorFirstUse:
		return fmt.Errorf("reset fixed requires anchor subscription or first_use")
	case r.Kind != ResetFixed && r.Anchor != "":
		return fmt.Errorf("reset %s takes no anchor", r.Kind)
	}
	return nil
}

// Validate refuses an unknown overrun.
func (o Overrun) Validate() error {
	if o != OverrunRequireEstimate && o != OverrunAdmitWhilePositive {
		return fmt.Errorf("overrun must be require_estimate or admit_while_positive")
	}
	return nil
}

// LimitRateCard counts a meter's usage toward a limit: Price is a rate-card
// price in the limit's unit, so it names no currency.
type LimitRateCard struct {
	Meter  string              `json:"meter"`
	Filter map[string][]string `json:"filter,omitempty"`
	Price  RatePrice           `json:"price"`
}

// ApplyLimit declares the limit its map key names; omitted fields keep their
// values. A new limit needs unit and reset; its overrun defaults to
// require_estimate.
type ApplyLimit struct {
	Unit      Field[string]          `json:"unit,omitzero"`
	Reset     Field[LimitReset]      `json:"reset,omitzero"`
	Overrun   Field[Overrun]         `json:"overrun,omitzero"`
	RateCards Field[[]LimitRateCard] `json:"rate_cards,omitzero"`
}

// Capacity is a limit's units in one window, in the limit's unit (native
// units of a currency limit). It travels as a decimal string.
type Capacity int64

// MarshalJSON writes the capacity as a decimal string.
func (c Capacity) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatInt(int64(c), 10))
}

// UnmarshalJSON reads a decimal string or a JSON integer.
func (c *Capacity) UnmarshalJSON(raw []byte) error {
	text := string(bytes.TrimSpace(raw))
	if len(text) > 0 && text[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("capacity must be an exact int64: %w", err)
	}
	*c = Capacity(v)
	return nil
}

// validate checks the declaration's syntax; meters and prices are checked
// against the catalog when it applies.
func (l ApplyLimit) validate() error {
	if err := validateApplicationFields(l); err != nil {
		return err
	}
	if l.Unit.Null || l.Reset.Null || l.Overrun.Null || l.RateCards.Null {
		return fmt.Errorf("nonnullable field is null")
	}
	if l.Unit.Set {
		if err := syntax.ValidateLimitUnit(l.Unit.Value); err != nil {
			return err
		}
	}
	if l.Reset.Set {
		if err := l.Reset.Value.Validate(); err != nil {
			return err
		}
	}
	if l.Overrun.Set {
		if err := l.Overrun.Value.Validate(); err != nil {
			return err
		}
	}
	if l.Unit.Set && syntax.LimitUnitCurrency(l.Unit.Value) && len(l.RateCards.Value) > 0 {
		return fmt.Errorf("a currency limit counts the rated amount and has no rate_cards")
	}
	for i, card := range l.RateCards.Value {
		if err := card.validate(); err != nil {
			return fmt.Errorf("rate card #%d: %w", i+1, err)
		}
	}
	return nil
}

func (c LimitRateCard) validate() error {
	if c.Meter == "" {
		return fmt.Errorf("meter is required")
	}
	if c.Price.Currency != "" {
		return fmt.Errorf("a limit's rate card is priced in the limit's unit and names no currency")
	}
	return nil
}

// validateProductLimits checks a product's capacities.
func validateProductLimits(limits Field[map[string]Capacity]) error {
	if !limits.Set {
		return nil
	}
	if limits.Null || limits.Value == nil {
		return fmt.Errorf("limits must be a map of capacities, not null; use {} for none")
	}
	for _, key := range slices.Sorted(maps.Keys(limits.Value)) {
		if err := syntax.ValidateLimitKey(key); err != nil {
			return err
		}
		if limits.Value[key] < 0 {
			return fmt.Errorf("limit %q: capacity cannot be negative", key)
		}
	}
	return nil
}
