package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type ManualRebillAttemptStatus string

const (
	ManualRebillAttemptPending   ManualRebillAttemptStatus = "pending"
	ManualRebillAttemptSucceeded ManualRebillAttemptStatus = "succeeded"
	ManualRebillAttemptFailed    ManualRebillAttemptStatus = "failed"
	ManualRebillAttemptUnknown   ManualRebillAttemptStatus = "unknown"
)

// ManualRebillAttempt is a durable idempotency record for processor-side dunning charges.
type ManualRebillAttempt struct {
	bun.BaseModel `bun:"table:billing.manual_rebill_attempts,alias:mra"`

	ID             uuid.UUID                 `bun:"id,pk,type:uuid" json:"id"`
	SubscriptionID uuid.UUID                 `bun:"subscription_id,type:uuid,notnull" json:"subscription_id"`
	PeriodEnd      time.Time                 `bun:"period_end,notnull" json:"period_end"`
	Processor      Processor                 `bun:"processor,notnull" json:"processor"`
	OrderReference string                    `bun:"order_reference,notnull" json:"order_reference"`
	Status         ManualRebillAttemptStatus `bun:"status,notnull" json:"status"`
	TransactionID  *string                   `bun:"transaction_id,nullzero" json:"transaction_id,omitempty"`
	FailureReason  *string                   `bun:"failure_reason,nullzero" json:"failure_reason,omitempty"`
	ClaimedUntil   *time.Time                `bun:"claimed_until,nullzero" json:"claimed_until,omitempty"`
	CreatedAt      time.Time                 `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt      time.Time                 `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
