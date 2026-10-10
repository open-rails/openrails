// Package catalogrules is the catalog engine's rules: charge-model rating and
// the validation of meters, rate cards and their prices.
package catalogrules

import (
	"fmt"
	"math/big"

	"github.com/open-rails/openrails/catalog"
)

// A ChargeModel is the shared pricing engine for metered usage rating and
// variable credit-purchase quoting: it turns an aggregated quantity into a
// cost in micros.
//
// Money is integer micros, so a rate keeps its natural granularity (micros per
// hour) and the aggregate is divided once: cost = round(quantity * unit_amount
// / divide_by). This pro-rates exactly and rounds money once; it never rounds
// usage up to whole units (use the package model for that).

// ChargeModel is a normalized, YAML-independent pricing rule.
type ChargeModel struct {
	Kind catalog.Model

	// per_unit: cost = round(quantity * UnitAmount / DivideBy). DivideBy<=0 == 1.
	UnitAmount int64
	DivideBy   int64
	Round      catalog.Round

	// flat: a fixed fee independent of quantity.
	FlatAmount int64

	// tiered: Mode volume|graduated over Tiers (bounds in the meter's unit).
	Mode  catalog.TierMode
	Tiers []ChargeTier

	// package: ceil(max(0, quantity-FreeUnits)/PackageSize) * PackageAmount.
	PackageSize   int64
	PackageAmount int64
	FreeUnits     int64

	// cap applied to the computed cost (0 = uncapped). There is no per-line
	// floor: minimums are an account commitment, not pricing.
	MaximumAmount int64
}

// ChargeTier is one band of a tiered price.
type ChargeTier struct {
	UpTo       *int64 // inclusive ceiling in units; nil = unbounded (last tier)
	UnitAmount int64  // per-unit micros within the band
	FlatAmount int64  // flat micros added once when the band is reached
}

// Rate computes the cost in micros for quantity >= 0 units. It is
// non-decreasing in quantity for flat/per_unit/package/graduated; volume rates
// may have tier cliffs.
func (cm ChargeModel) Rate(quantity int64) (int64, error) {
	if quantity < 0 {
		return 0, fmt.Errorf("quantity must be >= 0")
	}
	var cost int64
	var err error
	switch cm.Kind {
	case catalog.ModelFlat:
		cost = cm.FlatAmount
	case catalog.ModelPerUnit:
		cost, err = ratePerUnit(quantity, cm.UnitAmount, cm.DivideBy, cm.Round)
	case catalog.ModelTiered:
		cost, err = rateTiered(quantity, cm.Mode, cm.Tiers)
	case catalog.ModelPackage:
		cost, err = ratePackage(quantity, cm.PackageSize, cm.PackageAmount, cm.FreeUnits)
	default:
		return 0, fmt.Errorf("unknown charge model %q", cm.Kind)
	}
	if err != nil {
		return 0, err
	}
	if cm.MaximumAmount > 0 && cost > cm.MaximumAmount {
		cost = cm.MaximumAmount
	}
	return cost, nil
}

func ratePerUnit(quantity, unitAmount, divideBy int64, round catalog.Round) (int64, error) {
	if unitAmount < 0 {
		return 0, fmt.Errorf("unit_amount must be >= 0")
	}
	if divideBy <= 0 {
		divideBy = 1
	}
	return mulDivRound(quantity, unitAmount, divideBy, round)
}

func rateTiered(quantity int64, mode catalog.TierMode, tiers []ChargeTier) (int64, error) {
	if len(tiers) == 0 {
		return 0, fmt.Errorf("tiered price requires tiers")
	}
	switch mode {
	case catalog.TierModeVolume:
		return rateVolume(quantity, tiers)
	case catalog.TierModeGraduated:
		return rateGraduated(quantity, tiers)
	default:
		return 0, fmt.Errorf("tiered mode must be %q or %q, got %q", catalog.TierModeVolume, catalog.TierModeGraduated, mode)
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
func mulDivRound(a, b, denom int64, mode catalog.Round) (int64, error) {
	if denom <= 0 {
		return 0, fmt.Errorf("denominator must be positive")
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(denom)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() != 0 {
		switch mode {
		case catalog.RoundUp:
			q.Add(q, big.NewInt(1))
		case catalog.RoundDown:
			// QuoRem already truncated toward zero; with non-negative inputs that is floor.
		case catalog.RoundHalfUp, "":
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

// Of normalizes a RatePrice into its ChargeModel. For a matrix price use
// ForCell.
func Of(rp catalog.RatePrice) ChargeModel {
	cm := ChargeModel{Kind: rp.Model}
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

// ForCell is the per-unit ChargeModel of one matrix cell: the cell's
// unit_amount with the price's divisor and rounding, and the cell's cap or else
// the price's. It is false when the price is not a matrix or has no such cell.
func ForCell(rp catalog.RatePrice, dimValue string) (ChargeModel, bool) {
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
	return ChargeModel{Kind: catalog.ModelPerUnit, UnitAmount: cell.UnitAmount, DivideBy: rp.PerUnit.DivideBy, Round: rp.PerUnit.Round, MaximumAmount: maxAmt}, true
}

// RateUsage rates quantity units against a card, through the matrix cell of
// dimValue when the price is a matrix. Allowances are netted by the caller.
func RateUsage(rc catalog.RateCard, dimValue string, quantity int64) (int64, error) {
	if rc.Price.PerUnit != nil && rc.Price.PerUnit.Matrix != nil {
		cm, ok := ForCell(rc.Price, dimValue)
		if !ok {
			return 0, fmt.Errorf("meter %q rate card has no matrix cell for %q=%q", rc.Meter, rc.Price.PerUnit.Matrix.Dimension, dimValue)
		}
		return cm.Rate(quantity)
	}
	return Of(rc.Price).Rate(quantity)
}
