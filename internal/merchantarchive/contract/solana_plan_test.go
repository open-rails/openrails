package contract

import (
	"testing"

	"github.com/open-rails/openrails/internal/cardguard"
)

// A Solana plan id is the program's u64 in decimal; some pass the card scan.
// Wherever the archive holds one it is an identifier, and anywhere else the
// same digits are still refused.
func TestSolanaPlanIDIsNotACardNumber(t *testing.T) {
	const plan = "2308791993042649354"
	if !cardguard.ContainsPAN(plan) {
		t.Fatal("the fixture plan id must pass the card scan")
	}
	const owner = `"merchant_address": "4q5MVUWTVEWySWW78uppbRkxCAJKwrZcWnhbuQ5KJ9Mx"`
	accepted := []struct {
		table string
		row   map[string]string
	}{
		{"change_log", map[string]string{"changes": `{"version": 1, "revision": 1, "fields": {"plans": {"after": {"solana": {"rail": "solana", "plan_id": "` + plan + `", "token": "USDC", ` + owner + `}}}}}`}},
		{"price_psp_bindings", map[string]string{"plan_id": plan, "configuration": `{"token": "USDC", ` + owner + `}`}},
		{"invoice_attempts", map[string]string{"invoice_id": "01a211d7-a76c-7291-9740-dfd33f683595", "amount": "10000000", "currency": "USD", "rail": "solana",
			"rail_state": `{"flow": "subscribe", "plan_id": "` + plan + `"}`}},
	}
	for _, c := range accepted {
		if err := ValidateValues(profile(t, c.table), row(t, c.table, c.row)); err != nil {
			t.Errorf("%s: %v", c.table, err)
		}
	}
	refused := []struct {
		table string
		row   map[string]string
	}{
		{"change_log", map[string]string{"changes": `{"version": 1, "fields": {"plans": {"after": {"nmi": {"rail": "nmi", "plan_id": "` + plan + `"}}}}}`}},
		{"change_log", map[string]string{"changes": `{"version": 1, "fields": {"display_name": {"after": "` + plan + `"}}}`}},
		{"price_psp_bindings", map[string]string{"plan_id": plan, "configuration": `{}`}},
	}
	for _, c := range refused {
		if ValidateValues(profile(t, c.table), row(t, c.table, c.row)) == nil {
			t.Errorf("%s accepted %v", c.table, c.row)
		}
	}
}

func profile(t *testing.T, table string) Profile {
	t.Helper()
	for _, p := range Profiles {
		if p.Name == table {
			return p
		}
	}
	t.Fatalf("no profile %s", table)
	return Profile{}
}

func row(t *testing.T, table string, set map[string]string) []*string {
	t.Helper()
	p := profile(t, table)
	values := make([]*string, len(p.Columns))
	for i, c := range p.Columns {
		if v, ok := set[c.Name]; ok {
			values[i] = &v
		}
	}
	return values
}
