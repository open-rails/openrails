package moneyutil

import (
	"testing"

	"github.com/open-rails/openrails/internal/currency"
)

func TestFormatUnits(t *testing.T) {
	gold, _ := currency.Unit("GOLD", "Gold coins", "", 0)
	credits, _ := currency.Unit("CREDITS", "Credits", "", 2)
	usd, _ := LookupCurrency("USD")
	for _, c := range []struct {
		amount int64
		cur    Currency
		want   string
	}{{400, gold, "400 GOLD"}, {-5, gold, "-5 GOLD"}, {1250, credits, "12.50 CREDITS"}, {1, credits, "0.01 CREDITS"}, {19_990_000, usd, "19.99 USD"}} {
		if got := FormatUnits(c.amount, c.cur); got != c.want {
			t.Errorf("FormatUnits(%d, %s) = %q, want %q", c.amount, c.cur.Code, got, c.want)
		}
	}
}
