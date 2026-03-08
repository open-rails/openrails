package handlers

import "github.com/open-rails/openrails/pkg/api"

// -------------------------------- Solana / Payments Responses --------------------------------

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type PublicPriceResponse struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Amount    int64              `json:"amount"` // Amount in cents
	Currency  string             `json:"currency"`
	Recurring *api.RecurringInfo `json:"recurring,omitempty"`
}

// (Removed) NMI setup response and nonce: no longer needed because
// the Collect.js tokenization key is injected into the frontend template.

type CancelSubscriptionResponse struct {
	Message string `json:"message"`
	Success bool   `json:"success"`
}

type GenerateFlexFormURLResponse struct {
	RedirectURL string `json:"redirect_url" binding:"required"`
}

// -------------------------------- Enhanced Billing History Responses --------------------------------

// SubscriptionHistoryItem represents a subscription record with price details
type SubscriptionHistoryItem struct {
	ID                      string                 `json:"id"`
	Status                  string                 `json:"status"`
	Processor               string                 `json:"processor"`
	ProcessorSubscriptionID string                 `json:"processor_subscription_id"`
	StartedAt               int64                  `json:"started_at"`                         // Unix epoch seconds
	EndedAt                 *int64                 `json:"ended_at,omitempty"`                 // Unix epoch seconds
	CurrentPeriodStartsAt   *int64                 `json:"current_period_starts_at,omitempty"` // Unix epoch seconds
	CurrentPeriodEndsAt     *int64                 `json:"current_period_ends_at,omitempty"`   // Unix epoch seconds
	CancelledAt             *int64                 `json:"cancelled_at,omitempty"`             // Unix epoch seconds
	CancelType              *string                `json:"cancel_type,omitempty"`
	CancelFeedback          *string                `json:"cancel_feedback,omitempty"`
	CreatedAt               int64                  `json:"created_at"` // Unix epoch seconds
	UpdatedAt               int64                  `json:"updated_at"` // Unix epoch seconds
	Price                   *PriceInfo             `json:"price,omitempty"`
	PaymentMethod           *PaymentMethodInfo     `json:"payment_method,omitempty"`
	Metadata                map[string]interface{} `json:"metadata,omitempty"`
}

// PriceInfo represents price information for billing history
type PriceInfo struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Amount    int64              `json:"amount"` // Amount in cents
	Currency  string             `json:"currency"`
	Recurring *api.RecurringInfo `json:"recurring,omitempty"`
}

// PaymentMethodInfo represents payment method information for billing history
type PaymentMethodInfo struct {
	ID        string `json:"id"`
	Processor string `json:"processor"`
}

// PaymentItem represents a canonical payment record from Postgres
type PaymentItem struct {
	ID             string     `json:"id"`
	SubscriptionID *string    `json:"subscription_id,omitempty"`
	Processor      string     `json:"processor"`
	TransactionID  string     `json:"transaction_id"`
	Amount         int64      `json:"amount"` // Amount in cents
	Currency       string     `json:"currency"`
	Price          *PriceInfo `json:"price,omitempty"`
	PurchasedAt    int64      `json:"purchased_at"` // Unix epoch seconds
}

// PaymentEventItem represents a payment transaction event
type PaymentEventItem struct {
	EventID                string                 `json:"event_id"`
	SubscriptionID         *string                `json:"subscription_id,omitempty"`
	EventType              string                 `json:"event_type"`
	Processor              string                 `json:"processor"`
	ProcessorTransactionID *string                `json:"processor_transaction_id,omitempty"`
	Amount                 *int64                 `json:"amount,omitempty"` // Amount in cents
	Currency               string                 `json:"currency"`
	BillingInfo            map[string]interface{} `json:"billing_info,omitempty"`
	WebhookSource          *string                `json:"webhook_source,omitempty"`
	Metadata               map[string]interface{} `json:"metadata,omitempty"`
	Timestamp              int64                  `json:"timestamp"`  // Unix epoch seconds
	CreatedAt              int64                  `json:"created_at"` // Unix epoch seconds
}

// SubscriptionEventItem represents a subscription lifecycle event
type SubscriptionEventItem struct {
	EventID                 string                 `json:"event_id"`
	SubscriptionID          string                 `json:"subscription_id"`
	EventType               string                 `json:"event_type"`
	Processor               string                 `json:"processor"`
	ProcessorSubscriptionID *string                `json:"processor_subscription_id,omitempty"`
	ProcessorTransactionID  *string                `json:"processor_transaction_id,omitempty"`
	Amount                  *int64                 `json:"amount,omitempty"` // Amount in cents
	Currency                string                 `json:"currency"`
	Metadata                map[string]interface{} `json:"metadata,omitempty"`
	Timestamp               int64                  `json:"timestamp"`  // Unix epoch seconds
	CreatedAt               int64                  `json:"created_at"` // Unix epoch seconds
}
