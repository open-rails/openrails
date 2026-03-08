package handlers

import (
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
