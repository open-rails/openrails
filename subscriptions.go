package openrails

import "time"

// Page is the common bounded list envelope. Empty lists contain data: [].
type Page[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

type PageOptions struct{ Limit, Offset int }

type SubscriptionFilter struct {
	PageOptions
	CustomerID CustomerID
	Status     string
	Rail       string
}

// Subscription exposes lifecycle and recovery state without making provider
// credentials or mutable storage models part of the client contract. Ids are
// the typed family of ids.go; PSPID is the provider account's plain UUID.
type Subscription struct {
	LastRetryAt           *time.Time            `json:"last_retry_at"`
	RetryAttempts         *int                  `json:"retry_attempts"`
	NextRetryAt           *time.Time            `json:"next_retry_at"`
	GraceEndsAt           *time.Time            `json:"grace_ends_at"`
	DeletionScheduledAt   *time.Time            `json:"deletion_scheduled_at,omitempty"`
	Payments              []SubscriptionPayment `json:"payments,omitempty"`
	ID                    SubscriptionID        `json:"id"`
	CustomerID            CustomerID            `json:"customer_id"`
	ProductID             ProductID             `json:"product_id"`
	PriceID               PriceID               `json:"price_id"`
	PSPID                 string                `json:"psp_id"`
	Rail                  string                `json:"rail"`
	RailSubscriptionID    string                `json:"rail_subscription_id"`
	Status                string                `json:"status"`
	ScheduledPriceID      *PriceID              `json:"scheduled_price_id,omitempty"`
	PaymentMethodID       *PaymentMethodID      `json:"payment_method_id"`
	StartedAt             time.Time             `json:"started_at"`
	EndedAt               *time.Time            `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time            `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time            `json:"current_period_ends_at"`
	CancelledAt           *time.Time            `json:"cancelled_at"`
	CancelType            *string               `json:"cancel_type"`
	CancelFeedback        *string               `json:"cancel_feedback"`
	Resumable             bool                  `json:"resumable"`
	CancelScheduled       bool                  `json:"cancel_scheduled"`
	CancelMode            string                `json:"cancel_mode"`
	Price                 *SubscriptionPrice    `json:"price,omitempty"`
	Product               *SubscriptionProduct  `json:"product,omitempty"`
	CreatedAt             time.Time             `json:"created_at"`
	UpdatedAt             time.Time             `json:"updated_at"`
}

type SubscriptionPrice struct {
	ID                  PriceID   `json:"id"`
	Key                 string    `json:"key"`
	ProductID           ProductID `json:"product_id"`
	Amount              int64     `json:"amount,string"`
	Currency            string    `json:"currency"`
	AutoRenew           bool      `json:"auto_renew"`
	AccessDurationHours *int      `json:"access_duration_hours"`
	Archived            bool      `json:"archived"`
}

type SubscriptionProduct struct {
	ID          ProductID `json:"id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	TierGroup   *string   `json:"tier_group,omitempty"`
	TierRank    int       `json:"tier_rank"`
	Archived    bool      `json:"archived"`
}

type CancelSubscriptionRequest struct {
	Reason       string `json:"reason"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
}

type UpdateSubscriptionPaymentMethodRequest struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

// CodePaymentMethodPSPMismatch: the saved method the request named was vaulted
// by a different provider account than the one that owns the subscription
// (type invalid_request_error, 409). Provider vault references are
// account-scoped, so nothing was sent to the provider; collect the card again
// on the subscription's active provider account.
const CodePaymentMethodPSPMismatch = "payment_method_psp_mismatch"

var ErrPaymentMethodPSPMismatch error = newCodedError(CodePaymentMethodPSPMismatch, ErrConflict)

// SubscriptionPayment is an immutable payment summary for recovery history.
type SubscriptionPayment struct {
	ID            PaymentID `json:"id"`
	Status        string    `json:"status"`
	Amount        int64     `json:"amount,string"`
	Currency      string    `json:"currency"`
	Rail          string    `json:"rail"`
	TransactionID string    `json:"transaction_id"`
	PurchasedAt   time.Time `json:"purchased_at"`
}
