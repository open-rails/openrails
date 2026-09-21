package catalogpublish

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/catalog"
)

type (
	Manifest         = catalog.Manifest
	Product          = catalog.Product
	Price            = catalog.Price
	PriceTrial       = catalog.PriceTrial
	TierGroup        = catalog.TierGroup
	ApplyPlan        = openrails.CatalogPlan
	GroupPlan        = openrails.CatalogGroupPlan
	ProductPlan      = openrails.CatalogProductPlan
	PricePlan        = openrails.CatalogPricePlan
	ApplyResult      = openrails.CatalogApplyResult
	PendingActionFor = openrails.CatalogPendingAction
)

const (
	ProductCreate    = openrails.CatalogProductCreate
	ProductUpdate    = openrails.CatalogProductUpdate
	ProductUnchanged = openrails.CatalogProductUnchanged
	ProductArchive   = openrails.CatalogProductArchive
	PriceCreate      = openrails.CatalogPriceCreate
	PriceActivate    = openrails.CatalogPriceActivate
	PriceArchive     = openrails.CatalogPriceArchive
	PriceUnchanged   = openrails.CatalogPriceUnchanged
)

type PlanOptions struct{ ArchiveMissingProducts, ArchiveMissingPrices bool }
type ApplyOptions struct{ Insert, Overwrite, Prune bool }

func declaredTierRank(p Product) int {
	if p.TierRank == nil {
		return 0
	}
	return *p.TierRank
}

// Convert the declaration duration through the same pure duration parser.
func normalizeDuration(value string) (*int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "indefinite" {
		return nil, nil
	}
	d, err := catalog.ParseDurationSpec(value)
	if err != nil {
		return nil, err
	}
	if d < time.Hour || d%time.Hour != 0 {
		return nil, fmt.Errorf("duration %q must be whole hours or indefinite", value)
	}
	hours := int(d / time.Hour)
	return &hours, nil
}
