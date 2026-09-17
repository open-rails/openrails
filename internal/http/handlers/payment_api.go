package handlers

import (
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
)

func ProductToAPI(p *models.Product, prices []*models.Price) api.ProductObject {
	priceObjects := make([]api.PriceObject, len(prices))
	for i, price := range prices {
		priceObjects[i] = PriceToAPI(price)
	}
	return api.ProductObject{
		ID:               openrails.ProductID(p.ID),
		Object:           "product",
		Key:              p.Key,
		Name:             p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Active:           p.IsPurchasable(),
		Metadata:         map[string]string{},
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
		Prices:           priceObjects,
	}
}

func PaymentToAPI(p *models.Payment, refunds []*models.Payment) api.PaymentObject {
	var subID *openrails.SubscriptionID
	if p.SubscriptionID != nil {
		s := openrails.SubscriptionID(*p.SubscriptionID)
		subID = &s
	}
	var amountRefunded int64
	var refundObjects []api.PaymentObject
	for _, r := range refunds {
		if refundStatusCountsTowardAPIAmount(r.Status) {
			if r.Amount < 0 {
				amountRefunded += -r.Amount
			} else {
				amountRefunded += r.Amount
			}
		}
		refundObjects = append(refundObjects, PaymentToAPI(r, nil))
	}
	object := "charge"
	status := paymentAPIStatus(p.Status)
	captured := status == "succeeded" || status == "refunded" || status == "partially_refunded"
	if p.RefundedPaymentID != nil || p.Amount < 0 {
		object = "refund"
		captured = false
	}
	refunded := amountRefunded >= p.Amount && p.Amount > 0
	if object == "charge" && status != "failed" && refunded {
		status = "refunded"
	} else if object == "charge" && status != "failed" && amountRefunded > 0 {
		status = "partially_refunded"
	}
	payment := api.PaymentObject{ID: openrails.PaymentID(p.ID), Object: object, Status: status, Amount: p.Amount, AmountRefunded: amountRefunded, Currency: p.Currency, CustomerID: openrails.CustomerID(p.CustomerID), SubscriptionID: subID, Rail: string(p.Rail), TransactionID: p.TransactionID, Refunded: refunded, Captured: captured, FailureCode: p.FailureCode, FailureReason: p.FailureReason, CreatedAt: p.CreatedAt}
	if refunds != nil {
		if refundObjects == nil {
			refundObjects = []api.PaymentObject{}
		}
		payment.Refunds = &api.PaymentRefundsList{Object: "list", Data: refundObjects}
	}
	if p.Price != nil {
		priceObj := PriceToAPI(p.Price)
		payment.Price = &priceObj
	}
	return payment
}

type userPaymentObject struct {
	ID             openrails.PaymentID       `json:"id"`
	Object         string                    `json:"object"`
	Status         string                    `json:"status,omitempty"`
	Amount         int64                     `json:"amount,string"`
	AmountRefunded int64                     `json:"amount_refunded,string"`
	Currency       string                    `json:"currency"`
	CustomerID     openrails.CustomerID      `json:"customer_id"`
	SubscriptionID *openrails.SubscriptionID `json:"subscription_id,omitempty"`
	Rail           string                    `json:"rail"`
	Refunded       bool                      `json:"refunded"`
	Captured       bool                      `json:"captured,omitempty"`
	CreatedAt      time.Time                 `json:"created_at"`
	Price          *api.PriceObject          `json:"price,omitempty"`
	Card           *paymentCardJSON          `json:"card,omitempty"`
}

// paymentCardJSON is the card snapshot for a single payment (the card used for
// that charge), served from the DB. No Stripe fetch.
type paymentCardJSON struct {
	Brand string `json:"brand,omitempty"`
	Last4 string `json:"last4,omitempty"`
}

func paymentCardFromModel(p *models.Payment) *paymentCardJSON {
	brand, last4 := "", ""
	if p.CardBrand != nil {
		brand = *p.CardBrand
	}
	if p.CardLast4 != nil {
		last4 = *p.CardLast4
	}
	if brand == "" && last4 == "" {
		return nil
	}
	return &paymentCardJSON{Brand: brand, Last4: last4}
}

func PaymentToUserAPI(p *models.Payment) userPaymentObject {
	payment := PaymentToAPI(p, nil)
	return userPaymentObject{
		ID:             payment.ID,
		Object:         payment.Object,
		Status:         payment.Status,
		Amount:         payment.Amount,
		AmountRefunded: payment.AmountRefunded,
		Currency:       payment.Currency,
		CustomerID:     payment.CustomerID,
		SubscriptionID: payment.SubscriptionID,
		Rail:           payment.Rail,
		Refunded:       payment.Refunded,
		Captured:       payment.Captured,
		CreatedAt:      payment.CreatedAt,
		Price:          payment.Price,
		Card:           paymentCardFromModel(p),
	}
}

func refundStatusCountsTowardAPIAmount(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "completed":
		return true
	default:
		return false
	}
}

func paymentAPIStatus(status string) string {
	switch status {
	case "completed", "":
		return "succeeded"
	case "pending":
		return "pending"
	case "failed":
		return "failed"
	case "refunded":
		return "refunded"
	default:
		return status
	}
}

func PriceToAPI(p *models.Price) api.PriceObject {
	var recurring *api.RecurringInfo
	if ch := p.RecurringCycleHours(); ch != nil && *ch > 0 {
		recurring = &api.RecurringInfo{Interval: sharedformat.BillingCycleHoursToInterval(*ch)}
	}
	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}
	var providers []string
	if len(p.PSPLinks) > 0 {
		providers = make([]string, 0, len(p.PSPLinks))
		for name := range p.PSPLinks {
			providers = append(providers, name)
		}
		sort.Strings(providers)
	}
	return api.PriceObject{ID: openrails.PriceID(p.ID), Key: p.Key, Object: "price", UnitAmount: p.Amount, Currency: p.Currency, Type: priceType, Recurring: recurring, Product: openrails.ProductID(p.ProductID), Active: p.IsPurchasable(), Providers: providers, Metadata: map[string]string{}, CreatedAt: p.CreatedAt}
}
