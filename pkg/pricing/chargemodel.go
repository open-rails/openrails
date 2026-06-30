package pricing

import (
	"fmt"
	"math/big"
)

// This file is the shared pricing engine behind both #638 (metered resource
// rating) and #639/#640 (variable credit-purchase quoting). A ChargeModel turns
// an aggregated quantity (in the meter's unit, or a credit quantity) into a cost
// in micros. It is deliberately YAML-independent: the loader builds a ChargeModel
// from a RatePrice, and both the usage rater and the credit quoter evaluate it.
//
// Why a divisor instead of fractional per-unit rates: OpenRails money is INTEGER
// micros, so $0.00595/hour cannot be a per-second rate (1.65 micros/sec is not an
// integer). Instead we keep the rate at its natural granularity (e.g. micros per
// hour) and DIVIDE the aggregate once: cost = round(quantity * unit_amount /
// divide_by). This pro-rates exactly (DO bills per-second) and rounds money once —
// it does NOT round usage up to whole units. For block / round-up-to-next-unit
// pricing use the package model.

// Charge-model kinds.
const (
	ModelFlat    = "flat"
	ModelPerUnit = "per_unit"
	ModelTiered  = "tiered"
	ModelPackage = "package"
	ModelDynamic = "dynamic"
)

// Tiered modes (OpenMeter TieredPriceMode / Lago graduated|volume / Stripe tiers_mode).
const (
	TierModeVolume    = "volume"
	TierModeGraduated = "graduated"
)

// Money rounding for the per-unit divisor. half_up is the money default; up/down
// force ceil/floor of the final micros.
const (
	RoundHalfUp = "half_up"
	RoundUp     = "up"
	RoundDown   = "down"
)

// ChargeModel is a normalized, YAML-independent pricing rule.
type ChargeModel struct {
	Kind string

	// per_unit: cost = round(quantity * UnitAmount / DivideBy). DivideBy<=0 == 1.
	UnitAmount int64
	DivideBy   int64
	Round      string

	// flat: a fixed fee independent of quantity.
	FlatAmount int64

	// tiered: Mode volume|graduated over Tiers (bounds in the meter's unit).
	Mode  string
	Tiers []ChargeTier

	// package: ceil(max(0, quantity-FreeUnits)/PackageSize) * PackageAmount.
	PackageSize   int64
	PackageAmount int64
	FreeUnits     int64

	// dynamic: quantity IS a reported cost in micros; scale by Multiplier
	// (micros-per-micro; 1_000_000 == passthrough, 1_500_000 == +50%).
	Multiplier int64

	// commitments applied to the computed cost, after the model.
	MinimumAmount int64 // floor (0 = none)
	MaximumAmount int64 // cap   (0 = none)
}

// ChargeTier is one band of a tiered price.
type ChargeTier struct {
	UpTo       *int64 // inclusive ceiling in units; nil = unbounded (last tier)
	UnitAmount int64  // per-unit micros within the band
	FlatAmount int64  // flat micros added once when the band is reached
}

// Rate computes the cost in micros for `quantity` units under the charge model.
// quantity must be >= 0 (for dynamic it is the reported cost in micros). The
// function is non-decreasing in quantity for flat/per_unit/package/graduated, so
// it is safely invertible (see QuoteUnitsForSpend); volume is NOT (tier cliffs).
func (cm ChargeModel) Rate(quantity int64) (int64, error) {
	if quantity < 0 {
		return 0, fmt.Errorf("quantity must be >= 0")
	}
	var cost int64
	var err error
	switch cm.Kind {
	case ModelFlat:
		cost = cm.FlatAmount
	case ModelPerUnit:
		cost, err = ratePerUnit(quantity, cm.UnitAmount, cm.DivideBy, cm.Round)
	case ModelTiered:
		cost, err = rateTiered(quantity, cm.Mode, cm.Tiers)
	case ModelPackage:
		cost, err = ratePackage(quantity, cm.PackageSize, cm.PackageAmount, cm.FreeUnits)
	case ModelDynamic:
		cost, err = mulDivRound(quantity, cm.Multiplier, 1_000_000, RoundHalfUp)
	default:
		return 0, fmt.Errorf("unknown charge model %q", cm.Kind)
	}
	if err != nil {
		return 0, err
	}
	return applyCommitments(cost, cm.MinimumAmount, cm.MaximumAmount), nil
}

func applyCommitments(cost, min, max int64) int64 {
	if min > 0 && cost < min {
		cost = min
	}
	if max > 0 && cost > max {
		cost = max
	}
	return cost
}

func ratePerUnit(quantity, unitAmount, divideBy int64, round string) (int64, error) {
	if unitAmount < 0 {
		return 0, fmt.Errorf("unit_amount must be >= 0")
	}
	if divideBy <= 0 {
		divideBy = 1
	}
	return mulDivRound(quantity, unitAmount, divideBy, round)
}

