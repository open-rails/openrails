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
	ErrCheckoutAttemptValidation       = errors.New("checkout attempt validation failed")
	ErrCheckoutAttemptNotFound         = errors.New("checkout attempt not found")
	ErrCheckoutAttemptForbidden        = errors.New("checkout attempt access denied")
	ErrCheckoutAttemptExpired          = errors.New("checkout attempt expired")
	ErrCheckoutAttemptPending          = errors.New("checkout attempt request already pending")
	ErrCheckoutAttemptConflict         = errors.New("checkout attempt conflict")
	ErrCheckoutAttemptNotSolana        = errors.New("checkout attempt is not a solana session")
	ErrCheckoutAttemptAlreadyCompleted = errors.New("checkout attempt already completed")
)

type CheckoutAttemptPaymentRequest struct {
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

type CheckoutAttemptCreateRequest struct {
	PriceID        string
	PriceKey       string
	Entitlement    string
	OfferKind      billing.OfferKind
	Mode           string
	Payment        CheckoutAttemptPaymentRequest
	Metadata       map[string]string
	IdempotencyKey string

	// SuccessURL / CancelURL are the post-checkout redirect targets for hosted
	// Stripe Checkout, supplied by the caller (frontend, which knows its own
	// origin). Empty for non-Stripe rails. Threaded onto the CheckoutRequest
	// in initializeCheckoutAttempt; processStripeSubscription/Payment require them.
	SuccessURL string
	CancelURL  string

	// Acceptance, when set, is the present payer accepting a recurring price's
	// quoted terms in this request: the quote is confirmed and charged at once.
	Acceptance *billingauth.DelegatedPrincipal
}

type CheckoutAttemptConfirmPayment struct {
	Capture   *CustodianCaptureReference
	Rail      string
	Signature string
	Wallet    string
}

type CheckoutAttemptConfirmRequest struct {
	Payment CheckoutAttemptConfirmPayment
}

type CheckoutAttemptRedirectToURL = billing.CheckoutAttemptRedirectToURL

type CheckoutAttemptNextAction = billing.CheckoutAttemptNextAction

type CheckoutAttemptPaymentResponse = billing.CheckoutAttemptPaymentResponse

// CheckoutAttemptMembershipQuote is the immutable commercial agreement shown
// before the customer confirms. It carries no provider or execution authority.
type CheckoutAttemptMembershipQuote struct {
	ProductName  string          `json:"product_name"`
	CycleHours   int64           `json:"cycle_hours"`
	Entitlements map[string]*int `json:"entitlements"`
}

type CheckoutAttemptResponse struct {
	Operation       *billing.PaymentOperation       `json:"operation,omitempty"`
	Failure         *billing.PaymentFailure         `json:"failure,omitempty"`
	MembershipQuote *CheckoutAttemptMembershipQuote `json:"membership_quote,omitempty"`
	Capture         *CustodianCaptureAction         `json:"capture,omitempty"`
	PaymentMethodID *billing.PaymentMethodID        `json:"payment_method_id,omitempty"`
	Object          string                          `json:"object"`
	ID              billing.CheckoutAttemptID       `json:"id"`
	Status          string                          `json:"status"`
	Mode            string                          `json:"mode"`
	PriceID         *billing.PriceID                `json:"price_id"`
	Amount          *int64                          `json:"amount,string"`
	Currency        *string                         `json:"currency"`
	URL             string                          `json:"url,omitempty"`
	Payment         CheckoutAttemptPaymentResponse  `json:"payment"`
	PaymentID       *billing.PaymentID              `json:"payment_id,omitempty"`
	SubscriptionID  *billing.SubscriptionID         `json:"subscription_id,omitempty"`
	ExpiresAt       *time.Time                      `json:"expires_at,omitempty"`
	CreatedAt       time.Time                       `json:"created_at"`
	NextAction      *CheckoutAttemptNextAction      `json:"next_action,omitempty"`
	Message         string                          `json:"message,omitempty"`
	Metadata        map[string]string               `json:"metadata,omitempty"`
}

// CustodianCaptureReference is a vendor session token, not a card or payment
// authority. It is accepted only by the exact owned setup attempt that issued it.
type CustodianCaptureReference struct {
	CustodianID uuid.UUID `json:"custodian_id"`
	SessionID   string    `json:"session_id"`
	Token       string    `json:"token"`
}

func (CustodianCaptureReference) String() string   { return "[private custodian capture reference]" }
func (CustodianCaptureReference) GoString() string { return "[private custodian capture reference]" }

// CustodianCaptureAction initializes vendor-owned browser fields. Its scoped
// authorization is private, short lived, and never a shared cache/log value.
type CustodianCaptureAction struct {
	Kind             string    `json:"kind"`
	CustodianID      uuid.UUID `json:"custodian_id"`
	SessionID        string    `json:"session_id"`
	CustomerID       string    `json:"customer_id"`
	APIBaseURL       string    `json:"api_base_url"`
	SDKURL           string    `json:"sdk_url"`
	PublicAPIKey     string    `json:"public_api_key"`
	SDKAuthorization string    `json:"sdk_authorization"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (CustodianCaptureAction) String() string   { return "[private custodian capture action]" }
func (CustodianCaptureAction) GoString() string { return "[private custodian capture action]" }
