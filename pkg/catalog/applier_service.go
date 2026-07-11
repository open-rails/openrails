package catalog

import (
	"context"
	"encoding/json"
	"fmt"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// ServiceApplier adapts an in-process *service.Service to the Applier
// interface. The facade method set already matches the interface one-to-one, so
// the *service.Service satisfies Applier directly — this constructor exists to
// make the in-process wiring explicit and to give a single place to evolve the
// adapter if the facade signatures ever diverge.
//
// NewServiceApplier returns the service as an Applier. A compile-time assertion
// below guarantees *service.Service implements every method.
func NewServiceApplier(svc *billingservice.Service) Applier {
	return serviceApplier{Service: svc}
}

type serviceApplier struct {
	*billingservice.Service
}

func (a serviceApplier) SyncCatalogSidecars(ctx context.Context, m *Manifest) error {
	if a.Service == nil || m == nil {
		return nil
	}
	req := billingservice.SyncCatalogSidecarsRequest{
		UsageLimits: make([]billingservice.CatalogUsageLimitSpec, 0, len(m.UsageLimits)),
		Meters:      make([]billingservice.CatalogMeterSpec, 0, len(m.Meters)),
	}
	for _, balance := range m.CreditBalances {
		expiresHours, err := creditBalanceExpiresHours(balance)
		if err != nil {
			return err
		}
		req.CreditBalances = append(req.CreditBalances, billingservice.CatalogCreditBalanceSpec{
			Key:          balance.Key,
			Unit:         balance.Unit,
			ExpiresHours: expiresHours,
		})
	}
	for _, limit := range m.UsageLimits {
		windows := make([]billingservice.CatalogUsageLimitWindowSpec, 0, len(limit.Windows))
		for _, window := range limit.Windows {
			windows = append(windows, billingservice.CatalogUsageLimitWindowSpec{
				Window: window.Window,
				Amount: window.Amount,
			})
		}
		req.UsageLimits = append(req.UsageLimits, billingservice.CatalogUsageLimitSpec{
			Key:     limit.Key,
			Measure: limit.Measure,
			Windows: windows,
		})
	}
	for _, meter := range m.Meters {
		req.Meters = append(req.Meters, billingservice.CatalogMeterSpec{
			Key:           meter.Key,
			Kind:          meter.Kind,
			EventType:     meter.EventType,
			ValueProperty: meter.ValueProperty,
			Aggregation:   meter.Aggregation,
			Unit:          meter.Unit,
			GroupBy:       meter.GroupBy,
		})
	}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			if len(product.UsageLimits) > 0 {
				req.ProductLimits = append(req.ProductLimits, billingservice.CatalogProductUsageLimitsSpec{
					ProductKey: product.Key,
					Keys:       append([]string(nil), product.UsageLimits...),
				})
			}
			if len(product.Includes) > 0 {
				req.ProductIncludes = append(req.ProductIncludes, billingservice.CatalogProductIncludesSpec{
					ProductKey:   product.Key,
					IncludedKeys: append([]string(nil), product.Includes...),
				})
			}
			for i, rc := range product.RateCards {
				priceJSON, err := json.Marshal(rc.Price)
				if err != nil {
					return fmt.Errorf("product %q rate_card #%d price: %w", product.Key, i+1, err)
				}
				var allowanceJSON json.RawMessage
				if rc.Allowance != nil {
					allowanceJSON, err = json.Marshal(rc.Allowance)
					if err != nil {
						return fmt.Errorf("product %q rate_card #%d allowance: %w", product.Key, i+1, err)
					}
				}
				req.RateCards = append(req.RateCards, billingservice.CatalogRateCardSpec{
					ProductKey:  product.Key,
					Ordinal:     i + 1,
					MeterKey:    rc.Meter,
					PaymentTerm: rc.PaymentTerm,
					Filter:      rc.Filter,
					Allowance:   allowanceJSON,
					Price:       priceJSON,
				})
			}
			if product.isCreditTopUp() {
				creditKey := product.Credits[0].Key
				for i, price := range product.Prices {
					rp := price.ratePrice()
					priceJSON, err := json.Marshal(rp)
					if err != nil {
						return fmt.Errorf("product %q top-up price #%d: %w", product.Key, i+1, err)
					}
					req.CreditPurchases = append(req.CreditPurchases, billingservice.CatalogCreditPurchasePriceSpec{
						ProductKey: product.Key,
						Ordinal:    i + 1,
						CreditKey:  creditKey,
						Currency:   price.Currency,
						PSPs:       append([]string(nil), price.PSPs...),
						InputMin:   price.InputMin,
						InputMax:   price.InputMax,
						Round:      price.Round,
						Price:      priceJSON,
					})
				}
			}
		}
	}
	return a.Service.SyncCatalogSidecars(ctx, req)
}

var _ Applier = serviceApplier{}

func creditBalanceExpiresHours(balance CreditBalance) (*int, error) {
	if balance.ExpiresDefault == "" {
		return nil, nil
	}
	hours, err := durationSpecHours(balance.ExpiresDefault)
	if err != nil {
		return nil, fmt.Errorf("credit balance %q expires_default: %w", balance.Key, err)
	}
	return &hours, nil
}

func durationSpecHours(spec string) (int, error) {
	d, err := ParseDurationSpec(spec)
	if err != nil {
		return 0, err
	}
	return int(d.Hours()), nil
}
