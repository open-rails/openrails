package purchasedcredits

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/money/books"
)

// A pack of a merchant's unit never takes the legacy path: its paid
// currency's micros would post as units.
func TestReversalPath(t *testing.T) {
	for _, c := range []struct {
		name string
		lot  books.CreditLot
		want reversalPath
	}{
		{"booked money lot", books.CreditLot{Unit: "USD", PaidCurrency: "USD", PaidAmount: 20_000_000, DeferredCredited: 20_000_000}, reversalInBooks},
		{"booked unit pack", books.CreditLot{Unit: "GOLD", PaidCurrency: "USD", PaidAmount: 20_000_000, DeferredCredited: 20_000_000}, reversalInBooks},
		{"money lot from before the books", books.CreditLot{Unit: "USD", PaidCurrency: "USD", PaidAmount: 20_000_000}, reversalLegacy},
		{"unit pack never booked", books.CreditLot{Unit: "GOLD", PaidCurrency: "USD", PaidAmount: 20_000_000}, reversalRefused},
	} {
		if got := reversalPathOf(c.lot); got != c.want {
			t.Errorf("%s: path %d, want %d", c.name, got, c.want)
		}
	}
}
