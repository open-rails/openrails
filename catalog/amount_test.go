package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestHumanAmountsUseExactRegisteredNativeUnits(t *testing.T) {
	for _, test := range []struct {
		input    string
		amount   int64
		currency string
	}{
		{"9.99 USD", 9_990_000, "USD"},
		{"  9.990000\tusd  ", 9_990_000, "USD"},
		{"0009.9900 EUR", 9_990_000, "EUR"},
		{"500 JPY", 5_000_000, "JPY"},
		{"0.0001 JPY", 1, "JPY"},
		{"1 SOL", 1_000_000_000, "SOL"},
		{"0.000000001 SOL", 1, "SOL"},
		{"10 USDC", 10_000_000, "USDC"},
		{"0.000001 USDC", 1, "USDC"},
		{"0 USD", 0, "USD"},
		{"0.000000 USD", 0, "USD"},
		{"9007199254.740993 USD", 9_007_199_254_740_993, "USD"},
		{"9223372036854.775807 USD", math.MaxInt64, "USD"},
		{"9223372036854.775807 USDC", math.MaxInt64, "USDC"},
		{"9223372036.854775807 SOL", math.MaxInt64, "SOL"},
		{"922337203685477.5807 JPY", math.MaxInt64, "JPY"},
	} {
		t.Run(test.input, func(t *testing.T) {
			amount, currency, err := parseAmount(test.input)
			if err != nil || amount != test.amount || currency != test.currency {
				t.Fatalf("got %d %s, error %v; want %d %s", amount, currency, err, test.amount, test.currency)
			}
		})
	}
}

func TestHumanAmountsRefuseRoundingAndAmbiguousNumbers(t *testing.T) {
	for _, input := range []string{
		"", "9.99", "USD", "9.99 USD extra", "USD 9.99", "9.99 UNKNOWN",
		"-1 USD", "-0 USD", "+1 USD", ".99 USD", "1. USD", "1.2.3 USD",
		"1e3 USD", "1/2 USD", "1,000 USD", "1_000 USD", "NaN USD", "Inf USD",
		"0.0000001 USD", "9.9900000 USD", "0.0000000 USD", "0.0000000001 SOL", "0.0000001 USDC",
		"9223372036854.775808 USD", "9223372036.854775808 SOL",
		"9223372036854.775808 USDC", "922337203685477.5808 JPY",
		"9223372036854775807 USD",
	} {
		if _, _, err := parseAmount(input); err == nil {
			t.Errorf("accepted invalid or inexact amount %q", input)
		}
	}
}

func TestApplicationHumanAmountSharesNumericIdentity(t *testing.T) {
	var identity [32]byte
	for i, amount := range []string{"9.99 USD", "9.990000 usd", "0009.99 USD"} {
		application, err := ParseApplicationYAML([]byte(fmt.Sprintf("schema_version: 1\nproducts:\n- key: p\n  prices:\n  - key: monthly\n    amount: %s\n", amount)))
		if err != nil {
			t.Fatal(err)
		}
		price := application.Products[0].Prices[0]
		if price.UnitAmount != Value[int64](9_990_000) || price.Currency != Value("USD") {
			t.Fatalf("amount did not set both canonical money fields: %+v", price)
		}
		if i == 0 {
			identity = digest(t, *application)
		} else if digest(t, *application) != identity {
			t.Fatalf("equivalent decimal %q changed application identity", amount)
		}
		encoded, err := json.Marshal(application)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"amount":`) || !strings.Contains(string(encoded), `"unit_amount":"9990000"`) {
			t.Fatalf("serialized application is not normalized native units: %s", encoded)
		}
	}
	numeric, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","prices":[{"key":"monthly","unit_amount":"9990000","currency":"USD"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if digest(t, *numeric) != identity {
		t.Fatal("human and numeric money must address the same application")
	}
	omitted, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","prices":[{"key":"monthly"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	price := omitted.Products[0].Prices[0]
	if price.UnitAmount.Set || price.Currency.Set {
		t.Fatal("omitted amount must preserve both existing money fields")
	}
}

func TestApplicationHumanAmountRejectsMixedMoneyFields(t *testing.T) {
	for _, fields := range []string{
		`"amount":null`,
		`"amount":9.99`,
		`"amount":"9.99"`,
		`"amount":"9.99 USD","unit_amount":"9990000"`,
		`"amount":"9.99 USD","unit_amount":null`,
		`"amount":"9.99 USD","currency":"USD"`,
		`"amount":"9.99 USD","currency":null`,
		`"amount":"9.99 USD","unit_amount":"9990000","currency":"USD"`,
		`"Amount":"9.99 USD"`,
	} {
		raw := fmt.Sprintf(`{"schema_version":1,"products":[{"key":"p","prices":[{"key":"monthly",%s}]}]}`, fields)
		if _, err := ParseApplicationJSON([]byte(raw)); err == nil {
			t.Errorf("accepted ambiguous money fields %s", fields)
		}
	}
	if _, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","prices":[{"key":"free","amount":"0 USD"}]}]}`)); err != nil {
		t.Fatalf("zero must retain the existing catalog money rules: %v", err)
	}
}
