package billing

import (
	"time"
)

// Customer is one merchant-scoped billing record. Its ID is the host's stable
// subject UUID; the same UUID under another merchant is a different customer.
// Contact is who the customer is, from the merchant's directory; null when the
// directory holds no contact for it.
type Customer struct {
	ID         CustomerID       `json:"id"`
	Contact    *CustomerContact `json:"contact"`
	CreatedAt  time.Time        `json:"created_at"`
	LastSeenAt time.Time        `json:"last_seen_at"`
}

// CustomerContact is how to reach a customer, as the merchant's directory
// holds it: read live from the host's directory when OpenRails is embedded
// beside it (Deps.Contacts), else the copy its SCIM provisioning and verified
// access tokens keep. Active and SyncedAt describe that copy and are null for
// a live read.
type CustomerContact struct {
	Email    *string `json:"email"`
	Name     *string `json:"name"`
	Username *string `json:"username"`
	// Active is the directory's SCIM active flag, shown and never enforced:
	// the host's auth decides who signs in.
	Active *bool `json:"active"`
	// SyncedAt is when the copy last changed.
	SyncedAt *time.Time `json:"synced_at"`
}

// CustomerListParams lists customers, newest first. Search instead lists the
// customers whose email, username or name contains it, or whose id it is: at
// most limit, newest first, in one page. IDs instead reads 1 to
// MaxBatchItems named customers in one page; unknown ones are absent.
type CustomerListParams struct {
	Search string       `form:"search"`
	IDs    []CustomerID `form:"-"`
	PageRequest
}

// CustomerBillingProfile is one customer's billing at a glance: balances in
// every currency they hold one, recent subscriptions, payments and saved
// payment methods, and the first page of their keys and of their
// product-access windows (ListCustomerEntitlements and ListProductAccess read
// on from each page's cursor).
type CustomerBillingProfile struct {
	Customer       Customer                      `json:"customer"`
	Balances       []Balance                     `json:"balances"`
	Subscriptions  []Subscription                `json:"subscriptions"`
	Entitlements   ListPage[CustomerEntitlement] `json:"entitlements"`
	Payments       []Payment                     `json:"payments"`
	PaymentMethods []PaymentMethod               `json:"payment_methods"`
	ProductAccess  ListPage[ProductAccessGrant]  `json:"product_access"`
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

// Delinquency is a customer's arrears standing in one currency. OverdueStartedAt
// is the oldest overdue due date, null when current; EnteredAt is when the
// state began.
type Delinquency struct {
	CustomerID       CustomerID       `json:"customer_id"`
	Currency         string           `json:"currency"`
	State            DelinquencyState `json:"state"`
	OverdueStartedAt *time.Time       `json:"overdue_started_at"`
	OverdueAmount    int64            `json:"overdue_amount,string"`
	OverdueInvoices  int              `json:"overdue_invoices"`
	EnteredAt        time.Time        `json:"entered_at"`
	EvaluatedAt      time.Time        `json:"evaluated_at"`
}

// DelinquencyListParams lists the overdue customers, oldest debt first:
// State filters to grace or delinquent (both when empty).
type DelinquencyListParams struct {
	State DelinquencyState `form:"state"`
	PageRequest
}
