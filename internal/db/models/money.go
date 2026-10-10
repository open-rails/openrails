package models

import (
	"time"

	"github.com/google/uuid"
)

// Money domain types. Amount precision is implied by Currency.

type MoneyBalance struct {
	ID          uuid.UUID `json:"id"`
	MerchantID  uuid.UUID `json:"merchant_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Currency    string    `json:"currency"`
	Balance     int64     `json:"balance"`
	HeldBalance int64     `json:"held_balance"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
