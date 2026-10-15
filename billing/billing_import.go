package billing

import (
	"encoding/json"
	"strings"
	"time"
)

// DeclaredBilling is one merchant's declared billing facts: what a host knows
// to be true at AsOf, classified by OpenRails through the same pipeline the
// pull/probe/webhook planes use. Imports are idempotent by the rows' stable
// identities; re-posting the same book at the same as_of is a no-op.
type DeclaredBilling struct {
	// AsOf is the evidence horizon: every classification evaluates against it,
	// never wall-clock, so the same book classifies identically whenever it runs.
	AsOf time.Time `json:"as_of"`
	// SubscriptionsExhaustive declares that this call covers the merchant's
	// ENTIRE subscription book, so every local subscription it omits is
	// canceled. It must be false for batched imports.
	SubscriptionsExhaustive bool `json:"subscriptions_exhaustive,omitempty"`
	// ExpectedSubscriptions is the typed confirmation required alongside
	// SubscriptionsExhaustive; a mismatch refuses the whole import.
	ExpectedSubscriptions *int `json:"expected_subscriptions,omitempty"`
	// DefaultPSP attributes every declared provider row that names no PSP of
	// its own. A row that resolves to neither refuses the whole import.
	DefaultPSP     PSPRef                  `json:"default_psp,omitzero"`
	Customers      []DeclaredCustomer      `json:"customers,omitempty"`
	PaymentMethods []DeclaredPaymentMethod `json:"payment_methods,omitempty"`
	Subscriptions  []DeclaredSubscription  `json:"subscriptions,omitempty"`
	Transactions   []DeclaredTransaction   `json:"transactions,omitempty"`
	// AdminGrants are comped access windows with no payment behind them.
	AdminGrants []DeclaredAdminGrant `json:"admin_grants,omitempty"`
	// Purchases are settled one-off sales, each an order with its payment.
	Purchases []DeclaredPurchase `json:"purchases,omitempty"`
	// HistoricalPayments records confirmed receipts without inventing
	// subscriptions or access.
	HistoricalPayments []DeclaredHistoricalPayment `json:"historical_payments,omitempty"`
}

// DeclaredHistoricalPayment is a confirmed successful NMI payment and its
// known refunds, whose subscription or catalog purchase is unavailable.
// The caller supplies those facts. It records history only:
// no access, recurring agreement or future collection is created.
// Amount is in native units (micros for USD); PSP falls back to DefaultPSP.
type DeclaredHistoricalPayment struct {
	SourceID      string                     `json:"source_id"`
	Customer      CustomerID                 `json:"customer"`
	PSP           PSPRef                     `json:"psp,omitzero"`
	Rail          string                     `json:"rail"`
	TransactionID string                     `json:"transaction_id"`
	Amount        int64                      `json:"amount,string"`
	Currency      string                     `json:"currency"`
	PaidAt        time.Time                  `json:"paid_at"`
	Refunds       []DeclaredHistoricalRefund `json:"refunds,omitempty"`
}

// DeclaredHistoricalRefund is a confirmed refund with its own provider ID.
// Amount is positive in the original payment's currency and native units.
// Customer and PSP are inherited from the enclosing historical payment.
type DeclaredHistoricalRefund struct {
	TransactionID string    `json:"transaction_id"`
	Amount        int64     `json:"amount,string"`
	RefundedAt    time.Time `json:"refunded_at"`
}

