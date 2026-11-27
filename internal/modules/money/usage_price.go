package money

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A period's usage is priced from its total: each meter cell is priced card
// by card, in the order the cards' prints were first written, each card
// pricing its own quantity on top of the earlier cards' (R_p(Q_<p + q_p) -
// R_p(Q_<p)); a dynamic card charges what was stated. Within one card the
// order usage arrived in changes nothing, so late usage and concurrent gates
// price exactly and per-unit rounding happens once per total. A posting takes
// the price less what the period's earlier usage receipts took.

// printCard is a card print's terms and when it was first written.
type printCard struct {
	card  ratingCard
	order time.Time
}

// usagePrints are the merchant's card prints, by print.
type usagePrints map[string]printCard

// CardsInForce are the rate cards in force for an event type in a currency,
// as a gate's view prices with them; nil when none prices it.
func (s *MoneyService) CardsInForce(ctx context.Context, q *gen.Queries, eventType, currency string) ([]byte, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cards, err := usageCards(ctx, q, mid.UUID(), strings.TrimSpace(eventType), normalizeCurrency(currency))
	var coded *apperr.Error
	switch {
	case errors.As(err, &coded):
		return nil, nil
	case err != nil:
		return nil, err
	case len(cards) == 0:
		return nil, nil
	}
	return json.Marshal(cards)
}

// loadPrints reads the cards of prints; a print not yet committed is absent.
func loadPrints(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, prints []string) (usagePrints, error) {
	out := usagePrints{}
	slices.Sort(prints)
	prints = slices.Compact(prints)
	if len(prints) == 0 {
		return out, nil
	}
	rows, err := q.ListUsageCardPrints(ctx, gen.ListUsageCardPrintsParams{MerchantID: merchantID, Prints: prints})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		var card ratingCard
		if err := json.Unmarshal(r.Card, &card); err != nil {
			return nil, fmt.Errorf("decode card print %s: %w", r.Print, err)
		}
		out[r.Print] = printCard{card: card, order: r.CreatedAt}
	}
	return out, nil
}

// printQuantity is one print's quantity of a meter cell.
type printQuantity struct {
	print            string
	quantity, stated int64
	requests         int64
}

// cellPrice prices a period's quantities of one meter cell, by print, in
// place: qs is sorted into print order and the amounts are aligned with it.
// A print with no card is an error.
func (p usagePrints) cellPrice(cell string, qs []printQuantity) ([]int64, error) {
	slices.SortFunc(qs, func(a, b printQuantity) int {
		return cmp.Or(p[a.print].order.Compare(p[b.print].order), strings.Compare(a.print, b.print))
	})
	out := make([]int64, len(qs))
	var before int64
	for i, q := range qs {
		pc, ok := p[q.print]
		if !ok {
			return nil, fmt.Errorf("usage card print %s is not recorded", q.print)
		}
		if before > maxQuantity-q.quantity || q.quantity < 0 {
			return nil, fmt.Errorf("meter %q: period quantity out of range", pc.card.MeterKey)
		}
		if pc.card.Price.Model == catalog.ModelDynamic {
			out[i] = q.stated
		} else {
			amount, err := marginalPrice(pc.card, cell, before, q.quantity)
			if err != nil {
				return nil, err
			}
			out[i] = amount
		}
		before += q.quantity
	}
	return out, nil
}

// cellKey names a meter cell of a pricing period.
type cellKey struct {
	period      time.Time
	meter, cell string
}

// lineKey names a meter cell under one print.
type lineKey struct {
	meter, cell, print string
}

// periodLines are each period's priced lines, by meter cell and print.
type periodLines map[time.Time]map[lineKey]*PostingLine

// periodQuantity is a customer's settled usage of one meter cell under one
// card print in a pricing period.
type periodQuantity struct {
	period                     time.Time
	meter, cell, print         string
	quantity, stated, requests int64
}

// priceQuantities prices a customer's period quantities: the price of each
// period's total, line by line.
func (p usagePrints) priceQuantities(rows []periodQuantity) (periodLines, error) {
	cells := map[cellKey][]printQuantity{}
	for _, r := range rows {
		k := cellKey{r.period, r.meter, r.cell}
		cells[k] = append(cells[k], printQuantity{print: r.print, quantity: r.quantity, stated: r.stated, requests: r.requests})
	}
	out := periodLines{}
	for k, qs := range cells {
		amounts, err := p.cellPrice(k.cell, qs)
		if err != nil {
			return nil, err
		}
		lines := out[k.period]
		if lines == nil {
			lines = map[lineKey]*PostingLine{}
			out[k.period] = lines
		}
		for i, q := range qs {
			lines[lineKey{k.meter, k.cell, q.print}] = &PostingLine{MeterKey: k.meter, Cell: k.cell, Print: q.print,
				Requests: q.requests, Quantity: q.quantity, Stated: q.stated, Amount: amounts[i]}
		}
	}
	return out, nil
}

// PostingLine is what a posting added for one meter cell under one card
// print: usage's requests, quantity and stated amounts, and its price.
type PostingLine struct {
	MeterKey string `json:"meter_key"`
	Cell     string `json:"cell"`
	Print    string `json:"print"`
	Requests int64  `json:"requests"`
	Quantity int64  `json:"quantity"`
	Stated   int64  `json:"stated"`
	Amount   int64  `json:"amount"`
}

func decodePostingLines(raw []byte) ([]PostingLine, error) {
	var lines []PostingLine
	if len(raw) == 0 {
		return lines, nil
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, fmt.Errorf("decode posting lines: %w", err)
	}
	return lines, nil
}
