// Package service provides the in-process billing API for embedded hosts.
//
// All types in this file are exported and safe to use by external packages.
// These types do not import from internal/* packages.
package service

import (
	"github.com/open-rails/openrails"
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

// -------------------------------- Checkout Sessions --------------------------------

// CheckoutRailOption is a locally ready payment-provider choice for a price.
// Selector is the exact value accepted by CheckoutPayment.Rail; PSPID is the
// stable provider identity used for server-side method matching; Rail is the
// canonical gateway and Mode is "one_off" or "subscription".
type CheckoutRailOption = openrails.CheckoutRailOption

// CheckoutCustomerIdentity is the host-resolved customer identity used by
// checkout rails that require verified account attributes in addition to the
// stable customer ID.
type CheckoutCustomerIdentity = openrails.CheckoutCustomerIdentity

// CreateCheckoutSessionRequest specifies checkout session creation parameters.
type CreateCheckoutSessionRequest = openrails.CreateCheckoutSessionRequest

// CheckoutPayment specifies payment details for checkout.
type CheckoutPayment = openrails.CheckoutPayment

// CheckoutSession represents a checkout session.
type CheckoutSession = openrails.CheckoutSession

// ConfirmCheckoutSessionRequest specifies checkout confirmation parameters.
type ConfirmCheckoutSessionRequest = openrails.ConfirmCheckoutSessionRequest

// ConfirmPayment specifies payment confirmation details (primarily for Solana).
type ConfirmPayment = openrails.ConfirmPayment

// -------------------------------- Billing Status --------------------------------

// EffectiveTier is the single winning tier for a user within one tier group
// (or#912). Entitlement and ProductKey are IMMUTABLE identifiers — hosts key
// policy documents and token claims on Entitlement; DisplayName is mutable
// and for display only.
type EffectiveTier = openrails.EffectiveTier

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

// NOTE: HoldCreditsRequest, CreditHold, CaptureHoldRequest, CreditTransaction,
// WithdrawCreditsRequest, and EntitlementRecord are defined in service.go
// as they are part of the existing API.

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
