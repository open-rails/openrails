package handlers

import (
	"github.com/open-rails/openrails/internal/db/models"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
)

func ProductToAPI(p *models.Product, prices []*models.Price) api.ProductObject {
	priceObjects := make([]api.PriceObject, len(prices))
	for i, price := range prices {
		priceObjects[i] = PriceToAPI(price)
	}
	return api.ProductObject{ID: api.FormatProductID(p.ID), Object: "product", Name: p.DisplayName, Description: p.Description, Active: p.IsActive, Livemode: false, Metadata: map[string]string{}, Created: api.ToUnix(p.CreatedAt), Updated: api.ToUnix(p.UpdatedAt), Prices: priceObjects}
}

func PaymentToAPI(p *models.Payment, refunds []*models.Payment) api.PaymentObject {
	var subID *string
	if p.SubscriptionID != nil {
		s := api.FormatSubscriptionID(*p.SubscriptionID)
		subID = &s
	}
	var amountRefunded int64
	var refundObjects []api.PaymentObject
	for _, r := range refunds {
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
	payment := api.PaymentObject{ID: api.FormatPaymentID(p.ID), Object: "charge", Status: status, Amount: p.Amount, AmountRefunded: amountRefunded, Currency: p.Currency, User: api.FormatUserID(p.UserID), Subscription: subID, Processor: string(p.Processor), TransactionID: p.TransactionID, Refunded: refunded, Captured: true, Created: api.ToUnix(p.CreatedAt)}
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

func PriceToAPI(p *models.Price) api.PriceObject {
	var recurring *api.RecurringInfo
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, intervalCount := sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays)
		recurring = &api.RecurringInfo{Interval: interval, IntervalCount: intervalCount}
	}
	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}
	return api.PriceObject{ID: api.FormatPriceID(p.ID), Object: "price", Name: p.DisplayName, Amount: p.Amount, Currency: p.Currency, Type: priceType, Recurring: recurring, Product: api.FormatProductID(p.ProductID), Active: p.IsActive, Livemode: false, Metadata: map[string]string{}, Created: api.ToUnix(p.CreatedAt)}
}
