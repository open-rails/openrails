package models

import (
	"time"

	"github.com/google/uuid"
)

// BillingPolicyKind names WHICH quantity a policy caps (or#897). The two seed
// businesses differ on exactly this and on nothing else, so it is the one field
// the whole registry turns on.
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

	// BillingPolicyAccrualRateCap caps the measured accrual RATE (the cloud
	// quota: "no more than $X deployed at any instant"). Representable so the
	// vocabulary is complete and a manifest that declares it fails with a real
	// answer; REFUSED by the validator until or#897 PR 3 builds the measurement.
	BillingPolicyAccrualRateCap BillingPolicyKind = "accrual_rate_cap"
)

// BillingPolicy is the JSONB body stored in openrails.billing_policies.
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

	// SpendWindows are the rolling NEW-spend ceilings (kind=window_spend_cap
	// only). Shape is verbatim the retired trust-level budget window: at most
	// Limit of spend per WindowSeconds, metered in Redis.
	SpendWindows []BudgetWindowPolicy `json:"spend_windows,omitempty"`

	// BadSpendWindows are the #497 $-valued wasted/failed-spend grace windows for
	// the direct payer: at most Limit of wasted spend is forgiven per window, and
	// overage is charged at report time. Orthogonal to Kind, so allowed on any.
	BadSpendWindows []BudgetWindowPolicy `json:"bad_spend_windows,omitempty"`

	// PolicyCurrency is the currency for checks whose window carries none. Blank
	// means the request's currency.
	PolicyCurrency string `json:"policy_currency,omitempty"`
}

// BillingPolicyBinding is one row of openrails.billing_policy_bindings: which
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
