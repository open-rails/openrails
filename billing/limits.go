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

// CustomerLimitListParams names the customer whose limits are listed.
type CustomerLimitListParams struct {
	CustomerID CustomerID `form:"customer_id"`
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

// AdjustCustomerLimitParams adds Amount (negative takes away) to the capacity of
// the customer's current window of Limit only; the next window starts from
// the products' capacity again. It opens a session or first-use window not
// yet open. A rate limit takes no adjustment. SourceID identifies the
// adjustment per customer for 61 days: a retry with the same terms answers
// the customer's line without adding again, one with another limit or
// amount is ErrIdempotencyKeyReused.
type AdjustCustomerLimitParams struct {
	Limit    string `json:"limit"`
	Amount   int64  `json:"amount,string"`
	SourceID string `json:"source_id"`
}

// DenyCode is why a request was not admitted.
type DenyCode string

const (
	// DenyLimitReached: the request would not fit a limit even if every other
	// hold were returned. The refusal names the limit and when it resets.
	DenyLimitReached DenyCode = "limit_reached"
	// DenyCapacityHeld: the request would fit but for capacity gates' lease
	// holds keep; they shrink at their next sync. Retry after
	// RetryAfterSeconds. A gate's want and an admission alike.
	DenyCapacityHeld DenyCode = "capacity_held"
	// DenyInsufficientBalance: the prepaid balance does not cover the request.
	DenyInsufficientBalance DenyCode = "insufficient_balance"
	// DenyInsufficientCredit: the request does not fit the credit line.
	DenyInsufficientCredit DenyCode = "insufficient_credit"
	// DenyOutstandingCapReached: unpaid arrears have reached the customer's
	// credit line.
	DenyOutstandingCapReached DenyCode = "outstanding_cap_reached"
	// DenyDelinquentUnpaidOrder: a debt has outlived the merchant's grace
	// window.
	DenyDelinquentUnpaidOrder DenyCode = "delinquent_unpaid_order"
	// DenyBudgetExceeded: a spend-cap window is full.
	DenyBudgetExceeded DenyCode = "budget_exceeded"
	// DenyAccrualRateCapReached: the measured accrual rate, with the request,
	// would pass the customer's hourly quota.
	DenyAccrualRateCapReached DenyCode = "accrual_rate_cap_reached"
	// DenyDelegatedSpendNotAllowed: only the customer's own credential spends
	// its balance, not a delegated invoker.
	DenyDelegatedSpendNotAllowed DenyCode = "delegated_spend_not_allowed"
	// DenyFailureRateLimited: a delegated invoker's wasted spend passed its
	// cutoff.
	DenyFailureRateLimited DenyCode = "failure_rate_limited"
)

// Refusal is why a request was not admitted: the deny code, the limit it
// names and when that limit resets, and when to retry.
type Refusal struct {
	DenyCode          DenyCode   `json:"deny_code"`
	Limit             *string    `json:"limit"`
	ResetsAt          *time.Time `json:"resets_at"`
	RetryAfterSeconds *int64     `json:"retry_after_seconds"`
}
