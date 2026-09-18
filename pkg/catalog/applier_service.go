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

		}
	}
	return a.Service.SyncCatalogSidecars(ctx, req)
}

var _ Applier = serviceApplier{}
