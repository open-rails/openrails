package models

import (
	"time"

	"github.com/google/uuid"
)

// BillingPolicyKind names which quantity a policy caps.
type BillingPolicyKind string

const (
	// BillingPolicyOutstandingCap caps LEDGER-measured unpaid arrears: a credit
	// line on DEBT. Outstanding owed is subtracted from the line at admission, so
	// $155 unpaid against a $200 line leaves $45 of headroom and the next request
	// past it is refused with outstanding_cap_reached.
	BillingPolicyOutstandingCap BillingPolicyKind = "outstanding_cap"

	// BillingPolicyWindowSpendCap caps NEW spend per rolling window and does NOT
	// gate on prior debt — an unpaid invoice drives delinquency, not admission.
	BillingPolicyWindowSpendCap BillingPolicyKind = "window_spend_cap"

	// BillingPolicyAccrualRateCap caps the measured accrual RATE — the cloud
	// quota, "no more than $X/hour deployed at any instant". The host pre-admits a
	// prospective deployment by passing its rate DELTA; OpenRails measures what is
	// already accruing and answers from the bound cap. Like window_spend_cap and
	// unlike outstanding_cap, prior debt never gates it.
	BillingPolicyAccrualRateCap BillingPolicyKind = "accrual_rate_cap"
)

// BillingPolicy is the JSONB body stored in billing.billing_policies.
//
// Every quantity is measured from the double-entry ledger — never from invoices
// (presentation artifacts) and never from merchant-supplied numbers. The
// merchant declares the policy and binds it; OpenRails measures and enforces.
type BillingPolicy struct {
	Kind BillingPolicyKind `json:"kind"`

	// OutstandingCapAmount is the credit line on unpaid arrears, in micros
	// (kind=outstanding_cap only). Zero means "use the payer's own arrears
	// credit limit" — the per-account lever stays the per-account lever.
	OutstandingCapAmount int64 `json:"outstanding_cap_amount,omitempty"`

	// SpendWindows are the rolling new-spend ceilings (kind=window_spend_cap
	// only): at most Limit of spend per WindowSeconds, metered in Redis.
	SpendWindows []BudgetWindowPolicy `json:"spend_windows,omitempty"`

	// BadSpendWindows are wasted/failed-spend grace windows for the direct
	// payer: at most Limit of wasted spend is forgiven per window, and overage
	// is charged at report time. Allowed with any Kind.
	BadSpendWindows []BudgetWindowPolicy `json:"bad_spend_windows,omitempty"`

	// AccrualRateCapPerHour is the ceiling on the payer's measured accrual RATE,
	// in micros PER HOUR (kind=accrual_rate_cap only). Micros/hour is canonical on
	// every surface — the host's prospective delta speaks it too — so no caller has
	// to know the merchant's measurement window.
	AccrualRateCapPerHour int64 `json:"accrual_rate_cap_per_hour,omitempty"`

	// AccrualRateWindowSeconds is the LOOKBACK the rate is measured over
	// (kind=accrual_rate_cap only; 0 means DefaultAccrualRateWindowSeconds). It
	// smooths the measurement, it does not change the unit: a short window reacts
	// fast and reads bursty, a long one is stable and forgives a spike.
	AccrualRateWindowSeconds int64 `json:"accrual_rate_window_seconds,omitempty"`

	// CollectionThresholdAmount (micros) is when this payer's accrued arrears is
	// invoiced. Overrides the merchant-wide invoice.collection_threshold for payers
	// bound to this policy; unset defers to it. Orthogonal to Kind.
	CollectionThresholdAmount *int64 `json:"collection_threshold_amount,omitempty"`

	// CollectionCycleBoundary is declarable and refused: a payer's statement
	// periods must tile its lifetime, and rebinding a policy mid-cycle would
	// bill a stretch twice or never. It stays merchant-wide
	// (invoice.billing_period_boundary).
	CollectionCycleBoundary string `json:"collection_cycle_boundary,omitempty"`

	// DelinquencyGraceDays / DelinquencyAmountFloor override the merchant's
	// arrears_grace_days / arrears_delinquency_floor for bound payers; unset
	// defers to them. Allowed with any Kind: debt ages even where it never
	// gates admission.
	DelinquencyGraceDays   *int   `json:"delinquency_grace_days,omitempty"`
	DelinquencyAmountFloor *int64 `json:"delinquency_amount_floor,omitempty"`

	// PolicyCurrency is the currency for checks whose window carries none. Blank
	// means the request's currency.
	PolicyCurrency string `json:"policy_currency,omitempty"`
}

// DefaultAccrualRateWindowSeconds is the accrual-rate lookback when a policy
// declares none: one hour, matching the unit the cap is expressed in, so an
// undeclared window is the least surprising possible reading of "$X/hour".
const DefaultAccrualRateWindowSeconds int64 = 3600

// RateWindowSeconds is the effective accrual-rate lookback.
func (p BillingPolicy) RateWindowSeconds() int64 {
	if p.AccrualRateWindowSeconds > 0 {
		return p.AccrualRateWindowSeconds
	}
	return DefaultAccrualRateWindowSeconds
}

// BillingPolicyBinding is one row of billing.billing_policy_bindings: which
// named policy applies to whom. Exactly one rung is populated — CustomerID for
// a per-customer override, Tier for a per-tier override, neither for the
// merchant default.
type BillingPolicyBinding struct {
	ID         uuid.UUID  `json:"id"`
	MerchantID uuid.UUID  `json:"merchant_id"`
	CustomerID *uuid.UUID `json:"customer_id,omitempty"`
	Tier       string     `json:"tier,omitempty"`
	PolicyName string     `json:"policy"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}
