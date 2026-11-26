package models

import (
	"math"
	"time"

	"github.com/google/uuid"
)

// Money domain types. Amount precision is implied by Currency.

// MoneyBalance is a customer's balance in a currency: the ledger account,
// less what the ledger has not taken yet.
type MoneyBalance struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`
	// Balance is the ledger account's credits less debits.
	Balance int64 `json:"balance"`
	// Pending is usage not yet posted, at its provisional prices.
	Pending int64 `json:"pending"`
	// Accrued is what running charges accrued since their last posting, as
	// of AsOf.
	Accrued   int64     `json:"accrued"`
	AsOf      time.Time `json:"as_of"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Unposted is what the ledger has not taken: pending usage and accrual.
func (b MoneyBalance) Unposted() int64 { return addSat(b.Pending, b.Accrued) }

// Available is the balance less what the ledger has not taken.
func (b MoneyBalance) Available() int64 { return subSat(b.Balance, b.Unposted()) }

func addSat(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

func subSat(a, b int64) int64 {
	if b > 0 && a < math.MinInt64+b {
		return math.MinInt64
	}
	if b < 0 && a > math.MaxInt64+b {
		return math.MaxInt64
	}
	return a - b
}

type MoneyTransaction struct {
	ID              uuid.UUID      `json:"id"`
	MerchantID      uuid.UUID      `json:"merchant_id"`
	CustomerID      uuid.UUID      `json:"customer_id"`
	Currency        string         `json:"currency"`
	Invoker         string         `json:"invoker"`
	Resource        *string        `json:"resource,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Amount          int64          `json:"amount"`
	BalanceAfter    *int64         `json:"balance_after,omitempty"`
	TransactionType string         `json:"transaction_type"`
	Status          string         `json:"status"`
	Authorized      *int64         `json:"authorized_amount,omitempty"`
	Captured        *int64         `json:"captured_amount,omitempty"`
	Source          string         `json:"source"`
	SourceID        *string        `json:"source_id,omitempty"`
	InvoiceID       *uuid.UUID     `json:"invoice_id,omitempty"`
	ExpiresAt       *time.Time     `json:"expires_at,omitempty"`
	Description     *string        `json:"description,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	// Replayed reports that this write's idempotency coordinate was already
	// committed, so this call moved no money; the row is the earlier movement.
	Replayed bool `json:"replayed,omitempty"`
}

// MoneyAccount is a customer's per-currency spend policy and money-in
// configuration.
type MoneyAccount struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`

	BillingMode          string     `json:"billing_mode"`
	DefaultPaymentMethod *uuid.UUID `json:"default_payment_method_id,omitempty"`

	// CreditLimitAmount is the admin-set arrears credit line: under
	// billing_mode=arrears the balance may go negative up to it, and a hold
	// past it is denied insufficient_credit. 0 = off. Not self-serve: only
	// SetMoneyAccountCreditLimit writes it, never UpsertAccountSettings.
	CreditLimitAmount int64 `json:"credit_limit_amount"`

	TrustLevel *string `json:"trust_level,omitempty"`

	// CollectionRevokedAt is when the customer revoked its collection
	// agreement in the currency: no credit from then until it gives another.
	CollectionRevokedAt *time.Time `json:"collection_revoked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ExtendsCredit reports whether the account may spend past its balance: the
// merchant bills it in arrears and the customer has not revoked collection.
func (a *MoneyAccount) ExtendsCredit() bool {
	return a != nil && a.BillingMode == "arrears" && a.CollectionRevokedAt == nil
}