func rateTiered(quantity int64, mode string, tiers []ChargeTier) (int64, error) {
	if len(tiers) == 0 {
		return 0, fmt.Errorf("tiered price requires tiers")
	}
	switch mode {
	case TierModeVolume:
		return rateVolume(quantity, tiers)
	case TierModeGraduated:
		return rateGraduated(quantity, tiers)
	default:
		return 0, fmt.Errorf("tiered mode must be %q or %q, got %q", TierModeVolume, TierModeGraduated, mode)
	}
}

// rateVolume prices the WHOLE quantity at the band it lands in.
func rateVolume(quantity int64, tiers []ChargeTier) (int64, error) {
	for _, t := range tiers {
		if t.UpTo == nil || quantity <= *t.UpTo {
			return tierCost(quantity, t)
		}
	}
	return 0, fmt.Errorf("no tier matched quantity %d (last tier must be unbounded)", quantity)
}

// rateGraduated slices the quantity across bands and sums each band's cost.
func rateGraduated(quantity int64, tiers []ChargeTier) (int64, error) {
	total := new(big.Int)
	var lower int64 // previous band ceiling (exclusive lower bound of this band)
	remaining := quantity
	for _, t := range tiers {
		if remaining <= 0 {
			break
		}
		var sliceUnits int64
		if t.UpTo == nil {
			sliceUnits = remaining
		} else {
			width := max(*t.UpTo-lower, 0)
			sliceUnits = min(remaining, width)
			lower = *t.UpTo
		}
		if sliceUnits > 0 {
			c := new(big.Int).Mul(big.NewInt(sliceUnits), big.NewInt(t.UnitAmount))
			c.Add(c, big.NewInt(t.FlatAmount)) // flat added once, when band is reached
			total.Add(total, c)
			remaining -= sliceUnits
		}
	}
	if remaining > 0 {
		return 0, fmt.Errorf("quantity %d exceeds top tier (last tier must be unbounded)", quantity)
	}
	if !total.IsInt64() {
		return 0, fmt.Errorf("graduated cost overflows int64")
	}
	return total.Int64(), nil
}

func tierCost(units int64, t ChargeTier) (int64, error) {
	c := new(big.Int).Mul(big.NewInt(units), big.NewInt(t.UnitAmount))
	c.Add(c, big.NewInt(t.FlatAmount))
	if !c.IsInt64() {
		return 0, fmt.Errorf("tier cost overflows int64")
	}
	return c.Int64(), nil
}

func ratePackage(quantity, size, amount, free int64) (int64, error) {
	if size <= 0 {
		return 0, fmt.Errorf("package_size must be > 0")
	}
	billable := quantity - free
	if billable <= 0 {
		return 0, nil
	}
	packages := (billable + size - 1) / size // ceil
	c := new(big.Int).Mul(big.NewInt(packages), big.NewInt(amount))
	if !c.IsInt64() {
		return 0, fmt.Errorf("package cost overflows int64")
	}
	return c.Int64(), nil
}

// mulDivRound computes round(a*b/denom) in big.Int (overflow-safe), with a,b>=0
// and denom>0 so truncation == floor.
func mulDivRound(a, b, denom int64, mode string) (int64, error) {
	if denom <= 0 {
		return 0, fmt.Errorf("denominator must be positive")
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(denom)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() != 0 {
		switch mode {
		case RoundUp:
			q.Add(q, big.NewInt(1))
		case RoundDown:
			// QuoRem already truncated toward zero; with non-negative inputs that is floor.
		case RoundHalfUp, "":
			if new(big.Int).Mul(r, big.NewInt(2)).CmpAbs(d) >= 0 {
				q.Add(q, big.NewInt(1))
			}
		default:
			return 0, fmt.Errorf("unknown round mode %q", mode)
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("rated amount overflows int64")
	}
	return q.Int64(), nil
}

// QuoteUnitsForSpend returns the largest unit quantity whose cost is <= spend
// micros, plus that cost. It binary-searches Rate(), so it inverts ANY
// non-decreasing charge model (per_unit / graduated / package) without hand-coded
// tier inversion — this is the credit-purchase "enter $ -> credits" direction.
// It FLOORS units (never grants more value than paid). Volume mode is not
// monotonic at tier cliffs and must not be used here (see #640); callers building
// a credit_purchase price are validated to graduated/per_unit.
func QuoteUnitsForSpend(spend int64, cm ChargeModel) (units int64, cost int64, err error) {
	if spend < 0 {
		return 0, 0, fmt.Errorf("spend must be >= 0")
	}
	// Grow an upper bound until its cost exceeds the budget.
	lo, hi := int64(0), int64(1)
	for {
		c, err := cm.Rate(hi)
		if err != nil {
			return 0, 0, err
		}
		if c > spend {
			break
		}
		lo = hi
		if hi > (int64(1)<<62)/2 {
			break // pathological: free or near-free units; cap the search
		}
		hi *= 2
	}
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		c, err := cm.Rate(mid)
		if err != nil {
			return 0, 0, err
		}
		if c <= spend {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cost, err = cm.Rate(lo)
	if err != nil {
		return 0, 0, err
	}
	return lo, cost, nil
}
