package catalog

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/pkg/pricing"
)

// This file defines the #638 rate-card catalog model (rate cards, dimensional
// matrix pricing, allowances) and the #639/#640 variable credit-purchase shape,
// plus their validation. The pure charge-model engine and the declarative price
// types live in the leaf package pkg/pricing, so internal/modules/money can share
// them without an import cycle through pkg/service. The aliases below keep the
// catalog API stable.
//
// Naming follows OpenMeter's catalog vocabulary (value_property, group_by,
// mode: volume|graduated, maximum_amount, payment_term) with Stripe's
// transform_quantity divisor (divide_by/round, which
// integer-micros needs and OpenMeter/Lago lack) and Orb's matrix for per-SKU
// pricing. See #638's Research Appendix.

type (
	RatePrice    = pricing.RatePrice
	FlatPrice    = pricing.FlatPrice
	PerUnitPrice = pricing.PerUnitPrice
	TieredPrice  = pricing.TieredPrice
	PackagePrice = pricing.PackagePrice
	RateTier     = pricing.RateTier
	Matrix       = pricing.Matrix
	MatrixCell   = pricing.MatrixCell
	Allowance    = pricing.Allowance
	ChargeModel  = pricing.ChargeModel
	ChargeTier   = pricing.ChargeTier
)

const (
	ModelFlat    = pricing.ModelFlat
	ModelPerUnit = pricing.ModelPerUnit
	ModelTiered  = pricing.ModelTiered
	ModelPackage = pricing.ModelPackage

	TierModeVolume    = pricing.TierModeVolume
	TierModeGraduated = pricing.TierModeGraduated

	RoundHalfUp = pricing.RoundHalfUp
	RoundUp     = pricing.RoundUp
	RoundDown   = pricing.RoundDown
)

// QuoteUnitsForSpend re-exports the leaf quoter for catalog API consumers.
var QuoteUnitsForSpend = pricing.QuoteUnitsForSpend

// Meter aggregations. Union of OpenMeter and Lago minus AVG and weighted_sum:
// counters use sum/count; point-in-time gauges use max/min/latest/unique_count;
// time-weighted "gauge" usage (GiB-months) is modeled as sum of host-emitted
// unit-seconds + a price-level divide_by (the OpenMeter heartbeat approach),
// so no native weighted_sum is needed.
const (
	AggSum         = pricing.AggregationSum
	AggCount       = pricing.AggregationCount
	AggMax         = pricing.AggregationMax
	AggMin         = pricing.AggregationMin
	AggUniqueCount = pricing.AggregationUniqueCount
	AggLatest      = pricing.AggregationLatest
)

// payment terms (OpenMeter PaymentTermType / Lago pay_in_advance).
const (
	PaymentInAdvance = "in_advance"
	PaymentInArrears = "in_arrears"
)

// RateCard binds a meter (or a flat fee) to a charge-model price within a product.
// It references the meter directly (Lago-style), not via an OpenMeter Feature.
type RateCard struct {
	// Meter is the metered usage stream this card rates. Empty == a flat fee
	// (the Price must be model: flat).
	Meter string `json:"meter,omitempty" yaml:"meter,omitempty"`
	// Filter pins this card to a subset of the meter's group_by dimensions
	// (Lago charge-filters / OpenMeter meterGroupByFilters). Optional.
	Filter map[string][]string `json:"filter,omitempty" yaml:"filter,omitempty"`
	// Allowance is included usage netted off before the price rates the overage.
	Allowance   *Allowance `json:"allowance,omitempty" yaml:"allowance,omitempty"`
	PaymentTerm string     `json:"payment_term,omitempty" yaml:"payment_term,omitempty"`
	Price       RatePrice  `json:"price" yaml:"price"`
}

// RateUsage rates `quantity` metered units against this rate card, selecting the
// matrix cell for dimValue when the price is a matrix (else the base price). This
// is the seam the runtime rater calls once it has loaded a rate card and the
// per-resource dimension value (e.g. the droplet's size_slug). dimValue is
// ignored for non-matrix prices. Allowance netting (flat `included`, and pooled
// accrued allowances) is applied UPSTREAM by the rating engine — see #638's
// allowance/divisor unit note — not here, so this stays a pure quantity->cost map.
func (rc RateCard) RateUsage(dimValue string, quantity int64) (int64, error) {
	if rc.Price.PerUnit != nil && rc.Price.PerUnit.Matrix != nil {
		cm, ok := rc.Price.ChargeModelForCell(dimValue)
		if !ok {
			return 0, fmt.Errorf("meter %q rate card has no matrix cell for %q=%q", rc.Meter, rc.Price.PerUnit.Matrix.Dimension, dimValue)
		}
		return cm.Rate(quantity)
	}
	return rc.Price.ToChargeModel().Rate(quantity)
}

