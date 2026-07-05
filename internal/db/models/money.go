package models

import (
	"time"

	"github.com/google/uuid"
)

// Money domain types. Amount precision is implied by Currency (#494).
// No credit_type dimension — native money is identical for every merchant (#472).

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
}

// MoneyAccount is the per-(merchant, merchant subject) spend policy and money-in
// configuration (issue #237).
type MoneyAccount struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`

	BillingMode              string     `json:"billing_mode"`
	LowBalanceThreshold      *int64     `json:"low_balance_threshold,omitempty"`
	AutoTopupEnabled         bool       `json:"auto_topup_enabled"`
	AutoTopupAmount          *int64     `json:"auto_topup_amount,omitempty"`
	AutoTopupPaymentMethod   *uuid.UUID `json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryHours *int       `json:"default_credit_expiry_hours,omitempty"`

	// CreditLimitAmount is the admin-set arrears credit line (#489): under
	// billing_mode=arrears the balance may go negative up to this amount; AdmitHold
	// denies insufficient_credit when a new hold would exceed it. 0 = off. NOT
	// self-serve (set via SetCreditLimit, never UpsertAccountSettings).
	CreditLimitAmount int64      `json:"credit_limit_amount"`
	LastAlertAt       *time.Time `json:"last_alert_at,omitempty"`
	LastTopupAt       *time.Time `json:"last_topup_at,omitempty"`

	TrustLevel *string `json:"trust_level,omitempty"`
	// TrustLevelSource is "auto" (schedule-driven graduation, #476) or "admin" (an
	// explicit override that auto-graduation must not overwrite).
	TrustLevelSource string `json:"trust_level_source,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
