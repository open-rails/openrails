package checkout

import (
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

type UserIdentity struct {
	ID       string
	Email    *string
	Username string
	Roles    []string
}

type CheckoutRequest struct {
	PriceID         string `json:"price_id"`
	PaymentMethodID string `json:"payment_method_id,omitempty"`
	PaymentToken    string `json:"payment_token,omitempty"`
	Processor       string `json:"processor"`
	SuccessURL      string `json:"success_url,omitempty"`
	CancelURL       string `json:"cancel_url,omitempty"`

	IdempotencyKey    string            `json:"-"`
	CheckoutSessionID string            `json:"-"`
	Email             string            `json:"email,omitempty"`
	FirstName         string            `json:"first_name,omitempty"`
	LastName          string            `json:"last_name,omitempty"`
	Address1          string            `json:"address1,omitempty"`
	City              string            `json:"city,omitempty"`
	State             string            `json:"state,omitempty"`
	Zip               string            `json:"zip,omitempty"`
	Country           string            `json:"country,omitempty"`
	LastFour          string            `json:"last_four,omitempty"`
	CardType          string            `json:"card_type,omitempty"`
	ExpiryDate        string            `json:"expiry_date,omitempty"`
	Metadata          map[string]string `json:"-"`
}

type CheckoutResponse struct {
	Status         string     `json:"status"`
	Action         string     `json:"action,omitempty"`
	Message        string     `json:"message,omitempty"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	TransactionID  string     `json:"transaction_id,omitempty"`
	RedirectURL    string     `json:"redirect_url,omitempty"`
	DelayedStart   *time.Time `json:"delayed_start,omitempty"`
}

type CoverageInfo struct {
	HasCoverage  bool
	IsIndefinite bool
	EndDate      *time.Time
	SourceType   string
	SourceID     *uuid.UUID
}

type EligibilityStatus string

const (
	EligibilityAllowed   EligibilityStatus = "allowed"
	EligibilityBlocked   EligibilityStatus = "blocked"
	EligibilityUpgrade   EligibilityStatus = "upgrade"
	EligibilityDowngrade EligibilityStatus = "downgrade"
)

type EligibilityResult struct {
	Status               EligibilityStatus
	Reason               string
	Coverage             *CoverageInfo
	ExistingSubscription *models.Subscription
	ExistingProduct      *models.Product
}

type UpgradeResponse struct {
	*CheckoutResponse
	ProratedAmount    int64      `json:"prorated_amount,omitempty"`
	OldSubscriptionID *uuid.UUID `json:"old_subscription_id,omitempty"`
}

type RegisterPurchaseResponse struct {
	PaymentID    uuid.UUID
	Entitlements []string
	DelayedStart *time.Time
	Eligibility  EligibilityStatus
}

type SolanaPayResult struct {
	URL            string
	Reference      string
	Amount         int64
	Currency       string
	TokenAmount    string
	TokenUnits     uint64
	TokenMint      string
	Recipient      string
	TokenPriceUSD  float64
	FXRate         float64
	FXCurrency     string
	QuotedAt       time.Time
	QuoteExpiresAt time.Time
	Token          string
	ExpiresAt      time.Time
}

type SolanaTransactionBuildResponse struct {
	TransactionBase64 string
	Amount            int64
	TokenAmount       uint64
	TokenSymbol       string
	ExpiresAt         time.Time
	Instructions      string
}

type SolanaPaySessionInfo struct {
	ProductName string
}

type SolanaPayTransactionResponse struct {
	TransactionBase64 string
	Message           string
}
