package catalog

import (
	"context"
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
		req.Meters = append(req.Meters, billingservice.CatalogMeterSpec{Key: meter.Key, Kind: meter.Kind})
	}
	for _, group := range m.TierGroups {
		for _, product := range group.Products {
			if len(product.UsageLimits) > 0 {
				req.ProductLimits = append(req.ProductLimits, billingservice.CatalogProductUsageLimitsSpec{
					ProductSlug: product.Key,
					Keys:        append([]string(nil), product.UsageLimits...),
				})
			}
			if len(product.Includes) > 0 {
				req.ProductIncludes = append(req.ProductIncludes, billingservice.CatalogProductIncludesSpec{
					ProductSlug:   product.Key,
					IncludedSlugs: append([]string(nil), product.Includes...),
				})
			}
			for _, price := range product.Prices {
				if price.Metered == nil {
					continue
				}
				_, cycleDays, err := normalizeInterval(price.Interval)
				if err != nil {
					return fmt.Errorf("product %q price %s interval: %w", product.Key, PriceLabel(product.Key, price), err)
				}
				var cycle *int
				if cycleDays > 0 {
					cycle = &cycleDays
				}
				var perSeconds *int64
				if price.Metered.Per != "" {
					d, err := ParseDurationSpec(price.Metered.Per)
					if err != nil {
						return fmt.Errorf("product %q price %s metered per: %w", product.Key, PriceLabel(product.Key, price), err)
					}
					seconds := int64(d.Seconds())
					perSeconds = &seconds
				}
				req.MeteredPrices = append(req.MeteredPrices, billingservice.CatalogMeteredPriceSpec{
					ProductSlug:      product.Key,
					UnitAmount:       price.UnitAmount,
					Currency:         price.Currency,
					BillingCycleDays: cycle,
					MeterKey:         price.Metered.Meter,
					RateMicros:       price.Metered.Rate,
					PerUnits:         price.Metered.PerUnits,
					PerSeconds:       perSeconds,
				})
			}
		}
	}
	return a.Service.SyncCatalogSidecars(ctx, req)
}

var _ Applier = serviceApplier{}
