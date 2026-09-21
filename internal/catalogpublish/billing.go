package catalogpublish

import (
	"encoding/json"
	"fmt"

	billingservice "github.com/open-rails/openrails/internal/service"
)

func catalogBilling(m *Manifest) (billingservice.SyncCatalogSidecarsRequest, error) {
	req := billingservice.SyncCatalogSidecarsRequest{}
	for _, meter := range m.Meters {
		req.Meters = append(req.Meters, billingservice.CatalogMeterSpec{
			Key:           meter.Key,
			EventType:     meter.EventType,
			ValueProperty: meter.ValueProperty,
			Aggregation:   meter.Aggregation,
			Unit:          meter.Unit,
			GroupBy:       meter.GroupBy,
		})
	}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			for i, rc := range product.RateCards {
				priceJSON, err := json.Marshal(rc.Price)
				if err != nil {
					return req, fmt.Errorf("product %q rate_card #%d price: %w", product.Key, i+1, err)
				}
				var allowanceJSON json.RawMessage
				if rc.Allowance != nil {
					allowanceJSON, err = json.Marshal(rc.Allowance)
					if err != nil {
						return req, fmt.Errorf("product %q rate_card #%d allowance: %w", product.Key, i+1, err)
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

		}
	}
	return req, nil
}

var _ applier = (*billingservice.Service)(nil)
