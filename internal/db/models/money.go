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

// MoneyWindow is a prepaid money window (issue #335): one bulk reservation a
// host admits requests against locally. See CreditWindow for the mechanics.
type MoneyWindow struct {
	ID            uuid.UUID `json:"id"`
	MerchantID    uuid.UUID `json:"merchant_id"`
	CustomerID    uuid.UUID `json:"customer_id"`
	Currency      string    `json:"currency"`
	HeldAmount    int64     `json:"held_amount"`
	SettledAmount int64     `json:"settled_amount"`
	Status        string    `json:"status"` // open | closed | expired
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// MoneyAccount is the per-(merchant, merchant subject) spend policy and money-in
// configuration (issue #237). NULL cap columns mean "no cap".
type MoneyAccount struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`

	BillingMode              string     `json:"billing_mode"`
	MaxSpendPerDay           *int64     `json:"max_spend_per_day,omitempty"`
	MaxSpendPerMonth         *int64     `json:"max_spend_per_month,omitempty"`
	MaxOutstandingOwedAmount *int64     `json:"max_outstanding_owed_amount,omitempty"`
	LowBalanceThreshold      *int64     `json:"low_balance_threshold,omitempty"`
	AutoTopupEnabled         bool       `json:"auto_topup_enabled"`
	AutoTopupAmountCents     *int64     `json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod   *uuid.UUID `json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryDays  *int       `json:"default_credit_expiry_days,omitempty"`
	HardStopOnBreach         bool       `json:"hard_stop_on_breach"`
	AlertThresholdPct        int        `json:"alert_threshold_pct"`

	OutstandingOwedAmount int64 `json:"outstanding_owed_amount"`
	// CreditLimitAmount is the admin-set arrears credit line (#489): under
	// billing_mode=arrears the balance may go negative up to this amount; AdmitHold
	// denies insufficient_credit when a new hold would exceed it. 0 = off. NOT
	// self-serve (set via SetCreditLimit, never UpsertAccountSettings).
	CreditLimitAmount int64      `json:"credit_limit_amount"`
	LastAlertAt       *time.Time `json:"last_alert_at,omitempty"`
	LastTopupAt       *time.Time `json:"last_topup_at,omitempty"`

	VerifiedPaymentMethod bool       `json:"verified_payment_method"`
	VerifiedAt            *time.Time `json:"verified_at,omitempty"`
	SuspendedAt           *time.Time `json:"suspended_at,omitempty"`
	SuspendReason         *string    `json:"suspend_reason,omitempty"`
	Tier                  *string    `json:"tier,omitempty"`
	// TierSource is "auto" (schedule-driven graduation, #476) or "admin" (an
	// explicit override that auto-graduation must not overwrite).
	TierSource string `json:"tier_source,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
