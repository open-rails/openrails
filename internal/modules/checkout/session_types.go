package checkout

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
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
	PSPID           uuid.UUID
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

	// Card is a new card for a PSP whose card_entry is server (#1129). It is
	// never encoded: it is no part of a request fingerprint or a stored row.
	Card *cardguard.Card `json:"-"`
}

type CheckoutSessionCreateRequest struct {
	PriceID        string
	PriceKey       string
	Entitlement    string
	OfferKind      billing.OfferKind
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

	// Acceptance, when set, is the present payer accepting a recurring price's
	// quoted terms in this request: the quote is confirmed and charged at once.
	Acceptance *billingauth.DelegatedPrincipal
}

type CheckoutSessionConfirmPayment struct {
	Capture   *billing.CustodianCaptureReference
	Rail      string
	Signature string
	Wallet    string
}

type CheckoutSessionConfirmRequest struct {
	Payment CheckoutSessionConfirmPayment
}

type CheckoutSessionRedirectToURL = billing.CheckoutSessionRedirectToURL

type CheckoutSessionNextAction = billing.CheckoutSessionNextAction

type CheckoutSessionPaymentResponse = billing.CheckoutSessionPaymentResponse

// CheckoutSessionMembershipQuote is the immutable commercial agreement shown
// before the customer confirms. It carries no provider or execution authority.
type CheckoutSessionMembershipQuote struct {
	ProductName  string          `json:"product_name"`
	CycleHours   int64           `json:"cycle_hours"`
	Entitlements map[string]*int `json:"entitlements"`
}

type CheckoutSessionResponse struct {
	Operation       *billing.PaymentOperation       `json:"operation,omitempty"`
	Failure         *billing.PaymentFailure         `json:"failure,omitempty"`
	MembershipQuote *CheckoutSessionMembershipQuote `json:"membership_quote,omitempty"`
	Capture         *billing.CustodianCaptureAction `json:"capture,omitempty"`
	PaymentMethodID *billing.PaymentMethodID        `json:"payment_method_id,omitempty"`
	Object          string                          `json:"object"`
	ID              billing.CheckoutSessionID       `json:"id"`
	Status          string                          `json:"status"`
	Mode            string                          `json:"mode"`
	PriceID         *billing.PriceID                `json:"price_id"`
	Amount          *int64                          `json:"amount,string"`
	Currency        *string                         `json:"currency"`
	URL             string                          `json:"url,omitempty"`
	Payment         CheckoutSessionPaymentResponse  `json:"payment"`
	PaymentID       *billing.PaymentID              `json:"payment_id,omitempty"`
	SubscriptionID  *billing.SubscriptionID         `json:"subscription_id,omitempty"`
	ExpiresAt       *time.Time                      `json:"expires_at,omitempty"`
	CreatedAt       time.Time                       `json:"created_at"`
	NextAction      *CheckoutSessionNextAction      `json:"next_action,omitempty"`
	Message         string                          `json:"message,omitempty"`
	Metadata        map[string]string               `json:"metadata,omitempty"`
}
