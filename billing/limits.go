package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// Limit is a usage limit the merchant declares: what counts toward it (its
// Unit and RateCards), when its window resets, and how a request that would
// cross it is admitted. Products give it capacity.
type Limit struct {
	Key       string                  `json:"key"`
	Unit      string                  `json:"unit"`
	Reset     catalog.LimitReset      `json:"reset"`
	Overrun   catalog.Overrun         `json:"overrun"`
	RateCards []catalog.LimitRateCard `json:"rate_cards"`
	// Products are the products that give the limit capacity, by key.
	Products []LimitProduct `json:"products"`
	// Revision advances on every change to the limit.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LimitProduct is the capacity one product gives a limit in each window.
type LimitProduct struct {
	ProductID  ProductID `json:"product_id"`
	ProductKey string    `json:"product_key"`
	Capacity   int64     `json:"capacity,string"`
}

// SetLimitParams declares a limit whole. Overrun defaults to
// require_estimate. A currency unit takes no rate cards.
type SetLimitParams struct {
	Unit      string                  `json:"unit"`
	Reset     catalog.LimitReset      `json:"reset"`
	Overrun   catalog.Overrun         `json:"overrun,omitempty"`
	RateCards []catalog.LimitRateCard `json:"rate_cards,omitempty"`
	// ExpectedRevision refuses the change with revision_mismatch unless the
	// limit is at this revision; 0 expects no limit.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// CustomerLimit is where one customer stands on one limit in its current
// window. Capacity is its products' sum (or its override) plus the window's
// adjustments; Used is settled usage and Held what open holds set aside;
// Remaining is Capacity - Used - Held and may be negative. A window not yet
// opened (a session or first-use window before its first use), a rate limit
// and a lifetime limit have no WindowStartsAt; ResetsAt is null when nothing
// will refill.
type CustomerLimit struct {
	CustomerID     CustomerID `json:"customer_id"`
	Limit          string     `json:"limit"`
	Unit           string     `json:"unit"`
	Capacity       int64      `json:"capacity,string"`
	Used           int64      `json:"used,string"`
	Held           int64      `json:"held,string"`
	Remaining      int64      `json:"remaining,string"`
	WindowStartsAt *time.Time `json:"window_starts_at"`
	ResetsAt       *time.Time `json:"resets_at"`
}

// LimitOverride is a customer's own capacity for a limit, in place of what
// its products give. Inherit clears the override; capacity zero without
// Inherit gives the customer none.
type LimitOverride struct {
	Inherit  bool   `json:"inherit,omitempty"`
	Limit    string `json:"limit"`
	Capacity int64  `json:"capacity,string"`
}

// AdjustLimitParams adds Amount (negative takes away) to the capacity of
// the customer's current window of Limit only; the next window starts from
// the products' capacity again. It opens a session or first-use window not
// yet open. A rate limit takes no adjustment.
type AdjustLimitParams struct {
	Limit  string `json:"limit"`
	Amount int64  `json:"amount,string"`
}
