package checkout

import (
	"errors"
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
	Processor       string
	PaymentMethodID string
	PaymentToken    string
	TokenSymbol     string
	Flow            string
	Wallet          string
	Email           string
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
}

type CheckoutSessionConfirmPayment struct {
	Processor string
	Signature string
	Wallet    string
}

type CheckoutSessionConfirmRequest struct {
	Payment CheckoutSessionConfirmPayment
}

type CheckoutSessionRedirectToURL struct {
	URL       string `json:"url,omitempty"`
	ReturnURL string `json:"return_url,omitempty"`
}

type CheckoutSessionNextAction struct {
	Type          string                        `json:"type"`
	RedirectToURL *CheckoutSessionRedirectToURL `json:"redirect_to_url,omitempty"`
}

type CheckoutSessionPaymentResponse struct {
	Processor      string `json:"processor"`
	Reference      string `json:"reference,omitempty"`
	TransactionURL string `json:"transaction_url,omitempty"`
	SolanaPayURL   string `json:"solana_pay_url,omitempty"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	TransactionID  string `json:"transaction_id,omitempty"`
}

type CheckoutSessionResponse struct {
	Object         string                         `json:"object"`
	ID             string                         `json:"id"`
	Status         string                         `json:"status"`
	Mode           string                         `json:"mode"`
	PriceID        string                         `json:"price_id"`
	Payment        CheckoutSessionPaymentResponse `json:"payment"`
	PaymentID      *string                        `json:"payment_id,omitempty"`
	SubscriptionID *string                        `json:"subscription_id,omitempty"`
	ExpiresAt      *time.Time                     `json:"expires_at,omitempty"`
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`
	Message        string                         `json:"message,omitempty"`
	Metadata       map[string]string              `json:"metadata,omitempty"`
}
