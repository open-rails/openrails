package handlers

import (
	"sort"
	"strings"

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
		ID:               api.FormatProductID(p.ID),
		Object:           "product",
		Slug:             p.Slug,
		Name:             p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		CreditsSpec:      creditGrantSpecsToAPI(p.CreditsSpec),
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Active:           p.IsPurchasable(),
		Livemode:         false,
		Metadata:         map[string]string{},
		Created:          api.ToUnix(p.CreatedAt),
		Updated:          api.ToUnix(p.UpdatedAt),
		Prices:           priceObjects,
	}
}

func creditGrantSpecsToAPI(specs models.CreditsSpec) map[string]api.CreditGrantSpecObject {
	if len(specs) == 0 {
		return nil
	}
	out := make(map[string]api.CreditGrantSpecObject, len(specs))
	for creditType, spec := range specs {
		out[creditType] = api.CreditGrantSpecObject{
			Amount:      spec.Amount,
			ExpiresDays: spec.ExpiresDays,
			Cadence:     string(spec.Cadence),
		}
	}
	return out
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
	payment := api.PaymentObject{ID: api.FormatPaymentID(p.ID), Object: object, Status: status, Amount: p.Amount, AmountRefunded: amountRefunded, Currency: p.Currency, User: api.FormatUserID(p.MerchantSubjectID.String()), Subscription: subID, Processor: string(p.Processor), TransactionID: p.TransactionID, Refunded: refunded, Captured: captured, Created: api.ToUnix(p.CreatedAt)}
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
	ID             string           `json:"id"`
	Object         string           `json:"object"`
	Status         string           `json:"status,omitempty"`
	Amount         int64            `json:"amount"`
	AmountRefunded int64            `json:"amount_refunded"`
	Currency       string           `json:"currency"`
	User           string           `json:"user"`
	Subscription   *string          `json:"subscription,omitempty"`
	Processor      string           `json:"processor"`
	Refunded       bool             `json:"refunded"`
	Captured       bool             `json:"captured,omitempty"`
	Created        int64            `json:"created"`
	Price          *api.PriceObject `json:"price,omitempty"`
	Card           *paymentCardJSON `json:"card,omitempty"`
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
		User:           payment.User,
		Subscription:   payment.Subscription,
		Processor:      payment.Processor,
		Refunded:       payment.Refunded,
		Captured:       payment.Captured,
		Created:        payment.Created,
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
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, intervalCount := sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays)
		recurring = &api.RecurringInfo{Interval: interval, IntervalCount: intervalCount}
	}
	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}
	var providers []string
	if len(p.Processors) > 0 {
		providers = make([]string, 0, len(p.Processors))
		for name := range p.Processors {
			providers = append(providers, name)
		}
		sort.Strings(providers)
	}
	return api.PriceObject{ID: api.FormatPriceID(p.ID), Object: "price", UnitAmount: p.Amount, Currency: p.Currency, Type: priceType, Recurring: recurring, Product: api.FormatProductID(p.ProductID), Active: p.IsPurchasable(), Livemode: false, Providers: providers, Metadata: map[string]string{}, Created: api.ToUnix(p.CreatedAt)}
}
