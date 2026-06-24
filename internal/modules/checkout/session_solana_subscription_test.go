package checkout

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

func solanaRecurringPrice() *models.Price {
	p := &models.Price{}
	p.SetRailConfig(models.RailSolana, map[string]string{
		"plan_pda":          "PdA1111111111111111111111111111111111111111",
		"plan_id":           "7",
		"mint":              "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		"mint_symbol":       "USDC",
		"amount_base_units": "10000000",
		"period_hours":      "720",
		"created_at":        "1700000000",
		"merchant_address":  "Merch11111111111111111111111111111111111111",
	})
	return p
}

func TestPriceHasSolanaRecurring(t *testing.T) {
	if !priceHasSolanaRecurring(solanaRecurringPrice()) {
		t.Error("a fully-configured solana recurring price should be recognized")
	}
	if priceHasSolanaRecurring(&models.Price{}) {
		t.Error("a price with no solana config is not recurring")
	}
	// Missing a required key (amount) → not recurring.
	p := &models.Price{}
	p.SetRailConfig(models.RailSolana, map[string]string{
		"plan_id": "7", "period_hours": "720", "mint_symbol": "USDC",
	})
	if priceHasSolanaRecurring(p) {
		t.Error("missing amount_base_units must not count as recurring")
	}
	if priceHasSolanaRecurring(nil) {
		t.Error("nil price is not recurring")
	}
}

func TestParseSolanaPlanTerms(t *testing.T) {
	cfg := solanaRecurringPrice().GetRailConfig(models.RailSolana)
	terms, err := parseSolanaPlanTerms(cfg)
	if err != nil {
		t.Fatalf("parseSolanaPlanTerms: %v", err)
	}
	if terms.planID != 7 || terms.amount != 10_000_000 || terms.period != 720 ||
		terms.mintSymbol != "USDC" || terms.createdAt != 1_700_000_000 {
		t.Errorf("parsed terms wrong: %+v", terms)
	}

	// Invalid / missing fields reject.
	if _, err := parseSolanaPlanTerms(nil); err == nil {
		t.Error("nil cfg must error")
	}
	if _, err := parseSolanaPlanTerms(map[string]string{"plan_id": "x"}); err == nil {
		t.Error("bad plan_id must error")
	}
	if _, err := parseSolanaPlanTerms(map[string]string{
		"plan_id": "1", "amount_base_units": "0", "period_hours": "720", "mint_symbol": "USDC",
	}); err == nil {
		t.Error("zero amount must error")
	}
}

func TestGetStringSliceField(t *testing.T) {
	// JSONB decodes []string back as []any of strings.
	state := map[string]any{"sign_transactions": []any{"tx1", "tx2"}}
	got := getStringSliceField(state, "sign_transactions")
	if len(got) != 2 || got[0] != "tx1" || got[1] != "tx2" {
		t.Errorf("got %v, want [tx1 tx2]", got)
	}
	if getStringSliceField(nil, "x") != nil {
		t.Error("nil map -> nil")
	}
	if getStringSliceField(map[string]any{}, "missing") != nil {
		t.Error("missing key -> nil")
	}
	// Native []string also supported.
	if g := getStringSliceField(map[string]any{"k": []string{"a"}}, "k"); len(g) != 1 || g[0] != "a" {
		t.Errorf("native []string: got %v", g)
	}
}
