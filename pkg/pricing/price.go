package pricing

// RatePrice and friends are the declarative (YAML/JSON) form of a charge model,
// shared by the catalog manifest/loader (pkg/catalog) and the runtime
// rater/quoter (internal/modules/money). They normalize into ChargeModel, the
// pure evaluator in this package. This package is a leaf (no other openrails
// deps) so both sides import it without an import cycle.

// Allowance models included units before overage. The simple form is a flat
// `included` amount per period. The accrual form draws each active resource's
// matrix-cell `included` from the `accrue_from` meter, bounded by `cap`.
type Allowance struct {
	Included   int64  `json:"included,omitempty" yaml:"included,omitempty"`
	AccrueFrom string `json:"accrue_from,omitempty" yaml:"accrue_from,omitempty"`
	Cap        string `json:"cap,omitempty" yaml:"cap,omitempty"`
}

// RatePrice is the YAML/JSON form of a charge model. Model selects exactly one
// typed block; ToChargeModel normalizes it into the evaluator shape.
type RatePrice struct {
	Model    string `json:"model" yaml:"model"`
	Currency string `json:"currency,omitempty" yaml:"currency,omitempty"`

	Flat    *FlatPrice    `json:"flat,omitempty" yaml:"flat,omitempty"`
	PerUnit *PerUnitPrice `json:"per_unit,omitempty" yaml:"per_unit,omitempty"`
	Tiered  *TieredPrice  `json:"tiered,omitempty" yaml:"tiered,omitempty"`
	Package *PackagePrice `json:"package,omitempty" yaml:"package,omitempty"`
}

type FlatPrice struct {
	Amount int64 `json:"amount,omitempty" yaml:"amount,omitempty"`
}

type PerUnitPrice struct {
	UnitAmount int64  `json:"unit_amount,omitempty" yaml:"unit_amount,omitempty"`
	DivideBy   int64  `json:"divide_by,omitempty" yaml:"divide_by,omitempty"`
	Round      string `json:"round,omitempty" yaml:"round,omitempty"`

	// maximum_amount — per-period cap on the computed cost (0 = uncapped). A real
	// per-SKU pricing fact: DO caps a resource at its advertised monthly price.
	MaximumAmount int64 `json:"maximum_amount,omitempty" yaml:"maximum_amount,omitempty"`

	// matrix — unit_amount (and optional per-cell cap/included) vary by
	// one meter group_by dimension (Orb matrix). Only valid with model: per_unit.
	Matrix *Matrix `json:"matrix,omitempty" yaml:"matrix,omitempty"`
}

type TieredPrice struct {
	Mode  string     `json:"mode,omitempty" yaml:"mode,omitempty"`
	Tiers []RateTier `json:"tiers,omitempty" yaml:"tiers,omitempty"`
}

type PackagePrice struct {
	Amount      int64 `json:"amount,omitempty" yaml:"amount,omitempty"`
	PackageSize int64 `json:"package_size,omitempty" yaml:"package_size,omitempty"`
	FreeUnits   int64 `json:"free_units,omitempty" yaml:"free_units,omitempty"`
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
	MaximumAmount int64 `json:"maximum_amount,omitempty" yaml:"maximum_amount,omitempty"`
	Included      int64 `json:"included,omitempty" yaml:"included,omitempty"` // per-cycle included units other cards' allowances accrue from
}

// ToChargeModel normalizes a RatePrice into a ChargeModel. The shared Amount
// field maps to FlatAmount (flat) or PackageAmount (package); only the field
// relevant to Model is read by Rate. For matrix prices use ChargeModelForCell.
func (rp RatePrice) ToChargeModel() ChargeModel {
	cm := ChargeModel{
		Kind: rp.Model,
	}
	if rp.Flat != nil {
		cm.FlatAmount = rp.Flat.Amount
	}
	if rp.PerUnit != nil {
		cm.UnitAmount = rp.PerUnit.UnitAmount
		cm.DivideBy = rp.PerUnit.DivideBy
		cm.Round = rp.PerUnit.Round
		cm.MaximumAmount = rp.PerUnit.MaximumAmount
	}
	if rp.Tiered != nil {
		cm.Mode = rp.Tiered.Mode
		for _, t := range rp.Tiered.Tiers {
			cm.Tiers = append(cm.Tiers, ChargeTier{UpTo: t.UpTo, UnitAmount: t.UnitAmount, FlatAmount: t.FlatAmount})
		}
	}
	if rp.Package != nil {
		cm.PackageSize = rp.Package.PackageSize
		cm.PackageAmount = rp.Package.Amount
		cm.FreeUnits = rp.Package.FreeUnits
	}
	return cm
}

// ChargeModelForCell builds the per_unit ChargeModel for one matrix dimension
// value: the cell's unit_amount with the price's divisor/rounding, and the
// per-cell cap (falling back to the price-level cap). Returns false if the
// price is not a matrix or the dimension value has no cell.
func (rp RatePrice) ChargeModelForCell(dimValue string) (ChargeModel, bool) {
	if rp.PerUnit == nil || rp.PerUnit.Matrix == nil {
		return ChargeModel{}, false
	}
	cell, ok := rp.PerUnit.Matrix.Cells[dimValue]
	if !ok {
		return ChargeModel{}, false
	}
	maxAmt := cell.MaximumAmount
	if maxAmt == 0 {
		maxAmt = rp.PerUnit.MaximumAmount
	}
	return ChargeModel{
		Kind:          ModelPerUnit,
		UnitAmount:    cell.UnitAmount,
		DivideBy:      rp.PerUnit.DivideBy,
		Round:         rp.PerUnit.Round,
		MaximumAmount: maxAmt,
	}, true
}
