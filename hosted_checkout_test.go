package openrails

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestNewHostedCheckoutPlanStampsRegistryScale(t *testing.T) {
	hours := 720
	product := &CatalogProduct{ID: uuid.New(), DisplayName: "Premium"}
	price := &CatalogPrice{ID: uuid.New(), UnitAmount: math.MaxInt64, Currency: "jpy", AccessDurationHours: &hours, AutoRenew: true}
	plan, err := NewHostedCheckoutPlan(product, price)
	if err != nil {
		t.Fatal(err)
	}
	if plan != (HostedCheckoutPlan{DisplayName: "Premium", UnitAmount: math.MaxInt64, Currency: "JPY", UnitDecimals: 4, PeriodHours: &hours, AutomaticallyRenews: true}) {
		t.Fatalf("plan: %#v", plan)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["unit_amount"] != "9223372036854775807" || decoded["unit_decimals"] != float64(4) || decoded["currency"] != "JPY" {
		t.Fatalf("plan wire: %s", raw)
	}

	price.Currency = "XYZ"
	if _, err := NewHostedCheckoutPlan(product, price); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered currency must be ErrInvalid, got %v", err)
	}
	if _, err := NewHostedCheckoutPlan(nil, price); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing product must be ErrInvalid, got %v", err)
	}
}

func TestHostedCheckoutDriver(t *testing.T) {
	for rail, want := range map[string]string{"nmi": "collect_js", "NMI ": "collect_js", "stripe": "redirect", "ccbill": "redirect", "solana": "solana_pay"} {
		if got, ok := HostedCheckoutDriver(rail); !ok || got != want {
			t.Fatalf("%q -> %q %v, want %q", rail, got, ok, want)
		}
	}
	if _, ok := HostedCheckoutDriver("basis_theory"); ok {
		t.Fatal("a rail the browser cannot execute must not be offered")
	}
}
