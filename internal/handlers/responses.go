package handlers

import (
	"time"

	"github.com/doujins-org/ginapi/response"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/services"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
)

type GetSubscriptionResponse = services.UserSubscriptionResponse

// GetProductsResponse is now a Stripe-like list response
type GetProductsResponse = response.List[api.ProductObject]

// GetPricesResponse is a Stripe-like list response for prices
type GetPricesResponse = response.List[api.PriceObject]

// ProductToAPI converts a models.Product to api.ProductObject
func ProductToAPI(p *models.Product, prices []*models.Price) api.ProductObject {
	priceObjects := make([]api.PriceObject, len(prices))
	for i, price := range prices {
		priceObjects[i] = PriceToAPI(price)
	}

	return api.ProductObject{
		ID:          api.FormatProductID(p.ID),
		Object:      "product",
		Name:        p.DisplayName,
		Description: p.Description,
		Active:      p.IsActive,
		Livemode:    false,
		Metadata:    map[string]string{},
		Created:     api.ToUnix(p.CreatedAt),
		Updated:     api.ToUnix(p.UpdatedAt),
		Prices:      priceObjects,
	}
}

// PaymentToAPI converts a models.Payment to api.PaymentObject
// If refunds is provided, it calculates amount_refunded and includes the refunds list
func PaymentToAPI(p *models.Payment, refunds []*models.Payment) api.PaymentObject {
	var subID *string
	if p.SubscriptionID != nil {
		s := api.FormatSubscriptionID(*p.SubscriptionID)
		subID = &s
	}

	// Calculate refund totals
	var amountRefunded int64
	var refundObjects []api.PaymentObject

	for _, r := range refunds {
		// Refunds have negative amounts, so we negate to get positive refund amount
		if r.Amount < 0 {
			amountRefunded += -r.Amount
		} else {
			amountRefunded += r.Amount
		}
		refundObjects = append(refundObjects, PaymentToAPI(r, nil))
	}

	status := "succeeded"
	refunded := amountRefunded >= p.Amount && p.Amount > 0
	if refunded {
		status = "refunded"
	} else if amountRefunded > 0 {
		status = "partially_refunded"
	}

	payment := api.PaymentObject{
		ID:             api.FormatPaymentID(p.ID),
		Object:         "charge",
		Status:         status,
		Amount:         p.Amount,
		AmountRefunded: amountRefunded,
		Currency:       p.Currency,
		User:           api.FormatUserID(p.UserID),
		Subscription:   subID,
		Processor:      string(p.Processor),
		TransactionID:  p.TransactionID,
		Refunded:       refunded,
		Captured:       true,
		Created:        api.ToUnix(p.CreatedAt),
	}

	// Include refunds list if provided (always include for single payment detail view)
	if refunds != nil {
		// Ensure Data is never nil (use empty slice if no refunds)
		if refundObjects == nil {
			refundObjects = []api.PaymentObject{}
		}
		payment.Refunds = &api.PaymentRefundsList{
			Object: "list",
			Data:   refundObjects,
		}
	}

	// Include expanded price if available
	if p.Price != nil {
		priceObj := PriceToAPI(p.Price)
		payment.Price = &priceObj
	}

	return payment
}

// PriceToAPI converts a models.Price to api.PriceObject
func PriceToAPI(p *models.Price) api.PriceObject {
	var recurring *api.RecurringInfo
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, intervalCount := sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays)
		recurring = &api.RecurringInfo{
			Interval:      interval,
			IntervalCount: intervalCount,
		}
	}

	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}

	return api.PriceObject{
		ID:        api.FormatPriceID(p.ID),
		Object:    "price",
		Name:      p.DisplayName,
		Amount:    p.Amount,
		Currency:  p.Currency,
		Type:      priceType,
		Recurring: recurring,
		Product:   api.FormatProductID(p.ProductID),
		Active:    p.IsActive,
		Livemode:  false,
		Metadata:  map[string]string{},
		Created:   api.ToUnix(p.CreatedAt),
	}
}

// -------------------------------- Solana / Payments Responses --------------------------------

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// SupportedTokensResponse lists available Solana tokens from config
type SupportedTokensResponse struct {
	Tokens []TokenInfo `json:"tokens"`
}

type TokenInfo struct {
	Symbol   string        `json:"symbol"`
	Name     string        `json:"name"`
	Mint     string        `json:"mint"`
	Decimals int           `json:"decimals"`
	Price    float64       `json:"price"`
	Quote    *TokenQuote   `json:"quote,omitempty"`
	Balance  *TokenBalance `json:"balance,omitempty"`
}