// validateRatePrice normalizes and validates a charge-model price. `where` is a
// human label for error messages (e.g. `product "droplet" rate_card #1`).
func validateRatePrice(where string, rp *RatePrice) error {
	return pricing.ValidateRatePrice(where, rp)
}

// validateRateCard validates one rate card: a flat fee (no meter) or a metered
// usage price (meter required). Meters are validated to exist by the caller.
func validateRateCard(where string, rc *RateCard) error {
	rc.Meter = normalizeSlug(rc.Meter)
	rc.PaymentTerm = strings.ToLower(strings.TrimSpace(rc.PaymentTerm))
	switch rc.PaymentTerm {
	case "", PaymentInAdvance, PaymentInArrears:
	default:
		return fmt.Errorf("%s: payment_term must be %q or %q", where, PaymentInAdvance, PaymentInArrears)
	}
	if err := validateRatePrice(where, &rc.Price); err != nil {
		return err
	}
	if rc.Price.Model == ModelFlat {
		if rc.Meter != "" {
			return fmt.Errorf("%s: a flat rate card must not reference a meter", where)
		}
		if rc.Allowance != nil {
			return fmt.Errorf("%s: a flat rate card cannot have an allowance", where)
		}
		return nil
	}
	// usage price
	if rc.Meter == "" {
		return fmt.Errorf("%s: a %s rate card requires a meter", where, rc.Price.Model)
	}
	if rc.Allowance != nil {
		if err := validateAllowance(where, rc.Allowance); err != nil {
			return err
		}
	}
	return nil
}

func validateAllowance(where string, a *Allowance) error {
	return pricing.ValidateAllowance(where, a)
}

// validateRateCardModel validates the #638 rate-card and #639 credit-purchase
// additions on every product. Called from Manifest.validate after meters and
// products are normalized.
func (m *Manifest) validateRateCardModel() error {
	meters := map[string]Meter{}
	for _, mt := range m.Meters {
		meters[mt.Key] = mt
	}
	// One meter backs at most one usage rate card (mirrors the legacy metered
	// rule); a matrix on that card prices many SKUs off the one meter.
	usedMeters := map[string]string{}

	for gi := range m.TierGroups {
		for pi := range m.TierGroups[gi].Products {
			p := &m.TierGroups[gi].Products[pi]

			for ci := range p.RateCards {
				rc := &p.RateCards[ci]
				where := fmt.Sprintf("product %q rate_card #%d", p.Key, ci+1)
				if err := validateRateCard(where, rc); err != nil {
					return err
				}
				if rc.Price.Model == ModelFlat {
					continue
				}
				mt, ok := meters[rc.Meter]
				if !ok {
					return fmt.Errorf("%s references unknown meter %q", where, rc.Meter)
				}
				if prev, dup := usedMeters[rc.Meter]; dup {
					return fmt.Errorf("%s: meter %q is already rated by %s; one meter per usage rate card", where, rc.Meter, prev)
				}
				usedMeters[rc.Meter] = where
				if rc.Price.PerUnit != nil && rc.Price.PerUnit.Matrix != nil {
					if err := validateMatrixDimension(where, rc.Price.PerUnit.Matrix, mt); err != nil {
						return err
					}
				}
				if err := validateFilterKeys(where, &rc.Filter, mt); err != nil {
					return err
				}
				if rc.Allowance != nil && rc.Allowance.AccrueFrom != "" {
					if _, ok := meters[rc.Allowance.AccrueFrom]; !ok {
						return fmt.Errorf("%s allowance.accrue_from references unknown meter %q", where, rc.Allowance.AccrueFrom)
					}
				}
			}
			// Usage products need no declared cadence: the cap/allowance window is
			// the invoice period (calendar-month via the merchant boundary), #642.
		}
	}
	return nil
}

func validateMatrixDimension(where string, mx *Matrix, mt Meter) error {
	return pricing.ValidateDimensions(where, mt.GroupBy, nil, &RatePrice{
		Model: ModelPerUnit,
		PerUnit: &PerUnitPrice{
			Matrix: mx,
		},
	})
}

func validateFilterKeys(where string, filter *map[string][]string, mt Meter) error {
	if err := pricing.ValidateFilter(where, filter); err != nil {
		return err
	}
	return pricing.ValidateDimensions(where, mt.GroupBy, *filter, nil)
}
