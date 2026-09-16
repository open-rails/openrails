package openrails

import (
	"encoding/json"
	"math"
	"testing"
)

func TestInvoiceMoneyWireIsLossless(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, 0, 9007199254740993, math.MaxInt64} {
		invoice := InvoiceDTO{AmountDue: amount, TotalAmount: amount, MoneyMovements: AmountMap{"deposit": amount}, LineItems: []InvoiceLineItemDTO{{Amount: amount}}}
		raw, err := json.Marshal(invoice)
		if err != nil {
			t.Fatal(err)
		}
		var browser map[string]any
		if err := json.Unmarshal(raw, &browser); err != nil {
			t.Fatal(err)
		}
		if _, ok := browser["amount_due"].(string); !ok {
			t.Fatalf("unsafe browser amount: %s", raw)
		}
		var read InvoiceDTO
		if err := json.Unmarshal(raw, &read); err != nil {
			t.Fatal(err)
		}
		if read.AmountDue != amount || read.TotalAmount != amount || read.MoneyMovements["deposit"] != amount || read.LineItems[0].Amount != amount {
			t.Fatalf("lost amount precision: %s", raw)
		}
	}
	for _, raw := range []string{`{"deposit":9007199254740993}`, `{"deposit":"9223372036854775808"}`} {
		var out AmountMap
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			t.Fatalf("accepted invalid monetary wire %s", raw)
		}
	}
}
