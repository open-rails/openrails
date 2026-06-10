package models

import (
	"time"

	"github.com/google/uuid"
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
	ID             uuid.UUID                 `json:"id"`
	SubscriptionID uuid.UUID                 `json:"subscription_id"`
	PeriodEnd      time.Time                 `json:"period_end"`
	Processor      Processor                 `json:"processor"`
	OrderReference string                    `json:"order_reference"`
	Status         ManualRebillAttemptStatus `json:"status"`
	TransactionID  *string                   `json:"transaction_id,omitempty"`
	FailureReason  *string                   `json:"failure_reason,omitempty"`
	ClaimedUntil   *time.Time                `json:"claimed_until,omitempty"`
	CreatedAt      time.Time                 `json:"created_at"`
	UpdatedAt      time.Time                 `json:"updated_at"`
}
