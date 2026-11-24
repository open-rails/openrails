package service

import (
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Without the event store a catalog declares no meter, limit, product limit
// or rate card, and settings keep nothing only usage reads: spend windows,
// failed-usage grace and cutoffs, the accrual-rate cap. Clearing them is
// allowed; what is stored stays dormant.

// refuseUsageCatalog refuses a document declaring what needs the event store.
func (s *Service) refuseUsageCatalog(app catalogwire.Application) error {
	if s.rt.EventStore != nil {
		return nil
	}
	var paths []string
	for _, key := range slices.Sorted(maps.Keys(app.Meters)) {
		paths = append(paths, "meters."+key)
	}
	for _, key := range slices.Sorted(maps.Keys(app.Limits)) {
		paths = append(paths, "limits."+key)
	}
	for _, key := range slices.Sorted(maps.Keys(app.Products)) {
		p := app.Products[key]
		if p.Limits.Set && len(p.Limits.Value) > 0 {
			paths = append(paths, "products."+key+".limits")
		}
		if p.RateCards.Set && len(p.RateCards.Value) > 0 {
			paths = append(paths, "products."+key+".rate_cards")
		}
	}
	return s.eventStoreRefusal("meters, limits and rate cards", paths)
}

// refuseUsageSettings refuses settings only usage reads.
func (s *Service) refuseUsageSettings(settings *billing.MerchantSettings) error {
	if s.rt.EventStore != nil || settings == nil {
		return nil
	}
	var paths []string
	if len(settings.DelegatedInvokerWastedSpendLimits) > 0 {
		paths = append(paths, "delegated_invoker_wasted_spend_limits")
	}
	for i, p := range settings.BillingPolicies {
		at := "billing_policies[" + strconv.Itoa(i) + "]."
		if len(p.SpendWindows) > 0 {
			paths = append(paths, at+"spend_windows")
		}
		if len(p.BadSpendWindows) > 0 {
			paths = append(paths, at+"bad_spend_windows")
		}
		if p.AccrualRateCapPerHour != 0 || p.Kind == "accrual_rate_cap" {
			paths = append(paths, at+"accrual_rate_cap_per_hour")
		}
	}
	return s.eventStoreRefusal("spend windows, failed-usage windows and the accrual-rate cap", paths)
}

func (s *Service) eventStoreRefusal(what string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	var refusal *apperr.Error
	if err := s.rt.RequireEventStore(what); !errors.As(err, &refusal) {
		return err
	}
	return refusal.WithParam(paths[0]).WithMetadata(map[string]any{"paths": paths})
}
