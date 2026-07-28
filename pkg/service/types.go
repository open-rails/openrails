// Package service provides the in-process billing API for embedded hosts.
//
// All types in this file are exported and safe to use by external packages.
// These types do not import from internal/* packages.
package service

import (
	"time"

	"github.com/google/uuid"
)

// -------------------------------- Pagination --------------------------------

// PaginationOptions specifies limit/offset pagination parameters.
type PaginationOptions struct {
	Limit  int
	Offset int
}

// PaginatedResult wraps a paginated response with total count.
type PaginatedResult[T any] struct {
	Data       []T
	TotalItems int64
	Limit      int
	Offset     int
}

// -------------------------------- Products --------------------------------

// GetProductsOptions specifies filters for listing products.
type GetProductsOptions struct {
	PaginationOptions
	// Active filters by product active status. If nil, defaults to true.
	Active *bool
	// IncludeInactive allows admins to see inactive products.
	IncludeInactive bool
}

// Product represents a billing product.
type Product struct {
	ID               string
	Key              string
	Name             string
	Description      string
	EntitlementsSpec map[string]*int
	CreditsSpec      CreditsSpec
	TierGroup        *string
	TierRank         int
	Active           bool
	Created          int64 // Unix epoch seconds
	Updated          int64 // Unix epoch seconds
	Prices           []Price
}

// -------------------------------- Prices --------------------------------

// GetPricesOptions specifies filters for listing prices.
type GetPricesOptions struct {
	PaginationOptions
	// Active filters by price active status. If nil, defaults to true.
	Active *bool
	// Currency filters by currency code (e.g., "usd").
	Currency string
	// ProductID filters by product ID (UUID, not prefixed).
	ProductID *uuid.UUID
	// Type filters by price type: "recurring" or "one_time".
	Type string
	// IncludeInactive allows admins to see inactive prices.
	IncludeInactive bool
}

// Price represents a billing price.
type Price struct {
	ID         string
	UnitAmount int64  // Amount in the currency's smallest unit
	Currency   string // e.g., "usd"
	Type       string // "one_time" or "recurring"
	Recurring  *RecurringInfo
	ProductID  string
	Active     bool
	Created    int64 // Unix epoch seconds
}

// RecurringInfo describes the billing interval for recurring prices.
type RecurringInfo struct {
	Interval string // e.g. "30d", "365d"
}

// -------------------------------- Checkout Sessions --------------------------------

// CheckoutRailOption is a locally ready payment-provider choice for a price.
// Selector is the exact value accepted by CheckoutPayment.Rail; Rail is the
// canonical gateway and Mode is "one_off" or "subscription".
type CheckoutRailOption struct {
	Selector string
	Rail     string
	Mode     string
}

// CreateCheckoutSessionRequest specifies checkout session creation parameters.
type CreateCheckoutSessionRequest struct {
	PriceID        string
	Mode           string // "one_off" or "subscription" (optional, inferred from price)
	Payment        CheckoutPayment
	Metadata       map[string]string
	IdempotencyKey string
	SuccessURL     string // Required for Stripe hosted checkout
	CancelURL      string // Required for Stripe hosted checkout
}

// CheckoutPayment specifies payment details for checkout.
type CheckoutPayment struct {
	Rail            string // "nmi", "ccbill", "solana", "stripe"
	PaymentMethodID string // For returning customers with saved payment methods
	PaymentToken    string // For new card tokenization (NMI Collect.js)

	// Solana-specific
	TokenSymbol string // e.g., "USDC", "SOL"
	Flow        string // "transfer_request" or "transaction_request"
	Wallet      string // Solana wallet address

	// Billing details (CCBill/Stripe)
	Email     string
	FirstName string
	LastName  string
	Address1  string
	City      string
	State     string
	Zip       string
	Country   string

	// Card details (for display, from tokenization)
	LastFour   string
	CardType   string
	ExpiryDate string
}

