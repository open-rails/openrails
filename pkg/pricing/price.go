package pricing

// RatePrice and friends are the declarative (YAML/JSON) form of a charge model,
// shared by the catalog manifest/loader (pkg/catalog) and the runtime
// rater/quoter (internal/modules/money). They normalize into ChargeModel, the
// pure evaluator in this package. This package is a leaf (no other openrails
// deps) so both sides import it without an import cycle.

// Allowance models included units before overage. The simple form is a flat
// `included` amount per period. The DO-bandwidth form accrues per active resource
// of `accrue_from` (using each resource's `included_per_cycle` attribute) and
// pools across `pool`.
type Allowance struct {
	Included         int64  `json:"included,omitempty" yaml:"included,omitempty"`
	Unit             string `json:"unit,omitempty" yaml:"unit,omitempty"`
	Pool             string `json:"pool,omitempty" yaml:"pool,omitempty"`
	AccrueFrom       string `json:"accrue_from,omitempty" yaml:"accrue_from,omitempty"`
	IncludedPerCycle string `json:"included_per_cycle,omitempty" yaml:"included_per_cycle,omitempty"`
	Cap              string `json:"cap,omitempty" yaml:"cap,omitempty"`
}

// RatePrice is the YAML/JSON form of a charge model. ToChargeModel normalizes it
// into the ChargeModel that Rate/QuoteUnitsForSpend evaluate.
type RatePrice struct {
	Model    string `json:"model" yaml:"model"`
	Currency string `json:"currency,omitempty" yaml:"currency,omitempty"`

	// flat / package headline price.
	Amount    int64    `json:"amount,omitempty" yaml:"amount,omitempty"`
	Per       string   `json:"per,omitempty" yaml:"per,omitempty"`
	AutoRenew bool     `json:"auto_renew,omitempty" yaml:"auto_renew,omitempty"`
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`

	// per_unit: cost = round(quantity * unit_amount / divide_by).
	UnitAmount int64  `json:"unit_amount,omitempty" yaml:"unit_amount,omitempty"`
	DivideBy   int64  `json:"divide_by,omitempty" yaml:"divide_by,omitempty"`
	Round      string `json:"round,omitempty" yaml:"round,omitempty"`

	// tiered.
	Mode  string     `json:"mode,omitempty" yaml:"mode,omitempty"`
	Tiers []RateTier `json:"tiers,omitempty" yaml:"tiers,omitempty"`

	// package.
	PackageSize int64 `json:"package_size,omitempty" yaml:"package_size,omitempty"`
	FreeUnits   int64 `json:"free_units,omitempty" yaml:"free_units,omitempty"`

	// dynamic: micros-per-micro markup over a reported cost (1_000_000 == 1.0x).
	Multiplier int64 `json:"multiplier,omitempty" yaml:"multiplier,omitempty"`

	// commitments — period floor/cap on the computed cost.
	MinimumAmount int64 `json:"minimum_amount,omitempty" yaml:"minimum_amount,omitempty"`
	MaximumAmount int64 `json:"maximum_amount,omitempty" yaml:"maximum_amount,omitempty"`

	// matrix — unit_amount (and optional per-cell commitments/included) vary by
	// one meter group_by dimension (Orb matrix). Only valid with model: per_unit.
	Matrix *Matrix `json:"matrix,omitempty" yaml:"matrix,omitempty"`
}

type RateTier struct {
	UpTo       *int64 `json:"up_to" yaml:"up_to"` // inclusive ceiling; nil == unbounded (last)
	UnitAmount int64  `json:"unit_amount,omitempty" yaml:"unit_amount,omitempty"`
	FlatAmount int64  `json:"flat_amount,omitempty" yaml:"flat_amount,omitempty"`
}

type Matrix struct {
	Dimension string                `json:"dimension" yaml:"dimension"`
	Cells     map[string]MatrixCell `json:"cells" yaml:"cells"`
}

type MatrixCell struct {
	UnitAmount    int64 `json:"unit_amount" yaml:"unit_amount"`
	MinimumAmount int64 `json:"minimum_amount,omitempty" yaml:"minimum_amount,omitempty"`
	MaximumAmount int64 `json:"maximum_amount,omitempty" yaml:"maximum_amount,omitempty"`
	Included      int64 `json:"included,omitempty" yaml:"included,omitempty"` // feeds allowance.included_per_cycle
}

// ToChargeModel normalizes a RatePrice into a ChargeModel. The shared Amount
// field maps to FlatAmount (flat) or PackageAmount (package); only the field
// relevant to Model is read by Rate. For matrix prices use ChargeModelForCell.
func (rp RatePrice) ToChargeModel() ChargeModel {
	cm := ChargeModel{
		Kind:          rp.Model,
		UnitAmount:    rp.UnitAmount,
		DivideBy:      rp.DivideBy,
		Round:         rp.Round,
		FlatAmount:    rp.Amount,
		Mode:          rp.Mode,
		PackageSize:   rp.PackageSize,
		PackageAmount: rp.Amount,
		FreeUnits:     rp.FreeUnits,
		Multiplier:    rp.Multiplier,
		MinimumAmount: rp.MinimumAmount,
		MaximumAmount: rp.MaximumAmount,
	}
	for _, t := range rp.Tiers {
		cm.Tiers = append(cm.Tiers, ChargeTier{UpTo: t.UpTo, UnitAmount: t.UnitAmount, FlatAmount: t.FlatAmount})
	}
	return cm
}

// ChargeModelForCell builds the per_unit ChargeModel for one matrix dimension
// value: the cell's unit_amount with the price's divisor/rounding, and per-cell
// commitments (falling back to the price-level commitments). Returns false if the
// price is not a matrix or the dimension value has no cell.
func (rp RatePrice) ChargeModelForCell(dimValue string) (ChargeModel, bool) {
	if rp.Matrix == nil {
		return ChargeModel{}, false
	}
	cell, ok := rp.Matrix.Cells[dimValue]
	if !ok {
		return ChargeModel{}, false
	}
	minAmt := cell.MinimumAmount
	if minAmt == 0 {
		minAmt = rp.MinimumAmount
	}
	maxAmt := cell.MaximumAmount
	if maxAmt == 0 {
		maxAmt = rp.MaximumAmount
	}
	return ChargeModel{
		Kind:          ModelPerUnit,
		UnitAmount:    cell.UnitAmount,
		DivideBy:      rp.DivideBy,
		Round:         rp.Round,
		MinimumAmount: minAmt,
		MaximumAmount: maxAmt,
	}, true
}