// DeclaredPurchase is one settled one-off sale: an engine-owned order with
// frozen lines and the rail payment that paid it. Idempotent by SourceID and
// by (PSP, TransactionID). Amount must equal the lines' total, in a fiat
// currency; a row that does not is refused with a finding.
type DeclaredPurchase struct {
	SourceID string     `json:"source_id"`
	Customer CustomerID `json:"customer"`
	Rail     string     `json:"rail"`
	// PSP is the account that took the payment; falls back to DefaultPSP.
	PSP           PSPRef              `json:"psp,omitzero"`
	TransactionID string              `json:"transaction_id"`
	Currency      string              `json:"currency"`
	Amount        int64               `json:"amount,string"`
	PaidAt        time.Time           `json:"paid_at"`
	Lines         []DeclaredOrderLine `json:"lines"`
	Settlement    *DeclaredSettlement `json:"settlement,omitempty"`
	// Evidence is the host's verbatim legacy payload, stored for forensics.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// DeclaredOrderLine is one line as sold. UnitAmount is the price it was sold
// at, not today's catalog price. AccessEndsAt ends the product's access
// window; nil takes the price's access duration from PaidAt.
type DeclaredOrderLine struct {
	Price        PriceID    `json:"price"`
	Quantity     *int       `json:"quantity,omitempty"`
	UnitAmount   int64      `json:"unit_amount,string"`
	Description  string     `json:"description,omitempty"`
	AccessEndsAt *time.Time `json:"access_ends_at,omitempty"`
}

// DeclaredSettlement is how the rail settled a purchase, as its record shows:
// for Solana, the transfer's token mint, its amount in the token's base units
// and the paying and receiving wallets.
type DeclaredSettlement struct {
	Asset     string `json:"asset"`
	Quantity  string `json:"quantity"`
	Payer     string `json:"payer,omitempty"`
	Recipient string `json:"recipient,omitempty"`
}

// DeclaredCustomer ensures a customer row for a host subject. Who the
// customer is comes from the host's directory, never the import.
type DeclaredCustomer struct {
	Customer CustomerID `json:"customer"`
}

// PSPRef names the PSP a declared row belongs to: the psps row id, or the
// merchant's manifest PSP key. Exactly one form is set.
type PSPRef struct {
	ID  *PSPID `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
}

// IsZero reports whether the ref names nothing.
func (r PSPRef) IsZero() bool {
	return (r.ID == nil || r.ID.IsZero()) && strings.TrimSpace(r.Key) == ""
}

func (r PSPRef) String() string {
	if r.ID != nil && !r.ID.IsZero() {
		return r.ID.String()
	}
	return strings.TrimSpace(r.Key)
}

// DeclaredPaymentMethod is a stored instrument fact, idempotent by
// (psp, rail_customer_ref, rail_method_ref).
type DeclaredPaymentMethod struct {
	Customer CustomerID `json:"customer"`
	Rail     string     `json:"rail"`
	// PSP is the account holding the vault entry; falls back to DefaultPSP.
	PSP             PSPRef `json:"psp,omitzero"`
	RailCustomerRef string `json:"rail_customer_ref"`
	RailMethodRef   string `json:"rail_method_ref"`
	// RecurringTransactionID is the account-scoped NMI transaction reference
	// for an existing recurring stored-card agreement. When omitted, import
	// takes the first approved sale of the method's NMI schedule (its
	// recurring signup). It never replaces an already captured reference.
	RecurringTransactionID string `json:"recurring_transaction_id,omitempty"`
	// Card is the stored card's display facts, as the provider reported them.
	Card      *CardDetails `json:"card,omitempty"`
	CreatedAt time.Time    `json:"created_at,omitempty"`
}

// PaymentMethodRef links a DeclaredSubscription to a DeclaredPaymentMethod.
type PaymentMethodRef struct {
	Rail            string `json:"rail"`
	RailCustomerRef string `json:"rail_customer_ref"`
	RailMethodRef   string `json:"rail_method_ref"`
}

// CancelEvidence is explicit, settled cancel history. Kind is "" (none),
// "user_canceled", "chargeback" or "provider_terminated".
type CancelEvidence struct {
	Kind string    `json:"kind,omitempty"`
	At   time.Time `json:"at,omitempty"`
	// ScheduleLive means the provider-side recurring schedule was not confirmed
	// dead at AsOf; rails that delete remotely schedule the deferred delete.
	ScheduleLive bool `json:"schedule_live,omitempty"`
}

// DunningEvidence is the host's dunning state at AsOf.
type DunningEvidence struct {
	Retries      int        `json:"retries"`
	LastRetryAt  *time.Time `json:"last_retry_at,omitempty"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	ScheduleLive bool       `json:"schedule_live"`
}

// DeclaredTransaction is one charge-level fact, successes and declines alike.
// Amount is in the currency's native units (micros for USD), and must be a
// whole number of the provider's minor unit. An approved NMI or CCBill charge
// names the provider's own transaction id (digits only); one with no amount
// is looked up at the provider by that id after the import.
type DeclaredTransaction struct {
	RailSubscriptionID string    `json:"rail_subscription_id"`
	TransactionID      string    `json:"transaction_id"`
	Type               string    `json:"type,omitempty"` // sale | refund | chargeback | decline; "" = sale
	Success            bool      `json:"success"`
	Amount             int64     `json:"amount,string"`
	Currency           string    `json:"currency"`
	OccurredAt         time.Time `json:"occurred_at"`
	// NextBillingAt is the verified provider boundary accompanying this receipt.
	NextBillingAt *time.Time `json:"next_billing_at,omitempty"`
	// StripeInvoice is the complete verified bill, repeated identically for each receipt.
	StripeInvoice *DeclaredStripeInvoice `json:"stripe_invoice,omitempty"`
}

// DeclaredSubscription is one subscription's facts, not classifications.
type DeclaredSubscription struct {
	SourceID           string     `json:"source_id"` // host's stable id: idempotency and audit
	Customer           CustomerID `json:"customer"`
	Price              PriceID    `json:"price"`
	Rail               string     `json:"rail"`
	RailSubscriptionID string     `json:"rail_subscription_id"`
	// PSP is the merchant account owning this subscription at the provider;
	// falls back to DefaultPSP.
	PSP           PSPRef            `json:"psp,omitzero"`
	StartedAt     time.Time         `json:"started_at"`
	PaidThrough   *time.Time        `json:"paid_through,omitempty"`
	Cancel        CancelEvidence    `json:"cancel,omitempty"`
	Dunning       *DunningEvidence  `json:"dunning,omitempty"`
	PaymentMethod *PaymentMethodRef `json:"payment_method,omitempty"`
	// AcceptedSchedule is required for a live imported NMI/CCBill schedule.
	// These are operator-verified accepted terms, not a request to reconstruct
	// historical bills from the current catalog or paid-through timestamp.
	AcceptedSchedule *DeclaredProviderSchedule `json:"accepted_schedule,omitempty"`
	// Evidence is the host's verbatim legacy payload, stored for forensics.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// DeclaredProviderSchedule is the accepted recurring obligation of a provider
// schedule. FirstDueAt anchors its first renewal; retry dates do not move it.
type DeclaredProviderSchedule struct {
	SourceReference string `json:"source_reference"`
	Amount          int64  `json:"amount,string"`
	Currency        string `json:"currency"`
	Quantity        *int   `json:"quantity,omitempty"`
	IntervalHours   int    `json:"interval_hours,omitempty"`
	// IntervalMonths selects calendar cadence; exactly one interval is set.
	IntervalMonths int       `json:"interval_months,omitempty"`
	FirstDueAt     time.Time `json:"first_due_at"`
}

// DeclaredAdminGrant is a comped product window with no payment behind it,
// idempotent by SourceID. A nil EndsAt is indefinite.
type DeclaredAdminGrant struct {
	Customer CustomerID `json:"customer"`
	Product  ProductID  `json:"product"`
	SourceID string     `json:"source_id"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
}

// BillingImportResult reports per-SourceID outcomes for subscriptions, admin
// grants, purchases and historical payments. Customers and payment methods
// are idempotent upserts.
type BillingImportResult struct {
	Imported []string          `json:"imported"`
	Skipped  []string          `json:"skipped"`
	Blocked  []string          `json:"blocked"`
	Reasons  map[string]string `json:"reasons"`
}

// DeclaredStripeInvoice is an operator-verified invoice snapshot. All amounts
// use the currency's native units, like DeclaredTransaction; no amount is
// inferred from the current catalog. At most 1000 lines/payments are accepted.
type DeclaredStripeInvoice struct {
	ID                string                         `json:"id"`
	CustomerReference string                         `json:"customer_reference"`
	Status            string                         `json:"status"`
	Reason            string                         `json:"reason"`
	Currency          string                         `json:"currency"`
	Total             int64                          `json:"total,string"`
	AmountDue         int64                          `json:"amount_due,string"`
	AmountPaid        int64                          `json:"amount_paid,string"`
	AmountRemaining   int64                          `json:"amount_remaining,string"`
	AmountOffStripe   int64                          `json:"amount_off_stripe,string"`
	StartingBalance   int64                          `json:"starting_balance,string"`
	PrePaymentCredit  int64                          `json:"pre_payment_credit,string"`
	DueAt             time.Time                      `json:"due_at"`
	Lines             []DeclaredStripeInvoiceLine    `json:"lines"`
	Payments          []DeclaredStripeInvoicePayment `json:"payments"`
}

// DeclaredStripeInvoiceLine preserves one finalized line and its service period.
type DeclaredStripeInvoiceLine struct {
	ID             string    `json:"id"`
	PriceReference string    `json:"price_reference,omitempty"`
	Description    string    `json:"description"`
	Quantity       *int      `json:"quantity,omitempty"`
	Amount         int64     `json:"amount,string"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
}

// DeclaredStripeInvoicePayment is a verified actual invoice allocation. Credit
// and out-of-band settlement belong on the invoice, never in this cash list.
type DeclaredStripeInvoicePayment struct {
	ID              string    `json:"id"`
	ChargeID        string    `json:"charge_id"`
	PaymentIntentID string    `json:"payment_intent_id,omitempty"`
	Amount          int64     `json:"amount,string"`
	PaidAt          time.Time `json:"paid_at"`
}
