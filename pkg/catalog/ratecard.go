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
	AggSum         = "sum"
	AggCount       = "count"
	AggMax         = "max"
	AggMin         = "min"
	AggUniqueCount = "unique_count"
	AggLatest      = "latest"
)

var validAggregations = map[string]struct{}{
	AggSum: {}, AggCount: {}, AggMax: {}, AggMin: {}, AggUniqueCount: {}, AggLatest: {},
}

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

var validRoundModes = map[string]struct{}{
	"": {}, RoundHalfUp: {}, RoundUp: {}, RoundDown: {},
}

// validateRatePrice normalizes and validates a charge-model price. `where` is a
// human label for error messages (e.g. `product "droplet" rate_card #1`).
func validateRatePrice(where string, rp *RatePrice) error {
	rp.Model = strings.ToLower(strings.TrimSpace(rp.Model))
	if rp.Currency != "" {
		rp.Currency = normalizeCurrency(rp.Currency)
		if !validPriceCurrency(rp.Currency) {
			return fmt.Errorf("%s: currency must be an ISO money currency", where)
		}
	}
	blocks := 0
	if rp.Flat != nil {
		blocks++
	}
	if rp.PerUnit != nil {
		blocks++
	}
	if rp.Tiered != nil {
		blocks++
	}
	if rp.Package != nil {
		blocks++
	}
	if blocks != 1 {
		return fmt.Errorf("%s: price model requires exactly one matching sub-block", where)
	}

	switch rp.Model {
	case ModelFlat:
		if rp.Flat == nil {
			return fmt.Errorf("%s: model flat requires flat block", where)
		}
		if rp.Flat.Amount <= 0 {
			return fmt.Errorf("%s: flat price requires a positive amount", where)
		}
	case ModelPerUnit:
		if rp.PerUnit == nil {
			return fmt.Errorf("%s: model per_unit requires per_unit block", where)
		}
		rp.PerUnit.Round = strings.ToLower(strings.TrimSpace(rp.PerUnit.Round))
		if _, ok := validRoundModes[rp.PerUnit.Round]; !ok {
			return fmt.Errorf("%s: round must be up, down or half_up, got %q", where, rp.PerUnit.Round)
		}
		if rp.PerUnit.MaximumAmount < 0 {
			return fmt.Errorf("%s: maximum_amount must be >= 0", where)
		}
		if rp.PerUnit.Matrix == nil && rp.PerUnit.UnitAmount < 0 {
			return fmt.Errorf("%s: per_unit unit_amount must be >= 0", where)
		}
		if rp.PerUnit.DivideBy < 0 {
			return fmt.Errorf("%s: divide_by must be >= 0", where)
		}
		if rp.PerUnit.Matrix != nil {
			if err := validateMatrix(where, rp.PerUnit.Matrix); err != nil {
				return err
			}
		}
	case ModelTiered:
		if rp.Tiered == nil {
			return fmt.Errorf("%s: model tiered requires tiered block", where)
		}
		rp.Tiered.Mode = strings.ToLower(strings.TrimSpace(rp.Tiered.Mode))
		if rp.Tiered.Mode != TierModeVolume && rp.Tiered.Mode != TierModeGraduated {
			return fmt.Errorf("%s: tiered mode must be %q or %q, got %q", where, TierModeVolume, TierModeGraduated, rp.Tiered.Mode)
		}
		if err := validateTiers(where, rp.Tiered.Tiers); err != nil {
			return err
		}
	case ModelPackage:
		if rp.Package == nil {
			return fmt.Errorf("%s: model package requires package block", where)
		}
		if rp.Package.PackageSize <= 0 {
			return fmt.Errorf("%s: package_size must be > 0", where)
		}
		if rp.Package.Amount <= 0 {
			return fmt.Errorf("%s: package amount must be > 0", where)
		}
		if rp.Package.FreeUnits < 0 {
			return fmt.Errorf("%s: free_units must be >= 0", where)
		}
	default:
		return fmt.Errorf("%s: model must be one of flat/per_unit/tiered/package, got %q", where, rp.Model)
	}
	return nil
}

func validateTiers(where string, tiers []RateTier) error {
	if len(tiers) == 0 {
		return fmt.Errorf("%s: tiered price requires tiers", where)
	}
	var prev int64
	for i := range tiers {
		t := &tiers[i]
		if t.UnitAmount < 0 || t.FlatAmount < 0 {
			return fmt.Errorf("%s: tier #%d amounts must be >= 0", where, i+1)
		}
		last := i == len(tiers)-1
		switch {
		case t.UpTo == nil && !last:
			return fmt.Errorf("%s: only the last tier may be unbounded (up_to omitted)", where)
		case t.UpTo != nil && last:
			return fmt.Errorf("%s: the last tier must be unbounded (omit up_to)", where)
		case t.UpTo != nil:
			if *t.UpTo <= prev {
				return fmt.Errorf("%s: tier up_to values must strictly ascend (got %d after %d)", where, *t.UpTo, prev)
			}
			prev = *t.UpTo
		}
	}
	return nil
}

func validateMatrix(where string, mx *Matrix) error {
	mx.Dimension = strings.TrimSpace(mx.Dimension)
	if mx.Dimension == "" {
		return fmt.Errorf("%s: matrix requires a dimension", where)
	}
	if len(mx.Cells) == 0 {
		return fmt.Errorf("%s: matrix requires at least one cell", where)
	}
	for key, cell := range mx.Cells {
		if cell.UnitAmount < 0 || cell.MaximumAmount < 0 || cell.Included < 0 {
			return fmt.Errorf("%s: matrix cell %q amounts must be >= 0", where, key)
		}
	}
	return nil
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
	if a.Included < 0 {
		return fmt.Errorf("%s: allowance.included must be >= 0", where)
	}
	a.AccrueFrom = normalizeSlug(a.AccrueFrom)
	if a.Cap != "" {
		if _, err := ParseDurationSpec(a.Cap); err != nil {
			return fmt.Errorf("%s: allowance.cap: %w", where, err)
		}
	}
	return nil
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
				if err := validateFilterKeys(where, rc.Filter, mt); err != nil {
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
	if len(mt.GroupBy) == 0 {
		return nil // legacy meter / no declared dimensions: nothing to cross-check
	}
	if _, ok := mt.GroupBy[mx.Dimension]; !ok {
		return fmt.Errorf("%s matrix dimension %q is not a group_by key of meter %q", where, mx.Dimension, mt.Key)
	}
	return nil
}

func validateFilterKeys(where string, filter map[string][]string, mt Meter) error {
	if len(mt.GroupBy) == 0 {
		return nil
	}
	for k := range filter {
		if _, ok := mt.GroupBy[k]; !ok {
			return fmt.Errorf("%s filter key %q is not a group_by key of meter %q", where, k, mt.Key)
		}
	}
	return nil
}
