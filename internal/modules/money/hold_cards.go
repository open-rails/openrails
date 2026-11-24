package money

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// ViewCards are pinned money rate cards on the wire, and the highest
// marginal price of each meter cell they set. A dynamic card has no bound:
// its gate needs the stated amount.
func ViewCards(pinned []byte) ([]billing.ViewRateCard, []billing.PriceBound, error) {
	cards := []billing.ViewRateCard{}
	bounds := []billing.PriceBound{}
	if len(pinned) == 0 {
		return cards, bounds, nil
	}
	var rating []ratingCard
	if err := json.Unmarshal(pinned, &rating); err != nil {
		return nil, nil, fmt.Errorf("decode pinned rate cards: %w", err)
	}
	for _, c := range rating {
		cards = append(cards, billing.ViewRateCard{Meter: c.MeterKey, EventType: c.EventType, ValueProperty: c.ValueKey, Aggregation: c.Aggregation,
			GroupBy: c.GroupBy, Filter: c.Filter, Allowance: c.Allowance, Price: c.Price})
		bounds = append(bounds, priceBounds(c)...)
	}
	return cards, bounds, nil
}

// priceBounds is a card's highest marginal price per cell: per_unit its
// unit amount per divide_by, a graduated tier's highest unit amount, a
// package's whole amount for one unit.
func priceBounds(c ratingCard) []billing.PriceBound {
	p := c.Price
	switch {
	case p.PerUnit != nil:
		divide := max(p.PerUnit.DivideBy, 1)
		if p.PerUnit.Matrix == nil {
			return []billing.PriceBound{{Meter: c.MeterKey, UnitAmount: p.PerUnit.UnitAmount, DivideBy: divide}}
		}
		cells := make([]string, 0, len(p.PerUnit.Matrix.Cells))
		for cell := range p.PerUnit.Matrix.Cells {
			cells = append(cells, cell)
		}
		sort.Strings(cells)
		out := make([]billing.PriceBound, 0, len(cells))
		for _, cell := range cells {
			name := cell
			out = append(out, billing.PriceBound{Meter: c.MeterKey, Cell: &name, UnitAmount: p.PerUnit.Matrix.Cells[cell].UnitAmount, DivideBy: divide})
		}
		return out
	case p.Tiered != nil:
		var top int64
		for _, t := range p.Tiered.Tiers {
			top = max(top, t.UnitAmount)
		}
		return []billing.PriceBound{{Meter: c.MeterKey, UnitAmount: top, DivideBy: 1}}
	case p.Package != nil:
		return []billing.PriceBound{{Meter: c.MeterKey, UnitAmount: p.Package.Amount, DivideBy: 1}}
	case p.Model == catalog.ModelDynamic:
		return nil
	}
	return nil
}
