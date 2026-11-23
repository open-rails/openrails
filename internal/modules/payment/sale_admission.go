package payment

import (
	"encoding/json"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func (s *FulfillmentService) transactionBound(d *db.DB) *FulfillmentService {
	out := NewFulfillmentService(s.Log, catalog.NewPriceService(d), catalog.NewProductService(d), payments.NewPaymentService(d, s.clock), entitlements.NewEntitlementService(d, s.clock), nil, s.clock)
	out.transactionDB = d
	if s.SubscriptionService != nil {
		out.SubscriptionService = subscriptions.NewSubscriptionService(d, out.PriceService, out.ProductService, nil, s.clock)
	}
	return out
}

func saleReplayParams(in gen.BillingProviderIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: payments.TypeNMISale, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}