// TokenQuote represents the amount of a token required to pay for a specific price.
type TokenQuote struct {
	Amount        string  `json:"amount"`          // Human-readable token amount (e.g., "9.99")
	Units         uint64  `json:"units"`           // Token amount in smallest units
	TokenPriceUSD float64 `json:"token_price_usd"` // Token price in USD at quote time
	FXRate        float64 `json:"fx_rate"`         // FX rate used (1.0 for USD prices)
	FXCurrency    string  `json:"fx_currency"`     // Source currency (e.g., "eur", "usd")
	QuotedAt      string  `json:"quoted_at"`       // When the quote was generated (RFC3339)
	ExpiresAt     string  `json:"expires_at"`      // When the quote expires (RFC3339)
}

// TokenBalance represents the user's on-chain balance for a token.
type TokenBalance struct {
	Amount     string `json:"amount"`     // Human-readable balance (e.g., "125.50")
	Units      uint64 `json:"units"`      // Balance in smallest units
	Sufficient bool   `json:"sufficient"` // True if balance >= quote.units (when quote is present)
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

// -------------------------------- Payment Method Responses --------------------------------

// SubscriptionSummary represents a minimal subscription for payment method responses
type SubscriptionSummary struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// PaymentMethodResponse represents a Stripe-style payment method (card)
type PaymentMethodResponse struct {
	ID             string                       `json:"id"`                 // pm_xxx
	Object         string                       `json:"object"`             // "payment_method"
	Type           string                       `json:"type"`               // "card"
	Processor      string                       `json:"processor"`          // nmi, mobius, etc.
	Customer       *string                      `json:"customer,omitempty"` // usr_ prefix if available
	BillingDetails *PaymentMethodBillingDetails `json:"billing_details,omitempty"`
	Card           *PaymentMethodCardDetails    `json:"card,omitempty"`
	Metadata       map[string]string            `json:"metadata,omitempty"`
	Livemode       bool                         `json:"livemode"`
	Created        int64                        `json:"created"` // Unix epoch seconds
	FailureReason  *string                      `json:"failure_reason,omitempty"`
	Subscriptions  []SubscriptionSummary        `json:"subscriptions,omitempty"`
}

type PaymentMethodBillingDetails struct {
	Name    *string               `json:"name,omitempty"`
	Email   *string               `json:"email,omitempty"`
	Phone   *string               `json:"phone,omitempty"`
	Address *PaymentMethodAddress `json:"address,omitempty"`
}

type PaymentMethodAddress struct {
	Line1      *string `json:"line1,omitempty"`
	Line2      *string `json:"line2,omitempty"`
	City       *string `json:"city,omitempty"`
	State      *string `json:"state,omitempty"`
	PostalCode *string `json:"postal_code,omitempty"`
	Country    *string `json:"country,omitempty"`
}

type PaymentMethodCardDetails struct {
	Brand    *string `json:"brand,omitempty"` // visa, mastercard, amex
	Last4    *string `json:"last4,omitempty"`
	ExpMonth *int    `json:"exp_month,omitempty"`
	ExpYear  *int    `json:"exp_year,omitempty"`
}

// PaymentMethodToAPI converts a models.PaymentMethod to a Stripe-compatible PaymentMethodResponse
func PaymentMethodToAPI(pm *models.PaymentMethod) PaymentMethodResponse {
	card := &PaymentMethodCardDetails{
		Brand: pm.CardType,
		Last4: pm.LastFour,
	}
	if pm.ExpiryDate != nil {
		if month, year, ok := sharedformat.ParseExpiry(*pm.ExpiryDate); ok {
			card.ExpMonth = &month
			card.ExpYear = &year
		}
	}

	// Compose subscription summaries
	var subs []SubscriptionSummary
	for _, s := range pm.Subscriptions {
		summary := SubscriptionSummary{
			ID:        s.ID.String(),
			CreatedAt: s.CreatedAt,
		}
		if s.Product != nil {
			summary.DisplayName = s.Product.DisplayName
			summary.Description = s.Product.Description
		}
		subs = append(subs, summary)
	}

	return PaymentMethodResponse{
		ID:            api.FormatPaymentMethodID(pm.ID),
		Object:        "payment_method",
		Type:          "card",
		Processor:     string(pm.Processor),
		Card:          card,
		Created:       api.ToUnix(pm.CreatedAt),
		Metadata:      map[string]string{},
		FailureReason: pm.FailureReason,
		Subscriptions: subs,
	}
}

// PaymentMethodsToAPI converts a slice of models.PaymentMethod to PaymentMethodResponse slice
func PaymentMethodsToAPI(methods []*models.PaymentMethod) []PaymentMethodResponse {
	result := make([]PaymentMethodResponse, len(methods))
	for i, pm := range methods {
		result[i] = PaymentMethodToAPI(pm)
	}
	return result
}
