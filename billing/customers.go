package billing

import (
	"time"
)

// Customer is one merchant-scoped billing record. Its ID is the host's stable
// subject UUID; the same UUID under another merchant is a different customer.
// Email is the billing contact the merchant declared, null when none.
type Customer struct {
	ID         CustomerID `json:"id"`
	Email      *string    `json:"email"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
}

// EnsureCustomerParams declares a customer. EnsureCustomer creates the customer or
// replaces these fields; a nil Email clears it.
type EnsureCustomerParams struct {
	Email *string `json:"email"`
}

// CustomerListParams lists customers, newest first. Query matches an id
// prefix or an email substring.
type CustomerListParams struct {
	Query string `form:"q"`
	PageRequest
}

// CustomerBillingProfile is one customer's billing at a glance: balances in
// every currency they hold one, recent subscriptions, live entitlements and
// product access, payments and saved payment methods.
type CustomerBillingProfile struct {
	Customer       Customer             `json:"customer"`
	Balances       []Balance            `json:"balances"`
	Subscriptions  []Subscription       `json:"subscriptions"`
	Entitlements   []EntitlementRecord  `json:"entitlements"`
	Payments       []Payment            `json:"payments"`
	PaymentMethods []PaymentMethod      `json:"payment_methods"`
	ProductAccess  []ProductAccessGrant `json:"product_access"`
}

// CustomerBillingPolicy is the billing policy assigned to one customer. A
// null PolicyName means the customer inherits its tier's or the merchant's
// default policy.
type CustomerBillingPolicy struct {
	CustomerID CustomerID `json:"customer_id"`
	PolicyName *string    `json:"policy_name"`
}

// SetCustomerBillingPolicyParams assigns a declared billing policy to a
// customer; a null PolicyName restores inheritance.
type SetCustomerBillingPolicyParams struct {
	PolicyName *string `json:"policy_name"`
}

// DelinquencyState is how overdue a customer's arrears are in one currency.
type DelinquencyState string

const (
	DelinquencyCurrent DelinquencyState = "current"
	// DelinquencyGrace: past due, inside the grace window or under the floor.
	DelinquencyGrace DelinquencyState = "grace"
	// DelinquencyDelinquent: past grace and over the floor; new spend is
	// refused at admission.
	DelinquencyDelinquent DelinquencyState = "delinquent"
)

// Delinquency is a customer's arrears standing in one currency. OverdueSince
// is the oldest overdue due date, null when current; EnteredAt is when the
// state began.
type Delinquency struct {
	CustomerID      CustomerID       `json:"customer_id"`
	Currency        string           `json:"currency"`
	State           DelinquencyState `json:"state"`
	OverdueSince    *time.Time       `json:"overdue_since"`
	OverdueAmount   int64            `json:"overdue_amount,string"`
	OverdueInvoices int              `json:"overdue_invoices"`
	EnteredAt       time.Time        `json:"entered_at"`
	EvaluatedAt     time.Time        `json:"evaluated_at"`
}

// DelinquencyListParams lists the overdue customers, oldest debt first:
// State filters to grace or delinquent (both when empty).
type DelinquencyListParams struct {
	State DelinquencyState `form:"state"`
	PageRequest
}
