package checkout

import (
	"errors"
	"github.com/open-rails/openrails"
	"time"
)

var (
	ErrCheckoutSessionValidation       = errors.New("checkout session validation failed")
	ErrCheckoutSessionNotFound         = errors.New("checkout session not found")
	ErrCheckoutSessionForbidden        = errors.New("checkout session access denied")
	ErrCheckoutSessionExpired          = errors.New("checkout session expired")
	ErrCheckoutSessionPending          = errors.New("checkout session request already pending")
	ErrCheckoutSessionConflict         = errors.New("checkout session conflict")
	ErrCheckoutSessionNotSolana        = errors.New("checkout session is not a solana session")
	ErrCheckoutSessionAlreadyCompleted = errors.New("checkout session already completed")
)

type CheckoutSessionPaymentRequest struct {
	Rail            string
	PaymentMethodID string
	PaymentToken    string
	TokenSymbol     string
	Flow            string
	Wallet          string
	Email           string
	NameOnCard      string
	FirstName       string
	LastName        string
	Address1        string
	City            string
	State           string
	Zip             string
	Country         string
	LastFour        string
	CardType        string
	ExpiryDate      string
}

type CheckoutSessionCreateRequest struct {
	PriceID        string
	Mode           string
	Payment        CheckoutSessionPaymentRequest
	Metadata       map[string]string
	IdempotencyKey string

	// SubscriptionID is required for the solana_cancel / solana_tier_change modes:
	// the target lifecycle subscription the session acts on. The acting user must
	// own it (authorized server-side).
	SubscriptionID string
	// NewPriceID is required for the solana_tier_change mode: the price to change
	// TO. For a tier-change session this becomes the session's PriceID; PriceID in
	// the request is ignored.
	NewPriceID string

	// SuccessURL / CancelURL are the post-checkout redirect targets for hosted
	// Stripe Checkout, supplied by the caller (frontend, which knows its own
	// origin). Empty for non-Stripe rails. Threaded onto the CheckoutRequest
	// in initializeCheckoutSession; processStripeSubscription/Payment require them.
	SuccessURL string
	CancelURL  string
}

type CheckoutSessionConfirmPayment struct {
	Rail      string
	Signature string
	Wallet    string
}

type CheckoutSessionConfirmRequest struct {
	Payment CheckoutSessionConfirmPayment
}

type CheckoutSessionRedirectToURL = openrails.CheckoutSessionRedirectToURL

type CheckoutSessionNextAction = openrails.CheckoutSessionNextAction

type CheckoutSessionPaymentResponse = openrails.CheckoutSessionPaymentResponse

type CheckoutSessionResponse struct {
	Object         string                         `json:"object"`
	ID             string                         `json:"id"`
	Status         string                         `json:"status"`
	Mode           string                         `json:"mode"`
	PriceID        string                         `json:"price_id"`
	URL            string                         `json:"url,omitempty"`
	Payment        CheckoutSessionPaymentResponse `json:"payment"`
	PaymentID      *string                        `json:"payment_id,omitempty"`
	SubscriptionID *string                        `json:"subscription_id,omitempty"`
	ExpiresAt      *time.Time                     `json:"expires_at,omitempty"`
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`
	Message        string                         `json:"message,omitempty"`
	Metadata       map[string]string              `json:"metadata,omitempty"`
}
