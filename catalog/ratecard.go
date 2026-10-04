package catalog

// Model selects which price block of a RatePrice applies.
type Model string

const (
	// ModelFlat is a fixed fee per period, independent of usage.
	ModelFlat Model = "flat"
	// ModelPerUnit is unit_amount per unit, divided by divide_by and rounded
	// once.
	ModelPerUnit Model = "per_unit"
	// ModelTiered prices usage in bands.
	ModelTiered Model = "tiered"
	// ModelPackage charges per started package of units.
	ModelPackage Model = "package"
)

// TierMode is how a tiered price applies its bands.
type TierMode string

const (
	// TierModeVolume prices the whole quantity at the band it reaches.
	TierModeVolume TierMode = "volume"
	// TierModeGraduated prices each band's slice at that band's rate.
	TierModeGraduated TierMode = "graduated"
)

// Round is how a per-unit price rounds the divided amount.
type Round string

const (
	// RoundHalfUp is the money default.
	RoundHalfUp Round = "half_up"
	RoundUp     Round = "up"
	RoundDown   Round = "down"
)

// PaymentTerm is when a rate card's charge is collected.
type PaymentTerm string

const (
	PaymentInAdvance PaymentTerm = "in_advance"
	PaymentInArrears PaymentTerm = "in_arrears"
)

// Aggregation is how a meter turns its events into one quantity per period.
// Only sum and count can be billed.
type Aggregation string

const (
	AggregationSum         Aggregation = "sum"
	AggregationCount       Aggregation = "count"
	AggregationMax         Aggregation = "max"
	AggregationMin         Aggregation = "min"
	AggregationUniqueCount Aggregation = "unique_count"
	AggregationLatest      Aggregation = "latest"
)

// RatePrice is a charge model: Model names the one block that is set.
// Amounts are micros of Currency.
type RatePrice struct {
	Model    Model  `json:"model"`
	Currency string `json:"currency,omitempty"`

	Flat    *FlatPrice    `json:"flat,omitempty"`
	PerUnit *PerUnitPrice `json:"per_unit,omitempty"`
	Tiered  *TieredPrice  `json:"tiered,omitempty"`
	Package *PackagePrice `json:"package,omitempty"`
}

// FlatPrice is a fixed fee.
type FlatPrice struct {
	Amount int64 `json:"amount,omitempty,string"`
}

// PerUnitPrice costs round(quantity × unit_amount ÷ divide_by), capped at
// maximum_amount per period. A matrix varies unit_amount by one meter
// dimension.
type PerUnitPrice struct {
	UnitAmount    int64   `json:"unit_amount,omitempty,string"`
	DivideBy      int64   `json:"divide_by,omitempty"`
	Round         Round   `json:"round,omitempty"`
	MaximumAmount int64   `json:"maximum_amount,omitempty,string"`
	Matrix        *Matrix `json:"matrix,omitempty"`
}

// TieredPrice prices usage in ascending bands; the last band is unbounded.
type TieredPrice struct {
	Mode  TierMode   `json:"mode,omitempty"`
	Tiers []RateTier `json:"tiers,omitempty"`
}

// PackagePrice charges Amount per started PackageSize units after FreeUnits.
type PackagePrice struct {
	Amount      int64 `json:"amount,omitempty,string"`
	PackageSize int64 `json:"package_size,omitempty"`
	FreeUnits   int64 `json:"free_units,omitempty"`
}

// RateTier is one band. UpTo is its inclusive ceiling; nil marks the last.
type RateTier struct {
	UpTo       *int64 `json:"up_to"`
	UnitAmount int64  `json:"unit_amount,omitempty,string"`
	FlatAmount int64  `json:"flat_amount,omitempty,string"`
}

// Matrix prices one meter dimension's values separately.
type Matrix struct {
	Dimension string                `json:"dimension"`
	Cells     map[string]MatrixCell `json:"cells"`
}

// MatrixCell is one dimension value's per-unit amount, its cap, and the units
// it includes for other cards' accrued allowances.
type MatrixCell struct {
	UnitAmount    int64 `json:"unit_amount,string"`
	MaximumAmount int64 `json:"maximum_amount,omitempty,string"`
	Included      int64 `json:"included,omitempty"`
}

// Allowance is usage included before the price applies: Included units per
// period, or units accrued from another meter's matrix cells (AccrueFrom),
// bounded by Cap ("28d", "720h").
type Allowance struct {
	Included   int64  `json:"included,omitempty"`
	AccrueFrom string `json:"accrue_from,omitempty"`
	Cap        string `json:"cap,omitempty"`
}

// RateCard binds a meter's usage (or, with no meter, a flat fee) to a price
// within a product.
type RateCard struct {
	// Ordinal is the card's stable position; zero uses its declaration order.
	Ordinal int `json:"ordinal,omitempty"`
	// Meter is the usage it rates; empty for a flat fee.
	Meter string `json:"meter,omitempty"`
	// Filter limits the card to some values of the meter's dimensions.
	Filter      map[string][]string `json:"filter,omitempty"`
	Allowance   *Allowance          `json:"allowance,omitempty"`
	PaymentTerm PaymentTerm         `json:"payment_term,omitempty"`
	Price       RatePrice           `json:"price"`
}