// CheckoutSession represents a checkout session.
type CheckoutSession struct {
	ID             string
	Status         string // "created", "requires_action", "succeeded", "failed", "expired"
	Mode           string // "subscription", "one_off"
	PriceID        string
	Amount         int64
	Currency       string
	PaymentStatus  string // "unpaid", "paid", "no_payment_required"
	ClientSecret   *string
	URL            *string // Redirect URL for CCBill/Stripe
	SubscriptionID *string
	PaymentID      *string
	ExpiresAt      int64 // Unix epoch seconds
	Created        int64 // Unix epoch seconds
	Metadata       map[string]string
	RailData       map[string]any // Rail-specific response data
}

// ConfirmCheckoutSessionRequest specifies checkout confirmation parameters.
type ConfirmCheckoutSessionRequest struct {
	Payment ConfirmPayment
}

// ConfirmPayment specifies payment confirmation details (primarily for Solana).
type ConfirmPayment struct {
	Rail      string // Must match session rail
	Signature string // Solana transaction signature
	Wallet    string // Solana wallet that signed
}

// -------------------------------- Billing Status --------------------------------

// BillingStatus represents a user's overall billing status.
type BillingStatus struct {
	HasActiveSubscription bool
	Subscription          *SubscriptionDetail
	NextRenewalAt         *time.Time
	Entitlements          []string
}

// -------------------------------- Subscriptions --------------------------------

// GetSubscriptionsOptions specifies filters for listing subscriptions.
type GetSubscriptionsOptions struct {
	PaginationOptions
	// Status filters by subscription status. Empty means non-cancelled.
	// Use "all" to include cancelled subscriptions.
	Status string
}

// Subscription represents a user subscription.
type Subscription struct {
	ID                    string
	Status                string // "pending", "active", "past_due", "cancelled"
	Rail                  string // "nmi", "ccbill", "solana", "stripe"
	RailSubscriptionID    string
	StartedAt             int64  // Unix epoch seconds
	EndedAt               *int64 // Unix epoch seconds (nil if active)
	CurrentPeriodStartsAt *int64 // Unix epoch seconds
	CurrentPeriodEndsAt   *int64 // Unix epoch seconds
	CancelledAt           *int64 // Unix epoch seconds
	CancelType            *string
	CancelFeedback        *string
	Created               int64 // Unix epoch seconds
	Updated               int64 // Unix epoch seconds
	Price                 *Price
	PaymentMethod         *PaymentMethodSummary

	// Resumability surface (issues 215/216). These are derived from the single
	// shared predicate so library and HTTP consumers agree.
	//
	// Resumable: the cancellation can be undone right now (cancel mode is
	// reversible && status==cancelled && current period end in the future).
	// CancelScheduled: cancelled but the user retains paid access until the
	// current period ends (status==cancelled && current period end in the future);
	// the expiry date is CurrentPeriodEndsAt.
	// CancelMode: "reversible" | "destructive" | "external_portal".
	// CancelPortalURL: external consumer-portal URL for external_portal rails
	// (none since #696 — CCBill cancels via the DataLink SMS choke point).
	Resumable       bool
	CancelScheduled bool
	CancelMode      string
	CancelPortalURL *string
}

// SubscriptionDetail includes full subscription details for billing status.
type SubscriptionDetail struct {
	Subscription
	Product *Product
}

// CancelSubscriptionRequest specifies subscription cancellation parameters.
type CancelSubscriptionRequest struct {
	Feedback string // Optional cancellation feedback
}

// CancelSubscriptionResult is the result of a cancellation request.
type CancelSubscriptionResult struct {
	Success bool
	Message string
}

// ResumeSubscriptionResult is the result of a resume request.
type ResumeSubscriptionResult struct {
	Success bool
	Message string
}

// ChangeTierRequest specifies tier change parameters.
type ChangeTierRequest struct {
	NewPriceID string
}

// ChangeTierResult is the result of a tier change request.
type ChangeTierResult struct {
	Success        bool
	Message        string
	SubscriptionID string
}

