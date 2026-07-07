package catalog

import (
	"context"
	"strings"
	"testing"
)

// TestPlan_PriceKeyAutoDefault: a single price per product+interval auto-
// defaults to "<product-key>-<interval>".
func TestPlan_PriceKeyAutoDefault(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: solo
    display_name: Solo
    prices:
      - {currency: usd, unit_amount: 999, duration: 30d, auto_renew: true}
`)
	plan, err := Plan(context.Background(), newFakeApplier(), m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "solo")
	if pp == nil || len(pp.Prices) != 1 {
		t.Fatalf("expected 1 price, got %+v", pp)
	}
	if got, want := pp.Prices[0].CreateReq.Key, "solo-monthly"; got != want {
		t.Fatalf("auto-default key = %q, want %q", got, want)
	}
	if got := pp.Prices[0].Key; got != "solo-monthly" {
		t.Fatalf("plan key = %q, want solo-monthly", got)
	}
}

// TestPlan_PriceKeyCollisionRefused: two declared prices at the SAME interval
// with neither given an explicit key must both resolve to the identical
// default — refused loudly at plan time (#774), never silently colliding.
func TestPlan_PriceKeyCollisionRefused(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: ambiguous
    display_name: Ambiguous
    prices:
      - {currency: usd, unit_amount: 999, duration: 30d, auto_renew: true}
      - {currency: usd, unit_amount: 499, duration: 30d, auto_renew: true, providers: [stripe]}
`)
	_, err := Plan(context.Background(), newFakeApplier(), m)
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous-monthly") || !strings.Contains(err.Error(), "disambiguate") {
		t.Fatalf("error should name the colliding key and ask for disambiguation, got: %v", err)
	}
}

// TestPlan_PriceKeyExplicitDisambiguates: two prices at the same interval are
// fine once each carries a distinct explicit key.
func TestPlan_PriceKeyExplicitDisambiguates(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: promo
    display_name: Promo
    prices:
      - {currency: usd, unit_amount: 999, duration: 30d, auto_renew: true, key: promo-monthly-standard}
      - {currency: usd, unit_amount: 499, duration: 30d, auto_renew: true, key: promo-monthly-launch}
`)
	plan, err := Plan(context.Background(), newFakeApplier(), m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "promo")
	if len(pp.Prices) != 2 {
		t.Fatalf("expected 2 prices, got %d", len(pp.Prices))
	}
	seen := map[string]bool{}
	for _, p := range pp.Prices {
		seen[p.CreateReq.Key] = true
	}
	if !seen["promo-monthly-standard"] || !seen["promo-monthly-launch"] {
		t.Fatalf("explicit keys not carried through: %+v", pp.Prices)
	}
}

// TestPlan_PriceKeyRelabelOnMatch: a substance-unchanged price declared under
// a DIFFERENT key signals a relabel (plp.Key set) without touching the match
// action, and apply calls SetPriceKey exactly once.
func TestPlan_PriceKeyRelabelOnMatch(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: renamed
    display_name: Renamed
    prices:
      - {currency: usd, unit_amount: 1200, duration: 30d, auto_renew: true, key: renamed-pro-monthly}
`)
	f := newFakeApplier()
	product := f.seedProduct("renamed", "", 0, false)
	f.seedPrice(product.ID, 1200, "usd", 30*24, false)
	// fakeApplier.seedPrice does not set a key; simulate the pre-existing row's
	// old key directly.
	f.prices[product.ID][0].Key = "renamed-monthly"

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "renamed")
	if pp.Prices[0].Action != PriceUnchanged {
		t.Fatalf("substance unchanged -> want PriceUnchanged, got %s", pp.Prices[0].Action)
	}
	if pp.Prices[0].Key != "renamed-pro-monthly" {
		t.Fatalf("expected relabel signal to the new key, got %q", pp.Prices[0].Key)
	}

	if _, err := ApplyWithOptions(context.Background(), f, plan, ApplyOptions{Overwrite: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := f.relabeledPrices[pp.Prices[0].ExistingID]; got != "renamed-pro-monthly" {
		t.Fatalf("SetPriceKey not called with the new key: %+v", f.relabeledPrices)
	}
}
