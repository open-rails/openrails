package limits

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Measure is what one rate card reads of u, as Units reads it: whether the
// card counts u at all (its meter reads u's event type and its filter admits
// u), the meter's quantity, and the matrix cell u falls in ("" without a
// matrix). A matrix with no cell for u's value refuses u.
func Measure(m Meter, card catalog.LimitRateCard, u Usage) (int64, string, bool, error) {
	if m.EventType != u.EventType || !admits(m, card.Filter, u) {
		return 0, "", false, nil
	}
	q, err := quantity(m, u)
	if err != nil || q == 0 {
		return 0, "", false, err
	}
	cell := ""
	if card.Price.PerUnit != nil && card.Price.PerUnit.Matrix != nil {
		matrix := card.Price.PerUnit.Matrix
		value, param := eventValue(u, property(m, matrix.Dimension))
		if _, ok := matrix.Cells[value]; !ok {
			return 0, "", false, &apperr.Error{Status: http.StatusUnprocessableEntity, Code: billing.CodeUsageNotPriced, Param: param,
				Message: fmt.Sprintf("meter %q has no price for %s %q", card.Meter, matrix.Dimension, value)}
		}
		cell = value
	}
	return q, cell, true, nil
}

// PropertyKey is a meter property as events carry it.
func PropertyKey(p string) string { return propertyKey(p) }