// UpdateSubscriptionPaymentMethodRequest specifies payment method update parameters.
type UpdateSubscriptionPaymentMethodRequest struct {
	SubscriptionID  string
	PaymentMethodID string
}

// UpdateSubscriptionPaymentMethodResult is the result of updating subscription payment method.
type UpdateSubscriptionPaymentMethodResult struct {
	Success         bool
	Message         string
	SubscriptionID  string
	PaymentMethodID string
}

// -------------------------------- Payments --------------------------------

// GetPaymentsOptions specifies filters for listing payments.
type GetPaymentsOptions struct {
	PaginationOptions
	// UserID filters by user (for admin use).
	UserID string
	// SubscriptionID filters by subscription.
	SubscriptionID *uuid.UUID
	// Rail filters by payment rail.
	Rail string
	// StartDate filters payments after this date.
	StartDate *time.Time
	// EndDate filters payments before this date.
	EndDate *time.Time
}

// Payment represents a payment transaction.
type Payment struct {
	ID              string
	Status          string // "succeeded", "pending", "failed", "refunded", "partially_refunded"
	Amount          int64  // Amount in cents (positive for charges, negative for refunds)
	AmountRefunded  int64
	Currency        string
	UserID          string
	SubscriptionID  *string
	PaymentMethodID *string
	Rail            string
	TransactionID   string
	Refunded        bool
	Created         int64 // Unix epoch seconds
	Price           *Price
	Refunds         []Payment
}

// RefundPaymentRequest specifies refund parameters.
type RefundPaymentRequest struct {
	Amount              int64  // Amount in cents to refund
	RefundTransactionID string // Rail's refund transaction ID
}

// -------------------------------- Payment Methods --------------------------------

// GetPaymentMethodsOptions specifies filters for listing payment methods.
type GetPaymentMethodsOptions struct {
	PaginationOptions
}

// PaymentMethod represents a saved payment method.
type PaymentMethod struct {
	ID             string
	Type           string // "card"
	Rail           string // "nmi", "ccbill", "stripe", etc.
	Created        int64  // Unix epoch seconds
	BillingDetails *BillingDetails
	Card           *CardDetails
	Subscriptions  []SubscriptionSummary
}

// PaymentMethodSummary is a minimal payment method reference.
type PaymentMethodSummary struct {
	ID   string
	Rail string
}

// BillingDetails contains billing address information.
type BillingDetails struct {
	Name    *string
	Email   *string
	Phone   *string
	Address *Address
}

// Address represents a billing address.
type Address struct {
	Line1      *string
	Line2      *string
	City       *string
	State      *string
	PostalCode *string
	Country    *string
}

// CardDetails contains card-specific payment method information.
type CardDetails struct {
	Brand    *string // "visa", "mastercard", "amex"
	Last4    *string
	ExpMonth *int
	ExpYear  *int
}

// SubscriptionSummary is a minimal subscription reference for payment methods.
type SubscriptionSummary struct {
	ID          string
	DisplayName string
	Description string
	Created     int64 // Unix epoch seconds
}

// CreatePaymentMethodRequest specifies payment method creation parameters.
type CreatePaymentMethodRequest struct {
	PaymentToken string // NMI Collect.js token
	FirstName    string
	LastName     string
	Address1     string
	City         string
	State        string
	Zip          string
	Country      string
	Phone        string
	Email        string
	Company      string
	Address2     string
	Provider     string // Optional override
	LastFour     string // From tokenization
	CardType     string // From tokenization
	ExpiryDate   string // "MM/YY"
}

// UpdatePaymentMethodRequest specifies payment method update parameters.
type UpdatePaymentMethodRequest struct {
	PaymentToken string // New token with updated card
	FirstName    *string
	LastName     *string
	Address1     *string
	City         *string
	State        *string
	Zip          *string
	Country      *string
	Phone        *string
	Email        *string
	Company      *string
	Address2     *string
	Provider     *string
	LastFour     *string
	CardType     *string
	ExpiryDate   *string
}

// -------------------------------- Notifications --------------------------------

