package catalog

import (
	"strings"
	"testing"
)

// or#883: a grant's unit selects the money account at deposit
// (models.CreditGrantSpec.UnitCode -> money.validateCreditGrantSpec). A grant
// that names balance `ai-images` but declares `unit: usd` used to deposit into
// the USD account instead — money in the wrong place, no error. The unit is
// derivable from the balance the grant already names, so the knob is gone and
// declaring it is a loud removal error.

const creditUnitManifestPrefix = `
version: 1
credit_balances:
  - key: ai-images
    unit: local-stack/ai-image-credit
products:
  - key: premium
    display_name: Premium
    tier_group: g
    tier_rank: 1
    entitlements: [premium]
    credits:
`

const creditUnitManifestSuffix = `    prices:
      - currency: usd
        unit_amount: 12_000_000
        duration: 30d
        auto_renew: true
`

func creditUnitManifest(t *testing.T, grant string) string {
	t.Helper()
	return writeManifest(t, creditUnitManifestPrefix+grant+creditUnitManifestSuffix)
}

// Even an AGREEING declaration is refused: the field is gone, not validated.
// Tolerating the agreeing case keeps the knob alive and keeps teaching it.
func TestLoad_GrantUnitAgreeingWithBalanceIsAlsoRefused(t *testing.T) {
	_, err := Load(creditUnitManifest(t, `      - key: ai-images
        unit: local-stack/ai-image-credit
        amount: 100
`))
	if err == nil {
		t.Fatal("want load error for a declared grant unit (removed field), got nil")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Fatalf("want a unit removal error, got %v", err)
	}
}

// The positive half: an undeclared grant unit resolves to the balance's unit,
// which is the unit that reaches the deposit.
func TestLoad_GrantUnitDerivedFromBalance(t *testing.T) {
	m, err := Load(creditUnitManifest(t, `      - key: ai-images
        amount: 100
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	grant := m.TierGroups[0].Products[0].Credits[0]
	if grant.Unit != "local-stack/ai-image-credit" {
		t.Fatalf("grant unit not derived from balance: %q", grant.Unit)
	}
	// The unit that actually selects the money account at deposit time.
	spec := creditsSpec(m.TierGroups[0].Products[0].Credits)
	if got := spec["ai-images"].Unit; got != "local-stack/ai-image-credit" {
		t.Fatalf("credits_spec unit not derived from balance: %q", got)
	}
	if got := spec["ai-images"].Amount; got != 100 {
		t.Fatalf("credits_spec amount: got %d want 100", got)
	}
}

// A built-in currency balance derives the same way, normalized to the ledger's
// uppercase currency form.
func TestLoad_GrantUnitDerivedFromCurrencyBalance(t *testing.T) {
	m, err := Load(writeManifest(t, `
version: 1
credit_balances:
  - key: monthly-usd
    unit: usd
products:
  - key: premium
    display_name: Premium
    tier_group: g
    tier_rank: 1
    entitlements: [premium]
    credits:
      - key: monthly-usd
        amount: 25_000_000
        cadence: per_renewal
    prices:
      - currency: usd
        unit_amount: 12_000_000
        duration: 30d
        auto_renew: true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.TierGroups[0].Products[0].Credits[0].Unit; got != "USD" {
		t.Fatalf("grant unit not derived from currency balance: %q", got)
	}
}
