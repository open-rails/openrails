package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type CreditType struct {
	bun.BaseModel `bun:"table:billing.credit_types,alias:ct"`

	ID            uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	Name          string    `bun:"name,notnull,unique" json:"name"`
	DisplayName   string    `bun:"display_name,notnull" json:"display_name"`
	Unit          string    `bun:"unit,notnull" json:"unit"`
	DecimalPlaces int       `bun:"decimal_places,notnull" json:"decimal_places"`
	IsActive      bool      `bun:"is_active,notnull" json:"is_active"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
}

type UserCreditBalance struct {
	bun.BaseModel `bun:"table:billing.user_credit_balances,alias:ucb"`

	ID             uuid.UUID  `bun:"id,pk,type:uuid" json:"id"`
	UserID         string     `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID   uuid.UUID  `bun:"credit_type_id,notnull" json:"credit_type_id"`
	Balance        int64      `bun:"balance,notnull" json:"balance"`
	HeldBalance    int64      `bun:"held_balance,notnull" json:"held_balance"`
	Permanent      int64      `bun:"permanent_balance,notnull" json:"permanent_balance"`
	Expiring       int64      `bun:"expiring_balance,notnull" json:"expiring_balance"`
	EarliestExpiry *time.Time `bun:"earliest_expiry,nullzero" json:"earliest_expiry,omitempty"`
	CreatedAt      time.Time  `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt      time.Time  `bun:"updated_at,notnull" json:"updated_at"`
}

type CreditTransaction struct {
	bun.BaseModel `bun:"table:billing.credit_transactions,alias:ctran"`

	ID              uuid.UUID  `bun:"id,pk,type:uuid" json:"id"`
	UserID          string     `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID    uuid.UUID  `bun:"credit_type_id,notnull" json:"credit_type_id"`
	Amount          int64      `bun:"amount,notnull" json:"amount"`
	BalanceAfter    int64      `bun:"balance_after,notnull" json:"balance_after"`
	TransactionType string     `bun:"transaction_type,notnull" json:"transaction_type"`
	Source          string     `bun:"source,notnull" json:"source"`
	SourceID        *uuid.UUID `bun:"source_id,type:uuid,nullzero" json:"source_id,omitempty"`
	ExpiresAt       *time.Time `bun:"expires_at,nullzero" json:"expires_at,omitempty"`
	Description     *string    `bun:"description,nullzero" json:"description,omitempty"`
	CreatedAt       time.Time  `bun:"created_at,notnull" json:"created_at"`
}

type CreditExpiryBatch struct {
	bun.BaseModel `bun:"table:billing.credit_expiry_batches,alias:ceb"`

	ID                  uuid.UUID  `bun:"id,pk,type:uuid" json:"id"`
	UserID              string     `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID        uuid.UUID  `bun:"credit_type_id,notnull" json:"credit_type_id"`
	OriginalAmount      int64      `bun:"original_amount,notnull" json:"original_amount"`
	RemainingAmount     int64      `bun:"remaining_amount,notnull" json:"remaining_amount"`
	ExpiresAt           time.Time  `bun:"expires_at,notnull" json:"expires_at"`
	SourceTransactionID *uuid.UUID `bun:"source_transaction_id,type:uuid,nullzero" json:"source_transaction_id,omitempty"`
	CreatedAt           time.Time  `bun:"created_at,notnull" json:"created_at"`
}

type CreditHold struct {
	bun.BaseModel `bun:"table:billing.credit_holds,alias:ch"`

	ID           uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	UserID       string    `bun:"user_id,notnull" json:"user_id"`
	CreditTypeID uuid.UUID `bun:"credit_type_id,notnull" json:"credit_type_id"`
	Amount       int64     `bun:"amount,notnull" json:"amount"`
	Source       string    `bun:"source,notnull" json:"source"`
	SourceID     string    `bun:"source_id,notnull" json:"source_id"`
	Status       string    `bun:"status,notnull" json:"status"`
	ExpiresAt    time.Time `bun:"expires_at,notnull" json:"expires_at"`
	Captured     *int64    `bun:"captured_amount,nullzero" json:"captured_amount,omitempty"`
	CreatedAt    time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