// GetNotificationsOptions specifies filters for listing notifications.
type GetNotificationsOptions struct {
	PaginationOptions
	// Seen filters by read status. nil = all, true = read only, false = unread only.
	Seen *bool
}

// Notification represents an in-app notification.
type Notification struct {
	ID      string
	Type    string // e.g., "subscription_cancelled", "payment_failed"
	Title   string
	Message string
	Seen    bool
	Data    map[string]any
	Created int64 // Unix epoch seconds
}

// UnreadNotificationCount is the count of unread notifications.
type UnreadNotificationCount struct {
	Count int64
}

// -------------------------------- Credits --------------------------------

// CreditBalance represents a user's balance for a currency.
type CreditBalance struct {
	Currency      string
	DisplayName   string
	Unit          string
	DecimalPlaces int
	Balance       int64
	HeldBalance   int64
}

// GetCreditTransactionsOptions specifies filters for listing credit transactions.
type GetCreditTransactionsOptions struct {
	PaginationOptions
}

// NOTE: HoldCreditsRequest, CreditHold, CaptureHoldRequest, CreditTransaction,
// WithdrawCreditsRequest, and EntitlementRecord are defined in service.go
// as they are part of the existing API.

// -------------------------------- Solana Tokens --------------------------------

// SolanaToken represents a supported Solana token.
type SolanaToken struct {
	Symbol   string
	Name     string
	Mint     string // Token mint address
	Decimals int
	Price    float64 // Current USD price
}

// SupportedTokensResult contains the list of supported Solana tokens.
type SupportedTokensResult struct {
	Tokens []SolanaToken
}

// -------------------------------- Stripe Portal --------------------------------

// CreateStripePortalSessionRequest specifies Stripe portal session parameters.
type CreateStripePortalSessionRequest struct {
	ReturnURL string // URL to return to after portal session
}

// StripePortalSession represents a Stripe customer portal session.
type StripePortalSession struct {
	RedirectURL string
}

// -------------------------------- Admin Operations --------------------------------

// AdminGetSubscriptionsOptions specifies admin filters for listing subscriptions.
type AdminGetSubscriptionsOptions struct {
	PaginationOptions
	UserID string
	Status string
	Rail   string
}

// AdminGetPaymentsOptions specifies admin filters for listing payments.
type AdminGetPaymentsOptions struct {
	PaginationOptions
	UserID         string
	SubscriptionID *uuid.UUID
	Rail           string
	StartDate      *time.Time
	EndDate        *time.Time
}

// AdminUserProfile represents a user's billing profile for admin view.
type AdminUserProfile struct {
	UserID        string
	Email         string
	Subscriptions []Subscription
	Payments      []Payment
	Entitlements  []EntitlementRecord
}

// EntitlementGrantEntitlementRequest specifies entitlement grant parameters.
type EntitlementGrantEntitlementRequest struct {
	UserID      string
	Entitlement string
	StartAt     *time.Time // nil = now
	EndAt       *time.Time // nil = indefinite
	Reason      string
}

// AdminRevokeEntitlementRequest specifies entitlement revocation parameters.
type AdminRevokeEntitlementRequest struct {
	Reason string
}

// AdminCreateOffChannelPaymentRequest specifies off-channel payment parameters.
type AdminCreateOffChannelPaymentRequest struct {
	UserID        string
	PriceID       string
	Amount        int64
	Currency      string
	TransactionID string
	Rail          string
	Note          string
}

// -------------------------------- Webhooks --------------------------------

// HandleWebhookRequest contains the raw webhook data.
type HandleWebhookRequest struct {
	Provider  string            // "nmi", "ccbill", "stripe", "solana"
	Body      []byte            // Raw request body
	Headers   map[string]string // Relevant headers (signatures, etc.)
	ClientIP  string
	EventType string // Parsed event type if available
}

// WebhookResult contains the result of webhook processing.
type WebhookResult struct {
	Accepted  bool
	EventID   string
	EventType string
	Error     string // Non-empty if processing failed
}
